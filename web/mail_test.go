package web

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// smtpSink is a local SMTP server for tests: it keeps what it is sent and
// relays nothing.
type smtpSink struct {
	ln   net.Listener
	mu   sync.Mutex
	msgs []string
	rcpt [][]string
}

func newSMTPSink(t *testing.T) *smtpSink {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &smtpSink{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

func (s *smtpSink) serve(c net.Conn) {
	defer c.Close()
	rd := bufio.NewReader(c)
	say := func(l string) { _, _ = c.Write([]byte(l + "\r\n")) }
	say("220 sink")
	var rcpt []string
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			say("250 sink")
		case strings.HasPrefix(cmd, "MAIL FROM"):
			say("250 ok")
		case strings.HasPrefix(cmd, "RCPT TO"):
			rcpt = append(rcpt, strings.TrimSpace(line[8:]))
			say("250 ok")
		case cmd == "DATA":
			say("354 go")
			var b strings.Builder
			for {
				l, err := rd.ReadString('\n')
				if err != nil || l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			s.mu.Lock()
			s.msgs, s.rcpt = append(s.msgs, b.String()), append(s.rcpt, rcpt)
			s.mu.Unlock()
			rcpt = nil
			say("250 kept")
		case cmd == "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

func (s *smtpSink) messages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.msgs...)
}

func TestBuildMailRefusesHeaderInjection(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	msg, err := buildMail("Boards Support <support@example.com>", mailMessage{To: []string{"a@example.com"}, Subject: "Hyvää päivää", Text: "line\nnext"}, now)
	if err != nil {
		t.Fatal(err)
	}
	s := string(msg)
	if !strings.Contains(s, "Subject: =?utf-8?q?Hyv=C3=A4=C3=A4_p=C3=A4iv=C3=A4=C3=A4?=") || !strings.Contains(s, "line\r\nnext") {
		t.Fatalf("encoded subject and CRLF text: %s", s)
	}
	for _, m := range []mailMessage{
		{To: []string{"a@example.com"}, Subject: "x\r\nBcc: evil@example.com"},
		{To: []string{"a@example.com\r\nBcc: evil@example.com"}, Subject: "x"},
		{To: []string{"not an address"}, Subject: "x"},
		{Subject: "nobody"},
	} {
		if _, err := buildMail("support@example.com", m, now); err == nil {
			t.Errorf("negative: refused: %+v", m)
		}
	}
	if _, err := buildMail("a@example.com\r\nBcc: evil@example.com", mailMessage{To: []string{"a@example.com"}}, now); err == nil {
		t.Error("negative: a sender with a line break")
	}
}

func TestSendInvitation(t *testing.T) {
	sink := newSMTPSink(t)
	t.Setenv("MAIL_URL", "smtp://"+sink.ln.Addr().String())
	t.Setenv("MAIL_FROM", "")
	w := newTestWeKan(t)
	admin := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	ctx := context.Background()
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": admin.ID}, bson.M{"$set": bson.M{"isAdmin": true}})
	mine, _ := w.s.CreateBoard(ctx, admin, "Mine")
	victim, _ := w.s.CreateBoard(ctx, admin, "Private")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": mine.ID}, bson.M{"$push": bson.M{"members": bson.M{"userId": bob.ID, "isActive": true}}})
	call := func(cl *http.Client, args string) (int, map[string]any) {
		req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/sendInvitation", strings.NewReader(args))
		req.Header.Set("Content-Type", "application/json")
		res, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	if code, out := call(ann, `[["New.Person@Example.com"],["`+mine.ID+`"]]`); code != 200 {
		t.Fatalf("an admin invites: %d %v", code, out)
	}
	var inv struct {
		Code   string   `bson:"code"`
		Boards []string `bson:"boardsToBeInvited"`
		Valid  bool     `bson:"valid"`
	}
	if w.db.Collection("invitation_codes").FindOne(ctx, bson.M{"email": "new.person@example.com"}).Decode(&inv) != nil || inv.Code == "" || !inv.Valid || inv.Boards[0] != mine.ID {
		t.Fatalf("#4043: the code, kept lowercase: %+v", inv)
	}
	msgs := sink.messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0], inv.Code) || !strings.Contains(msgs[0], "To: new.person@example.com") ||
		!strings.Contains(msgs[0], "/sign-up") || !strings.Contains(msgs[0], "Subject: ANN (ann ) sent you an invitation") {
		t.Fatalf("the invitation mail: %v", msgs)
	}
	first := inv.Code
	if code, _ := call(ann, `[["new.person@example.com"],["`+mine.ID+`"]]`); code != 200 {
		t.Fatal("a re-invite")
	}
	_ = w.db.Collection("invitation_codes").FindOne(ctx, bson.M{"email": "new.person@example.com"}).Decode(&inv)
	if inv.Code != first || len(sink.messages()) != 2 {
		t.Fatal("a re-invite resends the code that is still valid")
	}
	if code, out := call(ann, `[["bob@example.invalid"],["`+mine.ID+`"]]`); code != 400 || out["error"] != "user-exist" {
		t.Fatalf("negative: an address with an account: %v", out)
	}

	bobc := w.client(t)
	body(t, w.signIn(t, bobc, "bob", "pw"))
	if code, out := call(bobc, `[["x@example.com"],["`+mine.ID+`"]]`); code != 400 || out["error"] != "not-allowed" {
		t.Fatalf("negative: not while registration is open: %v", out)
	}
	_, _ = w.db.Collection("settings").UpdateOne(ctx, bson.M{}, bson.M{"$set": bson.M{"disableRegistration": true, "mailDomainName": "@example.invalid"}})
	if code, out := call(bobc, `[["grant@example.invalid"],["`+victim.ID+`"]]`); code != 400 || out["error"] != "not-allowed" {
		t.Fatalf("negative: a domain-approved inviter cannot grant a foreign private board: %v", out)
	}
	if n, _ := w.db.Collection("invitation_codes").CountDocuments(ctx, bson.M{"email": "grant@example.invalid"}); n != 0 {
		t.Fatal("negative: no code for it")
	}
	var row struct {
		Count int64 `bson:"count"`
	}
	_ = w.db.Collection("eventlog").FindOne(ctx, bson.M{"bleed": "InvitationBoardBleed"}).Decode(&row)
	if row.Count != 1 {
		t.Fatalf("the attempt is recorded: %d", row.Count)
	}
	if code, out := call(bobc, `[["member@example.invalid"],["`+mine.ID+`"]]`); code != 200 {
		t.Fatalf("a domain-approved member invites to their own board: %d %v", code, out)
	}

	sink.ln.Close() // the mail server is gone
	if code, out := call(ann, `[["later@example.com"],["`+mine.ID+`"]]`); code != 400 || out["error"] != "email-fail" {
		t.Fatalf("negative: a failed mail: %v", out)
	}
	if n, _ := w.db.Collection("invitation_codes").CountDocuments(ctx, bson.M{"email": "later@example.com"}); n != 0 {
		t.Fatal("negative: a new code whose mail failed is taken back")
	}
}

// Signing up with an invitation code while registration is closed: only the
// code of that very address, once; the account joins the invited boards.
func TestSignUpWithInvitationCode(t *testing.T) {
	w := newTestWeKan(t)
	admin := w.seedUser(t, "ann", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, admin, "Invited")
	_, _ = w.db.Collection("settings").UpdateOne(ctx, bson.M{}, bson.M{"$set": bson.M{"disableRegistration": true}})
	_, _ = w.db.Collection("invitation_codes").InsertOne(ctx, bson.M{"_id": "invite000000001", "code": "abcdefgh12", "email": "new@example.invalid",
		"boardsToBeInvited": []string{b.ID}, "valid": true, "authorId": admin.ID})
	signUp := func(code, email string) *http.Response {
		res, err := w.client(t).PostForm(w.srv.URL+"/sign-up", url.Values{"username": {"newbie"}, "email": {email},
			"password": {"newbie-password-1"}, "password2": {"newbie-password-1"}, "invitationcode": {code}})
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res
	}
	if _, html := get(t, w.client(t), w.srv.URL+"/sign-up"); !strings.Contains(html, `name="invitationcode"`) {
		t.Fatal("closed registration asks for the invitation code")
	}
	if res := signUp("wrong-code", "new@example.invalid"); res.StatusCode != http.StatusForbidden {
		t.Fatalf("negative: a wrong code: %d", res.StatusCode)
	}
	if res := signUp("abcdefgh12", "other@example.invalid"); res.StatusCode != http.StatusForbidden {
		t.Fatalf("negative: the code of another address: %d", res.StatusCode)
	}
	if n, _ := w.db.Collection("users").CountDocuments(ctx, bson.M{"username": "newbie"}); n != 0 {
		t.Fatal("negative: no account from a refused code")
	}
	var row struct {
		Count int64 `bson:"count"`
	}
	_ = w.db.Collection("eventlog").FindOne(ctx, bson.M{"bleed": "InviteBleed"}).Decode(&row)
	if row.Count != 2 {
		t.Fatalf("each wrong code is recorded as InviteBleed: %d", row.Count)
	}
	if res := signUp("abcdefgh12", "New@Example.invalid"); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("the invited sign-up: %d", res.StatusCode)
	}
	var u User
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"username": "newbie"}).Decode(&u)
	var board Board
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": b.ID}).Decode(&board)
	if !board.MemberOf(u.ID) {
		t.Fatal("the account joins the invited board")
	}
	var inv bson.M
	_ = w.db.Collection("invitation_codes").FindOne(ctx, bson.M{"_id": "invite000000001"}).Decode(&inv)
	if inv["valid"] != false {
		t.Fatal("the code is spent")
	}
	_, _ = w.db.Collection("users").DeleteOne(ctx, bson.M{"_id": u.ID})
	if res := signUp("abcdefgh12", "new@example.invalid"); res.StatusCode != http.StatusForbidden {
		t.Fatalf("negative: a spent code opens nothing: %d", res.StatusCode)
	}
}
