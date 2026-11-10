package web

import (
	"regexp"
	"strconv"
	"strings"
)

// OpenProject's work-packages collection (GET /api/v3/work_packages), as
// Node.js WeKan's externalParsers.js parseOpenProject reads it - checked
// against WeKan's own parser. Its versions and sprints (scrumTransfer) are
// not read: Weddy has no sprints yet.

func init() { importParsers["openproject"] = parseOpenProject }

var (
	isoDuration       = regexp.MustCompile(`^P(?:(\d+(?:\.\d+)?)D)?(?:T(?:(\d+(?:\.\d+)?)H)?(?:(\d+(?:\.\d+)?)M)?(?:(\d+(?:\.\d+)?)S)?)?$`)
	customFieldKey    = regexp.MustCompile(`^customField\d+$`)
	halHrefID         = regexp.MustCompile(`/(\d+)/?$`)
	openProjectRelate = map[string]string{"blocks": "blocks", "blocked": "is-blocked-by", "duplicates": "duplicates", "duplicated": "is-duplicated-by"}
)

// isoDurationHours is WeKan's: PT1H30M and P2D as hours, to two decimals.
func isoDurationHours(v any) (float64, bool) {
	s, ok := v.(string)
	if !ok || s == "P" || s == "PT" {
		return 0, false
	}
	m := isoDuration.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	num := func(x string) float64 {
		if x == "" {
			return 0
		}
		f, _ := strconv.ParseFloat(x, 64)
		return f
	}
	return jsRound((num(m[1])*24+num(m[2])+num(m[3])/60+num(m[4])/3600)*100) / 100, true
}

// halTitle is WeKan's: a link's title, or a list's titles joined.
func halTitle(link any) (string, bool) {
	if list, ok := link.([]any); ok {
		var parts []string
		for _, l := range list {
			if t, ok := halTitle(l); ok {
				parts = append(parts, t)
			}
		}
		if len(parts) == 0 {
			return "", false
		}
		return strings.Join(parts, ", "), true
	}
	if t, ok := jsGet(link, "title").(string); ok && t != "" {
		return t, true
	}
	return "", false
}

func halID(link any) (string, bool) {
	href, ok := jsGet(link, "href").(string)
	if !ok {
		return "", false
	}
	if m := halHrefID.FindStringSubmatch(href); m != nil {
		return m[1], true
	}
	return "", false
}

func embeddedElements(owner any, key string) []any {
	v := jsGet(jsGet(owner, "_embedded"), key)
	if a, ok := v.([]any); ok {
		return a
	}
	if a, ok := jsGet(v, "elements").([]any); ok && jsTruthy(v) {
		return a
	}
	return []any{}
}

func parseOpenProject(data any) (jsObject, error) {
	var elements []any
	if a, ok := jsGet(jsGet(data, "_embedded"), "elements").([]any); ok {
		elements = a
	} else if a, ok := jsGet(data, "elements").([]any); ok {
		elements = a
	} else if a, ok := data.([]any); ok {
		elements = a
	}
	fieldNames := map[string]string{}
	schemas := append([]any{}, embeddedElements(data, "schemas")...)
	for _, wp := range elements {
		if s := jsGet(jsGet(wp, "_embedded"), "schema"); jsTruthy(s) {
			schemas = append(schemas, s)
		}
	}
	for _, schema := range schemas {
		obj, _ := schema.(jsObject)
		for _, kv := range obj {
			if customFieldKey.MatchString(kv.Key) {
				if n, ok := jsGet(kv.Value, "name").(string); ok && jsTruthy(kv.Value) {
					fieldNames[kv.Key] = n
				}
			}
		}
	}
	tasks := []any{}
	for _, wp := range elements {
		links := jsOr(jsGet(wp, "_links"), jsObject{})
		custom := jsObject{}
		addField := func(key string, v any, ok bool) {
			if !ok || v == nil || v == "" {
				return
			}
			name := key
			if n, ok := fieldNames[key]; ok {
				name = n
			}
			custom.set(name, v)
		}
		obj, _ := wp.(jsObject)
		for _, kv := range obj {
			if !customFieldKey.MatchString(kv.Key) {
				continue
			}
			if o, isObj := kv.Value.(jsObject); isObj {
				v, def := jsLookup(o, "raw")
				addField(kv.Key, v, def)
			} else {
				addField(kv.Key, kv.Value, true)
			}
		}
		lobj, _ := links.(jsObject)
		for _, kv := range lobj {
			if customFieldKey.MatchString(kv.Key) {
				t, ok := halTitle(kv.Value)
				addField(kv.Key, t, ok)
			}
		}
		if e, ok := isoDurationHours(jsGet(wp, "estimatedTime")); ok {
			custom.set("Estimated time (hours)", e)
		}
		if p, ok := jsGet(wp, "percentageDone").(float64); ok {
			custom.set("Progress (%)", p)
		}
		if sp, ok := jsGet(wp, "storyPoints").(float64); ok && !isInf(sp) && sp >= 0 {
			custom.set("Story points", sp)
		}
		id, hasID := "", false
		if v := jsGet(wp, "id"); v != nil {
			id, hasID = jsStr(v), true
		}
		deps := []any{}
		for _, rel := range embeddedElements(wp, "relations") {
			rl := jsOr(jsGet(rel, "_links"), jsObject{})
			from, fok := halID(jsGet(rl, "from"))
			to, tok := halID(jsGet(rl, "to"))
			if !hasID || !fok || from != id || !tok {
				continue
			}
			d := jsObject{}
			d.set("ref", to)
			typ := openProjectRelate[jsStr(jsGetOrUndef(rel, "type"))]
			if typ == "" {
				typ = "related-to"
			}
			d.set("type", typ)
			deps = append(deps, d)
		}
		var watchers []any
		for _, w := range embeddedElements(wp, "watchers") {
			if n := jsOr(jsGet(w, "name"), jsGet(w, "login")); w != nil && jsTruthy(n) {
				watchers = append(watchers, n)
			}
		}
		var files []any
		for _, f := range embeddedElements(wp, "attachments") {
			fl := jsOr(jsGet(f, "_links"), jsObject{})
			href := jsOr(jsGet(jsGet(fl, "downloadLocation"), "href"), jsGet(jsGet(fl, "staticDownloadLocation"), "href"))
			hs, ok := href.(string)
			if !ok || hs == "" {
				continue
			}
			o := jsObject{}
			o.set("name", jsStr(jsOr(jsGet(f, "fileName"), jsGet(f, "title"), "attachment")))
			if httpURL.MatchString(hs) {
				o.set("url", hs)
			} else {
				o.set("path", hs)
			}
			files = append(files, o)
		}
		var tags []any
		if t, ok := halTitle(jsGet(links, "type")); ok {
			tags = append(tags, t)
		}
		if t, ok := halTitle(jsGet(links, "priority")); ok && jsTruthy(jsGet(links, "priority")) {
			tags = append(tags, "priority:"+t)
		}
		if t, ok := halTitle(jsGet(links, "category")); ok {
			tags = append(tags, t)
		}
		if t, ok := halTitle(jsGet(links, "version")); ok && jsTruthy(jsGet(links, "version")) {
			tags = append(tags, "version:"+t)
		}
		if tags == nil {
			tags = []any{}
		}
		t := jsObject{}
		if len(files) > 0 {
			t.set("attachments", files)
		}
		if hasID {
			t.set("ref", id)
		}
		if p, ok := halID(jsGet(links, "parent")); ok {
			t.set("parent_ref", p)
		}
		t.set("dependencies", deps)
		t.set("title", jsOr(jsGet(wp, "subject"), jsGet(wp, "name"), "Imported work package"))
		desc := any("")
		if d := jsGet(wp, "description"); jsTruthy(d) {
			if v := jsOr(jsGet(d, "raw"), jsGet(d, "html")); jsTruthy(v) {
				desc = v
			}
		}
		t.set("description", desc)
		if s, ok := halTitle(jsGet(links, "status")); ok {
			t.set("column_name", s)
		} else {
			t.set("column_name", jsOr(jsGet(wp, "status"), "Imported"))
		}
		t.set("swimlane_name", "Default")
		v, def := jsOrDefined(wp, "dueDate", "due_date", "date")
		t.setDefined("date_due", v, def)
		v, def = jsLookup(wp, "startDate")
		t.setDefined("date_started", v, def)
		v, def = jsLookup(wp, "createdAt")
		t.setDefined("date_creation", v, def)
		if h, ok := isoDurationHours(jsGet(wp, "spentTime")); ok {
			t.set("spent_hours", h)
		}
		if a, ok := halTitle(jsGet(links, "assignee")); ok {
			t.set("owner_username", a)
		}
		assignees := []any{}
		if r, ok := halTitle(jsGet(links, "responsible")); ok {
			assignees = append(assignees, r)
		}
		t.set("assignees", assignees)
		if a, ok := halTitle(jsGet(links, "author")); ok {
			t.set("requested_by", a)
		}
		t.set("tags", tags)
		t.set("custom_fields", custom)
		if len(watchers) > 0 {
			t.set("watchers", watchers)
		}
		comments := []any{}
		for _, a := range embeddedElements(wp, "activities") {
			if a == nil || jsGet(a, "_type") != "Activity::Comment" || !jsTruthy(jsGet(a, "comment")) || !jsTruthy(jsGet(jsGet(a, "comment"), "raw")) {
				continue
			}
			c := jsObject{}
			c.set("text", jsGet(jsGet(a, "comment"), "raw"))
			if au, ok := halTitle(jsGet(jsGet(a, "_links"), "user")); ok {
				c.set("author", au)
			}
			v, def := jsLookup(a, "createdAt")
			c.setDefined("date", v, def)
			comments = append(comments, c)
		}
		t.set("comments", comments)
		tasks = append(tasks, t)
	}
	var name any
	if self := jsGet(jsGet(data, "_links"), "self"); jsTruthy(self) {
		name = jsGet(self, "title")
	}
	if !jsTruthy(name) && len(elements) > 0 {
		if p, ok := halTitle(jsGet(jsGet(elements[0], "_links"), "project")); ok {
			name = p
		}
	}
	if !jsTruthy(name) {
		name = "Imported OpenProject"
	}
	return importDoc(name, uniqColumns(tasks), tasks, []any{}, []any{}), nil
}
