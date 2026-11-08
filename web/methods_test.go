package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestMeteorMethods(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	c := w.client(t)
	body(t, w.signIn(t, c, "ann", "pw"))
	post := func(cl *http.Client, name, ctype, args string) int {
		req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/"+name, strings.NewReader(args))
		req.Header.Set("Content-Type", ctype)
		res, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if code := post(c, "setLanguage", "application/json", `["la"]`); code != 200 {
		t.Fatalf("setLanguage: %d", code)
	}
	var doc struct {
		Profile struct{ Language string } `bson:"profile"`
	}
	_ = w.db.Collection("users").FindOne(context.Background(), bson.M{"_id": u.ID}).Decode(&doc)
	if doc.Profile.Language != "la" {
		t.Fatalf("kept in profile.language: %q", doc.Profile.Language)
	}
	for _, c2 := range []struct {
		name, ctype, args string
		want              int
	}{
		{"setLanguage", "application/json", `["xx-nowhere"]`, 400},
		{"setLanguage", "application/json", `[5]`, 400},
		{"noSuchMethod", "application/json", `[]`, 404},
		{"setLanguage", "text/plain", `["fi"]`, 415},
		{"setLanguage", "application/x-www-form-urlencoded", `["fi"]`, 415},
	} {
		if code := post(c, c2.name, c2.ctype, c2.args); code != c2.want {
			t.Errorf("negative: %s %s %s: %d, want %d", c2.name, c2.ctype, c2.args, code, c2.want)
		}
	}
	if code := post(testClient, "setLanguage", "application/json", `["fi"]`); code != 401 {
		t.Fatalf("negative: nobody signed in: %d", code)
	}
}
