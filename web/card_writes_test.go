package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Meteor's /cards/update: a board member's title, description and color of
// one card - WeKan's activities and rules after - and nothing else.
func TestCardsUpdateCollectionWrite(t *testing.T) {
	w := newTestWeKan(t)
	ann := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	w.seedUser(t, "eve", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, ann, "Writes")
	list, _ := w.s.AddList(ctx, b, "L")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"_id": bson.M{"$exists": true}, "boardId": b.ID}).Decode(&sw)
	c, _ := w.s.AddCard(ctx, b, ann, list.ID, sw.ID, "Old")
	setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: true, IsCommentOnly: true})
	addRule(t, w, b.ID, "rtitle", bson.M{"activityType": "textContainsTrigger", "textContains": "urgent"}, bson.M{"actionType": "markCardComplete"}, true)
	as := func(name string) func(args string) map[string]any {
		cl := w.client(t)
		body(t, w.signIn(t, cl, name, "pw"))
		return func(args string) map[string]any {
			req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/%2Fcards%2Fupdate", strings.NewReader(args))
			req.Header.Set("Content-Type", "application/json")
			res, err := cl.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			var out map[string]any
			_ = json.NewDecoder(res.Body).Decode(&out)
			return out
		}
	}
	asAnn, asBob, asEve := as("ann"), as("bob"), as("eve")
	card := func() bson.M {
		var r bson.M
		_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": c.ID}).Decode(&r)
		return r
	}
	if out := asAnn(`[{"_id":"` + c.ID + `"},{"$set":{"title":"Urgent fix","color":"#a1b2c3"}}]`); out["result"] != 1.0 {
		t.Fatalf("a member's own write: %v", out)
	}
	if r := card(); r["title"] != "Urgent fix" || r["color"] != "#a1b2c3" || r["dueComplete"] != true {
		t.Errorf("written, and the rule ran: %v", r)
	}
	var act bson.M
	_ = w.db.Collection("activities").FindOne(ctx, bson.M{"activityType": "a-changedTitle", "cardId": c.ID}).Decode(&act)
	if act["oldValue"] != "Old" || act["value"] != "Urgent fix" {
		t.Errorf("WeKan's a-changedTitle: %v", act)
	}
	if out := asAnn(`["` + c.ID + `",{"$unset":{"description":""}}]`); out["result"] != 1.0 {
		t.Errorf("an id as the selector, and $unset: %v", out)
	}
	if out := asAnn(`["nosuchcard000001",{"$set":{"title":"x"}}]`); out["result"] != 0.0 {
		t.Errorf("no such card is none updated: %v", out)
	}
	// Negative: nobody without write access, and nothing but these fields.
	for who, call := range map[string]func(string) map[string]any{"a comment-only member": asBob, "a stranger": asEve} {
		if out := call(`[{"_id":"` + c.ID + `"},{"$set":{"title":"pwned"}}]`); out["error"] != 403.0 {
			t.Errorf("negative: %s: %v", who, out)
		}
	}
	for _, mod := range []string{`{"$set":{"boardId":"other0000000001"}}`, `{"$set":{"vote":{}}}`, `{"$set":{"customFields":[]}}`, `{"$set":{"parentId":"x"}}`,
		`{"$push":{"members":"x"}}`, `{"$set":{"title":"ok","sort":1}}`, `{}`} {
		if out := asAnn(`[{"_id":"` + c.ID + `"},` + mod + `]`); out["error"] != 403.0 {
			t.Errorf("negative: %s: %v", mod, out)
		}
	}
	for _, sel := range []string{`{"boardId":"` + b.ID + `"}`, `{"_id":{"$ne":""}}`, `{"_id":"` + c.ID + `","title":"x"}`} {
		if out := asAnn(`[` + sel + `,{"$set":{"title":"many"}}]`); out["error"] != 403.0 {
			t.Errorf("negative: a selector other than one id: %s %v", sel, out)
		}
	}
	for _, bad := range []string{`{"$set":{"color":"red;background:url(x)"}}`, `{"$set":{"color":"#abc"}}`, `{"$set":{"title":"  "}}`, `{"$unset":{"title":""}}`, `{"$set":{"title":5}}`, `{"$set":{"archived":"yes"}}`, `{"$unset":{"archived":""}}`} {
		if out := asAnn(`[{"_id":"` + c.ID + `"},` + bad + `]`); out["error"] != 400.0 {
			t.Errorf("negative: %s: %v", bad, out)
		}
	}
	if r := card(); r["title"] != "Urgent fix" || r["archived"] == true || r["sort"] == 1.0 {
		t.Errorf("negative: nothing refused was written: %v", r)
	}
	// Archived once: one activity, and none for the same again.
	count := func(kind string) int64 {
		n, _ := w.db.Collection("activities").CountDocuments(ctx, bson.M{"activityType": kind, "cardId": c.ID})
		return n
	}
	for i := 0; i < 2; i++ {
		asAnn(`[{"_id":"` + c.ID + `"},{"$set":{"archived":true}}]`)
	}
	if r := card(); r["archived"] != true || r["archivedAt"] == nil || count("archivedCard") != 1 {
		t.Errorf("archived once: %v %d", r["archived"], count("archivedCard"))
	}
	asAnn(`[{"_id":"` + c.ID + `"},{"$set":{"archived":false}}]`)
	if card()["archived"] != false || count("restoredCard") != 1 {
		t.Error("restored")
	}
}
