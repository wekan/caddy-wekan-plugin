package web

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Rules' scheduled triggers, as Node.js WeKan's server/scheduledRules.js,
// models/lib/scheduledDueFilter.js and models/lib/cronSchedule.js: once a
// minute, each scheduledTrigger whose time has come runs its rule's actions -
// on every card it selects, or once on the board for a board-level action -
// as the board's creator, and records the minute it ran, so it runs once.
//
//   calendar  at atTime: once on onDate, daily, on weekdays, weekly on
//             weekday, or monthly on dayOfMonth (the server's time zone)
//   due       at atTime, the cards whose due (or start) date is set, soon
//             or overdue by days
//   aging     at atTime, the cards days or more in their list
//   cron      a five-field cron expression, in UTC as GitHub Actions runs it

const boardLevelActions = "createCard addSwimlane moveAllCardsInList"

// cronSpec is a parsed cron expression.
type cronSpec struct {
	sets                             [5]map[int]bool
	dayRestricted, weekdayRestricted bool
}

var cronFields = []struct {
	name     string
	min, max int
	names    []string
}{
	{"minute", 0, 59, nil}, {"hour", 0, 23, nil}, {"day of month", 1, 31, nil},
	{"month", 1, 12, []string{"JAN", "FEB", "MAR", "APR", "MAY", "JUN", "JUL", "AUG", "SEP", "OCT", "NOV", "DEC"}},
	{"day of week", 0, 7, []string{"SUN", "MON", "TUE", "WED", "THU", "FRI", "SAT"}},
}

var cronPart = regexp.MustCompile(`^(\*|[^/-]+(?:-[^/]+)?)(?:/(\d+))?$`)

// parseCron is WeKan's parseCron.
func parseCron(expr string) (*cronSpec, error) {
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return nil, errors.New("a cron schedule has five fields: minute hour day month weekday")
	}
	c := &cronSpec{dayRestricted: parts[2] != "*", weekdayRestricted: parts[4] != "*"}
	for i, text := range parts {
		f := cronFields[i]
		value := func(t string) (int, error) {
			for k, n := range f.names {
				if strings.EqualFold(t, n) {
					if f.name == "month" {
						return k + 1, nil
					}
					return k, nil
				}
			}
			n, err := strconv.Atoi(t)
			if err != nil || strings.ContainsAny(t, "+-") {
				return 0, fmt.Errorf("%s: %q is not a number", f.name, t)
			}
			if n < f.min || n > f.max {
				return 0, fmt.Errorf("%s: %d is out of %d-%d", f.name, n, f.min, f.max)
			}
			return n, nil
		}
		set := map[int]bool{}
		for _, part := range strings.Split(text, ",") {
			m := cronPart.FindStringSubmatch(part)
			if m == nil {
				return nil, fmt.Errorf("%s: %q is not valid", f.name, part)
			}
			step := 1
			if m[2] != "" {
				step, _ = strconv.Atoi(m[2])
			}
			if step < 1 {
				return nil, fmt.Errorf("%s: a step must be at least 1", f.name)
			}
			top := f.max
			if f.name == "day of week" {
				top = 6
			}
			var from, to int
			var err error
			switch {
			case m[1] == "*":
				from, to = f.min, top
			case strings.Contains(m[1], "-"):
				ab := strings.SplitN(m[1], "-", 2)
				if from, err = value(ab[0]); err != nil {
					return nil, err
				}
				if to, err = value(ab[1]); err != nil {
					return nil, err
				}
			default:
				if from, err = value(m[1]); err != nil {
					return nil, err
				}
				to = from
				if m[2] != "" {
					to = top
				}
			}
			if from > to {
				return nil, fmt.Errorf("%s: %d-%d runs backwards", f.name, from, to)
			}
			for n := from; n <= to; n += step {
				if f.name == "day of week" && n == 7 {
					set[0] = true
				} else {
					set[n] = true
				}
			}
		}
		c.sets[i] = set
	}
	return c, nil
}

// matches is WeKan's cronMatches: the minute of t, in UTC; with both day
// fields restricted, either day matches.
func (c *cronSpec) matches(t time.Time) bool {
	t = t.UTC()
	if !c.sets[0][t.Minute()] || !c.sets[1][t.Hour()] || !c.sets[3][int(t.Month())] {
		return false
	}
	day, weekday := c.sets[2][t.Day()], c.sets[4][int(t.Weekday())]
	if c.dayRestricted && c.weekdayRestricted {
		return day || weekday
	}
	return day && weekday
}

// calendarDue is WeKan's: has this calendar trigger's day come?
func calendarDue(t bson.M, now time.Time) bool {
	switch docString(t, "scheduleType") {
	case "once":
		return docString(t, "onDate") == now.Format("2006-01-02")
	case "daily":
		return true
	case "weekday":
		return now.Weekday() >= time.Monday && now.Weekday() <= time.Friday
	case "weekly":
		return fmt.Sprint(t["weekday"]) == strconv.Itoa(int(now.Weekday())) || numberOf(t["weekday"]) == float64(now.Weekday())
	case "monthly":
		return fmt.Sprint(t["dayOfMonth"]) == strconv.Itoa(now.Day()) || numberOf(t["dayOfMonth"]) == float64(now.Day())
	}
	return false
}

// triggerDays is the trigger's days, as JavaScript's Number(..) || 0.
func triggerDays(t bson.M) float64 {
	switch v := t["days"].(type) {
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err == nil {
			return f
		}
		return 0
	}
	if n := numberOf(t["days"]); n == n { // not NaN
		return n
	}
	return 0
}

// matchesScheduledDate is WeKan's: the card's due - or start - date is set,
// ahead within days, or passed at least days ago.
func matchesScheduledDate(card bson.M, t bson.M, now time.Time) bool {
	field := "dueAt"
	if docString(t, "dateField") == "startAt" {
		field = "startAt"
	}
	at, ok := plain(card[field]).(time.Time)
	if !ok || at.IsZero() {
		return false
	}
	window := time.Duration(triggerDays(t) * float64(24*time.Hour))
	switch docString(t, "dueCondition") {
	case "set":
		return true
	case "soon":
		return !at.Before(now) && at.Sub(now) <= window
	case "overdue":
		return now.Sub(at) >= window
	}
	return false
}

// scheduledCards are the cards a scheduled trigger acts on.
func (s *Server) scheduledCards(ctx context.Context, t bson.M, now time.Time) []bson.M {
	sel := bson.M{"boardId": eq(docString(t, "boardId")), "archived": false}
	if name := docString(t, "listName"); name != "" && name != "*" {
		var l struct {
			ID string `bson:"_id"`
		}
		if s.db.Collection("lists").FindOne(ctx, bson.M{"title": eq(name), "boardId": eq(docString(t, "boardId"))}).Decode(&l) != nil {
			return nil
		}
		sel["listId"] = l.ID
	}
	cur, err := s.db.Collection("cards").Find(ctx, sel)
	if err != nil {
		return nil
	}
	var cards []bson.M
	_ = cur.All(ctx, &cards)
	var out []bson.M
	for _, c := range cards {
		switch docString(t, "scheduleKind") {
		case "due":
			if !matchesScheduledDate(c, t, now) {
				continue
			}
		case "aging":
			// Days in its list: since its last move or creation, else its own creation.
			since, _ := plain(c["createdAt"]).(time.Time)
			var act struct {
				CreatedAt time.Time `bson:"createdAt"`
			}
			if s.db.Collection("activities").FindOne(ctx, bson.M{"cardId": eq(docString(c, "_id")), "activityType": bson.M{"$in": bson.A{"moveCard", "createCard"}}},
				options.FindOne().SetSort(bson.D{{Key: "createdAt", Value: -1}})).Decode(&act) == nil {
				since = act.CreatedAt
			}
			if since.IsZero() || now.Sub(since).Hours()/24 < triggerDays(t) {
				continue
			}
		}
		out = append(out, c)
	}
	return out
}

// runScheduledTrigger is WeKan's runDueTrigger.
func (s *Server) runScheduledTrigger(ctx context.Context, t bson.M, slot string, now time.Time) {
	tid, boardID := docString(t, "_id"), docString(t, "boardId")
	var rule bson.M
	if s.db.Collection("rules").FindOne(ctx, bson.M{"boardId": eq(boardID), "$or": bson.A{bson.M{"triggerId": eq(tid)}, bson.M{"extraTriggerIds": eq(tid)}}}).Decode(&rule) != nil ||
		rule["enabled"] == false {
		return
	}
	actions := s.ruleActions(ctx, rule)
	if len(actions) == 0 {
		return
	}
	var b Board
	if s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(boardID)}).Decode(&b) != nil {
		return
	}
	var creator struct {
		CreatedBy string `bson:"createdBy"`
	}
	_ = s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(boardID)}, options.FindOne().SetProjection(bson.M{"createdBy": 1})).Decode(&creator)
	cards := s.scheduledCards(ctx, t, now)
	// The activity a scheduled run is, as WeKan's: the board's, by its creator.
	run := func(cardID string) bson.M {
		a := bson.M{"activityType": "scheduledTrigger"}
		a["boardId"], a["userId"] = boardID, creator.CreatedBy
		if cardID != "" {
			a["cardId"] = cardID
		}
		return a
	}
	for _, act := range actions {
		if len(cards) == 0 && strings.Contains(" "+boardLevelActions+" ", " "+docString(act, "actionType")+" ") {
			_ = s.performAction(ctx, run(""), act)
			continue
		}
		for _, c := range cards {
			if err := s.performAction(ctx, run(docString(c, "_id")), act); err != nil {
				s.log.Warn("scheduled rule action", "rule", rule["_id"], "error", err)
			}
		}
	}
	_, _ = s.db.Collection("triggers").UpdateOne(ctx, bson.M{"_id": eq(tid)}, bson.M{"$set": bson.M{"lastRunKey": slot, "lastRunAt": now}})
}

// scanScheduledRules is WeKan's scanScheduledRules: every scheduled trigger
// whose minute has come, once.
func (s *Server) scanScheduledRules(ctx context.Context, now time.Time) {
	hhmm := now.Format("15:04")
	slot := now.Format("2006-01-02") + " " + hhmm
	cur, err := s.db.Collection("triggers").Find(ctx, bson.M{"activityType": "scheduledTrigger"})
	if err != nil {
		return
	}
	var triggers []bson.M
	_ = cur.All(ctx, &triggers)
	for _, t := range triggers {
		if docString(t, "lastRunKey") == slot {
			continue // already ran this minute
		}
		if docString(t, "scheduleKind") == "cron" {
			c, err := parseCron(docString(t, "cron"))
			if err != nil || !c.matches(now) {
				continue
			}
			s.runScheduledTrigger(ctx, t, slot, now)
			continue
		}
		if at := docString(t, "atTime"); at != "" && at != hhmm {
			continue
		}
		if docString(t, "scheduleKind") == "calendar" && !calendarDue(t, now) {
			continue
		}
		s.runScheduledTrigger(ctx, t, slot, now)
	}
}

// runScheduledRules scans once a minute, on the minute, until stop.
func (s *Server) runScheduledRules(stop <-chan struct{}) {
	for {
		next := time.Now().Truncate(time.Minute).Add(time.Minute)
		select {
		case <-stop:
			return
		case <-time.After(time.Until(next)):
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			s.scanScheduledRules(ctx, time.Now())
			cancel()
		}
	}
}
