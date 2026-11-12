package web

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// A member who leaves a board - removed by an admin, or quitting - leaves its
// cards, lists and watchers as WeKan's Boards.before.update hook has them
// leave, and each card's change is in the change history by whoever did it.
func TestMemberLeavesBoardAsWeKan(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	cat := w.seedUser(t, "cat", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Leave")
	var ann User
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"username": "ann"}).Decode(&ann)
	for _, u := range []*User{bob, cat} {
		_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": rb.ID}, bson.M{"$push": bson.M{"members": bson.M{
			"userId": u.ID, "isActive": true, "isAdmin": u == cat, "grantedBy": bson.A{"team:t1"}}},
			"$addToSet": bson.M{"watchers": bson.M{"userId": u.ID, "level": "watching"}}})
		_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": u.ID}, bson.M{"$addToSet": bson.M{"profile.starredBoards": rb.ID}})
	}
	people := bson.A{bob.ID, cat.ID}
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": rb.Card}, bson.M{"$set": bson.M{
		"members": people, "requesters": people, "assigners": people, "watchers": people, "assignees": people}})
	other := "card" + randomID()[:10]
	_, _ = w.db.Collection("cards").InsertOne(ctx, bson.M{"_id": other, "boardId": rb.ID, "listId": rb.List, "swimlaneId": rb.Swimlane,
		"title": "Not theirs", "archived": false, "members": bson.A{ann.ID}})
	_, _ = w.db.Collection("lists").UpdateOne(ctx, bson.M{"_id": rb.List}, bson.M{"$set": bson.M{"watchers": people}})

	ac := w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	if r, _ := ac.PostForm(w.srv.URL+"/b/"+rb.ID+"/members/"+bob.ID+"/remove", url.Values{}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("removed: %d", r.StatusCode)
	}
	cc := w.client(t)
	body(t, w.signIn(t, cc, "cat", "pw"))
	req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/quitBoard", strings.NewReader(`["`+rb.ID+`"]`))
	req.Header.Set("Content-Type", "application/json")
	if res, err := cc.Do(req); err != nil || res.StatusCode != 200 {
		t.Fatalf("cat quits: %v", err)
	}

	var card, untouched, list, board bson.M
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": rb.Card}).Decode(&card)
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": other}).Decode(&untouched)
	_ = w.db.Collection("lists").FindOne(ctx, bson.M{"_id": rb.List}).Decode(&list)
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": rb.ID}).Decode(&board)
	for _, f := range []string{"members", "requesters", "assigners", "watchers"} {
		if a, _ := card[f].(bson.A); len(a) != 0 {
			t.Errorf("gone from the card's %s: %v", f, a)
		}
	}
	if a, _ := card["assignees"].(bson.A); len(a) != 2 {
		t.Errorf("assignees stay, as WeKan's: %v", a)
	}
	if a, _ := list["watchers"].(bson.A); len(a) != 0 {
		t.Errorf("gone from the list's watchers: %v", a)
	}
	if a, _ := board["watchers"].(bson.A); len(a) != 0 {
		t.Errorf("gone from the board's watchers: %v", a)
	}
	var bd Board
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": rb.ID}).Decode(&bd)
	for _, m := range bd.Members {
		if m.UserID == bob.ID || m.UserID == cat.ID {
			if m.IsActive || m.IsAdmin {
				t.Errorf("%s kept, inactive and no admin: %+v", m.UserID, m)
			}
			if _, ok := m.Rest["grantedBy"]; ok {
				t.Errorf("#4178: a removal is final, no grantedBy: %+v", m)
			}
		}
	}
	for _, u := range []*User{bob, cat} {
		var x User
		_ = w.db.Collection("users").FindOne(ctx, bson.M{"_id": u.ID}).Decode(&x)
		if slices.Contains(x.Profile.StarredBoards, rb.ID) {
			t.Errorf("#3249: a private board's star goes: %s", u.Username)
		}
	}

	// The history: the card's members, requesters and assigners, by the admin
	// for bob and by cat for themself; watchers are not a recorded field.
	byUser := map[string][]string{}
	for _, r := range historyRows(t, w, rb.ID) {
		if r["cardId"] == rb.Card && r["entityType"] == "card" {
			byUser[r["userId"].(string)] = append(byUser[r["userId"].(string)], r["group"].(string))
		}
		if r["cardId"] == other {
			t.Errorf("negative: a card they were not on is no row: %v", r)
		}
	}
	for _, who := range []string{ann.ID, cat.ID} {
		g := byUser[who]
		slices.Sort(g)
		if !slices.Contains(g, "members") || !slices.Contains(g, "assignees") {
			t.Errorf("rows by %s: %v", who, g)
		}
	}
	if f := verifyWithWeKan(t, historyRows(t, w, rb.ID)); len(f) != 0 {
		t.Errorf("WeKan's audit: %v", f)
	}
	if untouched["members"].(bson.A)[0] != ann.ID {
		t.Error("negative: another member stays on their card")
	}

}

// A public board's star stays: it is still readable without membership.
func TestMemberLeavesPublicBoardKeepsStar(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Public")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": rb.ID}, bson.M{"$set": bson.M{"permission": "public"},
		"$push": bson.M{"members": bson.M{"userId": bob.ID, "isActive": true, "isAdmin": false}}})
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": bob.ID}, bson.M{"$addToSet": bson.M{"profile.starredBoards": rb.ID}})
	ac := w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	if r, _ := ac.PostForm(w.srv.URL+"/b/"+rb.ID+"/members/"+bob.ID+"/remove", url.Values{}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("removed: %d", r.StatusCode)
	}
	var x User
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"_id": bob.ID}).Decode(&x)
	if !slices.Contains(x.Profile.StarredBoards, rb.ID) {
		t.Error("the star of a public board stays")
	}
}

// A deleted custom field's values leave every card of every board it is on,
// each a History row by whoever deleted it, as WeKan's CustomFields.before.remove
// through the hooked Cards.update.
func TestDeletedCustomFieldIsRecordedOnEveryBoard(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	tok := login(t, w, "ann")
	a := newRESTBoard(t, w, tok, "A")
	b := newRESTBoard(t, w, tok, "B")
	var ann User
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"username": "ann"}).Decode(&ann)
	var f struct {
		ID string `json:"_id"`
	}
	call(t, "POST", w.srv.URL+"/api/boards/"+a.ID+"/custom-fields", tok, `{"name":"Size","type":"text","settings":{}}`, &f)
	_, _ = w.db.Collection("customFields").UpdateOne(ctx, bson.M{"_id": f.ID}, bson.M{"$set": bson.M{"boardIds": bson.A{a.ID, b.ID}}})
	set := func(card string, vals bson.A) {
		_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": card}, bson.M{"$set": bson.M{"customFields": vals}})
	}
	set(a.Card, bson.A{bson.M{"_id": f.ID, "value": "L"}, bson.M{"_id": "keep", "value": 1}})
	set(b.Card, bson.A{bson.M{"_id": f.ID, "value": "S"}})
	plain := "card" + randomID()[:10]
	_, _ = w.db.Collection("cards").InsertOne(ctx, bson.M{"_id": plain, "boardId": a.ID, "listId": a.List, "swimlaneId": a.Swimlane,
		"title": "Without", "archived": false, "customFields": bson.A{bson.M{"_id": "keep", "value": 2}}})
	if code := call(t, "DELETE", w.srv.URL+"/api/boards/"+a.ID+"/custom-fields/"+f.ID, tok, "", nil); code != 200 {
		t.Fatalf("deleted: %d", code)
	}
	fields := func(id string) bson.A {
		var c bson.M
		_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": id}).Decode(&c)
		v, _ := c["customFields"].(bson.A)
		return v
	}
	if len(fields(a.Card)) != 1 || len(fields(b.Card)) != 0 || len(fields(plain)) != 1 {
		t.Fatalf("the values go from both boards, the other field stays: %v %v %v", fields(a.Card), fields(b.Card), fields(plain))
	}
	for _, rb := range []restBoard{a, b} {
		rows := 0
		for _, r := range historyRows(t, w, rb.ID) {
			if r["group"] == "customFields" && r["entityType"] == "card" {
				rows++
				if r["userId"] != ann.ID || r["cardId"] != rb.Card {
					t.Errorf("a row of %s's card by ann: %v", rb.ID, r)
				}
			}
			if r["cardId"] == plain {
				t.Errorf("negative: a card without the field is no row: %v", r)
			}
		}
		if rows != 1 {
			t.Errorf("one customFields row on %s: %d", rb.ID, rows)
		}
		if f := verifyWithWeKan(t, historyRows(t, w, rb.ID)); len(f) != 0 {
			t.Errorf("WeKan's audit of %s: %v", rb.ID, f)
		}
	}
}
