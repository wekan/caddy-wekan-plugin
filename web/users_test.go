package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func signUp(t *testing.T, w *testWeKan, c *http.Client, username, email, password string) *http.Response {
	t.Helper()
	res, err := c.PostForm(w.srv.URL+"/sign-up", url.Values{"username": {username}, "email": {email}, "password": {password}, "password2": {password}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestUsers(t *testing.T) {
	// These pin the ENFORCED password rules; WeKan's default, which Weddy
	// follows, only reports a short password (TestPasswordPolicy).
	t.Setenv("PASSWORD_MIN_LENGTH_ENFORCE", "true")
	w := newTestWeKan(t)
	ac := w.client(t)
	if res := signUp(t, w, ac, "first", "first@example.invalid", "first-password"); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("the first sign-up: %d %s", res.StatusCode, body(t, res))
	}
	var first User
	_ = w.db.Collection("users").FindOne(context.Background(), bson.M{"username": "first"}).Decode(&first)
	if !first.IsAdmin || CheckPassword(&first, "first-password") != nil {
		t.Fatal("the first account is the administrator, with a Meteor password")
	}
	if _, page := get(t, ac, w.srv.URL+"/"); !strings.Contains(page, "js-admin-people") {
		t.Fatal("signed in after signing up, with Admin Panel / People")
	}

	bc := w.client(t)
	for name, c := range map[string][3]string{
		"a short password":             {"bob", "bob@example.invalid", "short"},
		"the password is the username": {"bobbybob", "bob@example.invalid", "bobbybob"},
		"a taken username":             {"first", "other@example.invalid", "long-enough-1"},
		"a taken e-mail address":       {"other", "FIRST@example.invalid", "long-enough-1"},
		"a bad username":               {"b/o", "bo@example.invalid", "long-enough-1"},
		"a bad e-mail address":         {"bobo", "nope", "long-enough-1"},
	} {
		if res := signUp(t, w, bc, c[0], c[1], c[2]); res.StatusCode != http.StatusBadRequest {
			t.Errorf("negative: %s: %d", name, res.StatusCode)
		}
	}
	if res := signUp(t, w, bc, "bob", "bob@example.invalid", "bob-password-1"); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("a second sign-up: %d", res.StatusCode)
	}
	var bob User
	_ = w.db.Collection("users").FindOne(context.Background(), bson.M{"username": "bob"}).Decode(&bob)
	if bob.IsAdmin {
		t.Fatal("negative: only the first account is an administrator")
	}

	// Admin Panel / People.
	if res, _ := get(t, bc, w.srv.URL+"/admin/people"); res.StatusCode != http.StatusNotFound {
		t.Fatalf("negative: a non-admin sees People: %d", res.StatusCode)
	}
	if res, _ := bc.PostForm(w.srv.URL+"/admin/people/"+first.ID, url.Values{"loginDisabled": {"true"}}); res.StatusCode != http.StatusNotFound {
		t.Fatalf("negative: a non-admin disables the admin: %d", res.StatusCode)
	}
	if res, page := get(t, ac, w.srv.URL+"/admin/people"); res.StatusCode != 200 || !strings.Contains(page, ">bob<") {
		t.Fatal("People lists the users")
	}
	if res, _ := ac.PostForm(w.srv.URL+"/admin/people/"+first.ID, url.Values{"isAdmin": {"false"}}); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("negative: an admin cannot change themself: %d", res.StatusCode)
	}
	ac.PostForm(w.srv.URL+"/admin/people/"+bob.ID, url.Values{"loginDisabled": {"true"}})
	if res, _ := get(t, bc, w.srv.URL+"/"); res.StatusCode != http.StatusSeeOther {
		t.Fatal("a disabled user is signed out")
	}
	if res := w.signIn(t, w.client(t), "bob", "bob-password-1"); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("negative: a disabled user signs in: %d", res.StatusCode)
	}
	ac.PostForm(w.srv.URL+"/admin/people/"+bob.ID, url.Values{"loginDisabled": {"false"}, "isAdmin": {"true"}})
	_ = w.db.Collection("users").FindOne(context.Background(), bson.M{"username": "bob"}).Decode(&bob)
	if !bob.IsAdmin || bob.LoginDisabled {
		t.Fatal("enabled again, and an admin")
	}

	// Registration off: refused by the server, whatever a page shows.
	ac.PostForm(w.srv.URL+"/admin/registration", url.Values{"disableRegistration": {"true"}})
	if res := signUp(t, w, w.client(t), "carl", "carl@example.invalid", "carl-password-1"); res.StatusCode != http.StatusForbidden {
		t.Fatalf("negative: signing up while registration is off: %d", res.StatusCode)
	}
	if n, _ := w.db.Collection("users").CountDocuments(context.Background(), bson.M{"username": "carl"}); n != 0 {
		t.Fatal("negative: the refused sign-up made an account")
	}

	// The profile and the password.
	if res, _ := ac.PostForm(w.srv.URL+"/profile", url.Values{"fullname": {"First Admin"}, "initials": {"FA"}}); res.StatusCode != http.StatusSeeOther {
		t.Fatal("saving the profile")
	}
	_ = w.db.Collection("users").FindOne(context.Background(), bson.M{"_id": first.ID}).Decode(&first)
	if first.Profile.Fullname != "First Admin" || first.Profile.Initials != "FA" {
		t.Fatalf("the profile: %+v", first.Profile)
	}
	other := w.client(t)
	body(t, w.signIn(t, other, "first", "first-password"))
	res, _ := ac.PostForm(w.srv.URL+"/profile/password", url.Values{"oldPassword": {"wrong"}, "newPassword": {"new-password-1"}, "newPassword2": {"new-password-1"}})
	if !strings.Contains(res.Header.Get("Location"), "error=") {
		t.Fatal("negative: changing the password without the current one")
	}
	res, _ = ac.PostForm(w.srv.URL+"/profile/password", url.Values{"oldPassword": {"first-password"}, "newPassword": {"new-password-1"}, "newPassword2": {"new-password-1"}})
	if res.Header.Get("Location") != "/profile?saved=password" {
		t.Fatalf("changing the password: %s", res.Header.Get("Location"))
	}
	if res, _ := get(t, ac, w.srv.URL+"/"); res.StatusCode != 200 {
		t.Fatal("this session stays signed in")
	}
	if res, _ := get(t, other, w.srv.URL+"/"); res.StatusCode != http.StatusSeeOther {
		t.Fatal("the other sessions end when the password changes")
	}
	if res := w.signIn(t, w.client(t), "first", "new-password-1"); res.StatusCode != http.StatusSeeOther {
		t.Fatal("the new password signs in")
	}
}
