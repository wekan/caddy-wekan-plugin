package web

import (
	"math"
	"regexp"
	"strconv"
	"strings"
)

// GitLab's issues, Asana's tasks and Zenkit's list, as Node.js WeKan's
// externalParsers.js reads them - checked against WeKan's own parsers
// (TestImportParsersAreWeKans). Their sprints and releases
// (externalScrumPlanning.js's scrumTransfer) are not read: Weddy has no
// sprints yet, so the comparison leaves those keys out.

func init() {
	importParsers["gitlab"] = parseGitlab
	importParsers["asana"] = parseAsana
	importParsers["zenkit"] = parseZenkit
}

// jsRound2 is Math.round(x * 100) / 100 as WeKan writes it: Math.round(v) / 100.
func jsRound(v float64) float64 { return math.Floor(v + 0.5) }

// jsDefinedStr is String(v) when v is defined and not null, else undefined.
func jsRefOf(v any, defined bool) (string, bool) {
	if !defined || v == nil {
		return "", false
	}
	return jsStr(v), true
}

func newTaskList(names ...string) []any {
	out := []any{}
	for _, n := range names {
		o := jsObject{}
		o.set("title", n)
		out = append(out, o)
	}
	return out
}

func defaultSwimlanes() []any {
	o := jsObject{}
	o.set("name", "Default")
	return []any{o}
}

func importDoc(name any, columns, tasks, warnings, unsupported []any) jsObject {
	out := jsObject{}
	b := jsObject{}
	b.set("name", name)
	out.set("board", b)
	out.set("columns", columns)
	out.set("swimlanes", defaultSwimlanes())
	out.set("tasks", tasks)
	out.set("warnings", warnings)
	out.set("unsupported", unsupported)
	return out
}

func uniqColumns(tasks []any) []any {
	var names []any
	for _, t := range tasks {
		names = append(names, jsGet(t, "column_name"))
	}
	out := []any{}
	for _, n := range jsUniq(names) {
		o := jsObject{}
		o.set("title", n)
		out = append(out, o)
	}
	return out
}

// --- GitLab

var gitlabLinkTypes = map[string]string{"relates_to": "related-to", "blocks": "blocks", "is_blocked_by": "is-blocked-by"}

func gitlabUser(u any) any {
	if !jsTruthy(u) {
		return nil
	}
	v := jsOr(jsGet(u, "username"), jsGet(u, "name"))
	if !jsTruthy(v) {
		return nil
	}
	return v
}

func parseGitlab(data any) (jsObject, error) {
	issues, ok := data.([]any)
	if !ok {
		issues, _ = jsOr(jsGet(data, "issues"), []any{}).([]any)
	}
	unsupported, warnings := []any{}, []any{}
	tasks := []any{}
	for n, issue := range issues {
		path := "/" + strconv.Itoa(n)
		tags := []any{}
		labels, _ := jsOr(jsGet(issue, "labels"), []any{}).([]any)
		for _, l := range labels {
			var name any = l
			if _, ok := l.(string); !ok {
				name = nil
				if l != nil {
					name = jsGet(l, "name")
				}
			}
			if jsTruthy(name) {
				tags = append(tags, name)
			}
		}
		tag := func(name string) {
			for _, t := range tags {
				if t == name {
					return
				}
			}
			tags = append(tags, name)
		}
		milestone := jsGet(issue, "milestone")
		if t := jsGet(milestone, "title"); jsTruthy(milestone) && jsTruthy(t) {
			tag("milestone:" + jsStr(t))
		}
		if it := jsGet(issue, "iteration"); jsTruthy(it) && jsTruthy(jsGet(it, "title")) {
			tag("iteration:" + jsStr(jsGet(it, "title")))
		}
		if typ := jsGet(issue, "issue_type"); jsTruthy(typ) && typ != "issue" {
			tags = append(tags, "type:"+jsStr(typ))
		}
		if jsTruthy(jsGet(issue, "confidential")) {
			tags = append(tags, "confidential")
			warnings = append(warnings, path+": confidential in GitLab; on the board, the board's visibility decides who can read it")
		}
		var src []any
		if a, ok := jsGet(issue, "assignees").([]any); ok && len(a) > 0 {
			src = a
		} else if a := jsGet(issue, "assignee"); jsTruthy(a) {
			src = []any{a}
		}
		var assignees []any
		for _, a := range src {
			if u := gitlabUser(a); u != nil {
				assignees = append(assignees, u)
			}
		}
		custom := jsObject{}
		if w, ok := jsGet(issue, "weight").(float64); ok {
			custom.set("Weight", w)
		}
		stats := jsOr(jsGet(issue, "time_stats"), jsObject{})
		if e, ok := jsToNumber(jsGetOrNil(stats, "time_estimate")); ok && e > 0 {
			custom.set("Time estimate (hours)", jsRound(e/36)/100)
		}
		if c := jsGet(issue, "task_completion_status"); jsTruthy(c) {
			if count, ok := jsToNumber(jsGetOrNil(c, "count")); ok && count > 0 {
				done, ok := jsToNumber(jsGetOrNil(c, "completed_count"))
				if !ok || done != done {
					done = 0
				}
				custom.set("Tasks", jsNumber(done)+"/"+jsNumber(count))
			}
		}
		notes, hasNotes := jsGet(issue, "notes").([]any)
		comments := []any{}
		for _, note := range notes {
			body, ok := jsGet(note, "body").(string)
			if note == nil || jsTruthy(jsGet(note, "system")) || !ok || jsTrim(body) == "" {
				continue
			}
			c := jsObject{}
			c.set("text", body)
			if a := gitlabUser(jsGet(note, "author")); a != nil {
				c.set("author", a)
			}
			if a := jsGet(note, "author"); jsTruthy(a) {
				v, def := jsLookup(a, "name")
				c.setDefined("authorName", v, def)
			} else if a != nil || func() bool { _, ok := jsLookup(note, "author"); return ok }() {
				c.set("authorName", a)
			}
			v, def := jsLookup(note, "created_at")
			c.setDefined("date", v, def)
			if id := jsGet(note, "id"); id != nil {
				c.set("externalId", jsStr(id))
			}
			comments = append(comments, c)
		}
		if !hasNotes {
			if count, ok := jsToNumber(jsGetOrNil(issue, "user_notes_count")); ok && count > 0 {
				unsupported = append(unsupported, importUnsupportedObj(path+"/user_notes_count",
					jsStr(jsGet(issue, "user_notes_count"))+" comment(s) exist upstream but were not embedded in this export"))
			}
		}
		deps := []any{}
		links, _ := jsGet(issue, "links").([]any)
		for _, l := range links {
			if l == nil || jsGet(l, "iid") == nil {
				continue
			}
			d := jsObject{}
			d.set("ref", jsStr(jsGet(l, "iid")))
			typ := gitlabLinkTypes[jsStr(jsGetOrUndef(l, "link_type"))]
			if typ == "" {
				typ = "related-to"
			}
			d.set("type", typ)
			deps = append(deps, d)
		}
		if jsTruthy(jsGet(issue, "epic")) || jsTruthy(jsGet(issue, "epic_iid")) {
			unsupported = append(unsupported, importUnsupportedObj(path+"/epic", "epics are group-level in GitLab; the parent epic is not imported"))
		}
		var reference any
		if r := jsGet(issue, "references"); jsTruthy(r) {
			reference = jsGet(r, "full")
		}
		if !jsTruthy(reference) {
			reference = nil
			if iid := jsGet(issue, "iid"); iid != nil {
				reference = "#" + jsStr(iid)
			}
		}
		var footer []string
		if jsTruthy(reference) {
			footer = append(footer, "Source: "+jsStr(reference))
		}
		if u := jsGet(issue, "web_url"); jsTruthy(u) {
			footer = append(footer, jsStr(u))
		}
		var desc []string
		for _, s := range []string{jsOrString(jsGet(issue, "description")), strings.Join(footer, " ")} {
			if s != "" {
				desc = append(desc, s)
			}
		}
		t := jsObject{}
		if iid := jsGet(issue, "iid"); iid != nil {
			t.set("externalId", jsStr(iid))
		} else if id := jsGet(issue, "id"); id != nil {
			t.set("externalId", jsStr(id))
		}
		if iid := jsGet(issue, "iid"); iid != nil {
			t.set("ref", jsStr(iid))
		}
		t.set("title", jsOr(jsGet(issue, "title"), "Imported issue"))
		t.set("description", jsTrim(strings.Join(desc, "\n\n")))
		closed := jsGet(issue, "state") == "closed"
		t.set("column_name", map[bool]string{true: "Closed", false: "Open"}[closed])
		t.set("swimlane_name", "Default")
		if d := jsGet(issue, "due_date"); jsTruthy(d) {
			t.set("date_due", d)
		} else if jsTruthy(milestone) && jsTruthy(jsGet(milestone, "due_date")) {
			t.set("date_due", jsGet(milestone, "due_date"))
		}
		if c := jsGet(issue, "created_at"); jsTruthy(c) {
			t.set("date_creation", c)
		}
		if c := jsGet(issue, "closed_at"); closed && jsTruthy(c) {
			t.set("date_end", c)
		}
		if len(assignees) > 0 {
			t.set("owner_username", assignees[0])
			t.set("assignees", assignees[1:])
		} else {
			t.set("assignees", []any{})
		}
		if a := gitlabUser(jsGet(issue, "author")); a != nil {
			t.set("requested_by", a)
		}
		if spent, ok := jsToNumber(jsGetOrNil(stats, "total_time_spent")); ok && spent > 0 {
			t.set("spent_hours", jsRound(spent/36)/100)
		}
		t.set("tags", tags)
		if len(custom) > 0 {
			t.set("custom_fields", custom)
		}
		if len(comments) > 0 {
			t.set("comments", comments)
		}
		if len(deps) > 0 {
			t.set("dependencies", deps)
		}
		if att := issueAttachments(issue); len(att) > 0 {
			t.set("attachments", att)
		}
		tasks = append(tasks, t)
	}
	return importDoc("Imported GitLab issues", newTaskList("Open", "Closed"), tasks, warnings, unsupported), nil
}

// --- Asana

func asanaCustomValue(f any) (any, bool) {
	if _, ok := f.(jsObject); !ok {
		return nil, false
	}
	if n, ok := jsGet(f, "number_value").(float64); ok {
		return n, true
	}
	if s, ok := jsGet(f, "text_value").(string); ok && s != "" {
		return s, true
	}
	if e := jsGet(f, "enum_value"); jsTruthy(e) && jsTruthy(jsGet(e, "name")) {
		return jsGet(e, "name"), true
	}
	if m, ok := jsGet(f, "multi_enum_values").([]any); ok && len(m) > 0 {
		out := []any{}
		for _, v := range m {
			if n := jsGet(v, "name"); v != nil && jsTruthy(n) {
				out = append(out, n)
			}
		}
		return out, true
	}
	if d := jsGet(f, "date_value"); jsTruthy(d) && (jsTruthy(jsGet(d, "date_time")) || jsTruthy(jsGet(d, "date"))) {
		return jsOr(jsGet(d, "date_time"), jsGet(d, "date")), true
	}
	if p, ok := jsGet(f, "people_value").([]any); ok && len(p) > 0 {
		out := []any{}
		for _, v := range p {
			if n := jsOr(jsGet(v, "name"), jsGet(v, "email")); v != nil && jsTruthy(n) {
				out = append(out, n)
			}
		}
		return out, true
	}
	if s, ok := jsGet(f, "display_value").(string); ok && s != "" {
		return s, true
	}
	return nil, false
}

func asanaUser(u any) any {
	if !jsTruthy(u) {
		return nil
	}
	v := jsOr(jsGet(u, "email"), jsGet(u, "name"), jsGet(u, "gid"))
	if !jsTruthy(v) {
		return nil
	}
	return v
}

func parseAsana(data any) (jsObject, error) {
	items, ok := data.([]any)
	if !ok {
		items, _ = jsGet(data, "data").([]any)
		if items == nil {
			items = []any{}
		}
	}
	listed := map[string]bool{}
	for _, t := range items {
		if g := jsGet(t, "gid"); t != nil && jsTruthy(g) {
			listed[jsStr(g)] = true
		}
	}
	unsupported := []any{}
	tasks := []any{}
	for n, t := range items {
		at := "/data/" + strconv.Itoa(n)
		var section any
		if m, ok := jsGet(t, "memberships").([]any); ok && len(m) > 0 && jsTruthy(m[0]) && jsTruthy(jsGet(m[0], "section")) {
			section = jsGet(jsGet(m[0], "section"), "name")
		}
		if !jsTruthy(section) {
			section = map[bool]string{true: "Done", false: "In Progress"}[jsTruthy(jsGet(t, "completed"))]
		}
		custom := jsObject{}
		fields, _ := jsGet(t, "custom_fields").([]any)
		for _, f := range fields {
			if v, ok := asanaCustomValue(f); ok && f != nil && jsTruthy(jsGet(f, "name")) {
				custom.set(jsStr(jsGet(f, "name")), v)
			}
		}
		var embedded []any
		subs, _ := jsGet(t, "subtasks").([]any)
		for _, sub := range subs {
			if g := jsGet(sub, "gid"); sub != nil && jsTruthy(g) && listed[jsStr(g)] {
				continue
			}
			embedded = append(embedded, sub)
		}
		var followers []any
		fl, _ := jsGet(t, "followers").([]any)
		for _, f := range fl {
			if u := asanaUser(f); u != nil {
				followers = append(followers, u)
			}
		}
		var files []any
		atts, _ := jsGet(t, "attachments").([]any)
		for i, f := range atts {
			if _, ok := f.(jsObject); !ok {
				continue
			}
			name := jsStr(jsOr(jsGet(f, "name"), "attachment"))
			if s, ok := jsGet(f, "file").(string); ok && s != "" {
				o := jsObject{}
				o.set("name", name)
				o.set("file", s)
				files = append(files, o)
			} else if h := jsGet(f, "host"); jsTruthy(h) && h != "asana" {
				unsupported = append(unsupported, importUnsupportedObj(at+"/attachments/"+strconv.Itoa(i), name+": a link to "+jsStr(h)+", not a file stored in Asana"))
			} else if g := jsGet(f, "gid"); g != nil {
				o := jsObject{}
				o.set("name", name)
				o.set("asanaGid", jsStr(g))
				if u, ok := jsGet(f, "download_url").(string); ok {
					o.set("url", u)
				}
				files = append(files, o)
			} else {
				unsupported = append(unsupported, importUnsupportedObj(at+"/attachments/"+strconv.Itoa(i), name+": no file and no Asana id in the export"))
			}
		}
		footer := ""
		if p := jsGet(t, "permalink_url"); jsTruthy(p) {
			footer = "Source: " + jsStr(p)
		}
		o := jsObject{}
		if len(files) > 0 {
			o.set("attachments", files)
		}
		if g, def := jsLookup(t, "gid"); def && g != nil {
			o.set("ref", jsStr(g))
		}
		if p := jsGet(t, "parent"); jsTruthy(p) {
			if g, def := jsLookup(p, "gid"); def {
				o.set("parent_ref", jsStr(g))
			}
		}
		deps := []any{}
		dl, _ := jsGet(t, "dependencies").([]any)
		for _, d := range dl {
			if g, def := jsLookup(d, "gid"); d != nil && def {
				x := jsObject{}
				x.set("ref", jsStr(g))
				x.set("type", "is-blocked-by")
				deps = append(deps, x)
			}
		}
		o.set("dependencies", deps)
		o.set("title", jsOr(jsGet(t, "name"), "Imported task"))
		var desc []string
		for _, s := range []string{jsOrString(jsGet(t, "notes")), footer} {
			if s != "" {
				desc = append(desc, s)
			}
		}
		o.set("description", strings.Join(desc, "\n\n"))
		o.set("column_name", section)
		o.set("swimlane_name", "Default")
		v, def := jsOrDefined(t, "due_at", "due_on")
		o.setDefined("date_due", v, def)
		v, def = jsOrDefined(t, "start_at", "start_on")
		o.setDefined("date_started", v, def)
		if jsTruthy(jsGet(t, "completed")) {
			v, def = jsLookup(t, "completed_at")
			o.setDefined("date_end", v, def)
		}
		v, def = jsLookup(t, "created_at")
		o.setDefined("date_creation", v, def)
		if u := asanaUser(jsGet(t, "assignee")); u != nil {
			o.set("owner_username", u)
		}
		tags := []any{}
		tl, _ := jsOr(jsGet(t, "tags"), []any{}).([]any)
		for _, tag := range tl {
			var name any = tag
			if _, ok := tag.(string); !ok {
				name = nil
				if tag != nil {
					name = jsGet(tag, "name")
				}
			}
			if jsTruthy(name) {
				tags = append(tags, name)
			}
		}
		o.set("tags", tags)
		o.set("custom_fields", custom)
		if len(followers) > 0 {
			o.set("watchers", followers)
		}
		checklists := []any{}
		if len(embedded) > 0 {
			items := []any{}
			for _, sub := range embedded {
				it := jsObject{}
				if sub == nil {
					it.set("title", nil)
				} else {
					v, def := jsLookup(sub, "name")
					it.setDefined("title", v, def)
				}
				it.set("done", jsTruthy(jsGet(sub, "completed")))
				items = append(items, it)
			}
			cl := jsObject{}
			cl.set("title", "Subtasks")
			cl.set("items", items)
			checklists = append(checklists, cl)
		}
		o.set("checklists", checklists)
		comments := []any{}
		stories, _ := jsGet(t, "stories").([]any)
		for _, st := range stories {
			if st == nil || jsGet(st, "resource_subtype") != "comment_added" {
				continue
			}
			c := jsObject{}
			v, def := jsLookup(st, "text")
			c.setDefined("text", v, def)
			if u := asanaUser(jsGet(st, "created_by")); u != nil {
				c.set("author", u)
			}
			v, def = jsLookup(st, "created_at")
			c.setDefined("date", v, def)
			comments = append(comments, c)
		}
		o.set("comments", comments)
		tasks = append(tasks, o)
	}
	var name any
	if p := jsGet(data, "project"); jsTruthy(p) {
		name = jsGet(p, "name")
	}
	if !jsTruthy(name) && len(items) > 0 && jsTruthy(items[0]) {
		if m, ok := jsGet(items[0], "memberships").([]any); ok && len(m) > 0 && jsTruthy(m[0]) {
			if p := jsGet(m[0], "project"); jsTruthy(p) {
				name = jsGet(p, "name")
			}
		}
	}
	if !jsTruthy(name) {
		name = "Imported Asana project"
	}
	return importDoc(name, uniqColumns(tasks), tasks, []any{}, unsupported), nil
}

// --- Zenkit

const (
	zkText, zkNumber, zkURL, zkDate, zkCategories, zkFormula = 1, 2, 3, 4, 6, 7
	zkPersons, zkFiles, zkReferences, zkHierarchy            = 14, 15, 16, 17
	zkSubEntries, zkDependencies                             = 18, 19
)

var zenkitAdapterKeys = []string{"title", "name", "description", "notes", "stage_name", "stageName", "list",
	"due", "dueDate", "due_date", "tags", "assignee", "assignees", "id", "uuid", "parent_id", "parentId",
	"fields", "comments", "checklists", "created_at", "start"}

func zenkitNames(list any, key string) []any {
	out := []any{}
	l, _ := list.([]any)
	for _, v := range l {
		if s, ok := jsGet(v, key).(string); v != nil && ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func zenkitChecklists(cls any) []any {
	out := []any{}
	list, _ := cls.([]any)
	for _, c := range list {
		cl := jsObject{}
		if c == nil {
			cl.set("title", nil)
		} else {
			v, def := jsLookup(c, "name")
			cl.setDefined("title", v, def)
		}
		items := []any{}
		il, _ := jsGet(c, "items").([]any)
		for _, i := range il {
			it := jsObject{}
			if i == nil {
				it.set("title", nil)
			} else {
				v, def := jsOrDefined(i, "text", "title")
				it.setDefined("title", v, def)
			}
			it.set("done", i != nil && (jsTruthy(jsGet(i, "checked")) || jsTruthy(jsGet(i, "done"))))
			items = append(items, it)
		}
		cl.set("items", items)
		out = append(out, cl)
	}
	return out
}

func zenkitKind(e any) float64 {
	n, _ := jsToNumber(jsGetOrNil(e, "elementcategory"))
	return n
}

func parseZenkitAPI(data any) (jsObject, error) {
	var elements []any
	for _, e := range jsGet(data, "elements").([]any) {
		if _, ok := jsGet(e, "uuid").(string); e != nil && ok {
			elements = append(elements, e)
		}
	}
	named := func(cat float64, re *regexp.Regexp) any {
		for _, e := range elements {
			if zenkitKind(e) == cat && re.MatchString(jsOrString(jsGet(e, "name"))) {
				return e
			}
		}
		return nil
	}
	var categories, people, deps []any
	var descField, hierarchy any
	for _, e := range elements {
		switch zenkitKind(e) {
		case zkCategories:
			categories = append(categories, e)
		case zkPersons:
			people = append(people, e)
		case zkDependencies:
			deps = append(deps, e)
		}
		if descField == nil && zenkitKind(e) == zkText && !jsTruthy(jsGet(e, "isPrimary")) &&
			regexp.MustCompile(`(?i)description|notes?|details`).MatchString(jsOrString(jsGet(e, "name"))) {
			descField = e
		}
		if hierarchy == nil && zenkitKind(e) == zkHierarchy {
			hierarchy = e
		}
	}
	stage := named(zkCategories, regexp.MustCompile(`(?i)stage|status|state|column`))
	if stage == nil && len(categories) > 0 {
		stage = categories[0]
	}
	due := named(zkDate, regexp.MustCompile(`(?i)due|deadline|end`))
	start := named(zkDate, regexp.MustCompile(`(?i)start|begin`))
	unsupported, warnings := []any{}, []any{}
	src, ok := jsGet(data, "entries").([]any)
	if !ok {
		src, _ = jsGet(data, "listEntries").([]any)
	}
	var entries, live []any
	for _, e := range src {
		if jsTruthy(e) {
			entries = append(entries, e)
		}
	}
	for _, e := range entries {
		if !jsTruthy(jsGet(e, "deprecated_at")) {
			live = append(live, e)
		}
	}
	if len(live) != len(entries) {
		warnings = append(warnings, importUnsupportedObj("/entries", strconv.Itoa(len(entries)-len(live))+" deleted entr(ies) skipped"))
	}
	sortOrder := func(e any) float64 {
		n, ok := jsToNumber(jsGetOrNil(e, "sortOrder"))
		if !ok || n != n {
			return 0
		}
		return n
	}
	live = append([]any{}, live...)
	stableSortBy(live, sortOrder)
	reported := map[string]bool{}
	report := func(e any, reason string) {
		u := jsStr(jsGet(e, "uuid"))
		if reported[u] {
			return
		}
		reported[u] = true
		unsupported = append(unsupported, importUnsupportedObj("/elements/"+strconv.Itoa(indexOfItem(elements, e)),
			jsStr(jsOr(jsGet(e, "name"), jsGet(e, "uuid")))+": "+reason))
	}
	is := func(e, other any) bool { return other != nil && sameJS(e, other) }
	tasks := []any{}
	for n, entry := range live {
		value := func(e any, suffix string) any { return jsGetOrUndef(entry, jsStr(jsGet(e, "uuid"))+"_"+suffix) }
		custom := jsObject{}
		var tags []any
		for _, e := range elements {
			if jsTruthy(jsGet(e, "isPrimary")) || is(e, stage) || is(e, descField) || is(e, due) || is(e, start) || is(e, hierarchy) {
				continue
			}
			name := jsStr(jsGetOrUndef(e, "name"))
			switch k := zenkitKind(e); k {
			case zkText, zkURL:
				if s, ok := value(e, "text").(string); ok && s != "" {
					custom.set(name, s)
				}
			case zkNumber:
				if v, ok := value(e, "number").(float64); ok && !isInf(v) {
					custom.set(name, v)
				}
			case zkDate:
				if s, ok := value(e, "date").(string); ok && s != "" {
					custom.set(name, s)
				}
			case zkCategories:
				tags = append(tags, zenkitNames(value(e, "categories_sort"), "name")...)
			case zkPersons, zkDependencies, zkSubEntries:
			case zkFiles:
				report(e, "files are not part of the export")
			case zkReferences:
				report(e, "references to other lists are not imported")
			case zkFormula:
				report(e, "formula results are computed by Zenkit and not imported")
			default:
				if k >= 8 && k <= 13 && k == math.Trunc(k) {
					break
				}
				report(e, "this field type has no documented value format")
			}
		}
		var persons []any
		for _, p := range people {
			persons = append(persons, zenkitNames(value(p, "persons_sort"), "displayname")...)
		}
		var parent any
		hasParent := false
		if hierarchy != nil {
			if r := zenkitNames(value(hierarchy, "references_sort"), "uuid"); len(r) > 0 {
				parent, hasParent = r[0], true
			}
		}
		if c, ok := jsToNumber(jsGetOrNil(entry, "comment_count")); ok && c > 0 && c == c {
			unsupported = append(unsupported, importUnsupportedObj("/entries/"+strconv.Itoa(n)+"/comment_count",
				jsNumber(c)+" comment(s) are fetched separately from Zenkit"))
		}
		t := jsObject{}
		v, def := jsLookup(entry, "uuid")
		t.setDefined("ref", v, def)
		if hasParent {
			t.set("parent_ref", parent)
		}
		dl := []any{}
		for _, d := range deps {
			for _, ref := range zenkitNames(value(d, "references_sort"), "uuid") {
				x := jsObject{}
				x.set("ref", ref)
				x.set("type", "related-to")
				dl = append(dl, x)
			}
		}
		t.set("dependencies", dl)
		t.set("title", jsOr(jsGet(entry, "displayString"), "Imported item"))
		desc := any("")
		if descField != nil {
			if d := value(descField, "text"); jsTruthy(d) {
				desc = d
			}
		}
		t.set("description", desc)
		column := any("Inbox")
		if stage != nil {
			if c := zenkitNames(value(stage, "categories_sort"), "name"); len(c) > 0 {
				column = c[0]
			}
		}
		t.set("column_name", column)
		t.set("swimlane_name", "Default")
		if due != nil {
			if d, ok := value(due, "date").(undefinedValue); !ok {
				t.set("date_due", value(due, "date"))
			} else {
				_ = d
			}
		}
		if start != nil {
			if _, ok := value(start, "date").(undefinedValue); !ok {
				t.set("date_started", value(start, "date"))
			}
		}
		v, def = jsLookup(entry, "created_at")
		t.setDefined("date_creation", v, def)
		if len(persons) > 0 {
			t.set("owner_username", persons[0])
			t.set("assignees", persons[1:])
		} else {
			t.set("assignees", []any{})
		}
		if b := jsGet(entry, "created_by_displayname"); jsTruthy(b) {
			t.set("requested_by", b)
		}
		t.set("tags", jsUniq(tags))
		t.set("custom_fields", custom)
		t.set("checklists", zenkitChecklists(jsGet(entry, "checklists")))
		tasks = append(tasks, t)
	}
	var stageNames []any
	if stage != nil {
		if ed := jsGet(stage, "elementData"); jsTruthy(ed) {
			if _, ok := jsGet(ed, "predefinedCategories").([]any); ok {
				stageNames = zenkitNames(jsGet(ed, "predefinedCategories"), "name")
			}
		}
	}
	for _, t := range tasks {
		stageNames = append(stageNames, jsGet(t, "column_name"))
	}
	columns := []any{}
	for _, c := range jsUniq(stageNames) {
		o := jsObject{}
		o.set("title", c)
		columns = append(columns, o)
	}
	var name any
	if l := jsGet(data, "list"); jsTruthy(l) {
		name = jsGet(l, "name")
	}
	name = jsOr(name, jsGet(data, "title"), jsGet(data, "name"), "Imported ZenKit list")
	return importDoc(name, columns, tasks, warnings, unsupported), nil
}

// stableSortBy is Array.prototype.sort((a, b) => key(a) - key(b)), stable.
func stableSortBy(items []any, key func(any) float64) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && key(items[j-1])-key(items[j]) > 0; j-- {
			items[j-1], items[j] = items[j], items[j-1]
		}
	}
}

func parseZenkit(data any) (jsObject, error) {
	if obj, ok := data.(jsObject); ok {
		_, el := jsGet(obj, "elements").([]any)
		_, en := jsGet(obj, "entries").([]any)
		_, le := jsGet(obj, "listEntries").([]any)
		if el && (en || le) {
			return parseZenkitAPI(data)
		}
	}
	items, ok := data.([]any)
	if !ok {
		items, _ = jsOr(jsGet(data, "items"), []any{}).([]any)
	}
	stages, _ := jsOr(jsGet(data, "stages"), []any{}).([]any)
	unsupported := []any{}
	tasks := []any{}
	for n, t := range items {
		var unknown []string
		if obj, ok := t.(jsObject); ok {
			for _, kv := range obj {
				if !contains(zenkitAdapterKeys, kv.Key) {
					unknown = append(unknown, kv.Key)
				}
			}
		}
		if len(unknown) > 0 {
			if len(unknown) > 10 {
				unknown = unknown[:10]
			}
			unsupported = append(unsupported, importUnsupportedObj("/items/"+strconv.Itoa(n), "unrecognized field(s): "+strings.Join(unknown, ", ")))
		}
		src, ok := jsGet(t, "assignees").([]any)
		if !ok {
			src = []any{jsGet(t, "assignee")}
		}
		var people []any
		for _, a := range src {
			var v any = a
			if _, ok := a.(string); !ok {
				v = nil
				if a != nil {
					v = jsOr(jsGet(a, "email"), jsGet(a, "name"))
				}
			}
			if jsTruthy(v) {
				people = append(people, v)
			}
		}
		id, idDef := jsLookup(t, "id")
		if !idDef || id == nil {
			id, idDef = jsLookup(t, "uuid")
		}
		parent, pDef := jsLookup(t, "parent_id")
		if !pDef || parent == nil {
			parent, pDef = jsLookup(t, "parentId")
		}
		o := jsObject{}
		if r, ok := jsRefOf(id, idDef); ok {
			o.set("ref", r)
		}
		if r, ok := jsRefOf(parent, pDef); ok {
			o.set("parent_ref", r)
		}
		o.set("title", jsOr(jsGet(t, "title"), jsGet(t, "name"), "Imported item"))
		o.set("description", jsOr(jsGet(t, "description"), jsGet(t, "notes"), ""))
		o.set("column_name", jsOr(jsGet(t, "stage_name"), jsGet(t, "stageName"), jsGet(t, "list"), "Inbox"))
		o.set("swimlane_name", "Default")
		v, def := jsOrDefined(t, "due", "dueDate", "due_date")
		o.setDefined("date_due", v, def)
		v, def = jsLookup(t, "start")
		o.setDefined("date_started", v, def)
		v, def = jsLookup(t, "created_at")
		o.setDefined("date_creation", v, def)
		if len(people) > 0 {
			o.set("owner_username", people[0])
			o.set("assignees", people[1:])
		} else {
			o.set("assignees", []any{})
		}
		tags := []any{}
		if tl, ok := jsGet(t, "tags").([]any); ok {
			for _, tag := range tl {
				var name any = tag
				if _, ok := tag.(string); !ok {
					name = nil
					if tag != nil {
						name = jsGet(tag, "name")
					}
				}
				if jsTruthy(name) {
					tags = append(tags, name)
				}
			}
		}
		o.set("tags", tags)
		if f, ok := jsGet(t, "fields").(jsObject); ok {
			o.set("custom_fields", f)
		} else {
			o.set("custom_fields", jsObject{})
		}
		o.set("checklists", zenkitChecklists(jsGet(t, "checklists")))
		comments := []any{}
		cl, _ := jsGet(t, "comments").([]any)
		for _, c := range cl {
			cm := jsObject{}
			if s, ok := c.(string); ok {
				cm.set("text", s)
			} else if c == nil {
				cm.set("text", nil)
			} else {
				v, def := jsOrDefined(c, "text", "message")
				cm.setDefined("text", v, def)
			}
			if _, isObj := c.(jsObject); isObj {
				v, def := jsOrDefined(c, "author", "user")
				cm.setDefined("author", v, def)
				v, def = jsOrDefined(c, "date", "created_at")
				cm.setDefined("date", v, def)
			}
			comments = append(comments, cm)
		}
		o.set("comments", comments)
		tasks = append(tasks, o)
	}
	var columns []any
	if len(stages) > 0 {
		columns = []any{}
		for _, s := range stages {
			c := jsObject{}
			v, def := jsOrDefined(s, "name", "title")
			c.setDefined("title", v, def)
			columns = append(columns, c)
		}
	} else {
		columns = uniqColumns(tasks)
	}
	return importDoc(jsOr(jsGet(data, "title"), jsGet(data, "name"), "Imported ZenKit list"), columns, tasks, []any{}, unsupported), nil
}
