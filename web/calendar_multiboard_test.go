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

// WeKan's Multi Board Calendar (#2469): every board the viewer is a member of,
// each event prefixed with its board's title, read-only - and not another
// user's board, an archived one or a helper board.
func TestMultiboardCalendar(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	u := w.seedUser(t, "ann", "pw")
	other := w.seedUser(t, "bob", "pw")
	due := time.Now().UTC().Truncate(time.Second)
	board := func(owner *User, title string) *Board {
		b, err := w.s.CreateBoard(ctx, owner, title)
		if err != nil {
			t.Fatal(err)
		}
		l, _ := w.s.AddList(ctx, b, "L")
		var sw Swimlane
		_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
		c, _ := w.s.AddCard(ctx, b, owner, l.ID, sw.ID, title+" card")
		_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": c.ID}, bson.M{"$set": bson.M{"dueAt": due}})
		return b
	}
	here := board(u, "Here")
	board(u, "There")
	board(other, "Bobs")
	gone := board(u, "Gone")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": gone.ID}, bson.M{"$set": bson.M{"archived": true}})
	board(u, "^Subtasks^")
	cl := w.client(t)
	body(t, w.signIn(t, cl, "ann", "pw"))
	get := func(q string) string {
		res, err := cl.Get(w.srv.URL + "/b/" + here.ID + "/x" + q)
		if err != nil {
			t.Fatal(err)
		}
		return body(t, res)
	}
	h := get("?view=multiboard-cal")
	if !strings.Contains(h, `id="multiboard-calendar-view"`) || !strings.Contains(h, `data-readonly="true"`) {
		t.Fatal("the read-only Multi Board Calendar")
	}
	if !strings.Contains(h, "js-open-multiboard-cal-view") {
		t.Fatal("offered in the Board View menu")
	}
	titlesOf := func(h string) string {
		m := regexp.MustCompile(`data-events="([^"]*)"`).FindStringSubmatch(h)
		var events []calendarEvent
		if m == nil || json.Unmarshal([]byte(html.UnescapeString(m[1])), &events) != nil {
			t.Fatal("no events")
		}
		titles := []string{}
		for _, e := range events {
			titles = append(titles, e.Title)
		}
		return strings.Join(titles, " | ")
	}
	all := titlesOf(h)
	for _, want := range []string{"[Here] Here card", "[There] There card"} {
		if !strings.Contains(all, want) {
			t.Fatalf("%s missing: %s", want, all)
		}
	}
	for _, not := range []string{"Bobs", "Gone", "Subtasks"} {
		if strings.Contains(all, not) {
			t.Fatalf("negative: %s is on the calendar: %s", not, all)
		}
	}
	// The Filter applies to every board.
	if got := titlesOf(get("?view=multiboard-cal&text=there")); strings.Contains(got, "[Here]") || !strings.Contains(got, "[There] There card") {
		t.Fatalf("filtered on every board: %s", got)
	}
	// Negative: the board's own Calendar view is editable, and its own.
	h = get("?view=cal")
	if got := titlesOf(h); !strings.Contains(h, `id="calendar-view"`) || strings.Contains(got, "There") || strings.Contains(h, `data-readonly="true"`) {
		t.Fatalf("the board's Calendar view is the board's alone: %s", got)
	}
}
