package web

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestExportZip(t *testing.T) {
	w := newTestWeKan(t)
	w.s.FilesDir = filepath.Join(t.TempDir(), "files")
	w.seedUser(t, "ann", "pw")
	w.seedUser(t, "bob", "pw")
	a, bt := login(t, w, "ann"), login(t, w, "bob")
	b := newRESTBoard(t, w, a, "Zip / Board!")
	for _, name := range []string{"a.txt", "a.txt", "../../etc/passwd"} {
		data := base64.StdEncoding.EncodeToString([]byte("data of " + name))
		call(t, "POST", w.srv.URL+"/api/attachment/upload", a, `{"boardId":"`+b.ID+`","cardId":"`+b.Card+`","fileName":"`+name+`","fileType":"text/plain","fileData":"`+data+`"}`, nil)
	}
	_, _ = w.db.Collection("cards").UpdateOne(context.Background(), bson.M{"_id": b.Card}, bson.M{"$set": bson.M{"description": "<script>x</script>kept"}})
	get := func(url, token string) (*http.Response, []byte) {
		req, _ := http.NewRequest("GET", url, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := testClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return res, body
	}
	url := w.srv.URL + "/api/boards/" + b.ID + "/exportZip"
	res, body := get(url, a)
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "application/zip" ||
		res.Header.Get("Content-Disposition") != `attachment; filename="Zip-Board.zip"` {
		t.Fatalf("zip: %d %v", res.StatusCode, res.Header)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	var doc map[string]any
	for _, f := range zr.File {
		names = append(names, f.Name)
		if strings.Contains(f.Name, "..") || strings.HasPrefix(f.Name, "/") {
			t.Fatalf("negative: a path out of the zip: %s", f.Name)
		}
		if f.Name == "wekan.json" {
			rc, _ := f.Open()
			_ = json.NewDecoder(rc).Decode(&doc)
			rc.Close()
		}
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "attachments/a-1.txt,attachments/a.txt,attachments/passwd,wekan.json" {
		t.Fatalf("wekan.json and the files, a repeated name numbered: %v", names)
	}
	if doc["_format"] != "wekan-board-1.0.0" {
		t.Fatalf("wekan.json is the export: %v", doc["_format"])
	}
	for _, att := range doc["attachments"].([]any) {
		if _, has := att.(map[string]any)["file"]; has {
			t.Fatal("negative: the files are beside wekan.json, not inside it")
		}
	}
	if c := doc["cards"].([]any)[0].(map[string]any); c["description"] != "kept" {
		t.Fatalf("the export boundary applies: %v", c["description"])
	}
	if res, _ = get(url, bt); res.StatusCode != 403 {
		t.Fatalf("negative: a non-member: %d", res.StatusCode)
	}
	if res, _ = get(url, ""); res.StatusCode != 401 {
		t.Fatalf("negative: no token: %d", res.StatusCode)
	}
}
