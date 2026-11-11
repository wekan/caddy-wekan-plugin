package web

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

const hiddenValue = "CLASSIFIED-9999"

// adminOnlyBoard: a board of ann's with bob as an ordinary member, and a card
// holding an admin-only field's value beside an ordinary one.
func adminOnlyBoard(t *testing.T, w *testWeKan) (rb restBoard, annTok, bobTok, bobID, secret string) {
	t.Helper()
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	annTok = login(t, w, "ann")
	rb = newRESTBoard(t, w, annTok, "Budget")
	setMember(t, w, rb.ID, Member{UserID: bob.ID, IsActive: true})
	ctx := context.Background()
	secret, public := "fsecret"+randomID()[:8], "fpublic"+randomID()[:8]
	_, _ = w.db.Collection("customFields").InsertOne(ctx, bson.M{"_id": secret, "boardIds": bson.A{rb.ID}, "name": "Secret budget", "type": "text", "adminOnly": true})
	_, _ = w.db.Collection("customFields").InsertOne(ctx, bson.M{"_id": public, "boardIds": bson.A{rb.ID}, "name": "Public note", "type": "text"})
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": rb.Card}, bson.M{"$set": bson.M{
		"customFields":  bson.A{bson.M{"_id": secret, "value": hiddenValue}, bson.M{"_id": public, "value": "VISIBLE"}},
		"integrityHash": "digest-of-" + hiddenValue}})
	return rb, annTok, login(t, w, "bob"), bob.ID, secret
}

func getWith(t *testing.T, w *testWeKan, token, path string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", w.srv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := testClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func TestAdminOnlyFieldValueOverREST(t *testing.T) {
	w := newTestWeKan(t)
	rb, annTok, bobTok, _, secret := adminOnlyBoard(t, w)
	for _, p := range []string{"/api/cards/" + rb.Card, "/api/boards/" + rb.ID + "/lists/" + rb.List + "/cards/" + rb.Card} {
		if _, got := getWith(t, w, annTok, p); !strings.Contains(got, hiddenValue) {
			t.Errorf("the board's admin reads the value at %s: %s", p, got)
		}
		code, got := getWith(t, w, bobTok, p)
		if code != 200 || strings.Contains(got, hiddenValue) || !strings.Contains(got, "VISIBLE") || !strings.Contains(got, secret) {
			t.Errorf("a member at %s: %d %s - the value hidden, the field and the ordinary value kept", p, code, got)
		}
	}
	// Finding cards by the hidden value tells nothing.
	if code, got := getWith(t, w, bobTok, "/api/boards/"+rb.ID+"/cardsByCustomField/"+secret+"/"+hiddenValue); code != 403 || strings.Contains(got, rb.Card) {
		t.Errorf("negative: a member searched by the hidden value: %d %s", code, got)
	}
	if code, _ := getWith(t, w, bobTok, "/api/boards/"+rb.ID+"/cardsByCustomField/fnotdefined0001/x"); code != 403 {
		t.Errorf("negative: a field not defined on the board: %d", code)
	}
	if _, got := getWith(t, w, annTok, "/api/boards/"+rb.ID+"/cardsByCustomField/"+secret+"/"+hiddenValue); !strings.Contains(got, rb.Card) {
		t.Errorf("the admin finds them: %s", got)
	}
}

// Negative, over every GET route of the REST API: none sends an ordinary
// member the admin-only value - a route added later, too, since the routes
// are read from the source.
func TestNoRESTRouteSendsAnAdminOnlyValue(t *testing.T) {
	w := newTestWeKan(t)
	rb, _, bobTok, bobID, secret := adminOnlyBoard(t, w)
	values := map[string]string{"board": rb.ID, "list": rb.List, "card": rb.Card, "swimlane": rb.Swimlane, "field": secret,
		"value": hiddenValue, "user": bobID, "id": rb.Card, "chart": "burndown", "format": "json", "rule": "x", "stream": "security", "checklist": "x", "item": "x", "comment": "x"}
	routes := getAPIRoutes(t)
	if len(routes) < 50 {
		t.Fatalf("only %d GET /api routes found: the scan misses some", len(routes))
	}
	param := regexp.MustCompile(`\{([a-zA-Z.]+)\}`)
	for _, route := range routes {
		path := param.ReplaceAllStringFunc(route, func(m string) string {
			name := strings.TrimSuffix(strings.Trim(m, "{}"), "...")
			if v, ok := values[name]; ok {
				return v
			}
			t.Errorf("%s: no value for %s", route, m)
			return "x"
		})
		if _, got := getWith(t, w, bobTok, path); strings.Contains(got, hiddenValue) {
			t.Errorf("%s sends an ordinary member the admin-only value", route)
		}
	}
}

// getAPIRoutes: every "GET /api..." pattern registered in the package,
// including those built as "GET "+name+"..." from a name := "..." of the
// same file.
func getAPIRoutes(t *testing.T) []string {
	files, _ := filepath.Glob("*.go")
	literal := regexp.MustCompile(`"GET (/api[^"]*)"`)
	built := regexp.MustCompile(`"GET "\s*\+\s*(\w+)\s*\+\s*"([^"]*)"`)
	builtBare := regexp.MustCompile(`"GET "\s*\+\s*(\w+)\s*[,)]`)
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if strings.HasPrefix(p, "/api") && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		src := string(b)
		for _, m := range literal.FindAllStringSubmatch(src, -1) {
			add(m[1])
		}
		resolve := func(name string) string {
			m := regexp.MustCompile(`\b` + name + `\s*:?=\s*"([^"]*)"`).FindStringSubmatch(src)
			if m == nil {
				t.Errorf("%s: cannot resolve %s in a GET route", f, name)
				return ""
			}
			return m[1]
		}
		for _, m := range built.FindAllStringSubmatch(src, -1) {
			add(resolve(m[1]) + m[2])
		}
		for _, m := range builtBare.FindAllStringSubmatch(src, -1) {
			add(resolve(m[1]))
		}
	}
	return out
}
