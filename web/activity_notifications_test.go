package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// What WeKan's own changeHistoryIntegrity.js gives (.tools/tmp/mc/canon.cjs).
func TestCanonicalIsWeKans(t *testing.T) {
	if got := intentIDFor("act1"); got != "f38a994a5707fa937a2e0bf96c49ca10f9911444a1d06ffddc029c726c74525b" {
		t.Errorf("intent id: %s", got)
	}
	a := map[string]any{"_id": "act1", "activityType": "createCard", "userId": "u1",
		"createdAt": time.Date(2026, 11, 8, 10, 11, 12, 345e6, time.UTC), "modifiedAt": time.Date(2026, 11, 8, 10, 11, 12, 0, time.UTC),
		"title": "a<b>&c\u2028d \"q\" é", "n": 0.1, "big": 1e21, "small": 1e-7, "i": int32(3), "nested": bson.D{{Key: "z", Value: int32(1)}, {Key: "a", Value: bson.A{true, nil}}}}
	want := `{"_id":"act1","activityType":"createCard","big":1e+21,"createdAt":{"$date":"2026-11-08T10:11:12.345Z"},"i":3,"modifiedAt":{"$date":"2026-11-08T10:11:12.000Z"},"n":0.1,"nested":{"a":[true,null],"z":1},"small":1e-7,"title":"a<b>&c` + "\u2028" + `d \"q\" é","userId":"u1"}`
	if got := canonical(a); got != want {
		t.Errorf("canonical:\n%s\n%s", got, want)
	}
	if got := sha256Hex(canonical(a)); got != "9f4379f372990ee536ac028e62d96ca4d0e8a990e00c1334535ea3fb98d55192" {
		t.Errorf("hash: %s", got)
	}
}

func TestActivityNotificationPipeline(t *testing.T) {
	w := newTestWeKan(t)
	ann := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	a := login(t, w, "ann")
	b := newRESTBoard(t, w, a, "Pipeline")
	setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: true})
	ctx := context.Background()
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"watchers": bson.A{bson.M{"userId": bob.ID, "level": "watching"}}}})
	// An ordinary activity: intent and plan, delivered, both compacted.
	w.s.activity(ctx, ann.ID, "createCard", bson.M{"boardId": b.ID, "cardId": b.Card, "listId": b.List})
	var act bson.M
	_ = w.db.Collection("activities").FindOne(ctx, bson.M{"activityType": "createCard", "userId": ann.ID, "cardId": b.Card},
		options.FindOne().SetSort(bson.D{{Key: "createdAt", Value: -1}})).Decode(&act)
	id := act["_id"].(string)
	var intent, plan bson.M
	_ = w.db.Collection("activityNotificationIntents").FindOne(ctx, bson.M{"_id": intentIDFor(id)}).Decode(&intent)
	if intent["state"] != "completed" || intent["activity"] != nil || intent["dispatchUserId"] != ann.ID {
		t.Fatalf("the intent is a receipt: %v", intent)
	}
	_ = w.db.Collection("activityNotificationPlans").FindOne(ctx, bson.M{"_id": planIDFor(id)}).Decode(&plan)
	if intOf(plan["compactReceiptVersion"]) != 1 || plan["plan"] != nil {
		t.Fatalf("the plan is a receipt: %v", plan)
	}
	var u bson.M
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"_id": bob.ID}).Decode(&u)
	if !strings.Contains(canonical(nestedField(u, "profile", "notifications")), id) {
		t.Error("the watcher has it in the drawer")
	}
	// Recovery: a frozen plan, behind a crashed worker's expired lease, is
	// delivered as frozen; an intent whose activity is gone stays pending.
	ev := []string{"resumeevent00001", "orphanevent00001"}
	now := time.Now().UTC().Truncate(time.Millisecond)
	for i, e := range ev {
		activity := map[string]any{"_id": e, "activityType": "createCard", "userId": bob.ID, "createdAt": now, "modifiedAt": now}
		p := map[string]any{"version": 1, "activityId": e, "activityHash": sha256Hex(canonical(activity)), "dispatchUserId": nil,
			"recipients": []any{map[string]any{"userId": bob.ID, "tray": true, "email": nil}}}
		_, _ = w.db.Collection("activityNotificationPlans").InsertOne(ctx, bson.M{"_id": planIDFor(e), "checksum": sha256Hex(canonical(p)), "plan": p})
		if i == 0 {
			_, _ = w.db.Collection("activities").InsertOne(ctx, activity)
		}
		_, _ = w.db.Collection("activityNotificationLeases").InsertOne(ctx, bson.M{"_id": intentIDFor(e), "owner": "crashed-worker", "expiresAt": time.Unix(0, 0)})
		_, _ = w.db.Collection("activityNotificationIntents").InsertOne(ctx, bson.M{"_id": intentIDFor(e), "version": 1, "state": "pending",
			"activity": activity, "activityHash": p["activityHash"], "dispatchUserId": nil, "writerId": "w"})
	}
	after := ""
	w.s.recoverActivityNotifications(ctx, &after)
	state := func(e string) any {
		var r bson.M
		_ = w.db.Collection("activityNotificationIntents").FindOne(ctx, bson.M{"_id": intentIDFor(e)}).Decode(&r)
		return r["state"]
	}
	if state(ev[0]) != "completed" {
		t.Errorf("recovered: %v", state(ev[0]))
	}
	if n, _ := w.db.Collection("activityNotificationLeases").CountDocuments(ctx, bson.M{"_id": intentIDFor(ev[0])}); n != 0 {
		t.Error("the lease is released")
	}
	if state(ev[1]) != "pending" {
		t.Errorf("negative: an orphan stays pending: %v", state(ev[1]))
	}
	if n, _ := w.db.Collection("activities").CountDocuments(ctx, bson.M{"_id": ev[1]}); n != 0 {
		t.Error("negative: a missing activity is never recreated")
	}
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"_id": bob.ID}).Decode(&u)
	if !strings.Contains(canonical(nestedField(u, "profile", "notifications")), ev[0]) {
		t.Error("the frozen plan's drawer entry is delivered")
	}
	// A live lease is somebody else's work: nothing is done twice.
	_, _ = w.db.Collection("activityNotificationLeases").InsertOne(ctx, bson.M{"_id": "busyintent", "owner": "other", "expiresAt": time.Now().Add(time.Minute)})
	if err := w.s.withActivityLease(ctx, "busyintent", func() error { t.Error("negative: ran under a live lease"); return nil }); err != errLeaseBusy {
		t.Errorf("busy: %v", err)
	}
	// The scheduled cleanup: a completed intent's plan loses its body; a
	// pending one's keeps it, undelivered.
	for _, done := range []bool{true, false} {
		e := map[bool]string{true: "cleanupdone00001", false: "cleanuppending01"}[done]
		activity := map[string]any{"_id": e, "activityType": "createCard", "userId": bob.ID, "createdAt": now, "modifiedAt": now}
		hash := sha256Hex(canonical(activity))
		p := map[string]any{"version": 1, "activityId": e, "activityHash": hash, "dispatchUserId": nil, "recipients": []any{map[string]any{"userId": bob.ID,
			"tray": false, "email": map[string]any{"userId": bob.ID, "eventId": e, "subject": "PRIVATE", "html": "PRIVATE BODY", "language": "en", "boardId": nil, "cardId": nil}}}}
		in := bson.M{"_id": intentIDFor(e), "version": 1, "state": "completed", "activityHash": hash, "dispatchUserId": nil, "writerId": "w"}
		if !done {
			in["state"], in["activity"] = "pending", activity
		}
		_, _ = w.db.Collection("activityNotificationIntents").InsertOne(ctx, in)
		_, _ = w.db.Collection("activityNotificationPlans").InsertOne(ctx, bson.M{"_id": planIDFor(e), "checksum": sha256Hex(canonical(p)), "plan": p})
	}
	after = ""
	w.s.cleanupActivityPlans(ctx, &after)
	var done, pending bson.M
	_ = w.db.Collection("activityNotificationPlans").FindOne(ctx, bson.M{"_id": planIDFor("cleanupdone00001")}).Decode(&done)
	_ = w.db.Collection("activityNotificationPlans").FindOne(ctx, bson.M{"_id": planIDFor("cleanuppending01")}).Decode(&pending)
	if intOf(done["compactReceiptVersion"]) != 1 || strings.Contains(canonical(done), "PRIVATE") || len(done) != 4 {
		t.Errorf("a completed plan is a receipt: %v", done)
	}
	if !strings.Contains(canonical(pending), "PRIVATE BODY") {
		t.Errorf("negative: a pending plan keeps its body: %v", pending)
	}
}
