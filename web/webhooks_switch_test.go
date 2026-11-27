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

// Turn off all webhooks (WeKan's models/lib/webhooksSwitch.js): while it is
// on nothing is sent or queued - an activity's webhook, a direct delivery - a
// waiting job is kept untouched, and no webhook is added or changed through
// REST or Meteor, each such attempt in Admin Panel -> Problems; a delete is
// still allowed. Turned off again, webhooks are sent. Negative: a member who
// may not write the webhook is refused as before, and nothing is recorded.
func TestWebhooksSwitch(t *testing.T) {
	sink := newWebhookSink(t)
	webhookAllowLoopbackForTests = true
	t.Cleanup(func() { webhookAllowLoopbackForTests = false })
	realLookup := lookupHost
	t.Cleanup(func() { lookupHost = realLookup })
	lookupHost = func(_ context.Context, host string) ([]string, error) { return []string{"93.184.216.34"}, nil }
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	ctx := context.Background()
	at, bt := login(t, w, "ann"), login(t, w, "bob")
	rb := newRESTBoard(t, w, at, "Switch")
	setMember(t, w, rb.ID, Member{UserID: bob.ID, IsActive: true})
	hook := bson.M{"_id": "swhook1", "boardId": rb.ID, "url": sink.srv.URL + "/all", "type": "outgoing-webhooks",
		"enabled": true, "activities": bson.A{"all"}, "token": "tok"}
	if _, err := w.db.Collection("integrations").InsertOne(ctx, hook); err != nil {
		t.Fatal(err)
	}
	setOff := func(off bool) {
		_, _ = w.db.Collection("settings").UpdateOne(ctx, bson.M{}, bson.M{"$set": bson.M{"webhooksDisabled": off}})
	}
	if w.db.Collection("settings").FindOne(ctx, bson.M{}).Err() != nil {
		_, _ = w.db.Collection("settings").InsertOne(ctx, bson.M{"_id": "switchsettings"})
	}
	rest := func(tok, method, path, payload string) (int, string) {
		req, _ := http.NewRequest(method, w.srv.URL+path, strings.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		res, err := w.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res.StatusCode, body(t, res)
	}
	call := func(user, method, args string) map[string]any {
		c := w.client(t)
		body(t, w.signIn(t, c, user, "pw"))
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
	recorded := func() int64 {
		n, _ := w.db.Collection("eventlog").CountDocuments(ctx, bson.M{"bleed": "Webhooks turned off"})
		return n
	}
	jobs := func() int64 {
		n, _ := w.db.Collection("notificationWebhookJobs").CountDocuments(ctx, bson.M{})
		return n
	}

	setOff(true)
	if !w.s.webhooksOff(ctx) {
		t.Fatal("the switch reads the site setting")
	}
	if err := w.s.deliverWebhook(ctx, hook, []byte(`{"text":"x"}`)); err != errWebhooksOff {
		t.Errorf("a direct delivery is not made: %v", err)
	}
	before := jobs()
	if err := w.s.enqueueWebhook(ctx, "swhook1", "ev1", "", map[string]any{"text": "x"}, time.Now()); err != nil || jobs() != before {
		t.Errorf("nothing is queued: %v, %d jobs", err, jobs())
	}
	w.s.sendActivityWebhooks(bson.M{"_id": "act1", "boardId": rb.ID, "activityType": "createCard", "userId": bob.ID})
	// A job that was waiting before the switch: kept, untouched.
	_, _ = w.db.Collection("notificationWebhookJobs").InsertOne(ctx, bson.M{"_id": "waiting1", "integrationId": "swhook1", "eventId": "old",
		"groupKey": "", "body": bson.M{"text": "old"}, "state": "pending", "attempts": 0, "createdAt": time.Now().UTC(), "nextAttemptAt": time.Now().UTC().Add(-time.Minute)})
	w.s.drainWebhookQueue(ctx)
	time.Sleep(300 * time.Millisecond)
	if got := sink.bodies("/all"); len(got) != 0 {
		t.Errorf("nothing is sent while webhooks are off: %v", got)
	}
	var job bson.M
	_ = w.db.Collection("notificationWebhookJobs").FindOne(ctx, bson.M{"_id": "waiting1"}).Decode(&job)
	if job["state"] != "pending" || job["attempts"] != int32(0) && job["attempts"] != int64(0) {
		t.Errorf("the waiting job is kept untouched: %v", job)
	}

	// Writes: refused for the board's admin, and recorded.
	base := "/api/boards/" + rb.ID + "/integrations"
	for _, c := range []struct{ method, path, payload string }{
		{"POST", base, `{"url":"https://hooks.example/new"}`},
		{"PUT", base + "/swhook1", `{"title":"Renamed"}`},
		{"POST", base + "/swhook1/activities", `{"activities":["createCard"]}`},
		{"DELETE", base + "/swhook1/activities", `{"activities":["all"]}`},
	} {
		if code, out := rest(at, c.method, c.path, c.payload); code != http.StatusForbidden || !strings.Contains(out, "webhooks-disabled") {
			t.Errorf("%s %s while off: %d %s", c.method, c.path, code, out)
		}
	}
	if out := call("ann", "/integrations/insert", `[{"boardId":"`+rb.ID+`","url":"https://hooks.example/ddp"}]`); out["error"] != "webhooks-disabled" {
		t.Errorf("Meteor's insert while off: %v", out)
	}
	if out := call("ann", "/integrations/update", `[{"_id":"swhook1"},{"$set":{"title":"Renamed"}}]`); out["error"] != "webhooks-disabled" {
		t.Errorf("Meteor's update while off: %v", out)
	}
	var stored bson.M
	_ = w.db.Collection("integrations").FindOne(ctx, bson.M{"_id": "swhook1"}).Decode(&stored)
	if stored["title"] == "Renamed" {
		t.Error("the webhook was not changed")
	}
	if n, _ := w.db.Collection("integrations").CountDocuments(ctx, bson.M{"boardId": rb.ID}); n != 1 {
		t.Errorf("no webhook was added: %d", n)
	}
	if recorded() < 6 {
		t.Errorf("each refused write is in Admin Panel -> Problems: %d", recorded())
	}
	// Negative: a member who may not write webhooks is refused as before,
	// and that is not this record.
	was := recorded()
	if code, _ := rest(bt, "POST", base, `{"url":"https://hooks.example/bob"}`); code != http.StatusForbidden {
		t.Errorf("a normal member: %d", code)
	}
	if recorded() != was {
		t.Error("negative: a member who could not write it anyway is not recorded")
	}

	// Turned on again: the waiting job and new deliveries go out.
	setOff(false)
	if err := w.s.deliverWebhook(ctx, hook, []byte(`{"text":"back"}`)); err != nil {
		t.Errorf("delivered again: %v", err)
	}
	if got := sink.bodies("/all"); len(got) == 0 {
		t.Error("the delivery reached the webhook")
	}
	w.s.drainWebhookQueue(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_ = w.db.Collection("notificationWebhookJobs").FindOne(ctx, bson.M{"_id": "waiting1"}).Decode(&job)
		if job["state"] != "pending" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if job["state"] == "pending" {
		t.Errorf("the kept job is sent once webhooks are on: %v", job)
	}
	// A delete is the cleanup, and always allowed.
	setOff(true)
	if code, out := rest(at, "DELETE", base+"/swhook1", ""); code != http.StatusOK {
		t.Errorf("a delete while off: %d %s", code, out)
	}
}
