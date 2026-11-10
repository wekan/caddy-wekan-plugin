package web

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// todo.txt and Taskwarrior's `task export`, text sources as Node.js WeKan's
// todoTxtFormat.js and taskwarriorFormat.js read them - checked against
// WeKan's own parsers.

func init() {
	importTextParsers["todotxt"] = parseTodoTxt
	importTextParsers["taskwarrior"] = parseTaskwarrior
}

var (
	todoDate     = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	todoPriority = regexp.MustCompile(`^\(([A-Z])\)$`)
	todoProject  = jsRegexp(`^\+(\S+)$`)
	todoContext  = jsRegexp(`^@(\S+)$`)
	todoPair     = jsRegexp(`^([^:\s]+):([^:\s]+)$`)
	todoKnown    = []string{"due", "t", "list", "pri"}
	jsSpaces     = jsRegexp(`\s+`)
)

// jsSplitWords is s.trim().split(/\s+/).filter(Boolean).
func jsSplitWords(s string) []string {
	var out []string
	for _, w := range jsSpaces.Split(jsTrim(s), -1) {
		if w != "" {
			out = append(out, w)
		}
	}
	return out
}

func unspace(s string) string { return strings.ReplaceAll(s, "_", " ") }

func parseTodoTxt(text string) (jsObject, error) {
	lines := jsLines(text)
	if len(lines) > 10000 {
		return nil, errors.New("todo.txt has more than 10000 lines")
	}
	tasks, unsupported := []any{}, []any{}
	for n, line := range lines {
		words := jsSplitWords(line)
		if len(words) == 0 {
			continue
		}
		done, priority := false, ""
		if words[0] == "x" {
			done, words = true, words[1:]
		} else if m := todoPriority.FindStringSubmatch(words[0]); m != nil {
			priority, words = m[1], words[1:]
		}
		first := func() string {
			if len(words) > 0 {
				return words[0]
			}
			return ""
		}
		completed, created := "", ""
		if done && todoDate.MatchString(first()) {
			completed, words = words[0], words[1:]
			if todoDate.MatchString(first()) {
				created, words = words[0], words[1:]
			}
		} else if todoDate.MatchString(first()) {
			created, words = words[0], words[1:]
		}
		tags := []any{}
		meta := map[string]string{}
		var title []string
		for _, w := range words {
			if m := todoProject.FindStringSubmatch(w); m != nil {
				tags = append(tags, unspace(m[1]))
			} else if m := todoContext.FindStringSubmatch(w); m != nil {
				tags = append(tags, "@"+unspace(m[1]))
			} else if m := todoPair.FindStringSubmatch(w); m != nil && contains(todoKnown, m[1]) && func() bool { _, has := meta[m[1]]; return !has }() {
				meta[m[1]] = m[2]
			} else {
				title = append(title, w)
			}
		}
		if priority == "" && regexp.MustCompile(`^[A-Z]$`).MatchString(meta["pri"]) {
			priority = meta["pri"]
		}
		if priority != "" {
			tags = append([]any{"priority:" + priority}, tags...)
		}
		for _, k := range []string{"due", "t"} {
			if v, ok := meta[k]; ok && !todoDate.MatchString(v) {
				unsupported = append(unsupported, importUnsupportedObj("/"+strconv.Itoa(n)+"/"+k, k+": is not a YYYY-MM-DD date"))
				delete(meta, k)
			}
		}
		t := jsObject{}
		name := strings.Join(title, " ")
		if name == "" {
			name = "Imported task"
		}
		t.set("title", name)
		t.set("description", "")
		if l, ok := meta["list"]; ok && l != "" {
			t.set("column_name", unspace(l))
		} else {
			t.set("column_name", map[bool]string{true: "Done", false: "To Do"}[done])
		}
		t.set("swimlane_name", "Default")
		t.set("tags", tags)
		if v := meta["due"]; v != "" {
			t.set("date_due", v)
		}
		if v := meta["t"]; v != "" {
			t.set("date_started", v)
		}
		if created != "" {
			t.set("date_creation", created)
		}
		if done && completed != "" {
			t.set("date_end", completed)
		}
		tasks = append(tasks, t)
	}
	return importDoc("Imported todo.txt", uniqColumns(tasks), tasks, []any{}, unsupported), nil
}

var (
	twCompact  = regexp.MustCompile(`^(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})Z$`)
	twIgnored  = []string{"id", "urgency", "modified", "mask", "imask", "start"}
	twMapped   = []string{"uuid", "status", "description", "entry", "due", "scheduled", "end", "project", "priority", "tags", "annotations", "depends", "wekanlist", "wekandescription"}
	trailComma = regexp.MustCompile(`,$`)
)

// taskwarriorDate is WeKan's: Taskwarrior's 20260901T120000Z or an ISO
// date, as ISO.
func taskwarriorDate(v any) (string, bool) {
	s, ok := v.(string)
	if !ok || s == "" {
		return "", false
	}
	if m := twCompact.FindStringSubmatch(s); m != nil {
		s = m[1] + "-" + m[2] + "-" + m[3] + "T" + m[4] + ":" + m[5] + ":" + m[6] + "Z"
	}
	t, ok := jsParseDate(s)
	if !ok {
		return "", false
	}
	return isoDate(t), true
}

func twWords(v any) []any {
	var src []any
	switch x := v.(type) {
	case []any:
		src = x
	case string:
		for _, p := range strings.Split(x, ",") {
			src = append(src, p)
		}
	}
	out := []any{}
	for _, it := range src {
		if s := jsTrim(jsStr(it)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func parseTaskwarrior(text string) (jsObject, error) {
	source := jsTrim(text)
	var tasks []any
	switch {
	case source == "":
	case strings.HasPrefix(source, "["):
		v, err := decodeJS([]byte(source))
		if err != nil {
			return nil, err
		}
		a, ok := v.([]any)
		if !ok {
			return nil, errors.New("Taskwarrior export is not a JSON array")
		}
		tasks = a
	default:
		n := 0
		for _, line := range jsLines(source) {
			if jsTrim(line) == "" {
				continue
			}
			n++
			v, err := decodeJS([]byte(trailComma.ReplaceAllString(jsTrim(line), "")))
			if err != nil {
				return nil, errors.New("Taskwarrior line " + strconv.Itoa(n) + " is not a JSON object")
			}
			tasks = append(tasks, v)
		}
	}
	if len(tasks) > 10000 {
		return nil, errors.New("Taskwarrior export has more than 10000 tasks")
	}
	normalized, unsupported := []any{}, []any{}
	for n, task := range tasks {
		at := "/" + strconv.Itoa(n)
		obj, ok := task.(jsObject)
		if !ok {
			unsupported = append(unsupported, importUnsupportedObj(at, "not a task object"))
			continue
		}
		status := jsGet(obj, "status")
		if status == "deleted" {
			unsupported = append(unsupported, importUnsupportedObj(at+"/status", "a deleted task is not imported"))
			continue
		}
		if status == "recurring" {
			unsupported = append(unsupported, importUnsupportedObj(at+"/status", "a recurring template is not imported; its instances are"))
			continue
		}
		for _, kv := range obj {
			if !contains(twMapped, kv.Key) && !contains(twIgnored, kv.Key) {
				unsupported = append(unsupported, importUnsupportedObj(at+"/"+kv.Key, "Taskwarrior attribute "+kv.Key+" has no WeKan field"))
			}
		}
		done := status == "completed"
		column := ""
		if l, ok := jsGet(obj, "wekanlist").(string); ok && jsTrim(l) != "" {
			column = jsTrim(l)
		} else if done {
			column = "Done"
		} else if status == "waiting" {
			column = "Waiting"
		} else if jsTruthy(jsGet(obj, "start")) {
			column = "In Progress"
		} else {
			column = "To Do"
		}
		tags := []any{}
		if p, ok := jsGet(obj, "project").(string); ok && p != "" {
			tags = append(tags, "project:"+p)
		}
		if p, ok := jsGet(obj, "priority").(string); ok && contains([]string{"H", "M", "L"}, p) {
			tags = append(tags, "priority:"+p)
		}
		tags = append(tags, twWords(jsGet(obj, "tags"))...)
		for _, k := range []string{"entry", "due", "scheduled", "end"} {
			if v, def := jsLookup(obj, k); def {
				if _, ok := taskwarriorDate(v); !ok {
					unsupported = append(unsupported, importUnsupportedObj(at+"/"+k, k+" is not a Taskwarrior date"))
				}
			}
		}
		comments := []any{}
		notes, _ := jsGet(obj, "annotations").([]any)
		for _, note := range notes {
			d, ok := jsGet(note, "description").(string)
			if note == nil || !ok || jsTrim(d) == "" {
				continue
			}
			c := jsObject{}
			c.set("text", d)
			if when, ok := taskwarriorDate(jsGet(note, "entry")); ok {
				c.set("date", when)
			}
			comments = append(comments, c)
		}
		deps := []any{}
		for _, ref := range twWords(jsGet(obj, "depends")) {
			d := jsObject{}
			d.set("ref", ref)
			d.set("type", "is-blocked-by")
			deps = append(deps, d)
		}
		t := jsObject{}
		if u, ok := jsGet(obj, "uuid").(string); ok && u != "" {
			t.set("ref", u)
		}
		title := "Imported task"
		if d, ok := jsGet(obj, "description").(string); ok && jsTrim(d) != "" {
			title = jsTrim(d)
		}
		t.set("title", title)
		desc := ""
		if d, ok := jsGet(obj, "wekandescription").(string); ok {
			desc = d
		}
		t.set("description", desc)
		t.set("column_name", column)
		t.set("swimlane_name", "Default")
		t.set("tags", tags)
		if v, ok := taskwarriorDate(jsGet(obj, "due")); ok {
			t.set("date_due", v)
		}
		if v, ok := taskwarriorDate(jsGet(obj, "scheduled")); ok {
			t.set("date_started", v)
		}
		if v, ok := taskwarriorDate(jsGet(obj, "entry")); ok {
			t.set("date_creation", v)
		}
		if v, ok := taskwarriorDate(jsGet(obj, "end")); ok && done {
			t.set("date_end", v)
		}
		if len(comments) > 0 {
			t.set("comments", comments)
		}
		if len(deps) > 0 {
			t.set("dependencies", deps)
		}
		normalized = append(normalized, t)
	}
	return importDoc("Imported Taskwarrior", uniqColumns(normalized), normalized, []any{}, unsupported), nil
}
