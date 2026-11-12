package web

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// #6813, WeKan's checklist-same-board-canary spec in Go: a drag within one's
// own board is no canary; a move into a card of a board the mover cannot
// write to - by cardId, checklistId or boardId, or to a card that is not
// there - is ChecklistBleed's canary, detected, never blocking the account,
// and nothing moves.
func TestChecklistCrossBoardCanary(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	w.seedUser(t, "owner", "pw")
	own := newRESTBoard(t, w, login(t, w, "ann"), "Own")
	mine2 := newRESTBoard(t, w, login(t, w, "ann"), "Mine too")
	other := newRESTBoard(t, w, login(t, w, "owner"), "Other")
	_, _ = w.db.Collection("checklists").InsertMany(ctx, []any{
		bson.M{"_id": "clOwn", "cardId": own.Card, "boardId": own.ID, "title": "Steps", "sort": 0},
		bson.M{"_id": "clOther", "cardId": other.Card, "boardId": other.ID, "title": "Theirs", "sort": 0},
	})
	_, _ = w.db.Collection("checklistItems").InsertOne(ctx, bson.M{"_id": "ciOwn", "checklistId": "clOwn", "cardId": own.Card, "boardId": own.ID, "title": "One", "sort": 0})
	ac := w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	canaries := func() int64 {
		n, _ := w.db.Collection("eventlog").CountDocuments(ctx, bson.M{"bleed": "ChecklistBleed"})
		return n
	}
	for _, mod := range []string{
		`{"$set":{"cardId":"` + own.Card + `","checklistId":"clOwn","sort":1}}`,
		`{"$set":{"cardId":"` + mine2.Card + `"}}`, // a board ann may write: not an attack
	} {
		_, _ = methodCall(t, w, ac, "/checklistItems/update", `[{"_id":"ciOwn"},`+mod+`]`)
	}
	if n := canaries(); n != 0 {
		t.Fatalf("negative: a drag on one's own board, or onto another of one's boards, is no canary: %d", n)
	}
	attacks := map[string]struct{ coll, id, mod string }{
		"item by card":      {"checklistItems", "ciOwn", `{"$set":{"cardId":"` + other.Card + `"}}`},
		"item by checklist": {"checklistItems", "ciOwn", `{"$set":{"checklistId":"clOther"}}`},
		"item by board":     {"checklistItems", "ciOwn", `{"$set":{"boardId":"` + other.ID + `"}}`},
		"a missing card":    {"checklistItems", "ciOwn", `{"$set":{"cardId":"noSuchCard12345"}}`},
		"checklist by card": {"checklists", "clOwn", `{"$set":{"cardId":"` + other.Card + `"}}`},
	}
	for _, c := range attacks {
		_, _ = methodCall(t, w, ac, "/"+c.coll+"/update", `[{"_id":"`+c.id+`"},`+c.mod+`]`)
	}
	// One summary row per source; together they count every attempt.
	cur, _ := w.db.Collection("eventlog").Find(ctx, bson.M{"bleed": "ChecklistBleed"})
	var rows []bson.M
	_ = cur.All(ctx, &rows)
	total := 0
	for _, row := range rows {
		if row["action"] != "detected" {
			t.Errorf("a canary is detected, not blocked: %v", row["action"])
		}
		switch n := row["count"].(type) {
		case int32:
			total += int(n)
		case int64:
			total += int(n)
		}
	}
	if total != len(attacks) {
		t.Errorf("every attempt counted: %d of %d", total, len(attacks))
	}
	var item, me bson.M
	_ = w.db.Collection("checklistItems").FindOne(ctx, bson.M{"_id": "ciOwn"}).Decode(&item)
	if item["cardId"] != own.Card || item["checklistId"] != "clOwn" {
		t.Errorf("nothing moved: %v", item)
	}
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"username": "ann"}).Decode(&me)
	if me["loginDisabled"] == true {
		t.Error("a canary never blocks the account")
	}
}
