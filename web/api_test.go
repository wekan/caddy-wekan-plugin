package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// call makes a REST request as a WeKan script would, and decodes the answer.
func call(t *testing.T, method, u, token, reqBody string, out any) int {
	t.Helper()
	req, _ := http.NewRequest(method, u, strings.NewReader(reqBody))
	if reqBody != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if out != nil {
		_ = json.NewDecoder(res.Body).Decode(out)
	}
	return res.StatusCode
}

func TestRESTAPI(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "kim", "pw-kim")
	w.seedUser(t, "lee", "pw-lee")
	api := w.srv.URL

	var login struct {
		ID           string `json:"id"`
		Token        string `json:"token"`
		TokenExpires string `json:"tokenExpires"`
	}
	if code := call(t, "POST", api+"/users/login", "", `{"username":"kim","password":"pw-kim"}`, &login); code != 200 || login.ID != u.ID || login.Token == "" || login.TokenExpires == "" {
		t.Fatalf("POST /users/login answers id, token, tokenExpires as WeKan: %d %+v", code, login)
	}
	// A form body works too, as with WeKan.
	req, _ := http.NewRequest("POST", api+"/users/login", strings.NewReader("email=kim@example.invalid&password=pw-kim"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != 200 {
		t.Fatalf("form login by e-mail: %v", err)
	}
	if code := call(t, "POST", api+"/users/login", "", `{"username":"kim","password":"no"}`, nil); code != 401 {
		t.Fatalf("negative: a wrong password: %d", code)
	}
	tok := login.Token

	var me User
	if code := call(t, "GET", api+"/api/user", tok, "", &me); code != 200 || me.Username != "kim" {
		t.Fatalf("GET /api/user: %d", code)
	}
	if code := call(t, "GET", api+"/api/user", "", "", nil); code != 401 {
		t.Fatalf("negative: no token: %d", code)
	}
	if code := call(t, "GET", api+"/api/user", "made-up", "", nil); code != 401 {
		t.Fatalf("negative: a made-up token: %d", code)
	}

	var created struct {
		ID                string `json:"_id"`
		DefaultSwimlaneID string `json:"defaultSwimlaneId"`
	}
	if code := call(t, "POST", api+"/api/boards", tok, `{"title":"API board","owner":"`+u.ID+`"}`, &created); code != 200 || created.ID == "" || created.DefaultSwimlaneID == "" {
		t.Fatalf("POST /api/boards: %d %+v", code, created)
	}
	if code := call(t, "POST", api+"/api/boards", tok, `{"title":"x","owner":"someoneelse"}`, nil); code != 403 {
		t.Fatalf("negative: a board for another owner: %d", code)
	}
	var boards []map[string]string
	if code := call(t, "GET", api+"/api/users/"+u.ID+"/boards", tok, "", &boards); code != 200 || len(boards) != 1 || boards[0]["title"] != "API board" {
		t.Fatalf("GET /api/users/:id/boards: %d %v", code, boards)
	}

	var list struct {
		ID string `json:"_id"`
	}
	board := api + "/api/boards/" + created.ID
	if code := call(t, "POST", board+"/lists", tok, `{"title":"Backlog"}`, &list); code != 200 || list.ID == "" {
		t.Fatalf("POST lists: %d", code)
	}
	var lists, swimlanes []map[string]string
	call(t, "GET", board+"/lists", tok, "", &lists)
	call(t, "GET", board+"/swimlanes", tok, "", &swimlanes)
	if len(lists) != 1 || lists[0]["title"] != "Backlog" || len(swimlanes) != 1 {
		t.Fatalf("lists %v swimlanes %v", lists, swimlanes)
	}

	var card struct {
		ID string `json:"_id"`
	}
	cards := board + "/lists/" + list.ID + "/cards"
	if code := call(t, "POST", cards, tok, `{"title":"Write docs","description":"all of them","swimlaneId":"`+created.DefaultSwimlaneID+`","authorId":"spoofed"}`, &card); code != 200 || card.ID == "" {
		t.Fatalf("POST cards: %d", code)
	}
	var got Card
	if code := call(t, "GET", cards+"/"+card.ID, tok, "", &got); code != 200 || got.Description != "all of them" || got.UserID != u.ID {
		t.Fatalf("GET card, its author the session's user (GHSA-6jr3-42jf-vhm5): %d %+v", code, got)
	}
	if code := call(t, "PUT", cards+"/"+card.ID, tok, `{"title":"Write more docs"}`, nil); code != 200 {
		t.Fatalf("PUT card: %d", code)
	}
	var listed []map[string]any
	call(t, "GET", cards, tok, "", &listed)
	if len(listed) != 1 || listed[0]["title"] != "Write more docs" {
		t.Fatalf("GET cards: %v", listed)
	}
	if code := call(t, "PUT", cards+"/"+card.ID, tok, `{"title":"   "}`, nil); code != 400 {
		t.Fatalf("negative: an empty title: %d", code)
	}
	if code := call(t, "POST", cards, tok, `{"title":"x","swimlaneId":"nope"}`, nil); code != 400 {
		t.Fatalf("negative: a swimlane not of the board: %d", code)
	}

	// Another user: no reading, no writing, no listing.
	var other struct{ Token string }
	call(t, "POST", api+"/users/login", "", `{"username":"lee","password":"pw-lee"}`, &other)
	for _, c := range []struct{ method, url, body string }{
		{"GET", board, ""},
		{"GET", board + "/lists", ""},
		{"GET", cards, ""},
		{"GET", cards + "/" + card.ID, ""},
		{"POST", board + "/lists", `{"title":"x"}`},
		{"POST", cards, `{"title":"x","swimlaneId":"` + created.DefaultSwimlaneID + `"}`},
		{"PUT", cards + "/" + card.ID, `{"title":"hacked"}`},
		{"DELETE", cards + "/" + card.ID, ""},
		{"GET", api + "/api/users/" + u.ID + "/boards", ""},
	} {
		if code := call(t, c.method, c.url, other.Token, c.body, nil); code != 403 {
			t.Errorf("negative: %s %s by a non-member answered %d, want 403", c.method, c.url, code)
		}
	}

	if code := call(t, "DELETE", cards+"/"+card.ID, tok, "", nil); code != 200 {
		t.Fatalf("DELETE card: %d", code)
	}
	if code := call(t, "GET", cards+"/"+card.ID, tok, "", nil); code != 404 {
		t.Fatalf("the deleted card is gone: %d", code)
	}
}

func TestRESTLoginThrottle(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "max", "pw-max")
	for i := 0; i < loginFailures; i++ {
		call(t, "POST", w.srv.URL+"/users/login", "", `{"username":"max","password":"guess"}`, nil)
	}
	if code := call(t, "POST", w.srv.URL+"/users/login", "", `{"username":"max","password":"pw-max"}`, nil); code != 429 {
		t.Fatalf("locked out after %d failures (GHSA-2g94-9x3m-hv37): %d", loginFailures, code)
	}
}
