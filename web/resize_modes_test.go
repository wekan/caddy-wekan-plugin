package web

import (
	"context"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The viewer's own list widths, as WeKan's (#5729, #6409): the Set Width
// pop-over's same width for all lists and personal widths are the viewer's,
// drawn for them only, in WeKan's order - the board's same width first; a
// width under 200, another board's list and an unsafe key are refused.
func TestViewerListWidthModes(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	ro := w.seedUser(t, "rory", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Modes")
	other := newRESTBoard(t, w, login(t, w, "ann"), "Elsewhere")
	setMember(t, w, rb.ID, Member{UserID: ro.ID, IsActive: true, IsReadOnly: true})
	ac, rc := w.client(t), w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	body(t, w.signIn(t, rc, "rory", "pw"))
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
	type sizes struct {
		Fixed      map[string]bool               `bson:"fixedListWidthBoards"`
		FixedWidth map[string]float64            `bson:"fixedListWidths"`
		Widths     map[string]map[string]float64 `bson:"listWidths"`
	}
	profile := func(id string) sizes {
		var u struct {
			Profile sizes `bson:"profile"`
		}
		_ = w.db.Collection("users").FindOne(ctx, bson.M{"_id": id}).Decode(&u)
		return u.Profile
	}
	_, _ = w.db.Collection("lists").UpdateOne(ctx, bson.M{"_id": rb.List}, bson.M{"$set": bson.M{"width": 300.0}})

	// A read-only member's own same width: theirs to set, drawn for them,
	// and their handle with it; nobody else's board changes.
	if _, out := methodCall(t, w, rc, "setFixedListWidthEnabled", `["`+rb.ID+`",true]`); out["result"] != true {
		t.Fatalf("setFixedListWidthEnabled: %v", out)
	}
	if _, out := methodCall(t, w, rc, "setFixedListWidth", `["`+rb.ID+`",360]`); out["result"] != true {
		t.Fatalf("setFixedListWidth: %v", out)
	}
	if p := profile(ro.ID); !p.Fixed[rb.ID] || p.FixedWidth[rb.ID] != 360 {
		t.Fatalf("stored in the profile per board: %v", p)
	}
	if p := get("rory"); !strings.Contains(p, "--list-width:360px") || !strings.Contains(p, `data-fixed-width="true"`) || !strings.Contains(p, "js-list-resize-handle") {
		t.Fatal("the viewer's own same width, with the handle")
	}
	if p := get("ann"); !strings.Contains(p, "--list-width:300px") || strings.Contains(p, `data-fixed-width="true"`) {
		t.Fatal("another member sees the list's own width")
	}
	// Negative: too narrow is refused and the stored width is kept.
	if _, out := methodCall(t, w, rc, "setFixedListWidth", `["`+rb.ID+`",150]`); out["error"] != "invalid-width" {
		t.Fatalf("under 200: %v", out)
	}
	if profile(ro.ID).FixedWidth[rb.ID] != 360 {
		t.Fatal("a refused width does not overwrite")
	}
	// The board's same width comes first.
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": rb.ID}, bson.M{"$set": bson.M{"sameWidthForAllLists": true, "sameWidthForAllListsValue": 250.0}})
	if p := get("rory"); !strings.Contains(p, "--list-width:250px") || strings.Contains(p, "js-list-resize-handle") {
		t.Fatal("the board's same width over the viewer's, and a read-only member does not drag it")
	}
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": rb.ID}, bson.M{"$set": bson.M{"sameWidthForAllLists": false}})

	// Personal widths: the viewer's own width of one list, while the board allows them.
	if _, out := methodCall(t, w, rc, "setFixedListWidthEnabled", `["`+rb.ID+`",false]`); out["result"] != true {
		t.Fatalf("off: %v", out)
	}
	if _, out := methodCall(t, w, rc, "applyListWidthToStorage", `["`+rb.ID+`","`+rb.List+`",410,410]`); out["error"] != nil {
		t.Fatalf("applyListWidthToStorage: %v", out)
	}
	if lw := profile(ro.ID).Widths[rb.ID]; lw[rb.List] != 410 || len(lw) != 1 {
		t.Fatalf("one list's width only: %v", lw)
	}
	if !strings.Contains(get("rory"), "--list-width:300px") {
		t.Fatal("personal widths are drawn only while the board allows them")
	}
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": rb.ID}, bson.M{"$set": bson.M{"allowsPersonalListWidth": true}})
	if p := get("rory"); !strings.Contains(p, "--list-width:410px") || !strings.Contains(p, `data-personal-width="true"`) {
		t.Fatal("the viewer's personal width")
	}
	if !strings.Contains(get("ann"), "--list-width:300px") {
		t.Fatal("another member keeps the shared width")
	}
	var l struct {
		Width float64 `bson:"width"`
	}
	_ = w.db.Collection("lists").FindOne(ctx, bson.M{"_id": rb.List}).Decode(&l)
	if l.Width != 300 {
		t.Fatal("a personal width never changes the list")
	}
	// Negative: another board's list, too narrow, a board not seen, not a
	// boolean - and last an unsafe key, PrototypeBleed's attempt, which also
	// disables the account as WeKan's does.
	for _, c := range []struct{ name, method, args, want string }{
		{"other board's list", "applyListWidthToStorage", `["` + rb.ID + `","` + other.List + `",400,400]`, "error-notAuthorized"},
		{"too narrow", "applyListWidthToStorage", `["` + rb.ID + `","` + rb.List + `",120,120]`, "invalid-width"},
		{"board not seen", "setFixedListWidth", `["` + other.ID + `",300]`, "not-found"},
		{"not a boolean", "setFixedListWidthEnabled", `["` + rb.ID + `","yes"]`, "Match.Error"},
		{"map key", "setFixedListWidthEnabled", `["$where",true]`, "invalid-map-key"},
	} {
		if _, out := methodCall(t, w, rc, c.method, c.args); out["error"] != c.want {
			t.Fatalf("%s: %v", c.name, out)
		}
	}
	if p := profile(ro.ID); p.Fixed["$where"] || len(p.Widths[rb.ID]) != 1 {
		t.Fatal("nothing refused is written")
	}
	if n, _ := w.db.Collection("eventlog").CountDocuments(ctx, bson.M{"bleed": "PrototypeBleed", "source": "setFixedListWidthEnabled"}); n != 1 {
		t.Fatal("the unsafe key is in Problems as PrototypeBleed")
	}
	if code, out := methodCall(t, w, w.client(t), "setFixedListWidthEnabled", `["`+rb.ID+`",true]`); code != 401 || out["error"] != "not-logged-in" {
		t.Fatalf("signed out: %d %v", code, out)
	}
}
