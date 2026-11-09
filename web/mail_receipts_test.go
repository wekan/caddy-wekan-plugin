package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// WeKan's receipt retention: old finished jobs and requests become receipts
// in place; a replayed old cancel still answers what it did and keeps later
// mail; the same event is never queued again.
func TestEmailReceiptRetention(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	jobs, commands := w.db.Collection("notificationEmailJobs"), w.db.Collection("notificationEmailCommands")
	old := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Now().UTC()
	_, _ = jobs.InsertOne(ctx, bson.M{"_id": emailJobID("u1", "e-old"), "userId": "u1", "eventId": "e-old", "state": "sent", "finishedAt": old, "subject": "PRIVATE"})
	_, _ = jobs.InsertOne(ctx, bson.M{"_id": emailJobID("u1", "e-new"), "userId": "u1", "eventId": "e-new", "state": "sent", "finishedAt": recent, "subject": "KEPT"})
	_, _ = jobs.InsertOne(ctx, bson.M{"_id": emailJobID("u1", "e-wait"), "userId": "u1", "eventId": "e-wait", "state": "pending", "finishedAt": old})
	_, _ = jobs.InsertOne(ctx, bson.M{"_id": "not-wekans-shape", "userId": "u1", "eventId": "x", "state": "sent", "finishedAt": old})
	req := strings.Repeat("q", 32)
	_, _ = commands.InsertOne(ctx, bson.M{"_id": req, "userId": "u1", "action": "cancel", "actorId": "boss", "status": "completed", "finishedAt": old,
		"cutoff": old, "generation": 1})
	cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour)
	var a, b receiptCursor
	if n, err := w.s.compactEmailReceipts(ctx, true, cutoff, &a); err != nil || n != 1 {
		t.Fatalf("jobs: %d %v", n, err)
	}
	if n, err := w.s.compactEmailReceipts(ctx, false, cutoff, &b); err != nil || n != 1 {
		t.Fatalf("commands: %d %v", n, err)
	}
	get := func(coll, id string) bson.M {
		var r bson.M
		_ = w.db.Collection(coll).FindOne(ctx, bson.M{"_id": id}).Decode(&r)
		return r
	}
	if r := get("notificationEmailJobs", emailJobID("u1", "e-old")); len(r) != 3 || r["state"] != "sent" || intOf(r["compactReceiptVersion"]) != 1 {
		t.Errorf("an old job is a receipt: %v", r)
	}
	if r := get("notificationEmailJobs", emailJobID("u1", "e-new")); r["subject"] != "KEPT" {
		t.Errorf("negative: a recent one keeps its metadata: %v", r)
	}
	if r := get("notificationEmailJobs", emailJobID("u1", "e-wait")); r["compactReceiptVersion"] != nil {
		t.Errorf("negative: a pending one is never compacted: %v", r)
	}
	if r := get("notificationEmailJobs", "not-wekans-shape"); r["compactReceiptVersion"] != nil {
		t.Errorf("negative: another shape is never touched: %v", r)
	}
	r := plain(get("notificationEmailCommands", req)).(map[string]any)
	if !matchesEmailCommand(r, req, "u1", "cancel", "boss") || matchesEmailCommand(r, req, "u1", "cancel", "eve") {
		t.Errorf("the request is a receipt of who asked what: %v", r)
	}
	// The same event queued again is the receipt's, never a new job.
	if err := w.s.enqueueEmail(ctx, emailJob{UserID: "u1", EventID: "e-old", Subject: "S", HTML: "again", Language: "en"}); err != nil {
		t.Fatal(err)
	}
	if r := get("notificationEmailJobs", emailJobID("u1", "e-old")); r["html"] != nil {
		t.Errorf("negative: not queued again: %v", r)
	}
	// A replayed old cancel answers as before and cancels nothing queued since.
	_, _ = w.db.Collection("notificationEmailControls").InsertOne(ctx, bson.M{"_id": "u1", "generation": 1, "lastRequestId": req, "cancelCount": 1})
	_, _ = jobs.InsertOne(ctx, bson.M{"_id": emailJobID("u1", "fresh"), "userId": "u1", "eventId": "fresh", "state": "pending", "createdAt": recent, "nextAttemptAt": recent})
	out, err := w.s.controlEmailOutbox(ctx, "u1", "cancel", req, "boss", func() error { return nil })
	if err != nil || out["status"] != "completed" || get("notificationEmailJobs", emailJobID("u1", "fresh"))["state"] != "pending" {
		t.Errorf("negative: the replay keeps later mail: %v %v", out, err)
	}
	if _, err := w.s.controlEmailOutbox(ctx, "u1", "cancel", req, "eve", func() error { return nil }); err == nil {
		t.Error("negative: another actor's request id is refused")
	}
}
