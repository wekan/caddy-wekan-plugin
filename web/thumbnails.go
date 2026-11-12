package web

import (
	"bytes"
	"container/list"
	"errors"
	"image"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	xdraw "golang.org/x/image/draw"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

// An image attachment's thumbnail, as Node.js WeKan's #3275
// (models/lib/attachmentThumbnail.js, server/lib/imageThumbnail.js):
// /cdn/storage/attachments/<id>/thumbnail, behind every check of the
// original, at most 512 pixels on its longest side, never enlarged, kept in
// memory - 500 at most, 64 MiB together, the least recently used dropped
// first. WeKan's sharp writes WebP; pure Go has no WebP encoder, so Weddy
// writes JPEG, or PNG for a picture with transparency - both shown wherever a
// thumbnail is. SVG is never one (text that can carry scripts), nor are HEIC
// and AVIF, which pure Go cannot read: those answer 404, as WeKan does for
// what it cannot thumbnail.

const (
	thumbnailEdge      = 512
	thumbnailMaxBytes  = 32 << 20 // IMAGE_MAX_BYTES
	thumbnailMaxPixels = 40_000_000
	thumbnailEntries   = 500
	thumbnailCacheMax  = 64 << 20
)

var thumbnailTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/pjpeg": true, "image/gif": true,
	"image/webp": true, "image/bmp": true, "image/tiff": true}

var thumbnailExtensions = map[string]bool{"png": true, "jpg": true, "jpeg": true, "jpe": true, "jfif": true, "gif": true,
	"webp": true, "bmp": true, "tif": true, "tiff": true}

// canThumbnail is WeKan's: a raster type, or a raster extension when there is
// no type - here only the formats Weddy reads.
func canThumbnail(a *Attachment) bool {
	typ := strings.ToLower(strings.TrimSpace(strings.SplitN(a.Type, ";", 2)[0]))
	if typ != "" {
		return thumbnailTypes[typ]
	}
	i := strings.LastIndex(a.Name, ".")
	return i >= 0 && thumbnailExtensions[strings.ToLower(a.Name[i+1:])]
}

type thumbnailEntry struct {
	key, typ string
	data     []byte
}

var thumbnailCache = struct {
	sync.Mutex
	order *list.List
	byKey map[string]*list.Element
	bytes int
}{order: list.New(), byKey: map[string]*list.Element{}}

func cachedThumbnail(key string) (thumbnailEntry, bool) {
	thumbnailCache.Lock()
	defer thumbnailCache.Unlock()
	if e, ok := thumbnailCache.byKey[key]; ok {
		thumbnailCache.order.MoveToBack(e)
		return e.Value.(thumbnailEntry), true
	}
	return thumbnailEntry{}, false
}

func rememberThumbnail(t thumbnailEntry) {
	if len(t.data) > thumbnailCacheMax {
		return
	}
	thumbnailCache.Lock()
	defer thumbnailCache.Unlock()
	if old, ok := thumbnailCache.byKey[t.key]; ok {
		thumbnailCache.bytes -= len(old.Value.(thumbnailEntry).data)
		thumbnailCache.order.Remove(old)
	}
	thumbnailCache.byKey[t.key] = thumbnailCache.order.PushBack(t)
	thumbnailCache.bytes += len(t.data)
	for thumbnailCache.order.Len() > thumbnailEntries || thumbnailCache.bytes > thumbnailCacheMax {
		oldest := thumbnailCache.order.Front()
		e := oldest.Value.(thumbnailEntry)
		thumbnailCache.order.Remove(oldest)
		delete(thumbnailCache.byKey, e.key)
		thumbnailCache.bytes -= len(e.data)
	}
}

// makeThumbnail is WeKan's convertImageBufferToThumbnail: at most 512 on the
// longest side, never enlarged, within WeKan's input limits.
func makeThumbnail(r io.Reader) ([]byte, string, error) {
	data, err := io.ReadAll(io.LimitReader(r, thumbnailMaxBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) == 0 || len(data) > thumbnailMaxBytes {
		return nil, "", errors.New("invalid image input")
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > thumbnailMaxPixels {
		return nil, "", errors.New("invalid image input")
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", err
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w > thumbnailEdge || h > thumbnailEdge {
		if w >= h {
			w, h = thumbnailEdge, max(1, h*thumbnailEdge/b.Dx())
		} else {
			w, h = max(1, w*thumbnailEdge/b.Dy()), thumbnailEdge
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Src, nil)
	var out bytes.Buffer
	if dst.Opaque() {
		err = jpeg.Encode(&out, dst, &jpeg.Options{Quality: 80})
		return out.Bytes(), "image/jpeg", err
	}
	err = png.Encode(&out, dst)
	return out.Bytes(), "image/png", err
}

// serveAttachmentThumbnail answers /cdn/storage/attachments/<id>/thumbnail.
func (s *Server) serveAttachmentThumbnail(w http.ResponseWriter, r *http.Request) {
	a := s.downloadableAttachment(w, r)
	if a == nil {
		return
	}
	if !canThumbnail(a) {
		http.Error(w, "No thumbnail for this attachment", http.StatusNotFound)
		return
	}
	// The file's identity, so a replaced file never gets its old preview.
	key := a.ID + "|" + a.Versions.Original.Path + "|" + a.Versions.Original.Meta.GridFsFileID + "|" + strconv.FormatInt(a.Size, 10) + "|" + a.UploadedAt.String()
	t, ok := cachedThumbnail(key)
	if !ok {
		f, err := s.openAttachment(r.Context(), a)
		if err != nil {
			http.Error(w, "Attachment not found", http.StatusNotFound)
			return
		}
		data, typ, err := makeThumbnail(f)
		f.Close()
		if err != nil {
			// A damaged image falls back to its original, as WeKan's.
			http.Redirect(w, r, "/cdn/storage/attachments/"+a.ID, http.StatusFound)
			return
		}
		t = thumbnailEntry{key: key, typ: typ, data: data}
		rememberThumbnail(t)
	}
	h := w.Header()
	etag := `"thumb-` + sha256Hex(t.key)[:32] + `"`
	h.Set("ETag", etag)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("Cache-Control", "private, no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", t.typ)
	_, _ = w.Write(t.data)
}

// Thumbnail tells the card's attachment list to show a's thumbnail.
func (a Attachment) Thumbnail() bool { return canThumbnail(&a) }

// Ext is attachmentGallery's extension: the name's, lower-case, without its dot.
func (a Attachment) Ext() string {
	e := strings.ToLower(filepath.Ext(a.Name))
	return strings.TrimPrefix(e, ".")
}

// SizeText is WeKan's fileSize: the size in B, KB, MB or GB.
func (a Attachment) SizeText() string {
	n := float64(a.Size)
	for _, unit := range []string{"B", "KB", "MB", "GB"} {
		if n < 1024 || unit == "GB" {
			if unit == "B" {
				return strconv.FormatInt(a.Size, 10) + " B"
			}
			return strconv.FormatFloat(n, 'f', 1, 64) + " " + unit
		}
		n /= 1024
	}
	return ""
}

// PreviewKind is what the attachment viewer shows it as: image, pdf, video,
// audio, text - or none, opened in its own tab instead.
func (a Attachment) PreviewKind() string {
	t := strings.ToLower(a.Type)
	switch {
	case a.Thumbnail() || a.Ext() == "svg":
		return "image"
	case t == "application/pdf" || a.Ext() == "pdf":
		return "pdf"
	case strings.HasPrefix(t, "video/"):
		return "video"
	case strings.HasPrefix(t, "audio/"):
		return "audio"
	case t == "text/plain" || a.Ext() == "txt":
		return "text"
	case isZipAttachment(a.Type, a.Name):
		return "zip"
	}
	return "none"
}
