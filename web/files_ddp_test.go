package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func ddpStart(fileID, cardID, boardID string) string {
	meta := `"cardId":"` + cardID + `"`
	if boardID != "" {
		meta += `,"boardId":"` + boardID + `"`
	}
	if cardID == "" {
		meta = `"boardId":"` + boardID + `"`
	}
	return `[{"file":{"name":"notes.txt","type":"text/plain","size":8,"meta":{` + meta + `}},"fileId":"` + fileID + `","chunkSize":4,"fileLength":2}]`
}

func ddpChunk(fileID string, n int, data string) string {
	return `[{"fileId":"` + fileID + `","chunkId":` + string(rune('0'+n)) + `,"binData":"` + base64.StdEncoding.EncodeToString([]byte(data)) + `"}]`
}

// Meteor-Files' DDP upload, start to eof, onto one's own card: stored with
// its bytes in order, stamped with the card's board, list and swimlane,
// recorded in the change history, and answered without a server path.
func TestDDPUploadAsWeKan(t *testing.T) {
	w := newTestWeKan(t)
	w.s.FilesDir = filepath.Join(t.TempDir(), "files")
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Up")
	ac := w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	if _, out := methodCall(t, w, ac, "_FilesCollectionStart_attachments", ddpStart("fileOne", rb.Card, rb.ID)); out["error"] != nil {
		t.Fatalf("start: %v", out)
	}
	// Chunk 2 first: each chunk has its place.
	for _, c := range []struct {
		n    int
		data string
	}{{2, "efgh"}, {1, "abcd"}} {
		if _, out := methodCall(t, w, ac, "_FilesCollectionWrite_attachments", ddpChunk("fileOne", c.n, c.data)); out["error"] != nil {
			t.Fatalf("chunk %d: %v", c.n, out)
		}
	}
	_, out := methodCall(t, w, ac, "_FilesCollectionWrite_attachments", `[{"fileId":"fileOne","eof":true}]`)
	res, _ := out["result"].(map[string]any)
	if res == nil || res["status"] != float64(200) || res["name"] != "notes.txt" {
		t.Fatalf("eof answers the file: %v", out)
	}
	if raw, _ := json.Marshal(res); strings.Contains(string(raw), `"path"`) || strings.Contains(string(raw), "_storagePath") {
		t.Errorf("negative: no server path in the answer: %v", res)
	}
	var a Attachment
	if err := w.db.Collection("attachments").FindOne(ctx, bson.M{"_id": res["_id"]}).Decode(&a); err != nil {
		t.Fatal(err)
	}
	if a.Meta.CardID != rb.Card || a.Meta.BoardID != rb.ID || a.Meta.ListID != rb.List || a.Meta.SwimlaneID != rb.Swimlane {
		t.Errorf("stamped with its card's place: %+v", a.Meta)
	}
	p, _ := w.s.attachmentFile(&a)
	if b, _ := os.ReadFile(p); string(b) != "abcdefgh" {
		t.Errorf("the bytes in order: %q", b)
	}
	added := false
	for _, r := range historyRows(t, w, rb.ID) {
		added = added || (r["entityType"] == "attachment" && r["changeType"] == "added")
	}
	if !added {
		t.Error("the upload is a History row")
	}
	// Negatives of the protocol.
	if _, out := methodCall(t, w, ac, "_FilesCollectionWrite_attachments", ddpChunk("never", 1, "x")); out["error"] != float64(408) {
		t.Errorf("a write without a start: %v", out)
	}
	if _, out := methodCall(t, w, ac, "_FilesCollectionWrite_attachments", ddpChunk("fileOne", 1, "x")); out["error"] != float64(408) {
		t.Errorf("a finished upload takes no more chunks: %v", out)
	}
	if _, out := methodCall(t, w, ac, "_FilesCollectionStart_attachments", ddpStart(a.ID, rb.Card, "")); out["error"] != float64(400) {
		t.Errorf("data substitution: a stored id cannot be uploaded again: %v", out)
	}
	_, _ = methodCall(t, w, ac, "_FilesCollectionStart_attachments", ddpStart("../../../../etc/x", rb.Card, ""))
	if _, err := os.Stat(filepath.Join(w.s.attachmentsDir(), "..", "..", "etc")); err == nil {
		t.Error("negative: a fileId is never a path")
	}
	bob := w.seedUser(t, "bob", "pw")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": rb.ID}, bson.M{"$push": bson.M{"members": bson.M{"userId": bob.ID, "isActive": true, "isAdmin": false}}})
	_, _ = methodCall(t, w, ac, "_FilesCollectionStart_attachments", ddpStart("annsFile", rb.Card, ""))
	bc := w.client(t)
	body(t, w.signIn(t, bc, "bob", "pw"))
	if _, out := methodCall(t, w, bc, "_FilesCollectionWrite_attachments", ddpChunk("annsFile", 1, "x")); out["error"] != float64(408) {
		t.Errorf("negative: another user cannot write into an upload: %v", out)
	}
	if _, out := methodCall(t, w, ac, "_FilesCollectionWrite_attachments", `[{"fileId":"annsFile","chunkId":999999999,"binData":"eA=="}]`); out["error"] != float64(400) {
		t.Errorf("negative: a chunk past the size limit: %v", out)
	}
	// Admin Panel's upload switch stops it, unrecorded.
	_, _ = w.db.Collection("attachmentStorageSettings").UpdateOne(ctx, bson.M{}, bson.M{"$set": bson.M{"limitSettings.attachmentsUploadBlocked": true}},
		optionsUpsert())
	if _, out := methodCall(t, w, ac, "_FilesCollectionStart_attachments", ddpStart("blocked", rb.Card, "")); out["error"] != float64(403) {
		t.Errorf("uploads off: %v", out)
	}
	if n, _ := w.db.Collection("eventlog").CountDocuments(ctx, bson.M{"bleed": "UploadBleed"}); n != 0 {
		t.Errorf("negative: uploads turned off is no attack: %d", n)
	}
}

// UploadBleed: another board's card; StampBleed: one's own card stamped with
// another board. Both refused at the start with 403, recorded, nothing
// stored, and - signed-in attempts of high and critical severity - the
// account blocked.
func TestDDPUploadBleeds(t *testing.T) {
	w := newTestWeKan(t)
	w.s.FilesDir = filepath.Join(t.TempDir(), "files")
	ctx := context.Background()
	for _, name := range []string{"ann", "cat", "owner"} {
		w.seedUser(t, name, "pw")
	}
	other := newRESTBoard(t, w, login(t, w, "owner"), "Private")
	for _, c := range []struct {
		user, bleed string
		card, stamp func(own restBoard) (string, string)
	}{
		{"ann", "UploadBleed", func(own restBoard) (string, string) { return other.Card, other.ID }, nil},
		{"cat", "StampBleed", func(own restBoard) (string, string) { return own.Card, other.ID }, nil},
	} {
		own := newRESTBoard(t, w, login(t, w, c.user), "Own")
		cl := w.client(t)
		body(t, w.signIn(t, cl, c.user, "pw"))
		card, stamp := c.card(own)
		if _, out := methodCall(t, w, cl, "_FilesCollectionStart_attachments", ddpStart("f"+c.user, card, stamp)); out["error"] != float64(403) {
			t.Errorf("%s refused: %v", c.bleed, out)
		}
		if n, _ := w.db.Collection("attachments").CountDocuments(ctx, bson.M{"meta.boardId": other.ID}); n != 0 {
			t.Errorf("%s: nothing stored", c.bleed)
		}
		var row bson.M
		if w.db.Collection("eventlog").FindOne(ctx, bson.M{"bleed": c.bleed, "source": "upload:attachments"}).Decode(&row) != nil {
			t.Errorf("%s recorded", c.bleed)
		}
		var u User
		_ = w.db.Collection("users").FindOne(ctx, bson.M{"username": c.user}).Decode(&u)
		if !u.LoginDisabled {
			t.Errorf("%s: the account is blocked", c.bleed)
		}
	}
}

// /checklists/insert: on one's own card, stamped with the card's board; a
// boardId of another board is StampBleed, refused and recorded without an
// account; a card one may not edit is refused.
func TestInsertChecklistStamp(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	w.seedUser(t, "owner", "pw")
	own := newRESTBoard(t, w, login(t, w, "ann"), "Own")
	other := newRESTBoard(t, w, login(t, w, "owner"), "Other")
	ac := w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	if _, out := methodCall(t, w, ac, "/checklists/insert", `[{"_id":"clMine","cardId":"`+own.Card+`","title":"Mine","sort":0}]`); out["result"] != "clMine" {
		t.Fatalf("inserted: %v", out)
	}
	var cl bson.M
	_ = w.db.Collection("checklists").FindOne(ctx, bson.M{"_id": "clMine"}).Decode(&cl)
	if cl["boardId"] != own.ID {
		t.Errorf("stamped with the card's board: %v", cl["boardId"])
	}
	if n, _ := w.db.Collection("activities").CountDocuments(ctx, bson.M{"activityType": "addChecklist", "checklistId": "clMine"}); n != 1 {
		t.Error("an addChecklist activity")
	}
	if _, out := methodCall(t, w, ac, "/checklists/insert", `[{"cardId":"`+own.Card+`","boardId":"`+other.ID+`","title":"Stamped"}]`); out["reason"] != "Access denied" {
		t.Errorf("a stamp of another board: %v", out)
	}
	if n, _ := w.db.Collection("checklists").CountDocuments(ctx, bson.M{"boardId": other.ID}); n != 0 {
		t.Error("nothing on the other board")
	}
	var row bson.M
	if w.db.Collection("eventlog").FindOne(ctx, bson.M{"bleed": "StampBleed", "source": "ddp:checklist-insert"}).Decode(&row) != nil {
		t.Error("StampBleed recorded")
	}
	var u User
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"username": "ann"}).Decode(&u)
	if u.LoginDisabled {
		t.Error("negative: a refusal that names no account blocks none")
	}
	if _, out := methodCall(t, w, ac, "/checklists/insert", `[{"cardId":"`+other.Card+`","title":"Theirs"}]`); out["reason"] != "Access denied" {
		t.Errorf("negative: a card one may not edit: %v", out)
	}
	if _, out := methodCall(t, w, ac, "/checklists/insert", `[{"cardId":"`+own.Card+`","title":"  "}]`); out["error"] != float64(400) {
		t.Errorf("negative: an empty title: %v", out)
	}
}

// Admin Panel's two upload switches, each on its own paths, as WeKan's:
// attachmentsUploadBlocked stops the pages' uploads (the card's form and
// Meteor-Files'), apiUploadBlocked the REST API's.
func TestUploadSwitches(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.s.FilesDir = filepath.Join(t.TempDir(), "files")
	w.seedUser(t, "ann", "pw")
	tok := login(t, w, "ann")
	rb := newRESTBoard(t, w, tok, "Switch")
	ac := w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	form := func() int {
		var buf strings.Builder
		mw := multipart.NewWriter(&buf)
		fw, _ := mw.CreateFormFile("file", "a.txt")
		_, _ = fw.Write([]byte("abc"))
		_ = mw.Close()
		req, _ := http.NewRequest("POST", w.srv.URL+"/b/"+rb.ID+"/cards/"+rb.Card+"/attachments", strings.NewReader(buf.String()))
		req.Header.Set("Content-Type", mw.FormDataContentType())
		res, err := noRedirect(ac).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	rest := func() (int, map[string]any) {
		var out map[string]any
		code := call(t, "POST", w.srv.URL+"/api/attachment/upload", tok, `{"boardId":"`+rb.ID+`","cardId":"`+rb.Card+`","fileName":"a.txt","fileType":"text/plain","fileData":"YWJj"}`, &out)
		return code, out
	}
	set := func(field string, on bool) {
		_, _ = w.db.Collection("attachmentStorageSettings").UpdateOne(ctx, bson.M{}, bson.M{"$set": bson.M{"limitSettings." + field: on}}, optionsUpsert())
	}
	if form() != http.StatusSeeOther {
		t.Fatal("a form upload, switches off")
	}
	set("attachmentsUploadBlocked", true)
	if c := form(); c != http.StatusForbidden {
		t.Errorf("attachmentsUploadBlocked stops the form: %d", c)
	}
	if c, _ := rest(); c != 200 {
		t.Errorf("negative: nor the REST API, which has its own switch: %d", c)
	}
	set("attachmentsUploadBlocked", false)
	set("apiUploadBlocked", true)
	if c, out := rest(); c != http.StatusForbidden || !strings.Contains(jsonOf(out), "API uploads are disabled by administrator") {
		t.Errorf("apiUploadBlocked stops the REST API: %d %v", c, out)
	}
	if form() != http.StatusSeeOther {
		t.Error("negative: but not the form")
	}
}

func jsonOf(v any) string { b, _ := json.Marshal(v); return string(b) }

// noRedirect is the client, answering a redirect itself instead of following it.
func noRedirect(c *http.Client) *http.Client {
	cp := *c
	cp.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &cp
}
