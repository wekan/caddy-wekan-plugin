package web

import (
	"context"
	"encoding/json"
	"html"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// WeKan's Calendar Mode (#3194): offered below Calendar in the Board View
// menu, its page carries the due cards the board shows - with their labels and
// their list's place - and nothing undated.
func TestCalendarModePage(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	u := w.seedUser(t, "ann", "pw")
	b, _ := w.s.CreateBoard(ctx, u, "Calendar")
	l1, _ := w.s.AddList(ctx, b, "One")
	l2, _ := w.s.AddList(ctx, b, "Two")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$push": bson.M{"labels": bson.M{"_id": "lg", "name": "Broadcast", "color": "green"}}})
	due := time.Now().UTC().Truncate(time.Second)
	a, _ := w.s.AddCard(ctx, b, u, l1.ID, sw.ID, "Programme")
	c, _ := w.s.AddCard(ctx, b, u, l2.ID, sw.ID, "News")
	undated, _ := w.s.AddCard(ctx, b, u, l2.ID, sw.ID, "Undated")
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": a.ID}, bson.M{"$set": bson.M{"dueAt": due, "labelIds": bson.A{"lg"}}})
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": c.ID}, bson.M{"$set": bson.M{"dueAt": due}})
	cl := w.client(t)
	body(t, w.signIn(t, cl, "ann", "pw"))
	page := func(q string) string {
		res, err := cl.Get(w.srv.URL + "/b/" + b.ID + "/x" + q)
		if err != nil {
			t.Fatal(err)
		}
		return body(t, res)
	}
	cardsOf := func(h string) []calendarModeCard {
		m := regexp.MustCompile(`data-cards="([^"]*)"`).FindStringSubmatch(h)
		if m == nil {
			t.Fatal("no Calendar Mode on the page")
		}
		var out []calendarModeCard
		if err := json.Unmarshal([]byte(html.UnescapeString(m[1])), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	h := page("?view=calendar-mode")
	if !strings.Contains(h, "js-calendar-mode-view") || strings.Contains(h, `id="calendar-view"`) {
		t.Fatal("Calendar Mode, not the Calendar view")
	}
	cal, mode := strings.Index(h, "js-open-cal-view"), strings.Index(h, "js-open-calendar-mode-view")
	if cal < 0 || mode < cal {
		t.Fatal("Calendar Mode is offered below Calendar")
	}
	got := cardsOf(h)
	if len(got) != 2 {
		t.Fatalf("the two due cards: %+v", got)
	}
	for _, x := range got {
		if x.ID == undated.ID {
			t.Fatal("negative: a card with no due date is not on the calendar")
		}
		if x.ID == a.ID && (len(x.Labels) != 1 || x.Labels[0].Name != "Broadcast" || x.Labels[0].Color != "green" || x.ListSort >= got[1].ListSort && got[1].ID == c.ID) {
			t.Fatalf("the card's labels and its list's place: %+v", x)
		}
		if x.URL != "/b/"+b.ID+"/"+b.Slug+"/"+x.ID {
			t.Fatalf("the card opens: %s", x.URL)
		}
	}
	// The Filter applies here as on the board.
	if got := cardsOf(page("?view=calendar-mode&text=news")); len(got) != 1 || got[0].ID != c.ID {
		t.Fatalf("filtered: %+v", got)
	}
	// Negative: the board itself is not Calendar Mode.
	if strings.Contains(page(""), "js-calendar-mode-view") {
		t.Fatal("the board's own page is not Calendar Mode")
	}
}

// The calendar view the member chose in WeKan opens with the board: the
// board's own choice, else the shared one (#4906); an unknown one is not a
// view, and the Board View menu's Swimlanes always reaches the board.
func TestStoredCalendarView(t *testing.T) {
	u := &User{}
	u.Profile.BoardView = "board-view-cal"
	if storedCalendarView(u, "b1") != "cal" {
		t.Fatal("the shared view")
	}
	u.Profile.BoardViews = map[string]string{"b1": "board-view-multiboard-cal", "b2": "board-view-swimlanes", "b3": "not-a-view"}
	if storedCalendarView(u, "b1") != "multiboard-cal" || storedCalendarView(u, "b2") != "" {
		t.Fatal("the board's own choice wins, a non-calendar one included")
	}
	if storedCalendarView(u, "b3") != "cal" || storedCalendarView(nil, "b1") != "" {
		t.Fatal("negative: an unknown view falls back to the shared one; nobody signed in has none")
	}

	w := newTestWeKan(t)
	ctx := context.Background()
	ann := w.seedUser(t, "ann", "pw")
	b, _ := w.s.CreateBoard(ctx, ann, "Stored")
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": ann.ID}, bson.M{"$set": bson.M{"profile.boardView": "board-view-cal"}})
	cl := w.client(t)
	body(t, w.signIn(t, cl, "ann", "pw"))
	get := func(q string) string {
		res, err := cl.Get(w.srv.URL + "/b/" + b.ID + "/x" + q)
		if err != nil {
			t.Fatal(err)
		}
		return body(t, res)
	}
	if !strings.Contains(get(""), `id="calendar-view"`) {
		t.Fatal("the stored Calendar view opens with the board")
	}
	if h := get("?view=swimlanes"); strings.Contains(h, `id="calendar-view"`) || !strings.Contains(h, "js-swimlane") {
		t.Fatal("Swimlanes reaches the board")
	}
}
