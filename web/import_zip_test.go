package web

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func postZip(t *testing.T, url, token string, body []byte) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/zip")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := testClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func makeZip(entries map[string][]byte) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range entries {
		f, _ := zw.Create(name)
		_, _ = f.Write(data)
	}
	_ = zw.Close()
	return buf.Bytes()
}

func TestImportZip(t *testing.T) {
	w := newTestWeKan(t)
	w.s.FilesDir = filepath.Join(t.TempDir(), "files")
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	a, bt := login(t, w, "ann"), login(t, w, "bob")
	src := newRESTBoard(t, w, a, "Source")
	data := base64.StdEncoding.EncodeToString([]byte("file contents"))
	call(t, "POST", w.srv.URL+"/api/attachment/upload", a, `{"boardId":"`+src.ID+`","cardId":"`+src.Card+`","fileName":"notes.txt","fileType":"text/plain","fileData":"`+data+`"}`, nil)
	ctx := context.Background()
	var brd Board
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": src.ID}).Decode(&brd)
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": src.ID}, bson.M{"$set": bson.M{"labels.0.name": "Bug"}})
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": src.Card}, bson.M{"$set": bson.M{"labelIds": bson.A{brd.Labels[0].ID}}})

	// Export as .zip, import as a new board: the file comes back from the zip.
	req, _ := http.NewRequest("GET", w.srv.URL+"/api/boards/"+src.ID+"/exportZip", nil)
	req.Header.Set("Authorization", "Bearer "+a)
	res, err := testClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	zipped, _ := io.ReadAll(res.Body)
	res.Body.Close()
	code, out := postZip(t, w.srv.URL+"/api/import/zip?newBoard=1", a, zipped)
	newID, _ := out["boardId"].(string)
	if code != 200 || newID == "" || newID == src.ID {
		t.Fatalf("new board: %d %v", code, out)
	}
	var att Attachment
	if err := w.db.Collection("attachments").FindOne(ctx, bson.M{"meta.boardId": newID}).Decode(&att); err != nil || att.Name != "notes.txt" {
		t.Fatalf("the attachment is back, from the zip: %v %+v", err, att)
	}
	p, _ := w.s.attachmentFile(&att)
	if got, _ := os.ReadFile(p); string(got) != "file contents" {
		t.Fatalf("its bytes: %q", got)
	}

	// Into an existing board: cards added, a label of the same name reused.
	into := newRESTBoard(t, w, a, "Into")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": into.ID}, bson.M{"$set": bson.M{"labels.0.name": "Bug"}})
	code, out = postZip(t, w.srv.URL+"/api/import/zip?boardId="+into.ID, a, zipped)
	counts, _ := out["counts"].(map[string]any)
	if code != 200 || counts["cards"] != float64(1) {
		t.Fatalf("into a board: %d %v", code, out)
	}
	var target Board
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": into.ID}).Decode(&target)
	var added Card
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"boardId": into.ID, "_id": bson.M{"$ne": into.Card}}).Decode(&added)
	if len(target.Labels) != 6 || len(added.LabelIDs) != 1 || added.LabelIDs[0] != target.Labels[0].ID {
		t.Fatalf("the board's own label of that name, no new one: %v %v", target.Labels, added.LabelIDs)
	}

	// Negatives.
	if code, _ = postZip(t, w.srv.URL+"/api/import/zip?newBoard=1", a, makeZip(map[string][]byte{"other.json": []byte("{}")})); code != 400 {
		t.Fatalf("negative: no wekan.json: %d", code)
	}
	if code, _ = postZip(t, w.srv.URL+"/api/import/zip?newBoard=1", a, []byte("not a zip")); code != 400 {
		t.Fatalf("negative: not a zip: %d", code)
	}
	if code, _ = postZip(t, w.srv.URL+"/api/import/zip?newBoard=1", a, makeZip(map[string][]byte{"wekan.json": []byte(`{"_format":"trello"}`)})); code != 400 {
		t.Fatalf("negative: another format: %d", code)
	}
	old := MaxImportDocumentBytes
	MaxImportDocumentBytes = 100
	code, _ = postZip(t, w.srv.URL+"/api/import/zip?newBoard=1", a, makeZip(map[string][]byte{"wekan.json": []byte(`{"title":"` + strings.Repeat("x", 1000) + `"}`)}))
	MaxImportDocumentBytes = old
	if code != 413 {
		t.Fatalf("negative: ZipBombBleed - a document unpacking past the limit: %d", code)
	}
	evil := makeZip(map[string][]byte{"wekan.json": []byte(`{"_format":"wekan-board-1.0.0","title":"E","lists":[],"cards":[]}`),
		"attachments/../../../../tmp/evil": []byte("x")})
	if code, _ = postZip(t, w.srv.URL+"/api/import/zip?newBoard=1", a, evil); code != 200 {
		t.Fatalf("an entry named like a path is only a name: %d", code)
	}
	if _, err := os.Stat("/tmp/evil"); err == nil {
		t.Fatal("negative: ZipBleed - an entry's name became a path")
	}
	setMember(t, w, into.ID, Member{UserID: bob.ID, IsActive: true, IsReadOnly: true})
	if code, _ = postZip(t, w.srv.URL+"/api/import/zip?boardId="+into.ID, bt, zipped); code != 403 {
		t.Fatalf("negative: a read-only member imports nothing into the board: %d", code)
	}
	if code, _ = postZip(t, w.srv.URL+"/api/import/zip?newBoard=1", "", zipped); code != 401 {
		t.Fatalf("negative: nobody signed in: %d", code)
	}
}
