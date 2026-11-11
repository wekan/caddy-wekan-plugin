package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// /integrations/update and /customFields/update under WeKan's allow rules:
// a board admin edits a webhook, a writer a field, and nothing else gets
// through (negative).
func TestCollectionUpdates(t *testing.T) {
	realLookup := lookupHost
	t.Cleanup(func() { lookupHost = realLookup })
	lookupHost = func(_ context.Context, host string) ([]string, error) { return []string{"93.184.216.34"}, nil }
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Edits")
	setMember(t, w, rb.ID, Member{UserID: bob.ID, IsActive: true})
	caller := func(name string) func(method, args string) map[string]any {
		c := w.client(t)
		body(t, w.signIn(t, c, name, "pw"))
		return func(method, args string) map[string]any {
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
	}
	ann, bobCall := caller("ann"), caller("bob")
	_, _ = w.db.Collection("integrations").InsertOne(ctx, bson.M{"_id": "h1", "boardId": rb.ID, "url": "https://hooks.example/a",
		"type": "outgoing-webhooks", "enabled": true, "activities": bson.A{"all"}})
	_, _ = w.db.Collection("integrations").InsertOne(ctx, bson.M{"_id": "bot1", "boardId": rb.ID, "url": "https://bots.example/a",
		"bot": true, "enabled": true, "activities": bson.A{botActivity}})
	hook := func(id string) bson.M {
		var h bson.M
		_ = w.db.Collection("integrations").FindOne(ctx, bson.M{"_id": id}).Decode(&h)
		return h
	}
	if out := ann(`/integrations/update`, `[{"_id":"h1"},{"$set":{"title":"Chat","url":"https://hooks.example/b","enabled":false,"activities":["act-createCard"]}}]`); out["error"] != nil {
		t.Fatalf("a board admin edits their webhook: %v", out)
	}
	if h := hook("h1"); h["title"] != "Chat" || h["url"] != "https://hooks.example/b" || h["enabled"] != false || h["boardId"] != rb.ID {
		t.Errorf("the edit: %v", h)
	}
	for _, c := range []struct{ who, args string }{
		{"bob", `[{"_id":"h1"},{"$set":{"title":"Mine"}}]`},                   // a member, not an admin
		{"ann", `[{"_id":"h1"},{"$set":{"userId":"someone"}}]`},               // not a field a page edits
		{"ann", `[{"_id":"h1"},{"$set":{"title":"x"},"$unset":{"token":1}}]`}, // another operator
		{"ann", `[{"_id":"h1"},{"$set":{"enabled":"yes"}}]`},                  // not a boolean
		{"ann", `[{"_id":"bot1"},{"$set":{"activities":["all"]}}]`},           // a bot hears the dispatch only
		{"ann", `[{"_id":"nope"},{"$set":{"title":"x"}}]`},                    // no such webhook
	} {
		call := ann
		if c.who == "bob" {
			call = bobCall
		}
		if out := call("/integrations/update", c.args); out["error"] == nil {
			t.Errorf("negative: %s %s was accepted", c.who, c.args)
		}
	}
	lookupHost = func(_ context.Context, host string) ([]string, error) { return []string{"127.0.0.1"}, nil }
	if out := ann(`/integrations/update`, `[{"_id":"h1"},{"$set":{"url":"http://internal.example/"}}]`); out["error"] != "invalid-webhook-url" {
		t.Errorf("negative: a webhook onto this machine: %v", out)
	}
	if h := hook("h1"); h["title"] != "Chat" || h["url"] != "https://hooks.example/b" {
		t.Errorf("negative: a refused edit changed it: %v", h)
	}
	if acts, _ := hook("bot1")["activities"].(bson.A); len(acts) != 1 || acts[0] != botActivity {
		t.Errorf("negative: the bot's activities: %v", acts)
	}

	// Custom fields: any writer of its board edits it; an admin-only one, its boards' admins.
	_, _ = w.db.Collection("customFields").InsertOne(ctx, bson.M{"_id": "f1", "boardIds": bson.A{rb.ID}, "name": "Size", "type": "text"})
	_, _ = w.db.Collection("customFields").InsertOne(ctx, bson.M{"_id": "f2", "boardIds": bson.A{rb.ID}, "name": "Cost", "type": "number", "adminOnly": true})
	field := func(id string) bson.M {
		var f bson.M
		_ = w.db.Collection("customFields").FindOne(ctx, bson.M{"_id": id}).Decode(&f)
		return f
	}
	if out := bobCall("/customFields/update", `[{"_id":"f1"},{"$set":{"name":"Width","showOnCard":true}}]`); out["error"] != nil {
		t.Fatalf("a writer edits a field: %v", out)
	}
	if f := field("f1"); f["name"] != "Width" || f["showOnCard"] != true {
		t.Errorf("the field edit: %v", f)
	}
	before, _ := w.db.Collection("eventlog").CountDocuments(ctx, bson.M{"bleed": "AdminFieldBleed"})
	if out := bobCall("/customFields/update", `[{"_id":"f2"},{"$set":{"name":"Price"}}]`); out["error"] == nil {
		t.Error("negative: a member who is not an admin edited an admin-only field")
	}
	if after, _ := w.db.Collection("eventlog").CountDocuments(ctx, bson.M{"bleed": "AdminFieldBleed"}); after <= before {
		t.Error("AdminFieldBleed was not recorded")
	}
	if out := ann("/customFields/update", `[{"_id":"f2"},{"$set":{"name":"Price"}}]`); out["error"] != nil || field("f2")["name"] != "Price" {
		t.Errorf("a board admin edits an admin-only field: %v", out)
	}
	for _, args := range []string{
		`[{"_id":"f1"},{"$set":{"boardIds":["` + rb.ID + `"]}}]`, // never through this
		`[{"_id":"f1"},{"$set":{"type":"spreadsheet"}}]`,
		`[{"_id":"f1"},{"$set":{"name":""}}]`,
		`[{"_id":"f1"},{"$inc":{"sort":1}}]`,
	} {
		if out := ann("/customFields/update", args); out["error"] == nil {
			t.Errorf("negative: %s was accepted", args)
		}
	}
	setMember(t, w, rb.ID, Member{UserID: bob.ID, IsActive: true, IsReadOnly: true})
	if out := bobCall("/customFields/update", `[{"_id":"f1"},{"$set":{"name":"Mine"}}]`); out["error"] == nil || field("f1")["name"] != "Width" {
		t.Errorf("negative: a read-only member edited a field: %v", out)
	}
}
