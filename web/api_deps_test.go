package web

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestRESTDependencies(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	a, bt := login(t, w, "ann"), login(t, w, "bob")
	b := newRESTBoard(t, w, a, "Deps")
	var c2 struct {
		ID string `json:"_id"`
	}
	call(t, "POST", w.srv.URL+"/api/boards/"+b.ID+"/lists/"+b.List+"/cards", a, `{"title":"Beta","swimlaneId":"`+b.Swimlane+`"}`, &c2)
	base := w.srv.URL + "/api/boards/" + b.ID + "/cards/" + b.Card + "/dependencies"
	deps := func() []dependency {
		var raw bson.D
		_ = w.db.Collection("cards").FindOne(context.Background(), bson.M{"_id": b.Card}).Decode(&raw)
		return normalizeDependencies(docMap(raw)["cardDependencies"])
	}

	if code := call(t, "POST", base, a, `{"cardId":"`+c2.ID+`","type":"blocks","color":"#2196f3","icon":"lock"}`, nil); code != 200 {
		t.Fatalf("add: %d", code)
	}
	if d := deps(); len(d) != 1 || d[0] != (dependency{c2.ID, "blocks", "#2196f3", "lock"}) {
		t.Fatalf("stored as WeKan stores it: %+v", d)
	}
	var lines []map[string]any
	if code := call(t, "GET", w.srv.URL+"/api/boards/"+b.ID+"/dependencies", a, "", &lines); code != 200 || len(lines) != 1 ||
		lines[0]["from"] != b.Card || lines[0]["to"] != c2.ID || lines[0]["toTitle"] != "Beta" {
		t.Fatalf("the board's dependency lines: %d %v", code, lines)
	}
	call(t, "PUT", base+"/"+c2.ID, a, `{"color":"#ff0000","type":"is-blocked-by"}`, nil)
	call(t, "POST", base, a, `{"cardId":"`+c2.ID+`","type":"fixes"}`, nil)
	if d := deps(); len(d) != 1 || d[0].Type != "fixes" || d[0].Color != "#ff0000" {
		t.Fatalf("edited, and the same target again is updated in place: %+v", d)
	}
	for _, bad := range []string{`{"cardId":"` + c2.ID + `","type":"not-a-type"}`, `{"cardId":"` + b.Card + `"}`, `{}`,
		`{"cardId":"` + c2.ID + `","color":"red;background:url(x)"}`, `{"cardId":"` + c2.ID + `","icon":"<b>"}`, `{"cardId":"nosuchcard12345"}`} {
		if code := call(t, "POST", base, a, bad, nil); code != 400 {
			t.Fatalf("negative: %s: %d", bad, code)
		}
	}
	// A non-member, a read-only member: no; an assigned-only member: only between
	// cards assigned to them.
	if code := call(t, "POST", base, bt, `{"cardId":"`+c2.ID+`"}`, nil); code != 403 {
		t.Fatalf("negative: a non-member: %d", code)
	}
	setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: true, IsReadOnly: true})
	if code := call(t, "DELETE", base+"/"+c2.ID, bt, "", nil); code != 403 {
		t.Fatalf("negative: a read-only member: %d", code)
	}
	setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: true, IsNormalAssignedOnly: true})
	if code := call(t, "DELETE", base+"/"+c2.ID, bt, "", nil); code != 403 {
		t.Fatalf("negative: an assigned-only member on a card not assigned to them: %d", code)
	}
	_, _ = w.db.Collection("cards").UpdateOne(context.Background(), bson.M{"_id": b.Card}, bson.M{"$set": bson.M{"assignees": []string{bob.ID}}})
	if code := call(t, "DELETE", base+"/"+c2.ID, bt, "", nil); code != 200 || len(deps()) != 0 {
		t.Fatalf("an assigned-only member on their own card: %d", code)
	}
	if code := call(t, "GET", base, "", "", nil); code != 401 {
		t.Fatalf("negative: no token: %d", code)
	}
}

func TestRESTCardStickersLocations(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	a := login(t, w, "ann")
	b := newRESTBoard(t, w, a, "Stickers")
	card := w.srv.URL + "/api/boards/" + b.ID + "/lists/" + b.List + "/cards/" + b.Card
	get := func() bson.M {
		var m bson.M
		_ = w.db.Collection("cards").FindOne(context.Background(), bson.M{"_id": b.Card}).Decode(&m)
		return m
	}
	call(t, "PUT", card, a, `{"dueComplete":true}`, nil)
	if get()["dueComplete"] != true {
		t.Fatal("dueComplete")
	}
	call(t, "PUT", card, a, `{"dueComplete":"false"}`, nil)
	if get()["dueComplete"] != false {
		t.Fatal(`dueComplete "false", as a form sends it`)
	}
	if code := call(t, "PUT", card, a, `{"stickers":[{"icon":"taco-cool","highlight":"round"}]}`, nil); code != 200 {
		t.Fatalf("stickers: %d", code)
	}
	if st, _ := get()["stickers"].(bson.A); len(st) != 1 {
		t.Fatalf("one sticker: %v", get()["stickers"])
	}
	if code := call(t, "PUT", card, a, `{"locations":"[{\"name\":\"HQ\",\"address\":\"Helsinki\",\"latitude\":\"60.17\",\"longitude\":24.94}]"}`, nil); code != 200 {
		t.Fatalf("locations as JSON text, as a form sends them: %d", code)
	}
	loc, _ := get()["locations"].(bson.A)
	if len(loc) != 1 {
		t.Fatalf("one location: %v", loc)
	}
	l, _ := loc[0].(bson.D)
	if m := docMap(l); m["name"] != "HQ" || m["latitude"] != 60.17 || m["longitude"] != 24.94 || m["_id"] == "" {
		t.Fatalf("a location has an _id and numeric coordinates: %v", m)
	}
	for _, bad := range []string{`{"stickers":[{"icon":"x","highlight":"blink"}]}`, `{"stickers":"not json"}`, `{"stickers":{"icon":"x"}}`,
		`{"locations":[{"name":"x","latitude":"north"}]}`, `{"locations":[{"_id":{"$ne":""}}]}`} {
		if code := call(t, "PUT", card, a, bad, nil); code != 400 {
			t.Fatalf("negative: %s: %d", bad, code)
		}
	}
}
