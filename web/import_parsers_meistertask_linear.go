package web

import (
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// MeisterTask's and Linear's CSVs: Node.js WeKan's parseMeisterTaskCsv
// (models/lib/meistertaskCsvFormat.js) and parseLinearCsv
// (models/lib/linearCsvFormat.js), on WeKan's readCsv.

func init() {
	importTextParsers["meistertask"] = parseMeisterTaskCsv
	importTextParsers["linear"] = parseLinearCsv
}

// jsPrototypeKeys are the names a plain object answers from its prototype:
// STATUS[text] is truthy for them, as WeKan's lookup is.
var jsPrototypeKeys = map[string]bool{"constructor": true, "__proto__": true, "toString": true, "toLocaleString": true,
	"valueOf": true, "hasOwnProperty": true, "isPrototypeOf": true, "propertyIsEnumerable": true,
	"__defineGetter__": true, "__defineSetter__": true, "__lookupGetter__": true, "__lookupSetter__": true}

var (
	meistertaskKnown = map[string]bool{"project": true, "section": true, "name": true, "notes": true, "due_date": true,
		"status": true, "tags": true, "id": true, "token": true, "created_at": true, "updated_at": true,
		"status_updated_at": true, "completed_at": true, "assignee": true}
	meistertaskStatus = map[string]string{"1": "open", "2": "completed", "8": "binned", "16": "archived"}
	meistertaskISO    = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})(?:[T ](\d{2}):(\d{2})(?::(\d{2}))?(?:\.\d+)?(Z|[+-]\d{2}:?\d{2})?)?$`)
	meistertaskZone   = regexp.MustCompile(`^([+-]\d{2})(\d{2})$`)
)

// meistertaskDate is WeKan's: a day at UTC midnight when it is a real day,
// a date-time as V8 reads it.
func meistertaskDate(value string) (string, bool) {
	m := meistertaskISO.FindStringSubmatch(jsTrim(value))
	if m == nil {
		return "", false
	}
	n := func(s string) int { v, _ := strconv.Atoi(s); return v }
	if m[4] == "" {
		y := n(m[1])
		if y < 100 {
			y += 1900 // Date.UTC's
		}
		d := time.Date(y, time.Month(n(m[2])), n(m[3]), 0, 0, 0, 0, time.UTC)
		if int(d.Month()) != n(m[2]) || d.Day() != n(m[3]) {
			return "", false
		}
		return jsISO(d), true
	}
	zone := "Z"
	if m[7] != "" {
		zone = meistertaskZone.ReplaceAllString(m[7], "$1:$2")
	}
	sec := m[6]
	if sec == "" {
		sec = "00"
	}
	t, ok := jsDateTime(m[1] + "-" + m[2] + "-" + m[3] + "T" + m[4] + ":" + m[5] + ":" + sec + zone)
	if !ok {
		return "", false
	}
	return jsISO(t), true
}

func csvColumns(columns []string) []any {
	out := []any{}
	for _, c := range columns {
		o := jsObject{}
		o.set("title", c)
		out = append(out, o)
	}
	return out
}

func addOnce(list []string, s string) []string {
	for _, v := range list {
		if v == s {
			return list
		}
	}
	return append(list, s)
}

// splitTrimmed is text.split(sep).map(trim).filter(Boolean).
func splitTrimmed(text, sep string) []any {
	out := []any{}
	for _, p := range strings.Split(text, sep) {
		if p = jsTrim(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseMeisterTaskCsv(text string) (jsObject, error) {
	rows, err := jsReadCsv(text, "MeisterTask", ',')
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, errors.New("MeisterTask CSV is empty")
	}
	header := csvHeader{}
	for _, c := range rows[0] {
		header = append(header, jsLower(jsTrim(c)))
	}
	if header.col("name") == -1 || header.col("section") == -1 {
		return nil, errors.New("MeisterTask CSV needs the name and section columns")
	}
	unsupported := []any{}
	for _, name := range header {
		if name != "" && !meistertaskKnown[name] {
			unsupported = append(unsupported, importUnsupportedObj("/columns/"+name, `MeisterTask column "`+name+`" has no documented format and is not imported`))
		}
	}
	var columns []string
	tasks := []any{}
	project := ""
	for index, cells := range rows[1:] {
		at := "/row/" + strconv.Itoa(index+2)
		title := header.cell(cells, "name")
		if title == "" {
			unsupported = append(unsupported, importUnsupportedObj(at, "a MeisterTask row without a name is not imported"))
			continue
		}
		if project == "" {
			project = header.cell(cells, "project")
		}
		section := header.cell(cells, "section")
		if section == "" {
			section = "No section"
		}
		columns = addOnce(columns, section)
		date := func(name string) string {
			value := header.cell(cells, name)
			iso, ok := meistertaskDate(value)
			if value != "" && !ok {
				unsupported = append(unsupported, importUnsupportedObj(at+"/"+name, `MeisterTask date "`+value+`" is not an ISO 8601 date`))
			}
			return iso
		}
		statusText := header.cell(cells, "status")
		status := "open"
		if statusText != "" {
			status = meistertaskStatus[statusText]
			if status == "" && jsPrototypeKeys[statusText] {
				status = "(prototype)"
			}
		}
		if status == "" {
			unsupported = append(unsupported, importUnsupportedObj(at+"/status", `MeisterTask status "`+statusText+`" is not 1, 2, 8 or 16; imported as open`))
		}
		due := date("due_date")
		created := date("created_at")
		ended := ""
		if status == "completed" {
			if ended = date("completed_at"); ended == "" {
				ended = date("status_updated_at")
			}
		}
		t := jsObject{}
		t.set("title", title)
		t.set("description", header.cell(cells, "notes"))
		t.set("column_name", section)
		t.set("swimlane_name", "Default")
		t.set("tags", splitTrimmed(header.cell(cells, "tags"), ";"))
		if id := header.cell(cells, "id"); id != "" {
			t.set("ref", id)
		}
		if owner := header.cell(cells, "assignee"); owner != "" {
			t.set("owner_username", owner)
		}
		for _, d := range []struct{ key, value string }{{"date_due", due}, {"date_creation", created}, {"date_end", ended}} {
			if d.value != "" {
				t.set(d.key, d.value)
			}
		}
		if status == "binned" || status == "archived" {
			t.set("archived", true)
		}
		tasks = append(tasks, t)
	}
	return importDoc(jsOr(project, "Imported MeisterTask"), csvColumns(columns), tasks, []any{}, unsupported), nil
}

// --- Linear

var (
	linearPriorities = []string{"Urgent", "High", "Medium", "Low"}
	linearRead       = map[string]bool{"ID": true, "Id": true, "Team": true, "Title": true, "Description": true, "Status": true,
		"Estimate": true, "Priority": true, "Project ID": true, "Project": true, "Creator": true, "Assignee": true, "Labels": true,
		"Cycle Number": true, "Cycle Name": true, "Created": true, "Started": true, "Completed": true, "Canceled": true,
		"Archived": true, "Due Date": true, "Parent issue": true}
	linearDay      = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	linearDateTime = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T`)
)

// linearUnformula drops the ' a spreadsheet-safe export put before a
// formula-looking value (WeKan's FORMULA).
func linearUnformula(text string) string {
	if strings.HasPrefix(text, "'") {
		if r, _ := utf8.DecodeRuneInString(text[1:]); strings.ContainsRune(`+-=@∑√∏<>＜＞≤≥＝≠±÷×`, r) {
			return text[1:]
		}
	}
	return text
}

// linearDate is WeKan's: a day at UTC midnight when it is a real day, a
// date-time as V8 reads it.
func linearDate(value string) (string, bool) {
	text := jsTrim(value)
	switch {
	case text == "":
		return "", false
	case linearDay.MatchString(text):
		t, ok := jsDateTime(text + "T00:00:00.000Z")
		if !ok || jsISO(t)[:10] != text {
			return "", false
		}
		return jsISO(t), true
	case !linearDateTime.MatchString(text):
		return "", false
	}
	t, ok := jsDateTime(text)
	if !ok {
		return "", false
	}
	return jsISO(t), true
}

func parseLinearCsv(text string) (jsObject, error) {
	rows, err := jsReadCsv(text, "Linear", ',')
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, errors.New("Linear CSV is empty")
	}
	header := csvHeader{}
	for _, c := range rows[0] {
		header = append(header, jsTrim(c))
	}
	if header.col("Title") == -1 || header.col("Status") == -1 {
		return nil, errors.New("Linear CSV needs the Title and Status columns")
	}
	get := func(cells []string, name string) string {
		i := header.col(name)
		if i == -1 && name == "ID" {
			i = header.col("Id")
		}
		if i == -1 || i >= len(cells) {
			return ""
		}
		return linearUnformula(jsTrim(cells[i]))
	}
	unsupported := []any{}
	for _, name := range header {
		if name != "" && !linearRead[name] {
			unsupported = append(unsupported, importUnsupportedObj("/columns/"+name, `Linear column "`+name+`" has no WeKan place and is not imported`))
		}
	}
	var columns, swimlanes []string
	tasks := []any{}
	for index, cells := range rows[1:] {
		at := "/row/" + strconv.Itoa(index+2)
		title := get(cells, "Title")
		if title == "" {
			unsupported = append(unsupported, importUnsupportedObj(at, "a Linear row without a title is not imported"))
			continue
		}
		status := jsOr(get(cells, "Status"), "No status").(string)
		columns = addOnce(columns, status)
		team := jsOr(get(cells, "Team"), "Default").(string)
		swimlanes = addOnce(swimlanes, team)
		date := func(name string) string {
			value := get(cells, name)
			iso, ok := linearDate(value)
			if value != "" && !ok {
				unsupported = append(unsupported, importUnsupportedObj(at+"/"+name, `Linear date "`+value+`" is not an ISO 8601 date`))
			}
			return iso
		}
		custom := jsObject{}
		priority := get(cells, "Priority")
		known := false
		for _, p := range linearPriorities {
			known = known || p == priority
		}
		if known {
			custom.set("Priority", priority)
		} else if priority != "" && priority != "No priority" {
			unsupported = append(unsupported, importUnsupportedObj(at+"/Priority", `Linear priority "`+priority+`" is not one of `+strings.Join(linearPriorities, ", ")))
		}
		if estimate := get(cells, "Estimate"); estimate != "" {
			if n, ok := jsToNumber(estimate); ok && !math.IsInf(n, 0) {
				custom.set("Estimate", n)
			} else {
				unsupported = append(unsupported, importUnsupportedObj(at+"/Estimate", `Linear estimate "`+estimate+`" is not a number`))
			}
		}
		if p := get(cells, "Project"); p != "" {
			custom.set("Project", p)
		}
		cycle := get(cells, "Cycle Name")
		if cycle == "" && get(cells, "Cycle Number") != "" {
			cycle = "Cycle " + get(cells, "Cycle Number")
		}
		if cycle != "" {
			custom.set("Cycle", cycle)
		}
		canceled := date("Canceled")
		ended := date("Completed")
		if ended == "" {
			ended = canceled
		}
		if canceled != "" {
			custom.set("Canceled", true)
		}
		archived := get(cells, "Archived")
		if archived != "" {
			date("Archived")
		}
		t := jsObject{}
		t.set("title", title)
		t.set("description", get(cells, "Description"))
		t.set("column_name", status)
		t.set("swimlane_name", team)
		t.set("tags", splitTrimmed(get(cells, "Labels"), ","))
		for _, f := range []struct{ key, col string }{{"ref", "ID"}, {"parent_ref", "Parent issue"}, {"owner_username", "Assignee"}, {"requested_by", "Creator"}} {
			if v := get(cells, f.col); v != "" {
				t.set(f.key, v)
			}
		}
		for _, f := range []struct{ key, col string }{{"date_creation", "Created"}, {"date_started", "Started"}, {"date_due", "Due Date"}} {
			if v := date(f.col); v != "" {
				t.set(f.key, v)
			}
		}
		if ended != "" {
			t.set("date_end", ended)
		}
		if archived != "" {
			t.set("archived", true)
		}
		if len(custom) > 0 {
			t.set("custom_fields", custom)
		}
		tasks = append(tasks, t)
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
	out := importDoc("Imported Linear issues", csvColumns(columns), tasks, []any{}, unsupported)
	out.set("swimlanes", lanes)
	return out, nil
}
