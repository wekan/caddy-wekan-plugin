package web

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The member's date settings, as WeKan's Member Settings / Date format
// (dateFormatSettings.jade): the format, the first day of the week and the
// calendar dates are drawn in (#4335). Dates stay stored as Gregorian; the
// calendar only changes how weddy.js draws them, as WeKan's dateDisplay.js.

// dateFormats is WeKan's models/lib/dateFormatPolicy.js DATE_FORMATS.
var dateFormats = []string{"YYYY-MM-DD", "DD-MM-YYYY", "MM-DD-YYYY",
	"YYYY-MM-DD-date-only", "DD-MM-YYYY-date-only", "MM-DD-YYYY-date-only"}

type siteDateFormat struct {
	HideLogo bool   `bson:"hideLogo"`
	Format   string `bson:"globalDateFormat"`
	Enabled  bool   `bson:"hideDateFormat"`
}

func (s *Server) siteDateFormat(ctx context.Context) siteDateFormat {
	var site siteDateFormat
	_ = s.db.Collection("settings").FindOne(ctx, bson.M{}).Decode(&site)
	return site
}

// boardDateFormat applies WeKan's member, board, instance, default order.
func boardDateFormat(u *User, b *Board, site siteDateFormat) string {
	if u != nil && u.Profile.DateFormatOverride && slices.Contains(dateFormats, u.Profile.DateFormat) {
		return u.Profile.DateFormat
	}
	if b.DateFormatOverride && slices.Contains(dateFormats, b.DateFormat) {
		return b.DateFormat
	}
	if site.Enabled && slices.Contains(dateFormats, site.Format) {
		return site.Format
	}
	return dateFormats[0]
}

// calendarSystemIDs is WeKan's imports/lib/calendarSystems.js
// CALENDAR_SYSTEM_IDS: the profile names, and Intl's for the rest.
var calendarSystemIDs = []string{"gregorian", "jalali", "buddhist", "chinese", "coptic", "dangi",
	"ethioaa", "ethiopic", "hebrew", "indian", "islamic", "islamic-civil",
	"islamic-rgsa", "islamic-tbla", "islamic-umalqura", "iso8601", "japanese", "roc"}

// calendarIntl is the Intl calendar a profile name draws with.
func calendarIntl(id string) string {
	switch id {
	case "gregorian", "":
		return "gregory"
	case "jalali":
		return "persian"
	}
	return id
}

func (u *User) calendarSystem() string {
	if u == nil || !slices.Contains(calendarSystemIDs, u.Profile.CalendarSystem) {
		return "gregorian"
	}
	return u.Profile.CalendarSystem
}

// startDayOfWeek is the member's first day of the week, Monday unless set.
func (u *User) startDayOfWeek() int {
	if u == nil || u.Profile.StartDayOfWeek == nil || *u.Profile.StartDayOfWeek < 0 || *u.Profile.StartDayOfWeek > 6 {
		return 1
	}
	return *u.Profile.StartDayOfWeek
}

func (s *Server) setProfileFields(r *http.Request, u *User, set bson.M) error {
	_, err := s.db.Collection("users").UpdateOne(r.Context(), bson.M{"_id": eq(u.ID)}, bson.M{"$set": set})
	return err
}

func (u *User) showsWeekOfYear() bool {
	return u != nil && (u.Profile.ShowWeekOfYear == nil || *u.Profile.ShowWeekOfYear)
}

func isoDateWeek(value string) int {
	at, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return 0
	}
	_, week := at.In(time.Local).ISOWeek()
	return week
}

func (s *Server) methodToggleShowWeekOfYear(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	if len(args) != 0 {
		return nil, &methodError{"Match.Error", "Expected no arguments"}
	}
	show := !u.showsWeekOfYear()
	return show, s.setProfileFields(r, u, bson.M{"profile.showWeekOfYear": show})
}

// methodChangeDateFormat is WeKan's changeDateFormat(format, override).
func (s *Server) methodChangeDateFormat(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	format, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	override := true
	if len(args) > 1 && json.Unmarshal(args[1], &override) != nil {
		return nil, &methodError{"Match.Error", "Expected boolean"}
	}
	if !slices.Contains(dateFormats, format) {
		return nil, &methodError{"Match.Error", "Unknown date format"}
	}
	return true, s.setProfileFields(r, u, bson.M{"profile.dateFormat": format, "profile.dateFormatOverride": override})
}

func (s *Server) methodSetBoardDateFormat(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	var boardID, format string
	var enabled *bool
	if len(args) != 3 || json.Unmarshal(args[0], &boardID) != nil ||
		json.Unmarshal(args[1], &format) != nil || json.Unmarshal(args[2], &enabled) != nil ||
		enabled == nil || !slices.Contains(dateFormats, format) {
		return nil, &methodError{"Match.Error", "Expected a board, date format and boolean"}
	}
	board, err := s.boardFor(r.Context(), boardID, u)
	if err != nil || !board.AdminOf(u.ID) {
		return nil, &methodError{"not-authorized", "Only a board admin can change the board date format"}
	}
	result, err := s.db.Collection("boards").UpdateOne(r.Context(), bson.M{"_id": eq(boardID)},
		bson.M{"$set": bson.M{"dateFormat": format, "dateFormatOverride": *enabled, "modifiedAt": time.Now().UTC()}})
	if err != nil {
		return nil, err
	}
	s.hub.publishFrom(boardID, "board-changed", r.Header.Get("X-Weddy-Page"))
	return result.MatchedCount, nil
}

// methodChangeStartDayOfWeek is WeKan's changeStartDayOfWeek(day): 0 to 6.
func (s *Server) methodChangeStartDayOfWeek(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	var day float64
	if len(args) < 1 || json.Unmarshal(args[0], &day) != nil || day != float64(int(day)) || day < 0 || day > 6 {
		return nil, &methodError{"Match.Error", "Expected a day of the week, 0 to 6"}
	}
	return true, s.setProfileFields(r, u, bson.M{"profile.startDayOfWeek": int(day)})
}

// methodChangeCalendarSystem is WeKan's changeCalendarSystem(id): one of the
// catalog's, else invalid-calendar-system.
func (s *Server) methodChangeCalendarSystem(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	id, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(calendarSystemIDs, id) {
		return nil, &methodError{"invalid-calendar-system", "invalid-calendar-system"}
	}
	return true, s.setProfileFields(r, u, bson.M{"profile.calendarSystem": id})
}

// dateOption is one choice of the Member Settings / Date format pop-over.
type dateOption struct {
	Value, Name string
	Selected    bool
}

// dateSettingsView is what that pop-over shows: the member's format and
// whether it overrides the board's and everyone's, the first day of the week,
// and the calendar - each in the member's language.
type dateSettingsView struct {
	Override  bool
	Formats   []dateOption
	Days      []dateOption
	Calendars []dateOption
	Calendar  string
	StartDay  int
	Format    string
}

func dateSettingsOf(u *User, lang string) dateSettingsView {
	v := dateSettingsView{Override: u.Profile.DateFormatOverride, Calendar: u.calendarSystem(), StartDay: u.startDayOfWeek()}
	format := u.Profile.DateFormat
	if !slices.Contains(dateFormats, format) {
		format = dateFormats[0]
	}
	v.Format = format
	for _, f := range dateFormats {
		v.Formats = append(v.Formats, dateOption{Value: f, Name: f, Selected: f == format})
	}
	for i, d := range weekdayKeys {
		v.Days = append(v.Days, dateOption{Value: strconv.Itoa(i), Name: tr(lang, d), Selected: i == v.StartDay})
	}
	for _, id := range calendarSystemIDs {
		v.Calendars = append(v.Calendars, dateOption{Value: id, Name: tr(lang, calendarSystemKeys[id]), Selected: id == v.Calendar})
	}
	return v
}

// The translation keys, spelled out so cmd/sync-i18n copies them.
var weekdayKeys = []string{msg("sunday"), msg("monday"), msg("tuesday"), msg("wednesday"), msg("thursday"), msg("friday"), msg("saturday")}

var calendarSystemKeys = map[string]string{
	"gregorian": msg("calendar-system-gregorian"), "jalali": msg("calendar-system-jalali"),
	"buddhist": msg("calendar-system-buddhist"), "chinese": msg("calendar-system-chinese"),
	"coptic": msg("calendar-system-coptic"), "dangi": msg("calendar-system-dangi"),
	"ethioaa": msg("calendar-system-ethioaa"), "ethiopic": msg("calendar-system-ethiopic"),
	"hebrew": msg("calendar-system-hebrew"), "indian": msg("calendar-system-indian"),
	"islamic": msg("calendar-system-islamic"), "islamic-civil": msg("calendar-system-islamic-civil"),
	"islamic-rgsa": msg("calendar-system-islamic-rgsa"), "islamic-tbla": msg("calendar-system-islamic-tbla"),
	"islamic-umalqura": msg("calendar-system-islamic-umalqura"), "iso8601": msg("calendar-system-iso8601"),
	"japanese": msg("calendar-system-japanese"), "roc": msg("calendar-system-roc"),
}
