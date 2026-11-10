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
	"go.mongodb.org/mongo-driver/v2/mongo/options"
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
	// 403, as WeKan's universal file server (its CacheBleed spec): signed out,
	// a private board's attachment is refused.
	if res, _ := testClient.Get(api + "/cdn/storage/attachments/" + nodeID); res.StatusCode != 403 {
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

// A copied card gets copies of its live attachments - new files, each
// recording its origin - and its cover is the copy of the source's cover; a
// deleted attachment is not copied, and when the cover's file is gone the
// copy has no cover rather than another card's.
func TestCardCopyAttachments(t *testing.T) {
	w := newTestWeKan(t)
	w.s.FilesDir = filepath.Join(t.TempDir(), "files")
	w.seedUser(t, "ann", "pw")
	at := login(t, w, "ann")
	ctx := context.Background()
	rb := newRESTBoard(t, w, at, "Copies")
	if err := os.MkdirAll(filepath.Join(w.s.FilesDir, "attachments"), 0o750); err != nil {
		t.Fatal(err)
	}
	file := func(id, name, data string, extra bson.M, write bool) {
		doc := bson.M{"_id": id, "name": name, "type": "text/plain", "size": len(data), "meta": bson.M{"boardId": rb.ID, "cardId": rb.Card}, "uploadedAt": time.Now()}
		for k, v := range extra {
			doc[k] = v
		}
		_, _ = w.db.Collection("attachments").InsertOne(ctx, doc)
		if write {
			_ = os.WriteFile(filepath.Join(w.s.FilesDir, "attachments", id), []byte(data), 0o640)
		}
	}
	file("attA", "a.txt", "alpha", nil, true)
	file("attB", "b.txt", "bravo", nil, true)
	file("attGone", "gone.txt", "x", bson.M{"deletedAt": time.Now()}, true)
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": rb.Card}, bson.M{"$set": bson.M{"coverId": "attB"}})
	copyCard := func() string {
		var res struct {
			ID string `json:"_id"`
		}
		if code := call(t, "POST", w.srv.URL+"/api/boards/"+rb.ID+"/lists/"+rb.List+"/cards/"+rb.Card+"/copy", at,
			`{"toBoardId":"`+rb.ID+`","toSwimlaneId":"`+rb.Swimlane+`","toListId":"`+rb.List+`"}`, &res); code != 200 || res.ID == "" {
			t.Fatalf("copy: %d", code)
		}
		return res.ID
	}
	copyID := copyCard()
	cur, _ := w.db.Collection("attachments").Find(ctx, bson.M{"meta.cardId": copyID})
	var copies []bson.M
	_ = cur.All(ctx, &copies)
	if len(copies) != 2 {
		t.Fatalf("the live attachments are copied, not the deleted one: %d", len(copies))
	}
	byOrigin := map[string]string{}
	for _, a := range copies {
		meta, _ := plain(a["meta"]).(map[string]any)
		origin, _ := meta["copyFrom"].(string)
		id := docString(a, "_id")
		byOrigin[origin] = id
		data, _ := os.ReadFile(filepath.Join(w.s.FilesDir, "attachments", id))
		if want := map[string]string{"attA": "alpha", "attB": "bravo"}[origin]; string(data) != want {
			t.Errorf("the copy of %s has its own bytes: %q", origin, data)
		}
	}
	var copied struct {
		CoverID string `bson:"coverId"`
	}
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": copyID}).Decode(&copied)
	if copied.CoverID == "" || copied.CoverID != byOrigin["attB"] {
		t.Errorf("the cover is the copy of the source's cover: %q, want %q", copied.CoverID, byOrigin["attB"])
	}
	// Negative: the cover's file gone - no cover, never the source's.
	_ = os.Remove(filepath.Join(w.s.FilesDir, "attachments", "attB"))
	second := copyCard()
	copied.CoverID = ""
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": second}).Decode(&copied)
	if copied.CoverID != "" {
		t.Errorf("negative: no cover rather than another card's: %q", copied.CoverID)
	}
	if n, _ := w.db.Collection("attachments").CountDocuments(ctx, bson.M{"meta.cardId": rb.Card}); n != 3 {
		t.Errorf("negative: the source keeps its attachments: %d", n)
	}
}

// An attachment Node.js WeKan keeps in GridFS - the "attachments" bucket,
// named by versions.original.meta.gridFsFileId - is read as one on disk:
// downloaded on the board, over REST, and copied with its card; a GridFS id
// that is missing or malformed is not found.
func TestGridFSAttachments(t *testing.T) {
	w := newTestWeKan(t)
	w.s.FilesDir = filepath.Join(t.TempDir(), "files")
	w.seedUser(t, "ann", "pw")
	at := login(t, w, "ann")
	ctx := context.Background()
	rb := newRESTBoard(t, w, at, "Grid")
	bucket := w.db.GridFSBucket(options.GridFSBucket().SetName("attachments"))
	oid, err := bucket.UploadFromStream(ctx, "kept.txt", strings.NewReader("in gridfs"))
	if err != nil {
		t.Fatal(err)
	}
	doc := func(id string, grid string) {
		_, _ = w.db.Collection("attachments").InsertOne(ctx, bson.M{"_id": id, "name": id + ".txt", "type": "text/plain", "size": 9,
			"meta": bson.M{"boardId": rb.ID, "cardId": rb.Card}, "uploadedAt": time.Now(),
			"versions": bson.M{"original": bson.M{"storage": "gridfs", "meta": bson.M{"gridFsFileId": grid}}}})
	}
	doc("gridOK", oid.Hex())
	cl := w.client(t)
	body(t, w.signIn(t, cl, "ann", "pw"))
	res, err := cl.Get(w.srv.URL + "/cdn/storage/attachments/gridOK")
	if err != nil {
		t.Fatal(err)
	}
	if got := body(t, res); res.StatusCode != 200 || got != "in gridfs" {
		t.Errorf("downloaded from GridFS: %d %q", res.StatusCode, got)
	}
	var out struct {
		Base64Data string `json:"base64Data"`
	}
	if code := call(t, "GET", w.srv.URL+"/api/attachment/download/gridOK", at, "", &out); code != 200 {
		t.Errorf("REST download: %d", code)
	}
	if data, _ := base64.StdEncoding.DecodeString(out.Base64Data); string(data) != "in gridfs" {
		t.Errorf("REST download's bytes: %q", data)
	}
	// Copied with its card.
	var copied struct {
		ID string `json:"_id"`
	}
	call(t, "POST", w.srv.URL+"/api/boards/"+rb.ID+"/lists/"+rb.List+"/cards/"+rb.Card+"/copy", at,
		`{"toBoardId":"`+rb.ID+`","toSwimlaneId":"`+rb.Swimlane+`","toListId":"`+rb.List+`"}`, &copied)
	var c bson.M
	if w.db.Collection("attachments").FindOne(ctx, bson.M{"meta.cardId": copied.ID, "meta.copyFrom": "gridOK"}).Decode(&c) != nil {
		t.Error("a GridFS attachment is copied with its card")
	}
	// Negative: a GridFS id with nothing behind it, or not an id at all.
	doc("gridMissing", bson.NewObjectID().Hex())
	doc("gridBad", "not-an-object-id")
	for _, id := range []string{"gridMissing", "gridBad"} {
		res, err := cl.Get(w.srv.URL + "/cdn/storage/attachments/" + id)
		if err != nil {
			t.Fatal(err)
		}
		if body(t, res); res.StatusCode != http.StatusNotFound {
			t.Errorf("negative: %s is not found: %d", id, res.StatusCode)
		}
	}
}
