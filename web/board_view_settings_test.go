package web

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The views are WeKan's: made again from WeKan's file when it and a node are
// beside the plugin, the table is byte for byte board_views.json.
func TestBoardViewsTableIsWeKans(t *testing.T) {
	wekan, _ := filepath.Abs("../../../..")
	if _, err := os.Stat(filepath.Join(wekan, "models/lib/boardViewSettings.js")); err != nil {
		t.Skip("WeKan is not beside the plugin")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		matches, _ := filepath.Glob(filepath.Join(wekan, ".tools/node-v*/bin/node"))
		if len(matches) == 0 {
			t.Skip("no node")
		}
		node = matches[0]
	}
	out, err := exec.Command(node, "../cmd/board-views/generate.cjs", wekan).Output()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, boardViewsJSON) {
		t.Fatal("board_views.json is not what cmd/board-views/generate.cjs makes of WeKan's BOARD_VIEWS: run it again")
	}
	if len(boardViewsTable.Views) != len(boardViewKeys) {
		t.Fatalf("every view of boardViewKeys: %d and %d", len(boardViewsTable.Views), len(boardViewKeys))
	}
}

// Board Settings / Board View: a board admin's, every write checked as the
// REST API checks it; nothing else rides along.
func TestBoardViewSettingsPopover(t *testing.T) {
	if _, ok := boardViewWrite(map[string]any{"boardViewOrder": []any{"board-view-table"}, "permission": "public"}); ok {
		t.Fatal("another field is not Board View's")
	}
	if _, ok := boardViewWrite(map[string]any{"boardViewSettings.board-view-table.isAdmin": true}); ok {
		t.Fatal("only the two show flags")
	}
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Views")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": rb.ID}, bson.M{"$push": bson.M{"members": bson.M{"userId": bob.ID, "isAdmin": false, "isActive": true}}})
	ac := w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	write := func(cl *http.Client, set string) map[string]any {
		_, out := methodCall(t, w, cl, "/boards/update", `[{"_id":"`+rb.ID+`"},{"$set":`+set+`}]`)
		return out
	}
	_, html := get(t, ac, w.srv.URL+"/b/"+rb.ID+"/views")
	if !strings.Contains(html, `data-popup="boardViewSettingsPopup"`) || !strings.Contains(html, `js-board-view-row" data-view="board-view-swimlanes"`) ||
		!strings.Contains(html, "js-open-board-view-settings") {
		t.Fatal("the pop-over and its rows")
	}
	if out := write(ac, `{"boardViewOrder":["board-view-table","board-view-swimlanes"],"boardViewSettings.board-view-table.showOnPublic":false}`); out["error"] != nil {
		t.Fatalf("the admin orders and hides: %v", out)
	}
	var b bson.M
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": rb.ID}).Decode(&b)
	if order, _ := b["boardViewOrder"].(bson.A); len(order) == 0 || order[0] != "board-view-table" {
		t.Fatalf("the order: %v", b["boardViewOrder"])
	}
	if out := write(ac, `{"boardViewSettings.board-view-swimlanes.showOnPrivate":false}`); out["error"] == nil {
		t.Fatal("a side's default is never hidden")
	}
	if out := write(ac, `{"defaultPrivateBoardView":"board-view-nope"}`); out["error"] == nil {
		t.Fatal("an unknown view is refused")
	}
	bc := w.client(t)
	body(t, w.signIn(t, bc, "bob", "pw"))
	if out := write(bc, `{"boardViewOrder":["board-view-lists"]}`); out["reason"] != "Access denied" {
		t.Fatalf("a member who is no admin may not: %v", out)
	}
	if _, html := get(t, bc, w.srv.URL+"/b/"+rb.ID+"/views"); strings.Contains(html, `data-popup="boardViewSettingsPopup"`) {
		t.Fatal("nor does he get the pop-over")
	}
	// The rows in the board's order on the page.
	_, html = get(t, ac, w.srv.URL+"/b/"+rb.ID+"/views")
	if strings.Index(html, `data-view="board-view-table"`) > strings.Index(html, `js-board-view-row" data-view="board-view-swimlanes"`) {
		t.Fatal("the board's order")
	}
}
