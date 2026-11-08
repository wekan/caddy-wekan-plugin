package web

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestRoleFlags(t *testing.T) {
	for name, want := range map[string]string{"normal": "normal", "Member": "normal", "ADMIN": "board-admin", "comment": "comment-only",
		"commentOnly": "comment-only", "normalAssignedOnly": "normal-assigned-only", "commentAssignedOnly": "comment-assigned-only",
		"readOnly": "read-only", "readAssignedOnly": "read-assigned-only", "worker": "worker", "noComments": "no-comments"} {
		m, ok := roleFlags(name)
		m.IsActive = true
		if !ok || m.Role() != want {
			t.Errorf("%s: %s, want %s", name, m.Role(), want)
		}
	}
	if _, ok := roleFlags("superuser"); ok {
		t.Error("negative: a name that is no role")
	}
}

func TestRESTBoardsTitleAndMemberRoles(t *testing.T) {
	w := newTestWeKan(t)
	ann := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	a, bt := login(t, w, "ann"), login(t, w, "bob")
	b := newRESTBoard(t, w, a, "Roles")
	ctx := context.Background()
	board := w.srv.URL + "/api/boards/" + b.ID
	call(t, "POST", board+"/members/"+bob.ID+"/add", a, `{"action":"add","role":"normal"}`, nil)

	// A member's role, by name or by its flags; only a board admin sets it.
	if code := call(t, "POST", board+"/members/"+bob.ID, a, `{"role":"readOnly"}`, nil); code != 200 {
		t.Fatalf("role: %d", code)
	}
	var brd Board
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": b.ID}).Decode(&brd)
	if m := brd.member(bob.ID); m == nil || m.Role() != "read-only" {
		t.Fatalf("read-only: %+v", m)
	}
	call(t, "POST", board+"/members/"+bob.ID, a, `{"isWorker":"true"}`, nil)
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": b.ID}).Decode(&brd)
	if m := brd.member(bob.ID); m.Role() != "worker" {
		t.Fatalf("by flags: %s", m.Role())
	}
	if code := call(t, "POST", board+"/members/"+bob.ID, a, `{"role":"superuser"}`, nil); code != 400 {
		t.Fatalf("negative: no such role: %d", code)
	}
	if code := call(t, "POST", board+"/members/"+bob.ID, bt, `{"role":"admin"}`, nil); code != 403 {
		t.Fatalf("negative: a member makes themselves admin: %d", code)
	}
	if code := call(t, "POST", board+"/members/"+ann.ID, a, `{"role":"readOnly"}`, nil); code != 200 {
		t.Fatalf("own role: %d", code)
	}
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": b.ID}).Decode(&brd)
	if !brd.AdminOf(ann.ID) {
		t.Fatal("negative: nobody drops their own admin flag this way")
	}

	// The title: a board admin only.
	if code := call(t, "PUT", board+"/title", bt, `{"title":"Mine now"}`, nil); code != 403 {
		t.Fatalf("negative: not the board's admin: %d", code)
	}
	if code := call(t, "PUT", board+"/title", a, `{"title":"Renamed"}`, nil); code != 200 {
		t.Fatalf("title: %d", code)
	}
	if code := call(t, "PUT", board+"/title", a, `{"title":"  "}`, nil); code != 400 {
		t.Fatalf("negative: an empty title: %d", code)
	}

	// The boards anybody may open, and their count: a site admin only.
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"permission": "public"}})
	_, _ = w.db.Collection("boards").InsertOne(ctx, bson.M{"_id": "template0000001", "title": "^Templates^", "type": "board", "permission": "public"})
	if code := call(t, "GET", w.srv.URL+"/api/boards", a, "", nil); code != 403 {
		t.Fatalf("negative: not a site admin: %d", code)
	}
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": ann.ID}, bson.M{"$set": bson.M{"isAdmin": true}})
	var all []map[string]string
	if code := call(t, "GET", w.srv.URL+"/api/boards", a, "", &all); code != 200 || len(all) != 1 || all[0]["title"] != "Renamed" {
		t.Fatalf("the public boards, not WeKan's own caret-titled ones: %d %v", code, all)
	}
	var count map[string]int
	if code := call(t, "GET", w.srv.URL+"/api/boards_count", a, "", &count); code != 200 || count["public"] != 2 {
		t.Fatalf("boards_count: %d %v", code, count)
	}
}
