package web

import (
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"strconv"
	"time"
)

// A Taiga project dump: Node.js WeKan's parseTaiga
// (models/lib/taigaFormat.js) - user story statuses as lists, its swimlanes,
// stories with their points, epics, tags and their colors, comments, custom
// attributes and people; tasks as subtask cards beside their story; epics
// and issues in swimlanes of their own. Its Scrum planning waits for
// Weddy's sprints.

func init() { importParsers["taiga"] = parseTaiga }

const maxTaigaItems = 50000

var (
	taigaLabelHex = []struct{ name, hex string }{
		{"white", "#ffffff"}, {"green", "#3cb500"}, {"yellow", "#fad900"}, {"orange", "#ff9f19"}, {"red", "#eb4646"},
		{"purple", "#a632db"}, {"blue", "#0079bf"}, {"sky", "#00c2e0"}, {"lime", "#51e898"}, {"pink", "#ff78cb"},
		{"black", "#4d4d4d"}, {"silver", "#c0c0c0"}, {"peachpuff", "#ffdab9"}, {"crimson", "#dc143c"}, {"plum", "#dda0dd"},
		{"darkgreen", "#006400"}, {"slateblue", "#6a5acd"}, {"magenta", "#ff00ff"}, {"gold", "#ffd700"}, {"navy", "#000080"},
		{"gray", "#808080"}, {"saddlebrown", "#8b4513"}, {"paleturquoise", "#afeeee"}, {"mistyrose", "#ffe4e1"}, {"indigo", "#4b0082"},
	}
	taigaHex       = regexp.MustCompile(`(?i)^#?([0-9a-f]{6}|[0-9a-f]{3})$`)
	taigaDayRe     = regexp.MustCompile(`^\d{4}-\d\d-\d\d$`)
	taigaDateTime  = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d(?::\d\d(?:\.\d+)?)?(?:Z|[+-]\d\d:?\d\d)?$`)
	taigaZoneColon = regexp.MustCompile(`([+-]\d\d)(\d\d)$`)
	taigaZoned     = regexp.MustCompile(`(?:Z|[+-]\d\d:\d\d)$`)
)

func taigaRGB(hex string) []int {
	m := taigaHex.FindStringSubmatch(jsTrim(hex))
	if m == nil {
		return nil
	}
	h := m[1]
	if len(h) == 3 {
		h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
	}
	out := make([]int, 3)
	for i := range out {
		v, _ := strconv.ParseInt(h[i*2:i*2+2], 16, 32)
		out[i] = int(v)
	}
	return out
}

// taigaLabelColor is WeKan's wekanLabelColor: the WeKan label color nearest
// to a Taiga tag color.
func taigaLabelColor(hex string) string {
	source := taigaRGB(hex)
	if source == nil {
		return ""
	}
	best, bestDistance := "", math.Inf(1)
	for _, c := range taigaLabelHex {
		t := taigaRGB(c.hex)
		d := 0.0
		for i := range t {
			d += float64((t[i] - source[i]) * (t[i] - source[i]))
		}
		if d < bestDistance {
			best, bestDistance = c.name, d
		}
	}
	return best
}

// taigaDate is WeKan's: an ISO string; "" with true for none (undefined,
// null or ""), false when present but no date - a day that does not exist
// included, which Date would roll into the next month.
func taigaDate(v any) (string, bool) {
	if _, undef := v.(undefinedValue); undef || v == nil {
		return "", true
	}
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	if s == "" {
		return "", true
	}
	text := jsTrim(s)
	iso := ""
	switch {
	case taigaDayRe.MatchString(text):
		iso = text + "T00:00:00.000Z"
	case taigaDateTime.MatchString(text):
		iso = taigaZoneColon.ReplaceAllString(text, "$1:$2")
		if !taigaZoned.MatchString(iso) {
			iso += "Z"
		}
	}
	if iso == "" {
		return "", false
	}
	n := func(s string) int { x, _ := strconv.Atoi(s); return x }
	year, month, day := n(text[0:4]), n(text[5:7]), n(text[8:10])
	if year < 100 {
		year += 1900 // Date.UTC's
	}
	if cal := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC); int(cal.Month()) != month || cal.Day() != day {
		return "", false
	}
	t, ok := jsDateTime(iso)
	if !ok {
		return "", false
	}
	return jsISO(t), true
}

func taigaList(v any) []any { a, _ := v.([]any); return a }
func taigaStr(v any) string { s, _ := v.(string); return s }
func taigaHas(v any) bool {
	if _, undef := v.(undefinedValue); undef || v == nil {
		return false
	}
	s, isStr := v.(string)
	return !isStr || s != ""
}
func taigaIsObject(v any) bool { _, ok := v.(jsObject); return ok }
func taigaNum(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok && !math.IsInf(f, 0) && !math.IsNaN(f)
}
func taigaPerson(v any) string { return jsTrim(taigaStr(v)) }

// taigaUniq is uniq(): the truthy values, once each.
func taigaUniq(values []string) []any {
	out := []any{}
	for _, v := range values {
		if v != "" && !containsAny(out, v) {
			out = append(out, v)
		}
	}
	return out
}

func taigaOrder(v any, key string) float64 {
	if f, ok := taigaNum(jsGet(v, key)); ok {
		return f
	}
	return 0
}

func taigaTagName(tag any) string {
	switch t := tag.(type) {
	case string:
		return jsTrim(t)
	case []any:
		if len(t) > 0 {
			if s, ok := t[0].(string); ok {
				return jsTrim(s)
			}
		}
	}
	return ""
}

func taigaRef(v any) string {
	if f, ok := taigaNum(v); ok {
		return jsTrim(jsStr(f))
	}
	if s, ok := v.(string); ok && jsTrim(s) != "" {
		return jsTrim(s)
	}
	return ""
}

type taigaPos struct {
	item any
	pos  int
}

func taigaSorted(items []taigaPos, key string) []taigaPos {
	out := append([]taigaPos{}, items...)
	list := make([]any, len(out))
	for i := range out {
		list[i] = out[i]
	}
	jsSort(list, func(a, b any) float64 {
		x, y := a.(taigaPos), b.(taigaPos)
		if d := taigaOrder(x.item, key) - taigaOrder(y.item, key); d != 0 {
			return d
		}
		return float64(x.pos - y.pos)
	})
	for i := range list {
		out[i] = list[i].(taigaPos)
	}
	return out
}

func parseTaiga(input any) (jsObject, error) {
	dump := input
	if s, ok := input.(string); ok {
		v, err := decodeJS([]byte(s))
		if err != nil || !json.Valid([]byte(s)) {
			return nil, errors.New("A Taiga project dump must be JSON")
		}
		dump = v
	}
	if !taigaIsObject(dump) {
		return nil, errors.New("A Taiga project dump is one JSON object")
	}
	if _, ok := jsGet(dump, "user_stories").([]any); !ok {
		return nil, errors.New("A Taiga project dump needs its user_stories array")
	}
	count := 0
	for _, k := range []string{"user_stories", "tasks", "issues", "epics"} {
		count += len(taigaList(jsGet(dump, k)))
	}
	if count > maxTaigaItems {
		return nil, errors.New("A Taiga project dump with more than " + strconv.Itoa(maxTaigaItems) + " items is not imported")
	}
	unsupported := []any{}
	lost := func(path, reason string) { unsupported = append(unsupported, importUnsupportedObj(path, reason)) }
	date := func(v any, path string) string {
		d, ok := taigaDate(v)
		if !ok {
			lost(path, "a date that is not a date is left out")
			return ""
		}
		return d
	}
	var columns []any
	addColumn := func(name any) any {
		if jsTruthy(name) && !containsAny(columns, name) {
			columns = append(columns, name)
		}
		return name
	}
	// Lists: the user story statuses, then any other status an item uses.
	statuses := jsObject{}
	var us []any
	for _, s := range taigaList(jsGet(dump, "us_statuses")) {
		if taigaIsObject(s) && jsTrim(taigaStr(jsGet(s, "name"))) != "" {
			us = append(us, s)
		}
	}
	jsSort(us, func(a, b any) float64 { return taigaOrder(a, "order") - taigaOrder(b, "order") })
	for _, s := range us {
		name := jsTrim(jsGet(s, "name").(string))
		statuses.set(name, s)
		addColumn(name)
	}
	var defaultStatus any = jsTrim(taigaStr(jsGet(dump, "default_us_status")))
	if defaultStatus == "" {
		if len(columns) > 0 {
			defaultStatus = columns[0]
		} else {
			defaultStatus = "New"
		}
	}
	wip := 0
	for _, kv := range statuses {
		if f, ok := taigaNum(jsGet(kv.Value, "wip_limit")); ok && f > 0 {
			wip++
		}
	}
	if wip > 0 {
		lost("/us_statuses", "the WIP limits of "+strconv.Itoa(wip)+" status(es) are not imported; set them on the lists")
	}
	var lanesSorted []any
	for _, s := range taigaList(jsGet(dump, "swimlanes")) {
		if taigaIsObject(s) && jsTrim(taigaStr(jsGet(s, "name"))) != "" {
			lanesSorted = append(lanesSorted, s)
		}
	}
	jsSort(lanesSorted, func(a, b any) float64 { return taigaOrder(a, "order") - taigaOrder(b, "order") })
	var taigaLanes []string
	for _, s := range lanesSorted {
		taigaLanes = append(taigaLanes, jsTrim(jsGet(s, "name").(string)))
	}
	swimlanes := append([]string{}, taigaLanes...)
	if len(swimlanes) == 0 {
		swimlanes = []string{"Default"}
	}
	addSwimlane := func(name string) string {
		swimlanes = addOnce(swimlanes, name)
		return name
	}
	storyLane := func(v any) string {
		lane := jsTrim(taigaStr(v))
		if len(taigaLanes) == 0 {
			return "Default"
		}
		if contains(taigaLanes, lane) {
			return lane
		}
		return addSwimlane("Unclassified")
	}
	// Points: their numeric value, summed over the roles that compute points.
	pointValue := map[string]any{}
	for _, p := range taigaList(jsGet(dump, "points")) {
		if taigaIsObject(p) {
			if f, ok := taigaNum(jsGet(p, "value")); ok {
				pointValue[taigaStr(jsGet(p, "name"))] = f
			} else {
				pointValue[taigaStr(jsGet(p, "name"))] = nil
			}
		}
	}
	computable := map[string]bool{}
	for _, r := range taigaList(jsGet(dump, "roles")) {
		if taigaIsObject(r) && jsGet(r, "computable") != false {
			computable[taigaStr(jsGet(r, "name"))] = true
		}
	}
	anyRoles := len(taigaList(jsGet(dump, "roles"))) > 0
	storyPoints := func(story any) (float64, bool) {
		sum, found := 0.0, false
		for _, rp := range taigaList(jsGet(story, "role_points")) {
			if !taigaIsObject(rp) || anyRoles && !computable[taigaStr(jsGet(rp, "role"))] {
				continue
			}
			if v, ok := pointValue[taigaStr(jsGet(rp, "points"))].(float64); ok {
				sum += v
				found = true
			}
		}
		return sum, found
	}
	tagColors := map[string]string{}
	var rows []any
	switch tc := jsGet(dump, "tags_colors").(type) {
	case []any:
		rows = tc
	case jsObject:
		for _, kv := range tc {
			rows = append(rows, []any{kv.Key, kv.Value})
		}
	}
	for _, row := range rows {
		if r, ok := row.([]any); ok && len(r) > 1 {
			name, ok1 := r[0].(string)
			color, ok2 := r[1].(string)
			if ok1 && ok2 {
				tagColors[jsTrim(name)] = color
			}
		}
	}
	var usedTags []string
	tags := func(item any) []any {
		var names []string
		for _, tag := range taigaList(jsGet(item, "tags")) {
			// Older Taiga versions stored tags as [name, color].
			if t, ok := tag.([]any); ok && len(t) > 1 {
				if color, ok := t[1].(string); ok {
					if _, known := tagColors[taigaTagName(tag)]; !known {
						tagColors[taigaTagName(tag)] = color
					}
				}
			}
			names = append(names, taigaTagName(tag))
		}
		out := taigaUniq(names)
		for _, n := range out {
			usedTags = addOnce(usedTags, n.(string))
		}
		return out
	}
	comments := func(item any, path string) []any {
		out := []any{}
		for _, e := range taigaList(jsGet(item, "history")) {
			if !taigaIsObject(e) || jsTrim(taigaStr(jsGet(e, "comment"))) == "" || jsTruthy(jsGet(e, "delete_comment_date")) {
				continue
			}
			user, _ := jsGet(e, "user").([]any)
			at := func(i int) string {
				if i < len(user) {
					return taigaStr(user[i])
				}
				return ""
			}
			c := jsObject{}
			c.set("text", jsGet(e, "comment"))
			c.set("author", jsOr(at(0), at(1)))
			c.set("authorName", at(1))
			if d := date(jsGetOrUndef(e, "created_at"), path+"/history"); d != "" {
				c.set("date", d)
			}
			out = append(out, c)
		}
		return out
	}
	attachments := 0
	customFields := func(item any, extra jsObject) jsObject {
		attachments += len(taigaList(jsGet(item, "attachments")))
		fields := jsObject{}
		if values, ok := jsGet(item, "custom_attributes_values").(jsObject); ok {
			for _, kv := range values {
				if taigaHas(kv.Value) {
					fields.set(kv.Key, kv.Value)
				}
			}
		}
		if jsGet(item, "is_blocked") == true {
			fields.set("Blocked", jsOr(jsTrim(taigaStr(jsGet(item, "blocked_note"))), "Blocked"))
		}
		if r := jsTrim(taigaStr(jsGet(item, "due_date_reason"))); r != "" {
			fields.set("Due date reason", r)
		}
		for _, kv := range extra {
			if taigaHas(kv.Value) {
				fields.set(kv.Key, kv.Value)
			}
		}
		return jsOrder(fields)
	}
	owner := func(item any, out jsObject) jsObject {
		names := []string{taigaPerson(jsGet(item, "assigned_to"))}
		for _, u := range taigaList(jsGet(item, "assigned_users")) {
			names = append(names, taigaPerson(u))
		}
		assigned := taigaUniq(names)
		if len(assigned) > 0 {
			out.set("owner_username", assigned[0])
		}
		if len(assigned) > 1 {
			out.set("assignees", assigned[1:])
		}
		if p := taigaPerson(jsGet(item, "owner")); p != "" {
			out.set("requested_by", p)
		}
		var ws []string
		for _, w := range taigaList(jsGet(item, "watchers")) {
			ws = append(ws, taigaPerson(w))
		}
		if watchers := taigaUniq(ws); len(watchers) > 0 {
			out.set("watchers", watchers)
		}
		return out
	}
	setIf := func(o *jsObject, k, v string) {
		if v != "" {
			o.set(k, v)
		}
	}
	trueOrUndef := func(v any) any {
		if v == true {
			return true
		}
		return undefinedValue{}
	}
	// Which epics each user story belongs to, by the epics' order.
	var epicItems []taigaPos
	n := 0
	for _, e := range taigaList(jsGet(dump, "epics")) {
		if taigaIsObject(e) {
			epicItems = append(epicItems, taigaPos{e, n})
			n++
		}
	}
	epics := taigaSorted(epicItems, "epics_order")
	epicsOfStory := map[string][]string{}
	for _, ep := range epics {
		ref := taigaRef(jsGet(ep.item, "ref"))
		if ref == "" {
			continue
		}
		for _, link := range taigaList(jsGet(ep.item, "related_user_stories")) {
			if !taigaIsObject(link) || taigaRef(jsGet(link, "user_story")) == "" {
				continue
			}
			if slug := taigaStr(jsGet(link, "source_project_slug")); slug != "" {
				lost("/epics/"+strconv.Itoa(ep.pos)+"/related_user_stories", `a story of project "`+slug+`" is not part of this import`)
				continue
			}
			key := taigaRef(jsGet(link, "user_story"))
			epicsOfStory[key] = append(epicsOfStory[key], "epic-"+ref)
		}
	}
	tasks := []any{}
	// User stories, in kanban order.
	storyByRef := map[string]jsObject{}
	var storyItems []taigaPos
	for i, s := range taigaList(jsGet(dump, "user_stories")) {
		if taigaIsObject(s) {
			storyItems = append(storyItems, taigaPos{s, i})
		}
	}
	for _, sp := range taigaSorted(storyItems, "kanban_order") {
		story, at := sp.item, "/user_stories/"+strconv.Itoa(sp.pos)
		ref := taigaRef(jsGet(story, "ref"))
		status := addColumn(jsOr(jsTrim(taigaStr(jsGet(story, "status"))), defaultStatus))
		statusName, _ := status.(string)
		statusInfo := jsGet(statuses, statusName)
		var storyEpics []string
		if ref != "" {
			storyEpics = epicsOfStory[ref]
		}
		deps := []any{}
		dep := func(r string) {
			d := jsObject{}
			d.set("ref", r)
			d.set("type", "related-to")
			deps = append(deps, d)
		}
		if len(storyEpics) > 1 {
			for _, e := range storyEpics[1:] {
				dep(e)
			}
		}
		if r := taigaRef(jsGet(story, "generated_from_issue")); r != "" {
			dep("issue-" + r)
		}
		if r := taigaRef(jsGet(story, "generated_from_task")); r != "" {
			dep("task-" + r)
		}
		t := jsObject{}
		title := jsTrim(taigaStr(jsGet(story, "subject")))
		if title == "" {
			title = "Imported user story"
			if ref != "" {
				title = "#" + ref
			}
		}
		t.set("title", title)
		t.set("description", taigaStr(jsGet(story, "description")))
		t.set("column_name", status)
		t.set("swimlane_name", storyLane(jsGet(story, "swimlane")))
		t.set("tags", tags(story))
		if ref != "" {
			t.set("ref", "us-"+ref)
		}
		if len(storyEpics) > 0 {
			t.set("parent_ref", storyEpics[0])
		}
		if len(deps) > 0 {
			t.set("dependencies", deps)
		}
		if jsGet(statusInfo, "is_archived") == true {
			t.set("archived", true)
		}
		setIf(&t, "date_creation", date(jsGetOrUndef(story, "created_date"), at+"/created_date"))
		setIf(&t, "date_end", date(jsGetOrUndef(story, "finish_date"), at+"/finish_date"))
		setIf(&t, "date_due", date(jsGetOrUndef(story, "due_date"), at+"/due_date"))
		t.set("comments", comments(story, at))
		extra := jsObject{}
		if p, ok := storyPoints(story); ok {
			extra.set("Story points", p)
		} else {
			extra.set("Story points", undefinedValue{})
		}
		extra.set("Client requirement", trueOrUndef(jsGet(story, "client_requirement")))
		extra.set("Team requirement", trueOrUndef(jsGet(story, "team_requirement")))
		t.set("custom_fields", customFields(story, extra))
		t = owner(story, t)
		if ref != "" {
			storyByRef[ref] = t
		}
		tasks = append(tasks, t)
	}
	// Tasks, as subtask cards beside their story.
	var taskItems []taigaPos
	for i, s := range taigaList(jsGet(dump, "tasks")) {
		if taigaIsObject(s) {
			taskItems = append(taskItems, taigaPos{s, i})
		}
	}
	for _, tp := range taigaSorted(taskItems, "us_order") {
		item, at := tp.item, "/tasks/"+strconv.Itoa(tp.pos)
		ref := taigaRef(jsGet(item, "ref"))
		storyRef := taigaRef(jsGet(item, "user_story"))
		story, hasStory := storyByRef[storyRef]
		hasStory = hasStory && storyRef != ""
		t := jsObject{}
		title := jsTrim(taigaStr(jsGet(item, "subject")))
		if title == "" {
			title = "Imported task"
			if ref != "" {
				title = "#" + ref
			}
		}
		t.set("title", title)
		t.set("description", taigaStr(jsGet(item, "description")))
		if hasStory {
			t.set("column_name", jsGet(story, "column_name"))
			t.set("swimlane_name", jsGet(story, "swimlane_name"))
		} else {
			t.set("column_name", addColumn(defaultStatus))
			t.set("swimlane_name", swimlanes[0])
		}
		t.set("tags", tags(item))
		if ref != "" {
			t.set("ref", "task-"+ref)
		}
		if storyRef != "" {
			t.set("parent_ref", "us-"+storyRef)
		}
		setIf(&t, "date_creation", date(jsGetOrUndef(item, "created_date"), at+"/created_date"))
		setIf(&t, "date_end", date(jsGetOrUndef(item, "finished_date"), at+"/finished_date"))
		setIf(&t, "date_due", date(jsGetOrUndef(item, "due_date"), at+"/due_date"))
		t.set("comments", comments(item, at))
		extra := jsObject{}
		if s := jsTrim(taigaStr(jsGet(item, "status"))); s != "" {
			extra.set("Task status", s)
		}
		extra.set("Iocaine", trueOrUndef(jsGet(item, "is_iocaine")))
		t.set("custom_fields", customFields(item, extra))
		tasks = append(tasks, owner(item, t))
	}
	// Epics and issues, each in a swimlane of their own.
	var defaultEpicStatus any = jsTrim(taigaStr(jsGet(dump, "default_epic_status")))
	if defaultEpicStatus == "" {
		var es []any
		for _, s := range taigaList(jsGet(dump, "epic_statuses")) {
			if taigaIsObject(s) {
				es = append(es, s)
			}
		}
		jsSort(es, func(a, b any) float64 { return taigaOrder(a, "order") - taigaOrder(b, "order") })
		defaultEpicStatus = "New"
		if len(es) > 0 {
			if name := jsGetOrUndef(es[0], "name"); jsTruthy(name) {
				defaultEpicStatus = name
			}
		}
	}
	for _, ep := range epics {
		epic, at := ep.item, "/epics/"+strconv.Itoa(ep.pos)
		ref := taigaRef(jsGet(epic, "ref"))
		t := jsObject{}
		title := jsTrim(taigaStr(jsGet(epic, "subject")))
		if title == "" {
			title = "Imported epic"
			if ref != "" {
				title = "#" + ref
			}
		}
		t.set("title", title)
		t.set("description", taigaStr(jsGet(epic, "description")))
		t.set("column_name", addColumn(jsOr(jsTrim(taigaStr(jsGet(epic, "status"))), defaultEpicStatus)))
		t.set("swimlane_name", addSwimlane("Epics"))
		t.set("tags", tags(epic))
		if ref != "" {
			t.set("ref", "epic-"+ref)
		}
		if c := taigaStr(jsGet(epic, "color")); c != "" {
			t.set("color", c)
		}
		setIf(&t, "date_creation", date(jsGetOrUndef(epic, "created_date"), at+"/created_date"))
		t.set("comments", comments(epic, at))
		extra := jsObject{}
		extra.set("Client requirement", trueOrUndef(jsGet(epic, "client_requirement")))
		extra.set("Team requirement", trueOrUndef(jsGet(epic, "team_requirement")))
		t.set("custom_fields", customFields(epic, extra))
		tasks = append(tasks, owner(epic, t))
	}
	votes := 0
	var defaultIssueStatus any = jsOr(jsTrim(taigaStr(jsGet(dump, "default_issue_status"))), "New")
	for position, issue := range taigaList(jsGet(dump, "issues")) {
		if !taigaIsObject(issue) {
			continue
		}
		at := "/issues/" + strconv.Itoa(position)
		ref := taigaRef(jsGet(issue, "ref"))
		votes += len(taigaList(jsGet(issue, "votes")))
		var facets []string
		for _, f := range []string{"type", "priority", "severity"} {
			if v := jsTrim(taigaStr(jsGet(issue, f))); v != "" {
				facets = append(facets, f+":"+v)
			}
		}
		for _, f := range facets {
			usedTags = addOnce(usedTags, f)
		}
		t := jsObject{}
		title := jsTrim(taigaStr(jsGet(issue, "subject")))
		if title == "" {
			title = "Imported issue"
			if ref != "" {
				title = "#" + ref
			}
		}
		t.set("title", title)
		t.set("description", taigaStr(jsGet(issue, "description")))
		t.set("column_name", addColumn(jsOr(jsTrim(taigaStr(jsGet(issue, "status"))), defaultIssueStatus)))
		t.set("swimlane_name", addSwimlane("Issues"))
		var all []string
		for _, x := range tags(issue) {
			all = append(all, x.(string))
		}
		t.set("tags", taigaUniq(append(all, facets...)))
		if ref != "" {
			t.set("ref", "issue-"+ref)
		}
		setIf(&t, "date_creation", date(jsGetOrUndef(issue, "created_date"), at+"/created_date"))
		setIf(&t, "date_end", date(jsGetOrUndef(issue, "finished_date"), at+"/finished_date"))
		setIf(&t, "date_due", date(jsGetOrUndef(issue, "due_date"), at+"/due_date"))
		t.set("comments", comments(issue, at))
		t.set("custom_fields", customFields(issue, nil))
		tasks = append(tasks, owner(issue, t))
	}
	if len(columns) == 0 {
		addColumn(defaultStatus)
	}
	// What has no place on a WeKan board.
	if attachments > 0 {
		lost("/attachments", strconv.Itoa(attachments)+" attachment(s) are not imported: download them from Taiga and attach them to the cards")
	}
	if logo := jsGet(dump, "logo"); taigaIsObject(logo) && taigaStr(jsGet(logo, "name")) != "" {
		lost("/logo", "the project logo is not imported")
	}
	if n := len(taigaList(jsGet(dump, "wiki_pages"))); n > 0 {
		lost("/wiki_pages", strconv.Itoa(n)+" wiki page(s) are not imported: a WeKan board has no wiki")
	}
	if n := len(taigaList(jsGet(dump, "wiki_links"))); n > 0 {
		lost("/wiki_links", strconv.Itoa(n)+" wiki link(s) are not imported")
	}
	if n := len(taigaList(jsGet(dump, "timeline"))); n > 0 {
		lost("/timeline", "the project timeline ("+strconv.Itoa(n)+" event(s)) is not imported; comments are")
	}
	if votes > 0 {
		lost("/issues/votes", strconv.Itoa(votes)+" issue vote(s) are not imported")
	}
	members := 0
	for _, m := range taigaList(jsGet(dump, "memberships")) {
		if taigaIsObject(m) {
			members++
		}
	}
	if members > 0 {
		lost("/memberships", strconv.Itoa(members)+" project member(s) are not added to the board; Taiga names people by email, and an item keeps them only where a WeKan user is mapped")
	}
	labelColors := jsObject{}
	for _, name := range usedTags {
		if c := taigaLabelColor(tagColors[name]); c != "" {
			labelColors.set(name, c)
		}
	}
	lanes := []any{}
	for _, s := range swimlanes {
		o := jsObject{}
		o.set("name", s)
		lanes = append(lanes, o)
	}
	cols := []any{}
	for _, c := range columns {
		o := jsObject{}
		o.set("title", c)
		cols = append(cols, o)
	}
	out := jsObject{}
	b := jsObject{}
	b.set("name", jsOr(jsTrim(taigaStr(jsGet(dump, "name"))), "Imported Taiga project"))
	out.set("board", b)
	out.set("columns", cols)
	out.set("swimlanes", lanes)
	out.set("tasks", tasks)
	if len(labelColors) > 0 {
		out.set("label_colors", jsOrder(labelColors))
	}
	out.set("warnings", []any{})
	out.set("unsupported", unsupported)
	return out, nil
}
