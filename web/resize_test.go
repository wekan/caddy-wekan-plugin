package web

import (
	"context"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Swimlane and list resizing, as WeKan's: a writer's applyListWidth is the
// list's width for everybody and is drawn; a read-only member, another
// board's list and a width under the minimum are refused; the board's one
// width while same width is on is any writer's; a swimlane's height is the
// user's own; the locks take the handles away.
func TestListAndSwimlaneResize(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	ro := w.seedUser(t, "rory", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Resize")
	other := newRESTBoard(t, w, login(t, w, "ann"), "Other")
	setMember(t, w, rb.ID, Member{UserID: ro.ID, IsActive: true, IsReadOnly: true})
	ac, rc := w.client(t), w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	body(t, w.signIn(t, rc, "rory", "pw"))
	page := func() string {
		res, err := ac.Get(w.srv.URL + "/b/" + rb.ID + "/x")
		if err != nil {
			t.Fatal(err)
		}
		return body(t, res)
	}
	width := func(id string) float64 {
		var l struct {
			Width float64 `bson:"width"`
		}
		_ = w.db.Collection("lists").FindOne(ctx, bson.M{"_id": id}).Decode(&l)
		return l.Width
	}
	if p := page(); !strings.Contains(p, "js-list-resize-handle") || !strings.Contains(p, "js-swimlane-resize-handle") {
		t.Fatal("a writer has both handles")
	}
	if _, out := methodCall(t, w, ac, "applyListWidth", `["`+rb.ID+`","`+rb.List+`",310,310]`); out["result"] != true {
		t.Fatalf("applyListWidth: %v", out)
	}
	if width(rb.List) != 310 || !strings.Contains(page(), "--list-width:310px") {
		t.Fatal("the list is drawn at its width")
	}
	// Negative: a read-only member, another board's list, too narrow, not a number.
	for name, c := range map[string]struct {
		cl   int
		args string
		want string
	}{
		"read-only":   {1, `["` + rb.ID + `","` + rb.List + `",400,400]`, "error-notAuthorized"},
		"other board": {0, `["` + rb.ID + `","` + other.List + `",400,400]`, "error-notAuthorized"},
		"too narrow":  {0, `["` + rb.ID + `","` + rb.List + `",120,120]`, "invalid-width"},
		"too wide":    {0, `["` + rb.ID + `","` + rb.List + `",5000,5000]`, "invalid-width"},
		"not number":  {0, `["` + rb.ID + `","` + rb.List + `","400",400]`, "Match.Error"},
	} {
		cl := ac
		if c.cl == 1 {
			cl = rc
		}
		if _, out := methodCall(t, w, cl, "applyListWidth", c.args); out["error"] != c.want {
			t.Fatalf("%s: %v", name, out)
		}
	}
	if width(rb.List) != 310 || width(other.List) != 0 {
		t.Fatal("no refused width is written")
	}
	if p := func() string { res, _ := rc.Get(w.srv.URL + "/b/" + rb.ID + "/x"); return body(t, res) }(); strings.Contains(p, "js-list-resize-handle") {
		t.Fatal("a read-only member has no list handle")
	}
	if code, out := methodCall(t, w, w.client(t), "applyListWidth", `["`+rb.ID+`","`+rb.List+`",400,400]`); code != 401 || out["error"] != "not-logged-in" {
		t.Fatalf("signed out: %d %v", code, out)
	}

	// Same width for all lists: every list at the board's one width, which a
	// writer's drag sets and a read-only member's does not.
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": rb.ID}, bson.M{"$set": bson.M{"sameWidthForAllLists": true}})
	if p := page(); !strings.Contains(p, "--list-width:220px") || !strings.Contains(p, `data-same-width="true"`) {
		t.Fatal("same width: the default 220")
	}
	if _, out := methodCall(t, w, ac, "/boards/update", `[{"_id":"`+rb.ID+`"},{"$set":{"sameWidthForAllListsValue":260}}]`); out["result"] != float64(1) {
		t.Fatalf("a writer sets the one width: %v", out)
	}
	if !strings.Contains(page(), "--list-width:260px") {
		t.Fatal("drawn at the one width")
	}
	for _, args := range []string{`[{"_id":"` + rb.ID + `"},{"$set":{"sameWidthForAllListsValue":150}}]`,
		`[{"_id":"` + rb.ID + `"},{"$set":{"sameWidthForAllListsValue":300,"title":"x"}}]`} {
		if _, out := methodCall(t, w, ac, "/boards/update", args); out["reason"] != "Access denied" {
			t.Fatalf("only a valid width, alone: %s %v", args, out)
		}
	}
	if _, out := methodCall(t, w, rc, "/boards/update", `[{"_id":"`+rb.ID+`"},{"$set":{"sameWidthForAllListsValue":300}}]`); out["reason"] != "Access denied" {
		t.Fatalf("a read-only member does not: %v", out)
	}
	var bd bson.M
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": rb.ID}).Decode(&bd)
	if bd["sameWidthForAllListsValue"] != float64(260) || bd["title"] != "Resize" {
		t.Fatalf("nothing refused is written: %v", bd["sameWidthForAllListsValue"])
	}

	// A swimlane's height is the user's own.
	if _, out := methodCall(t, w, ac, "applySwimlaneHeightToStorage", `["`+rb.ID+`","`+rb.Swimlane+`",300]`); out["error"] != nil {
		t.Fatalf("applySwimlaneHeightToStorage: %v", out)
	}
	if !strings.Contains(page(), "--swimlane-height:305px") {
		t.Fatal("the swimlane at the user's height, 5px over, as WeKan's")
	}
	if p := func() string { res, _ := rc.Get(w.srv.URL + "/b/" + rb.ID + "/x"); return body(t, res) }(); strings.Contains(p, "--swimlane-height:305px") {
		t.Fatal("another user's swimlane keeps its own height")
	}
	// The locks take the handles away.
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": rb.ID}, bson.M{"$set": bson.M{"listWidthResizeLocked": true, "swimlaneHeightResizeLocked": true}})
	if p := page(); !strings.Contains(p, `id="js-list-`+rb.List+`"`) || strings.Contains(p, "js-list-resize-handle") || strings.Contains(p, "js-swimlane-resize-handle") {
		t.Fatal("locked: the board, with no handles")
	}
	// Negative: too low, a board not seen, and last an unsafe key -
	// PrototypeBleed's attempt, which disables the account.
	for _, c := range []struct{ name, args string }{
		{"too low", `["` + rb.ID + `","` + rb.Swimlane + `",20]`},
		{"not visible", `["nosuchboard01","` + rb.Swimlane + `",300]`},
		{"map key", `["` + rb.ID + `","$where",300]`},
	} {
		if _, out := methodCall(t, w, ac, "applySwimlaneHeightToStorage", c.args); out["error"] == nil {
			t.Fatalf("%s is refused: %v", c.name, out)
		}
	}
}
