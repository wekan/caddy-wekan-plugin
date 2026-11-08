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

// #1426: a verification link verifies its address and signs in; a wrong token
// does neither.
func TestVerifyEmailLink(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	ctx := context.Background()
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": u.ID}, bson.M{"$set": bson.M{"emails.0.verified": false,
		"services.email.verificationTokens": bson.A{bson.M{"token": "verify-token-1", "address": "ann@example.invalid", "when": time.Now()}}}})
	verified := func() bool {
		var d User
		_ = w.db.Collection("users").FindOne(ctx, bson.M{"_id": u.ID}).Decode(&d)
		return d.Emails[0].Verified
	}
	c := w.client(t)
	if res, _ := get(t, c, w.srv.URL+"/verify-email/not-a-real-token"); res.StatusCode != http.StatusSeeOther || verified() {
		t.Fatal("negative: a wrong token verifies nothing")
	}
	if res, _ := get(t, c, w.srv.URL+"/"); res.StatusCode != http.StatusSeeOther {
		t.Fatal("negative: and signs in nobody")
	}
	if res, _ := get(t, c, w.srv.URL+"/verify-email/verify-token-1"); res.StatusCode != http.StatusSeeOther || !verified() {
		t.Fatal("the link verifies the address")
	}
	if res, _ := get(t, c, w.srv.URL+"/"); res.StatusCode != 200 {
		t.Fatalf("and signs the user in: %d", res.StatusCode)
	}
	var d struct {
		Services struct {
			Email struct {
				Tokens []bson.M `bson:"verificationTokens"`
			} `bson:"email"`
		} `bson:"services"`
	}
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"_id": u.ID}).Decode(&d)
	if len(d.Services.Email.Tokens) != 0 {
		t.Fatalf("the token is spent: %v", d.Services.Email.Tokens)
	}
}

// A forgotten password: a reset link by mail, the same answer for an unknown
// address, one mail a minute, a new password by the sign-up rules, every
// other session signed out.
func TestForgotAndResetPassword(t *testing.T) {
	sink := newSMTPSink(t)
	t.Setenv("MAIL_URL", "smtp://"+sink.ln.Addr().String())
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	ctx := context.Background()
	old := w.client(t)
	body(t, w.signIn(t, old, "ann", "pw"))
	forgot := func(email string) string {
		res, err := w.client(t).PostForm(w.srv.URL+"/forgot-password", url.Values{"email": {email}})
		if err != nil {
			t.Fatal(err)
		}
		return body(t, res)
	}
	unknown, known := forgot("nobody@example.invalid"), forgot("Ann@Example.invalid")
	if !strings.Contains(unknown, "js-email-sent") || !strings.Contains(known, "js-email-sent") || len(sink.messages()) != 1 {
		t.Fatalf("one mail, and the same answer for an unknown address: %d", len(sink.messages()))
	}
	forgot("ann@example.invalid")
	if len(sink.messages()) != 1 {
		t.Fatal("negative: one reset mail a minute")
	}
	mailText := strings.ReplaceAll(sink.messages()[0], "=\r\n", "")
	i := strings.Index(mailText, "/reset-password/")
	if i < 0 {
		t.Fatalf("the link: %s", mailText)
	}
	token := strings.Fields(mailText[i+len("/reset-password/"):])[0]
	resetURL := w.srv.URL + "/reset-password/" + token
	if res, _ := get(t, w.client(t), resetURL); res.StatusCode != 200 {
		t.Fatalf("the reset page: %d", res.StatusCode)
	}
	nc := w.client(t)
	if res, _ := nc.PostForm(resetURL, url.Values{"password": {"short"}, "password2": {"short"}}); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("negative: the sign-up rules: %d", res.StatusCode)
	}
	if res, _ := nc.PostForm(resetURL, url.Values{"password": {"a-new-password-1"}, "password2": {"a-new-password-1"}}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("reset: %d", res.StatusCode)
	}
	if res, _ := get(t, nc, w.srv.URL+"/"); res.StatusCode != 200 {
		t.Fatal("signed in with the new password")
	}
	if res, _ := get(t, old, w.srv.URL+"/"); res.StatusCode != http.StatusSeeOther {
		t.Fatal("negative: the other sessions are signed out")
	}
	if res := w.signIn(t, w.client(t), "ann", "pw"); res.StatusCode != http.StatusUnauthorized {
		t.Fatal("negative: the old password is gone")
	}
	if res := w.signIn(t, w.client(t), "ann", "a-new-password-1"); res.StatusCode != http.StatusSeeOther {
		t.Fatal("the new password signs in")
	}
	if res, _ := w.client(t).PostForm(resetURL, url.Values{"password": {"another-pass-2"}, "password2": {"another-pass-2"}}); res.StatusCode != http.StatusNotFound {
		t.Fatalf("negative: a spent token: %d", res.StatusCode)
	}
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": u.ID}, bson.M{"$set": bson.M{"services.password.reset": bson.M{
		"token": "expired-token-1", "email": "ann@example.invalid", "when": time.Now().Add(-4 * 24 * time.Hour)}}})
	if res, _ := get(t, w.client(t), w.srv.URL+"/reset-password/expired-token-1"); res.StatusCode != http.StatusNotFound {
		t.Fatalf("negative: an expired token: %d", res.StatusCode)
	}
}

// Invitations in the sidebar: accept or decline, only with a real invitation;
// a client cannot write one itself (InviteProfileBleed).
func TestInvitationAcceptDeclineAndForgery(t *testing.T) {
	w := newTestWeKan(t)
	owner := w.seedUser(t, "ann", "pw")
	inv := w.seedUser(t, "bob", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, owner, "Invited")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$push": bson.M{"members": bson.M{"userId": inv.ID, "isActive": false}}})
	call := func(cl *http.Client, name, args string) (int, map[string]any) {
		req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/"+url.PathEscape(name), strings.NewReader(args))
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
	active := func() bool {
		var x Board
		_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": b.ID}).Decode(&x)
		return x.MemberOf(inv.ID)
	}
	bob := w.client(t)
	body(t, w.signIn(t, bob, "bob", "pw"))
	// A removed member forges an invitation, then accepts it.
	if code, out := call(bob, "/users/update", `[{"_id":"`+inv.ID+`"},{"$addToSet":{"profile.invitedBoards":"`+b.ID+`"}}]`); code != 400 || out["error"] != float64(403) {
		t.Fatalf("negative: a forged invitation: %d %v", code, out)
	}
	var row struct {
		Count int64 `bson:"count"`
	}
	_ = w.db.Collection("eventlog").FindOne(ctx, bson.M{"bleed": "InviteProfileBleed"}).Decode(&row)
	if row.Count != 1 {
		t.Fatalf("the forgery is recorded: %d", row.Count)
	}
	if _, out := call(bob, "acceptInvite", `["`+b.ID+`"]`); out["result"] != false || active() {
		t.Fatalf("negative: no invitation, nothing accepted: %v", out)
	}
	for _, mod := range []string{`{"$set":{"isAdmin":true}}`, `{"$set":{"profile":{"invitedBoards":["x"]}}}`, `{"$set":{"services.password.bcrypt":"x"}}`, `{"$inc":{"profile.x":1}}`} {
		if code, _ := call(bob, "/users/update", `[{"_id":"`+inv.ID+`"},`+mod+`]`); code != 400 {
			t.Errorf("negative: %s", mod)
		}
	}
	if code, _ := call(bob, "/users/update", `[{"_id":"`+owner.ID+`"},{"$set":{"profile.fullname":"x"}}]`); code != 400 {
		t.Error("negative: another user's document")
	}
	if code, _ := call(bob, "/users/update", `[{"_id":"`+inv.ID+`"},{"$set":{"profile.fullname":"Bob B"}}]`); code != 200 {
		t.Fatal("a user's own profile")
	}
	// A real invitation: shown in the sidebar, accepted.
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": inv.ID}, bson.M{"$set": bson.M{"profile.invitedBoards": bson.A{b.ID}}})
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"members": bson.A{
		bson.M{"userId": owner.ID, "isActive": true, "isAdmin": true}, bson.M{"userId": inv.ID, "isActive": true}}}})
	if _, html := get(t, bob, w.srv.URL+"/b/"+b.ID+"/"+b.Slug); !strings.Contains(html, "js-member-invite-accept") || !strings.Contains(html, "js-member-invite-decline") {
		t.Fatal("the sidebar shows the invitation")
	}
	if _, out := call(bob, "acceptInvite", `["`+b.ID+`"]`); out["result"] != true || !active() {
		t.Fatalf("accepted: %v", out)
	}
	var u User
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"_id": inv.ID}).Decode(&u)
	if len(u.Profile.InvitedBoards) != 0 {
		t.Fatal("the invitation is spent")
	}
	// Declined: the invitation goes and the member entry is made inactive.
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": inv.ID}, bson.M{"$set": bson.M{"profile.invitedBoards": bson.A{b.ID}}})
	if _, out := call(bob, "quitBoard", `["`+b.ID+`"]`); out["result"] != true || active() {
		t.Fatalf("declined: %v", out)
	}
	if _, out := call(bob, "acceptInvite", `["`+b.ID+`"]`); out["result"] != false || active() {
		t.Fatal("negative: a declined invitation cannot be accepted later")
	}
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	call(ann, "quitBoard", `["`+b.ID+`"]`)
	var x Board
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": b.ID}).Decode(&x)
	if !x.MemberOf(owner.ID) {
		t.Fatal("negative: the last admin stays")
	}
	_ = u
}

// #2445: an account with no address saves its first one from Edit Profile -
// only while Admin Panel allows it, and only its own.
func TestProfileEmail(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	other := w.seedUser(t, "bob", "pw")
	ctx := context.Background()
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": u.ID}, bson.M{"$unset": bson.M{"emails": ""}})
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	_, html := get(t, ann, w.srv.URL+"/")
	if i := strings.Index(html, "js-profile-email"); !strings.Contains(html, "js-edit-profile") || i < 0 || !strings.Contains(html[i:i+120], "readonly") {
		t.Fatal("Edit Profile, its address read-only while Admin Panel does not allow a change")
	}
	save := func(email string) {
		res, err := ann.PostForm(w.srv.URL+"/profile", url.Values{"fullname": {"Ann"}, "initials": {"A"}, "email": {email}})
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
	}
	first := func() string {
		var d User
		_ = w.db.Collection("users").FindOne(ctx, bson.M{"_id": u.ID}).Decode(&d)
		if len(d.Emails) == 0 {
			return ""
		}
		return d.Emails[0].Address
	}
	save("first@example.invalid")
	if first() != "" {
		t.Fatal("negative: not while Admin Panel does not allow it")
	}
	// It exists from the start, as WeKan makes it, so an update turns it on.
	if res, _ := w.db.Collection("accountSettings").UpdateOne(ctx, bson.M{"_id": "accounts-allowEmailChange"}, bson.M{"$set": bson.M{"booleanValue": true}}); res.MatchedCount != 1 {
		t.Fatal("accounts-allowEmailChange exists from the start")
	}
	if _, html := get(t, ann, w.srv.URL+"/"); !strings.Contains(html, `name="email"`) {
		t.Fatal("editable when allowed")
	}
	save("first@example.invalid")
	var d User
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"_id": u.ID}).Decode(&d)
	if first() != "first@example.invalid" || d.Emails[0].Verified {
		t.Fatalf("the first address, unverified: %+v", d.Emails)
	}
	call := func(args string) map[string]any {
		req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/setEmail", strings.NewReader(args))
		req.Header.Set("Content-Type", "application/json")
		res, err := ann.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		return out
	}
	if out := call(`["cross@example.invalid","` + other.ID + `"]`); out["error"] != "not-authorized" {
		t.Fatalf("negative: another user's address: %v", out)
	}
	if out := call(`["bob@example.invalid","` + u.ID + `"]`); out["error"] != "email-already-taken" {
		t.Fatalf("negative: somebody else's address: %v", out)
	}
	_, _ = w.db.Collection("accountSettings").UpdateOne(ctx, bson.M{"_id": "accounts-allowEmailChange"}, bson.M{"$set": bson.M{"booleanValue": false}})
	if out := call(`["off@example.invalid","` + u.ID + `"]`); out["error"] != "not-authorized" {
		t.Fatalf("negative: not once Admin Panel turns it off: %v", out)
	}
}
