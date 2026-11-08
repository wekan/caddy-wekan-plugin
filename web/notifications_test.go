package web

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestNotificationsAsWeKan(t *testing.T) {
	w := newTestWeKan(t)
	ann := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	cid := w.seedUser(t, "cid", "pw")
	out := w.seedUser(t, "out", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, ann, "Notes")
	l, _ := w.s.AddList(ctx, b, "Todo")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	c, _ := w.s.AddCard(ctx, b, ann, l.ID, sw.ID, "Alpha")
	setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: true})
	setMember(t, w, b.ID, Member{UserID: cid.ID, IsActive: true})
	count := func(u *User) int { return w.s.unreadNotifications(ctx, u.ID) }
	watchers := func(v bson.A) {
		_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"watchers": v}})
	}
	comment := func(by *User, text string) {
		var full Board
		_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": b.ID}).Decode(&full)
		if _, err := w.s.AddComment(ctx, &full, by, c, text); err != nil {
			t.Fatal(err)
		}
	}

	// #6658: a board nobody watches notifies nobody - not even a mention.
	comment(ann, "@bob look")
	if count(bob) != 0 {
		t.Fatal("negative: a muted (not watching) member is not notified")
	}
	// An explicit list watcher is, on a board nobody watches.
	_, _ = w.db.Collection("lists").UpdateOne(ctx, bson.M{"_id": l.ID}, bson.M{"$set": bson.M{"watchers": bson.A{bob.ID, out.ID}}})
	comment(ann, "for the list")
	if count(bob) != 1 {
		t.Fatalf("a list watcher gets the comment: %d", count(bob))
	}
	if count(out) != 0 {
		t.Fatal("negative: a list watcher who is not on the board gets nothing")
	}
	// Watching the board: every activity; tracking: what one takes part in, or a mention.
	watchers(bson.A{bson.M{"userId": cid.ID, "level": "watching"}, bson.M{"userId": ann.ID, "level": "tracking"}})
	comment(bob, "anything")
	if count(cid) != 1 {
		t.Fatalf("a watching member gets every activity: %d", count(cid))
	}
	before := count(ann)
	comment(bob, "@ann needs attention")
	if count(ann) != before+1 {
		t.Fatal("a tracking member is notified of a mention")
	}
	comment(ann, "my own note")
	if count(ann) != before+1 {
		t.Fatal("negative: never of one's own activity")
	}
	comment(ann, "@ann remind me")
	if count(ann) != before+2 {
		t.Fatal("but of one's own mention of oneself, as WeKan")
	}
	// A muted member assigned to the card is not opted back in (#6658).
	watchers(bson.A{bson.M{"userId": bob.ID, "level": "muted"}})
	_, _ = w.db.Collection("lists").UpdateOne(ctx, bson.M{"_id": l.ID}, bson.M{"$set": bson.M{"watchers": bson.A{}}})
	b2 := count(bob)
	w.s.activity(ctx, ann.ID, "joinMember", bson.M{"boardId": b.ID, "cardId": c.ID, "memberId": bob.ID, "listId": l.ID})
	if count(bob) != b2 {
		t.Fatal("negative: assigning a muted member creates no notification")
	}
	// Each activity once, whatever the reasons to be told.
	var u struct {
		Profile struct {
			Notifications []bson.M `bson:"notifications"`
		} `bson:"profile"`
	}
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"_id": cid.ID}).Decode(&u)
	seen := map[any]bool{}
	for _, n := range u.Profile.Notifications {
		if seen[n["activity"]] || n["read"] != nil {
			t.Fatalf("each activity once, unread: %v", u.Profile.Notifications)
		}
		seen[n["activity"]] = true
	}
}

func TestNotificationIndicator(t *testing.T) {
	w := newTestWeKan(t)
	ann := w.seedUser(t, "ann", "pw")
	c := w.client(t)
	body(t, w.signIn(t, c, "ann", "pw"))
	_, html := get(t, c, w.srv.URL+"/")
	hasAlert := func(html string) bool {
		m := regexp.MustCompile(`class="([^"]*notifications-drawer-toggle[^"]*)"`).FindStringSubmatch(html)
		return m != nil && slices.Contains(strings.Fields(m[1]), "alert")
	}
	if !strings.Contains(html, "notifications-drawer-toggle") || hasAlert(html) {
		t.Fatal("the indicator, without alert when nothing is unread")
	}
	_, _ = w.db.Collection("users").UpdateOne(context.Background(), bson.M{"_id": ann.ID},
		bson.M{"$set": bson.M{"profile.notifications": bson.A{bson.M{"activity": "act000000000001", "read": nil}}}})
	if _, html = get(t, c, w.srv.URL+"/"); !hasAlert(html) {
		t.Fatal("alert while something is unread")
	}
	var unread map[string]int
	res, _ := c.Get(w.srv.URL + "/notifications/unread")
	_ = json.NewDecoder(res.Body).Decode(&unread)
	res.Body.Close()
	if unread["unread"] != 1 {
		t.Fatalf("unread: %v", unread)
	}
}
