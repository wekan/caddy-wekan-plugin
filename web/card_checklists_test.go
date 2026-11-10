package web

import (
	"context"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// finishedPercent's Math.round, half up.
func TestChecklistPercent(t *testing.T) {
	for _, c := range []struct{ done, count, want int }{{0, 0, 0}, {0, 3, 0}, {1, 3, 33}, {2, 3, 67}, {1, 8, 13}, {3, 3, 100}} {
		if got := checklistPercent(c.done, c.count); got != c.want {
			t.Errorf("%d/%d: %d, want %d", c.done, c.count, got, c.want)
		}
	}
}

// The card details' checklists, sorted, with their progress and items; the
// menu that opens their Export only for whoever may change the card, and
// never another card's checklist.
func TestCardDetailsChecklists(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	ro := w.seedUser(t, "rob", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Lists")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$push": bson.M{"members": bson.M{"userId": ro.ID, "isActive": true, "isReadOnly": true}}})
	l, _ := w.s.AddList(ctx, b, "L")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	c, _ := w.s.AddCard(ctx, b, u, l.ID, sw.ID, "C")
	other, _ := w.s.AddCard(ctx, b, u, l.ID, sw.ID, "Other")
	_, _ = w.db.Collection("checklists").InsertMany(ctx, []any{
		bson.M{"_id": "clB", "cardId": c.ID, "title": "Second list", "sort": 2},
		bson.M{"_id": "clA", "cardId": c.ID, "title": "First list", "sort": 1},
		bson.M{"_id": "clX", "cardId": other.ID, "title": "Not this card", "sort": 0},
	})
	_, _ = w.db.Collection("checklistItems").InsertMany(ctx, []any{
		bson.M{"_id": "i2", "checklistId": "clA", "cardId": c.ID, "title": "Item two", "sort": 2, "isFinished": false},
		bson.M{"_id": "i1", "checklistId": "clA", "cardId": c.ID, "title": "Item one", "sort": 1, "isFinished": true},
		bson.M{"_id": "i3", "checklistId": "clA", "cardId": c.ID, "title": "Item three", "sort": 3, "isFinished": false},
	})
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	_, html := get(t, ann, w.srv.URL+"/b/"+b.ID+"/"+b.Slug+"/"+c.ID)
	first, second := strings.Index(html, "First list"), strings.Index(html, "Second list")
	if first < 0 || second < first {
		t.Fatalf("both checklists, in their sort order: %d %d", first, second)
	}
	if one, two, three := strings.Index(html, "Item one"), strings.Index(html, "Item two"), strings.Index(html, "Item three"); !(one < two && two < three && one > 0) {
		t.Errorf("the items in their sort order: %d %d %d", one, two, three)
	}
	if !strings.Contains(html, `aria-valuenow="33"`) || !strings.Contains(html, ">33%<") {
		t.Error("one of three finished is 33%")
	}
	if !strings.Contains(html, `checklist-item is-checked" role="checkbox" aria-checked="true"`) {
		t.Error("a finished item is checked")
	}
	if !strings.Contains(html, `js-open-checklist-details-menu" href="#" data-checklist="clA"`) || !strings.Contains(html, "js-export-checklist") {
		t.Error("the checklist's menu and its Export, for the card's editor")
	}
	if strings.Contains(html, "Not this card") {
		t.Error("negative: another card's checklist is not drawn")
	}
	if !strings.Contains(html, `data-title-checklist="Export checklist"`) {
		t.Error("the checklist's Export pop-over is titled as WeKan's")
	}
	rob := w.client(t)
	body(t, w.signIn(t, rob, "rob", "pw"))
	_, html = get(t, rob, w.srv.URL+"/b/"+b.ID+"/"+b.Slug+"/"+c.ID)
	if !strings.Contains(html, "First list") || strings.Contains(html, "js-open-checklist-details-menu") {
		t.Error("negative: a read-only member sees the checklists, without their menu")
	}
}
