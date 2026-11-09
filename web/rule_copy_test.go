package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Rules' Copy Card, as WeKan's rule-copy-card spec drives it: copied on
// creation with its due date, checklists and comments; once a causal chain,
// so a same-board copy rule stops; never to a board its user may not change.
func TestRuleCopyCard(t *testing.T) {
	w := newTestWeKan(t)
	ann := w.seedUser(t, "ann", "pw")
	eve := w.seedUser(t, "eve", "pw")
	ctx := context.Background()
	src, _ := w.s.CreateBoard(ctx, ann, "Source")
	dst, _ := w.s.CreateBoard(ctx, ann, "Destination")
	hidden, _ := w.s.CreateBoard(ctx, eve, "Hidden")
	srcList, _ := w.s.AddList(ctx, src, "In")
	dstList, _ := w.s.AddList(ctx, dst, "Out")
	lane := func(b *Board) string {
		var sw Swimlane
		_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
		return sw.ID
	}
	c := w.client(t)
	body(t, w.signIn(t, c, "ann", "pw"))
	call := func(method, args string) map[string]any {
		req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/"+method, strings.NewReader(args))
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
	rule := call("rules.createRule", `["`+src.ID+`","Copy on creation",{"activityType":"createCard","listName":"*","swimlaneName":"*","cardTitle":"*"},`+
		`{"actionType":"copyCard","boardId":"`+dst.ID+`","listId":"`+dstList.ID+`","swimlaneId":"`+lane(dst)+`"}]`)
	ruleRes, _ := rule["result"].(map[string]any)
	if ruleRes == nil {
		t.Fatalf("rule: %v", rule)
	}
	due := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	out := call("createCardWithDueDate", `["`+src.ID+`","`+srcList.ID+`","Automatic copy",{"$date":`+itoa(int(due.UnixMilli()))+`},"`+lane(src)+`"]`)
	sourceID, _ := out["result"].(string)
	if sourceID == "" {
		t.Fatalf("createCardWithDueDate: %v", out)
	}
	count := func(boardID string) int64 {
		n, _ := w.db.Collection("cards").CountDocuments(ctx, bson.M{"boardId": boardID, "title": "Automatic copy"})
		return n
	}
	var copied bson.M
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"boardId": dst.ID, "title": "Automatic copy"}).Decode(&copied)
	if count(dst.ID) != 1 || copied["listId"] != dstList.ID || !plain(copied["dueAt"]).(time.Time).Equal(due) {
		t.Fatalf("copied on creation, with its due date: %d %v", count(dst.ID), copied)
	}
	// A button running the same action copies checklists and comments too.
	if out := call("rules.updateRule", `["`+ruleRes["_id"].(string)+`","Copy on creation",{"activityType":"button","buttonType":"card","buttonLabel":"Copy"},`+
		`{"actionType":"copyCard","boardId":"`+dst.ID+`","listId":"`+dstList.ID+`","swimlaneId":"`+lane(dst)+`"}]`); out["result"] == nil {
		t.Fatalf("updateRule: %v", out)
	}
	var r bson.M
	_ = w.db.Collection("rules").FindOne(ctx, bson.M{"_id": ruleRes["_id"]}).Decode(&r)
	if r["triggerId"] != ruleRes["triggerId"] || r["buttonLabel"] != "Copy" {
		t.Errorf("updated in place, the button's label: %v", r)
	}
	_, _ = w.db.Collection("checklists").InsertOne(ctx, bson.M{"_id": "steps0000000001", "title": "Steps", "cardId": sourceID, "sort": 0})
	_, _ = w.db.Collection("checklistItems").InsertOne(ctx, bson.M{"_id": "item00000000001", "title": "Do it", "cardId": sourceID, "checklistId": "steps0000000001", "sort": 0, "isFinished": false})
	_, _ = w.db.Collection("card_comments").InsertOne(ctx, bson.M{"_id": "comment00000001", "cardId": sourceID, "boardId": src.ID, "userId": ann.ID, "text": "Keep this comment"})
	call("rules.runButton", `["`+ruleRes["_id"].(string)+`","`+sourceID+`"]`)
	var full bson.M
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"boardId": dst.ID, "title": "Automatic copy", "_id": bson.M{"$ne": copied["_id"]}}).Decode(&full)
	fid, _ := full["_id"].(string)
	nc, _ := w.db.Collection("checklists").CountDocuments(ctx, bson.M{"cardId": fid, "title": "Steps"})
	ni, _ := w.db.Collection("checklistItems").CountDocuments(ctx, bson.M{"cardId": fid, "title": "Do it"})
	nm, _ := w.db.Collection("card_comments").CountDocuments(ctx, bson.M{"cardId": fid, "boardId": dst.ID, "text": "Keep this comment"})
	if count(dst.ID) != 2 || nc != 1 || ni != 1 || nm != 1 {
		t.Errorf("a full copy: %d %d %d %d", count(dst.ID), nc, ni, nm)
	}
	// Negative: a stored action cannot smuggle a foreign list in, nor copy to
	// a board its user can no longer change.
	_, _ = w.db.Collection("actions").UpdateOne(ctx, bson.M{"_id": ruleRes["actionId"]}, bson.M{"$set": bson.M{"listId": srcList.ID}})
	call("rules.runButton", `["`+ruleRes["_id"].(string)+`","`+sourceID+`"]`)
	_, _ = w.db.Collection("actions").UpdateOne(ctx, bson.M{"_id": ruleRes["actionId"]}, bson.M{"$set": bson.M{"listId": dstList.ID}})
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": dst.ID}, bson.M{"$set": bson.M{"members": bson.A{}}})
	call("rules.runButton", `["`+ruleRes["_id"].(string)+`","`+sourceID+`"]`)
	if count(dst.ID) != 2 {
		t.Errorf("negative: nothing copied: %d", count(dst.ID))
	}
	// Negative: no rule names a board its creator may not change.
	if out := call("rules.updateRule", `["`+ruleRes["_id"].(string)+`","x",{"activityType":"createCard"},{"actionType":"copyCard","boardId":"`+hidden.ID+`"}]`); out["error"] != "not-authorized" {
		t.Errorf("negative: updateRule to a hidden board: %v", out)
	}
	if out := call("createCardWithDueDate", `["`+hidden.ID+`","x","y",{"$date":0},"z"]`); out["error"] != "not-authorized" {
		t.Errorf("negative: createCardWithDueDate on a board one may not change: %v", out)
	}
	// Same board, on creation: the copy is made once, for each event.
	own := call("rules.createRule", `["`+src.ID+`","Copy once",{"activityType":"createCard","listName":"*","swimlaneName":"*","cardTitle":"*"},`+
		`{"actionType":"copyCard","boardId":"`+src.ID+`","listId":"`+srcList.ID+`","swimlaneId":"`+lane(src)+`"}]`)
	if own["result"] == nil {
		t.Fatalf("same-board rule: %v", own)
	}
	for _, title := range []string{"First independent event", "Second independent event"} {
		call("createCardWithDueDate", `["`+src.ID+`","`+srcList.ID+`","`+title+`",{"$date":0},"`+lane(src)+`"]`)
		if n, _ := w.db.Collection("cards").CountDocuments(ctx, bson.M{"boardId": src.ID, "title": title}); n != 2 {
			t.Errorf("negative: %s copied once, not again: %d", title, n)
		}
	}
}

// The Rules page offers the boards its user may change, and their lists.
func TestCopyCardForm(t *testing.T) {
	w := newTestWeKan(t)
	ann := w.seedUser(t, "ann", "pw")
	eve := w.seedUser(t, "eve", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, ann, "Mine")
	dst, _ := w.s.CreateBoard(ctx, ann, "Target")
	hidden, _ := w.s.CreateBoard(ctx, eve, "Theirs")
	l, _ := w.s.AddList(ctx, dst, "Target list")
	c := w.client(t)
	body(t, w.signIn(t, c, "ann", "pw"))
	_, html := get(t, c, w.srv.URL+"/b/"+b.ID+"/"+b.Slug+"/rules")
	if !strings.Contains(html, `id="board-id-copy"`) || !strings.Contains(html, `value="`+dst.ID+`"`) || !strings.Contains(html, `value="`+l.ID+`" data-board="`+dst.ID+`"`) ||
		!strings.Contains(html, "js-copy-card-action") {
		t.Fatal("the Copy Card form")
	}
	if strings.Contains(html, hidden.ID) {
		t.Error("negative: no board its user may not change")
	}
}
