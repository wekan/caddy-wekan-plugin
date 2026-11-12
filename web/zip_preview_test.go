package web

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func zipOf(t *testing.T, names ...string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range names {
		f, _ := zw.Create(n)
		_, _ = f.Write([]byte("x"))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// WeKan's ZIP preview: an archive's entry names, as text, for whoever may
// download it; anything that is not a ZIP, an invalid one and one of too many
// entries are refused.
func TestZipPreview(t *testing.T) {
	w := newTestWeKan(t)
	w.s.FilesDir = filepath.Join(t.TempDir(), "files")
	w.seedUser(t, "ann", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Zips")
	ac := w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	upload := func(name string, content []byte) string {
		var buf strings.Builder
		mw := multipart.NewWriter(&buf)
		fw, _ := mw.CreateFormFile("file", name)
		_, _ = fw.Write(content)
		_ = mw.Close()
		req, _ := http.NewRequest("POST", w.srv.URL+"/b/"+rb.ID+"/cards/"+rb.Card+"/attachments", strings.NewReader(buf.String()))
		req.Header.Set("Content-Type", mw.FormDataContentType())
		res, err := noRedirect(ac).Do(req)
		if err != nil || res.StatusCode != http.StatusSeeOther {
			t.Fatalf("upload: %v", err)
		}
		res.Body.Close()
		var a Attachment
		_ = w.db.Collection("attachments").FindOne(t.Context(), bson.M{"name": name}, options.FindOne().SetSort(bson.D{{Key: "uploadedAt", Value: -1}})).Decode(&a)
		return a.ID
	}
	entries := func(cl *http.Client, id string) (int, []string) {
		res, err := cl.Get(w.srv.URL + "/cdn/storage/attachments/" + id + "/zip-entries")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		var out struct{ Entries []string }
		_ = json.Unmarshal(b, &out)
		return res.StatusCode, out.Entries
	}
	id := upload("bundle.zip", zipOf(t, "a.txt", "dir/<b>bold</b>.html", "dir/"))
	if code, got := entries(ac, id); code != 200 || strings.Join(got, "|") != "a.txt|dir/<b>bold</b>.html|dir/" {
		t.Errorf("the entry names, as they are: %d %q", code, got)
	}
	if _, html := get(t, ac, w.srv.URL+"/b/"+rb.ID+"/x/"+rb.Card); !strings.Contains(html, `data-kind="zip"`) || !strings.Contains(html, `id="zip-viewer"`) {
		t.Error("the tile opens the ZIP preview")
	}
	if code, _ := entries(ac, upload("notes.txt", []byte("hello"))); code != http.StatusBadRequest {
		t.Errorf("negative: not a ZIP: %d", code)
	}
	if code, _ := entries(ac, upload("broken.zip", []byte("PK not really a zip archive"))); code != http.StatusUnprocessableEntity {
		t.Errorf("negative: an invalid archive: %d", code)
	}
	many := make([]string, zipPreviewMaxEntries+1)
	for i := range many {
		many[i] = fmt.Sprintf("f%d", i)
	}
	if code, _ := entries(ac, upload("many.zip", zipOf(t, many...))); code != http.StatusUnprocessableEntity {
		t.Errorf("negative: too many entries: %d", code)
	}
	w.seedUser(t, "eve", "pw")
	ec := w.client(t)
	body(t, w.signIn(t, ec, "eve", "pw"))
	if code, got := entries(ec, id); code == 200 || len(got) != 0 {
		t.Errorf("negative: not for someone who may not download it: %d %v", code, got)
	}
}
