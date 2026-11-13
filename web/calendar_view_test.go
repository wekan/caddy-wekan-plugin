package web

import (
	"context"
	"encoding/json"
	"html"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// WeKan's calendar events of a card: its span start to end, and its received,
// due and end dates each an hour long, in its colour; none for an undated card.
func TestCalendarEventsOf(t *testing.T) {
	start := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	b := &Board{ID: "b1", Slug: "s"}
	lanes := []lane{{Lists: []laneList{{Cards: []Card{
		{ID: "c2", Title: "Two", Color: "green", StartAt: &start, EndAt: &end, DueAt: &start},
		{ID: "c1", Title: "One"},
	}}}}}
	got := calendarEventsOf(b, lanes, "en")
	if len(got) != 3 {
		t.Fatalf("span, due and end: %+v", got)
	}
	if got[0].Title != "Two" || !got[0].AllDay || got[0].Start != "2026-11-02T09:00:00.000Z" || got[0].URL != "/b/b1/s/c2" {
		t.Fatalf("the span, a day long: %+v", got[0])
	}
	if !strings.Contains(got[1].ClassName, "calendar-event-due") || !strings.Contains(got[1].ClassName, "calendar-event-green") || got[1].Title != "Two Due" {
		t.Fatalf("the due date, in the card's colour: %+v", got[1])
	}
}

// The Calendar view's page: the calendar with the cards the board shows, and
// its entry in the Board View menu.
func TestCalendarViewPage(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Cal")
	due := time.Now().UTC().Add(48 * time.Hour)
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": rb.Card}, bson.M{"$set": bson.M{"dueAt": due}})
	ac := w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	_, page := get(t, ac, w.srv.URL+"/b/"+rb.ID+"/cal?view=cal")
	if !strings.Contains(page, `id="calendar-view"`) || strings.Contains(page, "js-swimlane\"") {
		t.Fatal("the calendar, not the swimlanes")
	}
	m := regexp.MustCompile(`data-events="([^"]*)"`).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("the events")
	}
	var events []calendarEvent
	if err := json.Unmarshal([]byte(html.UnescapeString(m[1])), &events); err != nil || len(events) != 1 || events[0].ID != rb.Card {
		t.Fatalf("the card's due date: %v %+v", err, events)
	}
	_, board := get(t, ac, w.srv.URL+"/b/"+rb.ID+"/cal")
	if !strings.Contains(board, "js-open-cal-view") || strings.Contains(board, `id="calendar-view"`) {
		t.Fatal("the board offers the Calendar, and is the board")
	}
	// The Filter applies: a card the board does not show has no event.
	_, page = get(t, ac, w.srv.URL+"/b/"+rb.ID+"/cal?view=cal&advanced="+url.QueryEscape("@dueAt < '2000-01-01'"))
	if m := regexp.MustCompile(`data-events="([^"]*)"`).FindStringSubmatch(page); m == nil || html.UnescapeString(m[1]) != "null" && html.UnescapeString(m[1]) != "[]" {
		t.Fatalf("filtered out: %v", m)
	}
}
