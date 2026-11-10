package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ClickUp's task CSV: Node.js WeKan's parseClickUpCsv
// (models/lib/clickupCsvFormat.js) - statuses as lists, lists as swimlanes,
// priorities, types, custom ids, folders, estimates and time spent in hours,
// attachment links, assignees, and parents from either end of the link.

func init() { importTextParsers["clickup"] = parseClickUpCsv }

var (
	clickupPriority = map[string]string{"1": "Urgent", "2": "High", "3": "Normal", "4": "Low",
		"urgent": "Urgent", "high": "High", "normal": "Normal", "low": "Low"}
	clickupMillisRe  = regexp.MustCompile(`^\d{10,14}$`)
	clickupHTTP      = regexp.MustCompile(`(?i)^https?://`)
	clickupBracketRe = regexp.MustCompile(`^\[|\]$`)
	markdownURLBad   = jsRegexp(`[()<>\\\s\x00-\x1f\x7f]`)
)

// markdownLinkText is WeKan's markdownLink (models/lib/markdownLink.js): [title](url) with the
// title's brackets escaped and the URL's spaces, brackets and controls
// percent-encoded.
func markdownLinkText(title, url string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `[`, `\[`, `]`, `\]`).Replace(title)
	u := markdownURLBad.ReplaceAllStringFunc(url, func(c string) string {
		var b strings.Builder
		for _, x := range []byte(c) {
			fmt.Fprintf(&b, "%%%02X", x)
		}
		return b.String()
	})
	return "[" + escaped + "](" + u + ")"
}

// clickupList is WeKan's list(): "[a, b]" or "a,b" as its parts.
func clickupList(text string) []any {
	return splitTrimmed(clickupBracketRe.ReplaceAllString(jsTrim(text), ""), ",")
}

func clickupDate(value string) (string, bool) {
	text := jsTrim(value)
	switch {
	case text == "":
		return "", false
	case clickupMillisRe.MatchString(text):
		ms, _ := strconv.ParseInt(text, 10, 64)
		return jsISO(time.UnixMilli(ms)), true
	}
	return linearDate(text)
}

func clickupHours(ms float64) float64 { return jsRound(ms/3600000*100) / 100 }

func parseClickUpCsv(text string) (jsObject, error) {
	rows, err := jsReadCsv(text, "ClickUp", ',')
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, errors.New("ClickUp CSV is empty")
	}
	header := csvHeader{}
	for _, c := range rows[0] {
		header = append(header, jsTrim(c))
	}
	if header.col("Task Name") == -1 {
		return nil, errors.New("ClickUp CSV needs the Task Name column")
	}
	get := func(cells []string, names ...string) string {
		for _, name := range names {
			if v := header.cell(cells, name); v != "" {
				return v
			}
		}
		return ""
	}
	var columns, swimlanes []string
	tasks, unsupported := []any{}, []any{}
	space := ""
	childParent := map[string]string{}
	for index, cells := range rows[1:] {
		at := "/row/" + strconv.Itoa(index+2)
		title := get(cells, "Task Name")
		if title == "" {
			unsupported = append(unsupported, importUnsupportedObj(at, "a ClickUp row without a task name is not imported"))
			continue
		}
		if space == "" {
			space = get(cells, "Space Name")
		}
		status := jsOr(get(cells, "Status"), "No status").(string)
		columns = addOnce(columns, status)
		lane := jsOr(get(cells, "List Name", "List"), "Default").(string)
		swimlanes = addOnce(swimlanes, lane)
		date := func(names ...string) string {
			value := get(cells, names...)
			iso, ok := clickupDate(value)
			if value != "" && !ok {
				unsupported = append(unsupported, importUnsupportedObj(at+"/"+names[0], `ClickUp date "`+value+`" is neither milliseconds nor ISO 8601`))
			}
			return iso
		}
		custom := jsObject{}
		priority := get(cells, "Priority")
		if p := clickupPriority[jsLower(priority)]; p != "" {
			custom.set("Priority", p)
		} else if priority != "" {
			unsupported = append(unsupported, importUnsupportedObj(at+"/Priority", `ClickUp priority "`+priority+`" is not urgent, high, normal, low or 1-4`))
		}
		if typ := get(cells, "Task Type", "Task type"); typ != "" && jsLower(typ) != "task" {
			custom.set("Task Type", typ)
		}
		if v := get(cells, "Task Custom ID"); v != "" {
			custom.set("Custom ID", v)
		}
		if v := get(cells, "Folder Name/Path"); v != "" {
			custom.set("Folder", v)
		}
		if estimate, ok := jsToNumber(get(cells, "Time Estimated", "Time Estimate")); ok && estimate > 0 {
			custom.set("Time Estimate (hours)", clickupHours(estimate))
		}
		spent, _ := jsToNumber(get(cells, "Time Spent", "Time Tracked"))
		description := get(cells, "Task Content", "Description content")
		if attachments := get(cells, "Attachments"); attachments != "" && attachments != "[]" {
			list, err := decodeJS([]byte(attachments))
			entries, isList := list.([]any)
			if err != nil || !json.Valid([]byte(attachments)) || !isList {
				unsupported = append(unsupported, importUnsupportedObj(at+"/Attachments", "ClickUp attachments are not the documented JSON list"))
			} else {
				var links []string
				for _, a := range entries {
					u := jsGet(a, "url")
					if !jsTruthy(a) || !clickupHTTP.MatchString(jsStr(jsOr(u, ""))) {
						continue
					}
					links = append(links, markdownLinkText(jsStr(jsOr(jsGet(a, "title"), u)), jsStr(u)))
				}
				if len(links) > 0 {
					if description != "" {
						description += "\n\n" + strings.Join(links, "\n")
					} else {
						description = strings.Join(links, "\n")
					}
				}
			}
		}
		for _, name := range []string{"Checklists", "Comments", "Assigned Comments"} {
			if v := get(cells, name); v != "" && v != "0" && v != "[]" {
				unsupported = append(unsupported, importUnsupportedObj(at+"/"+name, "ClickUp "+name+" cells have no documented format and are not imported"))
			}
		}
		items := clickupList(get(cells, "Checklist"))
		people := clickupList(get(cells, "Assignees", "Task assignee"))
		id := get(cells, "Task ID")
		for _, child := range clickupList(get(cells, "Subtask IDs")) {
			if id != "" {
				childParent[child.(string)] = id
			}
		}
		t := jsObject{}
		t.set("title", title)
		t.set("description", description)
		t.set("column_name", status)
		t.set("swimlane_name", lane)
		t.set("tags", clickupList(get(cells, "Tags")))
		if id != "" {
			t.set("ref", id)
		}
		if v := get(cells, "Parent ID"); v != "" {
			t.set("parent_ref", v)
		}
		if len(people) > 0 {
			t.set("owner_username", people[0])
		}
		if len(people) > 1 {
			t.set("assignees", people[1:])
		}
		for _, f := range []struct{ key, col string }{{"date_creation", "Date created"}, {"date_due", "Due date"}, {"date_started", "Start date"}} {
			if v := date(f.col); v != "" {
				t.set(f.key, v)
			}
		}
		if spent > 0 && !math.IsInf(spent, 0) {
			t.set("spent_hours", clickupHours(spent))
		}
		if len(items) > 0 {
			its := []any{}
			for _, i := range items {
				o := jsObject{}
				o.set("title", i)
				o.set("done", false)
				its = append(its, o)
			}
			cl := jsObject{}
			cl.set("title", "Checklist")
			cl.set("items", its)
			t.set("checklists", []any{cl})
		}
		if len(custom) > 0 {
			t.set("custom_fields", custom)
		}
		tasks = append(tasks, t)
	}
	// A Subtask IDs list is the same link seen from the parent.
	for i, task := range tasks {
		t := task.(jsObject)
		_, hasParent := jsLookup(t, "parent_ref")
		if ref, ok := jsGet(t, "ref").(string); ok && !hasParent {
			if p, ok := childParent[ref]; ok {
				t.set("parent_ref", p)
				tasks[i] = t
			}
		}
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
	out := importDoc(jsOr(space, "Imported ClickUp tasks"), csvColumns(columns), tasks, []any{}, unsupported)
	out.set("swimlanes", lanes)
	return out, nil
}
