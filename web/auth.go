package web

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/bcrypt"
)

// Meteor's accounts-password: the password is first SHA-256'd to a hex digest,
// and the DIGEST is what bcrypt or argon2 hashed (services.password.bcrypt or
// .argon2). Checking it the same way is what lets every existing WeKan user sign
// in here with their password.
//
// SHA-256 is not how the password is stored - that is bcrypt (cost 10) or argon2
// of this digest, both slow and salted. It is Meteor's pre-hash, the input those
// were given; without it no existing password would match. (Code scanning alert
// #9 reads it as the storage hash: Hall-of-Fame.md in the wiki says why not.)
func passwordDigest(password string) string {
	sum := sha256.Sum256([]byte(password))
	return hex.EncodeToString(sum[:])
}

var errBadPassword = errors.New("incorrect username or password")

// CheckPassword checks a password against the user's stored hash.
func CheckPassword(u *User, password string) error {
	digest := passwordDigest(password)
	switch {
	case u.Services.Password.Bcrypt != "":
		if bcrypt.CompareHashAndPassword([]byte(u.Services.Password.Bcrypt), []byte(digest)) == nil {
			return nil
		}
	case u.Services.Password.Argon2 != "":
		if ok, err := checkArgon2(u.Services.Password.Argon2, digest); err == nil && ok {
			return nil
		}
	}
	return errBadPassword
}

// checkArgon2 checks a PHC string as node-argon2 writes it:
// $argon2id$v=19$m=65536,t=3,p=4$<salt>$<hash>, base64 without padding.
func checkArgon2(phc, digest string) (bool, error) {
	parts := strings.Split(phc, "$")
	if len(parts) != 6 {
		return false, fmt.Errorf("argon2: %d parts", len(parts))
	}
	// Each parameter is parsed at the width argon2 takes it, so nothing is cut
	// short by a conversion, and capped, so a hash in the database cannot make a
	// sign-in take unbounded memory or time.
	var m, t uint32
	var p uint8
	for _, kv := range strings.Split(parts[3], ",") {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "m", "t":
			n, err := strconv.ParseUint(v, 10, 32)
			if err != nil {
				return false, err
			}
			if k == "m" {
				m = uint32(n)
			} else {
				t = uint32(n)
			}
		case "p":
			n, err := strconv.ParseUint(v, 10, 8)
			if err != nil {
				return false, err
			}
			p = uint8(n)
		}
	}
	if m == 0 || m > argon2MaxMemory || t == 0 || t > argon2MaxTime || p == 0 {
		return false, fmt.Errorf("argon2: parameters m=%d t=%d p=%d out of range", m, t, p)
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, err
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, err
	}
	if len(want) < 16 || len(want) > 128 {
		return false, fmt.Errorf("argon2: a %d-byte hash", len(want))
	}
	var got []byte
	switch parts[1] {
	case "argon2id":
		got = argon2.IDKey([]byte(digest), salt, t, m, p, uint32(len(want)))
	case "argon2i":
		got = argon2.Key([]byte(digest), salt, t, m, p, uint32(len(want)))
	default:
		return false, fmt.Errorf("argon2: %s is not supported", parts[1])
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// Upper bounds of argon2's parameters: 1 GiB of memory (m is in KiB) and 16
// passes, far above node-argon2's defaults (64 MiB, 3).
const (
	argon2MaxMemory = 1 << 20
	argon2MaxTime   = 16
)

// HashPassword hashes a new password as Meteor does: bcrypt of the SHA-256 digest.
func HashPassword(password string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(passwordDigest(password)), 10)
	return string(h), err
}

// Login tokens are Meteor's: the token is a secret the client keeps, and the user
// document keeps only services.resume.loginTokens[].hashedToken =
// base64(sha256(token)) with the time it was made.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.StdEncoding.EncodeToString(sum[:])
}

const tokenAlphabet = "23456789ABCDEFGHJKLMNPQRSTWXYZabcdefghijkmnopqrstuvwxyz-_"

// LoginDays is how long a login token is valid: WeKan's
// ACCOUNTS_COMMON_LOGIN_EXPIRATION_IN_DAYS default.
const LoginDays = 90

func (s *Server) newLoginToken(ctx context.Context, userID string) (string, time.Time, error) {
	token := randomString(tokenAlphabet, 43)
	now := time.Now().UTC()
	_, err := s.db.Collection("users").UpdateOne(ctx, bson.M{"_id": eq(userID)}, bson.M{"$push": bson.M{
		"services.resume.loginTokens": bson.M{"when": now, "hashedToken": hashToken(token)}}})
	return token, now.Add(LoginDays * 24 * time.Hour), err
}

func (s *Server) userByToken(ctx context.Context, token string) (*User, error) {
	if token == "" {
		return nil, errors.New("no token")
	}
	var u User
	since := time.Now().Add(-LoginDays * 24 * time.Hour)
	err := s.db.Collection("users").FindOne(ctx, bson.M{"services.resume.loginTokens": bson.M{
		"$elemMatch": bson.M{"hashedToken": hashToken(token), "when": bson.M{"$gt": since}}}}).Decode(&u)
	if err != nil {
		return nil, err
	}
	if u.LoginDisabled {
		return nil, errors.New("login disabled")
	}
	return &u, nil
}

func (s *Server) dropToken(ctx context.Context, token string) {
	_, _ = s.db.Collection("users").UpdateOne(ctx, bson.M{"services.resume.loginTokens.hashedToken": hashToken(token)},
		bson.M{"$pull": bson.M{"services.resume.loginTokens": bson.M{"hashedToken": hashToken(token)}}})
}

// findLoginUser finds a user by username or e-mail address, as WeKan's sign-in
// form accepts either.
func (s *Server) findLoginUser(ctx context.Context, name string) (*User, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 254 { // 254: the longest e-mail address
		return nil, errBadPassword
	}
	var u User
	err := s.db.Collection("users").FindOne(ctx, bson.M{"$or": []bson.M{
		{"username": eq(name)}, {"emails.address": eq(strings.ToLower(name))}}}).Decode(&u)
	if err != nil {
		return nil, errBadPassword
	}
	return &u, nil
}

// The cookie is the one Meteor WeKan sets (server/header-login.js), with the
// same login token: a session survives moving a WeKan to Weddy.
const sessionCookie = "meteor_login_token"

// currentUser is the signed-in user of a page request (the cookie) or an API
// request (Authorization: Bearer <token>, as WeKan's REST API takes it).
func (s *Server) currentUser(r *http.Request) *User {
	token := ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		token = strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	} else if c, err := r.Cookie(sessionCookie); err == nil {
		token = c.Value
	}
	u, err := s.userByToken(r.Context(), token)
	if err != nil {
		return nil
	}
	return u
}

// setSessionCookie: always Secure, so the login token never travels over plain
// HTTP - Caddy gives every site HTTPS. Browsers keep a Secure cookie on
// http://localhost and http://127.0.0.1 too, so a local test works without TLS.
func setSessionCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", Expires: expires,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
}
