package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestAttachments(t *testing.T) {
	w := newTestWeKan(t)
	w.s.FilesDir = filepath.Join(t.TempDir(), "files")
	u := w.seedUser(t, "wes", "pw")
	w.seedUser(t, "xia", "pw")
	api := w.srv.URL
	var a, o struct{ Token string }
	call(t, "POST", api+"/users/login", "", `{"username":"wes","password":"pw"}`, &a)
	call(t, "POST", api+"/users/login", "", `{"username":"xia","password":"pw"}`, &o)
	b, _ := w.s.CreateBoard(context.Background(), u, "Files")
	l, _ := w.s.AddList(context.Background(), b, "L")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(context.Background(), bson.M{"boardId": b.ID}).Decode(&sw)
	c, _ := w.s.AddCard(context.Background(), b, u, l.ID, sw.ID, "C")

	// REST upload, WeKan's JSON with base64.
	png := []byte("\x89PNG\r\n\x1a\nfake")
	var up struct {
		Success      bool
		AttachmentID string `json:"attachmentId"`
		FileSize     int64  `json:"fileSize"`
	}
	body := `{"boardId":"` + b.ID + `","swimlaneId":"` + sw.ID + `","listId":"` + l.ID + `","cardId":"` + c.ID +
		`","fileName":"../../etc/pic.png","fileType":"image/png","fileData":"` + base64.StdEncoding.EncodeToString(png) + `"}`
	if code := call(t, "POST", api+"/api/attachment/upload", a.Token, body, &up); code != 200 || !up.Success || up.FileSize != int64(len(png)) {
		t.Fatalf("upload: %d %+v", code, up)
	}
	path := filepath.Join(w.s.FilesDir, "attachments", up.AttachmentID)
	if got, _ := os.ReadFile(path); !bytes.Equal(got, png) {
		t.Fatal("the file is <files>/attachments/<id>, as Node.js WeKan keeps it")
	}
	var att Attachment
	var route struct {
		DownloadRoute string `bson:"_downloadRoute"`
	}
	_ = w.db.Collection("attachments").FindOne(context.Background(), bson.M{"_id": up.AttachmentID}).Decode(&att)
	_ = w.db.Collection("attachments").FindOne(context.Background(), bson.M{"_id": up.AttachmentID}).Decode(&route)
	if att.Name != "pic.png" || att.Versions.Original.Path != path || att.Meta.CardID != c.ID || att.Meta.BoardID != b.ID || route.DownloadRoute != "/cdn/storage" {
		t.Fatalf("Meteor-Files' document, the name without its path: %+v", att)
	}

	var dl struct {
		Base64Data string `json:"base64Data"`
		FileName   string `json:"fileName"`
	}
	call(t, "GET", api+"/api/attachment/download/"+up.AttachmentID, a.Token, "", &dl)
	if got, _ := base64.StdEncoding.DecodeString(dl.Base64Data); !bytes.Equal(got, png) {
		t.Fatal("download gives the file back")
	}
	var list struct {
		Count int `json:"count"`
	}
	call(t, "GET", api+"/api/attachment/list/"+b.ID+"/"+sw.ID+"/"+l.ID+"/"+c.ID, a.Token, "", &list)
	if list.Count != 1 {
		t.Fatalf("list: %d", list.Count)
	}

	// The file itself, at Meteor-Files' address, to a reader of the board.
	cl := w.client(t)
	body2 := w.signIn(t, cl, "wes", "pw")
	body2.Body.Close()
	res, err := cl.Get(api + "/cdn/storage/attachments/" + up.AttachmentID)
	if err != nil {
		t.Fatal(err)
	}
	got := new(bytes.Buffer)
	_, _ = got.ReadFrom(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !bytes.Equal(got.Bytes(), png) || res.Header.Get("Content-Type") != "image/png" ||
		!strings.HasPrefix(res.Header.Get("Content-Disposition"), "inline") || res.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("serving an image: %d %v", res.StatusCode, res.Header)
	}

	// An HTML file is downloaded, never shown as a page of this site.
	call(t, "POST", api+"/api/attachment/upload", a.Token, `{"boardId":"`+b.ID+`","cardId":"`+c.ID+`","fileName":"x.html","fileType":"text/html","fileData":"`+
		base64.StdEncoding.EncodeToString([]byte("<script>alert(1)</script>"))+`"}`, &up)
	res, _ = cl.Get(api + "/cdn/storage/attachments/" + up.AttachmentID)
	res.Body.Close()
	if res.Header.Get("Content-Type") != "application/octet-stream" || !strings.HasPrefix(res.Header.Get("Content-Disposition"), "attachment") ||
		!strings.Contains(res.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("negative: HTML must be downloaded: %v", res.Header)
	}

	// A document Node.js WeKan wrote is served.
	nodeID := "nodeAttachment01"
	_ = os.WriteFile(filepath.Join(w.s.FilesDir, "attachments", nodeID), []byte("plain text"), 0o640)
	_, _ = w.db.Collection("attachments").InsertOne(context.Background(), bson.M{"_id": nodeID, "name": "notes.txt", "type": "text/plain",
		"size": 10, "versions": bson.M{"original": bson.M{"path": "/data/files/attachments/" + nodeID}},
		"meta": bson.M{"boardId": b.ID, "cardId": c.ID}, "uploadedAt": time.Now()})
	if res, _ := cl.Get(api + "/cdn/storage/attachments/" + nodeID); res.StatusCode != 200 {
		t.Fatalf("an attachment of Node.js WeKan: %d", res.StatusCode)
	}

	// Negative: a stored path is not followed out of the attachments directory.
	_, _ = w.db.Collection("attachments").InsertOne(context.Background(), bson.M{"_id": "evilPath01", "name": "p", "type": "text/plain",
		"versions": bson.M{"original": bson.M{"path": "/etc/passwd"}}, "meta": bson.M{"boardId": b.ID, "cardId": c.ID}})
	if res, _ := cl.Get(api + "/cdn/storage/attachments/evilPath01"); res.StatusCode != 404 {
		t.Fatalf("negative: /etc/passwd through a stored path: %d", res.StatusCode)
	}
	// Negative: another user, no user, bad data.
	oc := w.client(t)
	r2 := w.signIn(t, oc, "xia", "pw")
	r2.Body.Close()
	if res, _ := oc.Get(api + "/cdn/storage/attachments/" + nodeID); res.StatusCode != 404 {
		t.Fatalf("negative: another user's attachment: %d", res.StatusCode)
	}
	if res, _ := testClient.Get(api + "/cdn/storage/attachments/" + nodeID); res.StatusCode != 401 {
		t.Fatalf("negative: signed out: %d", res.StatusCode)
	}
	if code := call(t, "GET", api+"/api/attachment/download/"+nodeID, o.Token, "", nil); code != 403 {
		t.Fatalf("negative: another user downloads: %d", code)
	}
	if code := call(t, "POST", api+"/api/attachment/upload", o.Token, `{"boardId":"`+b.ID+`","cardId":"`+c.ID+`","fileName":"a","fileData":"YQ=="}`, nil); code != 404 {
		t.Fatalf("negative: another user uploads to a private board: %d", code)
	}
	if code := call(t, "POST", api+"/api/attachment/upload", a.Token, `{"boardId":"`+b.ID+`","cardId":"`+c.ID+`","fileName":"a","fileData":"%%%"}`, nil); code != 400 {
		t.Fatalf("negative: not base64: %d", code)
	}

	// The card's upload form, and its list on the card.
	var mp bytes.Buffer
	mw := multipart.NewWriter(&mp)
	fw, _ := mw.CreateFormFile("file", "form.txt")
	_, _ = fw.Write([]byte("from the form"))
	mw.Close()
	req, _ := http.NewRequest("POST", api+"/b/"+b.ID+"/cards/"+c.ID+"/attachments", &mp)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if res, err := cl.Do(req); err != nil || res.StatusCode != http.StatusSeeOther {
		t.Fatalf("the upload form: %v", err)
	}
	if _, page := get(t, cl, api+"/b/"+b.ID+"/"+b.Slug+"/"+c.ID); !strings.Contains(page, "form.txt") || !strings.Contains(page, "js-attachment-upload") {
		t.Fatal("the card shows its attachments and the upload form")
	}

	// Raw upload, the ids in the query.
	req, _ = http.NewRequest("POST", api+"/api/attachment/upload?boardId="+b.ID+"&cardId="+c.ID+"&fileName=raw.bin", strings.NewReader("raw bytes"))
	req.Header.Set("Authorization", "Bearer "+a.Token)
	if res, err := testClient.Do(req); err != nil || res.StatusCode != 200 {
		t.Fatalf("raw upload: %v %v", err, res)
	}

	// Delete keeps the document with deletedAt and the file, as WeKan's soft delete.
	if code := call(t, "DELETE", api+"/api/attachment/delete/"+nodeID, o.Token, "", nil); code != 403 {
		t.Fatalf("negative: another user deletes: %d", code)
	}
	if code := call(t, "DELETE", api+"/api/attachment/delete/"+nodeID, a.Token, "", nil); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	var doc bson.M
	_ = w.db.Collection("attachments").FindOne(context.Background(), bson.M{"_id": nodeID}).Decode(&doc)
	if doc == nil || doc["deletedAt"] == nil {
		t.Fatal("soft deleted: the document stays, with deletedAt")
	}
	if res, _ := cl.Get(api + "/cdn/storage/attachments/" + nodeID); res.StatusCode != 404 {
		t.Fatalf("a deleted attachment is not served: %d", res.StatusCode)
	}
}

func TestCleanFileName(t *testing.T) {
	for in, want := range map[string]string{"../../etc/passwd": "passwd", "a\\b\\c.txt": "c.txt", "..": "file", "": "file",
		"x\x00y\n.txt": "xy.txt", strings.Repeat("ä", 200): strings.Repeat("ä", 127)} {
		if got := cleanFileName(in); got != want {
			t.Errorf("cleanFileName(%q) = %q, want %q", in, got, want)
		}
	}
}
