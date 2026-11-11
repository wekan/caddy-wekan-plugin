package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestSelectionRoots(t *testing.T) {
	got := selectionRoots([]described{
		{Kind: "item", ID: "i1", Ancestors: []string{"checklist:c1", "card:k1", "list:l1", "swimlane:s1"}},
		{Kind: "checklist", ID: "c1", Ancestors: []string{"card:k1", "list:l1", "swimlane:s1"}},
		{Kind: "item", ID: "i2", Ancestors: []string{"checklist:c2", "card:k2"}},
		{Kind: "card", ID: "k3", Sort: 2}, {Kind: "card", ID: "k4", Sort: 1}, {Kind: "card", ID: "k4", Sort: 1},
	})
	var keys []string
	for _, d := range got {
		keys = append(keys, d.key())
	}
	if strings.Join(keys, " ") != "card:k4 card:k3 checklist:c1 item:i2" {
		t.Errorf("roots: %v", keys)
	}
}

// moveBoardObjects: mixed selections, across boards, and every refusal
// before any write (negative).
func TestMoveBoardObjects(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	eve := w.seedUser(t, "eve", "pw")
	at := login(t, w, "ann")
	a := newRESTBoard(t, w, at, "Source")
	b := newRESTBoard(t, w, at, "Destination")
	private := newRESTBoard(t, w, login(t, w, "eve"), "Private")
	c := w.client(t)
	body(t, w.signIn(t, c, "ann", "pw"))
	call := func(args string) map[string]any {
		req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/"+url.PathEscape("moveBoardObjects"), strings.NewReader(args))
		req.Header.Set("Content-Type", "application/json")
		res, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		return out
	}
	checklist := func(cardID, boardID, title string) (string, string) {
		cl, it := "cl"+randomID()[:10], "it"+randomID()[:10]
		_, _ = w.db.Collection("checklists").InsertOne(ctx, bson.M{"_id": cl, "cardId": cardID, "boardId": boardID, "title": title, "sort": 0.0})
		_, _ = w.db.Collection("checklistItems").InsertOne(ctx, bson.M{"_id": it, "cardId": cardID, "boardId": boardID, "checklistId": cl, "title": title + " item", "sort": 0.0})
		return cl, it
	}
	doc := func(coll, id string) bson.M {
		var d bson.M
		_ = w.db.Collection(coll).FindOne(ctx, bson.M{"_id": id}).Decode(&d)
		return d
	}
	cards := func(boardID string) int64 {
		n, _ := w.db.Collection("cards").CountDocuments(ctx, bson.M{"boardId": boardID})
		return n
	}
	to := func(rb restBoard, extra string) string {
		return `{"boardId":"` + rb.ID + `","swimlaneId":"` + rb.Swimlane + `","listId":"` + rb.List + `"` + extra + `}`
	}
	sel := func(pairs ...string) string {
		var p []string
		for i := 0; i < len(pairs); i += 2 {
			p = append(p, `{"kind":"`+pairs[i]+`","id":"`+pairs[i+1]+`"}`)
		}
		return "[" + strings.Join(p, ",") + "]"
	}

	// Refusals, before anything is written.
	if out := call(`["` + a.ID + `",` + sel("card", a.Card) + `,` + to(private, "") + `]`); out["error"] != "not-authorized" {
		t.Errorf("negative: a board the caller cannot change: %v", out)
	}
	if out := call(`["` + a.ID + `",` + sel("card", a.Card, "card", private.Card) + `,` + to(a, "") + `]`); out["error"] != "invalid-selection" {
		t.Errorf("negative: another board's card selected: %v", out)
	}
	if out := call(`["` + a.ID + `",` + sel("list", a.List) + `,` + to(a, "") + `]`); out["error"] != "invalid-destination" {
		t.Errorf("negative: into itself: %v", out)
	}
	if doc("cards", a.Card)["listId"] != a.List || doc("cards", private.Card)["boardId"] != private.ID {
		t.Fatal("negative: a refused move wrote something")
	}

	// A whole checklist, one of its items and a loose item: one new card.
	cl1, it1 := checklist(a.Card, a.ID, "Whole")
	second, _ := w.s.AddCard(ctx, &Board{ID: a.ID}, &User{ID: "x"}, a.List, a.Swimlane, "Beta")
	_, loose := checklist(second.ID, a.ID, "Loose")
	before := cards(a.ID)
	out := call(`["` + a.ID + `",` + sel("checklist", cl1, "item", it1, "item", loose) + `,` + to(a, "") + `]`)
	res, _ := out["result"].(map[string]any)
	if res == nil || res["moved"] != 2.0 || cards(a.ID) != before+1 {
		t.Fatalf("checklist and loose item: %v", out)
	}
	made := res["cardId"].(string)
	if doc("checklists", cl1)["cardId"] != made || doc("checklistItems", it1)["cardId"] != made || doc("checklistItems", loose)["cardId"] != made {
		t.Error("everything is on the one new card")
	}

	// A card to another board, before its card.
	anchor := b.Card
	out = call(`["` + a.ID + `",` + sel("card", second.ID) + `,` + to(b, `,"cardId":"`+anchor+`","position":"before"`) + `]`)
	if out["error"] != nil || doc("cards", second.ID)["boardId"] != b.ID {
		t.Fatalf("a card to another board: %v", out)
	}
	if doc("cards", second.ID)["sort"].(float64) >= doc("cards", anchor)["sort"].(float64) {
		t.Error("before the anchor")
	}

	// Dragging what the board does not let be dragged.
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": a.ID}, bson.M{"$set": bson.M{"allowsCardDragging": false}})
	if out := call(`["` + a.ID + `",` + sel("card", a.Card) + `,` + to(a, "") + `,{"drag":true}]`); out["error"] != "drag-disabled" {
		t.Errorf("negative: card dragging off: %v", out)
	}
	if out := call(`["` + a.ID + `",` + sel("card", a.Card) + `,` + to(a, "") + `,{"drag":false}]`); out["error"] != nil {
		t.Errorf("the menu still moves it: %v", out)
	}

	// An assigned-only member moves only their own cards.
	setMember(t, w, a.ID, Member{UserID: eve.ID, IsActive: true, IsNormalAssignedOnly: true})
	ec := w.client(t)
	body(t, w.signIn(t, ec, "eve", "pw"))
	req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/moveBoardObjects", strings.NewReader(`["`+a.ID+`",`+sel("card", a.Card)+`,`+to(a, "")+`]`))
	req.Header.Set("Content-Type", "application/json")
	r2, _ := ec.Do(req)
	var o2 map[string]any
	_ = json.NewDecoder(r2.Body).Decode(&o2)
	r2.Body.Close()
	if o2["error"] != "not-found" {
		t.Errorf("negative: an assigned-only member: %v", o2)
	}
	if n, _ := w.db.Collection("eventlog").CountDocuments(ctx, bson.M{"bleed": "AssignedBleed", "source": "move-board-objects"}); n == 0 {
		t.Error("AssignedBleed is recorded")
	}
}
