package web

import (
	"context"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// structuralMoveOptions, the mixed selection's Move pop-over: the writable
// boards, then step by step the target's swimlanes, lists, cards, checklists
// and items; an assigned-only member is offered only their cards; the board
// shows its checklists on minicards with a selection checkbox for writers.
func TestStructuralMoveOptions(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Moves")
	other := newRESTBoard(t, w, login(t, w, "ann"), "Elsewhere")
	_, _ = w.db.Collection("checklists").InsertOne(ctx, bson.M{"_id": "clMove", "cardId": rb.Card, "boardId": rb.ID, "title": "Steps", "sort": 0, "showChecklistAtMinicard": true})
	_, _ = w.db.Collection("checklistItems").InsertOne(ctx, bson.M{"_id": "ciMove", "checklistId": "clMove", "cardId": rb.Card, "boardId": rb.ID, "title": "Step one", "sort": 0, "isFinished": true})
	ac := w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	opts := func(target string) map[string]any {
		_, out := methodCall(t, w, ac, "structuralMoveOptions", `[`+target+`]`)
		res, _ := out["result"].(map[string]any)
		if res == nil {
			t.Fatalf("options %s: %v", target, out)
		}
		return res
	}
	ids := func(v any) []string {
		var out []string
		for _, o := range v.([]any) {
			out = append(out, o.(map[string]any)["_id"].(string))
		}
		return out
	}
	has := func(list []string, id string) bool {
		for _, x := range list {
			if x == id {
				return true
			}
		}
		return false
	}
	first := opts(`{"boardId":"` + rb.ID + `"}`)
	if b := ids(first["boardId"]); !has(b, rb.ID) || !has(b, other.ID) {
		t.Errorf("the writable boards: %v", b)
	}
	if !has(ids(first["swimlaneId"]), rb.Swimlane) || first["listId"] != nil {
		t.Errorf("the swimlanes, and no lists before one is chosen: %v", first)
	}
	deep := opts(`{"boardId":"` + rb.ID + `","swimlaneId":"` + rb.Swimlane + `","listId":"` + rb.List + `","cardId":"` + rb.Card + `","checklistId":"clMove"}`)
	if !has(ids(deep["listId"]), rb.List) || !has(ids(deep["cardId"]), rb.Card) || !has(ids(deep["checklistId"]), "clMove") || !has(ids(deep["itemId"]), "ciMove") {
		t.Errorf("each step: %v", deep)
	}
	// An assigned-only member is offered only the cards assigned to them.
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": rb.ID}, bson.M{"$push": bson.M{"members": bson.M{"userId": bob.ID, "isActive": true, "isAdmin": false, "isNormalAssignedOnly": true}}})
	bc := w.client(t)
	body(t, w.signIn(t, bc, "bob", "pw"))
	_, out := methodCall(t, w, bc, "structuralMoveOptions", `[{"boardId":"`+rb.ID+`","swimlaneId":"`+rb.Swimlane+`","listId":"`+rb.List+`","cardId":"`+rb.Card+`"}]`)
	res := out["result"].(map[string]any)
	if c, _ := res["cardId"].([]any); len(c) != 0 || res["checklistId"] != nil {
		t.Errorf("negative: an unassigned card is not offered: %v", res)
	}
	for name, target := range map[string]string{"no board": `{}`, "an unknown key": `{"boardId":"` + rb.ID + `","x":"1"}`, "not a text": `{"boardId":1}`} {
		if _, out := methodCall(t, w, ac, "structuralMoveOptions", `[`+target+`]`); out["error"] != "Match.Error" {
			t.Errorf("negative: %s: %v", name, out)
		}
	}
	w.seedUser(t, "eve", "pw")
	ec := w.client(t)
	body(t, w.signIn(t, ec, "eve", "pw"))
	if _, out := methodCall(t, w, ec, "structuralMoveOptions", `[{"boardId":"`+rb.ID+`"}]`); out["error"] != "not-authorized" {
		t.Errorf("negative: not a member: %v", out)
	}
	// The minicard shows the checklist, with its checkbox for a writer.
	_, html := get(t, ac, w.srv.URL+"/b/"+rb.ID+"/x")
	for _, want := range []string{`class="minicard-checklist"`, "Step one", `data-kind="checklist" data-id="clMove"`, `data-kind="list"`, `data-kind="swimlane"`, "js-toggle-multi-selection", "js-move-selection"} {
		if !strings.Contains(html, want) {
			t.Errorf("the board page has %s", want)
		}
	}
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": rb.ID}, bson.M{"$set": bson.M{"members.1.isNormalAssignedOnly": false, "members.1.isReadOnly": true}})
	if _, html := get(t, bc, w.srv.URL+"/b/"+rb.ID+"/x"); strings.Contains(html, "js-structural-selection") || strings.Contains(html, "js-move-selection") {
		t.Error("negative: a read-only member sees no selection checkbox and no Move")
	}
}
