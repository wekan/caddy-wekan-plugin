package web

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/bcrypt"

	"github.com/wekan/caddy-ferretdb-plugin/store"
)

// testWeKan is a Go WeKan over a fresh embedded FerretDB, served by httptest.
type testWeKan struct {
	s   *Server
	db  *mongo.Database
	srv *httptest.Server
}

func newTestWeKan(t *testing.T) *testWeKan {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	st, err := store.Open(ctx, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(st.DB, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(s) // HTTPS: the login cookie is Secure
	testClient = srv.Client()
	t.Cleanup(func() {
		srv.CloseClientConnections()
		_ = s.Close()
		srv.Close()
		_ = st.Close()
	})
	return &testWeKan{s: s, db: st.DB, srv: srv}
}

// seedUser inserts a user as Meteor's accounts-password writes one.
func (w *testWeKan) seedUser(t *testing.T, username, password string) *User {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(meteorPreHash(password)), 4)
	if err != nil {
		t.Fatal(err)
	}
	id := randomID()
	_, err = w.db.Collection("users").InsertOne(context.Background(), bson.M{
		"_id": id, "username": username, "createdAt": time.Now(),
		"emails":   []bson.M{{"address": username + "@example.invalid", "verified": true}},
		"profile":  bson.M{"fullname": strings.ToUpper(username)},
		"services": bson.M{"password": bson.M{"bcrypt": string(hash)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &User{ID: id, Username: username}
}

// testClient trusts the current test server's certificate.
var testClient *http.Client

// client is a browser: it keeps cookies and does not follow redirects.
func (w *testWeKan) client(t *testing.T) *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, Transport: w.srv.Client().Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (w *testWeKan) signIn(t *testing.T, c *http.Client, username, password string) *http.Response {
	t.Helper()
	res, err := c.PostForm(w.srv.URL+"/sign-in", url.Values{"username": {username}, "password": {password}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func body(t *testing.T, res *http.Response) string {
	t.Helper()
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func get(t *testing.T, c *http.Client, u string) (*http.Response, string) {
	t.Helper()
	res, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	return res, body(t, res)
}

func TestMeteorPasswords(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte(meteorPreHash("s3cret")), 4)
	u := &User{}
	u.Services.Password.Bcrypt = string(hash)
	if err := CheckPassword(u, "s3cret"); err != nil {
		t.Fatalf("bcrypt of the SHA-256 digest, as Meteor stores it: %v", err)
	}
	if CheckPassword(u, "wrong") == nil {
		t.Fatal("negative: a wrong password was accepted")
	}
	if CheckPassword(&User{}, "") == nil {
		t.Fatal("negative: a user with no password was accepted with an empty one")
	}

	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	key := argon2.IDKey([]byte(meteorPreHash("s3cret")), salt, 3, 4096, 1, 32)
	a := &User{}
	a.Services.Password.Argon2 = fmt.Sprintf("$argon2id$v=19$m=4096,t=3,p=1$%s$%s",
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
	if err := CheckPassword(a, "s3cret"); err != nil {
		t.Fatalf("argon2id PHC: %v", err)
	}
	if CheckPassword(a, "s3cre") == nil {
		t.Fatal("negative: argon2 accepted a wrong password")
	}
	a.Services.Password.Argon2 = "$argon2id$garbage"
	if CheckPassword(a, "s3cret") == nil {
		t.Fatal("negative: a malformed argon2 hash was accepted")
	}

	h, err := HashPassword("new")
	if err != nil || bcrypt.CompareHashAndPassword([]byte(h), []byte(meteorPreHash("new"))) != nil {
		t.Fatal("HashPassword must write what Meteor reads: bcrypt of the digest")
	}
}

func TestSignInAllBoardsAndLogout(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "alice", "pw-alice")
	c := w.client(t)

	res, _ := get(t, c, w.srv.URL+"/")
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/sign-in" {
		t.Fatalf("signed out, All Boards must send to sign-in: %d %s", res.StatusCode, res.Header.Get("Location"))
	}

	res = w.signIn(t, c, "alice", "wrong")
	if b := body(t, res); res.StatusCode != http.StatusUnauthorized || !strings.Contains(b, "js-login-error") {
		t.Fatalf("negative: a wrong password must show the error: %d", res.StatusCode)
	}
	if res = w.signIn(t, c, "nobody", "pw"); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("negative: an unknown user: %d", res.StatusCode)
	}

	res = w.signIn(t, c, "alice@example.invalid", "pw-alice")
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign in with the e-mail address: %d %s", res.StatusCode, body(t, res))
	}
	var tokenCookie *http.Cookie
	for _, ck := range res.Cookies() {
		if ck.Name == "meteor_login_token" {
			tokenCookie = ck
		}
	}
	if tokenCookie == nil || !tokenCookie.HttpOnly {
		t.Fatal("the login token must be an HttpOnly meteor_login_token cookie")
	}
	var raw struct {
		ID       string `bson:"_id"`
		Services struct {
			Resume struct {
				LoginTokens []struct {
					HashedToken string    `bson:"hashedToken"`
					When        time.Time `bson:"when"`
				} `bson:"loginTokens"`
			} `bson:"resume"`
		} `bson:"services"`
	}
	_ = w.db.Collection("users").FindOne(context.Background(), bson.M{"username": "alice"}).Decode(&raw)
	if tk := raw.Services.Resume.LoginTokens; len(tk) != 1 || tk[0].HashedToken != hashToken(tokenCookie.Value) || tk[0].When.IsZero() {
		t.Fatal("the user document keeps only the hashed token and its time, as Meteor does")
	}

	if _, err := w.s.CreateBoard(context.Background(), &User{ID: raw.ID}, "Roadmap 2027"); err != nil {
		t.Fatal(err)
	}
	res, page := get(t, c, w.srv.URL+"/")
	if res.StatusCode != 200 || !strings.Contains(page, "Roadmap 2027") || !strings.Contains(page, "board-list-item-name") {
		t.Fatalf("All Boards must list the board: %d", res.StatusCode)
	}

	req, _ := http.NewRequest("POST", w.srv.URL+"/logout", nil)
	if _, err := c.Do(req); err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.userByToken(context.Background(), tokenCookie.Value); err == nil {
		t.Fatal("logging out must remove the token from the user document")
	}
	if res, _ := get(t, c, w.srv.URL+"/"); res.StatusCode != http.StatusSeeOther {
		t.Fatal("negative: after logging out All Boards is not shown")
	}
}

func TestSignInThrottle(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "bob", "pw-bob")
	c := w.client(t)
	for i := 0; i < loginFailures; i++ {
		body(t, w.signIn(t, c, "bob", "guess"))
	}
	res := w.signIn(t, c, "bob", "pw-bob")
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("after %d failures even the right password waits: %d", loginFailures, res.StatusCode)
	}
}

func TestBoardListsCardsAndMultiDrag(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "carol", "pw")
	c := w.client(t)
	body(t, w.signIn(t, c, "carol", "pw"))

	res, err := c.PostForm(w.srv.URL+"/boards", url.Values{"title": {"Sprint"}})
	if err != nil || res.StatusCode != http.StatusSeeOther {
		t.Fatalf("create board: %v %v", err, res.StatusCode)
	}
	boardURL := res.Header.Get("Location")
	m := regexp.MustCompile(`^/b/([^/]+)/sprint$`).FindStringSubmatch(boardURL)
	if m == nil {
		t.Fatalf("board address: %q", boardURL)
	}
	boardID := m[1]

	for _, title := range []string{"Todo", "Done"} {
		if res, _ := c.PostForm(w.srv.URL+"/b/"+boardID+"/lists", url.Values{"title": {title}}); res.StatusCode != http.StatusSeeOther {
			t.Fatalf("add list %s: %d", title, res.StatusCode)
		}
	}
	var lists []List
	cur, _ := w.db.Collection("lists").Find(context.Background(), bson.M{"boardId": boardID})
	_ = cur.All(context.Background(), &lists)
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(context.Background(), bson.M{"boardId": boardID}).Decode(&sw)
	if len(lists) != 2 || sw.ID == "" {
		t.Fatalf("lists %d, swimlane %q", len(lists), sw.ID)
	}
	todo, done := lists[0], lists[1]
	if todo.Title != "Todo" {
		todo, done = done, todo
	}

	for _, title := range []string{"One", "Two", "Three"} {
		res, _ := c.PostForm(w.srv.URL+"/b/"+boardID+"/cards", url.Values{"title": {title}, "listId": {todo.ID}, "swimlaneId": {sw.ID}})
		if res.StatusCode != http.StatusSeeOther {
			t.Fatalf("add card: %d", res.StatusCode)
		}
	}
	res, page := get(t, c, w.srv.URL+boardURL)
	if res.StatusCode != 200 || !strings.Contains(page, `id="js-list-`+todo.ID+`"`) || strings.Count(page, "minicard-title") != 3 {
		t.Fatalf("the board must show the lists and the three cards: %d", res.StatusCode)
	}

	var cards []Card
	cur, _ = w.db.Collection("cards").Find(context.Background(), bson.M{"boardId": boardID})
	_ = cur.All(context.Background(), &cards)
	if len(cards) != 3 {
		t.Fatalf("cards: %d", len(cards))
	}
	moves := []Move{{CardID: cards[0].ID, ListID: done.ID, SwimlaneID: sw.ID, Sort: 1}, {CardID: cards[1].ID, ListID: done.ID, SwimlaneID: sw.ID, Sort: 2}}
	b, _ := json.Marshal(map[string]any{"moves": moves})
	res, err = c.Post(w.srv.URL+"/b/"+boardID+"/move", "application/json", strings.NewReader(string(b)))
	if err != nil || res.StatusCode != http.StatusNoContent {
		t.Fatalf("MultiDrag move of two cards: %v %d", err, res.StatusCode)
	}
	if n, _ := w.db.Collection("cards").CountDocuments(context.Background(), bson.M{"listId": done.ID}); n != 2 {
		t.Fatalf("two cards must be in Done: %d", n)
	}

	// The card page opens the card details over the board.
	res, page = get(t, c, w.srv.URL+boardURL+"/"+cards[2].ID)
	if res.StatusCode != 200 || !strings.Contains(page, "js-card-details") || !strings.Contains(page, ">Three<") {
		t.Fatalf("card page: %d", res.StatusCode)
	}

	// Negative: a card cannot be moved to a list of another board.
	other, _ := w.s.CreateBoard(context.Background(), &User{ID: cards[0].UserID}, "Other")
	ol, _ := w.s.AddList(context.Background(), other, "Elsewhere")
	b, _ = json.Marshal(map[string]any{"moves": []Move{{CardID: cards[2].ID, ListID: ol.ID, SwimlaneID: sw.ID}}})
	res, _ = c.Post(w.srv.URL+"/b/"+boardID+"/move", "application/json", strings.NewReader(string(b)))
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("negative: moving to another board's list must be refused: %d", res.StatusCode)
	}
	var still Card
	_ = w.db.Collection("cards").FindOne(context.Background(), bson.M{"_id": cards[2].ID}).Decode(&still)
	if still.ListID != todo.ID {
		t.Fatal("negative: the refused move must not have changed the card")
	}
	// Negative: an empty title adds nothing.
	if res, _ := c.PostForm(w.srv.URL+"/b/"+boardID+"/cards", url.Values{"title": {"  "}, "listId": {todo.ID}, "swimlaneId": {sw.ID}}); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("negative: a card without a title: %d", res.StatusCode)
	}
}

func TestUnknownFieldsAreKept(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "dora", "pw")
	b, _ := w.s.CreateBoard(context.Background(), u, "Keep")
	l, _ := w.s.AddList(context.Background(), b, "L")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(context.Background(), bson.M{"boardId": b.ID}).Decode(&sw)
	card, _ := w.s.AddCard(context.Background(), b, u, l.ID, sw.ID, "C")
	_, _ = w.db.Collection("cards").UpdateOne(context.Background(), bson.M{"_id": card.ID}, bson.M{"$set": bson.M{"labelIds": []string{"x"}, "dueAt": time.Now()}})
	if err := w.s.MoveCards(context.Background(), b, u, []Move{{CardID: card.ID, ListID: l.ID, SwimlaneID: sw.ID, Sort: 5}}); err != nil {
		t.Fatal(err)
	}
	var raw bson.M
	_ = w.db.Collection("cards").FindOne(context.Background(), bson.M{"_id": card.ID}).Decode(&raw)
	if raw["labelIds"] == nil || raw["dueAt"] == nil {
		t.Fatal("a field Go WeKan does not know yet must survive a change")
	}
}

func TestBoardVisibility(t *testing.T) {
	w := newTestWeKan(t)
	owner := w.seedUser(t, "erin", "pw")
	w.seedUser(t, "frank", "pw")
	b, _ := w.s.CreateBoard(context.Background(), owner, "Secret")
	l, _ := w.s.AddList(context.Background(), b, "L")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(context.Background(), bson.M{"boardId": b.ID}).Decode(&sw)

	c := w.client(t)
	body(t, w.signIn(t, c, "frank", "pw"))
	_, page := get(t, c, w.srv.URL+"/")
	if strings.Contains(page, "Secret") {
		t.Fatal("negative: All Boards lists only the user's own boards")
	}
	if res, page := get(t, c, w.srv.URL+"/b/"+b.ID+"/secret"); res.StatusCode != 404 || strings.Contains(page, "js-list") {
		t.Fatalf("negative: another's private board is not found: %d", res.StatusCode)
	}
	res, _ := c.PostForm(w.srv.URL+"/b/"+b.ID+"/cards", url.Values{"title": {"x"}, "listId": {l.ID}, "swimlaneId": {sw.ID}})
	if res.StatusCode != 404 {
		t.Fatalf("negative: no card on another's board: %d", res.StatusCode)
	}

	_, _ = w.db.Collection("boards").UpdateOne(context.Background(), bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"permission": "public"}})
	if res, page := get(t, c, w.srv.URL+"/b/"+b.ID+"/secret"); res.StatusCode != 200 || strings.Contains(page, "js-add-card") {
		t.Fatalf("a public board is readable, without the add buttons: %d", res.StatusCode)
	}
	// 403, not 404: the board is public, so it plainly exists; what is refused is
	// the write, by the same role rule as the REST API (Board.Writable).
	res, _ = c.PostForm(w.srv.URL+"/b/"+b.ID+"/cards", url.Values{"title": {"x"}, "listId": {l.ID}, "swimlaneId": {sw.ID}})
	if res.StatusCode != 403 {
		t.Fatalf("negative: a public board is not writable by a non-member: %d", res.StatusCode)
	}

	// A removed member (isActive false) sees it no longer.
	_, _ = w.db.Collection("boards").UpdateOne(context.Background(), bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"permission": "private", "members.0.isActive": false}})
	co := w.client(t)
	body(t, w.signIn(t, co, "erin", "pw"))
	if res, _ := get(t, co, w.srv.URL+"/b/"+b.ID+"/secret"); res.StatusCode != 404 {
		t.Fatalf("negative: an inactive member does not see the board: %d", res.StatusCode)
	}
}

func TestCrossSiteFormRefused(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "gina", "pw")
	c := w.client(t)
	body(t, w.signIn(t, c, "gina", "pw"))
	req, _ := http.NewRequest("POST", w.srv.URL+"/boards", strings.NewReader("title=Evil"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	res, err := c.Do(req)
	if err != nil || res.StatusCode != http.StatusForbidden {
		t.Fatalf("negative: a form from another site is refused: %v %d", err, res.StatusCode)
	}
	if n, _ := w.db.Collection("boards").CountDocuments(context.Background(), bson.M{"title": "Evil"}); n != 0 {
		t.Fatal("negative: the refused form created a board")
	}
	req, _ = http.NewRequest("POST", w.srv.URL+"/boards", strings.NewReader("title=Fine"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", w.srv.URL)
	if res, _ := c.Do(req); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("the same site's form works: %d", res.StatusCode)
	}
}

func TestLiveBoardChanges(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "hugo", "pw")
	b, _ := w.s.CreateBoard(context.Background(), u, "Live")
	c := w.client(t)
	body(t, w.signIn(t, c, "hugo", "pw"))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", w.srv.URL+"/b/"+b.ID+"/live", nil)
	req.Header.Set("Accept", "text/event-stream")
	res, err := c.Do(req)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("live: %v", err)
	}
	defer res.Body.Close()
	r := bufio.NewReader(res.Body)
	if line, _ := r.ReadString('\n'); !strings.HasPrefix(line, ": connected") {
		t.Fatalf("first line %q", line)
	}
	go func() {
		other := w.client(t)
		w.signIn(t, other, "hugo", "pw").Body.Close()
		r, _ := other.PostForm(w.srv.URL+"/b/"+b.ID+"/lists", url.Values{"title": {"New"}})
		if r != nil {
			r.Body.Close()
		}
	}()
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("no board-changed event: %v", err)
		}
		if strings.HasPrefix(line, "event: board-changed") {
			break
		}
	}

	// Negative: another user cannot listen to the board.
	w.seedUser(t, "ivy", "pw")
	o := w.client(t)
	body(t, w.signIn(t, o, "ivy", "pw"))
	if res, _ := get(t, o, w.srv.URL+"/b/"+b.ID+"/live"); res.StatusCode != 404 {
		t.Fatalf("negative: another user's live stream: %d", res.StatusCode)
	}
}

func TestTranslatedPages(t *testing.T) {
	w := newTestWeKan(t)
	req, _ := http.NewRequest("GET", w.srv.URL+"/sign-in", nil)
	req.Header.Set("Accept-Language", "fi-FI,fi;q=0.9,en;q=0.5")
	res, err := testClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if page := body(t, res); !strings.Contains(page, "Kirjaudu sisään") || !strings.Contains(page, `lang="fi"`) {
		t.Fatal("the sign-in page must be in Finnish for a Finnish browser")
	}
	req.Header.Set("Accept-Language", "ar")
	res, _ = testClient.Do(req)
	if page := body(t, res); !strings.Contains(page, `dir="rtl"`) {
		t.Fatal("Arabic is right to left")
	}
	req.Header.Set("Accept-Language", "xx-unknown")
	res, _ = testClient.Do(req)
	if page := body(t, res); !strings.Contains(page, "Log In") {
		t.Fatal("negative: an unknown language falls back to English")
	}

	u := w.seedUser(t, "jaana", "pw")
	_, _ = w.db.Collection("users").UpdateOne(context.Background(), bson.M{"_id": u.ID}, bson.M{"$set": bson.M{"profile.language": "fi"}})
	c := w.client(t)
	body(t, w.signIn(t, c, "jaana", "pw"))
	if _, page := get(t, c, w.srv.URL+"/"); !strings.Contains(page, "Kaikki taulut") {
		t.Fatal("the user's profile.language chooses the language")
	}
}

// Every translation key a page or the code uses is in the copied translations:
// cmd/sync-i18n was run after the last change.
func TestTranslationsAreSynced(t *testing.T) {
	re := regexp.MustCompile("(?:\\bt \\$?\\.L |\\btr\\([^;\"]*?, |\\bmsg\\()[\"`]([A-Za-z0-9_.-]+)[\"`]")
	en := map[string]string{}
	b, err := os.ReadFile("i18n/en.json")
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(b, &en)
	files, _ := filepath.Glob("templates/*.pug")
	gofiles, _ := filepath.Glob("*.go")
	for _, f := range append(files, gofiles...) {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, _ := os.ReadFile(f)
		for _, m := range re.FindAllSubmatch(src, -1) {
			if en[string(m[1])] == "" {
				t.Errorf("%s uses %q: run go run ./cmd/sync-i18n /path/to/wekan", f, m[1])
			}
		}
	}
}

// wrapped hides http.Flusher behind Unwrap, as Caddy's ResponseWriter wrapper does.
type wrapped struct{ http.ResponseWriter }

func (w wrapped) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Inside Caddy the writer is a wrapper: live updates must stream through it - a
// w.(http.Flusher) check answered "streaming unsupported" there.
func TestLiveStreamsThroughAWrappedWriter(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "nina", "pw")
	b, _ := w.s.CreateBoard(context.Background(), u, "Wrapped")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.s.ServeHTTP(wrapped{rw}, r)
	}))
	defer srv.Close()
	c := w.client(t)
	c.Transport = srv.Client().Transport
	res, err := c.PostForm(srv.URL+"/sign-in", url.Values{"username": {"nina"}, "password": {"pw"}})
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/b/"+b.ID+"/live", nil)
	res, err = c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	line, _ := bufio.NewReader(res.Body).ReadString('\n')
	if res.StatusCode != 200 || !strings.HasPrefix(line, ": connected") {
		t.Fatalf("negative: the stream must work through a wrapper: %d %q", res.StatusCode, line)
	}
}

func TestChangeLanguage(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "tove", "pw")
	c := w.client(t)
	body(t, w.signIn(t, c, "tove", "pw"))
	res, page := get(t, c, w.srv.URL+"/language")
	if res.StatusCode != 200 || !strings.Contains(page, ">Suomi<") || !strings.Contains(page, `dir="rtl"`) {
		t.Fatal("every language, named in itself, right-to-left ones too")
	}
	if res, _ := c.PostForm(w.srv.URL+"/language", url.Values{"language": {"fi"}}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("choosing Finnish: %d", res.StatusCode)
	}
	var raw struct {
		Profile struct{ Language string } `bson:"profile"`
	}
	_ = w.db.Collection("users").FindOne(context.Background(), bson.M{"_id": u.ID}).Decode(&raw)
	if raw.Profile.Language != "fi" {
		t.Fatalf("profile.language: %q", raw.Profile.Language)
	}
	if _, page := get(t, c, w.srv.URL+"/"); !strings.Contains(page, "Kaikki taulut") {
		t.Fatal("the pages are in Finnish after choosing it")
	}
	for _, bad := range []string{"", "xx", "<script>", "fi-NOPE-x"} {
		if res, _ := c.PostForm(w.srv.URL+"/language", url.Values{"language": {bad}}); res.StatusCode != http.StatusBadRequest {
			t.Errorf("negative: %q is no language: %d", bad, res.StatusCode)
		}
	}
}

// Changes are recorded as Node.js WeKan records them, so its activity feed,
// notifications and rules see them.
func TestActivities(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "yan", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Acts")
	l1, _ := w.s.AddList(ctx, b, "One")
	l2, _ := w.s.AddList(ctx, b, "Two")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	c, _ := w.s.AddCard(ctx, b, u, l1.ID, sw.ID, "Card")
	_ = w.s.MoveCards(ctx, b, u, []Move{{CardID: c.ID, ListID: l2.ID, SwimlaneID: sw.ID, Sort: 1}})
	_ = w.s.MoveCards(ctx, b, u, []Move{{CardID: c.ID, ListID: l2.ID, SwimlaneID: sw.ID, Sort: 2}})
	count := func(f bson.M) int64 {
		n, _ := w.db.Collection("activities").CountDocuments(ctx, f)
		return n
	}
	if count(bson.M{"activityType": "createBoard", "boardId": b.ID, "userId": u.ID}) != 1 {
		t.Error("createBoard")
	}
	if count(bson.M{"activityType": "createCard", "cardId": c.ID, "listName": "One", "cardTitle": "Card"}) != 1 {
		t.Error("createCard, with the list's name and the card's title")
	}
	if count(bson.M{"activityType": "moveCard", "cardId": c.ID, "oldListId": l1.ID, "listId": l2.ID, "listName": "Two"}) != 1 {
		t.Error("moveCard, from the old list to the new")
	}
	if count(bson.M{"activityType": "moveCard"}) != 1 {
		t.Error("negative: a move within the same list is no moveCard")
	}
}

// The board's sidebar, its menu and Export, with WeKan's class names: the menu
// for a member who is not comment-only or a worker, the export links for one
// who may export, never for an assigned-only member.
func TestBoardSidebarMenuAndExport(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	b, _ := w.s.CreateBoard(context.Background(), u, "Menu")
	page := func(name string) string {
		c := w.client(t)
		body(t, w.signIn(t, c, name, "pw"))
		_, html := get(t, c, w.srv.URL+"/b/"+b.ID+"/"+b.Slug)
		return html
	}
	html := page("ann")
	for _, want := range []string{"js-toggle-page-sidebar", "board-sidebar", "js-open-board-menu", "js-export-board",
		"export-board-pane-formats", `/api/boards/` + b.ID + `/export?attachments=false`, `/api/boards/` + b.ID + `/exportZip`} {
		if !strings.Contains(html, want) {
			t.Errorf("the board admin's page has %s", want)
		}
	}
	setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: true, IsWorker: true})
	if html = page("bob"); strings.Contains(html, "js-open-board-menu") {
		t.Error("negative: a worker has no board menu")
	}
	setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: true, IsNormalAssignedOnly: true})
	if html = page("bob"); !strings.Contains(html, "js-open-board-menu") || strings.Contains(html, "js-export-board") || strings.Contains(html, "/export?") {
		t.Error("negative: an assigned-only member gets the menu but no export")
	}
}

// Comments and activities in the pages: a comment from the card's form, the
// card's Activities in the reader's words, the board's activities for a board
// admin only; a role without comments gets no form, and a post is refused.
func TestCardCommentsAndActivitiesInThePages(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Talk")
	l, _ := w.s.AddList(ctx, b, "Todo")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	c, _ := w.s.AddCard(ctx, b, u, l.ID, sw.ID, "Alpha")
	w.s.activity(ctx, u.ID, "createCard", bson.M{"boardId": b.ID, "cardId": c.ID, "listId": l.ID})
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	cardURL := w.srv.URL + "/b/" + b.ID + "/" + b.Slug + "/" + c.ID
	res, _ := ann.PostForm(w.srv.URL+"/b/"+b.ID+"/cards/"+c.ID+"/comments", url.Values{"comment": {"Hello there"}})
	if res.StatusCode != http.StatusSeeOther { // back to the card, which Unpoly follows
		t.Fatalf("comment from the form: %d", res.StatusCode)
	}
	_, html := get(t, ann, cardURL)
	for _, want := range []string{"js-new-comment-form", `class="comment-text"`, "Hello there", `data-section="activities"`,
		"added Alpha to Todo", "js-toggle-show-activities", "<time"} {
		if !strings.Contains(html, want) {
			t.Errorf("the card page has %q", want)
		}
	}
	setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: true, IsReadOnly: true})
	bc := w.client(t)
	body(t, w.signIn(t, bc, "bob", "pw"))
	_, html = get(t, bc, cardURL)
	if strings.Contains(html, "js-new-comment-form") || strings.Contains(html, "js-toggle-show-activities") {
		t.Error("negative: a read-only member gets no comment form and not the board's activities")
	}
	res, _ = bc.PostForm(w.srv.URL+"/b/"+b.ID+"/cards/"+c.ID+"/comments", url.Values{"comment": {"sneaky"}})
	if res.StatusCode != 403 {
		t.Fatalf("negative: a read-only member's comment is refused: %d", res.StatusCode)
	}
	if n, _ := w.db.Collection("card_comments").CountDocuments(ctx, bson.M{"text": "sneaky"}); n != 0 {
		t.Fatal("negative: and not stored")
	}
}
