package web

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

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

func TestBoardDateFormatCascade(t *testing.T) {
	u := new(User)
	b := &Board{DateFormat: "DD-MM-YYYY-date-only"}
	u.Profile.DateFormat = "MM-DD-YYYY"
	site := siteDateFormat{Format: "DD-MM-YYYY"}
	check := func(label, want string) {
		t.Helper()
		if got := boardDateFormat(u, b, site); got != want {
			t.Errorf("%s: got %q, want %q", label, got, want)
		}
	}
	check("unchecked choices", "YYYY-MM-DD")
	site.Enabled = true
	check("site", "DD-MM-YYYY")
	b.DateFormatOverride = true
	check("board", "DD-MM-YYYY-date-only")
	u.Profile.DateFormatOverride = true
	check("member", "MM-DD-YYYY")
	u.Profile.DateFormat = "invalid"
	check("invalid member", "DD-MM-YYYY-date-only")
	if got := boardDateFormat(nil, b, site); got != b.DateFormat {
		t.Errorf("guest: got %q, want %q", got, b.DateFormat)
	}
}

func TestBoardDateFormatRendering(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	u := w.seedUser(t, "ann", "pw")
	client := w.client(t)
	body(t, w.signIn(t, client, "ann", "pw"))
	board, err := w.s.CreateBoard(ctx, u, "Dates")
	if err != nil {
		t.Fatal(err)
	}
	list, err := w.s.AddList(ctx, board, "List")
	if err != nil {
		t.Fatal(err)
	}
	var lane Swimlane
	if err := w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": board.ID}).Decode(&lane); err != nil {
		t.Fatal(err)
	}
	card, err := w.s.AddCard(ctx, board, u, list.ID, lane.ID, "Dated card")
	if err != nil {
		t.Fatal(err)
	}
	set := func(coll string, query, values bson.M) {
		t.Helper()
		if _, err := w.db.Collection(coll).UpdateOne(ctx, query, bson.M{"$set": values}); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Date(2026, 3, 21, 12, 0, 0, 0, time.UTC)
	set("cards", bson.M{"_id": card.ID}, bson.M{"dueAt": at})
	set("settings", bson.M{}, bson.M{"globalDateFormat": "DD-MM-YYYY-date-only", "hideDateFormat": true})
	set("users", bson.M{"_id": u.ID}, bson.M{"profile.dateFormat": "MM-DD-YYYY", "profile.dateFormatOverride": false})
	boardURL := w.srv.URL + "/b/" + board.ID + "/" + board.Slug
	assertDate := func(url, want string) {
		t.Helper()
		_, html := get(t, client, url)
		if !regexp.MustCompile(`<time datetime="` + at.Format("2006-01-02T15:04:05") + `(?:\.000)?Z">` + want + `</time>`).MatchString(html) {
			t.Fatalf("wanted rendered date %q with the unchanged datetime at %s, got %q", want, url,
				regexp.MustCompile(`<time[^>]*>[^<]*</time>`).FindAllString(html, -1))
		}
	}
	assertDate(boardURL, "21-03-2026")
	assertDate(boardURL+"/"+card.ID, "21-03-2026")
	boardFormatArgs := `["` + board.ID + `","YYYY-MM-DD-date-only",true]`
	member := w.seedUser(t, "member", "pw")
	if _, err := w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": board.ID},
		bson.M{"$push": bson.M{"members": bson.M{"userId": member.ID, "isActive": true}}}); err != nil {
		t.Fatal(err)
	}
	memberClient := w.client(t)
	body(t, w.signIn(t, memberClient, "member", "pw"))
	if _, out := methodCall(t, w, memberClient, "setBoardDateFormat", boardFormatArgs); out["error"] != "not-authorized" {
		t.Fatalf("ordinary member date format change: %v", out)
	}
	if _, out := methodCall(t, w, client, "setBoardDateFormat", boardFormatArgs); out["result"] != float64(1) {
		t.Fatalf("board admin date format change: %v", out)
	}
	assertDate(boardURL, "2026-03-21")
	set("users", bson.M{"_id": u.ID}, bson.M{"profile.dateFormat": "MM-DD-YYYY-date-only", "profile.dateFormatOverride": true})
	assertDate(boardURL, "03-21-2026")
}
