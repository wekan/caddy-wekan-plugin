package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// #5171: the tray grouping - member, else board, else Admin Panel - puts one
// card's or one board's entries in one row of the drawer.
func TestDrawerGrouping(t *testing.T) {
	w := newTestWeKan(t)
	ann := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	a := login(t, w, "ann")
	b := newRESTBoard(t, w, a, "Grouped")
	var other struct {
		ID string `json:"_id"`
	}
	call(t, "POST", w.srv.URL+"/api/boards/"+b.ID+"/lists/"+b.List+"/cards", a, `{"title":"Other","swimlaneId":"`+b.Swimlane+`"}`, &other)
	ctx := context.Background()
	notes := bson.A{}
	for i, card := range []string{b.Card, b.Card, other.ID} {
		id := "grpactivity0000" + string(rune('a'+i))
		_, _ = w.db.Collection("activities").InsertOne(ctx, bson.M{"_id": id, "activityType": "createCard", "userId": bob.ID, // inserted, not a filter
			"boardId": b.ID, "cardId": card, "listId": b.List, "createdAt": time.Now().Add(time.Duration(i) * time.Second)}) // inserted, not a filter
		notes = append(notes, bson.M{"activity": id, "read": nil})
	}
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": ann.ID}, bson.M{"$set": bson.M{"profile.notifications": notes}})
	c := w.client(t)
	body(t, w.signIn(t, c, "ann", "pw"))
	drawer := func() string {
		_, html := get(t, c, w.srv.URL+"/")
		i := strings.Index(html, `id="notifications-drawer"`)
		j := strings.Index(html[i:], "header-user")
		return html[i : i+j]
	}
	if d := drawer(); strings.Contains(d, "js-toggle-notification-group") || strings.Count(d, `<li class="notification `) != 3 {
		t.Fatalf("built in: every entry on its own: %s", d)
	}
	set := func(coll string, filter bson.M, field string, v any) {
		_, _ = w.db.Collection(coll).UpdateOne(ctx, filter, bson.M{"$set": bson.M{field: v}})
	}
	set("settings", bson.M{}, "notificationDelivery", bson.M{"tray": bson.M{"grouping": "board"}})
	if d := drawer(); strings.Count(d, "js-toggle-notification-group") != 1 || !strings.Contains(d, "3 notifications on board Grouped") || strings.Count(d, "is-group-rest") != 2 {
		t.Errorf("Admin Panel: by board: %s", d)
	}
	set("boards", bson.M{"_id": b.ID}, "notificationDelivery", bson.M{"tray": bson.M{"grouping": "card"}})
	if d := drawer(); strings.Count(d, "js-toggle-notification-group") != 1 || !strings.Contains(d, "2 notifications on card C") || strings.Count(d, "is-group-rest") != 1 {
		t.Errorf("the board wins: by card: %s", d)
	}
	set("users", bson.M{"_id": ann.ID}, "profile.notificationDelivery", bson.M{"tray": bson.M{"grouping": "none"}})
	if d := drawer(); strings.Contains(d, "js-toggle-notification-group") {
		t.Errorf("the member wins: none: %s", d)
	}
	set("users", bson.M{"_id": ann.ID}, "profile.notificationDelivery", bson.M{"tray": bson.M{"grouping": "$where"}})
	if d := drawer(); !strings.Contains(d, "2 notifications on card C") {
		t.Errorf("negative: an unknown grouping is no setting, and the board's holds: %s", d)
	}
}
