package web

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// fakeProvider is an OAuth2 / OIDC provider: it checks the PKCE verifier against
// the challenge of the authorization request, and answers userinfo with claims.
type fakeProvider struct {
	mu        sync.Mutex
	challenge map[string]string // code -> S256 challenge
	claims    map[string]map[string]any
	nonce     string
	srv       *httptest.Server
}

func newFakeProvider(t *testing.T) *fakeProvider {
	p := &fakeProvider{challenge: map[string]string{}, claims: map[string]map[string]any{}}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		switch r.URL.Path {
		case "/oauth/token":
			_ = r.ParseForm()
			code := r.PostForm.Get("code")
			sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
			if p.challenge[code] == "" || base64.RawURLEncoding.EncodeToString(sum[:]) != p.challenge[code] {
				http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
				return
			}
			payload, _ := json.Marshal(map[string]string{"nonce": p.nonce})
			idt := "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at-" + code, "token_type": "Bearer", "id_token": idt})
		case "/oauth/userinfo":
			c := p.claims[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer at-")]
			if c == nil {
				http.Error(w, "no", http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(c)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(p.srv.Close)
	return p
}

// login runs the whole flow for a browser: Weddy's redirect to the provider,
// the provider's consent (code for claims), and the callback.
func (p *fakeProvider) login(t *testing.T, w *testWeKan, c *http.Client, claims map[string]any) *http.Response {
	t.Helper()
	res, err := c.Get(w.srv.URL + "/oauth2/login")
	if err != nil || res.StatusCode != http.StatusFound {
		t.Fatalf("/oauth2/login: %v %v", err, res)
	}
	auth, _ := url.Parse(res.Header.Get("Location"))
	q := auth.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("state") == "" || q.Get("nonce") == "" ||
		!strings.HasSuffix(q.Get("redirect_uri"), "/_oauth/oidc") {
		t.Fatalf("the authorization request: %s", auth)
	}
	code := randomID()
	p.mu.Lock()
	p.challenge[code], p.claims[code] = q.Get("code_challenge"), claims
	if p.nonce == "" {
		p.nonce = q.Get("nonce")
	}
	p.mu.Unlock()
	res, err = c.Get(w.srv.URL + "/_oauth/oidc?code=" + code + "&state=" + q.Get("state"))
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.nonce = ""
	p.mu.Unlock()
	return res
}

func TestOIDC(t *testing.T) {
	w := newTestWeKan(t)
	p := newFakeProvider(t)
	cfg := OIDCFromEnv(func(k string) string {
		return map[string]string{"OAUTH2_ENABLED": "true", "OAUTH2_CLIENT_ID": "weddy", "OAUTH2_SECRET": "s",
			"OAUTH2_SERVER_URL": p.srv.URL}[k]
	})
	if cfg == nil || cfg.TokenURL != p.srv.URL+"/oauth/token" || !cfg.AutoRegistration || cfg.MergeExistingUsers {
		t.Fatalf("OIDCFromEnv: %+v", cfg)
	}
	if OIDCFromEnv(func(string) string { return "" }) != nil {
		t.Fatal("negative: OAUTH2_ENABLED unset is no OIDC")
	}
	w.s.OIDC = cfg
	existing := w.seedUser(t, "zoe", "pw")
	_, _ = w.db.Collection("users").UpdateOne(context.Background(), bson.M{"_id": existing.ID}, bson.M{"$set": bson.M{"emails.0.address": "zoe@example.invalid"}})

	// A new user: registered, signed in.
	c := w.client(t)
	res := p.login(t, w, c, map[string]any{"sub": "idp-1", "preferred_username": "anna", "email": "anna@example.invalid", "name": "Anna A", "email_verified": true})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("a new user signs in: %d %s", res.StatusCode, body(t, res))
	}
	var anna User
	_ = w.db.Collection("users").FindOne(context.Background(), bson.M{"services.oidc.id": "idp-1"}).Decode(&anna)
	if anna.Username != "anna" || anna.Profile.Fullname != "Anna A" || anna.IsAdmin {
		t.Fatalf("the new account: %+v", anna)
	}
	if r2, _ := get(t, c, w.srv.URL+"/"); r2.StatusCode != 200 {
		t.Fatal("signed in")
	}
	// The same user again: the same account.
	if res := p.login(t, w, w.client(t), map[string]any{"sub": "idp-1", "preferred_username": "anna2"}); res.StatusCode != http.StatusSeeOther {
		t.Fatal("a returning user")
	}
	if n, _ := w.db.Collection("users").CountDocuments(context.Background(), bson.M{"services.oidc.id": "idp-1"}); n != 1 {
		t.Fatal("negative: a returning user got a second account")
	}

	// Negative: an e-mail address of another account, linking off.
	zoe := map[string]any{"sub": "idp-2", "preferred_username": "z", "email": "zoe@example.invalid", "email_verified": true}
	if res := p.login(t, w, w.client(t), zoe); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("negative: taking over zoe's account by e-mail: %d", res.StatusCode)
	}
	cfg.MergeExistingUsers = true
	zoe["email_verified"] = false
	if res := p.login(t, w, w.client(t), zoe); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("negative: linking with an unverified address: %d", res.StatusCode)
	}
	zoe["email_verified"] = true
	if res := p.login(t, w, w.client(t), zoe); res.StatusCode != http.StatusSeeOther {
		t.Fatal("linking with OAUTH2_MERGE_EXISTING_USERS and a verified address")
	}
	var z User
	_ = w.db.Collection("users").FindOne(context.Background(), bson.M{"services.oidc.id": "idp-2"}).Decode(&z)
	if z.ID != existing.ID {
		t.Fatal("linked to zoe's own account")
	}

	// Negative: automatic registration off.
	cfg.AutoRegistration = false
	if res := p.login(t, w, w.client(t), map[string]any{"sub": "idp-3", "preferred_username": "nobody"}); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("negative: a new user with registration off: %d", res.StatusCode)
	}
	cfg.AutoRegistration = true

	// Negative: a disabled account.
	_, _ = w.db.Collection("users").UpdateOne(context.Background(), bson.M{"_id": anna.ID}, bson.M{"$set": bson.M{"loginDisabled": true}})
	if res := p.login(t, w, w.client(t), map[string]any{"sub": "idp-1"}); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("negative: a disabled account: %d", res.StatusCode)
	}

	// Negative: a callback this browser did not start, or with another state.
	if res, _ := w.client(t).Get(w.srv.URL + "/_oauth/oidc?code=x&state=y"); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("negative: no state cookie: %d", res.StatusCode)
	}
	bc := w.client(t)
	r1, _ := bc.Get(w.srv.URL + "/oauth2/login")
	r1.Body.Close()
	if res, _ := bc.Get(w.srv.URL + "/_oauth/oidc?code=x&state=forged"); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("negative: another state: %d", res.StatusCode)
	}

	// Negative: an id token with another nonce.
	p.nonce = "not-this-browsers"
	if res := p.login(t, w, w.client(t), map[string]any{"sub": "idp-4", "preferred_username": "nina"}); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("negative: another nonce: %d", res.StatusCode)
	}

	// Without OIDC configured, nothing answers.
	w.s.OIDC = nil
	if res, _ := w.client(t).Get(w.srv.URL + "/oauth2/login"); res.StatusCode != http.StatusNotFound {
		t.Fatal("negative: OIDC off")
	}
}
