package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// addRule stores a rule of a board: its trigger and action, as WeKan keeps them.
func addRule(t *testing.T, w *testWeKan, boardID, id string, trigger, action bson.M, enabled bool) {
	t.Helper()
	ctx := context.Background()
	trigger["_id"], action["_id"] = id+"t", id+"a"
	if _, ok := trigger["boardId"]; !ok {
		trigger["boardId"] = boardID
	}
	if _, ok := action["boardId"]; !ok {
		action["boardId"] = boardID
	}
	_, _ = w.db.Collection("triggers").InsertOne(ctx, trigger)
	_, _ = w.db.Collection("actions").InsertOne(ctx, action)
	_, _ = w.db.Collection("rules").InsertOne(ctx, bson.M{"_id": id, "boardId": boardID, "title": id, "triggerId": id + "t", "actionId": id + "a", "enabled": enabled})
}

func TestRulesRunOnActivities(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Rules")
	other, _ := w.s.CreateBoard(ctx, u, "Other")
	todo, _ := w.s.AddList(ctx, b, "Todo")
	done, _ := w.s.AddList(ctx, b, "Done")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	// When a card is added to Todo, label it; when it is moved to Done, archive it.
	addRule(t, w, b.ID, "rlabel", bson.M{"activityType": "createCard", "listName": "Todo", "swimlaneName": "*", "userId": "*", "cardTitle": "*"},
		bson.M{"actionType": "addLabel", "labelId": "label0000000001"}, true)
	addRule(t, w, b.ID, "rarch", bson.M{"activityType": "moveCard", "listName": "Done", "oldListName": "*", "userId": "*", "swimlaneName": "*"},
		bson.M{"actionType": "archive"}, true)
	addRule(t, w, b.ID, "roff", bson.M{"activityType": "createCard", "listName": "*"}, bson.M{"actionType": "setColor", "selectedColor": "red"}, false)
	addRule(t, w, other.ID, "rother", bson.M{"activityType": "createCard", "boardId": "*"}, bson.M{"actionType": "setColor", "selectedColor": "blue"}, true)
	c, _ := w.s.AddCard(ctx, b, u, todo.ID, sw.ID, "Alpha")
	var got Card
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": c.ID}).Decode(&got)
	if !slices.Contains(got.LabelIDs, "label0000000001") {
		t.Fatalf("a card added to Todo is labelled: %v", got.LabelIDs)
	}
	var raw bson.M
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": c.ID}).Decode(&raw)
	if raw["color"] != nil {
		t.Fatalf("negative: a disabled rule, and another board's rule, never fire: %v", raw["color"])
	}
	if err := w.s.MoveCards(ctx, b, u, []Move{{CardID: c.ID, ListID: done.ID, SwimlaneID: sw.ID, Sort: 0}}); err != nil {
		t.Fatal(err)
	}
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": c.ID}).Decode(&got)
	if !got.Archived {
		t.Fatal("a card moved to Done is archived")
	}
	// Two rules starting each other stop at WeKan's depth, and it is recorded.
	addRule(t, w, b.ID, "rloop1", bson.M{"activityType": "archivedCard"}, bson.M{"actionType": "unarchive"}, true)
	addRule(t, w, b.ID, "rloop2", bson.M{"activityType": "restoredCard"}, bson.M{"actionType": "archive"}, true)
	c2, _ := w.s.AddCard(ctx, b, u, todo.ID, sw.ID, "Loop")
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": c2.ID}, bson.M{"$set": bson.M{"archived": true}})
	w.s.activity(ctx, u.ID, "archivedCard", bson.M{"boardId": b.ID, "cardId": c2.ID})
	var row struct {
		Count int64 `bson:"count"`
	}
	_ = w.db.Collection("eventlog").FindOne(ctx, bson.M{"bleed": "BypassBleed", "source": "rules"}).Decode(&row)
	if row.Count < 1 {
		t.Fatal("a rule loop is stopped and recorded")
	}
}

func TestRuleCrossBoardMoveNeedsWrite(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Mine")
	foreign, _ := w.s.CreateBoard(ctx, bob, "Foreign")
	l, _ := w.s.AddList(ctx, b, "Todo")
	_, _ = w.s.AddList(ctx, foreign, "Todo")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	addRule(t, w, b.ID, "rmove", bson.M{"activityType": "createCard", "listName": "*"},
		bson.M{"actionType": "moveCardToTop", "boardId": foreign.ID, "listName": "Todo", "swimlaneName": "*"}, true)
	c, _ := w.s.AddCard(ctx, b, u, l.ID, sw.ID, "Stays")
	var got Card
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": c.ID}).Decode(&got)
	if got.BoardID != b.ID {
		t.Fatal("negative: RuleBleed - never onto a board the user may not change")
	}
	var row struct {
		Count int64 `bson:"count"`
	}
	_ = w.db.Collection("eventlog").FindOne(ctx, bson.M{"bleed": "CanaryBleed", "source": "canary:rule.cross-board-write"}).Decode(&row)
	if row.Count != 1 {
		t.Fatalf("the attempt is recorded: %d", row.Count)
	}
}

func TestRunButtonRule(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Buttons")
	victim, _ := w.s.CreateBoard(ctx, bob, "Private")
	mk := func(board *Board, owner *User) *Card {
		l, _ := w.s.AddList(ctx, board, "L")
		var sw Swimlane
		_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": board.ID}).Decode(&sw)
		c, _ := w.s.AddCard(ctx, board, owner, l.ID, sw.ID, "C")
		return c
	}
	mine, theirs := mk(b, u), mk(victim, bob)
	addRule(t, w, b.ID, "rbutton", bson.M{"activityType": "button"}, bson.M{"actionType": "archive"}, true)
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	call := func(args string) (int, map[string]any) {
		req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/"+url.PathEscape("rules.runButton"), strings.NewReader(args))
		req.Header.Set("Content-Type", "application/json")
		res, err := ann.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}
	archived := func(id string) bool {
		var c Card
		_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": id}).Decode(&c)
		return c.Archived
	}
	if _, out := call(`["rbutton","` + theirs.ID + `"]`); out["error"] != "not-authorized" || archived(theirs.ID) {
		t.Fatalf("negative: a button rule cannot archive a foreign private card: %v", out)
	}
	var row struct {
		Count int64 `bson:"count"`
	}
	_ = w.db.Collection("eventlog").FindOne(ctx, bson.M{"bleed": "RuleButtonBleed"}).Decode(&row)
	if row.Count != 1 {
		t.Fatalf("recorded: %d", row.Count)
	}
	if _, out := call(`["rbutton","` + mine.ID + `"]`); out["result"] != true || !archived(mine.ID) {
		t.Fatalf("the board's own card: %v", out)
	}
	if _, out := call(`["nosuchrule","` + mine.ID + `"]`); out["error"] != "not-found" {
		t.Fatalf("negative: no such rule: %v", out)
	}
}
