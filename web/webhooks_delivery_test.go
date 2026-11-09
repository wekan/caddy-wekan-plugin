package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// #3695: what a webhook carries is its own setting, then its board's (for
// the board's own webhooks), then Admin Panel's.
func TestWebhookPayloadSettings(t *testing.T) {
	w := newTestWeKan(t)
	ann := w.seedUser(t, "ann", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, ann, "Settings")
	p := map[string]any{"activityId": "act1", "cardId": "card1", "card": "Title", "board": "Settings", "url": "https://x/b/1",
		"cardUrl": "https://x/b/1/c", "boardUrl": "https://x/b/1", "user": "Ann", "username": "ann", "userId": ann.ID}
	own := bson.M{"_id": "h", "boardId": b.ID, "notificationDelivery": bson.M{"webhook": bson.M{"text": false, "fields": bson.A{"ids", "links"}}}}
	body := webhookPayload(w.s.webhookDeliveryFor(ctx, own), "act-createCard", "Ann created", p)
	if _, has := body["text"]; has {
		t.Error("negative: text off is no text")
	}
	for _, k := range []string{"activityId", "cardId", "url", "cardUrl", "boardUrl", "description"} {
		if _, has := body[k]; !has {
			t.Errorf("the ids and links groups have %s", k)
		}
	}
	for _, k := range []string{"card", "board", "user", "username"} {
		if _, has := body[k]; has {
			t.Errorf("negative: %s is in neither group asked for", k)
		}
	}
	// The board's setting for its own webhook, never for a global one.
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"notificationDelivery.webhook": bson.M{"grouping": "card", "text": false}}})
	if d := w.s.webhookDeliveryFor(ctx, bson.M{"_id": "h2", "boardId": b.ID}); d.Grouping != "card" || d.Text {
		t.Errorf("the board's setting: %+v", d)
	}
	if d := w.s.webhookDeliveryFor(ctx, bson.M{"_id": "h3", "boardId": "_global"}); d.Grouping != "none" || !d.Text {
		t.Errorf("negative: a global webhook keeps the defaults: %+v", d)
	}
	// Admin Panel's, under both; an invalid value is skipped.
	_, _ = w.db.Collection("settings").UpdateOne(ctx, bson.M{}, bson.M{"$set": bson.M{"notificationDelivery.webhook": bson.M{"schedule": "daily", "dailyTime": "07:30", "grouping": "nonsense"}}},
		optionsUpsert())
	if d := w.s.webhookDeliveryFor(ctx, bson.M{"_id": "h4", "boardId": "_global"}); d.Schedule != "daily" || d.DailyTime != "07:30" || d.Grouping != "none" {
		t.Errorf("Admin Panel's, an invalid grouping skipped: %+v", d)
	}
}

// #3695: a grouped webhook is queued and sent as one act-batch when it is
// due; a failure is retried with a growing wait and given up after ten; a
// webhook turned off has its jobs cancelled.
func TestWebhookQueue(t *testing.T) {
	var posts atomic.Int32
	var status atomic.Int32
	status.Store(200)
	var last atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		var m map[string]any
		_ = jsonDecode(r, &m)
		last.Store(m)
		rw.WriteHeader(int(status.Load()))
	}))
	t.Cleanup(srv.Close)
	webhookAllowLoopbackForTests = true
	t.Cleanup(func() { webhookAllowLoopbackForTests = false })
	w := newTestWeKan(t)
	ann := w.seedUser(t, "ann", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, ann, "Grouped")
	_, _ = w.db.Collection("integrations").InsertOne(ctx, bson.M{"_id": "hq", "boardId": b.ID, "url": srv.URL + "/q", "type": "outgoing-webhooks",
		"enabled": true, "activities": bson.A{"act-createCard"}, "userId": ann.ID, "notificationDelivery": bson.M{"webhook": bson.M{"grouping": "board"}}})
	l, _ := w.s.AddList(ctx, b, "Todo")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	w.s.AddCard(ctx, b, ann, l.ID, sw.ID, "One")
	w.s.AddCard(ctx, b, ann, l.ID, sw.ID, "Two")
	jobs := w.db.Collection("notificationWebhookJobs")
	// Webhooks are queued in the background: wait for each one.
	queued := func(want int64) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if n, _ := jobs.CountDocuments(ctx, bson.M{"integrationId": "hq"}); n >= want {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("%d jobs queued", want)
	}
	queued(2)
	if posts.Load() != 0 {
		t.Fatalf("negative: a grouped webhook is not sent at once: %d", posts.Load())
	}
	due := func() {
		_, _ = jobs.UpdateMany(ctx, bson.M{"integrationId": "hq", "state": "pending"}, bson.M{"$set": bson.M{"nextAttemptAt": time.Now().UTC().Add(-time.Second)}})
	}
	due()
	w.s.drainWebhookQueue(ctx)
	if posts.Load() != 1 {
		t.Fatalf("one POST for the group: %d", posts.Load())
	}
	m, _ := last.Load().(map[string]any)
	if m["description"] != "act-batch" || m["count"] != float64(2) {
		t.Errorf("an act-batch of two: %v", m)
	}
	if n, _ := jobs.CountDocuments(ctx, bson.M{"integrationId": "hq", "state": "sent", "body": bson.M{"$exists": false}}); n != 2 {
		t.Errorf("both sent, their bodies gone: %d", n)
	}
	// A failure is retried later, then given up.
	status.Store(500)
	w.s.AddCard(ctx, b, ann, l.ID, sw.ID, "Three")
	queued(3)
	for i := 0; i < 10; i++ {
		due()
		w.s.drainWebhookQueue(ctx)
		var j bson.M
		_ = jobs.FindOne(ctx, bson.M{"integrationId": "hq", "eventId": bson.M{"$exists": true}, "state": bson.M{"$in": bson.A{"pending", "failed"}}}).Decode(&j)
		if i == 0 {
			next, _ := plain(j["nextAttemptAt"]).(time.Time)
			if j["state"] != "pending" || intOf(j["attempts"]) != 1 || !next.After(time.Now().Add(20*time.Second)) {
				t.Errorf("a failure waits and tries again: %v", j)
			}
		}
	}
	if n, _ := jobs.CountDocuments(ctx, bson.M{"integrationId": "hq", "state": "failed", "attempts": 10}); n != 1 {
		t.Error("given up after ten")
	}
	// Turned off: cancelled.
	status.Store(200)
	w.s.AddCard(ctx, b, ann, l.ID, sw.ID, "Four")
	queued(4)
	_, _ = w.db.Collection("integrations").UpdateOne(ctx, bson.M{"_id": "hq"}, bson.M{"$set": bson.M{"enabled": false}})
	before := posts.Load()
	due()
	w.s.drainWebhookQueue(ctx)
	if posts.Load() != before {
		t.Error("negative: a webhook turned off is not sent to")
	}
	if n, _ := jobs.CountDocuments(ctx, bson.M{"integrationId": "hq", "state": "cancelled"}); n != 1 {
		t.Errorf("its job is cancelled: %d", n)
	}
}

func optionsUpsert() *options.UpdateOneOptionsBuilder { return options.UpdateOne().SetUpsert(true) }

func jsonDecode(r *http.Request, v any) error { return json.NewDecoder(r.Body).Decode(v) }
