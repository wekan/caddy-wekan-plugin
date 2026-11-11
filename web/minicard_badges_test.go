package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The minicard's assignees, as the board shows them, and - when it allows
// comments on minicards - the newest three comments, cut at 140 characters,
// of that card only.
func TestMinicardAssigneesAndComments(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	ctx := context.Background()
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": bob.ID}, bson.M{"$set": bson.M{"profile.fullname": "Bob Builder"}})
	b, _ := w.s.CreateBoard(ctx, u, "Mini")
	other, _ := w.s.CreateBoard(ctx, u, "Other")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$push": bson.M{"members": bson.M{"userId": bob.ID, "isActive": true}}})
	l, _ := w.s.AddList(ctx, b, "L")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	c, _ := w.s.AddCard(ctx, b, u, l.ID, sw.ID, "Card")
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": c.ID}, bson.M{"$set": bson.M{"assignees": bson.A{bob.ID}}})
	long := strings.Repeat("w", 150)
	for i, text := range []string{"oldest", "second", "third", long} {
		_, _ = w.db.Collection("card_comments").InsertOne(ctx, bson.M{"_id": "cm" + string(rune('a'+i)) + "0000001", "boardId": b.ID, "cardId": c.ID,
			"userId": u.ID, "text": text, "createdAt": time.Now().Add(time.Duration(i) * time.Minute)})
	}
	_, _ = w.db.Collection("card_comments").InsertOne(ctx, bson.M{"_id": "cmforeign01", "boardId": other.ID, "cardId": c.ID, "userId": u.ID, "text": "foreign", "createdAt": time.Now().Add(time.Hour)})
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	boardURL := w.srv.URL + "/b/" + b.ID + "/" + b.Slug
	_, html := get(t, ann, boardURL)
	if !strings.Contains(html, "minicard-assignees") || !strings.Contains(html, `title="Bob Builder">BB<`) {
		t.Error("the assignee's avatar, by default")
	}
	if strings.Contains(html, "minicard-comment-text") {
		t.Error("negative: no comments until the board allows them on minicards")
	}
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"allowsCommentsOnMinicard": true, "allowsAssigneeOnMinicard": false}})
	_, html = get(t, ann, boardURL)
	if strings.Contains(html, "minicard-assignees") {
		t.Error("negative: the board turned the assignees off")
	}
	if n := strings.Count(html, "minicard-comment-text"); n != 3 {
		t.Errorf("the newest three: %d", n)
	}
	if !strings.Contains(html, strings.Repeat("w", 140)+"…") || strings.Contains(html, strings.Repeat("w", 141)) {
		t.Error("cut at 140 characters")
	}
	if strings.Contains(html, ">oldest<") || strings.Contains(html, "foreign") {
		t.Error("negative: not the oldest, and not another board's comment")
	}
}
