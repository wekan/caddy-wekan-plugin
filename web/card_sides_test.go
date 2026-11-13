package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Each side as WeKan draws it: what the surface drew before its row existed
// stays on until turned off, what is new is off until chosen.
func TestMinicardSides(t *testing.T) {
	c := &Card{Color: "green", RequestedBy: "Req", Description: "d", Locations: []cardLocation{{Name: "Shop"}, {Address: "Street 1"}},
		DateLastActivity: time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)}
	got := sidesOfMinicard(bson.M{}, c, "")
	if got.ColorClass != "minicard-green" || got.RequestedBy != "Req" || !got.DescriptionBadge || got.Locations != nil || got.LastActivity != "" {
		t.Fatalf("defaults: %+v", got)
	}
	got = sidesOfMinicard(bson.M{"allowsCardColorOnMinicard": false, "allowsRequestedByOnMinicard": false, "allowsLocationOnMinicard": true,
		"allowsActivitiesOnMinicard": true, "allowsDescriptionBadgeOnMinicard": false}, c, "")
	if got.ColorClass != "" || got.RequestedBy != "" || got.DescriptionBadge || strings.Join(got.Locations, ",") != "Shop,Street 1" || !strings.HasPrefix(got.LastActivity, "2026-09-01") {
		t.Fatalf("chosen: %+v", got)
	}
	// The badge stands in for the text: none when the text is on the minicard.
	if sidesOfMinicard(bson.M{"allowsDescriptionTextOnMinicard": true}, c, "").DescriptionBadge {
		t.Fatal("no badge beside the text")
	}
	if sidesOfMinicard(bson.M{}, &Card{}, "").ColorClass != "" {
		t.Fatal("nothing for a card with nothing")
	}
}

// The opened card's sides, and a board changed outside Weddy announced to the
// pages that have it open - once, and not again for Weddy's own change.
func TestOpenedCardSidesAndBoardWatch(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Sides")
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": rb.Card}, bson.M{"$set": bson.M{"color": "green", "description": "E2E description"}})
	ac := w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	cardURL := w.srv.URL + "/b/" + rb.ID + "/sides/" + rb.Card
	_, html := get(t, ac, cardURL)
	if !strings.Contains(html, "card-details-green") || !strings.Contains(html, "js-card-collapse-toggle") ||
		strings.Contains(html, "card-details-swimlane-name") || strings.Contains(html, "card-details-description-badge") {
		t.Fatal("the card's defaults: its colour and caret, no swimlane name or badge")
	}
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": rb.ID}, bson.M{"$set": bson.M{"allowsCardCollapse": false,
		"allowsSwimlaneNameOnCard": true, "allowsDescriptionBadgeOnCard": true, "allowsCardColorOnCard": false}})
	_, html = get(t, ac, cardURL)
	if strings.Contains(html, "card-details-green") || strings.Contains(html, "js-card-collapse-toggle") ||
		!strings.Contains(html, "card-details-swimlane-name") || !strings.Contains(html, "card-details-description-badge") {
		t.Fatal("each side as chosen")
	}
	if _, out := methodCall(t, w, ac, "setCardCollapsed", `["no"]`); out["error"] != "Match.Error" {
		t.Fatalf("setCardCollapsed takes a boolean: %v", out)
	}
	if _, out := methodCall(t, w, ac, "setCardCollapsed", `[true]`); out["error"] != nil {
		t.Fatalf("setCardCollapsed: %v", out)
	}
	if _, html = get(t, ac, cardURL); strings.Contains(html, "card-details-collapsed") {
		t.Fatal("a board without the caret has no folded cards")
	}

	w.s.boardWatchOff.Store(true) // this test looks itself
	ch := w.s.hub.subscribe(rb.ID)
	defer w.s.hub.unsubscribe(rb.ID, ch)
	w.s.watchBoardsOnce(ctx)
	got := func() string {
		select {
		case ev := <-ch:
			return ev
		default:
			return ""
		}
	}
	if ev := got(); ev != "" {
		t.Fatalf("the first look announces nothing: %q", ev)
	}
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": rb.ID}, bson.M{"$set": bson.M{"allowsLocationOnMinicard": true}})
	w.s.watchBoardsOnce(ctx)
	if ev := got(); ev != "board-changed" {
		t.Fatalf("an outside change is announced: %q", ev)
	}
	w.s.watchBoardsOnce(ctx)
	if ev := got(); ev != "" {
		t.Fatalf("once: %q", ev)
	}
	// Weddy's own change announced itself: the watch does not again.
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": rb.ID}, bson.M{"$set": bson.M{"allowsLocationOnMinicard": false}})
	w.s.hub.publish(rb.ID, "board-changed")
	_ = got()
	w.s.watchBoardsOnce(ctx)
	if ev := got(); ev != "" {
		t.Fatalf("not twice: %q", ev)
	}
	// A page's own change (which that page skips), then an outside one before
	// the next look: the outside one still reaches that page.
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": rb.ID}, bson.M{"$set": bson.M{"title": "Mine"}})
	w.s.hub.publishFrom(rb.ID, "board-changed", "PageAAAAAAAAAAAAA")
	_ = got()
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": rb.ID}, bson.M{"$set": bson.M{"allowsCardCollapse": true}})
	w.s.watchBoardsOnce(ctx)
	if ev := got(); ev != "board-changed" {
		t.Fatalf("the outside change after a page's own is announced to all: %q", ev)
	}
}

// A value of another type than WeKan stores (a date as text, a location as a
// string) reads as none: the card is drawn, its other sides too.
func TestCardExtrasTolerateOtherTypes(t *testing.T) {
	var c Card
	fillCardExtras(&c, bson.M{"endAt": "not-a-date", "dueAt": time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), "color": 5,
		"requestedBy": "Req", "locations": bson.A{"Shop", bson.M{"name": "Works"}}})
	if c.EndAt != nil || c.DueAt == nil || c.Color != "" || c.RequestedBy != "Req" || len(c.Locations) != 1 || c.Locations[0].Name != "Works" {
		t.Fatalf("%+v", c)
	}
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Odd")
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": rb.Card}, bson.M{"$set": bson.M{"endAt": "not-a-date", "color": "green"}})
	ac := w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	if _, html := get(t, ac, w.srv.URL+"/b/"+rb.ID+"/odd"); !strings.Contains(html, `data-card="`+rb.Card+`"`) || !strings.Contains(html, "minicard-green") {
		t.Fatal("the card is drawn, with its colour")
	}
	if _, html := get(t, ac, w.srv.URL+"/b/"+rb.ID+"/odd/"+rb.Card); !strings.Contains(html, "card-details-green") {
		t.Fatal("and opened")
	}
}
