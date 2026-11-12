package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
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
	if len(card.Members) != 0 || len(card.Assignees) != 1 {
		t.Fatal("and from the board's cards' members; assignees stay, as WeKan's")
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
	// FerretDB matches neither {$eq: v} nor {$in: [...]} on an array of
	// plain values: both pull nothing, silently.
	bad := regexp.MustCompile(`"\$pull": bson\.M\{[^{}]*"[A-Za-z.]+": (eq\(|bson\.M\{"\$in")`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, _ := os.ReadFile(f)
		for i, line := range strings.Split(string(src), "\n") {
			if bad.MatchString(line) {
				t.Errorf("%s:%d pulls eq() or $in: pull each value itself, checked as an id first", f, i+1)
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

// WeKan's Boards in Archive: the user's archived boards, restored by an admin.
func TestArchivedBoards(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	ctx := context.Background()
	live, _ := w.s.CreateBoard(ctx, u, "Live")
	old, _ := w.s.CreateBoard(ctx, u, "Old")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": old.ID}, bson.M{"$set": bson.M{"archived": true},
		"$push": bson.M{"members": bson.M{"userId": bob.ID, "isActive": true}}})
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	_, html := get(t, ann, w.srv.URL+"/allboards/archive")
	if !strings.Contains(html, "js-board "+old.ID) || strings.Contains(html, "js-board "+live.ID) || !strings.Contains(html, "js-restore-board") {
		t.Fatal("the archived board, not the live one, with its restore")
	}
	bc := w.client(t)
	body(t, w.signIn(t, bc, "bob", "pw"))
	if _, html := get(t, bc, w.srv.URL+"/allboards/archive"); strings.Contains(html, "js-restore-board") {
		t.Fatal("negative: no restore for a member who is not its admin")
	}
	if r, _ := bc.PostForm(w.srv.URL+"/b/"+old.ID+"/restore", nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("negative: nor may they restore it: %d", r.StatusCode)
	}
	if r, _ := ann.PostForm(w.srv.URL+"/b/"+old.ID+"/restore", nil); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("restored: %d", r.StatusCode)
	}
	var b Board
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": old.ID}).Decode(&b)
	if b.Archived {
		t.Fatal("the board is back")
	}
}

// A card's title edited in place, and the user's own maximized card.
func TestCardTitleAndMaximized(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	ro := w.seedUser(t, "rob", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Cards")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$push": bson.M{"members": bson.M{"userId": ro.ID, "isActive": true, "isReadOnly": true}}})
	l, _ := w.s.AddList(ctx, b, "L")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	c, _ := w.s.AddCard(ctx, b, u, l.ID, sw.ID, "Alpha")
	cardURL := w.srv.URL + "/b/" + b.ID + "/" + b.Slug + "/" + c.ID
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	if r, _ := ann.PostForm(w.srv.URL+"/b/"+b.ID+"/cards/"+c.ID+"/title", url.Values{"title": {"Alpha Renamed"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("renamed: %d", r.StatusCode)
	}
	rob := w.client(t)
	body(t, w.signIn(t, rob, "rob", "pw"))
	if r, _ := rob.PostForm(w.srv.URL+"/b/"+b.ID+"/cards/"+c.ID+"/title", url.Values{"title": {"Mine"}}); r.StatusCode != http.StatusForbidden {
		t.Fatalf("negative: a read-only member: %d", r.StatusCode)
	}
	var got Card
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": c.ID}).Decode(&got)
	if got.Title != "Alpha Renamed" {
		t.Fatalf("the title: %s", got.Title)
	}
	if _, html := get(t, ann, cardURL); strings.Contains(html, "card-details-maximized") || !strings.Contains(html, "js-maximize-card-details") {
		t.Fatal("not maximized at first")
	}
	req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/toggleCardMaximized", strings.NewReader(`[]`))
	req.Header.Set("Content-Type", "application/json")
	if res, err := ann.Do(req); err != nil || res.StatusCode != 200 {
		t.Fatal("toggleCardMaximized")
	}
	if _, html := get(t, ann, cardURL); !strings.Contains(html, "card-details-maximized") || !strings.Contains(html, "js-minimize-card-details") {
		t.Fatal("maximized after the toggle")
	}
	if _, html := get(t, rob, cardURL); strings.Contains(html, "card-details-maximized") {
		t.Fatal("negative: one user's choice, not another's")
	}
}

// WeKan's card labels: a board label on the card or off by whoever may change
// it, a new label by a board admin only, in one of WeKan's colors.
func TestCardLabelsPopup(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Labels")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$push": bson.M{"members": bson.M{"userId": bob.ID, "isActive": true, "isReadOnly": true}}})
	l, _ := w.s.AddList(ctx, b, "L")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	c, _ := w.s.AddCard(ctx, b, u, l.ID, sw.ID, "C")
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	if r, _ := ann.PostForm(w.srv.URL+"/b/"+b.ID+"/labels", url.Values{"name": {"Seeded"}, "color": {"sky"}, "card": {c.ID}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("a label: %d", r.StatusCode)
	}
	var nb Board
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": b.ID}).Decode(&nb)
	var label string
	for _, x := range nb.Labels {
		if x.Name == "Seeded" && x.Color == "sky" {
			label = x.ID
		}
	}
	if label == "" {
		t.Fatal("the label is on the board")
	}
	if r, _ := ann.PostForm(w.srv.URL+"/b/"+b.ID+"/labels", url.Values{"name": {"x"}, "color": {"chartreuse"}}); r.StatusCode != http.StatusBadRequest {
		t.Fatal("negative: only WeKan's colors")
	}
	toggle := w.srv.URL + "/b/" + b.ID + "/cards/" + c.ID + "/labels/" + label
	if r, _ := ann.PostForm(toggle, nil); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("on the card: %d", r.StatusCode)
	}
	if _, html := get(t, ann, w.srv.URL+"/b/"+b.ID+"/"+b.Slug+"/"+c.ID); !strings.Contains(html, `class="card-label card-label-sky">Seeded`) {
		t.Fatal("the card shows it")
	}
	bc := w.client(t)
	body(t, w.signIn(t, bc, "bob", "pw"))
	if r, _ := bc.PostForm(toggle, nil); r.StatusCode != http.StatusForbidden {
		t.Fatalf("negative: a read-only member: %d", r.StatusCode)
	}
	if r, _ := bc.PostForm(w.srv.URL+"/b/"+b.ID+"/labels", url.Values{"name": {"Mine"}}); r.StatusCode != http.StatusNotFound {
		t.Fatalf("negative: only an admin makes labels: %d", r.StatusCode)
	}
	if r, _ := ann.PostForm(w.srv.URL+"/b/"+b.ID+"/cards/"+c.ID+"/labels/notalabel", nil); r.StatusCode != http.StatusNotFound {
		t.Fatal("negative: a label of the board only")
	}
	if r, _ := ann.PostForm(toggle, nil); r.StatusCode != http.StatusSeeOther {
		t.Fatal("off again")
	}
	var got Card
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": c.ID}).Decode(&got)
	if slices.Contains(got.LabelIDs, label) {
		t.Fatal("off the card")
	}
}

// WeKan's moveCardPopup and copyCardPopup: a board the user may change, one
// of its lists and swimlanes; a copy only of a card the user may copy.
func TestMoveAndCopyCardPopups(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "From")
	other, _ := w.s.CreateBoard(ctx, u, "To")
	foreign, _ := w.s.CreateBoard(ctx, bob, "Foreign")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$push": bson.M{"members": bson.M{"userId": bob.ID, "isActive": true, "isNormalAssignedOnly": true}}})
	la, _ := w.s.AddList(ctx, b, "A")
	lb, _ := w.s.AddList(ctx, b, "B")
	lo, _ := w.s.AddList(ctx, other, "Other list")
	lf, _ := w.s.AddList(ctx, foreign, "F")
	sw := func(id string) string {
		var x Swimlane
		_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": id}).Decode(&x)
		return x.ID
	}
	c, _ := w.s.AddCard(ctx, b, u, la.ID, sw(b.ID), "Alpha")
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	if _, html := get(t, ann, w.srv.URL+"/b/"+b.ID+"/"+b.Slug+"/"+c.ID); !strings.Contains(html, "js-move-card") || !strings.Contains(html, ">1. A</option>") {
		t.Fatal("Move and Copy, with the board's lists numbered")
	}
	post := func(cl *http.Client, verb string, v url.Values) int {
		r, err := cl.PostForm(w.srv.URL+"/b/"+b.ID+"/cards/"+c.ID+"/"+verb, v)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	where := func(id string) Card {
		var x Card
		_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": id}).Decode(&x)
		return x
	}
	if code := post(ann, "move-to", url.Values{"boardId": {b.ID}, "listId": {lb.ID}, "swimlaneId": {sw(b.ID)}}); code != http.StatusSeeOther || where(c.ID).ListID != lb.ID {
		t.Fatalf("moved within the board: %d", code)
	}
	if code := post(ann, "move-to", url.Values{"boardId": {foreign.ID}, "listId": {lf.ID}}); code != http.StatusForbidden {
		t.Fatalf("negative: a board not theirs to change: %d", code)
	}
	if code := post(ann, "move-to", url.Values{"boardId": {other.ID}, "listId": {lb.ID}}); code != http.StatusBadRequest {
		t.Fatalf("negative: a list of the destination board only: %d", code)
	}
	if code := post(ann, "copy-to", url.Values{"boardId": {other.ID}, "listId": {lo.ID}, "title": {"Alpha copy"}}); code != http.StatusSeeOther {
		t.Fatalf("copied: %d", code)
	}
	if n, _ := w.db.Collection("cards").CountDocuments(ctx, bson.M{"boardId": other.ID, "title": "Alpha copy"}); n != 1 || where(c.ID).BoardID != b.ID {
		t.Fatal("the copy on the other board, the original where it was")
	}
	bc := w.client(t)
	body(t, w.signIn(t, bc, "bob", "pw"))
	if code := post(bc, "copy-to", url.Values{"boardId": {foreign.ID}, "listId": {lf.ID}}); code != http.StatusForbidden {
		t.Fatalf("negative: an assigned-only member copies only their own card: %d", code)
	}
	if code := post(bc, "move-to", url.Values{"boardId": {b.ID}, "listId": {la.ID}}); code != http.StatusForbidden {
		t.Fatalf("negative: nor moves another's: %d", code)
	}
	if code := post(ann, "move-to", url.Values{"boardId": {other.ID}, "listId": {lo.ID}}); code != http.StatusSeeOther || where(c.ID).BoardID != other.ID {
		t.Fatalf("moved to another board: %d", code)
	}
	if res, _ := get(t, ann, w.srv.URL+"/b/"+foreign.ID+"/places"); res.StatusCode != http.StatusNotFound {
		t.Fatal("negative: no places of a board not theirs")
	}
}

// WeKan's Add Card to Top / Bottom of List.
func TestAddCardTopOrBottom(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Add")
	l, _ := w.s.AddList(ctx, b, "L")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	_, _ = w.s.AddCard(ctx, b, u, l.ID, sw.ID, "Existing")
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	add := func(title, pos string) {
		r, err := ann.PostForm(w.srv.URL+"/b/"+b.ID+"/cards", url.Values{"listId": {l.ID}, "swimlaneId": {sw.ID}, "title": {title}, "position": {pos}})
		if err != nil || r.StatusCode != http.StatusSeeOther {
			t.Fatalf("add %s: %v %v", title, err, r.StatusCode)
		}
	}
	add("New Top Card", "top")
	add("New Bottom Card", "bottom")
	cur, _ := w.db.Collection("cards").Find(ctx, bson.M{"listId": l.ID}, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}}))
	var cards []Card
	_ = cur.All(ctx, &cards)
	if len(cards) != 3 || cards[0].Title != "New Top Card" || cards[2].Title != "New Bottom Card" {
		t.Fatalf("top first, bottom last: %v", cards)
	}
}

// WeKan's card list picker: for whoever may change the card; others only see the list.
func TestCardListPicker(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	ro := w.seedUser(t, "rob", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Pick")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$push": bson.M{"members": bson.M{"userId": ro.ID, "isActive": true, "isReadOnly": true}}})
	la, _ := w.s.AddList(ctx, b, "Todo")
	_, _ = w.s.AddList(ctx, b, "Done")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	c, _ := w.s.AddCard(ctx, b, u, la.ID, sw.ID, "C")
	cardURL := w.srv.URL + "/b/" + b.ID + "/" + b.Slug + "/" + c.ID
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	if _, html := get(t, ann, cardURL); !strings.Contains(html, "card-details-list-picker") || !strings.Contains(html, ">Done</button>") {
		t.Fatal("the picker, with the board's other lists")
	}
	rob := w.client(t)
	body(t, w.signIn(t, rob, "rob", "pw"))
	if _, html := get(t, rob, cardURL); strings.Contains(html, "card-details-list-picker") || !strings.Contains(html, "<span>Todo</span>") {
		t.Fatal("negative: a read-only member sees the list, and no picker")
	}
}

// Every pop-over the page's script opens has its template - a template lost
// in an edit leaves a button that silently opens nothing.
func TestEveryPopOverHasItsTemplate(t *testing.T) {
	js, _ := os.ReadFile("static/weddy.js")
	var tpl strings.Builder
	files, _ := filepath.Glob("templates/*.pug")
	for _, f := range files {
		b, _ := os.ReadFile(f)
		tpl.Write(b)
	}
	for _, m := range regexp.MustCompile(`openPopOver\('([a-z-]+)'\)`).FindAllStringSubmatch(string(js), -1) {
		if !strings.Contains(tpl.String(), `data-pop="`+m[1]+`"`) {
			t.Errorf("openPopOver('%s') has no template", m[1])
		}
	}
}

// WeKan's archiveSelectedCards, with GHSA-6xqf-jh8x-4wxc's card check: an
// assigned-only member archives only their own cards, and a selection with
// one card not theirs changes nothing.
func TestArchiveSelectedCards(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Bulk")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$push": bson.M{"members": bson.M{"userId": bob.ID, "isActive": true, "isNormalAssignedOnly": true}}})
	l, _ := w.s.AddList(ctx, b, "L")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	theirs, _ := w.s.AddCard(ctx, b, u, l.ID, sw.ID, "Theirs")
	mine, _ := w.s.AddCard(ctx, b, u, l.ID, sw.ID, "Mine")
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": mine.ID}, bson.M{"$set": bson.M{"assignees": bson.A{bob.ID}}})
	call := func(cl *http.Client, args string) map[string]any {
		req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/archiveSelectedCards", strings.NewReader(args))
		req.Header.Set("Content-Type", "application/json")
		res, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		return out
	}
	archived := func(id string) bool {
		var c Card
		_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": id}).Decode(&c)
		return c.Archived
	}
	bc := w.client(t)
	body(t, w.signIn(t, bc, "bob", "pw"))
	if out := call(bc, `["`+b.ID+`",["`+mine.ID+`","`+theirs.ID+`"]]`); out["error"] != "not-authorized" || archived(mine.ID) || archived(theirs.ID) {
		t.Fatalf("negative: a selection with a card not theirs changes nothing: %v", out)
	}
	var row struct {
		Count int64 `bson:"count"`
	}
	_ = w.db.Collection("eventlog").FindOne(ctx, bson.M{"bleed": "UnassignedArchiveBleed"}).Decode(&row)
	if row.Count != 1 {
		t.Fatalf("the attempt is recorded: %d", row.Count)
	}
	if out := call(bc, `["`+b.ID+`",["`+mine.ID+`"]]`); out["result"] == nil || !archived(mine.ID) {
		t.Fatalf("their own card: %v", out)
	}
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	if out := call(ann, `["`+b.ID+`",["`+theirs.ID+`","nosuchcard"]]`); out["error"] != "invalid-card-selection" || archived(theirs.ID) {
		t.Fatalf("negative: every card of the board: %v", out)
	}
	if out := call(ann, `["`+b.ID+`",["`+theirs.ID+`"]]`); out["result"] == nil || !archived(theirs.ID) {
		t.Fatalf("the board admin: %v", out)
	}
}

// WeKan's notifications drawer: the clear item, only of boards the user may
// still read, escaped; mark all read and delete all touch only their own.
func TestNotificationsDrawer(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Mine")
	gone, _ := w.s.CreateBoard(ctx, bob, "Left")
	l, _ := w.s.AddList(ctx, b, "Todo")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	c, _ := w.s.AddCard(ctx, b, u, l.ID, sw.ID, `Alpha <script>x</script>`)
	_, _ = w.db.Collection("activities").InsertMany(ctx, []any{
		bson.M{"_id": "actseen0000001", "userId": bob.ID, "activityType": "createCard", "boardId": b.ID, "cardId": c.ID, "cardTitle": c.Title, "listId": l.ID, "swimlaneId": sw.ID, "createdAt": time.Now()},
		bson.M{"_id": "acthidden00001", "userId": bob.ID, "activityType": "createCard", "boardId": gone.ID, "cardTitle": "Secret", "createdAt": time.Now()},
	})
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": u.ID}, bson.M{"$set": bson.M{"profile.notifications": bson.A{
		bson.M{"activity": "actseen0000001", "read": nil}, bson.M{"activity": "acthidden00001", "read": nil}}}})
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": bob.ID}, bson.M{"$set": bson.M{"profile.notifications": bson.A{bson.M{"activity": "actseen0000001", "read": nil}}}})
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	_, html := get(t, ann, w.srv.URL+"/")
	if !strings.Contains(html, `id="notifications-drawer"`) || !strings.Contains(html, `href="/b/`+b.ID+`/`+b.Slug+`/`+c.ID+`"`) || !strings.Contains(html, "<b>Mine</b>: <b>BOB</b>") {
		t.Fatal("the drawer, its clear item linking the card")
	}
	if strings.Contains(html, "<script>x</script>") || !strings.Contains(html, "&lt;script&gt;") {
		t.Fatal("negative: a card title is escaped")
	}
	if strings.Contains(html, "Secret") {
		t.Fatal("negative: a board the user may not read keeps none of its activity in the drawer")
	}
	if r, _ := ann.PostForm(w.srv.URL+"/notifications/read-all", nil); r.StatusCode != 200 || w.s.unreadNotifications(ctx, u.ID) != 0 {
		t.Fatal("all read")
	}
	if r, _ := ann.PostForm(w.srv.URL+"/notifications/delete-all", nil); r.StatusCode != 200 {
		t.Fatal("delete all")
	}
	var d struct {
		Profile struct {
			Notifications []bson.M `bson:"notifications"`
		} `bson:"profile"`
	}
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"_id": u.ID}).Decode(&d)
	if len(d.Profile.Notifications) != 0 || w.s.unreadNotifications(ctx, bob.ID) != 1 {
		t.Fatal("negative: only the user's own are deleted")
	}
}

// WeKan's watch method for a list: watched by whoever may read the board,
// shown with the eye; only their own watching changes; notifications follow.
func TestWatchList(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Watch")
	l, _ := w.s.AddList(ctx, b, "Todo")
	call := func(cl *http.Client, args string) map[string]any {
		req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/watch", strings.NewReader(args))
		req.Header.Set("Content-Type", "application/json")
		res, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		return out
	}
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	if out := call(ann, `["list","`+l.ID+`","watching"]`); out["result"] != true {
		t.Fatalf("watched: %v", out)
	}
	if _, html := get(t, ann, w.srv.URL+"/b/"+b.ID+"/"+b.Slug); !strings.Contains(html, "list-header-watch-icon") {
		t.Fatal("the eye on the list")
	}
	bc := w.client(t)
	body(t, w.signIn(t, bc, "bob", "pw"))
	if out := call(bc, `["list","`+l.ID+`","watching"]`); out["error"] != "error-board-notAMember" {
		t.Fatalf("negative: not on a board they cannot read: %v", out)
	}
	if out := call(ann, `["list","`+l.ID+`","tracking"]`); out["error"] != "Match.Error" {
		t.Fatal("negative: watching or null only")
	}
	if out := call(ann, `["user","`+u.ID+`","watching"]`); out["error"] != "error-json-schema" {
		t.Fatal("negative: a list or a card only")
	}
	var got List
	_ = w.db.Collection("lists").FindOne(ctx, bson.M{"_id": l.ID}).Decode(&got)
	if !slices.Contains(got.Watchers, u.ID) || slices.Contains(got.Watchers, bob.ID) {
		t.Fatalf("only their own watching: %v", got.Watchers)
	}
	call(ann, `["list","`+l.ID+`",null]`)
	_ = w.db.Collection("lists").FindOne(ctx, bson.M{"_id": l.ID}).Decode(&got)
	if slices.Contains(got.Watchers, u.ID) {
		t.Fatal("unwatched")
	}
}
