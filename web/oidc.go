package web

// Sign in with OAuth2 / OpenID Connect, configured with Node.js WeKan's own
// environment variables (OAUTH2_*), so a WeKan's settings work in Weddy as they
// are, and at Meteor's redirect address /_oauth/oidc. As Meteor's oidc package
// does, the user comes from the provider's userinfo endpoint, with the access
// token of an authorization-code exchange - here with PKCE, a state and a nonce.

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"golang.org/x/oauth2"
)

// OIDCConfig is OAUTH2_* of Node.js WeKan.
type OIDCConfig struct {
	ClientID, Secret      string
	AuthURL, TokenURL     string
	UserinfoURL           string
	Scopes                []string
	IDMap, UsernameMap    string
	FullnameMap, EmailMap string
	AutoRegistration      bool // OAUTH2_AUTO_REGISTRATION, default true
	MergeExistingUsers    bool // OAUTH2_MERGE_EXISTING_USERS, default false
	HTTPClient            *http.Client
}

// OIDCFromEnv reads WeKan's OAUTH2_* variables; nil unless OAUTH2_ENABLED=true
// and the client, server and endpoints are set.
func OIDCFromEnv(get func(string) string) *OIDCConfig {
	if get == nil {
		get = os.Getenv
	}
	if strings.TrimSpace(get("OAUTH2_ENABLED")) != "true" {
		return nil
	}
	server := strings.TrimRight(strings.TrimSpace(get("OAUTH2_SERVER_URL")), "/")
	abs := func(p, def string) string {
		p = strings.TrimSpace(p)
		if p == "" {
			p = def
		}
		if strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") {
			return p
		}
		return server + "/" + strings.TrimLeft(p, "/")
	}
	or := func(v, def string) string {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
		return def
	}
	c := &OIDCConfig{
		ClientID: strings.TrimSpace(get("OAUTH2_CLIENT_ID")), Secret: get("OAUTH2_SECRET"),
		AuthURL: abs(get("OAUTH2_AUTH_ENDPOINT"), "/oauth/authorize"), TokenURL: abs(get("OAUTH2_TOKEN_ENDPOINT"), "/oauth/token"),
		UserinfoURL: abs(get("OAUTH2_USERINFO_ENDPOINT"), "/oauth/userinfo"),
		Scopes:      strings.Fields(or(get("OAUTH2_REQUEST_PERMISSIONS"), "openid profile email")),
		IDMap:       or(get("OAUTH2_ID_MAP"), "sub"), UsernameMap: or(get("OAUTH2_USERNAME_MAP"), "preferred_username"),
		FullnameMap: or(get("OAUTH2_FULLNAME_MAP"), "name"), EmailMap: or(get("OAUTH2_EMAIL_MAP"), "email"),
		AutoRegistration:   strings.TrimSpace(get("OAUTH2_AUTO_REGISTRATION")) != "false",
		MergeExistingUsers: strings.TrimSpace(get("OAUTH2_MERGE_EXISTING_USERS")) == "true",
	}
	if c.ClientID == "" || server == "" {
		return nil
	}
	return c
}

const oidcCookie = "weddy_oidc"

func (c *OIDCConfig) oauth2(r *http.Request) *oauth2.Config {
	scheme := "https"
	if r.TLS == nil && (strings.HasPrefix(r.Host, "localhost") || strings.HasPrefix(r.Host, "127.0.0.1")) {
		scheme = "http"
	}
	return &oauth2.Config{ClientID: c.ClientID, ClientSecret: c.Secret, Scopes: c.Scopes,
		Endpoint:    oauth2.Endpoint{AuthURL: c.AuthURL, TokenURL: c.TokenURL},
		RedirectURL: scheme + "://" + r.Host + "/_oauth/oidc"}
}

func (s *Server) oidcRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /oauth2/login", s.oidcStart)
	m.HandleFunc("GET /_oauth/oidc", s.oidcCallback)
}

// oidcStart sends the browser to the provider, with a state, a nonce and a PKCE
// verifier kept in a short-lived HttpOnly cookie.
func (s *Server) oidcStart(w http.ResponseWriter, r *http.Request) {
	if s.OIDC == nil {
		http.NotFound(w, r)
		return
	}
	state, nonce, verifier := randomString(tokenAlphabet, 32), randomString(tokenAlphabet, 32), oauth2.GenerateVerifier()
	http.SetCookie(w, &http.Cookie{Name: oidcCookie, Value: state + "." + nonce + "." + verifier, Path: "/_oauth/",
		MaxAge: 600, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, s.OIDC.oauth2(r).AuthCodeURL(state, oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("nonce", nonce)), http.StatusFound)
}

func (s *Server) oidcFail(w http.ResponseWriter, r *http.Request, key string) {
	s.render(w, r, "signin", http.StatusUnauthorized, map[string]any{"Title": tr(languageOf(r, nil).Tag, "log-in"), "Error": key})
}

func (s *Server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	if s.OIDC == nil {
		http.NotFound(w, r)
		return
	}
	ck, err := r.Cookie(oidcCookie)
	http.SetCookie(w, &http.Cookie{Name: oidcCookie, Value: "", Path: "/_oauth/", MaxAge: -1, HttpOnly: true, Secure: true})
	parts := []string{}
	if err == nil {
		parts = strings.Split(ck.Value, ".")
	}
	if len(parts) != 3 || subtle.ConstantTimeCompare([]byte(parts[0]), []byte(r.URL.Query().Get("state"))) != 1 {
		s.oidcFail(w, r, msg("error-ldap-login")) // a callback this browser did not start
		return
	}
	ctx := r.Context()
	if s.OIDC.HTTPClient != nil {
		ctx = context.WithValue(ctx, oauth2.HTTPClient, s.OIDC.HTTPClient)
	}
	tok, err := s.OIDC.oauth2(r).Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(parts[2]))
	if err != nil {
		s.log.Warn("oidc: code exchange", "error", err)
		s.oidcFail(w, r, msg("error-ldap-login"))
		return
	}
	if idt, ok := tok.Extra("id_token").(string); ok && idt != "" {
		if n := jwtClaim(idt, "nonce"); n != "" && subtle.ConstantTimeCompare([]byte(n), []byte(parts[1])) != 1 {
			s.oidcFail(w, r, msg("error-ldap-login"))
			return
		}
	}
	claims, err := s.oidcUserinfo(ctx, tok)
	if err != nil {
		s.log.Warn("oidc: userinfo", "error", err)
		s.oidcFail(w, r, msg("error-ldap-login"))
		return
	}
	u, err := s.oidcUser(r.Context(), claims)
	if err != nil {
		s.log.Warn("oidc: sign in refused", "error", err)
		s.oidcFail(w, r, msg("error-ldap-login"))
		return
	}
	token, expires, err := s.newLoginToken(r.Context(), u.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	setSessionCookie(w, token, expires)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// jwtClaim is a string claim of a JWT's payload, unverified - only to compare
// the nonce this browser sent; the user comes from userinfo.
func jwtClaim(jwt, name string) string {
	p := strings.Split(jwt, ".")
	if len(p) != 3 {
		return ""
	}
	b, err := base64.RawURLEncoding.DecodeString(p[1])
	if err != nil {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return ""
	}
	v, _ := m[name].(string)
	return v
}

func (s *Server) oidcUserinfo(ctx context.Context, tok *oauth2.Token) (map[string]any, error) {
	client := s.OIDC.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", s.OIDC.UserinfoURL, nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Accept", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("userinfo answered %d", res.StatusCode)
	}
	var claims map[string]any
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&claims); err != nil {
		return nil, err
	}
	return claims, nil
}

func claimString(claims map[string]any, name string) string {
	switch v := claims[name].(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%f", v), "0"), ".")
	}
	return ""
}

// oidcUser is the account of the provider's user: the one already linked to its
// id, else - with OAUTH2_AUTO_REGISTRATION not false - a new one. An existing
// account with the same e-mail address is linked only with
// OAUTH2_MERGE_EXISTING_USERS=true and a verified address, as in WeKan.
func (s *Server) oidcUser(ctx context.Context, claims map[string]any) (*User, error) {
	c := s.OIDC
	id := claimString(claims, c.IDMap)
	if id == "" || len(id) > 256 {
		return nil, errors.New("no id from the provider")
	}
	username := claimString(claims, c.UsernameMap)
	email := strings.ToLower(claimString(claims, c.EmailMap))
	fullname := claimString(claims, c.FullnameMap)
	verified, _ := claims["email_verified"].(bool)
	var u User
	if err := s.db.Collection("users").FindOne(ctx, bson.M{"services.oidc.id": eq(id)}).Decode(&u); err == nil {
		if u.LoginDisabled {
			return nil, errors.New("login disabled")
		}
		return &u, nil
	}
	if email != "" && email_.MatchString(email) {
		if err := s.db.Collection("users").FindOne(ctx, bson.M{"emails.address": eq(email)}).Decode(&u); err == nil {
			if !c.MergeExistingUsers || !verified {
				return nil, errors.New("an account has this e-mail address, and linking is off or the address is unverified")
			}
			if u.LoginDisabled {
				return nil, errors.New("login disabled")
			}
			_, err := s.db.Collection("users").UpdateOne(ctx, bson.M{"_id": eq(u.ID)},
				bson.M{"$set": bson.M{"services.oidc": bson.M{"id": id, "email": email, "username": username, "fullname": fullname}}})
			return &u, err
		}
	}
	if !c.AutoRegistration {
		return nil, errors.New("automatic registration is off (OAUTH2_AUTO_REGISTRATION=false)")
	}
	if !usernamePattern.MatchString(username) {
		username = strings.Split(email, "@")[0]
	}
	if !usernamePattern.MatchString(username) {
		username = "user"
	}
	base := username
	for i := 2; ; i++ {
		if n, _ := s.db.Collection("users").CountDocuments(ctx, bson.M{"username": eq(username)}); n == 0 {
			break
		}
		username = fmt.Sprintf("%s%d", base, i)
	}
	if fullname == "" {
		fullname = username
	}
	open, first := s.registrationOpen(ctx)
	_ = open // the provider decides who may sign in; WeKan's registration setting is for the form
	newID := randomID()
	doc := bson.M{"_id": newID, "username": username, "createdAt": time.Now().UTC(), "isAdmin": first, // inserted, not a filter
		"profile":              bson.M{"fullname": fullname, "boardView": "board-view-swimlanes"},
		"services":             bson.M{"oidc": bson.M{"id": id, "email": email, "username": username, "fullname": fullname}}, // inserted, not a filter
		"authenticationMethod": "oauth2"}
	if email != "" && email_.MatchString(email) {
		doc["emails"] = []bson.M{{"address": email, "verified": verified}}
	}
	if _, err := s.db.Collection("users").InsertOne(ctx, doc); err != nil {
		return nil, err
	}
	return &User{ID: newID, Username: username, IsAdmin: first}, nil
}
