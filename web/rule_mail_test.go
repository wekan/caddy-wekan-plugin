package web

import (
	"context"
	"io"
	"mime/quotedprintable"
	"strings"
	"testing"
	"time"

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

// "Checklists / Comments": the card's checklists and items, its comments with
// their authors' display names and reactions - never an author's address -
// only on request, and nothing at all for a card the rule's user may not read.
func TestRuleSendEmailDiscussion(t *testing.T) {
	sink := newSMTPSink(t)
	t.Setenv("MAIL_URL", "smtp://"+sink.ln.Addr().String())
	w := newTestWeKan(t)
	ann := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	ctx := context.Background()
	for _, u := range []*User{ann, bob} {
		_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": u.ID}, bson.M{"$set": bson.M{
			"emails": bson.A{bson.M{"address": u.Username + "@example.org"}}, "profile.fullname": strings.ToUpper(u.Username) + " Person"}})
	}
	b, _ := w.s.CreateBoard(ctx, ann, "Talk")
	l, _ := w.s.AddList(ctx, b, "Todo")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	c, _ := w.s.AddCard(ctx, b, ann, l.ID, sw.ID, "Hello")
	at := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	ins := func(coll string, doc bson.M) {
		if _, err := w.db.Collection(coll).InsertOne(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}
	ins("checklists", bson.M{"_id": "cl1", "cardId": c.ID, "title": "Launch", "sort": 0.0, "dueAt": at})
	ins("checklistItems", bson.M{"_id": "it1", "cardId": c.ID, "checklistId": "cl1", "title": "Write", "isFinished": true, "sort": 0.0})
	ins("checklistItems", bson.M{"_id": "it2", "cardId": c.ID, "checklistId": "cl1", "title": "Ship", "isFinished": false, "sort": 1.0})
	ins("checklistItems", bson.M{"_id": "it3", "cardId": c.ID, "checklistId": "cl1", "title": "Gone", "sort": 2.0, "deletedAt": at})
	ins("card_comments", bson.M{"_id": "cm1", "cardId": c.ID, "boardId": b.ID, "userId": bob.ID, "text": "Looks good", "createdAt": at})
	ins("card_comment_reactions", bson.M{"_id": "rx1", "cardId": c.ID, "boardId": b.ID, "cardCommentId": "cm1",
		"reactions": bson.A{bson.M{"reactionCodepoint": "&#128077;", "userIds": bson.A{ann.ID, bob.ID, ann.ID}}, bson.M{"reactionCodepoint": "&#1;", "userIds": bson.A{ann.ID}}}})
	act := bson.M{"actionType": "sendEmail", "emailTo": "ann@example.org", "emailSubject": "S", "emailMsg": "M", "includeChecklistsAndComments": true}
	if err := w.s.performAction(ctx, bson.M{"boardId": b.ID, "cardId": c.ID, "userId": ann.ID}, act); err != nil {
		t.Fatal(err)
	}
	msgs := sink.messages()
	if len(msgs) != 1 {
		t.Fatalf("one e-mail: %d", len(msgs))
	}
	decoded, _ := io.ReadAll(quotedprintable.NewReader(strings.NewReader(msgs[0])))
	m := strings.ReplaceAll(string(decoded), "\r\n", "\n")
	for _, want := range []string{"Checklists:\nLaunch\n  Due: 2026-11-09T12:00:00.000Z\n  [x] Write\n  [ ] Ship",
		"Comments:\n[2026-11-09T12:00:00.000Z]\nAuthor: BOB Person\nLooks good\nReactions: \U0001F44D 2"} {
		if !strings.Contains(m, want) {
			t.Errorf("the message has %q: %q", want, m)
		}
	}
	if strings.Contains(m, "Gone") || strings.Contains(m, "bob@example.org") {
		t.Errorf("negative: no deleted item, no author's address: %q", m)
	}
	// Negative: off unless asked.
	delete(act, "includeChecklistsAndComments")
	_ = w.s.performAction(ctx, bson.M{"boardId": b.ID, "cardId": c.ID, "userId": ann.ID}, act)
	if msgs := sink.messages(); len(msgs) != 2 || strings.Contains(msgs[1], "Checklists:") || strings.Contains(msgs[1], "Looks good") {
		t.Errorf("negative: not without includeChecklistsAndComments")
	}
	// Negative: the rule's user is an assigned-only member, the card not theirs.
	act["includeChecklistsAndComments"] = true
	setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: true, IsNormalAssignedOnly: true})
	if err := w.s.performAction(ctx, bson.M{"boardId": b.ID, "cardId": c.ID, "userId": bob.ID}, act); err == nil {
		t.Error("negative: a card its user may not read is refused")
	}
	// Negative: nor by somebody who is not on the private board at all.
	stranger := w.seedUser(t, "zed", "pw")
	if err := w.s.performAction(ctx, bson.M{"boardId": b.ID, "cardId": c.ID, "userId": stranger.ID}, act); err == nil {
		t.Error("negative: a board its user may not read is refused")
	}
	if len(sink.messages()) != 2 {
		t.Errorf("negative: no mail for a refused card: %d", len(sink.messages()))
	}
}
