package web

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// WeKan's Sort Cards: a display order of each list's cards by ?sort=, ties
// in the manual order, and no card's sort changed; anything else is no sort.
func TestBoardCardSort(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Sorting")
	l, _ := w.s.AddList(ctx, b, "L")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	set := func(id string, m bson.M) {
		if _, err := w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": m}); err != nil {
			t.Fatal(err)
		}
	}
	day := func(n int) time.Time { return time.Date(2030, 1, n, 12, 0, 0, 0, time.UTC) }
	// Manual order: Cherry, Apple, Banana, Damson.
	ids := map[string]string{}
	for i, title := range []string{"Cherry", "Apple", "Banana", "Damson"} {
		c, _ := w.s.AddCard(ctx, b, u, l.ID, sw.ID, title)
		ids[title] = c.ID
		set(c.ID, bson.M{"sort": float64(i), "createdAt": day(10 + i)})
	}
	set(ids["Cherry"], bson.M{"dueAt": day(5)})
	set(ids["Apple"], bson.M{"dueAt": day(3)})
	set(ids["Banana"], bson.M{"vote": bson.M{"positive": bson.A{"x", "y"}, "negative": bson.A{}}})
	set(ids["Damson"], bson.M{"vote": bson.M{"positive": bson.A{"x"}, "negative": bson.A{"y", "z"}}})
	labels := []Label{{ID: "lred", Name: "R", Color: "red"}, {ID: "lblu", Name: "B", Color: "blue"}}
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"labels": labels}})
	set(ids["Damson"], bson.M{"labelIds": bson.A{"lblu"}})
	set(ids["Apple"], bson.M{"labelIds": bson.A{"lblu", "lred"}})

	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	minicards := regexp.MustCompile(`class="minicard-wrapper js-minicard"[^>]*data-card-id="([^"]+)"`)
	titleOf := map[string]string{}
	for title, id := range ids {
		titleOf[id] = title
	}
	order := func(q string) (string, string) {
		_, html := get(t, ann, w.srv.URL+"/b/"+b.ID+"/"+b.Slug+q)
		var got []string
		for _, m := range minicards.FindAllStringSubmatch(html, -1) {
			got = append(got, titleOf[m[1]])
		}
		return strings.Join(got, ","), html
	}
	manual, html := order("")
	if manual != "Cherry,Apple,Banana,Damson" {
		t.Fatalf("manual order: %s", manual)
	}
	if !strings.Contains(html, `js-sort-cards"`) || strings.Contains(html, "js-sort-reset") || !strings.Contains(html, `data-pop="cards-sort"`) {
		t.Error("Sort Cards off, with its popup")
	}
	for q, want := range map[string]string{
		"due":          "Banana,Damson,Apple,Cherry", // no due date first, as WeKan's {dueAt: 1}
		"title":        "Apple,Banana,Cherry,Damson",
		"created-desc": "Damson,Banana,Apple,Cherry",
		"created-asc":  "Cherry,Apple,Banana,Damson",
		"votes":        "Banana,Cherry,Apple,Damson", // 2, 0, 0, -1; ties in manual order
		"label-order":  "Apple,Damson,Cherry,Banana", // red, blue, then no label in manual order
	} {
		got, html := order("?sort=" + q)
		if got != want {
			t.Errorf("sort=%s: %s, want %s", q, got, want)
		}
		if !strings.Contains(html, "js-sort-cards emphasis") || !strings.Contains(html, "js-sort-reset") ||
			!strings.Contains(html, `class="js-sort-card-order js-sort-`+q+` is-active" href="#" data-sort="`+q+`"`) ||
			strings.Count(html, " is-active\" href=\"#\" data-sort=") != 1 {
			t.Errorf("sort=%s: Sort is on, its reset, and the chosen order marked", q)
		}
	}
	// Negative: an order WeKan does not have is no order, and no card's sort changed.
	if got, html := order("?sort=sort"); got != manual || strings.Contains(html, "js-sort-reset") {
		t.Errorf("an unknown sort: %s", got)
	}
	if got, _ := order("?sort=%22%3E%3Cscript%3E"); got != manual {
		t.Errorf("markup is no sort: %s", got)
	}
	for i, title := range []string{"Cherry", "Apple", "Banana", "Damson"} {
		var c Card
		_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": ids[title]}).Decode(&c)
		if c.Sort != float64(i) {
			t.Errorf("%s: sort %v changed", title, c.Sort)
		}
	}
}
