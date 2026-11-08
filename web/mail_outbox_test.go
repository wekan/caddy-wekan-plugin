package web

import (
	"context"
	"errors"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The values WeKan's own modules give (.tools/tmp/mc/sched.cjs): the job id,
// and the schedules across Helsinki's daylight-saving change.
func TestOutboxIsWeKans(t *testing.T) {
	if got := emailJobID("userA", "act<1>&"); got != "c37f14021f469af5503c1cac56274d224fa50099fb244e8494033aa17b1d05fc" {
		t.Errorf("idFor: %s", got)
	}
	at := time.Date(2026, 3, 29, 0, 20, 0, 0, time.UTC)
	hki := emailDelivery{Timezone: "Europe/Helsinki"}
	for name, c := range map[string]struct {
		from time.Time
		d    emailDelivery
		want string
	}{
		"hourly over the DST change": {at, emailDelivery{Schedule: "interval", IntervalMinutes: 60, Timezone: hki.Timezone}, "2026-03-29T01:00:00Z"},
		"daily at 08:00":             {at, emailDelivery{Schedule: "daily", DailyTime: "08:00", Timezone: hki.Timezone}, "2026-03-29T05:00:00Z"},
		"quiet hours until 07:30": {time.Date(2026, 6, 1, 20, 0, 0, 0, time.UTC),
			emailDelivery{Schedule: "immediate", QuietStart: "22:00", QuietEnd: "07:30", Timezone: hki.Timezone}, "2026-06-02T04:30:00Z"},
		"a day, in UTC": {time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC), emailDelivery{Schedule: "interval", IntervalMinutes: 1440}, "2026-06-02T00:00:00Z"},
	} {
		if got := nextDeliveryAt(c.from, c.d, 30*time.Second).Format(time.RFC3339); got != c.want {
			t.Errorf("%s: %s, WeKan's %s", name, got, c.want)
		}
	}
	for _, k := range [][2]string{{"all", "all"}, {"board", "board:B"}, {"card", "card:C"}, {"none", "single:E"}} {
		if got := emailGroupKey(k[0], "B", "C", "E"); got != k[1] {
			t.Errorf("group key %s: %s", k[0], got)
		}
	}
	if emailGroupKey("card", "B", "", "E") != "board:B" || emailGroupKey("x", "B", "C", "E") != "" {
		t.Error("a card-less job groups by its board; an unknown grouping is none")
	}
}

func TestEmailRetryDecision(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	temp := &textproto.Error{Code: 451, Msg: "later"}
	d := emailRetryDecision(temp, "smtp", 1, now, 0)
	if d.state != "pending" || d.reason != "smtp-temporary" || d.next.Sub(now) != 5*time.Second {
		t.Errorf("451, first try: %+v", d)
	}
	if d := emailRetryDecision(temp, "smtp", 3, now, 1); d.next.Sub(now) != 25*time.Second {
		t.Errorf("doubling, with a quarter of jitter: %v", d.next.Sub(now))
	}
	if d := emailRetryDecision(temp, "smtp", 11, now, 0); d.next.Sub(now) != 48*time.Minute {
		t.Errorf("capped at 48 minutes: %v", d.next.Sub(now))
	}
	for name, c := range map[string]struct {
		err    error
		phase  string
		tries  int
		state  string
		reason string
	}{
		"rejected":      {&textproto.Error{Code: 550}, "smtp", 1, "failed", "smtp-rejected"},
		"login refused": {&textproto.Error{Code: 535}, "smtp", 1, "failed", "smtp-authentication"},
		"the 12th try":  {temp, "smtp", 12, "failed", "retry-limit"},
		"no address":    {errRecipientUnavailable, "preparation", 1, "pending", "recipient-unavailable"},
		"network":       {errors.New("connection refused"), "smtp", 1, "pending", "delivery-failed"},
	} {
		if d := emailRetryDecision(c.err, c.phase, c.tries, now, 0); d.state != c.state || d.reason != c.reason {
			t.Errorf("%s: %+v", name, d)
		}
	}
}

// A notification is a job; a 4xx keeps it and it is sent again without
// another notification; a member who left gets none; a legacy buffer is
// queued and sent; the reply text of the server is never stored.
func TestEmailOutbox(t *testing.T) {
	sink := newSMTPSink(t)
	t.Setenv("MAIL_URL", "smtp://"+sink.ln.Addr().String())
	w := newTestWeKan(t)
	ann := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	a := login(t, w, "ann")
	b := newRESTBoard(t, w, a, "Mail")
	setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: true})
	ctx := context.Background()
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": bob.ID}, bson.M{"$set": bson.M{"emails": bson.A{bson.M{"address": "Bob@Example.org", "verified": true}}}})
	jobs := w.db.Collection("notificationEmailJobs")
	past := time.Now().UTC().Add(-time.Second)
	enqueue := func(event, html string) {
		text := strings.ToUpper(html)
		if err := w.s.enqueueEmail(ctx, emailJob{UserID: bob.ID, EventID: event, Subject: "S", HTML: html, Language: "en", BoardID: b.ID, Text: &text, DeliverAt: &past}); err != nil {
			t.Fatal(err)
		}
	}
	enqueue("e1", "first <b>one</b>")
	enqueue("e1", "a duplicate is the same job")
	enqueue("e2", "second")
	if n, _ := jobs.CountDocuments(ctx, bson.M{"userId": bob.ID}); n != 2 {
		t.Fatalf("one job an event: %d", n)
	}
	sink.setReject(451)
	w.s.drainEmail(ctx)
	if len(sink.messages()) != 1 {
		t.Fatalf("one e-mail for both: %d", len(sink.messages()))
	}
	var job bson.M
	_ = jobs.FindOne(ctx, bson.M{"_id": emailJobID(bob.ID, "e1")}).Decode(&job)
	if job["state"] != "pending" || job["lastFailure"] != "smtp-temporary" || intOf(job["attempts"]) != 1 {
		t.Fatalf("kept for a retry: %v", job)
	}
	for _, v := range job {
		if s, ok := v.(string); ok && strings.Contains(s, "test rejection") {
			t.Error("negative: the server's reply text is never stored")
		}
	}
	sink.setReject(0)
	_, _ = jobs.UpdateMany(ctx, bson.M{"userId": bob.ID}, bson.M{"$set": bson.M{"nextAttemptAt": past}})
	w.s.drainEmail(ctx)
	msgs := sink.messages()
	if len(msgs) != 2 || !strings.Contains(msgs[1], "first <b>one</b>") || !strings.Contains(msgs[1], "SECOND") || !strings.Contains(msgs[1], "multipart/alternative") {
		t.Fatalf("sent again, HTML with its text: %q", msgs)
	}
	if n, _ := jobs.CountDocuments(ctx, bson.M{"userId": bob.ID, "state": "sent", "html": bson.M{"$exists": false}}); n != 2 {
		t.Errorf("sent, the content cleared: %d", n)
	}
	// Bob leaves the board: his queued job is cancelled, not sent.
	setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: false})
	enqueue("e3", "after leaving")
	w.s.drainEmail(ctx)
	_ = jobs.FindOne(ctx, bson.M{"_id": emailJobID(bob.ID, "e3")}).Decode(&job)
	if job["state"] != "cancelled" || len(sink.messages()) != 2 {
		t.Errorf("negative: a former member gets nothing: %v", job["state"])
	}
	// A legacy profile buffer is queued, then sent.
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": ann.ID}, bson.M{"$set": bson.M{"profile.emailBuffer": bson.A{"Legacy pending digest"}}})
	w.s.drainEmail(ctx)
	_, _ = jobs.UpdateMany(ctx, bson.M{"userId": ann.ID}, bson.M{"$set": bson.M{"nextAttemptAt": past}})
	w.s.drainEmail(ctx)
	var u bson.M
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"_id": ann.ID}).Decode(&u)
	if buf, _ := nestedField(u, "profile", "emailBuffer").(bson.A); len(buf) != 0 {
		t.Errorf("the buffer is emptied: %v", buf)
	}
	if n, _ := jobs.CountDocuments(ctx, bson.M{"userId": ann.ID, "state": "sent"}); n != 1 {
		t.Errorf("the legacy text is sent once: %d", n)
	}
}

// Grouping by board: one e-mail per board, never one for both.
func TestEmailOutboxGroups(t *testing.T) {
	sink := newSMTPSink(t)
	t.Setenv("MAIL_URL", "smtp://"+sink.ln.Addr().String())
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	a := login(t, w, "ann")
	ctx := context.Background()
	past := time.Now().UTC().Add(-time.Second)
	for i, title := range []string{"One", "Two"} {
		b := newRESTBoard(t, w, a, title)
		setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: true})
		k := "board:" + b.ID
		_ = w.s.enqueueEmail(ctx, emailJob{UserID: bob.ID, EventID: title, Subject: "S", HTML: title, Language: "en", BoardID: b.ID, GroupKey: &k, DeliverAt: &past})
		_ = i
	}
	w.s.drainEmail(ctx)
	w.s.drainEmail(ctx)
	if m := sink.messages(); len(m) != 2 || strings.Contains(m[0], "Two") == strings.Contains(m[1], "Two") {
		t.Errorf("one e-mail a board: %q", m)
	}
}
