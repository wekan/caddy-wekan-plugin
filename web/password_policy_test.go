package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestPasswordPolicyFrom(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if p := passwordPolicyFrom(nil, env(nil)); p != (passwordPolicy{MinLength: 8}) {
		t.Errorf("the default is 8, reported only: %+v", p)
	}
	p := passwordPolicyFrom(nil, env(map[string]string{"PASSWORD_MIN_LENGTH": " 12 ", "PASSWORD_MIN_LENGTH_ENFORCE": "true", "PASSWORD_CHECK_PWNED": "true"}))
	if p != (passwordPolicy{MinLength: 12, Enforce: true, CheckPwned: true}) {
		t.Errorf("from the environment: %+v", p)
	}
	for _, bad := range []string{"0", "-3", "7.5", "eight", ""} {
		if p := passwordPolicyFrom(nil, env(map[string]string{"PASSWORD_MIN_LENGTH": bad})); p.MinLength != 8 {
			t.Errorf("negative: %q keeps the default: %d", bad, p.MinLength)
		}
	}
	if p := passwordPolicyFrom(nil, env(map[string]string{"PASSWORD_MIN_LENGTH": "999"})); p.MinLength != 256 {
		t.Errorf("at most 256: %d", p.MinLength)
	}
	// Admin Panel's stored override wins over the environment, a false too.
	p = passwordPolicyFrom(map[string]any{"passwordMinLength": int32(10), "passwordMinLengthEnforce": false},
		env(map[string]string{"PASSWORD_MIN_LENGTH": "12", "PASSWORD_MIN_LENGTH_ENFORCE": "true"}))
	if p.MinLength != 10 || p.Enforce {
		t.Errorf("the override: %+v", p)
	}
}

func TestPasswordPolicyProblem(t *testing.T) {
	reported := passwordPolicy{MinLength: 8}
	enforced := passwordPolicy{MinLength: 8, Enforce: true}
	if p := reported.problem("short", "bob", "bob@example.invalid"); p != "" {
		t.Errorf("not enforced, a short password is set: %q", p)
	}
	if p := reported.problem("bob", "bob", ""); p != "" {
		t.Errorf("not enforced, nothing but the cap: %q", p)
	}
	if p := reported.problem(strings.Repeat("x", 513), "bob", ""); p != "password-too-long" {
		t.Errorf("the cap always holds: %q", p)
	}
	for pw, want := range map[string]string{"short": "password-too-short", "seven77": "password-too-short", "eight888": "",
		"Bobbybob": "password-same-as-username", "bobby@example.invalid": "password-same-as-username", "BOBBY123": ""} {
		if p := enforced.problem(pw, "bobbybob", "bobby@example.invalid"); p != want {
			t.Errorf("enforced %q: %q, want %q", pw, p, want)
		}
	}
	// A name under three characters is no reason to refuse.
	if p := (passwordPolicy{MinLength: 2, Enforce: true}).problem("bo", "bo", ""); p != "" {
		t.Errorf("a two-letter name: %q", p)
	}
	// Characters, not bytes.
	if (passwordPolicy{MinLength: 4, Enforce: true}).passwordTooShort("äöüß") {
		t.Error("four letters are four")
	}
}

// Sign-up under WeKan's default registers a short password; the enforced
// policy, set in Admin Panel's login options, refuses it; getPasswordPolicy
// answers a signed-out page.
func TestPasswordPolicySignUp(t *testing.T) {
	t.Setenv("PASSWORD_MIN_LENGTH_ENFORCE", "")
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "first", "pw")
	policy := func() passwordPolicy {
		req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/getPasswordPolicy", strings.NewReader(`[]`))
		req.Header.Set("Content-Type", "application/json")
		res, err := w.client(t).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out struct{ Result passwordPolicy }
		_ = json.NewDecoder(res.Body).Decode(&out)
		return out.Result
	}
	if p := policy(); p != (passwordPolicy{MinLength: 8}) {
		t.Errorf("signed out, the default policy: %+v", p)
	}
	// The form asks only its own 6, as WeKan's (config/accounts.js) - changed
	// from no minimum on 2026-11-13, when WeKan's password-length-report spec
	// pinned "not enforced: the form's own 6"; a 7-character password still
	// registers.
	if _, html := get(t, w.client(t), w.srv.URL+"/sign-up"); !strings.Contains(html, `name="password" autocomplete="new-password" minlength="6"`) {
		t.Error("not enforced, the sign-up form asks its own 6")
	}
	if res := signUp(t, w, w.client(t), "seven", "seven@example.invalid", "seven77"); res.StatusCode != http.StatusSeeOther {
		t.Errorf("not enforced, a 7-character password registers: %d", res.StatusCode)
	}
	_, _ = w.db.Collection("settings").UpdateOne(ctx, bson.M{}, bson.M{"$set": bson.M{"loginOptions.passwordMinLength": 10, "loginOptions.passwordMinLengthEnforce": true}})
	if p := policy(); p != (passwordPolicy{MinLength: 10, Enforce: true}) {
		t.Errorf("the override, signed out: %+v", p)
	}
	if _, html := get(t, w.client(t), w.srv.URL+"/sign-up"); !strings.Contains(html, `name="password" autocomplete="new-password" minlength="10"`) {
		t.Error("enforced at 10, the sign-up form asks 10")
	}
	if res := signUp(t, w, w.client(t), "nine", "nine@example.invalid", "nine99999"); res.StatusCode != http.StatusBadRequest {
		t.Errorf("negative: enforced at 10, a 9-character password: %d", res.StatusCode)
	}
	if res := signUp(t, w, w.client(t), "ten", "ten@example.invalid", "ten1010101"); res.StatusCode != http.StatusSeeOther {
		t.Errorf("enforced at 10, a 10-character password registers: %d", res.StatusCode)
	}
	if n, _ := w.db.Collection("users").CountDocuments(ctx, bson.M{"username": "nine"}); n != 0 {
		t.Error("negative: the refused account exists")
	}
	// Negative: a method that is not public still needs a login.
	req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/setLanguage", strings.NewReader(`["fi"]`))
	req.Header.Set("Content-Type", "application/json")
	if res, err := w.client(t).Do(req); err != nil || res.StatusCode != http.StatusUnauthorized {
		t.Errorf("negative: setLanguage signed out: %v %v", err, res.StatusCode)
	}
}

// /_session says whether a page's session still signs it in, and nothing
// more: 204 while it does, 401 once it ended - its password changed by an
// admin's setPassword, which signs every session out.
func TestSessionEndpointAndSetPassword(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "admin", "pw")
	bob := w.seedUser(t, "bob", "pw")
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"username": "admin"}, bson.M{"$set": bson.M{"isAdmin": true}})
	ac, bc := w.client(t), w.client(t)
	body(t, w.signIn(t, ac, "admin", "pw"))
	body(t, w.signIn(t, bc, "bob", "pw"))
	status := func(c *http.Client) int {
		res, err := c.Get(w.srv.URL + "/_session")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if b, _ := io.ReadAll(res.Body); len(b) != 0 {
			t.Fatal("no body")
		}
		return res.StatusCode
	}
	if status(bc) != http.StatusNoContent || status(w.client(t)) != http.StatusUnauthorized {
		t.Fatal("204 signed in, 401 not")
	}
	// Negative: only a site admin sets another's password.
	if _, out := methodCall(t, w, bc, "setPassword", `["Another-long-1","`+bob.ID+`"]`); out["error"] != "not-authorized" {
		t.Fatalf("not an admin: %v", out)
	}
	if _, out := methodCall(t, w, ac, "setPassword", `["New-long-pass-1","`+bob.ID+`"]`); out["error"] != nil {
		t.Fatalf("the admin's: %v", out)
	}
	if status(bc) != http.StatusUnauthorized {
		t.Fatal("bob's session ended")
	}
	if status(ac) != http.StatusNoContent {
		t.Fatal("the admin's own is not")
	}
	if res := w.signIn(t, w.client(t), "bob", "New-long-pass-1"); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("the new password signs in: %d", res.StatusCode)
	}
}
