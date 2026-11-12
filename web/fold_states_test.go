package web

import (
	"context"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// WeKan's folds, each the user's own: a swimlane folded keeps its header and
// shows no lists (no #swimlane-<id>), a minicard folded is minicard-collapsed,
// a minicard checklist folded shows no items; a key that is no safe map key
// is PrototypeBleed, and a board one cannot see is not folded.
func TestFoldStates(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Folds")
	_, _ = w.db.Collection("checklists").InsertOne(ctx, bson.M{"_id": "clFold", "cardId": rb.Card, "boardId": rb.ID, "title": "Foldable", "sort": 0, "showChecklistAtMinicard": true})
	_, _ = w.db.Collection("checklistItems").InsertOne(ctx, bson.M{"_id": "ciFold", "checklistId": "clFold", "cardId": rb.Card, "boardId": rb.ID, "title": "Hidden item", "sort": 0})
	ac := w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	page := func() string { _, html := get(t, ac, w.srv.URL+"/b/"+rb.ID+"/x"); return html }
	html := page()
	for _, want := range []string{`id="swimlane-` + rb.Swimlane + `"`, "js-collapse-swimlane", "js-collapse-minicard", "js-collapse-checklist", "Hidden item"} {
		if !strings.Contains(html, want) {
			t.Fatalf("unfolded, the page has %s", want)
		}
	}
	for method, args := range map[string]string{
		"setCardCollapsedState":   `["` + rb.ID + `","` + rb.Card + `",true]`,
		"setCollapsedCardSection": `["` + rb.Card + `","checklist-clFold",true]`,
	} {
		if _, out := methodCall(t, w, ac, method, args); out["error"] != nil {
			t.Fatalf("%s: %v", method, out)
		}
	}
	html = page()
	if !strings.Contains(html, "minicard-collapsed") || strings.Contains(html, "Hidden item") {
		t.Error("the minicard and its checklist are folded")
	}
	if _, out := methodCall(t, w, ac, "setSwimlaneCollapsedState", `["`+rb.ID+`","`+rb.Swimlane+`",true]`); out["error"] != nil {
		t.Fatalf("fold the swimlane: %v", out)
	}
	html = page()
	if strings.Contains(html, `id="swimlane-`+rb.Swimlane+`"`) || strings.Contains(html, `id="js-list-`+rb.List+`"`) || !strings.Contains(html, `class="swimlane-header js-swimlane-header" data-swimlane="`+rb.Swimlane+`"`) {
		t.Error("a folded swimlane keeps its header and shows no lists")
	}
	_, _ = methodCall(t, w, ac, "setSwimlaneCollapsedState", `["`+rb.ID+`","`+rb.Swimlane+`",false]`)
	if !strings.Contains(page(), `id="swimlane-`+rb.Swimlane+`"`) {
		t.Error("unfolded again")
	}
	// In order: the account is blocked by the first PrototypeBleed, so the
	// prototype keys come last.
	for _, c := range []struct{ name, method, args, err string }{
		{"not a section", "setCollapsedCardSection", `["` + rb.Card + `","nonsense",true]`, "Match.Error"},
		{"not a boolean", "setCardCollapsedState", `["` + rb.ID + `","` + rb.Card + `","yes"]`, "Match.Error"},
		{"a board one cannot see", "setSwimlaneCollapsedState", `["noSuchBoard12345","` + rb.Swimlane + `",true]`, "not-found"},
		{"a prototype key", "setSwimlaneCollapsedState", `["` + rb.ID + `","__proto__",true]`, "invalid-map-key"},
	} {
		if _, out := methodCall(t, w, ac, c.method, c.args); out["error"] != c.err {
			t.Errorf("negative: %s: %v", c.name, out)
		}
	}
	if n, _ := w.db.Collection("eventlog").CountDocuments(ctx, bson.M{"bleed": "PrototypeBleed"}); n == 0 {
		t.Error("a prototype key is recorded as PrototypeBleed")
	}
}
