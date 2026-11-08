package web

import (
	"context"
	"encoding/json"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestRESTLabelsMembersDates(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "una", "pw")
	v := w.seedUser(t, "vic", "pw")
	api := w.srv.URL
	var a, o struct{ Token string }
	call(t, "POST", api+"/users/login", "", `{"username":"una","password":"pw"}`, &a)
	call(t, "POST", api+"/users/login", "", `{"username":"vic","password":"pw"}`, &o)
	var b struct {
		ID  string `json:"_id"`
		Swl string `json:"defaultSwimlaneId"`
	}
	call(t, "POST", api+"/api/boards", a.Token, `{"title":"Labels"}`, &b)
	board := api + "/api/boards/" + b.ID
	var lst, c1, c2 struct {
		ID string `json:"_id"`
	}
	call(t, "POST", board+"/lists", a.Token, `{"title":"L"}`, &lst)
	cards := board + "/lists/" + lst.ID + "/cards"
	call(t, "POST", cards, a.Token, `{"title":"one","swimlaneId":"`+b.Swl+`"}`, &c1)
	call(t, "POST", cards, a.Token, `{"title":"two","swimlaneId":"`+b.Swl+`","members":"`+u.ID+`"}`, &c2)
	card := func(id string) Card {
		var c Card
		_ = w.db.Collection("cards").FindOne(context.Background(), bson.M{"_id": id}).Decode(&c)
		return c
	}
	if c := card(c1.ID); c.Members == nil || len(c.Members) != 0 || c.Assignees == nil {
		t.Fatalf("#2875: no members is [], never null: %+v", c)
	}
	if c := card(c2.ID); len(c.Members) != 1 || c.Members[0] != u.ID {
		t.Fatalf("one member id is wrapped in an array: %v", c.Members)
	}
	var brd Board
	_ = w.db.Collection("boards").FindOne(context.Background(), bson.M{"_id": b.ID}).Decode(&brd)
	if len(brd.Labels) != 6 {
		t.Fatalf("a new board has WeKan's six default labels: %d", len(brd.Labels))
	}

	// Labels: a board admin adds one; bulk add and remove merge.
	label := func(name, color, token string) (string, int) {
		var id string
		code := call(t, "PUT", board+"/labels", token, `{"label":{"name":"`+name+`","color":"`+color+`"}}`, &id)
		return id, code
	}
	la, _ := label("A", "green", a.Token)
	lb, _ := label("B", "red", a.Token)
	if la == "" || lb == "" || la == lb {
		t.Fatalf("label ids: %q %q", la, lb)
	}
	if _, code := label("x", "nocolor", a.Token); code != 400 {
		t.Fatalf("negative: an unknown color: %d", code)
	}
	ids := `["` + c1.ID + `","` + c2.ID + `"]`
	call(t, "POST", board+"/cards/labels", a.Token, `{"cardIds":`+ids+`,"addLabelIds":["`+la+`"]}`, nil)
	call(t, "POST", board+"/cards/labels", a.Token, `{"cardIds":`+ids+`,"addLabelIds":["`+lb+`"],"removeLabelIds":["`+la+`"]}`, nil)
	call(t, "POST", board+"/cards/labels", a.Token, `{"cardIds":["`+c1.ID+`"],"addLabelIds":["`+lb+`"]}`, nil)
	if l := card(c1.ID).LabelIDs; len(l) != 1 || l[0] != lb {
		t.Fatalf("#5819: merged, not replaced, and not doubled: %v", l)
	}
	if code := call(t, "POST", board+"/cards/labels", a.Token, `{"cardIds":`+ids+`,"addLabelIds":["does-not-exist"]}`, nil); code != 400 {
		t.Fatalf("negative: a label not on the board: %d", code)
	}

	// Board members: only a board admin adds, with a known role.
	if code := call(t, "POST", board+"/members/"+v.ID+"/add", a.Token, `{"action":"add","role":"superuser"}`, nil); code != 400 {
		t.Fatalf("negative: an unknown role: %d", code)
	}
	if code := call(t, "POST", board+"/members/"+v.ID+"/add", a.Token, `{"action":"add","role":"normal"}`, nil); code != 200 {
		t.Fatalf("add a member: %d", code)
	}
	if _, code := label("Sneaky", "blue", o.Token); code != 403 {
		t.Fatalf("negative: a non-admin member adds a label: %d", code)
	}
	if code := call(t, "POST", board+"/members/"+u.ID+"/add", o.Token, `{"action":"add","role":"admin"}`, nil); code != 403 {
		t.Fatalf("negative: a non-admin member adds members: %d", code)
	}
	call(t, "POST", board+"/members/"+v.ID+"/add", a.Token, `{"action":"add","role":"admin"}`, nil)
	_ = w.db.Collection("boards").FindOne(context.Background(), bson.M{"_id": b.ID}).Decode(&brd)
	if !brd.AdminOf(v.ID) || len(brd.Members) != 2 {
		t.Fatalf("role=admin makes an admin, without a second entry: %+v", brd.Members)
	}

	// Card members: only board members.
	member := cards + "/" + c1.ID + "/members/"
	if code := call(t, "POST", member+v.ID, a.Token, "", nil); code != 200 || len(card(c1.ID).Members) != 1 {
		t.Fatalf("a board member becomes a card member: %d", code)
	}
	if code := call(t, "POST", member+"not-a-member", a.Token, "", nil); code != 400 {
		t.Fatalf("negative: a non-member card member: %d", code)
	}
	if code := call(t, "DELETE", member+v.ID, a.Token, "", nil); code != 200 || len(card(c1.ID).Members) != 0 {
		t.Fatalf("remove a card member: %d", code)
	}

	// Dates and members through PUT.
	put := func(body string) int { return call(t, "PUT", cards+"/"+c1.ID, a.Token, body, nil) }
	put(`{"dueAt":"2026-06-07T12:00:00.000Z"}`)
	var raw bson.M
	_ = w.db.Collection("cards").FindOne(context.Background(), bson.M{"_id": c1.ID}).Decode(&raw)
	if raw["dueAt"] == nil {
		t.Fatal("#5846: a due date is set")
	}
	put(`{"dueAt":""}`)
	raw = nil
	_ = w.db.Collection("cards").FindOne(context.Background(), bson.M{"_id": c1.ID}).Decode(&raw)
	if raw["dueAt"] != nil {
		t.Fatal("#5846: an empty due date clears it")
	}
	if code := put(`{"dueAt":"tomorrow"}`); code != 400 {
		t.Fatalf("negative: not a date: %d", code)
	}
	for _, empty := range []string{`""`, `null`} {
		put(`{"members":["` + u.ID + `"],"assignees":["` + u.ID + `"]}`)
		put(`{"members":` + empty + `,"assignees":` + empty + `}`)
		if c := card(c1.ID); c.Members == nil || len(c.Members) != 0 || c.Assignees == nil || len(c.Assignees) != 0 {
			t.Fatalf("#3697: clearing with %s stores [], never null: %+v", empty, c)
		}
	}
	if code := put(`{"members":[{"$ne":1}]}`); code != 400 {
		t.Fatalf("negative: an object where an id goes: %d", code)
	}

	// Bulk create.
	var bulk []struct {
		ID string `json:"_id"`
	}
	call(t, "POST", cards+"/bulk", a.Token, `{"swimlaneId":"`+b.Swl+`","cards":[{"title":"Bulk","members":null},{"title":"Bulk 2"}]}`, &bulk)
	if len(bulk) != 2 || card(bulk[0].ID).Members == nil {
		t.Fatalf("bulk create, one {_id} per card: %v", bulk)
	}
	many, _ := json.Marshal(map[string]any{"cards": make([]map[string]string, 501)})
	if code := call(t, "POST", cards+"/bulk", a.Token, string(many), nil); code != 400 {
		t.Fatalf("negative: more than 500 cards: %d", code)
	}
}
