package web

import (
	"sort"
	"strconv"
	"strings"
)

// Each tool's file as the one "Kanboard shape" document importGeneral
// writes, as Node.js WeKan's models/lib/externalParsers.js reads it - key for
// key, a missing value missing and a null one null, so testdata/import's
// goldens (WeKan's own parsers' output) hold for Weddy too.

// importParsers are the sources Weddy reads, by WeKan's import source name.
var importParsers = map[string]func(any) (jsObject, error){
	"kanboard": parseKanboard,
}

// jsOrDefined is a || b || ...: the first truthy value, else the last, with
// whether that last one was defined.
func jsOrDefined(o any, keys ...string) (any, bool) {
	var v any
	var def bool
	for _, k := range keys {
		v, def = jsLookup(o, k)
		if jsTruthy(v) {
			return v, true
		}
	}
	return v, def
}

// jsUniq is [...new Set(arr.filter(Boolean))].
func jsUniq(values []any) []any {
	out := []any{}
	for _, v := range values {
		if !jsTruthy(v) {
			continue
		}
		dup := false
		for _, o := range out {
			dup = dup || o == v
		}
		if !dup {
			out = append(out, v)
		}
	}
	return out
}

func importUnsupportedObj(path, reason string) jsObject {
	o := jsObject{}
	o.set("path", path)
	o.set("reason", reason)
	return o
}

// --- Kanboard

var kanboardColors = map[string]string{"yellow": "yellow", "blue": "blue", "green": "green", "purple": "purple", "red": "red",
	"orange": "orange", "grey": "gray", "brown": "saddlebrown", "deep_orange": "crimson", "dark_grey": "black", "pink": "pink",
	"teal": "paleturquoise", "cyan": "sky", "lime": "lime", "light_green": "darkgreen", "amber": "gold"}

var kanboardLinkIDs = map[float64]string{1: "relates to", 2: "blocks", 3: "is blocked by", 4: "duplicates", 5: "is duplicated by",
	6: "is a child of", 7: "is a parent of", 8: "targets milestone", 9: "is a milestone of", 10: "fixes", 11: "is fixed by"}

type kanboardKind struct {
	dependency, parent string
	milestone          bool
}

var kanboardLinkLabels = map[string]kanboardKind{
	"relates to": {dependency: "related-to"}, "blocks": {dependency: "blocks"}, "is blocked by": {dependency: "is-blocked-by"},
	"duplicates": {dependency: "duplicates"}, "is duplicated by": {dependency: "is-duplicated-by"}, "fixes": {dependency: "fixes"},
	"is fixed by": {dependency: "is-fixed-by"}, "targets milestone": {dependency: "related-to", milestone: true},
	"is a milestone of": {dependency: "related-to", milestone: true}, "is a child of": {parent: "other"}, "is a parent of": {parent: "this"},
}

// dependencyPair is DEPENDENCY_PAIR: a dependency's family and its direction
// (0 none, 1 forward, -1 backward).
var dependencyPair = map[string]struct {
	family string
	dir    int
}{"related-to": {"related", 0}, "blocks": {"blocks", 1}, "is-blocked-by": {"blocks", -1}, "duplicates": {"duplicates", 1},
	"is-duplicated-by": {"duplicates", -1}, "fixes": {"fixes", 1}, "is-fixed-by": {"fixes", -1}}

// hasID is hasId(): defined, not null, not ”.
func hasID(v any, defined bool) bool {
	if !defined || v == nil {
		return false
	}
	s, isStr := v.(string)
	return !isStr || s != ""
}

// byID is WeKan's: String(item.id) -> its first non-empty name.
func byID(items any, nameKeys ...string) map[string]string {
	out := map[string]string{}
	list, _ := items.([]any)
	for _, it := range list {
		id, ok := jsLookup(it, "id")
		if it == nil || !ok || id == nil {
			continue
		}
		for _, k := range nameKeys {
			if n, ok := jsGet(it, k).(string); ok && n != "" {
				out[jsStr(id)] = n
				break
			}
		}
	}
	return out
}

func kanboardTags(tags any) []any {
	out := []any{}
	switch x := tags.(type) {
	case []any:
		for _, t := range x {
			var v any = t
			if _, isStr := t.(string); !isStr {
				v = nil
				if t != nil {
					v = jsGet(t, "name")
				}
			}
			if jsTruthy(v) {
				out = append(out, v)
			}
		}
	case jsObject:
		for _, kv := range x {
			if s, ok := kv.Value.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

type kanboardLinkRow struct {
	row   any
	index string
}

func kanboardLinkRows(links any) []kanboardLinkRow {
	var out []kanboardLinkRow
	switch x := links.(type) {
	case []any:
		for n, r := range x {
			out = append(out, kanboardLinkRow{r, strconv.Itoa(n)})
		}
	case jsObject:
		for _, kv := range x {
			rows, _ := kv.Value.([]any)
			for n, r := range rows {
				var row any = r
				if obj, ok := r.(jsObject); ok {
					merged := jsObject{}
					merged.set("label", kv.Key)
					for _, f := range obj {
						merged.set(f.Key, f.Value)
					}
					row = merged
				}
				out = append(out, kanboardLinkRow{row, kv.Key + "/" + strconv.Itoa(n)})
			}
		}
	}
	return out
}

type kanboardDep struct{ ref, typ string }

// kanboardLinks is WeKan's: the parents and dependencies the tasks' Kanboard
// links name, between tasks of this file only.
func kanboardLinks(raw []any, unsupported *[]any) (refs []string, hasRef []bool, parents map[int]string, deps map[int][]kanboardDep) {
	refs, hasRef = make([]string, len(raw)), make([]bool, len(raw))
	indexOfRef := map[string]int{}
	for n, t := range raw {
		if id, ok := jsLookup(t, "id"); t != nil && hasID(id, ok) {
			refs[n], hasRef[n] = jsStr(id), true
			if _, seen := indexOfRef[refs[n]]; !seen {
				indexOfRef[refs[n]] = n
			}
		}
	}
	parents, deps = map[int]string{}, map[int][]kanboardDep{}
	seen := map[string]bool{}
	push := func(path, reason string) { *unsupported = append(*unsupported, importUnsupportedObj(path, reason)) }
	for n, t := range raw {
		links := jsGet(t, "links")
		if t == nil || !jsTruthy(links) {
			continue
		}
		for _, lr := range kanboardLinkRows(links) {
			path := "/tasks/" + strconv.Itoa(n) + "/links/" + lr.index
			row, ok := lr.row.(jsObject)
			if !ok {
				push(path, "not a task link")
				continue
			}
			labelV := jsGet(row, "label")
			if !jsTruthy(labelV) {
				id, _ := jsToNumber(jsGet(row, "link_id"))
				if name, ok := kanboardLinkIDs[id]; ok {
					labelV = name
				} else {
					labelV = ""
				}
			}
			label := strings.ToLower(jsTrim(jsStr(labelV)))
			kind, ok := kanboardLinkLabels[label]
			if !ok {
				shown := jsOr(jsGet(row, "label"), jsGet(row, "link_id"), "")
				push(path, `link type "`+jsStr(shown)+`" has no WeKan equivalent; not imported`)
				continue
			}
			other, hasOther := "", false
			if v, def := jsLookup(row, "opposite_task_id"); hasID(v, def) {
				other, hasOther = jsStr(v), true
			} else if v, def := jsLookup(row, "task_id"); hasID(v, def) {
				other, hasOther = jsStr(v), true
			}
			_, known := indexOfRef[other]
			if !hasOther || !known {
				shown := ""
				if hasOther {
					shown = "#" + other + " "
				}
				push(path, "linked task "+shown+"is not part of this import")
				continue
			}
			if hasRef[n] && other == refs[n] {
				continue
			}
			if kind.parent != "" {
				if kind.parent == "this" && !hasRef[n] {
					push(path, "the task has no id, so its child cannot point at it")
					continue
				}
				child, parent := n, other
				if kind.parent != "other" {
					child, parent = indexOfRef[other], refs[n]
				}
				if existing, has := parents[child]; has {
					if existing != parent {
						push(path, "the child task already has a parent; a WeKan card has only one")
					}
					continue
				}
				parents[child] = parent
				continue
			}
			here := refs[n]
			if !hasRef[n] {
				here = "@" + strconv.Itoa(n)
			}
			pair := dependencyPair[kind.dependency]
			ends := []string{here, other}
			switch pair.dir {
			case 0:
				sort.Strings(ends)
			case -1:
				ends = []string{other, here}
			}
			key := pair.family + ":" + strings.Join(ends, ">")
			if seen[key] {
				continue
			}
			seen[key] = true
			dup := false
			for _, d := range deps[n] {
				dup = dup || d.ref == other
			}
			if dup {
				push(path, "only one link between the same two tasks is kept")
				continue
			}
			deps[n] = append(deps[n], kanboardDep{other, kind.dependency})
			if kind.milestone {
				push(path, `"`+label+`" has no WeKan equivalent; imported as "related to"`)
			}
		}
	}
	return
}

func parseKanboard(data any) (jsObject, error) {
	columnNames := byID(jsGet(data, "columns"), "title", "name")
	swimlaneNames := byID(jsGet(data, "swimlanes"), "name", "title")
	categoryNames := byID(jsGet(data, "categories"), "name")
	unsupported := []any{}
	raw, ok := data.([]any)
	if !ok {
		raw, _ = jsGet(data, "tasks").([]any)
	}
	refs, hasRef, parents, deps := kanboardLinks(raw, &unsupported)
	tasks := []any{}
	for n, task := range raw {
		at := "/tasks/" + strconv.Itoa(n)
		tags := kanboardTags(jsGet(task, "tags"))
		category := jsGet(task, "category_name")
		if !jsTruthy(category) {
			if c, ok := categoryNames[jsStr(jsGetOrUndef(task, "category_id"))]; ok {
				category = c
			}
		}
		if jsTruthy(category) {
			tags = append(tags, category)
		}
		if p, ok := jsToNumber(jsGet(task, "priority")); ok && p > 0 && !isInf(p) {
			tags = append(tags, "priority:"+jsNumber(p))
		}
		subtasks, _ := jsGet(task, "subtasks").([]any)
		if e, ok := jsToNumber(jsGet(task, "time_estimated")); ok && e > 0 && !isInf(e) {
			unsupported = append(unsupported, importUnsupportedObj(at+"/time_estimated", "WeKan cards have no estimate field; map it to a custom field by hand"))
		}
		if files, ok := jsGet(task, "files").([]any); ok && len(files) > 0 {
			unsupported = append(unsupported, importUnsupportedObj(at+"/files", strconv.Itoa(len(files))+" file(s): the API export carries metadata, not file contents"))
		}
		if links, ok := jsGet(task, "external_links").([]any); ok && len(links) > 0 {
			unsupported = append(unsupported, importUnsupportedObj(at+"/external_links", strconv.Itoa(len(links))+" external link(s) are not imported"))
		}
		footer := ""
		if u := jsGet(task, "url"); jsTruthy(u) {
			footer = "Source: " + jsStr(u)
		}
		o := jsObject{}
		if hasRef[n] {
			o.set("ref", refs[n])
		}
		if p, ok := parents[n]; ok {
			o.set("parent_ref", p)
		}
		if d, ok := deps[n]; ok {
			list := []any{}
			for _, x := range d {
				dep := jsObject{}
				dep.set("ref", x.ref)
				dep.set("type", x.typ)
				list = append(list, dep)
			}
			o.set("dependencies", list)
		}
		o.set("title", jsOr(jsGet(task, "title"), "Imported task"))
		var desc []string
		for _, s := range []string{jsOrString(jsGet(task, "description")), footer} {
			if s != "" {
				desc = append(desc, s)
			}
		}
		o.set("description", strings.Join(desc, "\n\n"))
		if v, def := jsOrDefined(task, "column_name", "column_title"); jsTruthy(v) {
			o.set("column_name", v)
		} else if c, ok := columnNames[jsStr(jsGetOrUndef(task, "column_id"))]; ok {
			o.set("column_name", c)
		} else {
			_ = def
		}
		if v := jsGet(task, "swimlane_name"); jsTruthy(v) {
			o.set("swimlane_name", v)
		} else if c, ok := swimlaneNames[jsStr(jsGetOrUndef(task, "swimlane_id"))]; ok {
			o.set("swimlane_name", c)
		}
		for _, f := range [][2]string{{"date_due", "date_due"}, {"date_started", "date_started"}, {"date_end", "date_completed"}, {"date_creation", "date_creation"}} {
			v, def := jsLookup(task, f[1])
			o.setDefined(f[0], v, def)
		}
		active := jsGet(task, "is_active")
		o.set("archived", active == "0" || active == 0.0 || active == false)
		if c, ok := kanboardColors[jsStr(jsGetOrUndef(task, "color_id"))]; ok {
			o.set("color", c)
		}
		v, def := jsLookup(task, "time_spent")
		o.setDefined("spent_hours", v, def)
		v, def = jsLookup(task, "owner_id")
		o.setDefined("owner_id", v, def)
		v, def = jsOrDefined(task, "owner_username", "assignee_username")
		o.setDefined("owner_username", v, def)
		v, def = jsOrDefined(task, "owner_name", "assignee_name")
		o.setDefined("owner_name", v, def)
		if v := jsOr(jsGet(task, "creator_username"), jsGet(task, "creator_name")); jsTruthy(v) {
			o.set("requested_by", v)
		}
		o.set("tags", jsUniq(tags))
		checklists := []any{}
		if len(subtasks) > 0 {
			items := []any{}
			for _, sub := range subtasks {
				it := jsObject{}
				if sub == nil {
					it.set("title", nil)
				} else {
					v, def := jsLookup(sub, "title")
					it.setDefined("title", v, def)
				}
				var status any = jsGetOrUndef(sub, "status")
				if sub == nil {
					status = nil
				}
				it.set("done", jsStr(status) == "2")
				items = append(items, it)
			}
			cl := jsObject{}
			cl.set("title", "Subtasks")
			cl.set("items", items)
			checklists = append(checklists, cl)
		}
		o.set("checklists", checklists)
		comments := []any{}
		list, _ := jsGet(task, "comments").([]any)
		for _, c := range list {
			cm := jsObject{}
			if c == nil {
				cm.set("text", nil)
				cm.set("author", nil)
				cm.set("date", nil)
			} else {
				v, def := jsLookup(c, "comment")
				cm.setDefined("text", v, def)
				v, def = jsOrDefined(c, "username", "name")
				cm.setDefined("author", v, def)
				v, def = jsLookup(c, "date_creation")
				cm.setDefined("date", v, def)
			}
			comments = append(comments, cm)
		}
		o.set("comments", comments)
		tasks = append(tasks, o)
	}
	out := jsObject{}
	board := jsObject{}
	name := jsOr(jsGet(jsGet(data, "board"), "name"), jsGet(jsGet(data, "board"), "title"), jsGet(data, "name"))
	if !jsTruthy(name) {
		name = "Imported Kanboard project"
	}
	board.set("name", name)
	out.set("board", board)
	columns := []any{}
	if list, ok := jsGet(data, "columns").([]any); ok && len(list) > 0 {
		for _, c := range list {
			title := jsOr(jsGet(c, "title"), jsGet(c, "name"))
			if !jsTruthy(title) {
				continue
			}
			col := jsObject{}
			col.set("title", title)
			if n, ok := jsToNumber(jsGet(c, "task_limit")); ok && n > 0 {
				col.set("task_limit", n)
			}
			columns = append(columns, col)
		}
	} else {
		var names []any
		for _, t := range tasks {
			names = append(names, jsGet(t, "column_name"))
		}
		for _, n := range jsUniq(names) {
			col := jsObject{}
			col.set("title", n)
			columns = append(columns, col)
		}
	}
	out.set("columns", columns)
	swimlanes := []any{}
	if list, ok := jsGet(data, "swimlanes").([]any); ok && len(list) > 0 {
		for _, sw := range list {
			n := jsOr(jsGet(sw, "name"), jsGet(sw, "title"))
			if jsTruthy(n) {
				o := jsObject{}
				o.set("name", n)
				swimlanes = append(swimlanes, o)
			}
		}
	} else {
		o := jsObject{}
		o.set("name", "Default")
		swimlanes = append(swimlanes, o)
	}
	out.set("swimlanes", swimlanes)
	out.set("tasks", tasks)
	out.set("warnings", []any{})
	out.set("unsupported", unsupported)
	return out, nil
}

// undefinedValue stands in for JavaScript's undefined where String() of it
// matters: String(undefined) is "undefined".
type undefinedValue struct{}

// jsGetOrUndef is o[k], undefined when o has no k.
func jsGetOrUndef(o any, k string) any {
	if v, ok := jsLookup(o, k); ok {
		return v
	}
	return undefinedValue{}
}

func isInf(f float64) bool { return f > 1.7976931348623157e308 || f < -1.7976931348623157e308 }

// jsOrString is (v || ”) as text.
func jsOrString(v any) string {
	if !jsTruthy(v) {
		return ""
	}
	return jsStr(v)
}
