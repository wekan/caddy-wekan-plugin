package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestHistoryPage(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	w.seedUser(t, "eve", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "History")
	private := newRESTBoard(t, w, login(t, w, "eve"), "Private")
	var ann User
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"username": "ann"}).Decode(&ann)
	setMember(t, w, rb.ID, Member{UserID: bob.ID, IsActive: true})
	second, _ := w.s.AddCard(ctx, &Board{ID: rb.ID}, &ann, rb.List, rb.Swimlane, "Second")
	for i, title := range []string{"One", "Two", "Three apple"} {
		_ = w.s.updateRecorded(ctx, ann.ID, "card", rb.Card, bson.M{"$set": bson.M{"title": title}})
		_ = i
	}
	_ = w.s.updateRecorded(ctx, bob.ID, "card", second.ID, bson.M{"$set": bson.M{"title": "Bob's"}})
	secret := "fsecret" + randomID()[:8]
	_, _ = w.db.Collection("customFields").InsertOne(ctx, bson.M{"_id": secret, "boardIds": bson.A{rb.ID}, "name": "S", "type": "text", "adminOnly": true})
	_ = w.s.updateRecorded(ctx, ann.ID, "card", rb.Card, bson.M{"$set": bson.M{"customFields": bson.A{bson.M{"_id": secret, "value": "CLASSIFIED-9999"}}}})
	var eveU User
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"username": "eve"}).Decode(&eveU)
	_ = w.s.updateRecorded(ctx, eveU.ID, "card", private.Card, bson.M{"$set": bson.M{"title": "PRIVATE-TITLE"}})

	caller := func(name string) func(args string) map[string]any {
		c := w.client(t)
		body(t, w.signIn(t, c, name, "pw"))
		return func(args string) map[string]any {
			req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/"+url.PathEscape("changeHistory.page"), strings.NewReader(args))
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
	annPage, bobPage := caller("ann"), caller("bob")
	result := func(out map[string]any) map[string]any { r, _ := out["result"].(map[string]any); return r }
	all := result(annPage(`[{"scope":"board","scopeId":"` + rb.ID + `"}]`))
	if all == nil || all["total"] != 5.0 {
		t.Fatalf("the board's rows: %v", all)
	}
	if first := all["rows"].([]any)[0].(map[string]any); first["entityId"] != rb.Card || first["group"] != "customFields" {
		t.Errorf("newest first: %v", first["group"])
	}
	if r := result(annPage(`[{"scope":"card","scopeId":"` + second.ID + `"}]`)); r["total"] != 1.0 {
		t.Errorf("the card's rows: %v", r["total"])
	}
	if r := result(annPage(`[{"scope":"board","scopeId":"` + rb.ID + `","search":"APPLE"}]`)); r["total"] != 1.0 {
		t.Errorf("searched: %v", r["total"])
	}
	if r := result(annPage(`[{"scope":"board","scopeId":"` + rb.ID + `","pageSize":2,"page":3}]`)); r["page"] != 3.0 || len(r["rows"].([]any)) != 1 {
		t.Errorf("the third page of two: %v", r)
	}
	if r := result(annPage(`[{"scope":"board","scopeId":"` + rb.ID + `","pageSize":2,"page":99}]`)); r["page"] != 3.0 {
		t.Errorf("a page past the end is the last: %v", r["page"])
	}
	if c := all["contributors"].([]any); len(c) != 2 || c[0].(map[string]any)["count"] != 4.0 {
		t.Errorf("contributors: %v", c)
	}
	// Admin-only values: the admin reads them, an ordinary member does not.
	if !strings.Contains(canonical(all), "CLASSIFIED-9999") {
		t.Error("the board's admin reads the value")
	}
	if strings.Contains(canonical(result(bobPage(`[{"scope":"board","scopeId":"`+rb.ID+`"}]`))), "CLASSIFIED-9999") {
		t.Error("negative: a member reads an admin-only value in History")
	}
	// Negative: a board not seen says nothing.
	if out := annPage(`[{"scope":"board","scopeId":"` + private.ID + `"}]`); out["error"] != "not-authorized" {
		t.Errorf("negative: a board not seen: %v", out)
	}
	if out := annPage(`[{"scope":"card","scopeId":"` + private.Card + `"}]`); out["error"] != "not-authorized" {
		t.Errorf("negative: a card of it: %v", out)
	}
	if r := result(annPage(`[{"userId":"` + eveU.ID + `"}]`)); r["total"] != 0.0 || strings.Contains(canonical(r), "PRIVATE-TITLE") {
		t.Errorf("negative: another member's changes on a board not seen: %v", r)
	}
	if out := annPage(`[{}]`); out["error"] != "bad-scope" {
		t.Errorf("negative: no scope and no user: %v", out)
	}
	// An assigned-only member reads only their own cards' rows.
	setMember(t, w, rb.ID, Member{UserID: bob.ID, IsActive: true, IsNormalAssignedOnly: true})
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": second.ID}, bson.M{"$set": bson.M{"assignees": bson.A{bob.ID}}})
	if r := result(bobPage(`[{"scope":"board","scopeId":"` + rb.ID + `"}]`)); r["total"] != 1.0 {
		t.Errorf("an assigned-only member's rows: %v", r["total"])
	}
}

// changeHistory.restore: a row's value made current, recorded for its author
// and the restorer; refused for a row the caller may not read or a card
// they may not edit now (negative).
func TestHistoryRestore(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	w.seedUser(t, "eve", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Restore")
	foreign := newRESTBoard(t, w, login(t, w, "eve"), "Foreign")
	var ann User
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"username": "ann"}).Decode(&ann)
	setMember(t, w, rb.ID, Member{UserID: bob.ID, IsActive: true, IsNormalAssignedOnly: true})
	row := func(value string) string {
		id := "hist" + randomID()[:12]
		_, _ = w.db.Collection("changeHistory").InsertOne(ctx, bson.M{"_id": id, "boardId": rb.ID, "entityType": "card", "entityId": rb.Card, "cardId": rb.Card,
			"group": "description", "changeType": "edited", "userId": ann.ID, "createdAt": time.Now().UTC(), "newContent": bson.M{"field": "description", "value": value}})
		return id
	}
	desc := func() string {
		var d bson.M
		_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": rb.Card}).Decode(&d)
		return docString(d, "description")
	}
	caller := func(name string) func(args string) map[string]any {
		c := w.client(t)
		body(t, w.signIn(t, c, name, "pw"))
		return func(args string) map[string]any {
			req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/changeHistory.restore", strings.NewReader(args))
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
	annR, bobR := caller("ann"), caller("bob")
	// Negative: an assigned-only member guessing a hidden card's row.
	hidden := row("Hidden historical description")
	if out := bobR(`[["` + hidden + `"]]`); out["error"] != "not-authorized" || desc() == "Hidden historical description" {
		t.Errorf("negative: a hidden card's row: %v", out)
	}
	// ...and their own card's row restores.
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": rb.Card}, bson.M{"$set": bson.M{"assignees": bson.A{bob.ID}}})
	allowed := row("Allowed history restore")
	if out := bobR(`[["` + allowed + `"]]`); canonical(out["result"]) != `{"restored":1,"skipped":0}` || desc() != "Allowed history restore" {
		t.Errorf("their own card's row: %v", out)
	}
	n, _ := w.db.Collection("changeHistory").CountDocuments(ctx, bson.M{"restoredFromId": allowed, "changeType": "restored"})
	if n != 2 {
		t.Errorf("recorded for the author and the restorer: %d", n)
	}
	// Negative: a card moved since to a board the restorer cannot reach.
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": rb.Card}, bson.M{"$set": bson.M{"boardId": foreign.ID, "listId": foreign.List, "swimlaneId": foreign.Swimlane}})
	moved := row("Unauthorized moved-card edit")
	if out := annR(`[["` + moved + `"]]`); out["error"] != "not-authorized" || desc() == "Unauthorized moved-card edit" {
		t.Errorf("negative: a card moved away: %v", out)
	}
	// Nothing to apply is skipped, not an error.
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": rb.Card}, bson.M{"$set": bson.M{"boardId": rb.ID, "listId": rb.List, "swimlaneId": rb.Swimlane}})
	odd := "hist" + randomID()[:12]
	_, _ = w.db.Collection("changeHistory").InsertOne(ctx, bson.M{"_id": odd, "boardId": rb.ID, "entityType": "card", "entityId": rb.Card, "cardId": rb.Card,
		"group": "title", "changeType": "edited", "userId": ann.ID, "createdAt": time.Now().UTC(), "newContent": bson.M{"nofield": true}})
	if out := annR(`["` + odd + `"]`); canonical(out["result"]) != `{"restored":0,"skipped":1}` {
		t.Errorf("not applicable: %v", out)
	}
}
