package web

import (
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Teamwork.com's Excel task import template: Node.js WeKan's
// readTeamworkWorkbook (server/lib/teamworkWorkbook.js) and
// parseTeamworkSheet (models/lib/teamworkFormat.js) - task lists as lists,
// "-" prefixed rows as subtasks, people, tags, dates (date cells or text in
// the file's own day order), priority, estimate and status.

func init() { importTextParsers["teamwork"] = parseTeamworkWorkbook }

const maxTeamworkRows = 20000

var (
	teamworkColumnKeys = []string{"tasklist", "task", "description", "assignto", "startdate", "duedate", "priority", "estimatedtime", "tags", "status"}
	teamworkPriority   = []string{"low", "medium", "high"}
	teamworkPrefix     = regexp.MustCompile(`^[-#>]+`)
	teamworkSpaces     = jsRegexp(`\s+`)
	teamworkDecimal    = regexp.MustCompile(`^\d+(\.\d+)?$`)
	teamworkClock      = regexp.MustCompile(`^(\d{1,3}):([0-5]\d)$`)
	teamworkWords      = jsRegexp(`^(?:(\d+(?:\.\d+)?)\s*h(?:ours?|rs?)?)?\s*(?:(\d+)\s*m(?:in(?:ute)?s?)?)?$`)
	teamworkComplete   = regexp.MustCompile(`(?i)^complete$`)
	teamworkActive     = regexp.MustCompile(`(?i)^active$`)
	teamworkSheetName  = jsRegexp(`(?i)^sheet\s*\d*$`)
)

func teamworkKey(text string) string { return teamworkSpaces.ReplaceAllString(jsLower(text), "") }

func teamworkIsDate(v any) bool { _, ok := v.(jsDate); return ok }

// teamworkEstimateMinutes is WeKan's: a time cell, minutes, 01:30 or 1h 15m.
func teamworkEstimateMinutes(v any) (float64, bool) {
	if d, ok := v.(jsDate); ok {
		t := time.Time(d).UTC()
		if t.Year() > 1900 {
			return 0, false
		}
		return float64(t.Hour()*60 + t.Minute()), true
	}
	if f, ok := v.(float64); ok {
		if math.IsInf(f, 0) || math.IsNaN(f) || f < 0 {
			return 0, false
		}
		return jsRound(f), true
	}
	text := jsLower(excelCellText(v))
	switch {
	case text == "":
		return 0, false
	case teamworkDecimal.MatchString(text):
		f, _ := strconv.ParseFloat(text, 64)
		return jsRound(f), true
	}
	if m := teamworkClock.FindStringSubmatch(text); m != nil {
		h, _ := strconv.Atoi(m[1])
		mm, _ := strconv.Atoi(m[2])
		return float64(h*60 + mm), true
	}
	if idx := teamworkWords.FindStringSubmatchIndex(text); idx != nil && (idx[2] >= 0 || idx[4] >= 0) {
		h, mm := 0.0, 0.0
		if idx[2] >= 0 {
			h, _ = strconv.ParseFloat(text[idx[2]:idx[3]], 64)
		}
		if idx[4] >= 0 {
			mm, _ = strconv.ParseFloat(text[idx[4]:idx[5]], 64)
		}
		return jsRound(h*60 + mm), true
	}
	return 0, false
}

func parseTeamworkWorkbook(text string) (jsObject, error) {
	data, err := excelWorkbookBytes(text, "Teamwork.com")
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(text, "\xd0\xcf\x11\xe0") {
		return nil, errors.New("Teamwork.com workbook is an .xls file; save it as .xlsx and import that")
	}
	sheets, err := readXlsxValues(data)
	if err != nil {
		return nil, err
	}
	return parseTeamworkSheet(sheets[0])
}

func parseTeamworkSheet(sheet xlsxValueSheet) (jsObject, error) {
	raw := sheet.Rows
	if len(raw) == 0 {
		return nil, errors.New("Teamwork.com workbook is empty")
	}
	if len(raw) > maxTeamworkRows {
		return nil, errors.New("Teamwork.com workbook has more than " + strconv.Itoa(maxTeamworkRows) + " rows")
	}
	keysOf := func(row []any) []string {
		var out []string
		for _, c := range row {
			out = append(out, teamworkKey(excelCellText(c)))
		}
		return out
	}
	headerAt := -1
	for i, r := range raw {
		if k := keysOf(r); contains(k, "tasklist") && contains(k, "task") {
			headerAt = i
			break
		}
	}
	if headerAt == -1 {
		return nil, errors.New("Teamwork.com workbook needs the Tasklist and Task columns of its import template")
	}
	header := csvHeader(keysOf(raw[headerAt]))
	cell := func(row []any, name string) any {
		i := header.col(teamworkKey(name))
		if i == -1 || i >= len(row) {
			return undefinedValue{}
		}
		return row[i]
	}
	text := func(row []any, name string) string { return excelCellText(cell(row, name)) }
	unsupported := []any{}
	report := func(path, reason string) { unsupported = append(unsupported, importUnsupportedObj(path, reason)) }
	var extra []string
	for _, k := range header {
		if k != "" && !contains(teamworkColumnKeys, k) {
			extra = append(extra, k)
		}
	}
	if len(extra) > 0 {
		var titles []string
		for _, c := range raw[headerAt] {
			if t := excelCellText(c); contains(extra, teamworkKey(t)) {
				titles = append(titles, t)
			}
		}
		report("/row/"+strconv.Itoa(headerAt+1), "columns that are not in Teamwork.com's import template are not imported: "+strings.Join(titles, ", "))
	}
	type bodyRow struct {
		row    []any
		number int
	}
	var body []bodyRow
	for i, r := range raw[headerAt+1:] {
		for _, c := range r {
			if excelCellText(c) != "" {
				body = append(body, bodyRow{r, headerAt + i + 2})
				break
			}
		}
	}
	var dateTexts []string
	for _, b := range body {
		for _, name := range []string{"Start date", "Due date"} {
			if teamworkIsDate(cell(b.row, name)) {
				dateTexts = append(dateTexts, "")
			} else {
				dateTexts = append(dateTexts, text(b.row, name))
			}
		}
	}
	order, conflict, guessed := plannerDateOrder(dateTexts)
	switch {
	case conflict:
		report("/dates", "Teamwork.com dates mix day/month and month/day order; read as month/day")
	case guessed:
		report("/dates", "Teamwork.com dates could be day/month or month/day; read as month/day")
	}
	var columns []string
	tasks := []any{}
	var stack []jsObject // the open task at each depth, stack[0] top-level
	list := ""
	for _, b := range body {
		row, path := b.row, "/row/"+strconv.Itoa(b.number)
		name := text(row, "Task")
		prefix := teamworkPrefix.FindString(name)
		title := jsTrim(name[len(prefix):])
		if title == "" {
			report(path, "a Teamwork.com row without a task name is not imported")
			continue
		}
		depth := len(prefix)
		if depth > len(stack) {
			if len(stack) > 0 {
				report(path+"/Task", "a subtask "+strconv.Itoa(depth)+" levels deep under a task "+strconv.Itoa(len(stack)-1)+" levels deep is imported one level below it")
			} else {
				report(path+"/Task", "a subtask without a task above it is imported as a task")
			}
			depth = len(stack)
		}
		stack = stack[:depth]
		var parent jsObject
		if depth > 0 {
			parent = stack[depth-1]
		}
		ownList := text(row, "Tasklist")
		var column string
		if parent != nil {
			column = jsGet(parent, "column_name").(string)
			if ownList != "" && ownList != column {
				report(path+"/Tasklist", `a subtask stays in its parent's task list "`+column+`", not "`+ownList+`"`)
			}
		} else {
			list = jsOr(ownList, list, "Tasks").(string)
			column = list
		}
		columns = addOnce(columns, column)
		date := func(columnName string) string {
			value := cell(row, columnName)
			if d, ok := value.(jsDate); ok {
				t := time.Time(d).UTC()
				return jsISO(time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC))
			}
			valueText := excelCellText(value)
			parsed, ok := plannerDate(valueText, order)
			if valueText != "" && !ok {
				report(path+"/"+columnName, `Teamwork.com date "`+valueText+`" is not a calendar date`)
			}
			return parsed
		}
		custom := jsObject{}
		if p := text(row, "Priority"); p != "" {
			if contains(teamworkPriority, jsLower(p)) {
				l := jsLower(p)
				custom.set("Priority", strings.ToUpper(l[:1])+l[1:])
			} else {
				report(path+"/Priority", `Teamwork.com priority "`+p+`" is not low, medium or high`)
			}
		}
		estimate := cell(row, "Estimated time")
		if et := excelCellText(estimate); et != "" {
			minutes, ok := teamworkEstimateMinutes(estimate)
			if !ok {
				report(path+"/Estimated time", `Teamwork.com estimated time "`+et+`" is not 25, 01:30, 1h 15m, 1h or 2 hours`)
			} else if minutes > 0 {
				custom.set("Estimated time (hours)", jsRound(minutes/60*100)/100)
			}
		}
		status := text(row, "Status")
		complete := teamworkComplete.MatchString(status)
		if status != "" && !complete && !teamworkActive.MatchString(status) {
			report(path+"/Status", `Teamwork.com status "`+status+`" is not Active or Complete`)
		}
		if complete {
			custom.set("Complete", true)
		}
		people := splitTrimmed(text(row, "Assign to"), ",")
		due := date("Due date")
		started := date("Start date")
		t := jsObject{}
		t.set("title", title)
		t.set("description", text(row, "Description"))
		t.set("column_name", column)
		t.set("swimlane_name", "Default")
		t.set("tags", splitTrimmed(text(row, "Tags"), ","))
		t.set("ref", "teamwork"+path)
		if parent != nil {
			t.set("parent_ref", jsGet(parent, "ref"))
		}
		if len(people) > 0 {
			t.set("owner_username", people[0])
		}
		if len(people) > 1 {
			t.set("assignees", people[1:])
		}
		if started != "" {
			t.set("date_started", started)
		}
		if due != "" {
			t.set("date_due", due)
		}
		if complete && due != "" {
			t.set("due_complete", true)
		}
		if len(custom) > 0 {
			t.set("custom_fields", custom)
		}
		tasks = append(tasks, t)
		stack = append(stack, t)
	}
	name := "Imported Teamwork.com tasks"
	if s := jsTrim(sheet.Name); s != "" && !teamworkSheetName.MatchString(s) {
		name = s
	}
	if len(columns) == 0 {
		columns = []string{"Tasks"}
	}
	return importDoc(name, csvColumns(columns), tasks, []any{}, unsupported), nil
}
