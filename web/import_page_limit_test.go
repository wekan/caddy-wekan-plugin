package web

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The import page reads an export .zip up to its parser's limit, as WeKan's
// 64 MB for Vikunja: a 40 MB-padded Vikunja export imports from the page and
// through its members-mapping step's payload; past 64 MB it is refused, and
// a JSON source stays at 32 MB.
func TestImportPageTakesWeKansArchiveLimit(t *testing.T) {
	if importPageLimit("vikunja") != 64<<20 || importPageLimit("plane") != 64<<20 || importPageLimit("notion") != 64<<20 {
		t.Fatal("the archives take WeKan's 64 MB")
	}
	if importPageLimit("kanboard") != maxImportForm || importPageLimit("csv") != maxImportForm {
		t.Error("negative: every other file stays at the form's limit")
	}
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	c := w.client(t)
	body(t, w.signIn(t, c, "ann", "pw"))
	data, err := os.ReadFile("testdata/external/vikunja.golden")
	if err != nil {
		t.Fatal(err)
	}
	archive := func(pad int) []byte {
		var buf bytes.Buffer
		z := zip.NewWriter(&buf)
		for name, text := range map[string]string{"data.json": string(data), "filters.json": "[]", "VERSION": "v1.0.0"} {
			f, _ := z.Create(name)
			_, _ = f.Write([]byte(text))
		}
		// An attachment that does not compress, so the .zip is its size.
		f, _ := z.CreateHeader(&zip.FileHeader{Name: "files/1/pad.bin", Method: zip.Store})
		noise := make([]byte, pad)
		_, _ = rand.Read(noise)
		_, _ = f.Write(noise)
		_ = z.Close()
		return buf.Bytes()
	}
	upload := func(file []byte, skip bool) *http.Response {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		f, _ := mw.CreateFormFile("file", "vikunja-export.zip")
		_, _ = f.Write(file)
		if skip {
			_ = mw.WriteField("skipMapping", "1")
		}
		_ = mw.Close()
		req, _ := http.NewRequest("POST", w.srv.URL+"/import/vikunja", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		res, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	boards := func() int64 { n, _ := w.db.Collection("boards").CountDocuments(ctx, bson.M{}); return n }
	big := archive(40 << 20)
	if len(big) <= maxImportForm {
		t.Fatalf("the archive is over 32 MB: %d", len(big))
	}
	before := boards()
	res := upload(big, true)
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther || boards() <= before {
		t.Fatalf("a 40 MB Vikunja export imports: %d, %d boards", res.StatusCode, boards()-before)
	}
	// The members-mapping step's payload: the same file in base64, posted
	// back as the step posts it.
	before = boards()
	res, err = c.PostForm(w.srv.URL+"/import/vikunja/members", url.Values{"payload": {base64.StdEncoding.EncodeToString(big)}, "skipMapping": {"1"}})
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther || boards() <= before {
		t.Errorf("the step's 40 MB payload imports: %d", res.StatusCode)
	}
	before = boards()
	res = upload(archive(65<<20), true)
	res.Body.Close()
	if res.StatusCode == http.StatusSeeOther || boards() != before {
		t.Errorf("negative: past 64 MB it is refused: %d, %d boards", res.StatusCode, boards()-before)
	}
}
