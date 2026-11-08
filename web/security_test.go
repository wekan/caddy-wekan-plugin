package web

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"golang.org/x/crypto/bcrypt"
)

// Code scanning alerts #1-#5: a value from a request is used in a query only
// through eq(), never put in a filter as it came. This reads every file of the
// package, so the shape cannot come back anywhere - not only where it was found.
// A projection's 0 or 1 is a constant of the code, never a value from a request.
var numberConstant = regexp.MustCompile(`^-?\d+$`)

func TestEveryQueryValueIsEqOrAnID(t *testing.T) {
	field := regexp.MustCompile(`"(_id|listId|swimlaneId|boardId|userId|username|emails\.address|hashedToken|services\.resume\.loginTokens\.hashedToken)":\s*([^,}\n]+)`)
	files, _ := filepath.Glob("*.go")
	var offenders []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, _ := os.ReadFile(f)
		for i, line := range strings.Split(string(src), "\n") {
			if !strings.Contains(line, "bson.M{") || strings.Contains(line, "$set") || strings.Contains(line, "InsertOne") || strings.Contains(line, "// inserted, not a filter") {
				continue
			}
			for _, m := range field.FindAllStringSubmatch(line, -1) {
				v := strings.TrimSpace(m[2])
				// eq(...), or an id of a document read from the database: x.ID,
				// doc["_id"], or dbIn(...) - ids read from documents, only the
				// ones that are ids.
				if strings.HasPrefix(v, "eq(") || (strings.Contains(v, ".") && strings.HasSuffix(v, "ID")) || strings.HasSuffix(v, `["_id"]`) || v == "randomID()" || v == "in" || strings.HasPrefix(v, "dbIn(") || numberConstant.MatchString(v) || strings.HasPrefix(v, "hashToken(") {
					continue
				}
				offenders = append(offenders, fmt.Sprintf("%s:%d %s: %s", f, i+1, m[1], v))
			}
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("query values from outside, not through eq():\n%s", strings.Join(offenders, "\n"))
	}
	// The scan itself catches the old shape (negative).
	if !field.MatchString(`bson.M{"_id": id, "boardId": b.ID}`) {
		t.Fatal("the scan misses the shape it guards")
	}
}

func TestIDsFromRequestsAreChecked(t *testing.T) {
	for _, ok := range []string{"ZaR7q3xHdQ9wW2sKp", "board940e4fb88965", "a_b-c"} {
		if !validID(ok) {
			t.Errorf("%q is an id", ok)
		}
	}
	for _, bad := range []string{"", `{"$ne":""}`, "$where", "a.b", "a b", strings.Repeat("a", 65), "a\x00"} {
		if validID(bad) {
			t.Errorf("negative: %q must not be used as an id", bad)
		}
	}

	w := newTestWeKan(t)
	u := w.seedUser(t, "olga", "pw")
	b, _ := w.s.CreateBoard(context.Background(), u, "Ids")
	c := w.client(t)
	body(t, w.signIn(t, c, "olga", "pw"))
	for _, path := range []string{"/b/" + url.PathEscape("$ne") + "/x", "/b/" + url.PathEscape("a.b") + "/x"} {
		if res, _ := get(t, c, w.srv.URL+path); res.StatusCode != http.StatusNotFound {
			t.Errorf("negative: %s answered %d, want 404", path, res.StatusCode)
		}
	}
	res, _ := c.Post(w.srv.URL+"/b/"+b.ID+"/move", "application/json",
		strings.NewReader(`{"moves":[{"cardId":{"$ne":""},"listId":"x","swimlaneId":"y"}]}`))
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("negative: an operator object where an id goes: %d", res.StatusCode)
	}
	if err := w.s.MoveCards(context.Background(), b, u, []Move{{CardID: "$where", ListID: "l", SwimlaneID: "s"}}); err == nil {
		t.Fatal("negative: a move of a card id that is no id")
	}
	if code := call(t, "GET", w.srv.URL+"/api/users/"+url.PathEscape("$ne")+"/boards", "", "", nil); code != 401 {
		t.Fatalf("no token: %d", code)
	}
	if _, err := w.s.findLoginUser(context.Background(), strings.Repeat("a", 300)); err == nil {
		t.Fatal("negative: a 300-character name is no user")
	}
}

// Alerts #6 and #7: argon2's parameters are parsed at the width argon2 takes
// them, and bounded.
func TestArgon2ParametersAreBounded(t *testing.T) {
	salt := base64.RawStdEncoding.EncodeToString([]byte("0123456789abcdef"))
	hash := base64.RawStdEncoding.EncodeToString(make([]byte, 32))
	for _, params := range []string{
		"m=4294967296,t=3,p=1", // more than 32 bits
		"m=65536,t=3,p=256",    // more than 8 bits: was cut to p=0
		"m=65536,t=3,p=0",
		"m=0,t=3,p=1",
		"m=2097152,t=3,p=1", // 2 GiB, above the bound
		"m=65536,t=100,p=1",
	} {
		start := time.Now()
		ok, err := checkArgon2("$argon2id$v=19$"+params+"$"+salt+"$"+hash, "digest")
		if ok || err == nil {
			t.Errorf("negative: %s must be refused, got ok=%v err=%v", params, ok, err)
		}
		if time.Since(start) > time.Second {
			t.Errorf("negative: %s was computed before it was refused", params)
		}
	}
	if _, err := checkArgon2("$argon2id$v=19$m=4096,t=1,p=1$"+salt+"$"+base64.RawStdEncoding.EncodeToString(make([]byte, 4)), "d"); err == nil {
		t.Error("negative: a 4-byte hash")
	}
}

// Alert #8: the login cookie, and the one that clears it, are Secure and HttpOnly.
func TestLoginCookieIsSecure(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "pia", "pw")
	c := w.client(t)
	res := w.signIn(t, c, "pia", "pw")
	body(t, res)
	var found bool
	for _, ck := range res.Cookies() {
		if ck.Name == sessionCookie {
			found = true
			if !ck.Secure || !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode {
				t.Fatalf("the login cookie: %+v", ck)
			}
		}
	}
	if !found {
		t.Fatal("no login cookie")
	}
	req, _ := http.NewRequest("POST", w.srv.URL+"/logout", nil)
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body(t, res)
	for _, ck := range res.Cookies() {
		if ck.Name == sessionCookie && (!ck.Secure || !ck.HttpOnly) {
			t.Fatalf("negative: the cookie that clears the login is not Secure: %+v", ck)
		}
	}
}

// Alert #9: SHA-256 is Meteor's pre-hash, never what is stored. What is stored
// is bcrypt of it, and a bare SHA-256 digest in its place signs nobody in.
func TestPasswordsAreStoredSlowlyHashed(t *testing.T) {
	h, err := HashPassword("pw")
	if err != nil {
		t.Fatal(err)
	}
	if cost, err := bcrypt.Cost([]byte(h)); err != nil || cost < 10 {
		t.Fatalf("stored as bcrypt, cost 10 or more: %v %d", err, cost)
	}
	w := newTestWeKan(t)
	u := w.seedUser(t, "quinn", "pw")
	_, _ = w.db.Collection("users").UpdateOne(context.Background(), bson.M{"_id": u.ID},
		bson.M{"$set": bson.M{"services.password.bcrypt": meteorPreHash("pw")}})
	if res := w.signIn(t, w.client(t), "quinn", "pw"); res.StatusCode == http.StatusSeeOther {
		t.Fatal("negative: a bare SHA-256 digest as the stored hash signed in")
	}
}

// Code scanning alert #9: Meteor's pre-hash is computed in one place, and no
// other fast hash in this package touches a password - as WeKan pins for its
// breach-check digest (#599).
func TestNoFastHashOfPasswords(t *testing.T) {
	want := sha256.Sum256([]byte("pässwörd"))
	if meteorPreHash("pässwörd") != hex.EncodeToString(want[:]) {
		t.Fatal("meteorPreHash is Meteor's SHA-256 hex digest, UTF-8 too")
	}
	fast := regexp.MustCompile(`\b(?:md5|sha1|sha256|sha512|sha3)\.(?:Sum\w*|New\w*)\(|crypto\.(?:MD5|SHA1|SHA256|SHA512)\.New\(`)
	pass := regexp.MustCompile(`(?i)pass`)
	files, _ := filepath.Glob("*.go")
	var offenders []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, _ := os.ReadFile(f)
		text := string(src)
		if f != "meteor_prehash.go" && strings.Contains(text, "crypto.SHA256") {
			offenders = append(offenders, f+": crypto.SHA256 outside meteor_prehash.go")
		}
		for i, line := range strings.Split(text, "\n") {
			if fast.MatchString(line) && pass.MatchString(line) {
				offenders = append(offenders, fmt.Sprintf("%s:%d %s", f, i+1, strings.TrimSpace(line)))
			}
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("a fast hash next to a password:\n%s", strings.Join(offenders, "\n"))
	}
	if !fast.MatchString(`sum := sha256.Sum256([]byte(password))`) {
		t.Fatal("the scan misses the old shape")
	}
}
