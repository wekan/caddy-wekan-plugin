package web

import (
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Pivotal Tracker's stories CSV: Node.js WeKan's parsePivotalCsv
// (models/lib/pivotalCsvFormat.js) - states as lists, types and labels as
// labels, points and priority, owners, requester, dates, and its repeated
// Task, Comment and Blocker columns as checklist, comments and links. Its
// Scrum planning waits for Weddy's sprints.

func init() { importTextParsers["pivotal"] = parsePivotalCsv }

var (
	pivotalKnown = map[string]bool{"id": true, "title": true, "labels": true, "iteration": true, "iteration start": true,
		"iteration end": true, "type": true, "estimate": true, "priority": true, "current state": true, "created at": true,
		"accepted at": true, "deadline": true, "requested by": true, "description": true, "url": true, "owned by": true,
		"blocker": true, "blocker status": true, "comment": true, "task": true, "task status": true, "pull request": true,
		"git branch": true}
	pivotalPriorityRe = jsRegexp(`(?i)^p[0-3] - \S` + jsDotU + `*$`)
	pivotalMonths     = []string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}
	pivotalWordsDate  = regexp.MustCompile(`^([A-Za-z]{3})[a-z]*\.? (\d{1,2}), (\d{4})$`)
	pivotalUSDate     = regexp.MustCompile(`^(\d{1,2})/(\d{1,2})/(\d{4})$`)
	pivotalCommentRe  = jsRegexp(`^((?s:.)*?)\s*\(([^()]+?) - ([A-Za-z]{3}[a-z]*\.? \d{1,2}, \d{4})\)\s*$`)
	pivotalRef        = regexp.MustCompile(`#\d+`)
)

func pivotalHas(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// pivotalDate is WeKan's: "Oct 10, 2026" or "10/10/2026" as that day at UTC.
func pivotalDate(value string) (string, bool) {
	text := jsTrim(value)
	n := func(s string) int { v, _ := strconv.Atoi(s); return v }
	var year, month, day int
	if w := pivotalWordsDate.FindStringSubmatch(text); w != nil {
		for i, name := range pivotalMonths {
			if strings.EqualFold(name, w[1]) {
				month = i + 1
			}
		}
		day, year = n(w[2]), n(w[3])
	} else if u := pivotalUSDate.FindStringSubmatch(text); u != nil {
		month, day, year = n(u[1]), n(u[2]), n(u[3])
	} else {
		return "", false
	}
	if month < 1 || month > 12 {
		return "", false
	}
	if year < 100 {
		year += 1900 // Date.UTC's
	}
	d := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
	if int(d.Month()) != month || d.Day() != day {
		return "", false
	}
	return jsISO(d), true
}

// pivotalComment is WeKan's: "Text (Author - Oct 10, 2026)" as its text,
// author and date; just the text when it has no such tail.
func pivotalComment(value string) jsObject {
	text := jsTrim(value)
	c := jsObject{}
	m := pivotalCommentRe.FindStringSubmatch(text)
	date, ok := "", false
	if m != nil {
		date, ok = pivotalDate(m[3])
	}
	if !ok {
		c.set("text", text)
		return c
	}
	c.set("text", m[1])
	c.set("author", jsTrim(m[2]))
	c.set("date", date)
	return c
}

func pivotalUnguard(text string) string {
	if strings.HasPrefix(text, "'=") {
		return text[1:]
	}
	return text
}

func parsePivotalCsv(text string) (jsObject, error) {
	rows, err := jsReadCsv(text, "Pivotal Tracker", ',')
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, errors.New("Pivotal Tracker CSV is empty")
	}
	header := csvHeader{}
	for _, c := range rows[0] {
		header = append(header, jsLower(jsTrim(c)))
	}
	if header.col("title") == -1 || header.col("current state") == -1 {
		return nil, errors.New("Pivotal Tracker CSV needs the Title and Current State columns")
	}
	positions := map[string][]int{}
	var names []string
	for i, name := range header {
		if _, seen := positions[name]; !seen {
			names = append(names, name)
		}
		positions[name] = append(positions[name], i)
	}
	cell := func(cells []string, i int) string {
		if i >= len(cells) {
			return ""
		}
		return pivotalUnguard(jsTrim(cells[i]))
	}
	all := func(cells []string, name string) []string {
		var out []string
		for _, i := range positions[name] {
			out = append(out, cell(cells, i))
		}
		return out
	}
	get := func(cells []string, name string) string {
		if a := all(cells, name); len(a) > 0 {
			return a[0]
		}
		return ""
	}
	type pair struct{ value, status string }
	pairs := func(cells []string, name, status string) []pair {
		statuses := all(cells, status)
		var out []pair
		for i, v := range all(cells, name) {
			s := ""
			if i < len(statuses) {
				s = jsLower(statuses[i])
			}
			if v != "" {
				out = append(out, pair{v, s})
			}
		}
		return out
	}
	unsupported := []any{}
	for _, name := range names {
		if name != "" && !pivotalKnown[name] {
			unsupported = append(unsupported, importUnsupportedObj("/columns/"+name, `Pivotal Tracker does not write a "`+name+`" column; it is not imported`))
		}
	}
	tasks := []any{}
	states := map[string]bool{}
	for index, cells := range rows[1:] {
		at := "/row/" + strconv.Itoa(index+2)
		title := get(cells, "title")
		if title == "" {
			unsupported = append(unsupported, importUnsupportedObj(at, "a Pivotal Tracker story without a title is not imported"))
			continue
		}
		date := func(name string) string {
			value := get(cells, name)
			iso, ok := pivotalDate(value)
			if value != "" && !ok {
				unsupported = append(unsupported, importUnsupportedObj(at+"/"+name, `Pivotal Tracker date "`+value+`" is not "Mon D, YYYY" or "MM/DD/YYYY"`))
			}
			return iso
		}
		stateText := jsLower(get(cells, "current state"))
		state := jsOr(stateText, "unscheduled").(string)
		if !pivotalHas(pivotalStates, state) {
			unsupported = append(unsupported, importUnsupportedObj(at+"/Current State", `Pivotal Tracker state "`+stateText+`" is not one of `+strings.Join(pivotalStates, ", ")+"; imported as unstarted"))
			state = "unstarted"
		}
		states[state] = true
		typeText := jsLower(get(cells, "type"))
		typ := jsOr(typeText, "feature").(string)
		if !pivotalHas(pivotalTypes, typ) {
			unsupported = append(unsupported, importUnsupportedObj(at+"/Type", `Pivotal Tracker type "`+typeText+`" is not one of `+strings.Join(pivotalTypes, ", ")+"; imported as feature"))
			typ = "feature"
		}
		tags := jsUniq(append([]any{typ}, splitTrimmed(get(cells, "labels"), ",")...))
		custom := jsObject{}
		if est := get(cells, "estimate"); est != "" && est != "-1" {
			if n, ok := jsToNumber(est); ok && !math.IsInf(n, 0) && n >= 0 {
				custom.set("Story points", n)
			} else {
				unsupported = append(unsupported, importUnsupportedObj(at+"/Estimate", `Pivotal Tracker estimate "`+est+`" is not a number of points`))
			}
		}
		if p := get(cells, "priority"); p != "" && jsLower(p) != "none" {
			if pivotalPriorityRe.MatchString(p) {
				custom.set("Priority", p)
			} else {
				unsupported = append(unsupported, importUnsupportedObj(at+"/Priority", `Pivotal Tracker priority "`+p+`" is not p0 to p3`))
			}
		}
		var owners []any
		for _, o := range all(cells, "owned by") {
			if o != "" {
				owners = append(owners, o)
			}
		}
		owners = jsUniq(owners)
		comments := []any{}
		n := 0
		for _, v := range all(cells, "comment") {
			if v == "" {
				continue
			}
			n++
			c := pivotalComment(v)
			if _, has := jsLookup(c, "author"); !has {
				unsupported = append(unsupported, importUnsupportedObj(at+"/Comment/"+strconv.Itoa(n), `a Pivotal Tracker comment without "(Author - Mon D, YYYY)" is imported without its author and date`))
			}
			comments = append(comments, c)
		}
		items := []any{}
		for _, p := range pairs(cells, "task", "task status") {
			o := jsObject{}
			o.set("title", p.value)
			o.set("done", p.status == "completed")
			items = append(items, o)
		}
		deps := []any{}
		for i, p := range pairs(cells, "blocker", "blocker status") {
			refs := pivotalRef.FindAllString(p.value, -1)
			if p.status != "resolved" && len(refs) == 1 {
				d := jsObject{}
				d.set("ref", refs[0][1:])
				d.set("type", "is-blocked-by")
				deps = append(deps, d)
				continue
			}
			reason := "a Pivotal Tracker blocker that names no single story (#id) is not imported"
			if p.status == "resolved" {
				reason = "a resolved Pivotal Tracker blocker is not imported"
			}
			unsupported = append(unsupported, importUnsupportedObj(at+"/Blocker/"+strconv.Itoa(i+1), reason))
		}
		for _, name := range []string{"pull request", "git branch"} {
			count := 0
			for _, v := range all(cells, name) {
				if v != "" {
					count++
				}
			}
			if count > 0 {
				unsupported = append(unsupported, importUnsupportedObj(at+"/"+name, strconv.Itoa(count)+" Pivotal Tracker "+name+"(s) of this story are not imported"))
			}
		}
		due, created, accepted := date("deadline"), date("created at"), date("accepted at")
		t := jsObject{}
		t.set("title", title)
		t.set("description", get(cells, "description"))
		t.set("column_name", strings.ToUpper(state[:1])+state[1:])
		t.set("swimlane_name", "Default")
		t.set("tags", tags)
		if id := get(cells, "id"); id != "" {
			t.set("ref", id)
		}
		if len(owners) > 0 {
			t.set("owner_username", owners[0])
		}
		if len(owners) > 1 {
			t.set("assignees", owners[1:])
		}
		if r := get(cells, "requested by"); r != "" {
			t.set("requested_by", r)
		}
		for _, d := range []struct{ key, value string }{{"date_due", due}, {"date_creation", created}, {"date_end", accepted}} {
			if d.value != "" {
				t.set(d.key, d.value)
			}
		}
		if len(custom) > 0 {
			t.set("custom_fields", custom)
		}
		if len(items) > 0 {
			cl := jsObject{}
			cl.set("title", "Tasks")
			cl.set("items", items)
			t.set("checklists", []any{cl})
		}
		if len(comments) > 0 {
			t.set("comments", comments)
		}
		if len(deps) > 0 {
			t.set("dependencies", deps)
		}
		tasks = append(tasks, t)
	}
	var columns []string
	for _, s := range pivotalStates {
		if states[s] {
			columns = append(columns, strings.ToUpper(s[:1])+s[1:])
		}
	}
	return importDoc("Imported Pivotal Tracker", csvColumns(columns), tasks, []any{}, unsupported), nil
}
