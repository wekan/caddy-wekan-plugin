package web

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestRESTAPIMore(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "rita", "pw")
	w.seedUser(t, "sam", "pw")
	api := w.srv.URL
	var login, other struct{ Token string }
	call(t, "POST", api+"/users/login", "", `{"username":"rita","password":"pw"}`, &login)
	call(t, "POST", api+"/users/login", "", `{"username":"sam","password":"pw"}`, &other)
	tok := login.Token
	var b struct {
		ID  string `json:"_id"`
		Swl string `json:"defaultSwimlaneId"`
	}
	call(t, "POST", api+"/api/boards", tok, `{"title":"More"}`, &b)
	board := api + "/api/boards/" + b.ID
	var id struct {
		ID string `json:"_id"`
	}
	find := func(coll, docID string) bson.M {
		var d bson.M
		if w.db.Collection(coll).FindOne(context.Background(), bson.M{"_id": docID}).Decode(&d) != nil {
			return nil
		}
		return d
	}

	// Swimlanes and lists, one by one.
	for _, coll := range []string{"swimlanes", "lists"} {
		if code := call(t, "POST", board+"/"+coll, tok, `{"title":"New `+coll+`","swimlaneId":"`+b.Swl+`"}`, &id); code != 200 || find(coll, id.ID)["title"] != "New "+coll {
			t.Fatalf("POST %s: %d", coll, code)
		}
		if code := call(t, "GET", board+"/"+coll+"/"+id.ID, tok, "", nil); code != 200 {
			t.Fatalf("GET one of %s: %d", coll, code)
		}
		if code := call(t, "PUT", board+"/"+coll+"/"+id.ID, tok, `{"title":"Renamed"}`, nil); code != 200 || find(coll, id.ID)["title"] != "Renamed" {
			t.Fatalf("PUT %s: %d", coll, code)
		}
		if code := call(t, "PUT", board+"/"+coll+"/"+id.ID, other.Token, `{"title":"x"}`, nil); code != 403 {
			t.Fatalf("negative: a non-member renames %s: %d", coll, code)
		}
		if code := call(t, "PUT", board+"/"+coll+"/"+id.ID, tok, `{"title":"  "}`, nil); code != 400 {
			t.Fatalf("negative: an empty title for %s: %d", coll, code)
		}
		if code := call(t, "DELETE", board+"/"+coll+"/"+id.ID, tok, "", nil); code != 200 || find(coll, id.ID) != nil {
			t.Fatalf("DELETE %s: %d", coll, code)
		}
		if code := call(t, "GET", board+"/"+coll+"/"+id.ID, tok, "", nil); code != 404 {
			t.Fatalf("the deleted %s is gone: %d", coll, code)
		}
	}

	// Custom fields.
	call(t, "POST", board+"/custom-fields", tok, `{"name":"Priority","type":"text","settings":{},"showOnCard":true}`, &id)
	if f := find("customFields", id.ID); f == nil || f["name"] != "Priority" {
		t.Fatal("POST custom-fields")
	}
	if code := call(t, "POST", board+"/custom-fields", tok, `{"name":"x","type":"script"}`, nil); code != 400 {
		t.Fatalf("negative: an unknown custom field type: %d", code)
	}
	if code := call(t, "GET", board+"/custom-fields/"+id.ID, tok, "", nil); code != 200 {
		t.Fatalf("GET custom field: %d", code)
	}
	if code := call(t, "DELETE", board+"/custom-fields/"+id.ID, other.Token, "", nil); code != 403 {
		t.Fatalf("negative: a non-member deletes a custom field: %d", code)
	}
	var fieldCard struct {
		ID string `json:"_id"`
	}
	var fl struct {
		ID string `json:"_id"`
	}
	call(t, "POST", board+"/lists", tok, `{"title":"F"}`, &fl)
	call(t, "POST", board+"/lists/"+fl.ID+"/cards", tok, `{"title":"F","swimlaneId":"`+b.Swl+`"}`, &fieldCard)
	_, _ = w.db.Collection("cards").UpdateOne(context.Background(), bson.M{"_id": fieldCard.ID},
		bson.M{"$set": bson.M{"customFields": []bson.M{{"_id": id.ID, "value": "high"}, {"_id": "other", "value": 1}}}})
	if code := call(t, "DELETE", board+"/custom-fields/"+id.ID, tok, "", nil); code != 200 || find("customFields", id.ID) != nil {
		t.Fatalf("DELETE custom field: %d", code)
	}
	if cf, _ := find("cards", fieldCard.ID)["customFields"].(bson.A); len(cf) != 1 {
		t.Fatalf("the deleted field's values are gone from its cards, the others kept: %v", cf)
	}

	// Checklists, their items, and comments on a card.
	var lst struct {
		ID string `json:"_id"`
	}
	call(t, "POST", board+"/lists", tok, `{"title":"L"}`, &lst)
	var card struct {
		ID string `json:"_id"`
	}
	call(t, "POST", board+"/lists/"+lst.ID+"/cards", tok, `{"title":"C","swimlaneId":"`+b.Swl+`"}`, &card)
	cl := board + "/cards/" + card.ID + "/checklists"
	call(t, "POST", cl, tok, `{"title":"My Checklist","items":["one","two"]}`, &id)
	if n, _ := w.db.Collection("checklistItems").CountDocuments(context.Background(), bson.M{"checklistId": id.ID}); n != 2 {
		t.Fatalf("the checklist's items: %d", n)
	}
	if code := call(t, "POST", cl+"/"+id.ID+"/items", tok, `{"title":"three"}`, nil); code != 200 {
		t.Fatalf("add an item: %d", code)
	}
	var lists []map[string]any
	call(t, "GET", cl, tok, "", &lists)
	if len(lists) != 1 || len(lists[0]["items"].([]any)) != 3 {
		t.Fatalf("GET checklists with items: %v", lists)
	}
	if code := call(t, "POST", board+"/cards/nope/checklists", tok, `{"title":"x"}`, nil); code != 404 {
		t.Fatalf("negative: a card not on the board: %d", code)
	}
	if code := call(t, "DELETE", cl+"/"+id.ID, tok, "", nil); code != 200 || find("checklists", id.ID) != nil {
		t.Fatalf("DELETE checklist: %d", code)
	}
	if n, _ := w.db.Collection("checklistItems").CountDocuments(context.Background(), bson.M{"checklistId": id.ID}); n != 0 {
		t.Fatal("the deleted checklist's items are gone too")
	}

	cm := board + "/cards/" + card.ID + "/comments"
	call(t, "POST", cm, tok, `{"authorId":"someone-else","comment":"Hello from the API"}`, &id)
	if c := find("card_comments", id.ID); c == nil || c["text"] != "Hello from the API" || c["userId"] != u.ID {
		t.Fatalf("a comment, its author the caller: %v", c)
	}
	var comments []map[string]any
	call(t, "GET", cm, tok, "", &comments)
	if len(comments) != 1 || comments[0]["_id"] != id.ID {
		t.Fatalf("GET comments: %v", comments)
	}
	if code := call(t, "POST", cm, other.Token, `{"comment":"x"}`, nil); code != 403 {
		t.Fatalf("negative: a non-member comments: %d", code)
	}

	// Logging out revokes the presented token (#1437), and only it.
	if code := call(t, "POST", api+"/users/logout", tok, "{}", nil); code != 200 {
		t.Fatalf("logout: %d", code)
	}
	if code := call(t, "GET", board, tok, "", nil); code != 401 {
		t.Fatalf("negative: the token works after logging out: %d", code)
	}
	if code := call(t, "GET", api+"/api/user", other.Token, "", nil); code != 200 {
		t.Fatal("another user's token still works")
	}
}
