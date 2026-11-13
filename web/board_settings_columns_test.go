package web

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestColumnFields(t *testing.T) {
	if got := columnFields("card", "draggable"); !slices.Equal(got, []string{"allowsCardDragging", "allowsChecklistDragging", "allowsChecklistItemDragging", "allowsSubtaskDragging"}) {
		t.Fatalf("Card's Draggable: %v", got)
	}
	if got := columnFields("list", "settings"); !slices.Equal(got, []string{"listWidthResizeLocked", "sameWidthForAllLists"}) {
		t.Fatalf("List's Settings: %v", got)
	}
	mini := columnFields("card", "minicard")
	if !slices.Contains(mini, "showLabelText") || !slices.Contains(mini, "scrum.visibility.minicardBacklogRank") ||
		slices.Contains(mini, "showsMinicardLabelText") || slices.Contains(mini, "showsListOnMinicard") {
		t.Fatalf("the minicard column: its board rows, the Scrum ones by path, no personal or per-card row: %v", mini)
	}
	for _, bad := range [][2]string{{"card", "members"}, {"swimlane", "card"}, {"nope", "draggable"}} {
		if len(columnFields(bad[0], bad[1])) != 0 {
			t.Fatalf("%v is no column", bad)
		}
	}
}

// setBoardSettingsColumn: a board admin's, each column at once, nothing else
// touched; an unknown column, another board, a member who is no admin, a
// value that is no boolean refused.
func TestSetBoardSettingsColumn(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Columns")
	foreign := newRESTBoard(t, w, login(t, w, "bob"), "Bobs")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": rb.ID}, bson.M{"$push": bson.M{"members": bson.M{"userId": bob.ID, "isAdmin": false, "isActive": true}}})
	ac := w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	call := func(cl *http.Client, args string) map[string]any {
		_, out := methodCall(t, w, cl, "setBoardSettingsColumn", args)
		return out
	}
	if out := call(ac, `["`+foreign.ID+`","card","card",false]`); out["error"] != "not-authorized" {
		t.Fatalf("another board: %v", out)
	}
	if out := call(ac, `["`+rb.ID+`","card","members",false]`); out["error"] != "invalid-setting" {
		t.Fatalf("an unknown column: %v", out)
	}
	if out := call(ac, `["`+rb.ID+`","card","draggable","false"]`); out["error"] == nil {
		t.Fatal("a boolean only")
	}
	if out := call(ac, `["`+rb.ID+`","list","settings",true]`); out["error"] != nil {
		t.Fatalf("the admin's: %v", out)
	}
	if out := call(ac, `["`+rb.ID+`","card","minicard",true]`); out["error"] != nil {
		t.Fatalf("a Card Settings side, Scrum rows included: %v", out)
	}
	var b bson.M
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": rb.ID}).Decode(&b)
	if b["listWidthResizeLocked"] != true || b["sameWidthForAllLists"] != true || b["swimlaneHeightResizeLocked"] != nil {
		t.Fatalf("only List's Settings column: %v", b)
	}
	sc, _ := plain(b["scrum"]).(map[string]any)
	vis, _ := sc["visibility"].(map[string]any)
	if b["allowsCardNumberOnMinicard"] != true || vis["minicardBacklogRank"] != true || vis["cardBacklogRank"] == true {
		t.Fatalf("the minicard column, its Scrum flags through scrum.configure: %v %v", b["allowsCardNumberOnMinicard"], vis)
	}
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": foreign.ID}).Decode(&b)
	if b["allowsCardDragging"] == false {
		t.Fatal("the other board untouched")
	}
	bc := w.client(t)
	body(t, w.signIn(t, bc, "bob", "pw"))
	if out := call(bc, `["`+rb.ID+`","card","draggable",false]`); out["error"] != "not-authorized" {
		t.Fatalf("a member who is no admin: %v", out)
	}
	_, html := get(t, bc, w.srv.URL+"/b/"+rb.ID+"/columns")
	if strings.Contains(html, "board-settings-column-actions") || !strings.Contains(html, `data-panel="section-card"`) {
		t.Fatal("he reads Card's hamburger, with no column actions")
	}
	_, html = get(t, ac, w.srv.URL+"/b/"+rb.ID+"/columns")
	for _, want := range []string{`data-popup="boardSwimlaneSettingsPopup"`, `data-popup="boardListSettingsPopup"`,
		`board-settings-column-actions" data-section="list" data-column="settings"`, `board-settings-column-actions" data-section="card" data-column="draggable"`} {
		if !strings.Contains(html, want) {
			t.Fatalf("the admin's page has %s", want)
		}
	}
}
