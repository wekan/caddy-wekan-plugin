package web

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func rawBody(t *testing.T, method, url, token, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := testClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func TestRESTUsersForASiteAdmin(t *testing.T) {
	w := newTestWeKan(t)
	ann := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	a, bt := login(t, w, "ann"), login(t, w, "bob")
	ctx := context.Background()
	b := newRESTBoard(t, w, bt, "Bob's")
	api := w.srv.URL + "/api"

	// Not a site admin: refused everywhere.
	for _, c := range [][3]string{{"GET", "/users", ""}, {"GET", "/users/" + bob.ID, ""}, {"PUT", "/users/" + bob.ID, `{"action":"disableLogin"}`},
		{"POST", "/users/", `{"username":"x1","email":"x1@example.invalid","password":"long enough"}`},
		{"POST", "/createtoken/" + bob.ID, `{"reason":"x"}`}, {"POST", "/deletetoken", `{"userId":"` + bob.ID + `"}`}} {
		if code := call(t, c[0], api+c[1], a, c[2], nil); code != 403 {
			t.Fatalf("negative: %s %s by a non-admin: %d", c[0], c[1], code)
		}
	}
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": ann.ID}, bson.M{"$set": bson.M{"isAdmin": true}})

	var list []map[string]string
	if code := call(t, "GET", api+"/users", a, "", &list); code != 200 || len(list) != 2 {
		t.Fatalf("users: %d %v", code, list)
	}
	for _, path := range []string{"/users/" + bob.ID, "/users/bob", "/user"} {
		code, body := rawBody(t, "GET", api+path, a, "")
		if code != 200 || strings.Contains(body, "bcrypt") || strings.Contains(body, "hashedToken") || strings.Contains(body, "services") {
			t.Fatalf("negative: %s carries no secret (GHSA-6qpx-x7vr-p9w6): %d %s", path, code, body)
		}
	}
	var one map[string]any
	call(t, "GET", api+"/users/bob", a, "", &one)
	if boards, _ := one["boards"].([]any); len(boards) != 1 || boards[0].(map[string]any)["boardId"] != b.ID {
		t.Fatalf("a user by name, with their boards: %v", one["boards"])
	}
	if code := call(t, "GET", api+"/users/nobody", a, "", nil); code != 404 {
		t.Fatalf("negative: no such user: %d", code)
	}

	// Disable signs the user out everywhere; enable lets them sign in again.
	call(t, "PUT", api+"/users/"+bob.ID, a, `{"action":"disableLogin"}`, nil)
	if code := call(t, "GET", api+"/user", bt, "", nil); code != 401 {
		t.Fatalf("a disabled user's token no longer works: %d", code)
	}
	call(t, "PUT", api+"/users/"+ann.ID, a, `{"action":"disableLogin"}`, nil)
	if code := call(t, "GET", api+"/user", a, "", nil); code != 200 {
		t.Fatal("negative: an admin does not disable themselves")
	}
	call(t, "PUT", api+"/users/"+bob.ID, a, `{"action":"enableLogin"}`, nil)
	if login(t, w, "bob") == "" {
		t.Fatal("enabled again")
	}

	// takeOwnership: the admin takes bob's place on the boards bob administers.
	var owned []map[string]string
	if code := call(t, "PUT", api+"/users/"+bob.ID, a, `{"action":"takeOwnership"}`, &owned); code != 200 || len(owned) != 1 || owned[0]["_id"] != b.ID {
		t.Fatalf("takeOwnership: %d %v", code, owned)
	}
	var brd Board
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": b.ID}).Decode(&brd)
	if !brd.AdminOf(ann.ID) || brd.MemberOf(bob.ID) {
		t.Fatalf("the board is the admin's now: %+v", brd.Members)
	}

	// A new account; a duplicate, a short password and a bad address refused.
	var nu struct {
		ID string `json:"_id"`
	}
	if code := call(t, "POST", api+"/users/", a, `{"username":"cid","email":"cid@example.invalid","password":"a long password"}`, &nu); code != 200 || nu.ID == "" {
		t.Fatalf("create: %d", code)
	}
	for _, bad := range []string{`{"username":"cid","email":"c2@example.invalid","password":"a long password"}`,
		`{"username":"dan","email":"cid@example.invalid","password":"a long password"}`,
		`{"username":"dan","email":"dan@example.invalid","password":"short"}`,
		`{"username":"dan","email":"not an address","password":"a long password"}`, `{"username":"d$n","email":"dan@example.invalid","password":"a long password"}`} {
		if code := call(t, "POST", api+"/users/", a, bad, nil); code != 400 && code != 403 {
			t.Fatalf("negative: %s: %d", bad, code)
		}
	}

	// A token for another user needs a reason, which is kept.
	if code := call(t, "POST", api+"/createtoken/"+nu.ID, a, `{}`, nil); code != 400 {
		t.Fatalf("negative: no reason: %d", code)
	}
	var tok struct {
		AuthToken string `json:"authToken"`
	}
	if code := call(t, "POST", api+"/createtoken/"+nu.ID, a, `{"reason":"support ticket 42"}`, &tok); code != 200 || tok.AuthToken == "" {
		t.Fatalf("createtoken: %d", code)
	}
	var me map[string]any
	if call(t, "GET", api+"/user", tok.AuthToken, "", &me); me["username"] != "cid" {
		t.Fatalf("the token is cid's: %v", me)
	}
	if n, _ := w.db.Collection("impersonatedUsers").CountDocuments(ctx, bson.M{"adminId": ann.ID, "userId": nu.ID, "reason": "restCreateToken: support ticket 42"}); n != 1 {
		t.Fatal("the reason is kept in impersonatedUsers")
	}
	call(t, "POST", api+"/deletetoken", a, `{"userId":"`+nu.ID+`","token":"`+tok.AuthToken+`"}`, nil)
	if code := call(t, "GET", api+"/user", tok.AuthToken, "", nil); code != 401 {
		t.Fatalf("the token is deleted: %d", code)
	}
	tok2 := login(t, w, "bob")
	call(t, "POST", api+"/deletetoken", a, `{"userId":"`+bob.ID+`","token":"`+hashToken(tok2)+`"}`, nil)
	if code := call(t, "GET", api+"/user", tok2, "", nil); code != 401 {
		t.Fatalf("a token given by its stored hash is deleted too: %d", code)
	}
	tok3 := login(t, w, "bob")
	call(t, "POST", api+"/deletetoken", a, `{"userId":"`+bob.ID+`"}`, nil)
	if code := call(t, "GET", api+"/user", tok3, "", nil); code != 401 {
		t.Fatalf("all of a user's tokens: %d", code)
	}
}
