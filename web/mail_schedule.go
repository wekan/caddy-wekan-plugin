package web

import (
	"context"
	"regexp"
	"slices"
	"strconv"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// When and how a notification e-mail is delivered (#5171), as Node.js WeKan's
// models/lib/notificationDelivery.js: the member's Member Settings, else the
// board's, else Admin Panel's, else the built-in - every e-mail due for the
// recipient combined after EMAIL_NOTIFICATION_TIMEOUT. A corrupt value at any
// level is ignored, never trusted.

type emailDelivery struct {
	Grouping, Schedule, DailyTime, QuietStart, QuietEnd, Timezone string
	IntervalMinutes                                               int
	GroupingFromDefault                                           bool
}

var (
	emailGroupings  = []string{"all", "board", "card", "none"}
	emailSchedules  = []string{"immediate", "interval", "daily"}
	emailIntervals  = []int{5, 10, 15, 30, 60, 120, 240, 480, 720, 1440}
	hhmm            = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)
	timezonePattern = regexp.MustCompile(`^[A-Za-z0-9_+\-/]+$`)
)

func validTimezone(v string) bool {
	if v == "" || len(v) > 64 || !timezonePattern.MatchString(v) {
		return false
	}
	_, err := time.LoadLocation(v)
	return err == nil
}

// nestedField is a nested value of a decoded document.
func nestedField(v any, keys ...string) any {
	for _, k := range keys {
		switch d := v.(type) {
		case bson.M:
			v = d[k]
		case bson.D:
			v = docMap(d)[k]
		case map[string]any:
			v = d[k]
		default:
			return nil
		}
	}
	return v
}

// emailDeliveryFor resolves the e-mail channel for a member on a board.
func (s *Server) emailDeliveryFor(ctx context.Context, userID, boardID string) emailDelivery {
	var user, board, set bson.M
	_ = s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(userID)}).Decode(&user)
	if validID(boardID) {
		_ = s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(boardID)}).Decode(&board)
	}
	_ = s.db.Collection("settings").FindOne(ctx, bson.M{}).Decode(&set)
	levels := []any{nestedField(user, "profile", "notificationDelivery", "email"),
		nestedField(board, "notificationDelivery", "email"), nestedField(set, "notificationDelivery", "email")}
	str := func(key string, ok func(string) bool) (string, bool) {
		for _, l := range levels {
			if v, isStr := nestedField(l, key).(string); isStr && ok(v) {
				return v, true
			}
		}
		return "", false
	}
	d := emailDelivery{Grouping: "all", Schedule: "immediate", IntervalMinutes: 60, DailyTime: "08:00"}
	var found bool
	if d.Grouping, found = str("grouping", func(v string) bool { return slices.Contains(emailGroupings, v) }); !found {
		d.Grouping, d.GroupingFromDefault = "all", true
	}
	if v, ok := str("schedule", func(v string) bool { return slices.Contains(emailSchedules, v) }); ok {
		d.Schedule = v
	}
	for _, l := range levels {
		if n := intOf(nestedField(l, "intervalMinutes")); slices.Contains(emailIntervals, n) {
			d.IntervalMinutes = n
			break
		}
	}
	if v, ok := str("dailyTime", hhmm.MatchString); ok {
		d.DailyTime = v
	}
	if v, ok := str("timezone", validTimezone); ok {
		d.Timezone = v
	}
	// Quiet hours are one setting: both ends from the same level.
	for _, l := range levels {
		start, _ := nestedField(l, "quietStart").(string)
		if start == "" {
			continue
		}
		end, _ := nestedField(l, "quietEnd").(string)
		if hhmm.MatchString(start) && hhmm.MatchString(end) {
			d.QuietStart, d.QuietEnd = start, end
		}
		break
	}
	return d
}

// emailGroupKey is groupKeyFor: the key the jobs of one e-mail share.
func emailGroupKey(grouping, boardID, cardID, eventID string) string {
	switch grouping {
	case "all":
		return "all"
	case "board":
		return "board:" + orDash(boardID)
	case "card":
		if cardID != "" {
			return "card:" + cardID
		}
		return "board:" + orDash(boardID)
	case "none":
		return "single:" + eventID
	}
	return ""
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func minutesOfDay(t string) int {
	h, _ := strconv.Atoi(t[:2])
	m, _ := strconv.Atoi(t[3:])
	return h*60 + m
}

// localAt is the instant the clock in loc shows y-m-d h:m; Go's time.Date
// settles a daylight-saving change itself.
func localAt(y int, mo time.Month, d, minutes int, loc *time.Location) time.Time {
	return time.Date(y, mo, d, minutes/60, minutes%60, 0, 0, loc)
}

func nextIntervalBoundary(from time.Time, interval int, loc *time.Location) time.Time {
	l := from.In(loc)
	next := ((l.Hour()*60+l.Minute())/interval + 1) * interval
	day := time.Date(l.Year(), l.Month(), l.Day()+next/1440, 0, 0, 0, 0, time.UTC)
	return localAt(day.Year(), day.Month(), day.Day(), next%1440, loc)
}

func nextDailyTime(from time.Time, at string, loc *time.Location) time.Time {
	l := from.In(loc)
	for days := 0; days < 3; days++ {
		t := localAt(l.Year(), l.Month(), l.Day()+days, minutesOfDay(at), loc)
		if !t.Before(from) {
			return t
		}
	}
	return from.Add(24 * time.Hour)
}

func inQuietHours(t time.Time, start, end string, loc *time.Location) bool {
	if start == "" || end == "" || start == end {
		return false
	}
	l := t.In(loc)
	now, a, b := l.Hour()*60+l.Minute(), minutesOfDay(start), minutesOfDay(end)
	if a < b {
		return now >= a && now < b
	}
	return now >= a || now < b
}

// nextDeliveryAt is WeKan's: deterministic, so every notification of one
// window shares one time - and one e-mail.
func nextDeliveryAt(created time.Time, d emailDelivery, base time.Duration) time.Time {
	loc := time.UTC
	if d.Timezone != "" {
		if l, err := time.LoadLocation(d.Timezone); err == nil {
			loc = l
		}
	}
	var at time.Time
	switch d.Schedule {
	case "interval":
		at = nextIntervalBoundary(created, max(d.IntervalMinutes, 1), loc)
	case "daily":
		at = nextDailyTime(created, d.DailyTime, loc)
	default:
		at = created.Add(base)
	}
	if inQuietHours(at, d.QuietStart, d.QuietEnd, loc) {
		at = nextDailyTime(at, d.QuietEnd, loc)
	}
	return at.UTC()
}
