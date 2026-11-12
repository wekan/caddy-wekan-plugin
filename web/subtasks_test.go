package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// WeKan's addSubtaskCard: the parent board's subtasks board made once, with
// its members, a Default swimlane and a Queue list it then names; the subtask
// there, its parent named, the parent's labels when asked, the board's
// automatic custom fields, a unique sort; never on a board its maker may not
// write (SubtaskDepositBleed).
func TestAddSubtask(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	ann := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Parent")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": rb.ID}, bson.M{"$push": bson.M{"members": bson.M{"userId": bob.ID, "isActive": true, "isAdmin": false, "isCommentOnly": true}}})
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": rb.Card}, bson.M{"$set": bson.M{"labelIds": bson.A{"lab1"}}})
	ac := w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	add := func(cl *http.Client, title string, inherit bool) map[string]any {
		in := "false"
		if inherit {
			in = "true"
		}
		_, out := methodCall(t, w, cl, "addSubtaskCard", `["`+rb.Card+`","`+title+`",`+in+`]`)
		return out
	}
	first := add(ac, "First", true)
	id1, _ := first["result"].(string)
	if id1 == "" {
		t.Fatalf("a subtask: %v", first)
	}
	var parent bson.M
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": rb.ID}).Decode(&parent)
	subID, _ := parent["subtasksDefaultBoardId"].(string)
	var sub Board
	if w.db.Collection("boards").FindOne(ctx, bson.M{"_id": subID}).Decode(&sub) != nil || sub.Title != "^Parent^" {
		t.Fatalf("the subtasks board: %+v", sub)
	}
	if !sub.MemberOf(bob.ID) || !sub.AdminOf(ann.ID) || len(sub.Members) != 2 {
		t.Errorf("with the board's members: %+v", sub.Members)
	}
	var subDoc bson.M
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": subID}).Decode(&subDoc)
	queue, _ := subDoc["subtasksDefaultListId"].(string)
	var c1 bson.M
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": id1}).Decode(&c1)
	if c1["parentId"] != rb.Card || c1["boardId"] != subID || c1["listId"] != queue || queue == "" {
		t.Errorf("on the subtasks board's Queue, its parent named: %v", c1)
	}
	if l, _ := c1["labelIds"].(bson.A); len(l) != 1 || l[0] != "lab1" {
		t.Errorf("#2184: the parent's labels: %v", c1["labelIds"])
	}
	// A field automatically on cards of the subtasks board starts on the next.
	_, _ = w.db.Collection("customFields").InsertOne(ctx, bson.M{"_id": "autoField01", "boardIds": bson.A{subID}, "name": "Auto", "type": "text", "automaticallyOnCard": true})
	id2, _ := add(ac, "Second", false)["result"].(string)
	var c2 bson.M
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": id2}).Decode(&c2)
	if c2["boardId"] != subID || c2["listId"] != queue {
		t.Error("the same subtasks board and list, made once")
	}
	if l, _ := c2["labelIds"].(bson.A); len(l) != 0 {
		t.Errorf("no labels unless asked: %v", c2["labelIds"])
	}
	if f, _ := c2["customFields"].(bson.A); len(f) != 1 {
		t.Errorf("#4037: the automatic custom field: %v", c2["customFields"])
	}
	if c2["sort"] == c1["sort"] {
		t.Error("#3826: a sort of its own")
	}
	if n, _ := w.db.Collection("boards").CountDocuments(ctx, bson.M{"title": "^Parent^"}); n != 1 {
		t.Errorf("one subtasks board: %d", n)
	}
	if empty := add(ac, "   ", false); empty["result"] != nil || empty["error"] != nil {
		t.Errorf("an empty title makes nothing: %v", empty)
	}
	// The card page lists it and offers WeKan's form.
	_, html := get(t, ac, w.srv.URL+"/b/"+rb.ID+"/x/"+rb.Card)
	for _, want := range []string{"Second", `title="Add Subtask"`, "js-add-subtask-item", "subtask-inherit-labels"} {
		if !strings.Contains(html, want) {
			t.Errorf("the card page has %s", want)
		}
	}
	// The form makes one too.
	res, _ := noRedirect(ac).PostForm(w.srv.URL+"/b/"+rb.ID+"/cards/"+rb.Card+"/subtasks", url.Values{"title": {"Third"}, "inheritLabels": {"on"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("the form: %d", res.StatusCode)
	}
	if n, _ := w.db.Collection("cards").CountDocuments(ctx, bson.M{"parentId": rb.Card, "title": "Third"}); n != 1 {
		t.Error("the form's subtask")
	}
	// Negative: a comment-only member may not add one.
	bc := w.client(t)
	body(t, w.signIn(t, bc, "bob", "pw"))
	if out := add(bc, "Not mine", false); out["error"] != "not-authorized" {
		t.Errorf("negative: comment-only: %v", out)
	}
	// Negative: a subtasks board ann may not write is SubtaskDepositBleed.
	w.seedUser(t, "eve", "pw")
	foreign := newRESTBoard(t, w, login(t, w, "eve"), "Foreign")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": rb.ID}, bson.M{"$set": bson.M{"subtasksDefaultBoardId": foreign.ID}})
	if out := add(ac, "Smuggled", false); out["error"] != "not-authorized" {
		t.Errorf("negative: an unwritable deposit: %v", out)
	}
	if n, _ := w.db.Collection("cards").CountDocuments(ctx, bson.M{"boardId": foreign.ID, "title": "Smuggled"}); n != 0 {
		t.Error("nothing on the foreign board")
	}
	if n, _ := w.db.Collection("eventlog").CountDocuments(ctx, bson.M{"bleed": "SubtaskDepositBleed"}); n == 0 {
		t.Error("recorded as SubtaskDepositBleed")
	}
}
