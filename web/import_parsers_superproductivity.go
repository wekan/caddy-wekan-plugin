package web

import (
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A Super Productivity backup: Node.js WeKan's parseSuperProductivity
// (models/lib/superProductivityFormat.js) - its projects as swimlanes, each
// task's done state, in-progress tag and backlog as the list, its tags,
// notes and web-link attachments, deadline and due dates, estimate,
// priority and time spent, its subtasks' parents, and its two archives.

func init() { importTextParsers["superproductivity"] = parseSuperProductivity }

var (
	spPriority  = map[string]string{"1": "Low", "2": "Medium", "3": "High", "low": "Low", "medium": "Medium", "high": "High"}
	spDay       = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})$`)
	spWebLink   = regexp.MustCompile(`(?i)^https?://`)
	spInProgTag = "KANBAN_IN_PROGRESS"
	spTodayTag  = "TODAY"
)

func spIsObject(v any) bool { _, ok := v.(jsObject); return ok }

// spText is the module's text(): a string, a finite number as its string, or "".
func spText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		if !math.IsInf(x, 0) && !math.IsNaN(x) {
			return jsStr(x)
		}
	}
	return ""
}

// spEntities is WeKan's entitiesOf: an NgRx entity state's entities in its
// ids order, then the ones its ids leave out.
func spEntities(state any) []any {
	entities, ok := jsGet(state, "entities").(jsObject)
	if !spIsObject(state) || !ok {
		return nil
	}
	given, _ := jsGet(state, "ids").([]any)
	ids := append([]any{}, given...)
	for _, kv := range entities {
		ids = append(ids, kv.Key)
	}
	seen := map[string]bool{}
	var out []any
	for _, raw := range ids {
		var id string
		switch x := raw.(type) {
		case string:
			id = x
		case float64:
			id = jsStr(x)
		default:
			continue
		}
		e, has := jsLookup(entities, id)
		if seen[id] || !has || !spIsObject(e) {
			continue
		}
		seen[id] = true
		out = append(out, e)
	}
	return out
}

func spMs(v any) string {
	f, ok := v.(float64)
	if !ok || math.IsInf(f, 0) || math.IsNaN(f) || f <= 0 || f > 8.64e15 {
		return ""
	}
	return jsISO(time.UnixMilli(int64(math.Trunc(f))))
}

func spDayDate(v any) string {
	m := spDay.FindStringSubmatch(jsTrim(spText(v)))
	if m == nil {
		return ""
	}
	n := func(s string) int { x, _ := strconv.Atoi(s); return x }
	y := n(m[1])
	if y < 100 {
		y += 1900 // Date.UTC's
	}
	d := time.Date(y, time.Month(n(m[2])), n(m[3]), 0, 0, 0, 0, time.UTC)
	if int(d.Month()) != n(m[2]) || d.Day() != n(m[3]) {
		return ""
	}
	return jsISO(d)
}

func spHours(ms float64) float64 { return jsRound(ms/3600000*100) / 100 }

func parseSuperProductivity(input string) (jsObject, error) {
	text := strings.TrimPrefix(input, "\ufeff")
	doc, err := decodeJS([]byte(text))
	if err != nil || !json.Valid([]byte(text)) {
		return nil, errors.New("Super Productivity backup is not valid JSON")
	}
	if !spIsObject(doc) {
		return nil, errors.New("Super Productivity backup must be a JSON object")
	}
	data := doc
	_, a := jsLookup(doc, "crossModelVersion")
	_, b := jsLookup(doc, "timestamp")
	if d, c := jsLookup(doc, "data"); a && b && c {
		data = d
	}
	if !spIsObject(data) {
		return nil, errors.New("Super Productivity backup has no data object")
	}
	for _, k := range []string{"taskArchive", "improvement", "obstruction"} {
		if _, has := jsLookup(data, k); has {
			return nil, errors.New("this is a Super Productivity backup from before version 14; import it into Super Productivity and export it again")
		}
	}
	if !spIsObject(jsGet(data, "task")) || !spIsObject(jsGet(data, "project")) {
		return nil, errors.New("Super Productivity backup needs the task and project models")
	}
	unsupported := []any{}
	report := func(path, reason string) { unsupported = append(unsupported, importUnsupportedObj(path, reason)) }

	projects := spEntities(jsGet(data, "project"))
	projectByID := map[string]any{}
	backlog := map[string]bool{}
	for _, p := range projects {
		projectByID[jsStr(jsGetOrUndef(p, "id"))] = p
		ids, _ := jsGet(p, "backlogTaskIds").([]any)
		for _, id := range ids {
			backlog[jsStr(id)] = true
		}
	}
	tagTitle := map[string]string{}
	for _, t := range spEntities(jsGet(data, "tag")) {
		tagTitle[jsStr(jsGetOrUndef(t, "id"))] = jsTrim(spText(jsGet(t, "title")))
	}
	// Active tasks first, then the two archives.
	type entry struct {
		task     any
		id, path string
		archived bool
	}
	var all []entry
	seenIDs := map[string]bool{}
	for _, src := range []struct {
		path     string
		state    any
		archived bool
	}{
		{"/data/task", jsGet(data, "task"), false},
		{"/data/archiveYoung/task", jsGet(jsGet(data, "archiveYoung"), "task"), true},
		{"/data/archiveOld/task", jsGet(jsGet(data, "archiveOld"), "task"), true},
	} {
		for _, task := range spEntities(src.state) {
			id := spText(jsGet(task, "id"))
			if id == "" || seenIDs[id] {
				continue
			}
			seenIDs[id] = true
			all = append(all, entry{task, id, src.path + "/" + id, src.archived})
		}
	}
	byID := map[string]entry{}
	for _, e := range all {
		byID[e.id] = e
	}
	// Project titles as swimlane names; two of one title stay apart.
	swimlaneOf := map[string]string{}
	var swimlaneNames []string
	nameFor := func(projectID string) string {
		key := ""
		if _, ok := projectByID[projectID]; ok {
			key = projectID
		}
		if n, ok := swimlaneOf[key]; ok {
			return n
		}
		base := "No project"
		if key != "" {
			base = jsOr(jsTrim(spText(jsGet(projectByID[key], "title"))), "Untitled project").(string)
		}
		name := base
		for n := 2; contains(swimlaneNames, name); n++ {
			name = base + " (" + strconv.Itoa(n) + ")"
		}
		swimlaneOf[key] = name
		swimlaneNames = append(swimlaneNames, name)
		return name
	}
	projectOf := func(task any) string {
		if p := spText(jsGet(task, "projectId")); p != "" {
			return p
		}
		if parent, ok := byID[spText(jsGet(task, "parentId"))]; ok {
			return spText(jsGet(parent.task, "projectId"))
		}
		return ""
	}
	usedProjects := map[string]bool{}
	for _, e := range all {
		usedProjects[projectOf(e.task)] = true
	}
	for _, p := range projects {
		if id := jsStr(jsGetOrUndef(p, "id")); usedProjects[id] {
			nameFor(id)
		}
	}
	breakdown := false
	used := map[string]bool{}
	tasks := []any{}
	for _, e := range all {
		task, id, path := e.task, e.id, e.path
		parent, hasParent := byID[spText(jsGet(task, "parentId"))]
		projectID := projectOf(task)
		var tagIDs []string
		if list, ok := jsGet(task, "tagIds").([]any); ok {
			for _, t := range list {
				tagIDs = append(tagIDs, jsStr(t))
			}
		}
		done := jsGet(task, "isDone") == true || e.archived
		list := "To Do"
		switch {
		case done:
			list = "Done"
		case contains(tagIDs, spInProgTag):
			list = "In Progress"
		case backlog[id] || hasParent && backlog[parent.id]:
			list = "Backlog"
		}
		used[list] = true
		tags := []any{}
		for _, tagID := range tagIDs {
			if tagID == spTodayTag || tagID == spInProgTag {
				continue
			}
			if title := tagTitle[tagID]; title != "" {
				if !containsAny(tags, title) {
					tags = append(tags, title)
				}
			} else {
				report(path+"/tagIds", `tag "`+tagID+`" is not in the backup's tag model`)
			}
		}
		description := spText(jsGet(task, "notes"))
		var links []string
		attachments, _ := jsGet(task, "attachments").([]any)
		for index, att := range attachments {
			target := jsTrim(spText(jsGet(att, "path")))
			if spWebLink.MatchString(target) {
				title := jsOr(jsTrim(spText(jsGet(att, "title"))), target).(string)
				links = append(links, "- "+markdownLinkText(title, target))
			} else {
				kind := jsOr(spText(jsGet(att, "type")), "local").(string)
				report(path+"/attachments/"+strconv.Itoa(index), "a "+kind+" attachment is a path on the user's device, not a web link, and is not imported")
			}
		}
		if len(links) > 0 {
			if description != "" {
				description += "\n\n"
			}
			description += strings.Join(links, "\n")
		}
		deadline := jsOr(spMs(jsGet(task, "deadlineWithTime")), spDayDate(jsGet(task, "deadlineDay"))).(string)
		scheduled := jsOr(spMs(jsGet(task, "dueWithTime")), spDayDate(jsGet(task, "dueDay"))).(string)
		due := jsOr(deadline, scheduled).(string)
		started := ""
		if deadline != "" && scheduled != "" {
			started = scheduled
		}
		ended := ""
		if done {
			ended = spMs(jsGet(task, "doneOn"))
		}
		created := spMs(jsGet(task, "created"))
		custom := jsObject{}
		if est, ok := jsToNumber(jsGetOrUndef(task, "timeEstimate")); ok && !math.IsInf(est, 0) && est > 0 {
			custom.set("Time estimate (hours)", spHours(est))
		}
		pkey := jsLower(spText(jsGet(task, "priority")))
		if p := spPriority[pkey]; p != "" {
			custom.set("Priority", p)
		} else if jsPrototypeKeys[pkey] {
			// PRIORITY_NAMES["constructor"] is a function: the key is set,
			// and JSON.stringify leaves its value out.
			custom.set("Priority", undefinedValue{})
		}
		spent, spentOK := jsToNumber(jsGetOrUndef(task, "timeSpent"))
		if days, ok := jsGet(task, "timeSpentOnDay").(jsObject); ok && len(days) > 0 {
			breakdown = true
		}
		if spText(jsGet(task, "repeatCfgId")) != "" {
			report(path+"/repeatCfgId", "Super Productivity repeat configurations are not imported; the task imports once")
		}
		if spMs(jsGet(task, "remindAt")) != "" || spMs(jsGet(task, "deadlineRemindAt")) != "" {
			report(path+"/remindAt", "Super Productivity reminders are not imported")
		}
		if issueID, issueType := spText(jsGet(task, "issueId")), spText(jsGet(task, "issueType")); issueID != "" || issueType != "" {
			report(path+"/issueId", "the link to "+jsOr(issueType, "an issue provider").(string)+" issue "+issueID+" is not imported")
		}
		t := jsObject{}
		t.set("title", jsOr(jsTrim(spText(jsGet(task, "title"))), "Untitled"))
		t.set("description", description)
		t.set("column_name", list)
		t.set("swimlane_name", nameFor(projectID))
		t.set("tags", tags)
		t.set("ref", id)
		if hasParent {
			t.set("parent_ref", parent.id)
		} else if p := spText(jsGet(task, "parentId")); p != "" {
			t.set("parent_ref", p)
		}
		for _, d := range []struct{ key, value string }{{"date_due", due}, {"date_started", started}, {"date_end", ended}, {"date_creation", created}} {
			if d.value != "" {
				t.set(d.key, d.value)
			}
		}
		if spentOK && !math.IsInf(spent, 0) && spent > 0 {
			t.set("spent_hours", spHours(spent))
		}
		if len(custom) > 0 {
			t.set("custom_fields", custom)
		}
		if e.archived {
			t.set("archived", true)
		}
		tasks = append(tasks, t)
	}
	if breakdown {
		report("/data/task/timeSpentOnDay", "the per-day breakdown of tracked time is not imported; each card keeps the total as spent hours")
	}
	if notes := len(spEntities(jsGet(data, "note"))); notes > 0 {
		report("/data/note", strconv.Itoa(notes)+" Super Productivity note(s) belong to projects, not tasks, and are not imported")
	}
	if cfgs, ok := jsGet(jsGet(data, "boards"), "boardCfgs").([]any); ok && spIsObject(jsGet(data, "boards")) && len(cfgs) > 0 {
		report("/data/boards", "Super Productivity boards are saved filters, not containers; lists come from each task's done state, in-progress tag and backlog instead")
	}
	var columns []string
	for _, title := range []string{"Backlog", "To Do", "In Progress", "Done"} {
		if used[title] || title == "To Do" || title == "Done" {
			columns = append(columns, title)
		}
	}
	lanes := swimlaneNames
	if len(lanes) == 0 {
		lanes = []string{"No project"}
	}
	laneObjs := []any{}
	for _, l := range lanes {
		o := jsObject{}
		o.set("name", l)
		laneObjs = append(laneObjs, o)
	}
	name := "Super Productivity"
	if len(swimlaneNames) == 1 && swimlaneNames[0] != "No project" {
		name = swimlaneNames[0]
	}
	out := importDoc(name, csvColumns(columns), tasks, []any{}, unsupported)
	out.set("swimlanes", laneObjs)
	return out, nil
}

func containsAny(list []any, v any) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
