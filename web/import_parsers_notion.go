package web

import (
	"archive/zip"
	"bytes"
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// Notion's Markdown & CSV export: Node.js WeKan's readNotionArchive
// (server/lib/notionArchive.js) and parseNotionExport
// (models/lib/notionFormat.js) - each database CSV (its _all.csv) with the
// page files of its rows, Status or a select-like column as the lists, its
// property columns read by name and by what they hold, and a row's page as
// its description. One database CSV alone imports too.

func init() { importTextParsers["notion"] = parseNotionText }

const (
	maxNotionZipBytes      = 64 << 20
	maxNotionCSVBytes      = 32 << 20
	maxNotionPageBytes     = 2 << 20
	maxNotionInflatedBytes = 96 << 20
	maxNotionZipEntries    = 20000
	maxNotionPages         = 20000
	maxNotionDatabases     = 50
)

var (
	notionRange       = jsRegexp(`\s+(?:→|->)\s+`)
	notionIDSuffix    = jsRegexp(`(?i)\s+[0-9a-f]{32}$`)
	notionDirPrefix   = regexp.MustCompile(`^(?s:.)*[\\/]`)
	notionExt         = regexp.MustCompile(`(?i)\.(csv|md)$`)
	notionAllSuffix   = regexp.MustCompile(`(?i)_all$`)
	notionMonths      = []string{"january", "february", "march", "april", "may", "june", "july", "august", "september", "october", "november", "december"}
	notionZone        = jsRegexp(`(?i)\s*\((?:GMT|UTC)(?:([+-])(\d{1,2})(?::?(\d{2}))?)?\)$`)
	notionWordsDate   = jsRegexp(`^([A-Za-z]+)\.?\s+(\d{1,2}),?\s+(\d{4})(?:\s+(\d{1,2}):(\d{2})\s*([AaPp][Mm])?)?$`)
	notionISODate     = regexp.MustCompile(`^(\d{4})[-/](\d{2})[-/](\d{2})(?:[ T](\d{2}):(\d{2}))?$`)
	notionUSDate      = jsRegexp(`^(\d{1,2})/(\d{1,2})/(\d{4})(?:\s+(\d{1,2}):(\d{2})\s*([AaPp][Mm])?)?$`)
	notionEndClock    = jsRegexp(`^(\d{1,2}):(\d{2})\s*([AaPp][Mm])?$`)
	notionTrailClock  = jsRegexp(`\s+\d{1,2}:\d{2}\s*([AaPp][Mm])?$`)
	notionTitle       = regexp.MustCompile(`^(name|title|task|task name|page)$`)
	notionStatus      = regexp.MustCompile(`^status$`)
	notionLabels      = regexp.MustCompile(`^(tags?|labels?|categor(y|ies)|multi-?select)$`)
	notionPeople      = regexp.MustCompile(`^(assignees?|assigned to|owners?|person|people|responsible|members?)$`)
	notionCreator     = regexp.MustCompile(`^created by$`)
	notionDescription = regexp.MustCompile(`^(description|notes?)$`)
	notionSwimlane    = regexp.MustCompile(`^swimlane$`)
	notionStart       = regexp.MustCompile(`^(start|start date|starts|begin|begins)$`)
	notionCreated     = regexp.MustCompile(`^(created|created time|created at|date created)$`)
	notionEdited      = regexp.MustCompile(`^(last edited|last edited time|updated|last updated)$`)
	notionEnded       = regexp.MustCompile(`^(completed|completed on|completed at|done|done on|end|end date|finished|finished on)$`)
	notionDueName     = regexp.MustCompile(`(due|deadline|date)`)
	notionRelation    = jsRegexp(`(?i)\.md\)|(?:^|[\s(])https?://(?:www\.)?notion\.(?:so|site|com)/`)
	notionYesNo       = regexp.MustCompile(`(?i)^(yes|no)$`)
	notionYes         = regexp.MustCompile(`(?i)^yes$`)
	notionDecimal     = regexp.MustCompile(`^-?\d+(\.\d+)?$`)
	notionCommaSpace  = jsRegexp(`,\s`)
	notionNewline     = regexp.MustCompile(`[\r\n]`)
	notionHeading     = jsRegexp(`^#\s+`)
	notionProp        = jsRegexp(`^([^:\n]+):\s?(` + jsDotU + `*)$`)
	notionTrailSpace  = jsRegexp(`\s+$`)
	notionNotWord     = regexp.MustCompile(`[^\p{L}\p{N}]+`)
	notionLinkTarget  = regexp.MustCompile(`^[^)\s]+\)`)
	notionWebTarget   = regexp.MustCompile(`(?i)^(https?:|mailto:|#)`)
	notionUnsafe      = map[string]bool{"__proto__": true, "constructor": true, "prototype": true}
	notionMacOS       = regexp.MustCompile(`(^|/)__MACOSX/`)
)

func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

func notionLower(s string) string { return jsLower(jsTrim(s)) }

// stripNotionId is WeKan's: "Tasks 1a2b...c6_all.csv" -> "Tasks".
func stripNotionID(name string) string {
	text := jsTrim(notionDirPrefix.ReplaceAllString(name, ""))
	text = notionAllSuffix.ReplaceAllString(notionExt.ReplaceAllString(text, ""), "")
	return jsTrim(notionIDSuffix.ReplaceAllString(text, ""))
}

func notionUTC(y, m, d, h, min int) (time.Time, bool) {
	yy := y
	if yy < 100 {
		yy += 1900 // Date.UTC's
	}
	t := time.Date(yy, time.Month(m), d, h, min, 0, 0, time.UTC)
	if int(t.Month()) != m || t.Day() != d {
		return time.Time{}, false
	}
	return t, true
}

func notionClock(hours, minutes, meridiem string) (int, int, bool) {
	h, _ := strconv.Atoi(hours)
	m, _ := strconv.Atoi(jsOr(minutes, "0").(string))
	if meridiem != "" {
		if h < 1 || h > 12 {
			return 0, 0, false
		}
		h %= 12
		if strings.EqualFold(meridiem[:1], "p") {
			h += 12
		}
	}
	if h > 23 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

// notionDate is WeKan's: one date cell as ISO, at UTC unless it says its zone.
func notionDate(value string) (string, bool) {
	text := jsTrim(value)
	if text == "" {
		return "", false
	}
	offset := 0
	if idx := notionZone.FindStringSubmatchIndex(text); idx != nil {
		if idx[2] >= 0 {
			h, _ := strconv.Atoi(text[idx[4]:idx[5]])
			m := 0
			if idx[6] >= 0 {
				m, _ = strconv.Atoi(text[idx[6]:idx[7]])
			}
			offset = h*60 + m
			if text[idx[2]:idx[3]] == "-" {
				offset = -offset
			}
		}
		text = jsTrim(text[:idx[0]])
	}
	n := func(s string) int { v, _ := strconv.Atoi(s); return v }
	var date time.Time
	ok := false
	if m := notionWordsDate.FindStringSubmatch(text); m != nil {
		month := -1
		want := strings.ToLower(m[1])
		for i, name := range notionMonths {
			if name == want || len(m[1]) >= 3 && strings.HasPrefix(name, want) {
				month = i
				break
			}
		}
		if month == -1 {
			return "", false
		}
		h, mi := 0, 0
		if m[4] != "" {
			var cok bool
			if h, mi, cok = notionClock(m[4], m[5], m[6]); !cok {
				return "", false
			}
		}
		date, ok = notionUTC(n(m[3]), month+1, n(m[2]), h, mi)
	} else if m := notionISODate.FindStringSubmatch(text); m != nil {
		date, ok = notionUTC(n(m[1]), n(m[2]), n(m[3]), n(m[4]), n(m[5]))
	} else if m := notionUSDate.FindStringSubmatch(text); m != nil {
		h, mi := 0, 0
		if m[4] != "" {
			var cok bool
			if h, mi, cok = notionClock(m[4], m[5], m[6]); !cok {
				return "", false
			}
		}
		date, ok = notionUTC(n(m[3]), n(m[1]), n(m[2]), h, mi)
	}
	if !ok {
		return "", false
	}
	return jsISO(date.Add(-time.Duration(offset) * time.Minute)), true
}

// notionDateRange is WeKan's: a single date or a range, start and end.
func notionDateRange(value string) (start, end string, ok bool) {
	text := jsTrim(value)
	if text == "" {
		return "", "", false
	}
	parts := notionRange.Split(text, -1)
	switch len(parts) {
	case 1:
		s, ok := notionDate(text)
		return s, "", ok
	case 2:
		s, sok := notionDate(parts[0])
		e, eok := notionDate(parts[1])
		if !eok && sok {
			// "October 8, 2026 3:00 PM → 5:00 PM": the end is a time that day.
			if t := notionEndClock.FindStringSubmatch(jsTrim(parts[1])); t != nil {
				if _, _, cok := notionClock(t[1], t[2], t[3]); cok {
					e, eok = notionDate(notionTrailClock.ReplaceAllString(jsTrim(parts[0]), "") + " " + t[0])
				}
			}
		}
		if sok && eok {
			return s, e, true
		}
	}
	return "", "", false
}

func notionClassify(name string, values []string) string {
	var filled []string
	for _, v := range values {
		if jsTrim(v) != "" {
			filled = append(filled, v)
		}
	}
	key := notionLower(name)
	every := func(f func(string) bool) bool {
		for _, v := range filled {
			if !f(v) {
				return false
			}
		}
		return len(filled) > 0
	}
	some := func(f func(string) bool) bool {
		for _, v := range filled {
			if f(v) {
				return true
			}
		}
		return false
	}
	switch {
	case some(notionRelation.MatchString):
		return "relation"
	case every(func(v string) bool { _, _, ok := notionDateRange(v); return ok }):
		return "date"
	case every(func(v string) bool { return notionYesNo.MatchString(jsTrim(v)) }):
		return "checkbox"
	case every(func(v string) bool { return notionDecimal.MatchString(jsTrim(v)) }):
		return "number"
	case notionLabels.MatchString(key):
		return "labels"
	case notionPeople.MatchString(key):
		return "people"
	case notionCreator.MatchString(key):
		return "creator"
	case notionDescription.MatchString(key):
		return "description"
	case notionSwimlane.MatchString(key):
		return "swimlane"
	}
	return "text"
}

func notionLooksLikeSelect(values []string) bool {
	var filled []string
	distinct := map[string]bool{}
	for _, v := range values {
		if v = jsTrim(v); v != "" {
			if utf16Len(v) > 60 || notionNewline.MatchString(v) || notionCommaSpace.MatchString(v) {
				return false
			}
			filled = append(filled, v)
			distinct[v] = true
		}
	}
	if len(filled) == 0 {
		return false
	}
	limit := math.Max(1, math.Min(20, math.Ceil(float64(len(filled))*0.75)))
	return float64(len(distinct)) <= limit
}

// notionPageBody is WeKan's: a row's page without its "# Title" and its
// "Property: value" lines.
func notionPageBody(text string, header []string) (heading, body string) {
	names := map[string]bool{}
	for _, h := range header {
		names[notionLower(h)] = true
	}
	text = strings.ReplaceAll(strings.ReplaceAll(strings.TrimPrefix(text, "\ufeff"), "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(text, "\n")
	i := 0
	skipBlank := func() {
		for i < len(lines) && jsTrim(lines[i]) == "" {
			i++
		}
	}
	skipBlank()
	if i < len(lines) && notionHeading.MatchString(lines[i]) {
		heading = jsTrim(notionHeading.ReplaceAllString(lines[i], ""))
		i++
	}
	skipBlank()
	for i < len(lines) {
		m := notionProp.FindStringSubmatch(lines[i])
		if m == nil || utf16Len(m[1]) > 200 || !names[notionLower(m[1])] {
			break
		}
		i++
	}
	skipBlank()
	return heading, notionTrailSpace.ReplaceAllString(strings.Join(lines[i:], "\n"), "")
}

func notionNormalTitle(v string) string {
	return notionNotWord.ReplaceAllString(notionLower(v), "")
}

// notionLinksInside is WeKan's /\]\((?!https?:|mailto:|#)[^)\s]+\)/i: a
// Markdown link to a file or page inside the export.
func notionLinksInside(body string) bool {
	for i := 0; i+1 < len(body); i++ {
		if body[i] != ']' || body[i+1] != '(' {
			continue
		}
		rest := body[i+2:]
		if !notionWebTarget.MatchString(rest) && notionLinkTarget.MatchString(rest) {
			return true
		}
	}
	return false
}

type notionPage struct {
	name, text string
}

type notionDatabase struct {
	name, csv string
	pages     []notionPage
}

type notionSource struct {
	databases             []notionDatabase
	assets, nested, other int
	skipped               []any
	fromArchive           bool
}

type notionState struct {
	unsupported, warnings []any
	columns               []string
}

func notionParseDatabase(db notionDatabase, index int, swimlane string, st *notionState) ([]any, error) {
	at := "/databases/" + strconv.Itoa(index)
	label := "the CSV"
	if db.name != "" {
		label = `"` + db.name + `"`
	}
	if utf16Len(db.csv) > maxNotionCSVBytes {
		return nil, errors.New("Notion CSV is larger than " + strconv.Itoa(maxNotionCSVBytes) + " characters")
	}
	rows, err := jsReadCsv(db.csv, "Notion", ',')
	if err != nil {
		return nil, err
	}
	report := func(path, reason string) { st.unsupported = append(st.unsupported, importUnsupportedObj(path, reason)) }
	warn := func(path, reason string) { st.warnings = append(st.warnings, importUnsupportedObj(path, reason)) }
	if len(rows) == 0 {
		report(at, "Notion database "+label+" is empty")
		return nil, nil
	}
	var header []string
	for _, c := range rows[0] {
		header = append(header, jsTrim(strings.TrimPrefix(c, "\ufeff")))
	}
	body := rows[1:]
	values := func(c int) []string {
		out := make([]string, len(body))
		for i, cells := range body {
			if c < len(cells) {
				out[i] = cells[c]
			}
		}
		return out
	}
	titleCol := -1
	for i, name := range header {
		if notionTitle.MatchString(notionLower(name)) {
			titleCol = i
			break
		}
	}
	if titleCol == -1 {
		titleCol = 0
	}
	kinds := make([]string, len(header))
	for c, name := range header {
		if c == titleCol {
			kinds[c] = "title"
		} else {
			kinds[c] = notionClassify(name, values(c))
		}
	}
	// The list column: Status by name, else the first select-looking text column.
	groupCol := -1
	for c, name := range header {
		if c != titleCol && notionStatus.MatchString(notionLower(name)) {
			groupCol = c
			break
		}
	}
	if groupCol == -1 {
		for c, name := range header {
			if kinds[c] == "text" && !notionUnsafe[name] && notionLooksLikeSelect(values(c)) {
				groupCol = c
				break
			}
		}
		if groupCol != -1 {
			warn(at+"/columns/"+header[groupCol], "Notion database "+label+` has no Status column; "`+header[groupCol]+`" looks like a select and its values became the lists`)
		} else {
			warn(at+"/columns", "Notion database "+label+` has no Status column and no select-like column; every card is in the list "No Status"`)
		}
	}
	if groupCol != -1 {
		kinds[groupCol] = "group"
	}
	// Dates: start, creation, end by name, the first other one is the due date.
	for c, name := range header {
		if kinds[c] != "date" {
			continue
		}
		key := notionLower(name)
		switch {
		case notionStart.MatchString(key):
			kinds[c] = "start"
		case notionCreated.MatchString(key):
			kinds[c] = "created"
		case notionEdited.MatchString(key):
			kinds[c] = "edited"
		case notionEnded.MatchString(key):
			kinds[c] = "end"
		}
	}
	dueCol := -1
	for c, name := range header {
		if kinds[c] == "date" && notionDueName.MatchString(notionLower(name)) {
			dueCol = c
			break
		}
	}
	if dueCol == -1 {
		for c := range header {
			if kinds[c] == "date" {
				dueCol = c
				break
			}
		}
	}
	if dueCol != -1 {
		kinds[dueCol] = "due"
	}
	for c, name := range header {
		if kinds[c] == "date" {
			dueName := "undefined"
			if dueCol != -1 {
				dueName = header[dueCol]
			}
			warn(at+"/columns/"+name, `Notion date column "`+name+`" is not the due date ("`+dueName+`" is); it is kept as a text field`)
			kinds[c] = "text"
		}
	}
	// Only the first person column is the owner and assignees.
	peopleCol := -1
	for c := range header {
		if kinds[c] == "people" {
			peopleCol = c
			break
		}
	}
	for c, name := range header {
		// A property name is the user's own text; these would reach an object's prototype.
		if notionUnsafe[name] && kinds[c] != "title" && kinds[c] != "group" {
			report(at+"/columns/"+strconv.Itoa(c+1), `Notion column "`+name+`" is not imported: the name is reserved`)
			kinds[c] = "skip"
			continue
		}
		if kinds[c] == "people" && c != peopleCol {
			kinds[c] = "text"
		}
		if kinds[c] == "relation" {
			report(at+"/columns/"+name, `Notion relation column "`+name+`" links to other pages; relations are not imported`)
		}
		if kinds[c] == "edited" {
			report(at+"/columns/"+name, `Notion column "`+name+`" (last edited time) has no WeKan place and is not imported`)
		}
	}
	type parsedPage struct {
		name, heading, body string
		used                bool
	}
	pages := make([]*parsedPage, len(db.pages))
	for i, p := range db.pages {
		h, b := notionPageBody(p.text, header)
		pages[i] = &parsedPage{name: stripNotionID(p.name), heading: h, body: b}
	}
	findPage := func(title string) *parsedPage {
		want := notionNormalTitle(title)
		if want == "" {
			return nil
		}
		for _, match := range []func(p *parsedPage) bool{
			func(p *parsedPage) bool { return notionNormalTitle(p.heading) == want },
			func(p *parsedPage) bool { return notionNormalTitle(p.name) == want },
			// Notion shortens long file names; a prefix of the title is the same page.
			func(p *parsedPage) bool {
				n := notionNormalTitle(p.name)
				return p.heading == "" && utf16Len(n) >= 20 && strings.HasPrefix(want, n)
			},
		} {
			for _, p := range pages {
				if !p.used && match(p) {
					p.used = true
					return p
				}
			}
		}
		return nil
	}
	tasks := []any{}
	for r, cells := range body {
		rowAt := at + "/row/" + strconv.Itoa(r+2)
		get := func(c int) string {
			if c < 0 || c >= len(cells) {
				return ""
			}
			return jsTrim(cells[c])
		}
		title := get(titleCol)
		if title == "" {
			report(rowAt, "a Notion row without a title is not imported")
			continue
		}
		status := "No Status"
		if groupCol != -1 {
			status = jsOr(get(groupCol), "No Status").(string)
		}
		st.columns = addOnce(st.columns, status)
		t := jsObject{}
		t.set("title", title)
		t.set("column_name", status)
		t.set("swimlane_name", swimlane)
		tags := []any{}
		t.set("tags", tags)
		custom := jsObject{}
		var description []string
		first := func(list []any) any {
			if len(list) > 0 {
				return list[0]
			}
			return undefinedValue{}
		}
		for c, name := range header {
			value := get(c)
			if value == "" {
				continue
			}
			switch kinds[c] {
			case "labels":
				tags = append(tags, splitTrimmed(value, ",")...)
				t.set("tags", tags)
			case "people":
				people := splitTrimmed(value, ",")
				t.set("owner_username", first(people))
				if len(people) > 1 {
					t.set("assignees", people[1:])
				}
			case "creator":
				t.set("requested_by", first(splitTrimmed(value, ",")))
			case "description":
				description = append(description, value)
			case "swimlane":
				t.set("swimlane_name", value)
			case "start":
				s, _, _ := notionDateRange(value)
				t.set("date_started", s)
			case "created":
				s, _, _ := notionDateRange(value)
				t.set("date_creation", s)
			case "end":
				s, e, _ := notionDateRange(value)
				t.set("date_end", jsOr(e, s))
			case "due":
				s, e, _ := notionDateRange(value)
				if e != "" {
					if !jsTruthy(jsGet(t, "date_started")) {
						t.set("date_started", s)
					}
					t.set("date_due", e)
				} else {
					t.set("date_due", s)
				}
			case "checkbox":
				custom.set(name, notionYes.MatchString(value))
			case "number":
				f, _ := strconv.ParseFloat(value, 64)
				custom.set(name, f)
			case "text":
				custom.set(name, value)
			}
		}
		if page := findPage(title); page != nil {
			if page.body != "" {
				description = append(description, page.body)
			}
			if notionLinksInside(page.body) {
				report(rowAt+"/page", `the page of "`+title+`" links to files or pages inside the export; they are not imported and the links are kept as text`)
			}
		}
		if len(description) > 0 {
			t.set("description", strings.Join(description, "\n\n"))
		}
		if len(custom) > 0 {
			t.set("custom_fields", jsOrder(custom))
		}
		if len(tags) == 0 {
			t.del("tags")
		}
		tasks = append(tasks, t)
	}
	unmatched := 0
	for _, p := range pages {
		if !p.used {
			unmatched++
		}
	}
	if unmatched > 0 {
		report(at+"/pages", strconv.Itoa(unmatched)+" page file(s) of Notion database "+label+" match no row and are not imported")
	}
	return tasks, nil
}

func parseNotionExport(src notionSource) (jsObject, error) {
	var databases []notionDatabase
	for _, db := range src.databases {
		if jsTrim(db.csv) != "" {
			databases = append(databases, db)
		}
	}
	if len(databases) == 0 {
		return nil, errors.New("Notion export has no database CSV")
	}
	if len(databases) > maxNotionDatabases {
		return nil, errors.New("Notion export has more than " + strconv.Itoa(maxNotionDatabases) + " databases")
	}
	st := &notionState{unsupported: []any{}, warnings: []any{}}
	several := len(databases) > 1
	tasks := []any{}
	var swimlanes []string
	for i, db := range databases {
		name := stripNotionID(db.name)
		if name == "" && several {
			name = "Database " + strconv.Itoa(i+1)
		}
		lane := "Default"
		if several {
			lane = name
		}
		db.name = name
		part, err := notionParseDatabase(db, i, lane, st)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, part...)
	}
	for _, t := range tasks {
		swimlanes = addOnce(swimlanes, jsGet(t, "swimlane_name").(string))
	}
	if src.assets > 0 {
		st.unsupported = append(st.unsupported, importUnsupportedObj("/files", strconv.Itoa(src.assets)+" image(s) or other file(s) in the Notion export are not imported"))
	}
	if src.nested > 0 {
		st.unsupported = append(st.unsupported, importUnsupportedObj("/subpages", strconv.Itoa(src.nested)+" nested subpage file(s) in the Notion export are not imported"))
	}
	if src.other > 0 {
		st.unsupported = append(st.unsupported, importUnsupportedObj("/pages", strconv.Itoa(src.other)+" Markdown page(s) outside any database are not imported"))
	}
	st.unsupported = append(st.unsupported, src.skipped...)
	name := "Imported Notion database"
	if first := stripNotionID(databases[0].name); !several && first != "" {
		name = first
	}
	if len(swimlanes) == 0 {
		swimlanes = []string{"Default"}
	}
	lanes := []any{}
	for _, s := range swimlanes {
		o := jsObject{}
		o.set("name", s)
		lanes = append(lanes, o)
	}
	out := importDoc(name, csvColumns(st.columns), tasks, st.warnings, st.unsupported)
	out.set("swimlanes", lanes)
	return out, nil
}

// --- server/lib/notionArchive.js

func notionBase(name string) string { return name[strings.LastIndex(name, "/")+1:] }

func notionDir(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[:i]
	}
	return ""
}

var (
	notionCSVExt  = regexp.MustCompile(`(?i)\.csv$`)
	notionAllCSV  = regexp.MustCompile(`(?i)_all\.csv$`)
	notionZipExt  = regexp.MustCompile(`(?i)\.zip$`)
	notionMdExt   = regexp.MustCompile(`(?i)\.md$`)
	notionKnownFx = regexp.MustCompile(`(?i)\.(csv|md|zip)$`)
)

func readNotionArchive(raw []byte, budget *int, depth int) (notionSource, error) {
	var src notionSource
	if len(raw) == 0 {
		return src, errors.New("Notion export is empty")
	}
	if len(raw) > maxNotionZipBytes {
		return src, errors.New("Notion export is larger than " + strconv.Itoa(maxNotionZipBytes) + " bytes")
	}
	if !bytes.HasPrefix(raw, []byte("PK\x03\x04")) && !bytes.HasPrefix(raw, []byte("PK\x05\x06")) {
		return src, errors.New("Notion export is not a .zip file")
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return src, errors.New("Notion export is not a readable .zip file")
	}
	var entries []*zip.File
	for _, f := range zr.File {
		if !f.FileInfo().IsDir() {
			entries = append(entries, f)
		}
	}
	if len(entries) > maxNotionZipEntries {
		return src, errors.New("Notion export has more than " + strconv.Itoa(maxNotionZipEntries) + " files")
	}
	read := func(f *zip.File, max int) ([]byte, error) {
		if f.UncompressedSize64 > uint64(max) {
			return nil, errors.New("Notion " + notionBase(f.Name) + " is too large")
		}
		limit := max
		if *budget < limit {
			limit = *budget
		}
		b, err := readBounded(f, int64(limit))
		if errors.Is(err, errTooLarge) {
			return nil, errors.New("Notion " + notionBase(f.Name) + " is too large")
		}
		if err != nil {
			return nil, errors.New("Notion " + notionBase(f.Name) + " could not be read")
		}
		*budget -= len(b)
		return b, nil
	}
	var useful []*zip.File
	for _, f := range entries {
		if !notionMacOS.MatchString(f.Name) && !strings.HasPrefix(notionBase(f.Name), "._") {
			useful = append(useful, f)
		}
	}
	var csvs []*zip.File
	for _, f := range useful {
		if notionCSVExt.MatchString(f.Name) {
			csvs = append(csvs, f)
		}
	}
	if len(csvs) == 0 {
		var zips []*zip.File
		for _, f := range useful {
			if notionZipExt.MatchString(f.Name) {
				zips = append(zips, f)
			}
		}
		if depth == 0 && len(zips) > 0 && len(zips) == len(useful) {
			var merged notionSource
			for _, inner := range zips {
				b, err := read(inner, maxNotionZipBytes)
				if err != nil {
					return src, err
				}
				part, err := readNotionArchive(b, budget, 1)
				if err != nil {
					if strings.Contains(err.Error(), "no database CSV") {
						continue
					}
					return src, err
				}
				merged.databases = append(merged.databases, part.databases...)
				merged.assets += part.assets
				merged.nested += part.nested
				merged.other += part.other
				merged.skipped = append(merged.skipped, part.skipped...)
			}
			if len(merged.databases) == 0 {
				return src, errors.New("Notion export has no database CSV")
			}
			merged.fromArchive = true
			return merged, nil
		}
		return src, errors.New("Notion export has no database CSV")
	}
	// A database is its _all.csv when it has one: a view CSV has some rows only.
	var bases []string
	byBase := map[string]*zip.File{}
	for _, f := range csvs {
		base := notionAllSuffix.ReplaceAllString(notionCSVExt.ReplaceAllString(f.Name, ""), "")
		if _, has := byBase[base]; !has {
			bases = append(bases, base)
			byBase[base] = f
		} else if notionAllCSV.MatchString(f.Name) {
			byBase[base] = f
		}
	}
	if len(bases) > maxNotionDatabases {
		return src, errors.New("Notion export has more than " + strconv.Itoa(maxNotionDatabases) + " databases")
	}
	var mds []*zip.File
	for _, f := range useful {
		if notionMdExt.MatchString(f.Name) {
			mds = append(mds, f)
		}
	}
	claimed := map[string]bool{}
	pageCount := 0
	for _, base := range bases {
		entry := byBase[base]
		csv, err := read(entry, maxNotionCSVBytes)
		if err != nil {
			return src, err
		}
		var own []*zip.File
		for _, md := range mds {
			if notionDir(md.Name) == base {
				own = append(own, md)
			}
		}
		if len(own) == 0 {
			for _, md := range mds {
				if notionDir(md.Name) == notionDir(entry.Name) && !claimed[md.Name] {
					own = append(own, md)
				}
			}
		}
		for _, md := range mds {
			if strings.HasPrefix(md.Name, base+"/") && notionDir(md.Name) != base {
				src.nested++
			}
		}
		var pages []notionPage
		for _, md := range own {
			claimed[md.Name] = true
			if pageCount++; pageCount > maxNotionPages {
				return src, errors.New("Notion export has more than " + strconv.Itoa(maxNotionPages) + " pages")
			}
			b, err := read(md, maxNotionPageBytes)
			if err != nil {
				return src, err
			}
			pages = append(pages, notionPage{name: notionBase(md.Name), text: strings.ToValidUTF8(string(b), "�")})
		}
		src.databases = append(src.databases, notionDatabase{name: notionBase(entry.Name), csv: strings.ToValidUTF8(string(csv), "�"), pages: pages})
	}
	for _, md := range mds {
		if claimed[md.Name] {
			continue
		}
		inside := false
		for _, base := range bases {
			inside = inside || strings.HasPrefix(md.Name, base+"/")
		}
		if !inside {
			src.other++
		}
	}
	for _, f := range useful {
		if !notionKnownFx.MatchString(f.Name) {
			src.assets++
		}
	}
	if views := len(csvs) - len(bases); views > 0 {
		src.skipped = append(src.skipped, importUnsupportedObj("/views", strconv.Itoa(views)+" view CSV(s) were left out for the _all.csv of the same database, which has every row"))
	}
	src.fromArchive = true
	return src, nil
}

// parseNotionText is the import page's and importBoard's reading: a .zip's
// bytes as the export, else the text of one database CSV.
func parseNotionText(text string) (jsObject, error) {
	if strings.HasPrefix(text, "PK\x03\x04") || strings.HasPrefix(text, "PK\x05\x06") {
		budget := maxNotionInflatedBytes
		src, err := readNotionArchive([]byte(text), &budget, 0)
		if err != nil {
			return nil, err
		}
		return parseNotionExport(src)
	}
	if jsTrim(text) == "" {
		return nil, errors.New("Notion CSV is empty")
	}
	return parseNotionExport(notionSource{databases: []notionDatabase{{name: "", csv: text}}})
}
