package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// WeKan's board announcement (#1566): a board admin's, shown as plain text
// until a member dismisses it, and again after an edit.
func TestBoardAnnouncement(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Notice")
	setMember(t, w, rb.ID, Member{UserID: bob.ID, IsActive: true})
	ac, bc := w.client(t), w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	body(t, w.signIn(t, bc, "bob", "pw"))
	page := func(cl interface{}) string {
		c := ac
		if cl == "bob" {
			c = bc
		}
		res, err := c.Get(w.srv.URL + "/b/" + rb.ID + "/x?view=swimlanes")
		if err != nil {
			t.Fatal(err)
		}
		return body(t, res)
	}
	post := func(c interface{}, form string) int {
		cl := ac
		if c == "bob" {
			cl = bc
		}
		res, err := cl.Post(w.srv.URL+"/b/"+rb.ID+"/announcement", "application/x-www-form-urlencoded", strings.NewReader(form))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		return res.StatusCode
	}
	if strings.Contains(page("bob"), "js-board-announcement\"") {
		t.Fatal("no announcement before one is written")
	}
	// Negative: a member who is not a board admin may not write one.
	if code := post("bob", "enabled=true&body=x"); code != 403 {
		t.Fatalf("a normal member wrote the announcement: %d", code)
	}
	if code := post("ann", "enabled=true&body="+strings.Repeat("a", 2001)); code != 400 {
		t.Fatalf("over 2000 characters: %d", code)
	}
	if code := post("ann", "enabled=true&body=%3Cb%3EFreeze%3C%2Fb%3E+on+Friday"); code >= 400 {
		t.Fatalf("the admin's save: %d", code)
	}
	h := page("bob")
	if !strings.Contains(h, "&lt;b&gt;Freeze&lt;/b&gt; on Friday") || strings.Contains(h, "<b>Freeze</b>") {
		i := strings.Index(h, "board-announcement")
		t.Fatalf("shown to members, as plain text: %d %.400s", i, h[max(0, i-50):max(0, i)+350])
	}
	if !strings.Contains(page("ann"), "js-open-board-announcement") || strings.Contains(h, "js-open-board-announcement") {
		t.Fatal("the editor is the board admin's alone")
	}
	// Dismissed at its version, it is gone for that member only, until edited.
	if _, out := methodCall(t, w, bc, "dismissBoardAnnouncement", `["`+rb.ID+`"]`); out["result"] == nil {
		t.Fatalf("dismiss: %v", out)
	}
	if strings.Contains(page("bob"), "Freeze") || !strings.Contains(page("ann"), "Freeze</b> on Friday") && !strings.Contains(page("ann"), "&lt;b&gt;Freeze") {
		t.Fatal("dismissed for bob only")
	}
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": rb.ID}, bson.M{"$set": bson.M{"announcement.body": "Moved to Monday", "announcement.updatedAt": time.Now().Add(time.Second)}})
	if !strings.Contains(page("bob"), "Moved to Monday") {
		t.Fatal("an edit shows again")
	}
	// Disabled, or saved empty, it shows to nobody.
	post("ann", "enabled=&body=Hidden")
	if strings.Contains(page("bob"), "Hidden") {
		t.Fatal("disabled")
	}
	if a, ok := cleanBoardAnnouncement(true, "   ", time.Now()); !ok || a["enabled"] != false {
		t.Fatal("empty text is never enabled")
	}
	// Negative: dismissing another user's board, or an unsafe key.
	other := newRESTBoard(t, w, login(t, w, "ann"), "Private")
	if _, out := methodCall(t, w, bc, "dismissBoardAnnouncement", `["`+other.ID+`"]`); out["error"] != "not-found" {
		t.Fatalf("a board bob cannot see: %v", out)
	}
}
