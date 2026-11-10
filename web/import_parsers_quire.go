package web

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Quire's project CSV: Node.js WeKan's parseQuireCsv
// (models/lib/quireCsvFormat.js) - statuses as lists, its task tree from a
// Parent column, an ID path or the depth of the ID column, tags, assignees,
// its priority, and the dates it writes in words or numbers.

func init() { importTextParsers["quire"] = parseQuireCsv }

var (
	quirePriorities  = []string{"Low", "Medium", "High", "Urgent"}
	quirePriorityNum = map[string]string{"-1": "Low", "0": "Medium", "1": "High", "2": "Urgent"}
	quireRead        = map[string]bool{"id": true, "name": true, "parent": true, "assignee": true, "tag": true, "start": true,
		"started": true, "due": true, "completed": true, "priority": true, "status": true, "description": true,
		"created": true, "created by": true}
	quireMonths     = []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}
	quireMonthDay   = regexp.MustCompile(`^([A-Za-z]{3})[a-z]*\.? (\d{1,2}),? (\d{4})$`)
	quireDayMonth   = regexp.MustCompile(`^(\d{1,2}) ([A-Za-z]{3})[a-z]*\.?,? (\d{4})$`)
	quireNumeric    = regexp.MustCompile(`^(\d{4})[-/](\d{2})[-/](\d{2})$`)
	quireDateTimeRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}`)
	quireTaskID     = regexp.MustCompile(`^#?\d+$`)
)

func quireUnformula(text string) string {
	if len(text) > 1 && text[0] == '\'' && strings.ContainsRune("+-=@", rune(text[1])) {
		return text[1:]
	}
	return text
}

// quireUTCDay is WeKan's utcDay: a real day of a year Date.UTC keeps (it
// moves 0-99 to the 1900s, so they are no day).
func quireUTCDay(year, month, day int) (string, bool) {
	if year < 100 {
		return "", false
	}
	d := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
	if d.Year() != year || int(d.Month()) != month || d.Day() != day {
		return "", false
	}
	return jsISO(d), true
}

func quireMonth(name string) int {
	for i, m := range quireMonths {
		if m == jsLower(name) {
			return i + 1
		}
	}
	return 0
}

func quireDate(value string) (string, bool) {
	text := jsTrim(value)
	n := func(s string) int { v, _ := strconv.Atoi(s); return v }
	if text == "" {
		return "", false
	}
	if m := quireMonthDay.FindStringSubmatch(text); m != nil {
		if month := quireMonth(m[1]); month > 0 {
			return quireUTCDay(n(m[3]), month, n(m[2]))
		}
		return "", false
	}
	if m := quireDayMonth.FindStringSubmatch(text); m != nil {
		if month := quireMonth(m[2]); month > 0 {
			return quireUTCDay(n(m[3]), month, n(m[1]))
		}
		return "", false
	}
	if m := quireNumeric.FindStringSubmatch(text); m != nil {
		return quireUTCDay(n(m[1]), n(m[2]), n(m[3]))
	}
	if !quireDateTimeRe.MatchString(text) {
		return "", false
	}
	t, ok := jsDateTime(text)
	if !ok {
		return "", false
	}
	return jsISO(t), true
}

func quireTaskRef(value string) string {
	text := jsTrim(quireUnformula(value))
	if quireTaskID.MatchString(text) {
		return "#" + strings.TrimPrefix(text, "#")
	}
	return text
}

func quireSplit(value string) []string {
	var out []string
	for _, p := range strings.Split(quireUnformula(value), ",") {
		if p = jsTrim(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func uniqStrings(values []string) []any {
	out := []any{}
	seen := map[string]bool{}
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func parseQuireCsv(text string) (jsObject, error) {
	rows, err := jsReadCsv(text, "Quire", ',')
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, errors.New("Quire CSV is empty")
	}
	var header, lower []string
	for _, c := range rows[0] {
		header = append(header, jsTrim(c))
		lower = append(lower, jsLower(jsTrim(c)))
	}
	at := func(name string) []int {
		var out []int
		for i, c := range lower {
			if c == name {
				out = append(out, i)
			}
		}
		return out
	}
	if len(at("name")) == 0 {
		return nil, errors.New("Quire CSV needs the Name column")
	}
	all := func(cells []string, name string) []string {
		var out []string
		for _, i := range at(name) {
			v := ""
			if i < len(cells) {
				v = jsTrim(cells[i])
			}
			out = append(out, v)
		}
		return out
	}
	get := func(cells []string, name string) string {
		for _, v := range all(cells, name) {
			if v != "" {
				return quireUnformula(v)
			}
		}
		return ""
	}
	unsupported := []any{}
	var odd []string
	for i, name := range header {
		if name != "" && !quireRead[lower[i]] {
			odd = append(odd, name)
		}
	}
	for _, name := range uniqStrings(odd) {
		unsupported = append(unsupported, importUnsupportedObj("/columns/"+name.(string), `Quire column "`+name.(string)+`" has no WeKan place and is not imported`))
	}
	idColumns := at("id")
	var columns []string
	tasks := []any{}
	var lastAtDepth []string
	for index, cells := range rows[1:] {
		where := "/row/" + strconv.Itoa(index+2)
		title := get(cells, "name")
		if title == "" {
			unsupported = append(unsupported, importUnsupportedObj(where, "a Quire row without a name is not imported"))
			continue
		}
		type filledCell struct {
			cell  string
			depth int
		}
		var filled []filledCell
		for depth, c := range idColumns {
			v := ""
			if c < len(cells) {
				v = jsTrim(cells[c])
			}
			if v != "" {
				filled = append(filled, filledCell{v, depth})
			}
		}
		var path []string
		for _, f := range filled {
			for _, p := range quireSplit(f.cell) {
				path = append(path, quireTaskRef(p))
			}
		}
		ref := ""
		if len(path) > 0 {
			ref = path[len(path)-1]
		}
		parent := quireTaskRef(get(cells, "parent"))
		if parent == "" && len(path) > 1 {
			parent = path[len(path)-2]
		}
		if parent == "" && len(filled) == 1 && filled[0].depth > 0 && filled[0].depth-1 < len(lastAtDepth) {
			parent = lastAtDepth[filled[0].depth-1]
		}
		if len(filled) == 1 {
			// lastAtDepth[depth] = ref; lastAtDepth.length = depth + 1
			d := filled[0].depth
			for len(lastAtDepth) <= d {
				lastAtDepth = append(lastAtDepth, "")
			}
			lastAtDepth[d] = ref
			lastAtDepth = lastAtDepth[:d+1]
		}
		status := jsOr(get(cells, "status"), "To-Do").(string)
		columns = addOnce(columns, status)
		date := func(name string) string {
			value := get(cells, jsLower(name))
			iso, ok := quireDate(value)
			if value != "" && !ok {
				unsupported = append(unsupported, importUnsupportedObj(where+"/"+name, `Quire date "`+value+`" could not be read`))
			}
			return iso
		}
		custom := jsObject{}
		priority := get(cells, "priority")
		named := ""
		for _, p := range quirePriorities {
			if jsLower(p) == jsLower(priority) {
				named = p
				break
			}
		}
		if named == "" {
			named = quirePriorityNum[priority]
		}
		if named != "" && named != "Medium" {
			custom.set("Priority", named)
		} else if priority != "" && named == "" {
			unsupported = append(unsupported, importUnsupportedObj(where+"/Priority", `Quire priority "`+priority+`" is not one of `+strings.Join(quirePriorities, ", ")))
		}
		start := date("Start")
		started := date("Started")
		if start != "" && started != "" {
			unsupported = append(unsupported, importUnsupportedObj(where+"/Started", "Quire's Started date is not kept beside its Start date"))
		}
		var peopleList []string
		for _, a := range all(cells, "assignee") {
			peopleList = append(peopleList, quireSplit(a)...)
		}
		people := uniqStrings(peopleList)
		created, due, completed := date("Created"), date("Due"), date("Completed")
		var tagList []string
		for _, a := range all(cells, "tag") {
			tagList = append(tagList, quireSplit(a)...)
		}
		t := jsObject{}
		t.set("title", title)
		t.set("description", get(cells, "description"))
		t.set("column_name", status)
		t.set("swimlane_name", "Default")
		t.set("tags", uniqStrings(tagList))
		if ref != "" {
			t.set("ref", ref)
		}
		if parent != "" && parent != ref {
			t.set("parent_ref", parent)
		}
		if len(people) > 0 {
			t.set("owner_username", people[0])
		}
		if len(people) > 1 {
			t.set("assignees", people[1:])
		}
		if c := get(cells, "created by"); c != "" {
			t.set("requested_by", c)
		}
		if created != "" {
			t.set("date_creation", created)
		}
		if s := jsOr(start, started).(string); s != "" {
			t.set("date_started", s)
		}
		if due != "" {
			t.set("date_due", due)
		}
		if completed != "" {
			t.set("date_end", completed)
		}
		if len(custom) > 0 {
			t.set("custom_fields", custom)
		}
		tasks = append(tasks, t)
	}
	return importDoc("Imported Quire project", csvColumns(columns), tasks, []any{}, unsupported), nil
}
