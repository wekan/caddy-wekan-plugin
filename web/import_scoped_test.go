package web

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// sortsAfter is insertPosition.js's: at the top, after the last, or evenly
// in the gap.
func TestSortsAfter(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	for _, c := range []struct {
		existing []float64
		target   *float64
		count    int
		want     []float64
	}{
		{nil, nil, 2, []float64{0, 1}},
		{nil, f(4), 2, []float64{5, 6}},
		{[]float64{3, 1, 2}, nil, 2, []float64{-1, 0}},
		{[]float64{0, 1, 2}, f(2), 2, []float64{3, 4}},
		{[]float64{0, 1, 2}, f(1), 1, []float64{1.5}},
		{[]float64{0, 4}, f(0), 3, []float64{1, 2, 3}},
		{[]float64{0, 1}, f(0), 0, nil},
	} {
		if got := sortsAfter(c.existing, c.target, c.count); !reflect.DeepEqual(got, c.want) {
			t.Errorf("sortsAfter(%v, %v, %d) = %v, not %v", c.existing, c.target, c.count, got, c.want)
		}
	}
}

// importScoped, as WeKan's ScopedImporter: a list after the list whose menu
// was used, a card below the card, a document's swimlanes at the top of the
// board with its lists matched by title, custom fields by name, the
// selection's parts only - and a list, swimlane or card of another board
// refused, recorded, and nothing written.
func TestImportScoped(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	a, bt := login(t, w, "ann"), login(t, w, "bob")
	b := newRESTBoard(t, w, a, "Into")
	other := newRESTBoard(t, w, bt, "Private")
	c := w.client(t)
	body(t, w.signIn(t, c, "ann", "pw"))
	_, _ = w.db.Collection("customFields").InsertOne(ctx, bson.M{"_id": "fieldOnBoard001", "boardIds": bson.A{b.ID}, "name": "Size", "type": "text"}) // inserted, not a filter
	doc := map[string]any{"_format": "wekan-board-1.0.0",
		"swimlanes": []any{map[string]any{"_id": "laneX", "title": "Imported lane"}},
		"lists":     []any{map[string]any{"_id": "listX", "title": "L"}, map[string]any{"_id": "listY", "title": "New list"}},
		"cards": []any{map[string]any{"_id": "cardX", "title": "planted", "listId": "listX", "swimlaneId": "laneX", "description": "D",
			"dueAt": "2026-10-10T00:00:00.000Z", "customFields": []any{map[string]any{"_id": "fieldFromFile1", "value": "XL"}}}},
		"customFields":   []any{map[string]any{"_id": "fieldFromFile1", "name": "Size"}},
		"comments":       []any{map[string]any{"_id": "cm1", "cardId": "cardX", "text": "a comment", "userId": bob.ID}},
		"checklists":     []any{map[string]any{"_id": "cl1", "cardId": "cardX", "title": "Steps"}},
		"checklistItems": []any{map[string]any{"_id": "ci1", "checklistId": "cl1", "cardId": "cardX", "title": "One", "isFinished": true}},
	}
	call := func(target map[string]any, fields any) map[string]any {
		raw, _ := json.Marshal([]any{target, doc, fields})
		req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/importScoped", strings.NewReader(string(raw)))
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
	planted := func() []bson.M {
		cur, _ := w.db.Collection("cards").Find(ctx, bson.M{"title": "planted"})
		var out []bson.M
		_ = cur.All(ctx, &out)
		return out
	}
	// The attack: another board's list, swimlane and card.
	for _, target := range []map[string]any{{"listId": other.List, "swimlaneId": other.Swimlane}, {"swimlaneId": other.Swimlane}, {"cardId": other.Card}, {"cardId": "doesNotExistAnywhere1"}} {
		target["boardId"] = b.ID
		if out := call(target, nil); out["error"] != "error-import-placement" || !strings.HasSuffix(strOf(out["reason"]), "does not belong to this board.") {
			t.Errorf("negative: %v is refused: %v", target, out)
		}
	}
	if len(planted()) != 0 {
		t.Fatal("negative: nothing is planted")
	}
	if n, _ := w.db.Collection("eventlog").CountDocuments(ctx, bson.M{"bleed": "ImportPlacementBleed", "source": "method:importScoped"}); n != 1 {
		t.Errorf("the refusals are in Admin Panel -> Problems: %d", n)
	}
	// Another board altogether: refused as not writable.
	if out := call(map[string]any{"boardId": other.ID}, nil); out["error"] == nil {
		t.Errorf("negative: a board ann may not change: %v", out)
	}
	// Beside the board's own card: below it, in its list and swimlane, with
	// the selection's parts.
	out := call(map[string]any{"boardId": b.ID, "cardId": b.Card}, []any{"description", "checklists", "custom-fields"})
	res, _ := out["result"].(map[string]any)
	if res["cards"] != float64(1) || res["checklists"] != float64(1) || res["comments"] != float64(0) {
		t.Fatalf("a card's import: %v", out)
	}
	var target, got bson.M
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": b.Card}).Decode(&target)
	got = planted()[0]
	if got["listId"] != target["listId"] || got["swimlaneId"] != target["swimlaneId"] || got["description"] != "D" || got["dueAt"] != nil {
		t.Errorf("below the card, its list and lane, description but no dates: %v", got)
	}
	if ts, _ := toFloat(plain(target["sort"])); func() bool { gs, _ := toFloat(plain(got["sort"])); return gs <= ts }() {
		t.Errorf("below the card: sort %v after %v", got["sort"], target["sort"])
	}
	if cf, _ := got["customFields"].(bson.A); len(cf) != 1 || docMap(cf[0].(bson.D))["_id"] != "fieldOnBoard001" {
		t.Errorf("the custom field by name: %v", got["customFields"])
	}
	// A list's import: the document's lists after it, as new lists.
	before, _ := w.db.Collection("lists").CountDocuments(ctx, bson.M{"boardId": b.ID})
	out = call(map[string]any{"boardId": b.ID, "listId": b.List}, nil)
	if res, _ := out["result"].(map[string]any); res["lists"] != float64(2) {
		t.Errorf("a list's import makes its lists: %v", out)
	}
	if n, _ := w.db.Collection("lists").CountDocuments(ctx, bson.M{"boardId": b.ID}); n != before+2 {
		t.Errorf("two new lists: %d", n-before)
	}
	// The board's import: its swimlanes, and the lists matched by title.
	before, _ = w.db.Collection("lists").CountDocuments(ctx, bson.M{"boardId": b.ID, "title": "L"})
	out = call(map[string]any{"boardId": b.ID}, nil)
	if res, _ := out["result"].(map[string]any); res["swimlanes"] != float64(1) || res["cards"] != float64(1) || res["comments"] != float64(1) {
		t.Errorf("the board's import: %v", out)
	}
	if n, _ := w.db.Collection("lists").CountDocuments(ctx, bson.M{"boardId": b.ID, "title": "L"}); n != before {
		t.Errorf("negative: a list of the same title is reused, not made again: %d -> %d", before, n)
	}
	var comment bson.M
	_ = w.db.Collection("card_comments").FindOne(ctx, bson.M{"text": "a comment"}).Decode(&comment)
	if comment["userId"] == bob.ID {
		t.Error("negative: a comment is the importer's, not its exported author's")
	}
}
