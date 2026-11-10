package web

import (
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"
	"time"
)

// A Tasks.org backup: Node.js WeKan's parseTasksOrgBackup
// (models/lib/tasksorgFormat.js) - its CalDAV lists as lists in Tasks.org's
// own order, tasks with their tags, priority, estimate, time spent, dates,
// comments and parent, and the account's name as the board's title.

func init() { importParsers["tasksorg"] = parseTasksOrgBackup }

var tasksorgPriority = []string{"High", "Medium", "Low"}

func tasksorgIsObject(v any) bool { _, ok := v.(jsObject); return ok }

func tasksorgArray(v any) []any {
	if a, ok := v.([]any); ok {
		return a
	}
	return nil
}

func tasksorgText(v any) string { s, _ := v.(string); return s }

func tasksorgPositive(v any) bool {
	f, ok := v.(float64)
	return ok && !math.IsInf(f, 0) && !math.IsNaN(f) && f > 0
}

// tasksorgDate is WeKan's: epoch ms as ISO; a due or start date with no
// time - a whole minute - as that day at 12:00 UTC.
func tasksorgDate(v any, allDayAware bool) (string, bool) {
	if !tasksorgPositive(v) {
		return "", false
	}
	ms := v.(float64)
	if ms > 8.64e15 {
		return "", false
	}
	at := func(ms float64) string { return jsISO(time.UnixMilli(int64(math.Trunc(ms)))) }
	if !allDayAware {
		return at(ms), true
	}
	if r := math.Mod(ms, 60000); r > 0 {
		return at(ms - r), true
	}
	d := time.UnixMilli(int64(math.Trunc(ms))).UTC()
	return jsISO(time.Date(d.Year(), d.Month(), d.Day(), 12, 0, 0, 0, time.UTC)), true
}

func parseTasksOrgBackup(input any) (jsObject, error) {
	doc := input
	if s, ok := input.(string); ok {
		v, err := decodeJS([]byte(s))
		if err != nil || !json.Valid([]byte(s)) {
			return nil, errors.New("Tasks.org backup is not JSON")
		}
		doc = v
	}
	data := jsGet(doc, "data")
	if _, ok := jsGet(data, "tasks").([]any); !tasksorgIsObject(doc) || !tasksorgIsObject(data) || !ok {
		return nil, errors.New("Tasks.org backup needs data.tasks (Settings > Backups > Export tasks)")
	}
	unsupported := []any{}
	var calendars, accounts []any
	for _, c := range tasksorgArray(jsGet(data, "caldavCalendars")) {
		if tasksorgIsObject(c) {
			calendars = append(calendars, c)
		}
	}
	for _, a := range tasksorgArray(jsGet(data, "caldavAccounts")) {
		if tasksorgIsObject(a) {
			accounts = append(accounts, a)
		}
	}
	// Lists in Tasks.org's own order; those without one keep the file's order.
	orderOf := func(c any) float64 {
		if f, ok := jsGet(c, "order").(float64); ok && f == math.Trunc(f) && !math.IsInf(f, 0) && f >= 0 {
			return f
		}
		return math.Inf(1)
	}
	ordered := append([]any{}, calendars...)
	sort.SliceStable(ordered, func(i, j int) bool { return orderOf(ordered[i]) < orderOf(ordered[j]) })
	listByUUID := map[string]string{}
	var columns []string
	useTitle := func(title string) string {
		columns = addOnce(columns, title)
		return title
	}
	for _, c := range ordered {
		uuid := tasksorgText(jsGet(c, "uuid"))
		if _, seen := listByUUID[uuid]; uuid == "" || seen {
			continue
		}
		listByUUID[uuid] = useTitle(jsOr(jsTrim(tasksorgText(jsGet(c, "name"))), "Untitled list").(string))
	}
	tasks := []any{}
	usedAccounts := []string{}
	for index, backup := range tasksorgArray(jsGet(data, "tasks")) {
		at := "/data/tasks/" + strconv.Itoa(index)
		task := jsGet(backup, "task")
		if !tasksorgIsObject(backup) || !tasksorgIsObject(task) {
			unsupported = append(unsupported, importUnsupportedObj(at, "a Tasks.org backup entry without a task is not imported"))
			continue
		}
		title := jsTrim(tasksorgText(jsGet(task, "title")))
		named := "an untitled task"
		if title != "" {
			named = `"` + title + `"`
		}
		if tasksorgPositive(jsGet(task, "deletionDate")) {
			unsupported = append(unsupported, importUnsupportedObj(at+"/task/deletionDate", "Tasks.org task "+named+" is deleted and is not imported"))
			continue
		}
		var caldav any
		for _, row := range tasksorgArray(jsGet(backup, "caldavTasks")) {
			if tasksorgIsObject(row) && !tasksorgPositive(jsGet(row, "deleted")) {
				caldav = row
				break
			}
		}
		column := "No list"
		if cal := tasksorgText(jsGet(caldav, "calendar")); caldav != nil && cal != "" {
			if list, ok := listByUUID[cal]; ok {
				column = list
				for _, c := range calendars {
					if jsGet(c, "uuid") == cal {
						if acc := tasksorgText(jsGet(c, "account")); acc != "" {
							usedAccounts = addOnce(usedAccounts, acc)
						}
						break
					}
				}
			} else {
				unsupported = append(unsupported, importUnsupportedObj(at+"/caldavTasks/0/calendar", `Tasks.org list "`+cal+`" is not in the backup's caldavCalendars; the task is put in No list`))
			}
		}
		useTitle(column)
		custom := jsObject{}
		priority, has := jsLookup(task, "priority")
		if !has {
			priority = 3.0
		}
		// TASKSORG_PRIORITY[priority]: an array read by a number or its
		// canonical string - and "length" is 3.
		name := ""
		switch p := priority.(type) {
		case float64:
			if p == math.Trunc(p) && p >= 0 && p < 3 {
				name = tasksorgPriority[int(p)]
			}
		case string:
			if n, err := strconv.Atoi(p); err == nil && strconv.Itoa(n) == p && n >= 0 && n < 3 {
				name = tasksorgPriority[n]
			}
		}
		switch {
		case name != "":
			custom.set("Priority", name)
		case priority == "length":
			custom.set("Priority", 3.0)
		case priority != 3.0:
			unsupported = append(unsupported, importUnsupportedObj(at+"/task/priority", "Tasks.org priority "+string(jsJSON(priority, ""))+" is not 0, 1, 2 or 3"))
		}
		if e := jsGet(task, "estimatedSeconds"); tasksorgPositive(e) {
			custom.set("Estimate (hours)", jsRound(e.(float64)/36)/100)
		}
		if r := tasksorgText(jsGet(task, "recurrence")); r != "" {
			unsupported = append(unsupported, importUnsupportedObj(at+"/task/recurrence", `Tasks.org repeat rule "`+r+`" has no WeKan equivalent`))
		}
		for _, k := range []struct{ key, what string }{{"alarms", "Tasks.org reminder(s) of " + named + " are not imported"},
			{"geofences", "Tasks.org location reminder(s) of " + named + " are not imported"},
			{"attachments", "Tasks.org attachment(s) of " + named + ": the backup has no file contents"}} {
			if n := len(tasksorgArray(jsGet(backup, k.key))); n > 0 {
				unsupported = append(unsupported, importUnsupportedObj(at+"/"+k.key, strconv.Itoa(n)+" "+k.what))
			}
		}
		if tasksorgPositive(jsGet(task, "timerStart")) {
			unsupported = append(unsupported, importUnsupportedObj(at+"/task/timerStart", "the running Tasks.org timer of "+named+" is not imported"))
		}
		comments := []any{}
		for c, comment := range tasksorgArray(jsGet(backup, "comments")) {
			if !tasksorgIsObject(comment) {
				continue
			}
			if tasksorgText(jsGet(comment, "picture")) != "" {
				unsupported = append(unsupported, importUnsupportedObj(at+"/comments/"+strconv.Itoa(c)+"/picture", "a Tasks.org comment picture is not in the backup"))
			}
			message := tasksorgText(jsGet(comment, "message"))
			if jsTrim(message) == "" {
				continue
			}
			o := jsObject{}
			o.set("text", message)
			if created := jsGet(comment, "created"); tasksorgPositive(created) {
				if d, ok := tasksorgDate(created, false); ok {
					o.set("date", d)
				} else {
					o.set("date", undefinedValue{})
				}
			}
			comments = append(comments, o)
		}
		var tagNames []string
		for _, tag := range tasksorgArray(jsGet(backup, "tags")) {
			if tasksorgIsObject(tag) {
				if n := jsTrim(tasksorgText(jsGet(tag, "name"))); n != "" {
					tagNames = append(tagNames, n)
				}
			}
		}
		ref := tasksorgText(jsGet(caldav, "remoteId"))
		if ref == "" {
			ref = tasksorgText(jsGet(task, "remoteId"))
		}
		due, _ := tasksorgDate(jsGet(task, "dueDate"), true)
		start, _ := tasksorgDate(jsGet(task, "hideUntil"), true)
		ended, _ := tasksorgDate(jsGet(task, "completionDate"), false)
		created, _ := tasksorgDate(jsGet(task, "creationDate"), false)
		t := jsObject{}
		t.set("title", title)
		t.set("description", tasksorgText(jsGet(task, "notes")))
		t.set("column_name", column)
		t.set("swimlane_name", "Default")
		t.set("tags", uniqStrings(tagNames))
		if ref != "" {
			t.set("ref", ref)
		}
		if p := tasksorgText(jsGet(caldav, "remoteParent")); p != "" {
			t.set("parent_ref", p)
		}
		for _, d := range []struct{ key, value string }{{"date_due", due}, {"date_started", start}, {"date_end", ended}, {"date_creation", created}} {
			if d.value != "" {
				t.set(d.key, d.value)
			}
		}
		if e := jsGet(task, "elapsedSeconds"); tasksorgPositive(e) {
			t.set("spent_hours", jsRound(e.(float64)/36)/100)
		}
		if len(comments) > 0 {
			t.set("comments", comments)
		}
		if len(custom) > 0 {
			t.set("custom_fields", custom)
		}
		tasks = append(tasks, t)
	}
	name := ""
	if len(usedAccounts) == 1 {
		for _, a := range accounts {
			if jsGet(a, "uuid") == usedAccounts[0] {
				name = jsTrim(tasksorgText(jsGet(a, "name")))
				break
			}
		}
	}
	return importDoc(jsOr(name, "Imported Tasks.org"), csvColumns(columns), tasks, []any{}, unsupported), nil
}
