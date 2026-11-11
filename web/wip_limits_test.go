package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// WIP limits: a board admin's methods, and the Archive's Restore with a hard
// limit (#6772) - a full list's card goes to a list with room, Restore all
// says how many it left out, and a soft limit only warns (negative).
func TestWipLimitsAndRestore(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Wip")
	setMember(t, w, rb.ID, Member{UserID: bob.ID, IsActive: true})
	var board Board
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": rb.ID}).Decode(&board)
	other := "listc" + randomID()[:10]
	_, _ = w.db.Collection("lists").InsertOne(ctx, bson.M{"_id": other, "boardId": rb.ID, "title": "List C", "sort": 5, "archived": false})
	caller := func(name string) (*http.Client, func(method, args string) map[string]any) {
		c := w.client(t)
		body(t, w.signIn(t, c, name, "pw"))
		return c, func(method, args string) map[string]any {
			req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/"+url.PathEscape(method), strings.NewReader(args))
			req.Header.Set("Content-Type", "application/json")
			res, err := c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			var out map[string]any
			_ = json.NewDecoder(res.Body).Decode(&out)
			return out
		}
	}
	annC, ann := caller("ann")
	_, bobCall := caller("bob")
	wip := func() wipLimit {
		var l wipList
		_ = w.db.Collection("lists").FindOne(ctx, bson.M{"_id": rb.List}).Decode(&l)
		return l.WipLimit
	}
	if out := bobCall("applyWipLimit", `["`+rb.List+`",3]`); out["error"] != "not-authorized" {
		t.Errorf("negative: a member who is not an admin: %v", out)
	}
	if out := ann("applyWipLimit", `["`+rb.List+`",0]`); out["error"] != nil || wip().Value != 1 {
		t.Errorf("a limit under one is one: %v %v", out, wip())
	}
	if out := ann("applyWipLimit", `["`+rb.List+`","x"]`); out["error"] != "Match.Error" {
		t.Errorf("negative: not a number: %v", out)
	}
	ann("enableWipLimit", `["`+rb.List+`"]`)
	if wl := wip(); !wl.Enabled || wl.Soft {
		t.Fatalf("enabled, hard: %v", wl)
	}

	// The list holds one card (rb.Card); archive a second, then the list is full.
	archived := "card" + randomID()[:12]
	_, _ = w.db.Collection("cards").InsertOne(ctx, bson.M{"_id": archived, "boardId": rb.ID, "listId": rb.List, "swimlaneId": rb.Swimlane,
		"title": "Alpha Card", "archived": true, "sort": 1})
	boardURL := w.srv.URL + "/b/" + rb.ID + "/" + board.Slug
	if _, html := get(t, annC, boardURL); !strings.Contains(html, `data-pop="restore-card-`+archived+`"`) ||
		!strings.Contains(html, "js-restore-wip-reached") || !strings.Contains(html, `<option value="`+other+`">List C</option>`) ||
		strings.Contains(html, `<option value="`+rb.List+`"`) {
		t.Fatal("a full list's card asks for another list, offering List C and not the full one")
	}
	restore := func(form url.Values) int {
		res, _ := post(t, annC, w.srv.URL+"/b/"+rb.ID+"/archive/restore", form)
		return res.StatusCode
	}
	// Its list full: the board again, opening the card's popup - not restored.
	res, _ := post(t, annC, w.srv.URL+"/b/"+rb.ID+"/archive/restore", url.Values{"kind": {"cards"}, "id": {archived}})
	if loc := res.Header.Get("Location"); res.StatusCode != http.StatusSeeOther || !strings.HasSuffix(loc, "?restoreBlocked="+archived) {
		t.Errorf("negative: restored into the full list: %d %s", res.StatusCode, loc)
	} else if _, html := get(t, annC, w.srv.URL+loc); !strings.Contains(html, `js-open-pop-on-load" data-pop="restore-card-`+archived+`"`) {
		t.Error("the board opens the card's popup")
	}
	if code := restore(url.Values{"kind": {"cards"}, "id": {archived}, "toList": {rb.List}}); code != http.StatusConflict {
		t.Errorf("negative: named the full list: %d", code)
	}
	if code := restore(url.Values{"kind": {"cards"}, "id": {archived}, "toList": {other}}); code != http.StatusSeeOther {
		t.Fatalf("restored into List C: %d", code)
	}
	var c Card
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": archived}).Decode(&c)
	if c.Archived || c.ListID != other {
		t.Errorf("the card: archived %v, list %s", c.Archived, c.ListID)
	}

	// Restore all: what has room goes back, the rest is counted.
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": archived}, bson.M{"$set": bson.M{"archived": true, "listId": rb.List}})
	gamma := "card" + randomID()[:12]
	_, _ = w.db.Collection("cards").InsertOne(ctx, bson.M{"_id": gamma, "boardId": rb.ID, "listId": other, "swimlaneId": rb.Swimlane,
		"title": "Gamma Card", "archived": true, "sort": 2})
	res, _ = post(t, annC, w.srv.URL+"/b/"+rb.ID+"/archive/restore-all-cards", url.Values{})
	loc := res.Header.Get("Location")
	if res.StatusCode != http.StatusSeeOther || !strings.HasSuffix(loc, "?restoreSkipped=1") {
		t.Fatalf("Restore all goes back to the board with the count: %d %s", res.StatusCode, loc)
	}
	if _, html := get(t, annC, w.srv.URL+loc); !strings.Contains(html, "js-restore-all-skipped") || !strings.Contains(html, "1 card(s) were not restored") ||
		!strings.Contains(html, `js-open-pop-on-load" data-pop="restore-all-skipped"`) {
		t.Error("the board opens Restore all's left-out count")
	}
	if _, html := get(t, annC, boardURL); strings.Contains(html, "js-restore-all-skipped") {
		t.Error("negative: the plain board opens no left-out count")
	}
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": gamma}).Decode(&c)
	if c.Archived {
		t.Error("Gamma, whose list has room, was not restored")
	}
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": archived}).Decode(&c)
	if !c.Archived {
		t.Error("negative: Alpha went into its full list")
	}

	// A soft limit only warns: Restore restores at once.
	ann("enableSoftLimit", `["`+rb.List+`"]`)
	if !wip().Soft {
		t.Fatal("soft")
	}
	if _, html := get(t, annC, boardURL); strings.Contains(html, `data-pop="restore-card-`+archived+`"`) {
		t.Error("negative: a soft limit asks for another list")
	}
	if code := restore(url.Values{"kind": {"cards"}, "id": {archived}}); code != http.StatusSeeOther {
		t.Errorf("a soft limit restores: %d", code)
	}
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": archived}).Decode(&c)
	if c.Archived || c.ListID != rb.List {
		t.Errorf("restored to its own list: %v %s", c.Archived, c.ListID)
	}
}

func TestWipLimitBlocksRestore(t *testing.T) {
	for _, c := range []struct {
		w     wipLimit
		count int64
		want  bool
	}{
		{wipLimit{Value: 1, Enabled: true}, 1, true},
		{wipLimit{Value: 1, Enabled: true}, 3, true}, // over, after a lowered limit
		{wipLimit{Value: 2, Enabled: true}, 1, false},
		{wipLimit{Value: 1, Enabled: true, Soft: true}, 5, false},
		{wipLimit{Value: 1}, 5, false},
	} {
		if c.w.blocksRestore(c.count) != c.want {
			t.Errorf("%+v at %d: want %v", c.w, c.count, c.want)
		}
	}
}
