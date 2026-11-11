package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestHistoryView(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	w.seedUser(t, "eve", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Viewed")
	other := newRESTBoard(t, w, login(t, w, "eve"), "Other")
	var ann User
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"username": "ann"}).Decode(&ann)
	var board Board
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": rb.ID}).Decode(&board)
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": rb.Card}, bson.M{"$set": bson.M{"description": ""}})
	for _, d := range []string{"First text", "Second banana"} {
		_ = w.s.updateRecorded(ctx, ann.ID, "card", rb.Card, bson.M{"$set": bson.M{"description": d}})
	}
	c := w.client(t)
	body(t, w.signIn(t, c, "ann", "pw"))
	page := w.srv.URL + "/b/" + rb.ID + "/history"
	_, html := get(t, c, page)
	if strings.Count(html, "js-history-row") != 2 || !strings.Contains(html, "Second banana") || !strings.Contains(html, "js-history-contributor") {
		t.Fatalf("the board's rows and its contributor")
	}
	if strings.Index(html, "Second banana") > strings.Index(html, "First text") {
		t.Error("newest first")
	}
	if _, h := get(t, c, page+"?q=banana"); strings.Count(h, "js-history-row") != 1 {
		t.Error("searched")
	}
	if _, h := get(t, c, page+"?card="+rb.Card); strings.Count(h, "js-history-row") != 2 {
		t.Error("the card's history")
	}
	if _, h := get(t, c, page+"?user="+ann.ID); strings.Count(h, "js-history-row") != 2 {
		t.Error("a contributor's changes")
	}
	// The menus lead there.
	if _, h := get(t, c, w.srv.URL+"/b/"+rb.ID+"/"+board.Slug+"/"+rb.Card); !strings.Contains(h, `href="/b/`+rb.ID+`/history"`) ||
		!strings.Contains(h, `href="/b/`+rb.ID+`/history?card=`+rb.Card+`"`) {
		t.Error("the board menu and the card menu link to History")
	}
	// Restore the older row from the page: its value is the description again.
	var first bson.M
	_ = w.db.Collection("changeHistory").FindOne(ctx, bson.M{"boardId": rb.ID, "newContent.value": "First text"}).Decode(&first)
	res, _ := post(t, c, w.srv.URL+"/b/"+rb.ID+"/history/restore", url.Values{"id": {docString(first, "_id")}, "back": {"/b/" + rb.ID + "/history"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("restore: %d", res.StatusCode)
	}
	var card bson.M
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": rb.Card}).Decode(&card)
	if card["description"] != "First text" {
		t.Errorf("restored: %v", card["description"])
	}
	// Negative: somebody not on the board, and a card of another board.
	ec := w.client(t)
	body(t, w.signIn(t, ec, "eve", "pw"))
	if res, h := get(t, ec, page); res.StatusCode != http.StatusNotFound || strings.Contains(h, "banana") {
		t.Errorf("negative: not on the board: %d", res.StatusCode)
	}
	if res, _ := get(t, c, page+"?card="+other.Card); res.StatusCode != http.StatusNotFound {
		t.Errorf("negative: another board's card: %d", res.StatusCode)
	}
	if res, _ := post(t, ec, w.srv.URL+"/b/"+rb.ID+"/history/restore", url.Values{"id": {docString(first, "_id")}}); res.StatusCode != http.StatusNotFound {
		t.Errorf("negative: restore from a board not seen: %d", res.StatusCode)
	}
}
