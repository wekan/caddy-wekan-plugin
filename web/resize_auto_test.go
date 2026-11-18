package web

import (
	"context"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// WeKan's automatic list width (#6409): the board's autoWidth, set by who may
// write the board, or in personal mode the viewer's own. While it is on, the
// lists share the row with no width and no resize handle of their own.
func TestAutoListWidth(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	ro := w.seedUser(t, "rory", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Auto")
	other := newRESTBoard(t, w, login(t, w, "ann"), "Elsewhere")
	setMember(t, w, rb.ID, Member{UserID: ro.ID, IsActive: true, IsReadOnly: true})
	ac, rc := w.client(t), w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	body(t, w.signIn(t, rc, "rory", "pw"))
	_, _ = w.db.Collection("lists").UpdateOne(ctx, bson.M{"_id": rb.List}, bson.M{"$set": bson.M{"width": 300.0}})
	get := func(who string) string {
		cl := ac
		if who == "rory" {
			cl = rc
		}
		res, err := cl.Get(w.srv.URL + "/b/" + rb.ID + "/x")
		if err != nil {
			t.Fatal(err)
		}
		return body(t, res)
	}
	auto := func(html string) bool { return strings.Contains(html, "list-auto-width") }
	drawnAt300 := func(html string) bool { return strings.Contains(html, "--list-width:300px") }

	// Off: the list's own width, and its handle for the board's writer.
	if h := get("ann"); auto(h) || !drawnAt300(h) || !strings.Contains(h, "js-list-resize-handle") {
		t.Fatal("before: the list's own width and handle")
	}

	// Negative: a read-only member may not switch the board's automatic width.
	if _, out := methodCall(t, w, rc, "setBoardAutoWidth", `["`+rb.ID+`",true]`); out["error"] != "error-notAuthorized" {
		t.Fatalf("read-only setBoardAutoWidth: %v", out)
	}
	if code, out := methodCall(t, w, w.client(t), "setBoardAutoWidth", `["`+rb.ID+`",true]`); code != 401 || out["error"] != "not-logged-in" {
		t.Fatalf("signed out: %d %v", code, out)
	}
	if _, out := methodCall(t, w, ac, "setBoardAutoWidth", `["`+rb.ID+`","yes"]`); out["error"] != "Match.Error" {
		t.Fatalf("not a boolean: %v", out)
	}

	// The board's writer turns it on: everyone's lists share the row, and
	// nobody has a handle.
	if _, out := methodCall(t, w, ac, "setBoardAutoWidth", `["`+rb.ID+`",true]`); out["result"] != true {
		t.Fatalf("setBoardAutoWidth: %v", out)
	}
	for _, who := range []string{"ann", "rory"} {
		h := get(who)
		if !auto(h) || drawnAt300(h) || strings.Contains(h, "js-list-resize-handle") {
			t.Fatalf("%s: automatic width, no width of its own, no handle", who)
		}
	}
	var b struct {
		AutoWidth bool `bson:"autoWidth"`
	}
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": rb.ID}).Decode(&b)
	if !b.AutoWidth {
		t.Fatal("stored on the board")
	}

	// Personal mode: the board's switch no longer counts; each viewer has
	// their own, and anyone signed in may set theirs.
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": rb.ID}, bson.M{"$set": bson.M{"allowsPersonalListWidth": true}})
	if auto(get("rory")) {
		t.Fatal("personal mode: the board's automatic width is not the viewer's")
	}
	if _, out := methodCall(t, w, rc, "setPersonalAutoWidth", `["`+rb.ID+`",true]`); out["result"] != true {
		t.Fatalf("setPersonalAutoWidth: %v", out)
	}
	if !auto(get("rory")) || auto(get("ann")) {
		t.Fatal("the viewer's own automatic width is theirs only")
	}
	// Negative: not on a board they cannot see, and never an unsafe key.
	if _, out := methodCall(t, w, rc, "setPersonalAutoWidth", `["`+other.ID+`",true]`); out["error"] != "not-found" {
		t.Fatalf("another board: %v", out)
	}
	var u struct {
		Profile struct {
			Auto map[string]bool `bson:"autoWidthBoards"`
		} `bson:"profile"`
	}
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"_id": ro.ID}).Decode(&u)
	if len(u.Profile.Auto) != 1 || !u.Profile.Auto[rb.ID] {
		t.Fatalf("only this board in the profile: %v", u.Profile.Auto)
	}
	if _, out := methodCall(t, w, rc, "setPersonalAutoWidth", `["`+rb.ID+`",false]`); out["result"] != true || auto(get("rory")) {
		t.Fatal("turned off again")
	}
	// Last, as in TestListAndSwimlaneResize: an unsafe key is PrototypeBleed,
	// which disables the account.
	if _, out := methodCall(t, w, rc, "setPersonalAutoWidth", `["a.b",true]`); out["error"] == nil || out["result"] == true {
		t.Fatalf("an unsafe key: %v", out)
	}
}

func TestAutoListWidthPrecedence(t *testing.T) {
	// Automatic width outranks every width, and takes the handle away even
	// from who may change the board.
	z := boardResize{Auto: true, SameWidth: true, SameWidthValue: 400, Fixed: true, FixedWidth: 300}
	if z.listWidth("l", 250) != 0 || z.canResize(true) {
		t.Fatal("automatic width: no width, no handle")
	}
	if !autoWidthOn(true, false, true) || autoWidthOn(true, true, false) || !autoWidthOn(false, true, false) {
		t.Fatal("personal mode reads the viewer's switch, else the board's")
	}
}
