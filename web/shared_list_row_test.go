package web

import (
	"context"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// #6902, One shared list header row (sharedListHeaders): every swimlane shows
// every list of the board, with the first swimlane's headers as the row, and
// nothing is written to a list. Negative: without it a list stays in its own
// swimlane.
func TestSharedListHeaders(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Shared")
	_, _ = w.db.Collection("swimlanes").InsertOne(ctx, bson.M{"_id": "lanetwo001", "boardId": b.ID, "title": "Second", "sort": 1, "archived": false})
	own, _ := w.s.AddList(ctx, b, "Lane two list")
	_, _ = w.db.Collection("lists").UpdateOne(ctx, bson.M{"_id": own.ID}, bson.M{"$set": bson.M{"swimlaneId": "lanetwo001"}})
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	page := func() string {
		_, html := get(t, ann, w.srv.URL+"/b/"+b.ID+"/"+b.Slug)
		return html
	}
	shown := func(html string) int { return strings.Count(html, `id="js-list-`+own.ID+`"`) }
	html := page()
	if shown(html) != 1 || strings.Contains(html, "shared-list-headers") {
		t.Fatalf("negative: without the setting the list is in its own swimlane only: %d", shown(html))
	}
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"sharedListHeaders": true}})
	lanes, _ := w.db.Collection("swimlanes").CountDocuments(ctx, bson.M{"boardId": b.ID, "archived": false})
	html = page()
	if shown(html) != int(lanes) || lanes < 2 {
		t.Errorf("every swimlane shows the list: %d of %d", shown(html), lanes)
	}
	if !strings.Contains(html, "board-canvas shared-list-headers") || strings.Count(html, "is-first-swimlane") != 1 {
		t.Error("the canvas is marked, and one swimlane is the first")
	}
	var l bson.M
	_ = w.db.Collection("lists").FindOne(ctx, bson.M{"_id": own.ID}).Decode(&l)
	if l["swimlaneId"] != "lanetwo001" {
		t.Errorf("nothing is written to the list: %v", l["swimlaneId"])
	}
}
