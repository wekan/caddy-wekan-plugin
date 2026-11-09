package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// WeKan's cronSchedule: five fields, in UTC, names, ranges, steps, 7 = Sunday,
// and either day when both day fields are restricted.
func TestCronSchedule(t *testing.T) {
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	for _, c := range []struct {
		expr, when string
		want       bool
	}{
		{"30 5 * * 1-5", "2026-11-09T05:30:00Z", true}, {"30 5 * * 1-5", "2026-11-08T05:30:00Z", false},
		{"*/15 * * * *", "2026-11-09T10:45:00Z", true}, {"*/15 * * * *", "2026-11-09T10:44:00Z", false},
		{"0 0 1,15 * *", "2026-11-15T00:00:00Z", true}, {"0 9 * JAN-MAR MON", "2026-02-02T09:00:00Z", true},
		{"0 9 * * 7", "2026-11-08T09:00:00Z", true}, {"0 9 13 * 5", "2026-11-13T09:00:00Z", true},
		{"0 9 13 * 5", "2026-11-20T09:00:00Z", true}, {"0 9 13 * 5", "2026-11-19T09:00:00Z", false},
	} {
		spec, err := parseCron(c.expr)
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		if got := spec.matches(at(c.when)); got != c.want {
			t.Errorf("%s at %s: %v", c.expr, c.when, got)
		}
	}
	for _, bad := range []string{"", "* * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "5-1 * * * *", "*/0 * * * *", "+5 * * * *", "* * * FOO *"} {
		if _, err := parseCron(bad); err == nil {
			t.Errorf("negative: %q parses", bad)
		}
	}
}

// The minute scan: a calendar schedule at its time, a due-soon trigger on its
// cards, an aging one, a cron one - each once a minute, never when disabled.
func TestScheduledRules(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Scheduled")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"createdBy": u.ID}})
	todo, _ := w.s.AddList(ctx, b, "Todo")
	w.s.AddList(ctx, b, "Empty") // a trigger selecting no cards runs a board-level action once, as WeKan's
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	now := time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local) // a Monday
	soon, _ := w.s.AddCard(ctx, b, u, todo.ID, sw.ID, "soon")
	later, _ := w.s.AddCard(ctx, b, u, todo.ID, sw.ID, "later")
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": soon.ID}, bson.M{"$set": bson.M{"dueAt": now.Add(12 * time.Hour)}})
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": later.ID}, bson.M{"$set": bson.M{"dueAt": now.Add(72 * time.Hour)}})
	addRule(t, w, b.ID, "rdue", bson.M{"activityType": "scheduledTrigger", "scheduleKind": "due", "dueCondition": "soon", "days": float64(1), "atTime": "09:00", "listName": "*"},
		bson.M{"actionType": "setColor", "selectedColor": "red"}, true)
	addRule(t, w, b.ID, "rweekly", bson.M{"activityType": "scheduledTrigger", "scheduleKind": "calendar", "scheduleType": "weekly", "weekday": float64(1), "atTime": "09:00", "listName": "Empty"},
		bson.M{"actionType": "createCard", "listName": "Todo", "swimlaneName": "*", "cardName": "Weekly"}, true)
	addRule(t, w, b.ID, "rtuesday", bson.M{"activityType": "scheduledTrigger", "scheduleKind": "calendar", "scheduleType": "weekly", "weekday": float64(2), "atTime": "09:00", "listName": "*"},
		bson.M{"actionType": "setColor", "selectedColor": "blue"}, true)
	addRule(t, w, b.ID, "roff", bson.M{"activityType": "scheduledTrigger", "scheduleKind": "calendar", "scheduleType": "daily", "atTime": "09:00", "listName": "*"},
		bson.M{"actionType": "setColor", "selectedColor": "green"}, false)
	addRule(t, w, b.ID, "rcron", bson.M{"activityType": "scheduledTrigger", "scheduleKind": "cron", "cron": "0 " + itoa(now.UTC().Hour()) + " * * *", "listName": "*"},
		bson.M{"actionType": "markCardComplete"}, true)
	addRule(t, w, b.ID, "rbadcron", bson.M{"activityType": "scheduledTrigger", "scheduleKind": "cron", "cron": "nonsense", "listName": "*"},
		bson.M{"actionType": "archive"}, true)
	color := func(id string) any {
		var c bson.M
		_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": id}).Decode(&c)
		return c["color"]
	}
	count := func(title string) int64 {
		n, _ := w.db.Collection("cards").CountDocuments(ctx, bson.M{"boardId": b.ID, "title": title})
		return n
	}
	w.s.scanScheduledRules(ctx, now)
	w.s.scanScheduledRules(ctx, now) // the same minute: once
	if color(soon.ID) != "red" || color(later.ID) != nil {
		t.Errorf("due soon, its cards only: %v %v", color(soon.ID), color(later.ID))
	}
	if count("Weekly") != 1 {
		t.Errorf("a board-level action on its day, once a minute: %d", count("Weekly"))
	}
	var c bson.M
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": later.ID}).Decode(&c)
	if c["dueComplete"] != true || c["archived"] == true {
		t.Errorf("cron, and negative: an invalid cron never runs: %v", c)
	}
	// Negative: not its day, not its time, disabled.
	w.s.scanScheduledRules(ctx, now.Add(time.Minute))
	if count("Weekly") != 1 || strings.Contains(fmtAny(color(soon.ID)), "blue") || color(soon.ID) == "green" {
		t.Error("negative: another minute, another day, a disabled rule")
	}
	// Aging: a card days in its list.
	addRule(t, w, b.ID, "raging", bson.M{"activityType": "scheduledTrigger", "scheduleKind": "aging", "days": float64(7), "atTime": "10:00", "listName": "Todo"},
		bson.M{"actionType": "setColor", "selectedColor": "gold"}, true)
	_, _ = w.db.Collection("activities").UpdateMany(ctx, bson.M{"cardId": soon.ID}, bson.M{"$set": bson.M{"createdAt": now.Add(-8 * 24 * time.Hour)}})
	w.s.scanScheduledRules(ctx, now.Add(time.Hour))
	if color(soon.ID) != "gold" || color(later.ID) == "gold" {
		t.Errorf("aging: %v %v", color(soon.ID), color(later.ID))
	}
}

func fmtAny(v any) string {
	s, _ := v.(string)
	return s
}
