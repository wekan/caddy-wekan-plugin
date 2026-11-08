package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestAdminPeople(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	boss := w.seedUser(t, "boss", "pw")
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": boss.ID}, bson.M{"$set": bson.M{"isAdmin": true}})
	bob := w.seedUser(t, "bob", "pw")
	w.seedUser(t, "taken", "pw")
	ac, bc := w.client(t), w.client(t)
	body(t, w.signIn(t, ac, "boss", "pw"))
	body(t, w.signIn(t, bc, "bob", "pw"))
	if _, page := get(t, ac, w.srv.URL+"/admin/people"); !strings.Contains(page, `class="admin-panel-tabs"`) || !strings.Contains(page, `data-id="people-setting"`) ||
		!strings.Contains(page, `class="edit-user"`) || !strings.Contains(page, `data-pop="edit-user-`+bob.ID+`"`) {
		t.Fatalf("WeKan's People pane: %.300s", page)
	}
	if res, page := get(t, bc, w.srv.URL+"/people"); strings.Contains(page, `class="username"`) || res.StatusCode != 404 {
		t.Errorf("negative: a non-admin sees no users: %d", res.StatusCode)
	}
	edit := func(c *http.Client, id string, v url.Values) string {
		res, err := c.PostForm(w.srv.URL+"/admin/people/"+id+"/edit", v)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.Header.Get("Location")
	}
	user := func(id string) User {
		var u User
		_ = w.db.Collection("users").FindOne(ctx, bson.M{"_id": id}).Decode(&u)
		return u
	}
	if loc := edit(ac, bob.ID, url.Values{"username": {"robert"}, "fullname": {"Robert B"}, "initials": {"RB"}, "isAdmin": {"false"},
		"loginDisabled": {"false"}, "password": {"New-password-77"}}); loc != "/admin/people" {
		t.Fatalf("saved: %s", loc)
	}
	if u := user(bob.ID); u.Username != "robert" || u.Profile.Fullname != "Robert B" || u.Profile.Initials != "RB" || CheckPassword(&u, "New-password-77") != nil {
		t.Errorf("edited: %+v", u)
	}
	if res, _ := get(t, bc, w.srv.URL+"/"); res.StatusCode != http.StatusSeeOther {
		t.Errorf("a new password signs the user out everywhere: %d", res.StatusCode)
	}
	if loc := edit(ac, bob.ID, url.Values{"username": {"taken"}}); !strings.Contains(loc, "error=") || user(bob.ID).Username != "robert" {
		t.Errorf("negative: a username taken: %s", loc)
	}
	if loc := edit(ac, boss.ID, url.Values{"username": {"boss"}, "isAdmin": {"false"}, "loginDisabled": {"true"}}); loc != "/admin/people" || !user(boss.ID).IsAdmin || user(boss.ID).LoginDisabled {
		t.Errorf("negative: the admin cannot take away their own admin or access: %s", loc)
	}
	nb := w.client(t)
	body(t, w.signIn(t, nb, "robert", "New-password-77"))
	if loc := edit(nb, boss.ID, url.Values{"fullname": {"pwned"}}); loc != "" || user(boss.ID).Profile.Fullname == "pwned" {
		t.Errorf("negative: a non-admin edits nobody: %s", loc)
	}
	// setCreateUser.
	call := func(c *http.Client, args string) map[string]any {
		req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/setCreateUser", strings.NewReader(args))
		req.Header.Set("Content-Type", "application/json")
		res, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		return out
	}
	args := func(username, isAdmin string) string {
		return `["New User","` + username + `","NU","Created-pass-88","` + isAdmin + `","false","` + username + `@example.org",[],[],[]]`
	}
	if out := call(ac, args("newuser", "false")); out["error"] != nil || out["result"] == nil {
		t.Fatalf("created: %v", out)
	}
	var nu User
	if w.db.Collection("users").FindOne(ctx, bson.M{"username": "newuser"}).Decode(&nu) != nil || nu.Profile.Fullname != "New User" || CheckPassword(&nu, "Created-pass-88") != nil || nu.IsAdmin {
		t.Errorf("as asked: %+v", nu)
	}
	for name, c := range map[string]struct {
		client *http.Client
		args   string
		error  string
	}{
		"a non-admin":      {nb, args("other1", "false"), "not-authorized"},
		"a username taken": {ac, args("newuser", "false"), "username-already-taken"},
		"not true/false":   {ac, args("other2", "yes"), "invalid-parameters"},
	} {
		if out := call(c.client, c.args); out["error"] != c.error {
			t.Errorf("negative: %s: %v", name, out)
		}
	}
}
