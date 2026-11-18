package web

import (
	"context"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The member's date settings, as WeKan's Member Settings / Date (#4335):
// changeDateFormat, changeStartDayOfWeek and changeCalendarSystem, each
// refusing what WeKan's schema and catalog refuse, and the page drawing dates
// in the chosen calendar from the stored Gregorian ones.
func TestMemberDateSettings(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	u := w.seedUser(t, "ann", "pw")
	cl := w.client(t)
	body(t, w.signIn(t, cl, "ann", "pw"))
	ok := func(method, args string) {
		t.Helper()
		if _, out := methodCall(t, w, cl, method, args); out["result"] != true {
			t.Fatalf("%s %s: %v", method, args, out)
		}
	}
	refused := func(method, args, want string) {
		t.Helper()
		if _, out := methodCall(t, w, cl, method, args); out["error"] != want {
			t.Fatalf("negative: %s %s: %v, want %s", method, args, out, want)
		}
	}
	ok("changeDateFormat", `["DD-MM-YYYY",true]`)
	ok("changeStartDayOfWeek", `[0]`)
	ok("changeCalendarSystem", `["buddhist"]`)
	var got struct {
		Profile struct {
			DateFormat         string `bson:"dateFormat"`
			DateFormatOverride bool   `bson:"dateFormatOverride"`
			CalendarSystem     string `bson:"calendarSystem"`
			StartDayOfWeek     *int   `bson:"startDayOfWeek"`
		} `bson:"profile"`
	}
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"_id": u.ID}).Decode(&got)
	if got.Profile.DateFormat != "DD-MM-YYYY" || !got.Profile.DateFormatOverride || got.Profile.CalendarSystem != "buddhist" || got.Profile.StartDayOfWeek == nil || *got.Profile.StartDayOfWeek != 0 {
		t.Fatalf("stored: %+v", got.Profile)
	}
	refused("changeCalendarSystem", `["martian"]`, "invalid-calendar-system")
	refused("changeCalendarSystem", `[5]`, "Match.Error")
	refused("changeStartDayOfWeek", `[7]`, "Match.Error")
	refused("changeStartDayOfWeek", `[1.5]`, "Match.Error")
	refused("changeStartDayOfWeek", `["1"]`, "Match.Error")
	refused("changeDateFormat", `["%Y $where"]`, "Match.Error")
	refused("changeDateFormat", `["YYYY-MM-DD","yes"]`, "Match.Error")
	if code, _ := methodCall(t, w, w.client(t), "changeCalendarSystem", `["jalali"]`); code == 200 {
		t.Fatal("negative: signed out, nothing is changed")
	}
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"_id": u.ID}).Decode(&got)
	if got.Profile.CalendarSystem != "buddhist" {
		t.Fatalf("a refused change changed it: %s", got.Profile.CalendarSystem)
	}

	// The page carries the calendar and the settings pop-over shows the
	// saved choice, every calendar of the catalog offered.
	b, _ := w.s.CreateBoard(ctx, u, "Dates")
	res, _ := cl.Get(w.srv.URL + "/b/" + b.ID + "/x")
	h := body(t, res)
	for _, want := range []string{`data-calendar-system="buddhist"`, `data-start-day="0"`, `data-date-format="DD-MM-YYYY"`,
		`value="buddhist" selected`, `value="0" selected`, `value="DD-MM-YYYY" selected`, `js-member-date-settings`} {
		if !strings.Contains(h, want) {
			t.Errorf("the page: %s", want)
		}
	}
	for _, id := range calendarSystemIDs {
		if !strings.Contains(h, `value="`+id+`"`) {
			t.Errorf("calendar %s is offered", id)
		}
	}
	// An unknown stored value is drawn as Gregorian, never echoed.
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": u.ID}, bson.M{"$set": bson.M{"profile.calendarSystem": `x"><script>`, "profile.startDayOfWeek": 9}})
	res, _ = cl.Get(w.srv.URL + "/b/" + b.ID + "/x")
	if h := body(t, res); !strings.Contains(h, `data-calendar-system="gregorian"`) || !strings.Contains(h, `data-start-day="1"`) || strings.Contains(h, "<script>\"") {
		t.Fatal("negative: an unknown calendar or day falls back to Gregorian and Monday")
	}
}
