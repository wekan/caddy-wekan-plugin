package web

import (
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Wrike's Excel import template, read back: Node.js WeKan's readWrikeWorkbook
// (server/lib/wrikeWorkbook.js) and parseWrikeRows (models/lib/wrikeFormat.js)
// - folders as swimlanes, statuses (custom ones first) as lists, parents and
// dependencies by Key, people, dates, priority, duration and other columns.

func init() { importTextParsers["wrike"] = parseWrikeWorkbook }

const (
	maxWrikeRows = 65000
	maxWrikeKey  = 999999
)

var (
	wrikeRoles = map[string]string{"key": "key", "parent task": "parent", "title": "title", "status": "status",
		"custom status": "customStatus", "priority": "priority", "assigned to": "people", "start date": "start",
		"duration": "duration", "end date": "end", "depends on": "depends", "start date constraint": "constraint",
		"description": "description", "default task workflow": "defaultWorkflow", "default project workflow": "defaultWorkflow",
		"workflow": "workflow"}
	wrikeStatuses   = []string{"Active", "Completed", "Deferred", "Cancelled"}
	wrikeISODay     = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})(?:[T ]00:00(?::00(?:\.000)?)?Z?)?$`)
	wrikeSerial     = regexp.MustCompile(`^\d{1,7}$`)
	wrikePeopleRe   = jsRegexp(`([^,<>]*)(?:<([^<>]*)>)?\s*(?:,|$)`)
	wrikeDependency = jsRegexp(`(?i)^(\d+)\s*(FS|SS|FF|SF)?$`)
	wrikeFolder     = regexp.MustCompile(`^/(?s:.)*/$`)
	wrikeDigits     = regexp.MustCompile(`^\d+$`)
	wrikeSplitDeps  = regexp.MustCompile(`[,;]`)
)

// wrikeRoleOf is WeKan's ROLES[title]: a prototype name answers too, as
// none of its roles.
func wrikeRoleOf(title string) string {
	k := jsLower(jsTrim(title))
	if r, ok := wrikeRoles[k]; ok {
		return r
	}
	if jsPrototypeKeys[k] {
		return "(prototype)"
	}
	return ""
}

// wrikeCellText is the module's cellText: a Date as its UTC day.
func wrikeCellText(v any) string {
	if d, ok := v.(jsDate); ok {
		t := time.Time(d).UTC()
		return strconv.Itoa(t.Year()) + "-" + t.Format("01-02")
	}
	if o, ok := v.(jsObject); ok {
		if _, rich := jsGet(o, "richText").([]any); !rich {
			if t := jsGetOrUndef(o, "text"); !isUndef(t) {
				return wrikeCellText(t)
			}
			if r := jsGetOrUndef(o, "result"); !isUndef(r) {
				return wrikeCellText(r)
			}
		}
	}
	return excelCellText(v)
}

func wrikeDate(text string) (string, bool) {
	value := jsTrim(text)
	n := func(s string) int { v, _ := strconv.Atoi(s); return v }
	if m := wrikeISODay.FindStringSubmatch(value); m != nil {
		y := n(m[1])
		if y < 100 {
			y += 1900 // Date.UTC's
		}
		d := time.Date(y, time.Month(n(m[2])), n(m[3]), 0, 0, 0, 0, time.UTC)
		if d.Day() != n(m[3]) || int(d.Month()) != n(m[2]) {
			return "", false
		}
		return jsISO(d), true
	}
	if wrikeSerial.MatchString(value) {
		if v := n(value); v > 0 && v < 2958466 {
			ms := math.Floor((float64(v)-25569)*86400000 + 0.5)
			return jsISO(time.UnixMilli(int64(ms))), true
		}
	}
	return "", false
}

// wrikePeople is WeKan's: "Name <email>, ..." as each name, or its email.
func wrikePeople(text string) []any {
	people := []any{}
	for _, m := range wrikePeopleRe.FindAllStringSubmatch(text, -1) {
		if m[0] == "" {
			break
		}
		name, email := jsTrim(m[1]), jsTrim(m[2])
		if name != "" || email != "" {
			people = append(people, jsOr(name, email))
		}
	}
	return people
}

func parseWrikeWorkbook(text string) (jsObject, error) {
	data, err := excelWorkbookBytes(text, "Wrike")
	if err != nil {
		return nil, err
	}
	sheets, err := readXlsxValues(data)
	if err != nil {
		return nil, err
	}
	sheet := sheets[0]
	for _, s := range sheets {
		if s.Name == "Tasks" {
			sheet = s
			break
		}
	}
	return parseWrikeRows(sheet.Rows)
}

func parseWrikeRows(rows [][]any) (jsObject, error) {
	if len(rows) == 0 {
		return nil, errors.New("Wrike workbook is empty")
	}
	if len(rows) > maxWrikeRows {
		return nil, errors.New("Wrike workbook has more than " + strconv.Itoa(maxWrikeRows) + " rows")
	}
	cells := make([][]string, len(rows))
	for i, r := range rows {
		cells[i] = []string{}
		for _, c := range r {
			cells[i] = append(cells[i], wrikeCellText(c))
		}
	}
	hasValue := func(row []string) bool {
		for _, c := range row {
			if c != "" {
				return true
			}
		}
		return false
	}
	headerAt := -1
	for i, r := range cells {
		for _, c := range r {
			if wrikeRoleOf(c) == "title" {
				headerAt = i
				break
			}
		}
		if headerAt != -1 {
			break
		}
	}
	if headerAt == -1 {
		return nil, errors.New(`Wrike workbook has no header row with a "Title" column`)
	}
	header := cells[headerAt]
	unsupported := []any{}
	report := func(path, reason string) { unsupported = append(unsupported, importUnsupportedObj(path, reason)) }
	for i, r := range cells[:headerAt] {
		if hasValue(r) {
			report("/row/"+strconv.Itoa(i+1), "a row above the header row is not imported")
		}
	}
	at := map[string]int{}
	for _, role := range []string{"key", "parent", "title", "status", "customStatus", "workflow", "priority", "people", "start", "duration", "end", "depends", "constraint", "description"} {
		at[role] = -1
		for i, t := range header {
			if wrikeRoleOf(t) == role {
				at[role] = i
				break
			}
		}
	}
	cell := func(row []string, i int) string {
		if i < 0 || i >= len(row) {
			return ""
		}
		return row[i]
	}
	get := func(row []string, role string) string { return cell(row, at[role]) }
	headerName := func(role string) string { return cell(header, at[role]) }
	folderName := func(title string) string {
		var parts []string
		for _, p := range strings.Split(title, "/") {
			if p = jsTrim(p); p != "" {
				parts = append(parts, p)
			}
		}
		return jsOr(strings.Join(parts, "/"), "Default").(string)
	}
	// Folder keys first, so a Parent Task or Depends On can name a folder that
	// comes later in the sheet.
	folders := map[string]string{}
	for _, row := range cells[headerAt+1:] {
		if t := get(row, "title"); wrikeFolder.MatchString(t) && get(row, "key") != "" {
			folders[get(row, "key")] = folderName(t)
		}
	}
	tasks := []any{}
	var workflows []string
	seen := map[string]bool{}
	folder := "Default"
	for offset, row := range cells[headerAt+1:] {
		path := "/row/" + strconv.Itoa(headerAt+offset+2)
		if !hasValue(row) {
			continue
		}
		title, key := get(row, "title"), get(row, "key")
		if wrikeFolder.MatchString(title) {
			folder = folderName(title)
			var kept []string
			for _, role := range []string{"status", "people", "start", "duration", "end", "description"} {
				if get(row, role) != "" {
					kept = append(kept, headerName(role))
				}
			}
			if len(kept) > 0 {
				report(path, "a Wrike folder or project's own "+strings.Join(kept, ", ")+" is not imported")
			}
			continue
		}
		if title == "" {
			report(path, "a Wrike row without a Title is not imported")
			continue
		}
		if key != "" && seen[key] {
			report(path, "a repeated row of the task with Key "+key+" (a task in several folders) is imported once")
			continue
		}
		if key != "" {
			seen[key] = true
		}
		if key == "" {
			report(path, "a Wrike task without a Key cannot be a parent or a dependency")
		} else if n, err := strconv.Atoi(key); !wrikeDigits.MatchString(key) || err != nil || n < 1 || n > maxWrikeKey {
			report(path+"/Key", `Wrike Key "`+key+`" is not an integer between 1 and `+strconv.Itoa(maxWrikeKey))
		}
		t := jsObject{}
		t.set("title", title)
		t.set("description", get(row, "description"))
		t.set("ref", jsOr(key, "wrike"+path))
		t.set("swimlane_name", folder)
		t.set("column_name", jsOr(get(row, "customStatus"), get(row, "status"), "Active"))
		t.set("tags", []any{})
		group := get(row, "status")
		if cs := get(row, "customStatus"); cs != "" && contains(wrikeStatuses, group) && wrikeGroupForName(cs) != group {
			report(path+"/"+headerName("status"), "the "+group+` status group of "`+cs+`" is not kept by the list: import the Wrike workflow in Rules to add the rule that does what it does`)
		}
		if w := get(row, "workflow"); w != "" {
			workflows = addOnce(workflows, w)
		}
		if parent := get(row, "parent"); parent != "" {
			if f, ok := folders[parent]; ok {
				t.set("swimlane_name", f)
			} else {
				t.set("parent_ref", parent)
			}
		}
		people := wrikePeople(get(row, "people"))
		if len(people) > 0 {
			t.set("owner_username", people[0])
		}
		if len(people) > 1 {
			t.set("assignees", people[1:])
		}
		for _, f := range []struct{ role, field string }{{"start", "date_started"}, {"end", "date_due"}} {
			value := get(row, f.role)
			if value == "" {
				continue
			}
			if d, ok := wrikeDate(value); ok {
				t.set(f.field, d)
			} else {
				report(path+"/"+headerName(f.role), `Wrike date "`+value+`" is not a date`)
			}
		}
		if c := get(row, "constraint"); c != "" {
			report(path+"/"+headerName("constraint"), "a Wrike start date constraint ("+c+") is not imported")
		}
		deps := []any{}
		for _, part := range wrikeSplitDeps.Split(get(row, "depends"), -1) {
			if part = jsTrim(part); part == "" {
				continue
			}
			m := wrikeDependency.FindStringSubmatch(part)
			if m == nil {
				report(path+"/"+headerName("depends"), `Wrike dependency "`+part+`" is not <Key><FS|SS|FF|SF>`)
				continue
			}
			if _, isFolder := folders[m[1]]; isFolder {
				report(path+"/"+headerName("depends"), "a dependency on the folder with Key "+m[1]+" is not imported")
				continue
			}
			if kind := strings.ToUpper(jsOr(m[2], "FS").(string)); kind != "FS" {
				report(path+"/"+headerName("depends"), "the "+kind+` kind of Wrike dependency "`+part+`" is kept as is-blocked-by`)
			}
			d := jsObject{}
			d.set("ref", m[1])
			d.set("type", "is-blocked-by")
			deps = append(deps, d)
		}
		if len(deps) > 0 {
			t.set("dependencies", deps)
		}
		custom := jsObject{}
		if p := get(row, "priority"); p != "" {
			custom.set("Priority", p)
		}
		if d := get(row, "duration"); d != "" {
			custom.set("Duration", d)
		}
		for index, name := range header {
			value := cell(row, index)
			if name == "" || value == "" {
				continue
			}
			switch role := wrikeRoleOf(name); role {
			case "defaultWorkflow":
				report(path+"/"+name, "the Wrike "+name+` "`+value+`" of a task is not imported`)
			case "":
				if mondayNumber.MatchString(value) {
					f, _ := strconv.ParseFloat(value, 64)
					custom.set(name, f)
				} else {
					custom.set(name, value)
				}
			}
		}
		if len(custom) > 0 {
			t.set("custom_fields", jsOrder(custom))
		}
		tasks = append(tasks, t)
	}
	var columns, swimlanes []string
	for _, t := range tasks {
		columns = addOnce(columns, jsGet(t, "column_name").(string))
		swimlanes = addOnce(swimlanes, jsGet(t, "swimlane_name").(string))
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
	name := "Imported Wrike board"
	if len(workflows) == 1 {
		name = workflows[0]
	}
	out := importDoc(name, csvColumns(columns), tasks, []any{}, unsupported)
	out.set("swimlanes", lanes)
	return out, nil
}
