package web

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestRESTAdminProblems(t *testing.T) {
	w := newTestWeKan(t)
	ann := w.seedUser(t, "ann", "pw")
	w.seedUser(t, "bob", "pw")
	a, bt := login(t, w, "ann"), login(t, w, "bob")
	ctx := context.Background()
	mine, hidden := newRESTBoard(t, w, a, "Mine"), newRESTBoard(t, w, bt, "Hidden")
	// bob attempts ParentBleed: a row in the security stream.
	call(t, "POST", w.srv.URL+"/api/boards/"+hidden.ID+"/lists/"+hidden.List+"/cards", bt,
		`{"title":"x","swimlaneId":"`+hidden.Swimlane+`","parentId":"`+mine.Card+`"}`, nil)
	_, _ = w.db.Collection("cards").InsertOne(ctx, bson.M{"_id": "brokencard00001", "title": "lost", "boardId": mine.ID, "listId": ""})
	api := w.srv.URL + "/api/admin/problems"
	if code := call(t, "GET", api, a, "", nil); code != 403 {
		t.Fatalf("negative: not a site admin: %d", code)
	}
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": ann.ID}, bson.M{"$set": bson.M{"isAdmin": true}})
	var over map[string]any
	if code := call(t, "GET", api, a, "", &over); code != 200 || over["anyProblems"] != true {
		t.Fatalf("overview: %d %v", code, over)
	}
	areas, _ := over["newProblems"].([]any)
	if len(areas) != 1 || areas[0].(map[string]any)["stream"] != "security" {
		t.Fatalf("the security stream has a new problem: %v", areas)
	}
	var page map[string]any
	if code := call(t, "GET", api+"/security?search=parent&limit=5", a, "", &page); code != 200 || page["total"] != float64(1) {
		t.Fatalf("the stream, searched: %d %v", code, page)
	}
	call(t, "GET", api+"/security?search=nomatch", a, "", &page)
	if page["total"] != float64(0) {
		t.Fatal("negative: a search that matches nothing")
	}
	if code := call(t, "GET", api+"/nosuchstream", a, "", nil); code != 404 {
		t.Fatalf("negative: an unknown stream: %d", code)
	}
	if code := call(t, "POST", api+"/security/acknowledge", a, "", nil); code != 200 {
		t.Fatalf("acknowledge: %d", code)
	}
	call(t, "GET", api, a, "", &over)
	if areas, _ := over["newProblems"].([]any); len(areas) != 0 {
		t.Fatalf("acknowledged: %v", areas)
	}
	if code := call(t, "POST", api+"/api/acknowledge", a, "", nil); code != 404 {
		t.Fatalf("negative: the api stream is read, not acknowledged: %d", code)
	}
}
