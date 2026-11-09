package web

import (
	"context"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"os"
	"path/filepath"
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

// "Details": the card's board, list, swimlane, fields, people by name only,
// labels, custom fields (an admin-only one for a board admin alone), notes and
// the cards it names - a card on a board its user may not read left out.
func TestRuleSendEmailDetails(t *testing.T) {
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
	b, _ := w.s.CreateBoard(ctx, ann, "Plans")
	setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: true})
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"labels": bson.A{bson.M{"_id": "lab1", "name": "Urgent", "color": "red"}}}})
	l, _ := w.s.AddList(ctx, b, "Doing")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	parent, _ := w.s.AddCard(ctx, b, ann, l.ID, sw.ID, "Epic")
	secret, _ := w.s.CreateBoard(ctx, ann, "Secret") // ann's alone
	sl, _ := w.s.AddList(ctx, secret, "Hidden")
	var ssw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": secret.ID}).Decode(&ssw)
	hidden, _ := w.s.AddCard(ctx, secret, ann, sl.ID, ssw.ID, "Hidden plan")
	c, _ := w.s.AddCard(ctx, b, ann, l.ID, sw.ID, "Task")
	due := time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC)
	_, _ = w.db.Collection("customFields").InsertMany(ctx, []any{
		bson.M{"_id": "cfPrio", "boardIds": bson.A{b.ID}, "name": "Priority", "type": "dropdown", "sort": 1.0,
			"settings": bson.M{"dropdownItems": bson.A{bson.M{"_id": "hi", "name": "High"}}}},
		bson.M{"_id": "cfCost", "boardIds": bson.A{b.ID}, "name": "Cost", "type": "currency", "sort": 2.0, "settings": bson.M{"currencyCode": "EUR"}},
		bson.M{"_id": "cfSalary", "boardIds": bson.A{b.ID}, "name": "Salary", "type": "text", "adminOnly": true, "sort": 3.0}})
	_, _ = w.db.Collection("card_text_notes").InsertOne(ctx, bson.M{"_id": "n1", "cardId": c.ID, "boardId": b.ID, "title": "Plan", "text": "Do it", "createdAt": due})
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": c.ID}, bson.M{"$set": bson.M{
		"dueAt": due, "labelIds": bson.A{"lab1"}, "members": bson.A{bob.ID}, "assignees": bson.A{ann.ID},
		"parentId": parent.ID, "cardDependencies": bson.A{bson.M{"cardId": hidden.ID, "type": "blocks"}},
		"customFields": bson.A{bson.M{"_id": "cfPrio", "value": "hi"}, bson.M{"_id": "cfCost", "value": 12.5}, bson.M{"_id": "cfSalary", "value": "9000"}}}})
	mail := func(userID string) string {
		t.Helper()
		before := len(sink.messages())
		act := bson.M{"actionType": "sendEmail", "emailTo": "ann@example.org", "emailSubject": "S", "emailMsg": "M", "includeCardDetails": true}
		if err := w.s.performAction(ctx, bson.M{"boardId": b.ID, "cardId": c.ID, "userId": userID}, act); err != nil {
			t.Fatal(err)
		}
		msgs := sink.messages()
		if len(msgs) != before+1 {
			t.Fatalf("one e-mail: %d", len(msgs)-before)
		}
		decoded, _ := io.ReadAll(quotedprintable.NewReader(strings.NewReader(msgs[len(msgs)-1])))
		return strings.ReplaceAll(string(decoded), "\r\n", "\n")
	}
	m := mail(bob.ID) // a normal member
	for _, want := range []string{"Board: Plans", "List: Doing", "Due: 2026-12-01T09:00:00.000Z", "Created by: ANN Person",
		"Labels: Urgent", "Members: BOB Person", "Assignees: ANN Person", "Priority: High", "Cost: 12.5 EUR", "Plan: Do it", "Parent: Epic"} {
		if !strings.Contains(m, want) {
			t.Errorf("the details have %q: %q", want, m)
		}
	}
	for _, not := range []string{"Salary", "9000", "Hidden plan", "bob@example.org"} {
		if strings.Contains(m, not) {
			t.Errorf("negative: a normal member's mail has no %q: %q", not, m)
		}
	}
	// The board admin sees the admin-only field, and the card it can read.
	m = mail(ann.ID)
	for _, want := range []string{"Salary: 9000", "blocks: Hidden plan"} {
		if !strings.Contains(m, want) {
			t.Errorf("a board admin's details have %q: %q", want, m)
		}
	}
	// Negative: a stranger's rule gets no details and no mail.
	zed := w.seedUser(t, "zed", "pw")
	before := len(sink.messages())
	if err := w.s.performAction(ctx, bson.M{"boardId": b.ID, "cardId": c.ID, "userId": zed.ID},
		bson.M{"actionType": "sendEmail", "emailTo": "ann@example.org", "includeCardDetails": true}); err == nil || len(sink.messages()) != before {
		t.Error("negative: a card its user may not read sends nothing")
	}
}

// "Attachments": the triggering card's live files go with the mail, with a
// manifest in its text; never a deleted one or another card's, a name or a
// type never becomes a header, 8 MiB is the limit, and a card its user may
// not read sends nothing.
func TestRuleSendEmailAttachments(t *testing.T) {
	sink := newSMTPSink(t)
	t.Setenv("MAIL_URL", "smtp://"+sink.ln.Addr().String())
	w := newTestWeKan(t)
	w.s.FilesDir = filepath.Join(t.TempDir(), "files")
	ann := w.seedUser(t, "ann", "pw")
	ctx := context.Background()
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": ann.ID}, bson.M{"$set": bson.M{"profile.fullname": "ANN Person"}})
	b, _ := w.s.CreateBoard(ctx, ann, "Files")
	l, _ := w.s.AddList(ctx, b, "Todo")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	c, _ := w.s.AddCard(ctx, b, ann, l.ID, sw.ID, "With files")
	other, _ := w.s.AddCard(ctx, b, ann, l.ID, sw.ID, "Elsewhere")
	if err := os.MkdirAll(filepath.Join(w.s.FilesDir, "attachments"), 0o750); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC)
	file := func(id, cardID, name, typ string, data []byte, extra bson.M) {
		doc := bson.M{"_id": id, "name": name, "type": typ, "size": len(data), "userId": ann.ID, "uploadedAt": at,
			"meta": bson.M{"boardId": b.ID, "cardId": cardID}}
		for k, v := range extra {
			doc[k] = v
		}
		if _, err := w.db.Collection("attachments").InsertOne(ctx, doc); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(w.s.FilesDir, "attachments", id), data, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	file("a1", c.ID, "report.txt", "text/plain", []byte("report body"), nil)
	file("a2", c.ID, "../evil\r\nBcc: x@evil.example.png", "image/png\r\nX-Evil: 1", []byte{0x89, 'P', 'N', 'G'}, nil)
	file("a3", c.ID, "gone.txt", "text/plain", []byte("gone"), bson.M{"deletedAt": at})
	file("a4", other.ID, "other.txt", "text/plain", []byte("other"), nil)
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": c.ID}, bson.M{"$set": bson.M{"coverId": "a1"}})
	act := bson.M{"actionType": "sendEmail", "emailTo": "ann@example.org", "emailSubject": "Files", "emailMsg": "Here", "includeAttachments": true}
	if err := w.s.performAction(ctx, bson.M{"boardId": b.ID, "cardId": c.ID, "userId": ann.ID}, act); err != nil {
		t.Fatal(err)
	}
	msgs := sink.messages()
	if len(msgs) != 1 {
		t.Fatalf("one e-mail: %d", len(msgs))
	}
	msg, err := mail.ReadMessage(strings.NewReader(msgs[0]))
	if err != nil {
		t.Fatal(err)
	}
	if msg.Header.Get("Bcc") != "" || msg.Header.Get("X-Evil") != "" {
		t.Error("negative: a name or a type never becomes a header")
	}
	mt, params, _ := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if mt != "multipart/mixed" {
		t.Fatalf("multipart/mixed: %s", mt)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	var text string
	got := map[string]string{}
	types := map[string]string{}
	for {
		p, err := mr.NextPart()
		if err != nil {
			break
		}
		body, _ := io.ReadAll(p)
		if p.FileName() == "" {
			d, _ := io.ReadAll(quotedprintable.NewReader(strings.NewReader(string(body))))
			text = strings.ReplaceAll(string(d), "\r\n", "\n")
			continue
		}
		data, _ := base64.StdEncoding.DecodeString(strings.ReplaceAll(string(body), "\r\n", ""))
		got[p.FileName()] = string(data)
		types[p.FileName()] = p.Header.Get("Content-Type")
	}
	if got["report.txt"] != "report body" || types["report.txt"] != "text/plain" {
		t.Errorf("report.txt as it is: %q %q", got["report.txt"], types["report.txt"])
	}
	if got["evilBcc: x@evil.example.png"] != "\x89PNG" || types["evilBcc: x@evil.example.png"] != "application/octet-stream" {
		t.Errorf("negative: the name's path and line breaks gone, an unsafe type octet-stream: %v %v", got, types)
	}
	if len(got) != 2 {
		t.Errorf("negative: no deleted file and no other card's: %v", got)
	}
	for _, want := range []string{"Attachments:\nFile: report.txt\nSize (bytes): 11\nType: text/plain\nCover: true\nUploaded: 2026-11-09T08:00:00.000Z\nUploaded by: ANN Person"} {
		if !strings.Contains(text, want) {
			t.Errorf("the manifest has %q: %q", want, text)
		}
	}
	// Negative: over 8 MiB together, nothing is sent.
	file("a5", c.ID, "big.bin", "application/octet-stream", make([]byte, ruleMailMaxAttachmentBytes), nil)
	if err := w.s.performAction(ctx, bson.M{"boardId": b.ID, "cardId": c.ID, "userId": ann.ID}, act); err == nil || len(sink.messages()) != 1 {
		t.Error("negative: over the limit sends nothing")
	}
	_, _ = w.db.Collection("attachments").DeleteOne(ctx, bson.M{"_id": "a5"})
	// Negative: a user who may not read the card.
	zed := w.seedUser(t, "zed", "pw")
	if err := w.s.performAction(ctx, bson.M{"boardId": b.ID, "cardId": c.ID, "userId": zed.ID}, act); err == nil || len(sink.messages()) != 1 {
		t.Error("negative: a card its user may not read sends nothing")
	}
	// Without the choice, no files.
	delete(act, "includeAttachments")
	_ = w.s.performAction(ctx, bson.M{"boardId": b.ID, "cardId": c.ID, "userId": ann.ID}, act)
	if m := sink.messages(); len(m) != 2 || strings.Contains(m[1], "multipart/mixed") || strings.Contains(m[1], "report.txt") {
		t.Error("negative: not without includeAttachments")
	}
}
