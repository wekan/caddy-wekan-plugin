package web

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Admin Panel / Problems / Recovery's e-mail delivery queue, as WeKan's
// email-recovery-controls spec drives it: the report, pause and resume,
// cancellation that keeps later mail, retry, a busy recipient, and capacity.
func TestEmailRecoveryControls(t *testing.T) {
	sink := newSMTPSink(t)
	t.Setenv("MAIL_URL", "smtp://"+sink.ln.Addr().String())
	w := newTestWeKan(t)
	boss := w.seedUser(t, "boss", "pw")
	bob := w.seedUser(t, "bob", "pw")
	w.seedUser(t, "eve", "pw")
	ctx := context.Background()
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": boss.ID}, bson.M{"$set": bson.M{"isAdmin": true}})
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": bob.ID}, bson.M{"$set": bson.M{"emails": bson.A{bson.M{"address": "bob@example.org", "verified": true}}}})
	client := func(name string) func(method, args string) map[string]any {
		c := w.client(t)
		body(t, w.signIn(t, c, name, "pw"))
		return func(method, args string) map[string]any {
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
	}
	asBoss, asEve := client("boss"), client("eve")
	jobs := w.db.Collection("notificationEmailJobs")
	queued := func(id, userID string, due time.Time, extra bson.M) {
		doc := bson.M{"_id": id, "userId": userID, "eventId": id, "boardId": nil, "cardId": nil, "state": "pending", "attempts": 0,
			"subject": "PRIVATE-QUEUE-SUBJECT", "html": "PRIVATE-QUEUE-BODY", "language": "en", "createdAt": time.Now().UTC(), "nextAttemptAt": due}
		for k, v := range extra {
			doc[k] = v
		}
		if _, err := jobs.InsertOne(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}
	later := time.Now().UTC().Add(24 * time.Hour)
	job := func(id string) bson.M {
		var j bson.M
		_ = jobs.FindOne(ctx, bson.M{"_id": id}).Decode(&j)
		return j
	}
	report := func(search string, page int) map[string]any {
		out := asBoss("emailRecoveryReport", `[{"search":"`+search+`","page":`+itoa(page)+`}]`)
		r, _ := out["result"].(map[string]any)
		if r == nil {
			t.Fatalf("report: %v", out)
		}
		return r
	}
	control := func(userID, action, req string) map[string]any {
		return asBoss("controlEmailRecovery", `[{"userId":"`+userID+`","action":"`+action+`","requestId":"`+req+`"}]`)
	}

	// The report: a row a recipient, ten a page, a paused one with nothing queued.
	for i := 0; i < 13; i++ {
		u := "mailreport-" + itoa(10+i)
		queued(u+"-0", u, later, nil)
		queued(u+"-1", u, later, bson.M{"attempts": 3})
	}
	_, _ = w.db.Collection("notificationEmailControls").InsertOne(ctx, bson.M{"_id": "mailreport-empty", "paused": true})
	_, _ = w.db.Collection("notificationEmailControls").InsertOne(ctx, bson.M{"_id": "mailreport-10", "paused": true})
	r := report("mailreport", 1)
	rows := r["rows"].([]any)
	first := rows[0].(map[string]any)
	if r["total"].(float64) != 14 || len(rows) != 10 || first["queued"].(float64) != 2 || first["retrying"].(float64) != 1 || first["paused"] != true {
		t.Fatalf("report: %v", r)
	}
	p2 := report("mailreport", 2)["rows"].([]any)
	if len(p2) != 4 || p2[3].(map[string]any)["userId"] != "mailreport-empty" || p2[3].(map[string]any)["queued"].(float64) != 0 {
		t.Errorf("page 2, the paused empty one: %v", p2)
	}
	raw, _ := json.Marshal(r)
	if regexp.MustCompile(`PRIVATE-QUEUE|html|subject|eventId|@`).Match(raw) {
		t.Errorf("negative: the report leaks: %s", raw)
	}
	// Negative: an administrator's only.
	if out := asEve("emailRecoveryReport", `[{"search":"","page":1}]`); out["error"] != "not-authorized" {
		t.Errorf("negative: report by a non-admin: %v", out)
	}
	if out := asEve("controlEmailRecovery", `[{"userId":"mailreport-11","action":"pause","requestId":"`+strings.Repeat("a", 32)+`"}]`); out["error"] != "not-authorized" {
		t.Errorf("negative: control by a non-admin: %v", out)
	}
	if n, _ := w.db.Collection("notificationEmailControls").CountDocuments(ctx, bson.M{"_id": "mailreport-11"}); n != 0 {
		t.Error("negative: a refused control wrote nothing")
	}
	if out := control(bob.ID, "delete", strings.Repeat("x", 32)); out["error"] != "email-recovery-failed" {
		t.Errorf("negative: an unknown action: %v", out)
	}

	// Pause holds existing and new mail; resume sends both, in one e-mail.
	queued("held-mail", bob.ID, later, nil)
	if out := control(bob.ID, "pause", strings.Repeat("p", 32)); canonical(out["result"]) != `{"status":"completed"}` {
		t.Fatalf("pause: %v", out)
	}
	_, _ = jobs.UpdateOne(ctx, bson.M{"_id": "held-mail"}, bson.M{"$set": bson.M{"nextAttemptAt": time.Now().UTC()}})
	queued("new-held-mail", bob.ID, time.Now().UTC(), nil)
	w.s.drainEmail(ctx)
	if len(sink.messages()) != 0 || job("held-mail")["state"] != "pending" || job("new-held-mail")["state"] != "pending" {
		t.Fatalf("negative: nothing sent while paused: %d", len(sink.messages()))
	}
	if out := control(bob.ID, "resume", strings.Repeat("r", 32)); canonical(out["result"]) != `{"status":"completed"}` {
		t.Fatalf("resume: %v", out)
	}
	w.s.drainEmail(ctx)
	if len(sink.messages()) != 1 || job("held-mail")["state"] != "sent" || job("new-held-mail")["state"] != "sent" {
		t.Fatalf("resumed: %d %v", len(sink.messages()), job("held-mail")["state"])
	}
	// Cancel: what is queued goes, content and all; a replay keeps later mail.
	queued("cancel-mail", bob.ID, later, nil)
	cancel := strings.Repeat("c", 32)
	if out := control(bob.ID, "cancel", cancel); canonical(out["result"]) != `{"status":"completed"}` {
		t.Fatalf("cancel: %v", out)
	}
	if j := job("cancel-mail"); j["state"] != "cancelled" || j["html"] != nil {
		t.Errorf("cancelled, content gone: %v", j)
	}
	time.Sleep(5 * time.Millisecond)
	queued("later-mail", bob.ID, later, nil)
	if out := control(bob.ID, "cancel", cancel); canonical(out["result"]) != `{"status":"completed"}` || job("later-mail")["state"] != "pending" {
		t.Errorf("negative: a replayed cancel keeps later mail: %v %v", out, job("later-mail")["state"])
	}
	// Negative: a request superseded by a newer one changes nothing.
	old := strings.Repeat("o", 32)
	_, _ = w.db.Collection("notificationEmailCommands").InsertOne(ctx, bson.M{"_id": old, "userId": bob.ID, "action": "cancel", "actorId": boss.ID,
		"cutoff": time.Now().UTC(), "generation": 1, "status": "pending"})
	if out := control(bob.ID, "cancel", old); canonical(out["result"]) != `{"status":"superseded"}` || job("later-mail")["state"] != "pending" {
		t.Errorf("negative: superseded: %v", out)
	}
	// Negative: another actor's request id is not this one's.
	if out := asBoss("controlEmailRecovery", `[{"userId":"`+bob.ID+`","action":"pause","requestId":"`+cancel+`"}]`); out["error"] != "email-recovery-failed" {
		t.Errorf("negative: a request id names one action: %v", out)
	}
	// A delivery in progress: the control answers busy and changes nothing.
	_, _ = w.db.Collection("notificationEmailLeases").InsertOne(ctx, bson.M{"_id": bob.ID, "owner": "test-in-flight", "expiresAt": time.Now().Add(time.Minute)})
	if out := control(bob.ID, "pause", strings.Repeat("b", 32)); out["error"] != "email-recovery-busy" {
		t.Errorf("busy: %v", out)
	}
	var c bson.M
	_ = w.db.Collection("notificationEmailControls").FindOne(ctx, bson.M{"_id": bob.ID}).Decode(&c)
	if c["paused"] != false || intOf(c["cancelCount"]) != 1 {
		t.Errorf("negative: busy changed nothing, one cancel counted: %v", c)
	}
	// ... and the outbox does not send under somebody else's lease either.
	_, _ = jobs.UpdateOne(ctx, bson.M{"_id": "later-mail"}, bson.M{"$set": bson.M{"nextAttemptAt": time.Now().UTC()}})
	before := len(sink.messages())
	w.s.drainEmail(ctx)
	if len(sink.messages()) != before || intOf(job("later-mail")["cycleAttempts"]) != 0 {
		t.Error("negative: no delivery under another sender's lease")
	}
	_, _ = w.db.Collection("notificationEmailLeases").DeleteMany(ctx, bson.M{"owner": "test-in-flight"})

	// Capacity: every send slot taken - the mail waits, no attempt spent.
	for slot := 0; slot < emailSendCapacity; slot++ {
		_, _ = w.db.Collection("notificationEmailSendSlots").InsertOne(ctx, bson.M{"_id": "slot-" + itoa(slot), "owner": "capacity-owner", "expiresAt": time.Now().Add(time.Minute)})
	}
	w.s.drainEmail(ctx)
	if j := job("later-mail"); j["state"] != "pending" || intOf(j["cycleAttempts"]) != 0 || intOf(j["attempts"]) != 0 || len(sink.messages()) != before {
		t.Errorf("negative: no attempt without a slot: %v", j)
	}
	_, _ = w.db.Collection("notificationEmailSendSlots").DeleteMany(ctx, bson.M{"owner": "capacity-owner"})
	w.s.drainEmail(ctx)
	if job("later-mail")["state"] != "sent" {
		t.Errorf("sent once a slot is free: %v", job("later-mail"))
	}
	if n, _ := w.db.Collection("notificationEmailSendSlots").CountDocuments(ctx, bson.M{}); n != 0 {
		t.Errorf("slots released: %d", n)
	}

	// A permanent rejection stays stopped until an administrator retries it.
	sink.setReject(550)
	queued("rejected-mail", bob.ID, time.Now().UTC(), nil)
	w.s.drainEmail(ctx)
	if j := job("rejected-mail"); j["state"] != "failed" || j["lastFailure"] != "smtp-rejected" {
		t.Fatalf("failed for good: %v", j)
	}
	w.s.drainEmail(ctx)
	if intOf(job("rejected-mail")["cycleAttempts"]) != 1 {
		t.Error("negative: a failed message is not retried by itself")
	}
	row := report(bob.ID, 1)["rows"].([]any)[0].(map[string]any)
	if row["failed"].(float64) != 1 || canonical(row["failures"]) != `[{"count":1,"reason":"smtp-rejected"}]` {
		t.Errorf("stopped, and why: %v", row)
	}
	sink.setReject(0)
	if out := control(bob.ID, "retry", strings.Repeat("t", 32)); canonical(out["result"]) != `{"status":"completed"}` {
		t.Fatalf("retry: %v", out)
	}
	w.s.drainEmail(ctx)
	if j := job("rejected-mail"); j["state"] != "sent" || j["html"] != nil || intOf(j["cycleAttempts"]) != 1 || intOf(j["attempts"]) != 1 {
		t.Errorf("retried and sent: %v", j)
	}
}

// Four recipients at once: a slow SMTP server for one holds up no other.
func TestEmailOutboxRecipientsInParallel(t *testing.T) {
	sink := newSMTPSink(t)
	t.Setenv("MAIL_URL", "smtp://"+sink.ln.Addr().String())
	w := newTestWeKan(t)
	ctx := context.Background()
	var ids []string
	for _, name := range []string{"ann", "bob"} {
		u := w.seedUser(t, name, "pw")
		_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": u.ID}, bson.M{"$set": bson.M{"emails": bson.A{bson.M{"address": name + "@example.org"}}}})
		past := time.Now().UTC().Add(-time.Second)
		if err := w.s.enqueueEmail(ctx, emailJob{UserID: u.ID, EventID: "e-" + name, Subject: "S", HTML: "hi", Language: "en", DeliverAt: &past}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, u.ID)
	}
	// One recipient's lease is held as a slow delivery would hold it.
	_, _ = w.db.Collection("notificationEmailLeases").InsertOne(ctx, bson.M{"_id": ids[0], "owner": "slow", "expiresAt": time.Now().Add(time.Minute)})
	w.s.drainEmail(ctx)
	var a, b bson.M
	_ = w.db.Collection("notificationEmailJobs").FindOne(ctx, bson.M{"userId": ids[0]}).Decode(&a)
	_ = w.db.Collection("notificationEmailJobs").FindOne(ctx, bson.M{"userId": ids[1]}).Decode(&b)
	if a["state"] != "pending" || b["state"] != "sent" {
		t.Errorf("the other recipient is delivered: %v %v", a["state"], b["state"])
	}
}

// The panel is on the Recovery page, its strings in the language.
func TestEmailRecoveryPanel(t *testing.T) {
	w := newTestWeKan(t)
	boss := w.seedUser(t, "boss", "pw")
	_, _ = w.db.Collection("users").UpdateOne(context.Background(), bson.M{"_id": boss.ID}, bson.M{"$set": bson.M{"isAdmin": true}})
	c := w.client(t)
	body(t, w.signIn(t, c, "boss", "pw"))
	_, page := get(t, c, w.srv.URL+"/admin/problems/recovery")
	for _, want := range []string{"email-recovery-reports", "Email delivery queue", `data-action="refresh-email"`, `data-k="email-failure-smtp-rejected"`} {
		if !strings.Contains(page, want) {
			t.Errorf("page has %q", want)
		}
	}
}
