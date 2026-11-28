package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestWebsiteBoardView(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	board := newRESTBoard(t, w, login(t, w, "ann"), "Website fixture")
	update := func(collection, id string, fields bson.M) {
		t.Helper()
		if _, err := w.db.Collection(collection).UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": fields}); err != nil {
			t.Fatal(err)
		}
	}
	insert := func(collection string, doc bson.M) string {
		t.Helper()
		id := randomID()
		doc["_id"], doc["boardId"], doc["archived"] = id, board.ID, false
		if _, err := w.db.Collection(collection).InsertOne(ctx, doc); err != nil {
			t.Fatal(err)
		}
		return id
	}
	update("boards", board.ID, bson.M{"permission": "public", "defaultPublicBoardView": "board-view-website"})
	update("lists", board.List, bson.M{"title": "Julkaistu"})
	pages := insert("swimlanes", bson.M{"title": "Sivut"})
	drafts := insert("lists", bson.M{"title": "Drafts"})
	update("cards", board.Card, bson.M{"title": "Latest post", "receivedAt": time.Now().UTC()})
	page := insert("cards", bson.M{"title": "About us", "description": "**Published body**", "listId": board.List, "swimlaneId": pages})
	insert("cards", bson.M{"title": "Child page", "parentId": page, "listId": board.List, "swimlaneId": pages})
	draft := insert("cards", bson.M{"title": "Draft article", "listId": drafts, "swimlaneId": pages})
	cdn := randomID()
	if _, err := w.db.Collection("attachments").InsertOne(ctx, bson.M{
		"_id": cdn, "name": "photo.png", "type": "image/png", "isImage": true,
		"meta": bson.M{"boardId": board.ID, "cardId": page},
	}); err != nil {
		t.Fatal(err)
	}
	guest := w.client(t)
	url := w.srv.URL + "/b/" + board.ID + "/website-fixture"
	for _, address := range []string{url, url + "?view=website&page=" + draft} {
		res, html := get(t, guest, address)
		if res.StatusCode != http.StatusOK || !strings.Contains(html, "website-summary") ||
			!strings.Contains(html, "Latest post") || !strings.Contains(html, "Child page") ||
			strings.Contains(html, "Draft article") || strings.Contains(html, "js-list") {
			t.Fatalf("website landing route %s: %d %s", address, res.StatusCode, html)
		}
	}
	res, html := get(t, guest, url+"?page="+page)
	if res.StatusCode != http.StatusOK || !strings.Contains(html, "<strong>Published body</strong>") ||
		!strings.Contains(html, attachmentURL(cdn)) {
		t.Fatalf("website article: %d %s", res.StatusCode, html)
	}
	res, html = get(t, guest, url+"?view=swimlanes")
	if res.StatusCode != http.StatusOK || !strings.Contains(html, "js-open-website-view") {
		t.Fatalf("website in board menu: %d", res.StatusCode)
	}
	update("boards", board.ID, bson.M{"permission": "private"})
	res, html = get(t, guest, url+"?view=website&page="+page)
	if res.StatusCode != http.StatusNotFound || strings.Contains(html, "Published body") {
		t.Fatalf("private website access: %d", res.StatusCode)
	}
}
