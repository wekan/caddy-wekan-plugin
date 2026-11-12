package web

import (
	"context"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// createListAfter, WeKan's inline Add List composer's method (#6465).
func TestCreateListAfter(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	w.seedUser(t, "bob", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Lanes")
	_, _ = w.db.Collection("lists").UpdateOne(ctx, bson.M{"_id": rb.List}, bson.M{"$set": bson.M{"sort": 0.0}})
	_, _ = w.db.Collection("lists").InsertOne(ctx, bson.M{"_id": "listTwo12345", "boardId": rb.ID, "title": "Two", "sort": 4.0, "archived": false})
	lane2 := "lane2abcdefgh"
	_, _ = w.db.Collection("swimlanes").InsertOne(ctx, bson.M{"_id": lane2, "boardId": rb.ID, "title": "Second", "sort": 9.0, "archived": false})
	ac := w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	create := func(params string) (string, map[string]any) {
		_, out := methodCall(t, w, ac, "createListAfter", `[{"boardId":"`+rb.ID+`",`+params+`}]`)
		id, _ := out["result"].(string)
		return id, out
	}
	list := func(id string) List {
		var l List
		_ = w.db.Collection("lists").FindOne(ctx, bson.M{"_id": id}).Decode(&l)
		return l
	}
	between, _ := create(`"title":"Between","afterListId":"` + rb.List + `"`)
	if l := list(between); l.Sort != 2 || l.SwimlaneID != rb.Swimlane || l.Title != "Between" {
		t.Errorf("between the list and the next, in the default swimlane: %+v", l)
	}
	last, _ := create(`"title":"Last","afterListId":"listTwo12345"`)
	if l := list(last); l.Sort != 5 {
		t.Errorf("after the last list: %+v", l)
	}
	end, _ := create(`"title":" End "`)
	if l := list(end); l.Sort != 6 || l.Title != "End" {
		t.Errorf("at the end, trimmed: %+v", l)
	}
	other, _ := create(`"title":"Elsewhere","swimlaneId":"` + lane2 + `","afterListId":"` + rb.List + `"`)
	if l := list(other); l.SwimlaneID != lane2 {
		t.Errorf("a list shown in every swimlane is in the one the composer opened in: %+v", l)
	}
	if n, _ := w.db.Collection("activities").CountDocuments(ctx, bson.M{"activityType": "createList", "listId": between}); n != 1 {
		t.Error("a createList activity")
	}
	for name, c := range map[string]struct{ params, err string }{
		"an unknown list":    {`"title":"x","afterListId":"noSuchList123"`, "list-not-found"},
		"an empty title":     {`"title":"  "`, "Match.Error"},
		"an unknown key":     {`"title":"x","sort":1`, "Match.Error"},
		"a title not a text": {`"title":1`, "Match.Error"},
	} {
		if _, out := create(c.params); out["error"] != c.err {
			t.Errorf("negative: %s: %v", name, out)
		}
	}
	bc := w.client(t)
	body(t, w.signIn(t, bc, "bob", "pw"))
	if _, out := methodCall(t, w, bc, "createListAfter", `[{"boardId":"`+rb.ID+`","title":"x"}]`); out["error"] != "not-authorized" {
		t.Errorf("negative: not a writer of the board: %v", out)
	}
	// The board page offers the composer to its writers, not to a reader.
	_, html := get(t, ac, w.srv.URL+"/b/"+rb.ID+"/x")
	for _, want := range []string{"js-add-list-here", "js-add-list-inline-template", "js-multiline-title-mode", "Add many lines as:"} {
		if !strings.Contains(html, want) {
			t.Errorf("the board page has %s", want)
		}
	}
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": rb.ID}, bson.M{"$set": bson.M{"permission": "public"}})
	if _, html := get(t, bc, w.srv.URL+"/b/"+rb.ID+"/x"); strings.Contains(html, "js-add-list-here") {
		t.Error("negative: a reader of a public board is not offered it")
	}
}
