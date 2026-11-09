package web

import (
	"context"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Rules' "send an email": people tokens and @members as addresses, a
// non-member's dropped; variables in the subject and the message; the card's
// title, description and link at the end.
func TestRuleSendEmail(t *testing.T) {
	sink := newSMTPSink(t)
	t.Setenv("MAIL_URL", "smtp://"+sink.ln.Addr().String())
	t.Setenv("ROOT_URL", "https://wekan.example/")
	w := newTestWeKan(t)
	ann := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	eve := w.seedUser(t, "eve", "pw")
	ctx := context.Background()
	for _, u := range []*User{ann, bob, eve} {
		_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": u.ID}, bson.M{"$set": bson.M{"emails": bson.A{bson.M{"address": u.Username + "@example.org"}}}})
	}
	b, _ := w.s.CreateBoard(ctx, ann, "Mailing")
	setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: true})
	l, _ := w.s.AddList(ctx, b, "Todo")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	addRule(t, w, b.ID, "rmail", bson.M{"activityType": "createCard", "cardTitle": "*"},
		bson.M{"actionType": "sendEmail", "emailTo": "{creator}, @bob, @eve, someone@example.net, not an address, someone@example.net",
			"emailSubject": "New\r\nBcc: x@evil.example card {card}", "emailMsg": "On {board}: {card}"}, true)
	c, _ := w.s.AddCard(ctx, b, ann, l.ID, sw.ID, "Hello")
	msgs := sink.messages()
	if len(msgs) != 1 {
		t.Fatalf("one e-mail: %d", len(msgs))
	}
	sink.mu.Lock()
	rcpt := strings.Join(sink.rcpt[0], " ")
	sink.mu.Unlock()
	for _, want := range []string{"ann@example.org", "bob@example.org", "someone@example.net"} {
		if !strings.Contains(rcpt, want) {
			t.Errorf("to %s: %s", want, rcpt)
		}
	}
	if strings.Contains(rcpt, "eve@example.org") || strings.Count(rcpt, "someone@example.net") != 1 {
		t.Errorf("negative: not a non-member, once each: %s", rcpt)
	}
	m := msgs[0]
	if strings.Contains(m, "\r\nBcc:") || !strings.Contains(m, "New Bcc: x@evil.example card Hello") {
		t.Errorf("negative: the subject is one line, never a header: %q", m)
	}
	for _, want := range []string{"On Mailing: Hello", "Card: Hello", "Link: https://wekan.example/b/" + b.ID + "/" + b.Slug + "/" + c.ID} {
		if !strings.Contains(m, want) {
			t.Errorf("the message has %q: %q", want, m)
		}
	}
	// Negative: no recipient, no mail.
	if err := w.s.performAction(ctx, bson.M{"boardId": b.ID, "cardId": c.ID, "userId": ann.ID}, bson.M{"actionType": "sendEmail", "emailTo": "@eve, nonsense"}); err != nil {
		t.Fatal(err)
	}
	if len(sink.messages()) != 1 {
		t.Error("negative: nobody to send to")
	}
}
