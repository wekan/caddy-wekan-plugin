package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestInactivityArchive(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	board := newRESTBoard(t, w, login(t, w, "ann"), "Inactivity")
	setMember(t, w, board.ID, Member{UserID: bob.ID, IsActive: true})
	admin, member := w.client(t), w.client(t)
	body(t, w.signIn(t, admin, "ann", "pw"))
	body(t, w.signIn(t, member, "bob", "pw"))
	post := func(client *http.Client, form string) int {
		t.Helper()
		response, err := client.Post(w.srv.URL+"/b/"+board.ID+"/auto-archive",
			"application/x-www-form-urlencoded", strings.NewReader(form))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		return response.StatusCode
	}
	if status := post(member, "days=30"); status != http.StatusForbidden {
		t.Fatalf("ordinary member: %d", status)
	}
	if status := post(admin, "days=4000"); status != http.StatusBadRequest {
		t.Fatalf("invalid number of days: %d", status)
	}
	if status := post(admin, "days=30"); status >= 400 {
		t.Fatalf("save: %d", status)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	stale := now.AddDate(0, 0, -40)
	for _, c := range []bson.M{
		{"_id": "old", "dateLastActivity": stale},
		{"_id": "fresh", "dateLastActivity": now},
		{"_id": "child", "dateLastActivity": now, "parentId": "old"},
		{"_id": "shared-child", "dateLastActivity": now, "parentId": "old", "parentIds": bson.A{"old", "fresh"}},
		{"_id": "template", "dateLastActivity": stale, "type": "template-card"},
	} {
		c["boardId"], c["archived"], c["title"] = board.ID, false, c["_id"]
		if _, err := w.db.Collection("cards").InsertOne(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		w.s.scanInactiveCards(ctx, now)
	}
	for id, archived := range map[string]bool{"old": true, "child": true, "fresh": false, "shared-child": false, "template": false} {
		var card Card
		if err := w.db.Collection("cards").FindOne(ctx, bson.M{"_id": id}).Decode(&card); err != nil || card.Archived != archived {
			t.Errorf("%s archived = %v, want %v: %v", id, card.Archived, archived, err)
		}
	}
	if count, err := w.db.Collection("activities").CountDocuments(ctx, bson.M{"boardId": board.ID, "activityType": "archivedCard"}); err != nil || count != 2 {
		t.Fatalf("archive activities = %d: %v", count, err)
	}
	if status := post(admin, "days="); status >= 400 {
		t.Fatalf("clear: %d", status)
	}
	var stored bson.M
	if err := w.db.Collection("boards").FindOne(ctx, bson.M{"_id": board.ID}).Decode(&stored); err != nil || stored["autoArchiveInactiveDays"] != nil {
		t.Fatalf("clear did not remove setting: %v %v", stored["autoArchiveInactiveDays"], err)
	}
}
