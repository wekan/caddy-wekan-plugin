package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func TestArchiveHistoryReplay(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	board := newRESTBoard(t, w, login(t, w, "ann"), "Archive history")
	for _, item := range []struct{ entity, id string }{
		{"card", board.Card}, {"list", board.List}, {"swimlane", board.Swimlane},
	} {
		t.Run(item.entity, func(t *testing.T) {
			coll := w.db.Collection(historyCollections[item.entity])
			row := map[string]any{"entityType": item.entity, "entityId": item.id, "boardId": board.ID, "group": "lifecycle",
				"previousContent": map[string]any{"field": "archived", "value": false},
				"newContent":      map[string]any{"field": "archived", "value": true}}
			check := func(archived bool) {
				t.Helper()
				var doc bson.M
				if err := coll.FindOne(ctx, bson.M{"_id": item.id}).Decode(&doc); err != nil ||
					doc["archived"] != archived || (doc["archivedAt"] != nil) != archived {
					t.Fatalf("archive status %v and timestamp: %+v, %v", archived, doc, err)
				}
			}
			if w.s.applyHistoryRow(ctx, row, "undo") {
				t.Fatal("undo accepted a stale archive state")
			}
			if !w.s.applyHistoryRow(ctx, row, "redo") {
				t.Fatal("redo did not archive")
			}
			check(true)
			if !w.s.applyHistoryRow(ctx, row, "undo") {
				t.Fatal("undo did not unarchive")
			}
			check(false)
			if !w.s.applyHistoryRow(ctx, row, "restore") {
				t.Fatal("restore did not archive")
			}
			check(true)
		})
	}
}

func TestTemplateListArchiveGroupedHistory(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	user := w.seedUser(t, "ann", "pw")
	token := login(t, w, "ann")
	board := newRESTBoard(t, w, token, "Template archive")
	if _, err := w.db.Collection("lists").UpdateOne(ctx, bson.M{"_id": board.List},
		bson.M{"$set": bson.M{"type": "template-list"}}); err != nil {
		t.Fatal(err)
	}
	base := w.srv.URL + "/api/boards/" + board.ID + "/lists/" + board.List
	assertState := func(archived bool) {
		t.Helper()
		for coll, id := range map[string]string{"lists": board.List, "cards": board.Card} {
			if n, err := w.db.Collection(coll).CountDocuments(ctx, bson.M{"_id": id, "archived": archived}); err != nil || n != 1 {
				t.Fatalf("%s archive state %v: %d, %v", coll, archived, n, err)
			}
		}
	}
	reverse := func(direction, key string) {
		t.Helper()
		out, err := w.s.reverseLast(httptest.NewRequest("POST", "/", nil), user, board.ID, direction, nil)
		if err != nil || out[key] != true || out["entityId"] != board.List {
			t.Fatalf("%s: %v %v", direction, out, err)
		}
	}
	for _, test := range []struct {
		path    string
		archive bool
	}{{"archive", true}, {"unarchive", false}} {
		if code := call(t, "POST", base+"/"+test.path, token, "", nil); code != 200 {
			t.Fatalf("%s: %d", test.path, code)
		}
		assertState(test.archive)
		reverse("undo", "undone")
		assertState(!test.archive)
		reverse("redo", "redone")
		assertState(test.archive)
	}
}

func TestRESTCustomFieldHistoryReplay(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	token := login(t, w, "ann")
	board := newRESTBoard(t, w, token, "Custom field history")
	base := w.srv.URL + "/api/boards/" + board.ID
	var field struct {
		ID string `json:"_id"`
	}
	call(t, "POST", base+"/custom-fields", token,
		`{"name":"Replay text","type":"text"}`, &field)
	if field.ID == "" {
		t.Fatal("missing custom field ID")
	}
	// The REST value route edits an existing card field. Seed its empty value.
	if _, err := w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": board.Card},
		bson.M{"$set": bson.M{"customFields": bson.A{bson.M{"_id": field.ID, "value": nil}}}}); err != nil {
		t.Fatal(err)
	}
	client := w.client(t)
	body(t, w.signIn(t, client, "ann", "pw"))
	replay := func(method string, args any, key string, want any) {
		t.Helper()
		payload, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		res, err := client.Post(w.srv.URL+"/_methods/changeHistory."+method,
			"application/json", strings.NewReader(string(payload)))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out struct {
			Result map[string]any
			Error  any
		}
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil ||
			res.StatusCode != http.StatusOK || out.Error != nil || out.Result[key] != want {
			t.Fatalf("%s: HTTP %d, %#v, %v", method, res.StatusCode, out, err)
		}
	}
	value := func(want any) {
		t.Helper()
		var stored struct {
			Fields []struct {
				ID    string `bson:"_id"`
				Value any
			} `bson:"customFields"`
		}
		if err := w.db.Collection("cards").FindOne(ctx, bson.M{"_id": board.Card}).Decode(&stored); err != nil {
			t.Fatal(err)
		}
		for _, f := range stored.Fields {
			if f.ID == field.ID && f.Value == want {
				return
			}
		}
		t.Fatalf("custom fields %+v, wanted %q = %#v", stored.Fields, field.ID, want)
	}
	endpoint := base + "/lists/" + board.List + "/cards/" + board.Card + "/customFields/" + field.ID
	const first = "First line\n日本語"
	previous := any(nil)
	args := []string{board.ID}
	value(previous)
	for _, text := range []string{first, "Second value"} {
		payload, err := json.Marshal(map[string]string{"value": text})
		if err != nil {
			t.Fatal(err)
		}
		var reply map[string]any
		call(t, "POST", endpoint, token, string(payload), &reply)
		value(text)
		replay("undoLast", args, "undone", true)
		value(previous)
		replay("redoLast", args, "redone", true)
		value(text)
		previous = text
	}

	for _, row := range historyRows(t, w, board.ID) {
		content, _ := row["newContent"].(map[string]any)
		if row["group"] == "customFields" && strings.Contains(canonical(content), "First line") &&
			row["changeType"] == "edited" {
			replay("restore", [][]string{{docString(row, "_id")}}, "restored", float64(1))
			value(first)
			return
		}
	}
	t.Fatal("missing first custom-field edit in History")
}
