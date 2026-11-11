package web

import (
	"context"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// UnassignedAttachBleed (GHSA-w26g-rc43-q6cm): an assigned-only member who
// kept a card's and an attachment's ids after the assignment was removed
// uploads nothing to it and deletes nothing from it - through REST or DDP -
// and each attempt is in Admin Panel -> Problems, without disabling the
// account; the board's owner still deletes.
func TestUnassignedAttachBleed(t *testing.T) {
	w := newTestWeKan(t)
	w.s.FilesDir = t.TempDir()
	u := w.seedUser(t, "ann", "pw")
	am := w.seedUser(t, "amy", "pw")
	ro := w.seedUser(t, "rob", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Attach")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$push": bson.M{"members": bson.M{"$each": []any{
		bson.M{"userId": am.ID, "isActive": true, "isNormalAssignedOnly": true}, bson.M{"userId": ro.ID, "isActive": true, "isReadOnly": true}}}}})
	l, _ := w.s.AddList(ctx, b, "L")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	c, _ := w.s.AddCard(ctx, b, u, l.ID, sw.ID, "C")
	_, _ = w.db.Collection("attachments").InsertOne(ctx, bson.M{"_id": "existing01", "name": "e.txt", "size": 1, "type": "text/plain", "userId": u.ID,
		"meta": bson.M{"boardId": b.ID, "cardId": c.ID, "listId": l.ID, "swimlaneId": sw.ID}})
	amyTok := login(t, w, "amy")
	upload := func(tok, name string) int {
		req, _ := http.NewRequest("POST", w.srv.URL+"/api/attachment/upload?boardId="+b.ID+"&swimlaneId="+sw.ID+"&listId="+l.ID+"&cardId="+c.ID+"&fileName="+name+"&fileType=text/plain", strings.NewReader("synthetic"))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "text/plain")
		res, err := w.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	del := func(tok string) int {
		req, _ := http.NewRequest("DELETE", w.srv.URL+"/api/attachment/delete/existing01", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		res, err := w.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	deleted := func() bool {
		var a bson.M
		_ = w.db.Collection("attachments").FindOne(ctx, bson.M{"_id": "existing01"}).Decode(&a)
		return a["deletedAt"] != nil
	}
	events := func(source string) int64 {
		n, _ := w.db.Collection("eventlog").CountDocuments(ctx, bson.M{"bleed": "UnassignedAttachBleed", "source": source})
		return n
	}
	// While assigned, the member uploads (positive).
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": c.ID}, bson.M{"$set": bson.M{"assignees": bson.A{am.ID}}})
	if code := upload(amyTok, "assigned.txt"); code != http.StatusOK {
		t.Fatalf("while assigned: %d", code)
	}
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": c.ID}, bson.M{"$set": bson.M{"assignees": bson.A{}}})
	if code := upload(amyTok, "after.txt"); code != http.StatusForbidden {
		t.Errorf("negative: upload after unassignment: %d", code)
	}
	if code := del(amyTok); code != http.StatusForbidden || deleted() {
		t.Errorf("negative: delete after unassignment: %d", code)
	}
	amy := w.client(t)
	body(t, w.signIn(t, amy, "amy", "pw"))
	req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/api.attachment.delete", strings.NewReader(`["existing01"]`))
	req.Header.Set("Content-Type", "application/json")
	res, err := amy.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out := body(t, res)
	if !strings.Contains(out, "not-authorized") || deleted() {
		t.Errorf("negative: the DDP twin refuses too: %s", out)
	}
	for _, src := range []string{"attachment-api:upload", "attachment-api:delete", "attachment-method:delete"} {
		if events(src) < 1 {
			t.Errorf("the attempt in Admin Panel -> Problems: %s", src)
		}
	}
	var amyDoc bson.M
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"_id": am.ID}).Decode(&amyDoc)
	if amyDoc["loginDisabled"] == true {
		t.Error("negative: medium severity never disables the account")
	}
	// Negative: a read-only member's refusal is not this bleed.
	rob := w.client(t)
	body(t, w.signIn(t, rob, "rob", "pw"))
	before := events("attachment-method:delete")
	r2, _ := rob.PostForm(w.srv.URL+"/attachments/existing01/delete", nil)
	r2.Body.Close()
	if r2.StatusCode != http.StatusForbidden || events("attachment-method:delete") != before {
		t.Errorf("negative: a read-only member's refusal is not recorded as UnassignedAttachBleed: %d", r2.StatusCode)
	}
	// The owner still deletes, through the method.
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	req, _ = http.NewRequest("POST", w.srv.URL+"/_methods/api.attachment.delete", strings.NewReader(`["existing01"]`))
	req.Header.Set("Content-Type", "application/json")
	if res, err := ann.Do(req); err != nil || res.StatusCode != 200 || !deleted() {
		t.Errorf("the owner deletes: %v", err)
	}
}

// Negative, tree-wide: every attachment write that refuses with
// boardCard's or WritableIn's answer records an assigned-only attempt.
func TestEveryAttachmentWriteRecordsAssignedOnly(t *testing.T) {
	src, err := os.ReadFile("attachments.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for _, fn := range []string{"apiUploadAttachment", "apiDeleteAttachment", "uploadAttachment", "deleteAttachmentPage", "methodAttachmentDelete"} {
		m := regexp.MustCompile(`(?s)func \(s \*Server\) ` + fn + `\(.*?\n}\n`).FindString(s)
		if m == "" {
			t.Errorf("%s is where the test looks for it", fn)
			continue
		}
		if !strings.Contains(m, "noteAssignedAttachmentWrite(") {
			t.Errorf("%s refuses without recording an assigned-only attempt", fn)
		}
	}
}

// WeKan's attachmentGallery: a tile per attachment with its size and
// download link; the "+" and cardAttachmentsPopup for whoever may change
// the card, not a read-only member; and the viewer overlay, hidden.
func TestAttachmentGallery(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	ro := w.seedUser(t, "rob", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Gallery")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$push": bson.M{"members": bson.M{"userId": ro.ID, "isActive": true, "isReadOnly": true}}})
	l, _ := w.s.AddList(ctx, b, "L")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	c, _ := w.s.AddCard(ctx, b, u, l.ID, sw.ID, "C")
	_, _ = w.db.Collection("attachments").InsertOne(ctx, bson.M{"_id": "pdf0000001", "name": "spec.pdf", "size": 2048, "type": "application/pdf", "userId": u.ID,
		"meta": bson.M{"boardId": b.ID, "cardId": c.ID, "listId": l.ID, "swimlaneId": sw.ID}})
	cardURL := w.srv.URL + "/b/" + b.ID + "/" + b.Slug + "/" + c.ID
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	_, html := get(t, ann, cardURL)
	for _, want := range []string{"card-attachmentGallery", "js-add-attachment", `data-kind="pdf"`, "(2.0 KB)", "?download=true", `id="viewer-overlay" class="hidden"`, `data-pop="card-attachments"`, "js-attach-file"} {
		if !strings.Contains(html, want) {
			t.Errorf("the editor's gallery: %s", want)
		}
	}
	rob := w.client(t)
	body(t, w.signIn(t, rob, "rob", "pw"))
	_, html = get(t, rob, cardURL)
	if !strings.Contains(html, "spec.pdf") || strings.Contains(html, "js-add-attachment") || strings.Contains(html, "js-attach-file") || strings.Contains(html, "js-confirm-delete") {
		t.Error("negative: a reader sees the attachment, without adding or deleting")
	}
}

func TestAttachmentSizeText(t *testing.T) {
	for _, c := range []struct {
		n    int64
		want string
	}{{512, "512 B"}, {2048, "2.0 KB"}, {5 << 20, "5.0 MB"}, {3 << 30, "3.0 GB"}} {
		if got := (Attachment{Size: c.n}).SizeText(); got != c.want {
			t.Errorf("%d: %q", c.n, got)
		}
	}
}
