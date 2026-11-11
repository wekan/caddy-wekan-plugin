package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// #1023: undo restores a deleted list and its cards - and only those - and
// redo deletes them again; a read-only member cannot undo; a tampered row is
// refused; two undos at once apply once.
func TestListDeleteUndoRedo(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Undo")
	var board Board
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": rb.ID}).Decode(&board)
	var annU User
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"username": "ann"}).Decode(&annU)
	separate := "separate" + randomID()[:8]
	_, _ = w.db.Collection("cards").InsertOne(ctx, bson.M{"_id": separate, "boardId": rb.ID, "listId": rb.List, "swimlaneId": rb.Swimlane,
		"title": "Previously deleted", "archived": false, "deletedAt": time.Now().UTC(), "deletedBy": annU.ID, "deleteBatchId": "independent-delete"})
	c := w.client(t)
	body(t, w.signIn(t, c, "ann", "pw"))
	call := func(c *http.Client, method, args string) map[string]any {
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
	deleted := func(coll, id string) bool {
		var d bson.M
		_ = w.db.Collection(coll).FindOne(ctx, bson.M{"_id": id}).Decode(&d)
		return d["deletedAt"] != nil
	}
	shown := func() bool {
		_, html := get(t, c, w.srv.URL+"/b/"+rb.ID+"/"+board.Slug)
		return strings.Contains(html, "js-list-"+rb.List)
	}
	if !shown() {
		t.Fatal("the list is on the board")
	}
	if out := call(c, "lists.softRemove", `["`+rb.List+`"]`); out["error"] != nil {
		t.Fatalf("softRemove: %v", out)
	}
	if !deleted("lists", rb.List) || !deleted("cards", rb.Card) || shown() {
		t.Fatal("the list and its card are deleted and gone from the board")
	}
	if out := call(c, "changeHistory.undoLast", `["`+rb.ID+`"]`); canonical(out["result"].(map[string]any)["undone"]) != "true" {
		t.Fatalf("undo: %v", out)
	}
	if deleted("lists", rb.List) || deleted("cards", rb.Card) || !shown() {
		t.Error("undo restores the list and its card")
	}
	var sep bson.M
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": separate}).Decode(&sep)
	if sep["deletedAt"] == nil || sep["deleteBatchId"] != "independent-delete" {
		t.Error("negative: the card deleted on its own stays deleted, its batch its own")
	}
	if out := call(c, "changeHistory.redoLast", `["`+rb.ID+`"]`); canonical(out["result"].(map[string]any)["redone"]) != "true" {
		t.Fatalf("redo: %v", out)
	}
	if !deleted("lists", rb.List) || !deleted("cards", rb.Card) {
		t.Error("redo deletes them again")
	}
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": separate}).Decode(&sep)
	if sep["deleteBatchId"] != "independent-delete" {
		t.Error("negative: redo leaves the other card's batch alone")
	}
	call(c, "changeHistory.undoLast", `["`+rb.ID+`"]`)
	if deleted("cards", rb.Card) || !deleted("cards", separate) {
		t.Error("undo again: only the list's batch back")
	}
	if f := verifyWithWeKan(t, historyRows(t, w, rb.ID)); len(f) != 0 {
		t.Errorf("WeKan's audit: %v", f)
	}

	// A read-only member cannot undo.
	call(c, "lists.softRemove", `["`+rb.List+`"]`)
	setMember(t, w, rb.ID, Member{UserID: annU.ID, IsActive: true, IsReadOnly: true})
	if out := call(c, "changeHistory.undoLast", `["`+rb.ID+`"]`); out["error"] != "not-authorized" || !deleted("lists", rb.List) {
		t.Errorf("negative: a read-only member: %v", out)
	}
	setMember(t, w, rb.ID, Member{UserID: annU.ID, IsActive: true, IsAdmin: true})

	// A tampered row is refused, and seen in Problems.
	var last bson.M
	_ = w.db.Collection("changeHistory").FindOne(ctx, bson.M{"boardId": rb.ID, "undone": false, "isCheckpoint": false},
		nil).Decode(&last)
	cur, _ := w.db.Collection("changeHistory").Find(ctx, bson.M{"boardId": rb.ID, "undone": false, "isCheckpoint": false})
	var rows []bson.M
	_ = cur.All(ctx, &rows)
	newest := rows[len(rows)-1]
	_, _ = w.db.Collection("changeHistory").UpdateOne(ctx, bson.M{"_id": newest["_id"]}, bson.M{"$set": bson.M{"newContent.deletedAt": time.Unix(0, 0)}})
	if out := call(c, "changeHistory.undoLast", `["`+rb.ID+`"]`); out["error"] != "history-integrity-failed" || !deleted("lists", rb.List) {
		t.Errorf("negative: a tampered row: %v", out)
	}
	if n, _ := w.db.Collection("eventlog").CountDocuments(ctx, bson.M{"bleed": "HistoryIntegrity"}); n == 0 {
		t.Error("HistoryIntegrity is recorded")
	}
	var d struct {
		LoginDisabled bool `bson:"loginDisabled"`
	}
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"_id": annU.ID}).Decode(&d)
	if d.LoginDisabled {
		t.Error("negative: a tampered row is not the caller's attempt")
	}
	_, _ = w.db.Collection("changeHistory").UpdateOne(ctx, bson.M{"_id": newest["_id"]}, bson.M{"$set": bson.M{"newContent.deletedAt": plain(newest["newContent"]).(map[string]any)["deletedAt"]}})
	_ = last

	// Two undos at once: one applies, one finds nothing or a conflict.
	var wg sync.WaitGroup
	results := make([]string, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = canonical(call(c, "changeHistory.undoLast", `["`+rb.ID+`"]`)["result"])
		}(i)
	}
	wg.Wait()
	applied := 0
	for _, r := range results {
		if strings.Contains(r, `"undone":true`) {
			applied++
		}
	}
	if applied > 1 {
		t.Errorf("negative: the same row applied twice: %v", results)
	}
	n, _ := w.db.Collection("changeHistory").CountDocuments(ctx, bson.M{"boardId": rb.ID, "changeType": "restored"})
	if n < 1 {
		t.Error("each reversal is recorded as a checkpoint")
	}
}
