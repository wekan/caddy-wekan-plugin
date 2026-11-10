package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// WeKan's descriptionForm: the card's description saved by whoever may
// change the card - not a Worker, not on a board that hides descriptions -
// a change writing a-changedDescription, and the text shown as markdown.
func TestCardDescription(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	wk := w.seedUser(t, "wes", "pw")
	ro := w.seedUser(t, "rob", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Desc")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$push": bson.M{"members": bson.M{"$each": []any{
		bson.M{"userId": wk.ID, "isActive": true, "isWorker": true}, bson.M{"userId": ro.ID, "isActive": true, "isReadOnly": true}}}}})
	l, _ := w.s.AddList(ctx, b, "L")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	c, _ := w.s.AddCard(ctx, b, u, l.ID, sw.ID, "C")
	path := w.srv.URL + "/b/" + b.ID + "/cards/" + c.ID + "/description"
	desc := func() string {
		var d Card
		_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": c.ID}).Decode(&d)
		return d.Description
	}
	post := func(cl *http.Client, v string) int {
		r, err := cl.PostForm(path, url.Values{"description": {v}})
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	if _, html := get(t, ann, w.srv.URL+"/b/"+b.ID+"/"+b.Slug+"/"+c.ID); !strings.Contains(html, "js-card-description") || !strings.Contains(html, "fa-pencil-square-o") {
		t.Error("the editor's pencil and form")
	}
	if code := post(ann, "**Bold** words\n"); code != http.StatusSeeOther || desc() != "**Bold** words" {
		t.Fatalf("saved: %d %q", code, desc())
	}
	var act bson.M
	if w.db.Collection("activities").FindOne(ctx, bson.M{"cardId": c.ID, "activityType": "a-changedDescription"}).Decode(&act) != nil || act["value"] != "**Bold** words" {
		t.Errorf("a-changedDescription: %v", act)
	}
	_ = post(ann, "**Bold** words")
	if n, _ := w.db.Collection("activities").CountDocuments(ctx, bson.M{"cardId": c.ID, "activityType": "a-changedDescription"}); n != 1 {
		t.Errorf("negative: no activity for the same text: %d", n)
	}
	rob := w.client(t)
	body(t, w.signIn(t, rob, "rob", "pw"))
	if _, html := get(t, rob, w.srv.URL+"/b/"+b.ID+"/"+b.Slug+"/"+c.ID); !strings.Contains(html, "<strong>Bold</strong>") || strings.Contains(html, "js-card-description") {
		t.Error("a reader sees the markdown, without the editor")
	}
	// Negatives: a Worker, a read-only member, too long, a board without descriptions.
	wes := w.client(t)
	body(t, w.signIn(t, wes, "wes", "pw"))
	if code := post(wes, "Worker's"); code != http.StatusForbidden {
		t.Errorf("negative: a Worker: %d", code)
	}
	if code := post(rob, "Reader's"); code != http.StatusForbidden {
		t.Errorf("negative: a read-only member: %d", code)
	}
	if code := post(ann, strings.Repeat("x", 100001)); code != http.StatusBadRequest {
		t.Errorf("negative: too long: %d", code)
	}
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"allowsDescriptionText": false}})
	if code := post(ann, "Hidden"); code != http.StatusForbidden || desc() != "**Bold** words" {
		t.Errorf("negative: the board hides descriptions: %d", code)
	}
}
