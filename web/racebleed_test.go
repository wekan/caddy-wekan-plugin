package web

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// RaceBleed (GHSA-3vjg-3p4q-8fwh), as WeKan's tests/claimedCardRemoval.test.cjs:
// a card delete racing a move of that card to another board must never leave
// the card alive without its checklists, items and comments.

func raceBleedChildren(t *testing.T, w *testWeKan, b *Board, cardID string) {
	t.Helper()
	ctx := context.Background()
	for coll, extra := range map[string]bson.M{"checklists": {"title": "Keep me"}, "checklistItems": {"title": "Marked"}, "card_comments": {"text": "hi"}} {
		extra["_id"], extra["cardId"], extra["boardId"] = randomID(), cardID, b.ID
		if _, err := w.db.Collection(coll).InsertOne(ctx, extra); err != nil {
			t.Fatal(err)
		}
	}
}

func raceBleedChildCount(w *testWeKan, cardID string) int64 {
	var n int64
	for _, coll := range []string{"checklists", "checklistItems", "card_comments"} {
		c, _ := w.db.Collection(coll).CountDocuments(context.Background(), bson.M{"cardId": cardID})
		n += c
	}
	return n
}

func TestRaceBleedDeleteAfterMoveKeepsChildren(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	u := w.seedUser(t, "ada", "pw")
	b, _ := w.s.CreateBoard(ctx, u, "Source")
	secret, _ := w.s.CreateBoard(ctx, u, "Secret")
	l, _ := w.s.AddList(ctx, b, "One")
	ls, _ := w.s.AddList(ctx, secret, "Hidden")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)

	// The reporter's race, made deterministic: the delete has looked the card
	// up, then the admin's move lands, then the delete goes on.
	c, _ := w.s.AddCard(ctx, b, u, l.ID, sw.ID, "Raced")
	raceBleedChildren(t, w, b, c.ID)
	looked := *c
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": c.ID}, bson.M{"$set": bson.M{"boardId": secret.ID, "listId": ls.ID}})
	if err := w.s.DeleteCard(ctx, b, u, &looked); err != errCardGone {
		t.Fatalf("the delete that lost the race: %v, want errCardGone", err)
	}
	if n, _ := w.db.Collection("cards").CountDocuments(ctx, bson.M{"_id": c.ID, "boardId": secret.ID}); n != 1 {
		t.Fatal("the moved card is gone")
	}
	if n := raceBleedChildCount(w, c.ID); n != 3 {
		t.Fatalf("RaceBleed: the moved card lost its children (%d of 3 left)", n)
	}
	if n, _ := w.db.Collection("activities").CountDocuments(ctx, bson.M{"cardId": c.ID, "activityType": "deleteCard"}); n != 0 {
		t.Fatal("negative: a deleteCard activity for a card that was not deleted")
	}
	var row bson.M
	if err := w.db.Collection("eventlog").FindOne(ctx, bson.M{"bleed": "RaceBleed"}).Decode(&row); err != nil {
		t.Fatal("the lost race is not in Problems as RaceBleed")
	}
	var acct struct {
		LoginDisabled bool `bson:"loginDisabled"`
	}
	if err := w.db.Collection("users").FindOne(ctx, bson.M{"_id": u.ID}).Decode(&acct); err != nil || acct.LoginDisabled {
		t.Fatal("negative: a stale delete must never disable the account")
	}

	// The delete that wins removes the card first, then its children.
	d, _ := w.s.AddCard(ctx, b, u, l.ID, sw.ID, "Deleted")
	raceBleedChildren(t, w, b, d.ID)
	if err := w.s.DeleteCard(ctx, b, u, d); err != nil {
		t.Fatal(err)
	}
	if n, _ := w.db.Collection("cards").CountDocuments(ctx, bson.M{"_id": d.ID}); n != 0 || raceBleedChildCount(w, d.ID) != 0 {
		t.Fatal("a deleted card, or its children, remain")
	}
}

func TestRaceBleedRESTDeleteRacingMove(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	admin := w.seedUser(t, "ada", "pw")
	member := w.seedUser(t, "ben", "pw")
	api := w.srv.URL
	token := func(name string) string {
		var l struct{ Token string }
		call(t, "POST", api+"/users/login", "", `{"username":"`+name+`","password":"pw"}`, &l)
		return l.Token
	}
	ta, tb := token("ada"), token("ben")
	b, _ := w.s.CreateBoard(ctx, admin, "Source")
	secret, _ := w.s.CreateBoard(ctx, admin, "Secret")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$push": bson.M{"members": bson.M{"userId": member.ID, "isActive": true, "isAdmin": false}}})
	l, _ := w.s.AddList(ctx, b, "One")
	ls, _ := w.s.AddList(ctx, secret, "Hidden")
	var sw, ss Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": secret.ID}).Decode(&ss)

	for round := 0; round < 12; round++ {
		c, _ := w.s.AddCard(ctx, b, admin, l.ID, sw.ID, "Race")
		raceBleedChildren(t, w, b, c.ID)
		url := api + "/api/boards/" + b.ID + "/lists/" + l.ID + "/cards/" + c.ID
		var move, del int
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			move = call(t, "PUT", url, ta, `{"newBoardId":"`+secret.ID+`","newSwimlaneId":"`+ss.ID+`","newListId":"`+ls.ID+`"}`, nil)
		}()
		go func() { defer wg.Done(); del = call(t, "DELETE", url, tb, "", nil) }()
		wg.Wait()
		if del != 200 && del != 404 || move >= 500 {
			t.Fatalf("round %d: move %d, delete %d", round, move, del)
		}
		if n, _ := w.db.Collection("cards").CountDocuments(ctx, bson.M{"_id": c.ID}); n == 1 {
			if del == 200 {
				t.Fatalf("round %d: the delete answered 200 for a card that stayed", round)
			}
			if got := raceBleedChildCount(w, c.ID); got != 3 {
				t.Fatalf("round %d: RaceBleed - the surviving card has %d of 3 children", round, got)
			}
		} else if got := raceBleedChildCount(w, c.ID); got != 0 {
			t.Fatalf("round %d: the deleted card left %d children", round, got)
		}
	}
}

func TestRaceBleedSourceOrder(t *testing.T) {
	// Negative, by the shape: the card's own removal comes before any of its
	// children's, and its result decides.
	src, err := os.ReadFile("api_cards.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	at := strings.Index(s, "func (s *Server) DeleteCard(")
	body := s[at : at+strings.Index(s[at:], "\n}\n")]
	card := strings.Index(body, `Collection("cards").DeleteOne(`)
	kids := strings.Index(body, `DeleteMany(ctx, bson.M{"cardId": c.ID})`)
	if card < 0 || kids < 0 || card > kids {
		t.Fatal("DeleteCard must remove the card before its children")
	}
	if !strings.Contains(body[card:kids], "res.DeletedCount == 0") {
		t.Fatal("and only go on when that removal matched")
	}
}
