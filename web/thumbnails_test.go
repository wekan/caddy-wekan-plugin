package web

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// #3275: an image attachment's thumbnail, behind the original's checks: at
// most 512 on its longest side, never enlarged, JPEG or - with transparency -
// PNG; none for SVG, text or a picture too large to read.
func TestAttachmentThumbnails(t *testing.T) {
	w := newTestWeKan(t)
	w.s.FilesDir = filepath.Join(t.TempDir(), "files")
	ann := w.seedUser(t, "ann", "pw")
	w.seedUser(t, "zed", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, ann, "Pictures")
	if err := os.MkdirAll(filepath.Join(w.s.FilesDir, "attachments"), 0o750); err != nil {
		t.Fatal(err)
	}
	img := func(wd, ht int, alpha bool) *image.NRGBA {
		m := image.NewNRGBA(image.Rect(0, 0, wd, ht))
		for y := 0; y < ht; y++ {
			for x := 0; x < wd; x++ {
				a := uint8(255)
				if alpha && x < wd/2 {
					a = 0
				}
				m.SetNRGBA(x, y, color.NRGBA{uint8(x), uint8(y), 99, a})
			}
		}
		return m
	}
	encode := func(m image.Image, asJPEG bool) []byte {
		var buf bytes.Buffer
		if asJPEG {
			_ = jpeg.Encode(&buf, m, nil)
		} else {
			_ = png.Encode(&buf, m)
		}
		return buf.Bytes()
	}
	// A PNG whose header claims 100000 x 100000 pixels: refused unread.
	bomb := func() []byte {
		var buf bytes.Buffer
		buf.Write([]byte("\x89PNG\r\n\x1a\n"))
		ihdr := make([]byte, 13)
		binary.BigEndian.PutUint32(ihdr[0:], 100000)
		binary.BigEndian.PutUint32(ihdr[4:], 100000)
		ihdr[8], ihdr[9] = 8, 2
		chunk := append([]byte("IHDR"), ihdr...)
		_ = binary.Write(&buf, binary.BigEndian, uint32(13))
		buf.Write(chunk)
		_ = binary.Write(&buf, binary.BigEndian, crc32.ChecksumIEEE(chunk))
		return buf.Bytes()
	}()
	file := func(id, name, typ string, data []byte) {
		_, _ = w.db.Collection("attachments").InsertOne(ctx, bson.M{"_id": id, "name": name, "type": typ, "size": len(data),
			"meta": bson.M{"boardId": b.ID, "cardId": "c"}, "uploadedAt": time.Now()})
		_ = os.WriteFile(filepath.Join(w.s.FilesDir, "attachments", id), data, 0o640)
	}
	file("thAlpha", "wide.png", "image/png", encode(img(2000, 1000, true), false))
	file("thPhoto", "photo.jpg", "image/jpeg", encode(img(800, 600, false), true))
	file("thSmall", "small.png", "image/png", encode(img(100, 100, false), false))
	file("thSVG", "x.svg", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`))
	file("thText", "x.txt", "text/plain", []byte("hello"))
	file("thBomb", "bomb.png", "image/png", bomb)
	cl := w.client(t)
	body(t, w.signIn(t, cl, "ann", "pw"))
	get := func(c *http.Client, id string) (*http.Response, []byte) {
		res, err := c.Get(w.srv.URL + "/cdn/storage/attachments/" + id + "/thumbnail")
		if err != nil {
			t.Fatal(err)
		}
		return res, []byte(body(t, res))
	}
	for id, want := range map[string]struct {
		typ  string
		w, h int
	}{"thAlpha": {"image/png", 512, 256}, "thPhoto": {"image/jpeg", 512, 384}, "thSmall": {"image/jpeg", 100, 100}} {
		res, data := get(cl, id)
		if res.StatusCode != 200 || res.Header.Get("Content-Type") != want.typ {
			t.Errorf("%s: %d %s", id, res.StatusCode, res.Header.Get("Content-Type"))
			continue
		}
		cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil || cfg.Width != want.w || cfg.Height != want.h {
			t.Errorf("%s: %dx%d, want %dx%d (%v)", id, cfg.Width, cfg.Height, want.w, want.h, err)
		}
	}
	for _, id := range []string{"thSVG", "thText"} {
		if res, _ := get(cl, id); res.StatusCode != http.StatusNotFound {
			t.Errorf("negative: no thumbnail for %s: %d", id, res.StatusCode)
		}
	}
	// A picture it cannot read falls back to its original, as WeKan's.
	noRedirect := *cl
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if res, _ := get(&noRedirect, "thBomb"); res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/cdn/storage/attachments/thBomb" {
		t.Errorf("negative: an unreadable picture falls back to its original: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	// The card shows an image's thumbnail, and none for a text file.
	if !(Attachment{Type: "image/png"}).Thumbnail() || (Attachment{Type: "text/plain"}).Thumbnail() || (Attachment{Type: "image/svg+xml"}).Thumbnail() {
		t.Error("a thumbnail in the list for a raster image only")
	}
	// The bomb is refused at its header, before a pixel is decoded.
	if _, _, err := makeThumbnail(bytes.NewReader(bomb)); err == nil || err.Error() != "invalid image input" {
		t.Errorf("negative: too many pixels is refused unread: %v", err)
	}
	// The original's checks: signed in, and a reader of its board.
	if res, _ := get(testClient, "thPhoto"); res.StatusCode != http.StatusForbidden {
		t.Errorf("negative: not signed in, a private board: %d", res.StatusCode)
	}
	// Revalidation: the same ETag answers 304; every answer varies by who asks.
	res, _ := get(cl, "thPhoto")
	if v := res.Header.Get("Vary"); v != "Cookie, Authorization, X-Auth-Token" {
		t.Errorf("CacheBleed: Vary %q", v)
	}
	req, _ := http.NewRequest("GET", w.srv.URL+"/cdn/storage/attachments/thPhoto/thumbnail", nil)
	req.Header.Set("If-None-Match", res.Header.Get("ETag"))
	if again, err := cl.Do(req); err != nil || again.StatusCode != http.StatusNotModified {
		t.Errorf("revalidated with its ETag: %v", again)
	} else {
		again.Body.Close()
	}
	zed := w.client(t)
	body(t, w.signIn(t, zed, "zed", "pw"))
	if res, _ := get(zed, "thPhoto"); res.StatusCode != http.StatusNotFound {
		t.Errorf("negative: not a reader of the board: %d", res.StatusCode)
	}
	// A public board's attachment is read by anybody.
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"permission": "public"}})
	if res, _ := get(testClient, "thPhoto"); res.StatusCode != http.StatusOK {
		t.Errorf("a public board's thumbnail, signed out: %d", res.StatusCode)
	}
}
