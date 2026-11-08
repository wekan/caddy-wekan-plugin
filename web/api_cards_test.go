package web

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestRESTCardsAndUsers(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	u := w.seedUser(t, "ada", "pw")
	v := w.seedUser(t, "ben", "pw")
	admin := w.seedUser(t, "root", "pw")
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": admin.ID}, bson.M{"$set": bson.M{"isAdmin": true}})
	api := w.srv.URL
	token := func(name string) string {
		var l struct{ Token string }
		call(t, "POST", api+"/users/login", "", `{"username":"`+name+`","password":"pw"}`, &l)
		return l.Token
	}
	ta, tb, tr := token("ada"), token("ben"), token("root")
	b, _ := w.s.CreateBoard(ctx, u, "Cards")
	l1, _ := w.s.AddList(ctx, b, "One")
	l2, _ := w.s.AddList(ctx, b, "Two")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	board := api + "/api/boards/" + b.ID

	// A card Node.js WeKan wrote with a high number, the board's counter not moved.
	_, _ = w.db.Collection("cards").InsertOne(ctx, bson.M{"_id": "nodeCard0000001", "title": "old", "boardId": b.ID,
		"listId": l1.ID, "swimlaneId": sw.ID, "cardNumber": 301, "sort": 5, "archived": false})
	cards := make([]string, 8)
	for i := range cards {
		cards[i] = fmt.Sprintf(`{"title":"Bulk %d"}`, i)
	}
	var created []struct {
		ID string `json:"_id"`
	}
	call(t, "POST", board+"/lists/"+l1.ID+"/cards/bulk", ta, `{"swimlaneId":"`+sw.ID+`","cards":[`+strings.Join(cards, ",")+`]}`, &created)
	numbers := map[int64]bool{}
	cur, _ := w.db.Collection("cards").Find(ctx, bson.M{"boardId": b.ID})
	var all []Card
	_ = cur.All(ctx, &all)
	for _, c := range all {
		if numbers[c.CardNumber] {
			t.Fatalf("#5813: card number %d twice", c.CardNumber)
		}
		numbers[c.CardNumber] = true
		if c.ID != "nodeCard0000001" && c.CardNumber <= 301 {
			t.Fatalf("a new number above the highest existing one: %d", c.CardNumber)
		}
	}
	if len(created) != 8 {
		t.Fatalf("bulk create: %d", len(created))
	}

	// Bulk delete.
	ids := make([]string, 0, 8)
	for _, c := range created {
		ids = append(ids, `"`+c.ID+`"`)
	}
	var del struct {
		Deleted []string `json:"deleted"`
	}
	if code := call(t, "DELETE", board+"/cards/bulk", tb, `{"cardIds":[`+strings.Join(ids, ",")+`]}`, nil); code != 403 {
		t.Fatalf("negative: a non-member bulk deletes: %d", code)
	}
	call(t, "DELETE", board+"/cards/bulk", ta, `{"cardIds":[`+strings.Join(ids, ",")+`]}`, &del)
	if len(del.Deleted) != 8 {
		t.Fatalf("#4743 bulk delete: %v", del.Deleted)
	}

	// Delete with children, one deleteCard activity.
	c, _ := w.s.AddCard(ctx, b, u, l1.ID, sw.ID, "Parent")
	for coll, extra := range map[string]bson.M{"checklists": {"title": "CL"}, "checklistItems": {"title": "i"}, "card_comments": {"text": "hi"}} {
		extra["_id"], extra["cardId"], extra["boardId"] = randomID(), c.ID, b.ID
		_, _ = w.db.Collection(coll).InsertOne(ctx, extra)
	}
	if code := call(t, "DELETE", board+"/lists/"+l1.ID+"/cards/"+c.ID, ta, "", nil); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	for _, coll := range []string{"checklists", "checklistItems", "card_comments"} {
		if n, _ := w.db.Collection(coll).CountDocuments(ctx, bson.M{"cardId": c.ID}); n != 0 {
			t.Errorf("#3252: %s of the deleted card remain", coll)
		}
	}
	if n, _ := w.db.Collection("activities").CountDocuments(ctx, bson.M{"cardId": c.ID, "activityType": "deleteCard"}); n != 1 {
		t.Fatalf("one deleteCard activity: %d", n)
	}

	// Copy to position 0, with checklists.
	src, _ := w.s.AddCard(ctx, b, u, l1.ID, sw.ID, "Source")
	_, _ = w.db.Collection("checklists").InsertOne(ctx, bson.M{"_id": "srcChecklist001", "cardId": src.ID, "title": "Steps"})
	_, _ = w.db.Collection("checklistItems").InsertOne(ctx, bson.M{"_id": "srcItem00000001", "checklistId": "srcChecklist001", "cardId": src.ID, "title": "one"})
	first, _ := w.s.AddCard(ctx, b, u, l2.ID, sw.ID, "Already there")
	var cp struct {
		ID string `json:"_id"`
	}
	call(t, "POST", board+"/lists/"+l1.ID+"/cards/"+src.ID+"/copy", ta, `{"toBoardId":"`+b.ID+`","toSwimlaneId":"`+sw.ID+`","toListId":"`+l2.ID+`","position":0}`, &cp)
	var copied Card
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": cp.ID}).Decode(&copied)
	if copied.Title != "Source" || copied.ListID != l2.ID || copied.Sort >= first.Sort {
		t.Fatalf("a copy at position 0: %+v (first %v)", copied, first.Sort)
	}
	if n, _ := w.db.Collection("checklistItems").CountDocuments(ctx, bson.M{"cardId": cp.ID}); n != 1 {
		t.Fatal("the copy has the checklist and its item")
	}
	other, _ := w.s.CreateBoard(ctx, v, "Ben's")
	if code := call(t, "POST", board+"/lists/"+l1.ID+"/cards/"+src.ID+"/copy", ta, `{"toBoardId":"`+other.ID+`","toSwimlaneId":"x","toListId":"y"}`, nil); code != 403 {
		t.Fatalf("negative: a copy to a board the caller is not on: %d", code)
	}

	// Card settings.
	var set map[string]any
	call(t, "GET", board+"/cardSettings", ta, "", &set)
	if set["allowsDueDate"] != true {
		t.Fatalf("#3062 card settings: %v", set)
	}
	call(t, "PUT", board+"/cardSettings", ta, `{"allowsDueDate":false}`, nil)
	var raw bson.M
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": b.ID}).Decode(&raw)
	if raw["allowsDueDate"] != false {
		t.Fatal("allowsDueDate saved")
	}
	if code := call(t, "PUT", board+"/cardSettings", ta, `{"title":"hacked"}`, nil); code != 400 {
		t.Fatalf("negative: a field that is no allows* setting: %d", code)
	}
	if code := call(t, "PUT", board+"/cardSettings", tb, `{"allowsDueDate":true}`, nil); code != 403 {
		t.Fatalf("negative: a non-member changes the settings: %d", code)
	}

	// My Cards.
	due, _ := w.s.AddCard(ctx, b, u, l1.ID, sw.ID, "Mine")
	call(t, "PUT", board+"/lists/"+l1.ID+"/cards/"+due.ID, ta, `{"members":["`+u.ID+`"],"dueAt":"2026-06-07T12:00:00.000Z"}`, nil)
	var mine []map[string]any
	call(t, "GET", api+"/api/user/cards?due=true", ta, "", &mine)
	if len(mine) != 1 || mine[0]["_id"] != due.ID {
		t.Fatalf("#4815 My Cards with due: %v", mine)
	}
	mine = nil
	call(t, "GET", api+"/api/user/cards", tb, "", &mine)
	if len(mine) != 0 {
		t.Fatal("negative: another user's cards are not ben's")
	}
	if code := call(t, "GET", api+"/api/user/cards", "", "", nil); code != 401 {
		t.Fatalf("negative: without a token: %d", code)
	}

	// Deleting users.
	gone := w.seedUser(t, "gone", "pw")
	if code := call(t, "DELETE", api+"/api/users/"+gone.ID, ta, "", nil); code != 403 {
		t.Fatalf("negative: a non-admin deletes a user: %d", code)
	}
	var d map[string]string
	if code := call(t, "DELETE", api+"/api/users/"+gone.ID, tr, "", &d); code != 200 || d["_id"] != gone.ID {
		t.Fatalf("delete a user: %d %v", code, d)
	}
	d = nil
	if code := call(t, "DELETE", api+"/api/users/"+gone.ID, tr, "", &d); code != 404 || d["error"] != "User not found" {
		t.Fatalf("a repeat is 404 User not found: %d %v", code, d)
	}
	if code := call(t, "DELETE", api+"/api/users/"+admin.ID, tr, "", nil); code != 400 {
		t.Fatalf("negative: an admin deletes themself: %d", code)
	}
}
