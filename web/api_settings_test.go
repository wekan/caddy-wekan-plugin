package web

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestRESTBoardDomainsAndSettings(t *testing.T) {
	w := newTestWeKan(t)
	ann := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	a, bt := login(t, w, "ann"), login(t, w, "bob")
	b := newRESTBoard(t, w, a, "Domains")
	ctx := context.Background()
	domains := w.srv.URL + "/api/boards/" + b.ID + "/domains"

	var got []map[string]any
	if code := call(t, "POST", domains, a, `{"domain":"  Example.COM "}`, &got); code != 200 || len(got) != 1 || got[0]["domain"] != "example.com" || got[0]["isActive"] != true {
		t.Fatalf("add, normalized: %d %v", code, got)
	}
	call(t, "POST", domains, a, `{"domain":"example.com"}`, &got)
	if len(got) != 1 {
		t.Fatal("negative: a domain once")
	}
	for _, bad := range []string{`{"domain":"nodot"}`, `{"domain":"a@b.c"}`, `{"domain":"a b.c"}`, `{}`} {
		if code := call(t, "POST", domains, a, bad, nil); code != 400 {
			t.Fatalf("negative: %s: %d", bad, code)
		}
	}
	setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: true})
	if code := call(t, "POST", domains, bt, `{"domain":"evil.example"}`, nil); code != 403 {
		t.Fatalf("negative: a member who is not a board admin: %d", code)
	}
	if code := call(t, "GET", domains, a, "", nil); code != 403 {
		t.Fatalf("negative: reading them is for a site admin: %d", code)
	}
	call(t, "DELETE", domains+"/EXAMPLE.com", a, "", &got)
	if len(got) != 0 {
		t.Fatalf("deleted: %v", got)
	}

	// Settings: a site admin only, only REST's fields, never mailServer.
	if code := call(t, "GET", w.srv.URL+"/api/settings", a, "", nil); code != 403 {
		t.Fatalf("negative: not a site admin: %d", code)
	}
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": ann.ID}, bson.M{"$set": bson.M{"isAdmin": true}})
	var set map[string]any
	if code := call(t, "PUT", w.srv.URL+"/api/settings", a, `{"productName":"Our WeKan","disableRegistration":true,"mailServer":{"password":"x"},"isAdmin":true}`, &set); code != 200 {
		t.Fatalf("put: %d", code)
	}
	if set["productName"] != "Our WeKan" || set["disableRegistration"] != true || set["mailServer"] != nil {
		t.Fatalf("set: %v", set)
	}
	var raw bson.M
	_ = w.db.Collection("settings").FindOne(ctx, bson.M{}).Decode(&raw)
	// The settings document carries WeKan's default mailServer from the start,
	// so the check is that the request's password did not reach it.
	if ms, _ := raw["mailServer"].(bson.M); ms != nil && ms["password"] != "" {
		t.Fatal("negative: mailServer is never written over REST")
	}
	if code := call(t, "PUT", w.srv.URL+"/api/settings", a, `{"productName":{"$set":1}}`, nil); code != 400 {
		t.Fatalf("negative: an object: %d", code)
	}
	_, _ = w.db.Collection("settings").UpdateOne(ctx, bson.M{}, bson.M{"$set": bson.M{"mailServer": bson.M{"password": "secret"}}})
	call(t, "GET", w.srv.URL+"/api/settings", a, "", &set)
	if set["mailServer"] != nil || set["productName"] != "Our WeKan" {
		t.Fatalf("negative: mailServer is never read over REST: %v", set)
	}
}
