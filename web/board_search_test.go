package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestBoardSearchMatches(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	assignee := w.seedUser(t, "amy", "pw")
	ownerClient, assigneeClient := w.client(t), w.client(t)
	w.signIn(t, ownerClient, "ann", "pw")
	w.signIn(t, assigneeClient, "amy", "pw")
	board := newRESTBoard(t, w, login(t, w, "ann"), "Search fixture")
	update := func(collection, id string, modifier bson.M) {
		t.Helper()
		if _, err := w.db.Collection(collection).UpdateOne(ctx, bson.M{"_id": id}, modifier); err != nil {
			t.Fatal(err)
		}
	}
	update("boards", board.ID, bson.M{"$push": bson.M{"members": bson.M{
		"userId": assignee.ID, "isActive": true, "isReadAssignedOnly": true,
	}}})
	update("cards", board.Card, bson.M{"$set": bson.M{
		"description": "Search needle", "assignees": bson.A{assignee.ID},
	}})
	insert := func(collection string, doc bson.M) {
		t.Helper()
		if _, err := w.db.Collection(collection).InsertOne(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}
	insert("cards", bson.M{"_id": "searchOther", "boardId": board.ID, "title": "Comment match", "archived": false})
	insert("card_comments", bson.M{"_id": "searchComment", "boardId": board.ID, "cardId": "searchOther", "text": "Needle"})
	insert("cards", bson.M{"_id": "searchArchive", "boardId": board.ID, "title": "Needle archive", "archived": true})
	insert("cards", bson.M{"_id": "searchDeleted", "boardId": board.ID, "title": "Needle deleted", "archived": false, "deletedAt": time.Now()})
	other := newRESTBoard(t, w, login(t, w, "ann"), "Other search fixture")
	update("cards", other.Card, bson.M{"$set": bson.M{"title": "Needle on another board"}})
	search := func(client *http.Client, term string, status int, want ...string) {
		t.Helper()
		res, output := get(t, client, w.srv.URL+"/b/"+board.ID+"/search/matches?q="+url.QueryEscape(term))
		if res.StatusCode != status {
			t.Fatalf("search status = %d, want %d: %s", res.StatusCode, status, output)
		}
		if status != http.StatusOK {
			return
		}
		var results []boardSearchMatch
		if err := json.Unmarshal([]byte(output), &results); err != nil || results == nil {
			t.Fatalf("search should return an array: %s (%v)", output, err)
		}
		got := make([]string, 0, len(results))
		for _, result := range results {
			got = append(got, result.ID)
			if result.Title == "" || result.URL != "/b/"+board.ID+"/search-fixture/"+result.ID {
				t.Errorf("invalid result: %+v", result)
			}
		}
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("search = %v, want %v", got, want)
		}
	}
	search(ownerClient, "NEEDLE", http.StatusOK, board.Card, "searchOther")
	search(ownerClient, ".*", http.StatusOK)
	search(assigneeClient, "needle", http.StatusOK, board.Card)
	search(w.client(t), "needle", http.StatusNotFound)
	update("boards", board.ID, bson.M{"$set": bson.M{"permission": "public"}})
	search(w.client(t), "needle", http.StatusOK, board.Card, "searchOther")
}
