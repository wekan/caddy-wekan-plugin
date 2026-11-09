package web

import (
	"context"
	"html"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The Rules page's Blocks view: for a board admin, the board's rules with
// their trigger and action documents - what WeKan keeps beside the fields
// too - the palette, the words and Blockly's messages, and Blockly itself;
// for anybody else, none of it.
func TestRulesBlocksView(t *testing.T) {
	w := newTestWeKan(t)
	ann := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, ann, "Blocks")
	setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: true})
	_, _ = w.db.Collection("triggers").InsertOne(ctx, bson.M{"_id": "tB", "boardId": b.ID, "activityType": "createCard", "listName": "*", "extraData": bson.M{"keep": "unchanged"}})
	_, _ = w.db.Collection("actions").InsertOne(ctx, bson.M{"_id": "aB", "boardId": b.ID, "actionType": "markCardComplete"})
	_, _ = w.db.Collection("rules").InsertOne(ctx, bson.M{"_id": "rB", "boardId": b.ID, "title": "Existing block rule", "triggerId": "tB", "actionId": "aB"})
	page := func(user string) string {
		cl := w.client(t)
		body(t, w.signIn(t, cl, user, "pw"))
		res, err := cl.Get(w.srv.URL + "/b/" + b.ID + "/" + b.Slug + "/rules?view=blocks")
		if err != nil {
			t.Fatal(err)
		}
		return html.UnescapeString(body(t, res))
	}
	p := page("ann")
	for _, want := range []string{`data-pane="blocks"`, "Existing block rule", `"activityType":"createCard"`, `"extraData":{"keep":"unchanged"}`,
		`"actionType":"markCardComplete"`, `"messages":{`, `"DELETE_BLOCK":`, `"triggers":[`, "/static/vendor/blockly.min.js", "/static/weddy-blocks.js"} {
		if !strings.Contains(p, want) {
			t.Errorf("the Blocks view has %q", want)
		}
	}
	if strings.Contains(p, `"boardId":"`+b.ID+`"`) {
		t.Error("negative: a rule's documents travel without their board")
	}
	for _, path := range []string{"/static/vendor/blockly.min.js", "/static/vendor/blockly-media/delete-icon.svg"} {
		res, err := testClient.Get(w.srv.URL + path)
		if err != nil || res.StatusCode != 200 {
			t.Errorf("%s is served: %v", path, err)
		}
		if res != nil {
			res.Body.Close()
		}
	}
	if m := page("bob"); strings.Contains(m, "Existing block rule") || strings.Contains(m, "blockly.min.js") {
		t.Error("negative: a normal member gets no rules and no editor")
	}
}
