package web

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestRESTItemsCommentsCards(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	a, bt := login(t, w, "ann"), login(t, w, "bob")
	b := newRESTBoard(t, w, a, "Items")
	ctx := context.Background()
	board := w.srv.URL + "/api/boards/" + b.ID
	var cl struct {
		ID string `json:"_id"`
	}
	call(t, "POST", board+"/cards/"+b.Card+"/checklists", a, `{"title":"CL","items":["one"]}`, &cl)
	var it bson.M
	_ = w.db.Collection("checklistItems").FindOne(ctx, bson.M{"checklistId": cl.ID}).Decode(&it)
	item := board + "/cards/" + b.Card + "/checklists/" + cl.ID + "/items/" + it["_id"].(string)

	// Checklist items: read, edit (title, isFinished, dueAt), delete.
	var got map[string]any
	if code := call(t, "GET", item, a, "", &got); code != 200 || got["title"] != "one" {
		t.Fatalf("an item: %d %v", code, got)
	}
	if code := call(t, "PUT", item, a, `{"title":"uno","isFinished":"true","dueAt":"2030-01-02T03:04:05Z"}`, nil); code != 200 {
		t.Fatalf("edit: %d", code)
	}
	_ = w.db.Collection("checklistItems").FindOne(ctx, bson.M{"_id": it["_id"]}).Decode(&it)
	if it["title"] != "uno" || it["isFinished"] != true || it["dueAt"] == nil {
		t.Fatalf("edited: %v", it)
	}
	call(t, "PUT", item, a, `{"dueAt":null}`, nil)
	id := it["_id"]
	it = bson.M{} // a fresh map: Decode does not remove the keys a map already has
	_ = w.db.Collection("checklistItems").FindOne(ctx, bson.M{"_id": id}).Decode(&it)
	if _, has := it["dueAt"]; has {
		t.Fatal("dueAt null clears it")
	}
	for _, bad := range []string{`{"dueAt":"tomorrow"}`, `{"dueAt":"2030-02-30T00:00:00Z"}`, `{"title":""}`} {
		if code := call(t, "PUT", item, a, bad, nil); code != 400 {
			t.Fatalf("negative: %s: %d", bad, code)
		}
	}
	if code := call(t, "PUT", board+"/cards/"+b.Card+"/checklists/othercl00000001/items/"+it["_id"].(string), a, `{"title":"x"}`, nil); code != 404 {
		t.Fatalf("negative: an item of another checklist: %d", code)
	}
	if code := call(t, "DELETE", item, bt, "", nil); code != 403 {
		t.Fatalf("negative: a non-member deletes nothing: %d", code)
	}
	if code := call(t, "DELETE", item, a, "", nil); code != 200 || w.db.Collection("checklistItems").FindOne(ctx, bson.M{"_id": it["_id"]}).Err() == nil {
		t.Fatalf("delete: %d", code)
	}

	// Comments: the author edits and deletes their own; another member may not,
	// and the attempt is a CommentBleed canary; a board admin may, unless the
	// board restricts comment editing.
	setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: true})
	var cm struct {
		ID string `json:"_id"`
	}
	call(t, "POST", board+"/cards/"+b.Card+"/comments", a, `{"comment":"by ann"}`, &cm)
	comment := board + "/cards/" + b.Card + "/comments/" + cm.ID
	if code := call(t, "GET", comment, bt, "", &got); code != 200 || got["text"] != "by ann" {
		t.Fatalf("a member reads a comment: %d", code)
	}
	if code := call(t, "PUT", comment, bt, `{"comment":"bob was here"}`, nil); code != 403 {
		t.Fatalf("negative: another member edits: %d", code)
	}
	if code := call(t, "DELETE", comment, bt, "", nil); code != 403 {
		t.Fatalf("negative: another member deletes: %d", code)
	}
	var row bson.M
	if err := w.db.Collection("eventlog").FindOne(ctx, bson.M{"bleed": "CommentBleed"}).Decode(&row); err != nil || row["action"] != "detected" {
		t.Fatalf("the attempts are a canary in Admin Panel -> Problems: %v %v", err, row)
	}
	if code := call(t, "GET", w.srv.URL+"/api/user", bt, "", nil); code != 200 {
		t.Fatal("negative: a medium-severity canary does not disable the account")
	}
	if code := call(t, "PUT", comment, a, `{"comment":"  edited  "}`, nil); code != 200 {
		t.Fatalf("the author edits: %d", code)
	}
	call(t, "GET", comment, a, "", &got)
	if got["text"] != "edited" {
		t.Fatalf("trimmed: %v", got["text"])
	}
	if code := call(t, "PUT", comment, a, `{"comment":"   "}`, nil); code != 400 {
		t.Fatalf("negative: an empty comment: %d", code)
	}
	var bc struct {
		ID string `json:"_id"`
	}
	call(t, "POST", board+"/cards/"+b.Card+"/comments", bt, `{"comment":"by bob"}`, &bc)
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"restrictCommentEditing": true}})
	if code := call(t, "DELETE", board+"/cards/"+b.Card+"/comments/"+bc.ID, a, "", nil); code != 403 {
		t.Fatalf("negative: restrictCommentEditing stops even a board admin: %d", code)
	}
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"restrictCommentEditing": false}})
	if code := call(t, "DELETE", board+"/cards/"+b.Card+"/comments/"+bc.ID, a, "", nil); code != 200 {
		t.Fatalf("a board admin deletes another's comment: %d", code)
	}
	if code := call(t, "GET", board+"/cards/"+b.Card+"/comments/"+bc.ID, a, "", nil); code != 404 {
		t.Fatalf("deleted: %d", code)
	}

	// Cards: assignees, a custom field value, archive and restore, by swimlane,
	// by a custom field, by id with checklists.
	card := board + "/lists/" + b.List + "/cards/" + b.Card
	var res map[string]any
	if code := call(t, "POST", card+"/assignees/"+bob.ID, a, "", &res); code != 200 || len(res["assignees"].([]any)) != 1 {
		t.Fatalf("assign: %d %v", code, res)
	}
	if code := call(t, "POST", card+"/assignees/nosuchuser00001", a, "", nil); code != 400 {
		t.Fatalf("negative: only a board member is assigned: %d", code)
	}
	call(t, "DELETE", card+"/assignees/"+bob.ID, a, "", &res)
	if len(res["assignees"].([]any)) != 0 {
		t.Fatalf("unassigned: %v", res)
	}
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": b.Card}, bson.M{"$set": bson.M{"customFields": bson.A{bson.M{"_id": "field0000000001", "value": nil}}}})
	if code := call(t, "POST", card+"/customFields/field0000000001", a, `{"value":"red"}`, nil); code != 200 {
		t.Fatalf("a custom field value: %d", code)
	}
	if code := call(t, "POST", card+"/customFields/field0000000001", a, `{"value":{"$ne":1}}`, nil); code != 400 {
		t.Fatalf("negative: an object as a value: %d", code)
	}
	var byField []map[string]any
	if code := call(t, "GET", board+"/cardsByCustomField/field0000000001/red", a, "", &byField); code != 200 || len(byField) != 1 {
		t.Fatalf("cards by a custom field: %d %v", code, byField)
	}
	var bySwimlane []map[string]any
	if code := call(t, "GET", board+"/swimlanes/"+b.Swimlane+"/cards", a, "", &bySwimlane); code != 200 || len(bySwimlane) != 1 || bySwimlane[0]["listId"] != b.List {
		t.Fatalf("a swimlane's cards: %d %v", code, bySwimlane)
	}
	var byID map[string]any
	if code := call(t, "GET", w.srv.URL+"/api/cards/"+b.Card, a, "", &byID); code != 200 || byID["checklists"] == nil {
		t.Fatalf("a card by id, with its checklists: %d %v", code, byID)
	}
	if code := call(t, "GET", w.srv.URL+"/api/cards/"+b.Card, login(t, w, "ann"), "", nil); code != 200 {
		t.Fatal("again")
	}
	w.seedUser(t, "cid", "pw")
	if code := call(t, "GET", w.srv.URL+"/api/cards/"+b.Card, login(t, w, "cid"), "", nil); code != 403 {
		t.Fatalf("negative: a card by id on a board the caller cannot read: %d", code)
	}
	if code := call(t, "POST", card+"/archive", a, "", &res); code != 200 || res["archived"] != true {
		t.Fatalf("archive: %d %v", code, res)
	}
	if code := call(t, "POST", card+"/archive", a, "", nil); code != 404 {
		t.Fatalf("negative: an archived card is not archived again: %d", code)
	}
	if code := call(t, "POST", card+"/unarchive", a, "", &res); code != 200 || res["archived"] != false {
		t.Fatalf("unarchive: %d %v", code, res)
	}
}
