package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The sidebar's members: shown, added by whoever may invite, removed by a
// board admin only - never the last admin - and off the board's cards.
func TestBoardMembersInSidebar(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	w.seedUser(t, "bert", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Members")
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	boardURL := w.srv.URL + "/b/" + b.ID + "/" + b.Slug
	var found []map[string]string
	res, _ := get(t, ann, w.srv.URL+"/b/"+b.ID+"/members/search?q=b")
	_ = json.NewDecoder(strings.NewReader(body(t, mustGet(t, ann, w.srv.URL+"/b/"+b.ID+"/members/search?q=b")))).Decode(&found)
	if res.StatusCode != 200 || len(found) != 2 {
		t.Fatalf("the search finds bob and bert: %v", found)
	}
	if r, _ := ann.PostForm(w.srv.URL+"/b/"+b.ID+"/members", url.Values{"userId": {bob.ID}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("added: %d", r.StatusCode)
	}
	_, html := get(t, ann, boardURL)
	if !strings.Contains(html, `title="BOB (bob)"`) || !strings.Contains(html, "js-remove-member") {
		t.Fatal("the member in the sidebar, and a remove for the admin")
	}
	_ = json.NewDecoder(strings.NewReader(body(t, mustGet(t, ann, w.srv.URL+"/b/"+b.ID+"/members/search?q=bo")))).Decode(&found)
	if len(found) != 0 {
		t.Fatalf("negative: a member is not offered again: %v", found)
	}
	l, _ := w.s.AddList(ctx, b, "L")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	c, _ := w.s.AddCard(ctx, b, u, l.ID, sw.ID, "C")
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": c.ID}, bson.M{"$set": bson.M{"members": bson.A{bob.ID}, "assignees": bson.A{bob.ID}}})

	bc := w.client(t)
	body(t, w.signIn(t, bc, "bob", "pw"))
	if _, html := get(t, bc, boardURL); strings.Contains(html, "js-remove-member") {
		t.Fatal("negative: a member who is not an admin is not offered a remove")
	}
	if r, _ := bc.PostForm(w.srv.URL+"/b/"+b.ID+"/members/"+u.ID+"/remove", nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("negative: nor can they remove anybody: %d", r.StatusCode)
	}
	if r, _ := ann.PostForm(w.srv.URL+"/b/"+b.ID+"/members/"+bob.ID+"/remove", nil); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("removed: %d", r.StatusCode)
	}
	var board Board
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": b.ID}).Decode(&board)
	kept := false
	for _, m := range board.Members {
		kept = kept || (m.UserID == bob.ID && !m.IsActive)
	}
	if board.MemberOf(bob.ID) || !kept {
		t.Fatal("#5122: kept, inactive")
	}
	if _, html := get(t, ann, boardURL); strings.Contains(html, `title="BOB (bob)"`) {
		t.Fatal("#6479: gone from the sidebar")
	}
	var card Card
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": c.ID}).Decode(&card)
	if len(card.Members) != 0 || len(card.Assignees) != 0 {
		t.Fatal("and from the board's cards")
	}
	if r, _ := ann.PostForm(w.srv.URL+"/b/"+b.ID+"/members/"+u.ID+"/remove", nil); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("negative: not oneself, the last admin: %d", r.StatusCode)
	}
	if r, _ := bc.PostForm(w.srv.URL+"/b/"+b.ID+"/members", url.Values{"userId": {bob.ID}}); r.StatusCode != http.StatusNotFound {
		t.Fatalf("negative: a removed member cannot add themself back: %d", r.StatusCode)
	}
}

func mustGet(t *testing.T, c *http.Client, u string) *http.Response {
	t.Helper()
	res, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// FerretDB pulls a plain value, not {$eq: value}: a $pull of eq() straight on
// a list of values matches nothing and silently leaves the value in. This
// guard fails for any such $pull, so a removal cannot quietly do nothing.
func TestNoPullOfEqOnValues(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	bad := regexp.MustCompile(`"\$pull": bson\.M\{[^{}]*"[A-Za-z.]+": eq\(`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, _ := os.ReadFile(f)
		for i, line := range strings.Split(string(src), "\n") {
			if bad.MatchString(line) {
				t.Errorf("%s:%d pulls eq(): pull the value itself, checked as an id first", f, i+1)
			}
		}
	}
}

// cardDetailsActionsPopup's Archive: by whoever may change the card only.
func TestArchiveCardFromDetails(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	ro := w.seedUser(t, "rob", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Archive")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$push": bson.M{"members": bson.M{"userId": ro.ID, "isActive": true, "isReadOnly": true}}})
	l, _ := w.s.AddList(ctx, b, "L")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	c, _ := w.s.AddCard(ctx, b, u, l.ID, sw.ID, "C")
	archived := func() bool {
		var x Card
		_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": c.ID}).Decode(&x)
		return x.Archived
	}
	rob := w.client(t)
	body(t, w.signIn(t, rob, "rob", "pw"))
	if _, html := get(t, rob, w.srv.URL+"/b/"+b.ID+"/"+b.Slug+"/"+c.ID); strings.Contains(html, "js-open-card-details-menu") {
		t.Fatal("negative: no actions menu for a read-only member")
	}
	if r, _ := rob.PostForm(w.srv.URL+"/b/"+b.ID+"/cards/"+c.ID+"/archive", nil); r.StatusCode != http.StatusForbidden || archived() {
		t.Fatalf("negative: a read-only member archives nothing: %d", r.StatusCode)
	}
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	if r, _ := ann.PostForm(w.srv.URL+"/b/"+b.ID+"/cards/"+c.ID+"/archive", nil); r.StatusCode != http.StatusSeeOther || !archived() {
		t.Fatalf("archived: %d", r.StatusCode)
	}
}

// WeKan's boardChangeTitlePopup and the board's Archive: a board admin
// renames; whoever may change the board restores what is archived.
func TestRenameBoardAndRestore(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	ro := w.seedUser(t, "rob", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Old")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$push": bson.M{"members": bson.M{"$each": bson.A{
		bson.M{"userId": bob.ID, "isActive": true}, bson.M{"userId": ro.ID, "isActive": true, "isReadOnly": true}}}}})
	l, _ := w.s.AddList(ctx, b, "L")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	c, _ := w.s.AddCard(ctx, b, u, l.ID, sw.ID, "Gone")
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": c.ID}, bson.M{"$set": bson.M{"archived": true}})
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	_, html := get(t, ann, w.srv.URL+"/b/"+b.ID+"/"+b.Slug)
	if !strings.Contains(html, "js-edit-board-title") || !strings.Contains(html, "js-open-archives") || !strings.Contains(html, `data-id="`+c.ID+`"`) {
		t.Fatal("the rename for the admin, and the archived card in the Archive")
	}
	bc := w.client(t)
	body(t, w.signIn(t, bc, "bob", "pw"))
	if _, html := get(t, bc, w.srv.URL+"/b/"+b.ID+"/"+b.Slug); strings.Contains(html, "js-edit-board-title") {
		t.Fatal("negative: no rename for a member who is not an admin")
	}
	if r, _ := bc.PostForm(w.srv.URL+"/b/"+b.ID+"/title", url.Values{"title": {"Mine"}}); r.StatusCode != http.StatusNotFound {
		t.Fatalf("negative: nor may they rename: %d", r.StatusCode)
	}
	if r, _ := ann.PostForm(w.srv.URL+"/b/"+b.ID+"/title", url.Values{"title": {"  "}}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("negative: a title is needed: %d", r.StatusCode)
	}
	if r, _ := ann.PostForm(w.srv.URL+"/b/"+b.ID+"/title", url.Values{"title": {"New Name"}, "description": {"About"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("renamed: %d", r.StatusCode)
	}
	var nb Board
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": b.ID}).Decode(&nb)
	if nb.Title != "New Name" || nb.Description != "About" || nb.Slug != "new-name" {
		t.Fatalf("the title, description and slug: %+v", nb)
	}
	rob := w.client(t)
	body(t, w.signIn(t, rob, "rob", "pw"))
	if r, _ := rob.PostForm(w.srv.URL+"/b/"+b.ID+"/archive/restore", url.Values{"kind": {"cards"}, "id": {c.ID}}); r.StatusCode != http.StatusForbidden {
		t.Fatalf("negative: a read-only member restores nothing: %d", r.StatusCode)
	}
	if r, _ := bc.PostForm(w.srv.URL+"/b/"+b.ID+"/archive/restore", url.Values{"kind": {"users"}, "id": {u.ID}}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("negative: only cards, lists and swimlanes: %d", r.StatusCode)
	}
	if r, _ := bc.PostForm(w.srv.URL+"/b/"+b.ID+"/archive/restore", url.Values{"kind": {"cards"}, "id": {c.ID}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("restored: %d", r.StatusCode)
	}
	var got Card
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": c.ID}).Decode(&got)
	if got.Archived {
		t.Fatal("the card is back")
	}
}
