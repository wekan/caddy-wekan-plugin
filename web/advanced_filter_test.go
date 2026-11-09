package web

import (
	"context"
	"net/url"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// WeKan's Advanced Filter, as its advanced-filter specs drive it: the field
// itself must match, the whole decimal token, the card's own dates, and a
// filter that does not parse is an error.
func TestAdvancedFilter(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Filter")
	list, _ := w.s.AddList(ctx, b, "L")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	wrong, _ := w.s.AddCard(ctx, b, u, list.ID, sw.ID, "wrong")
	right, _ := w.s.AddCard(ctx, b, u, list.ID, sw.ID, "right")
	missing, _ := w.s.AddCard(ctx, b, u, list.ID, sw.ID, "missing")
	for _, f := range []bson.M{{"_id": "points000000001", "name": "Points", "type": "number"}, {"_id": "other0000000001", "name": "Other", "type": "number"},
		{"_id": "cost00000000001", "name": "Cost", "type": "currency"}, {"_id": "endname00000001", "name": "@endAt", "type": "text"},
		{"_id": "stage0000000001", "name": "Stage", "type": "dropdown", "settings": bson.M{"dropdownItems": bson.A{bson.M{"_id": "optdone", "name": "Done"}}}}} {
		f["boardIds"] = bson.A{b.ID}
		_, _ = w.db.Collection("customFields").InsertOne(ctx, f)
	}
	set := func(id string, fields bson.A, extra bson.M) {
		s := bson.M{"customFields": fields}
		for k, v := range extra {
			s[k] = v
		}
		_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": s})
	}
	old, recent := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC), time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	set(wrong.ID, bson.A{bson.M{"_id": "points000000001", "value": 1}, bson.M{"_id": "other0000000001", "value": 2}, bson.M{"_id": "cost00000000001", "value": 2.25},
		bson.M{"_id": "endname00000001", "value": "other"}}, bson.M{"endAt": old})
	set(right.ID, bson.A{bson.M{"_id": "points000000001", "value": 2}, bson.M{"_id": "other0000000001", "value": 1}, bson.M{"_id": "cost00000000001", "value": 2.5},
		bson.M{"_id": "endname00000001", "value": "keep"}, bson.M{"_id": "stage0000000001", "value": "optdone"}}, bson.M{"endAt": recent})
	set(missing.ID, bson.A{bson.M{"_id": "other0000000001", "value": 2}, bson.M{"_id": "cost00000000001", "value": 2.75}}, bson.M{"endAt": nil})
	name := map[string]string{wrong.ID: "wrong", right.ID: "right", missing.ID: "missing"}
	for filter, want := range map[string][]string{
		"Points = 2": {"right"}, "Points > 1": {"right"}, "Points != 2": {"wrong"},
		"Points = 2 and Other = 1": {"right"}, "Points = 2 or Points = 1": {"right", "wrong"},
		"not Points = 2": {"missing", "wrong"}, "!(Points = 2)": {"missing", "wrong"},
		"not(Points = 1 or Points = 2)": {"missing"}, "(Points = 1 or Points = 2) and (Other = 1)": {"right"},
		"not(Points = 1) and (Points = 2)": {"right"},
		"Cost = 2.5":                       {"right"}, "Cost > 2.5": {"missing"}, "Cost >= 2.5": {"missing", "right"}, "Cost <= 2.5": {"right", "wrong"},
		"Cost = 25e-1": {"right"}, "Cost != 2.5": {"missing", "wrong"},
		"Stage = Done": {"right"}, "Stage == /Do.*/i": {"right"}, "Stage != Done": {},
		"@endAt < '2026-09-01'": {"wrong"}, "@endAt >= '2026-09-01' or @endAt = none": {"missing", "right"},
		"not @endAt < '2026-09-01'":                                     {"missing", "right"},
		"(@endAt >= '2026-09-01' or @endAt = none) and '@endAt' = keep": {"right"},
	} {
		ids, ok := w.s.filteredCardIDs(ctx, b.ID, filter)
		if !ok {
			t.Errorf("%q does not parse", filter)
			continue
		}
		var got []string
		for id := range ids {
			got = append(got, name[id])
		}
		sort.Strings(got)
		if want == nil {
			want = []string{}
		}
		if got == nil {
			got = []string{}
		}
		if !slices.Equal(got, want) {
			t.Errorf("%q: %v, want %v", filter, got, want)
		}
	}
	// Negative: a filter that does not parse is an error, not "everything".
	for _, bad := range []string{"Points = 2 or", "Cost > 2hours", "UnknownField = 2", "@endAt = '2026-02-30'", "'unfinished", "(Points = 2", "Points = /[/"} {
		if _, ok := w.s.filteredCardIDs(ctx, b.ID, bad); ok {
			t.Errorf("negative: %q parses", bad)
		}
		if w.s.cardMatchesAdvancedFilter(ctx, b.ID, right.ID, bad) {
			t.Errorf("negative: %q matches", bad)
		}
	}
	if !w.s.cardMatchesAdvancedFilter(ctx, b.ID, right.ID, "Points = 2") || w.s.cardMatchesAdvancedFilter(ctx, b.ID, wrong.ID, "Points = 2") ||
		w.s.cardMatchesAdvancedFilter(ctx, b.ID, right.ID, " ") {
		t.Error("one card")
	}
	// A string that is not a date is none of them, and never compared.
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": wrong.ID}, bson.M{"$set": bson.M{"endAt": "not-a-date"}})
	if ids, _ := w.s.filteredCardIDs(ctx, b.ID, "@endAt != '2026-08-01'"); len(ids) != 1 || !ids[right.ID] {
		t.Errorf("not a date: %v", ids)
	}
	if r := parseAdvancedFilterDate("31/02/2026", true); r != nil {
		t.Error("negative: 31 February is no date")
	}
	if r := parseAdvancedFilterDate("06/04/2020", true); r == nil || r.start.Month() != 4 || r.start.Day() != 6 {
		t.Errorf("day first: %v", r)
	}
}

// The board with an Advanced Filter shows only its cards; one that does not
// parse keeps the last that did; and a rule's advanced-filter trigger fires on
// the card's activity only when the card matches.
func TestAdvancedFilterBoardAndRule(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Filtered")
	list, _ := w.s.AddList(ctx, b, "L")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	one, _ := w.s.AddCard(ctx, b, u, list.ID, sw.ID, "one")
	two, _ := w.s.AddCard(ctx, b, u, list.ID, sw.ID, "two")
	_, _ = w.db.Collection("customFields").InsertOne(ctx, bson.M{"_id": "points000000001", "name": "Points", "type": "number", "boardIds": bson.A{b.ID}})
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": two.ID}, bson.M{"$set": bson.M{"customFields": bson.A{bson.M{"_id": "points000000001", "value": 2}}}})
	c := w.client(t)
	body(t, w.signIn(t, c, "ann", "pw"))
	page := func(q string) string {
		_, html := get(t, c, w.srv.URL+"/b/"+b.ID+"/"+b.Slug+q)
		return html
	}
	has := func(html, id string) bool { return strings.Contains(html, `data-card-id="`+id+`"`) }
	if h := page(""); !has(h, one.ID) || !has(h, two.ID) || !strings.Contains(h, "js-field-advanced-filter") || strings.Contains(h, "js-filter-reset") {
		t.Fatal("the board, unfiltered, with the Filter view")
	}
	if h := page("?advanced=" + url.QueryEscape("Points = 2")); has(h, one.ID) || !has(h, two.ID) || !strings.Contains(h, "js-filter-reset") {
		t.Error("filtered to the matching card")
	}
	if h := page("?advanced=" + url.QueryEscape("Points = 2 or") + "&last=" + url.QueryEscape("Points = 2")); has(h, one.ID) || !has(h, two.ID) {
		t.Error("negative: a filter that does not parse keeps the last that did")
	}
	if h := page("?advanced=" + url.QueryEscape("Points = 2 or")); !has(h, one.ID) || !has(h, two.ID) || strings.Contains(h, "js-filter-reset") {
		t.Error("negative: with none that did, nothing is filtered")
	}
	addRule(t, w, b.ID, "radv", bson.M{"activityType": "advancedFilterTrigger", "advancedFilter": "not(Points = 1) and (Points = 2)"},
		bson.M{"actionType": "markCardComplete"}, true)
	addRule(t, w, b.ID, "rbad", bson.M{"activityType": "advancedFilterTrigger", "advancedFilter": "Points = 2 or"},
		bson.M{"actionType": "setColor", "selectedColor": "red"}, true)
	for _, id := range []string{one.ID, two.ID} {
		w.s.activity(ctx, u.ID, "a-changedTitle", bson.M{"boardId": b.ID, "cardId": id})
	}
	done := func(id string) bson.M {
		var r bson.M
		_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": id}).Decode(&r)
		return r
	}
	if done(two.ID)["dueComplete"] != true || done(one.ID)["dueComplete"] == true {
		t.Errorf("the matching card only: %v %v", done(two.ID)["dueComplete"], done(one.ID)["dueComplete"])
	}
	if done(two.ID)["color"] != nil {
		t.Error("negative: a trigger that does not parse never fires")
	}
}
