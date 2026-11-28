package web

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Jira's issues collection, as Node.js WeKan's externalExportFormatters.js
// jira writes it - the shape its Jira import reads back: each card an issue
// keyed WEKAN-n, its list the status, labels, dates, people, its parent, its
// checklists as subtasks, its comments, and the time spent as Jira's time
// tracking. Selected custom values use Jira’s names map and field keys.

func init() { apiFormatters["jira"] = formatJira }

// jsOptional is a value JSON.stringify writes, or undefined (left out) for
// what WeKan's item has none of.
func jsOptional(s string) any {
	if s == "" {
		return undefinedValue{}
	}
	return s
}

type jiraEstimateMapping struct {
	key, unit, localID, name string
}

var jiraEstimateKey = regexp.MustCompile(`^customfield_[0-9]{1,20}$`)

func (fr *fieldReader) jiraEstimateMapping() *jiraEstimateMapping {
	var match *jiraEstimateMapping
	for id, definition := range fr.defs {
		settings := asM(definition["settings"])
		key := strOf(settings["jiraEstimateFieldId"])
		if strOf(definition["type"]) != "number" || key == "" {
			continue
		}
		if match != nil || !jiraEstimateKey.MatchString(key) || !fr.mayRead(id) {
			return nil
		}
		unit := "points"
		if raw, exists := settings["jiraEstimateUnit"]; exists {
			name, ok := raw.(string)
			unit = strings.TrimSpace(name)
			if !ok || unit == "" || len(utf16.Encode([]rune(name))) > 80 {
				return nil
			}
		}
		match = &jiraEstimateMapping{key: key, unit: unit, localID: id, name: strOf(definition["name"])}
	}
	return match
}

func (m jiraEstimateMapping) value(card map[string]any) jsObject {
	var result jsObject
	found := false
	for _, field := range fieldValues(card) {
		if strOf(field["_id"]) != m.localID {
			continue
		}
		if found {
			return nil
		}
		found = true
		var value float64
		switch n := field["value"].(type) {
		case nil:
			result = jsObject{{m.key, nil}}
			continue
		case float64:
			value = n
		case int32:
			value = float64(n)
		case int64:
			value = float64(n)
		default:
			return nil
		}
		if !(value >= 0 && value <= 1e12) {
			return nil
		}
		result = jsObject{{m.key, value}}
	}
	return result
}

var jiraTimeEstimateFields = [...]struct{ kind, key string }{
	{"original", "originalEstimateSeconds"},
	{"remaining", "remainingEstimateSeconds"},
}

func jiraTimeSeconds(value any) (float64, bool) {
	hours, ok := toFloat(value)
	seconds := math.Round(hours * 3600)
	return seconds, ok && hours >= 0 && seconds <= 9007199254740991
}

func (fr *fieldReader) jiraTimeEstimateIds() [2]string {
	var ids [2]string
	var counts [2]int
	for id, definition := range fr.defs {
		if strOf(definition["type"]) != "number" {
			continue
		}
		kind := strOf(asM(definition["settings"])["jiraTimeField"])
		for i, field := range jiraTimeEstimateFields {
			if field.kind == kind {
				counts[i]++
				ids[i] = id
			}
		}
	}
	for i, id := range ids {
		if counts[i] != 1 || !fr.mayRead(id) {
			ids[i] = ""
		}
	}
	return ids
}

func jiraTimeEstimates(card map[string]any, ids [2]string, tracking *jsObject) {
	var values [2]any
	var counts [2]int
	for _, assignment := range fieldValues(card) {
		for i, id := range ids {
			if id != "" && strOf(assignment["_id"]) == id {
				counts[i]++
				values[i] = assignment["value"]
			}
		}
	}
	for i, field := range jiraTimeEstimateFields {
		if seconds, ok := jiraTimeSeconds(values[i]); counts[i] == 1 && ok {
			tracking.set(field.key, seconds)
		}
	}
}

func formatJira(d extBoard) any {
	keys := map[string]string{}
	fieldKeys := map[string]string{}
	names := jsObject{}
	nextField := 90001
	for idx, i := range d.Items {
		keys[i.CardID] = "WEKAN-" + strconv.Itoa(idx+1)
		for _, field := range i.CustomFields {
			if _, exists := fieldKeys[field.Key]; exists {
				continue
			}
			key := "customfield_" + strconv.Itoa(nextField)
			nextField++
			if d.JiraEstimate != nil && key == d.JiraEstimate.key {
				key = "customfield_" + strconv.Itoa(nextField)
				nextField++
			}
			fieldKeys[field.Key] = key
			names.set(key, field.Key)
		}
	}
	versions := map[string]jsObject{}
	for _, release := range d.Releases {
		id, name := strOf(release["_id"]), strOf(release["name"])
		if id == "" || jsTrim(name) == "" {
			continue
		}
		exportID := id
		if source := asM(release["provenance"]); strOf(source["system"]) == "jira" {
			if record := strOf(source["recordId"]); record != "" {
				exportID = record
			}
		}
		version := jsObject{{"id", exportID}, {"name", name}, {"released", strOf(release["state"]) == "released"}}
		date := isoDate(release["plannedEnd"])
		if date == "" {
			date = isoDate(release["releasedAt"])
		}
		if date != "" {
			version.set("releaseDate", date[:10])
		}
		if notes := strOf(release["notes"]); notes != "" {
			version.set("description", notes)
		}
		versions[id] = version
	}
	categories := map[string]string{"todo": "new", "doing": "indeterminate", "done": "done"}
	issues := []any{}
	for _, i := range d.Items {
		status := jsObject{}
		status.set("name", i.ListTitle)
		f := jsObject{}
		f.set("summary", i.Title)
		f.set("description", i.Description)
		if category := categories[i.JiraStatusCategory]; category != "" {
			status.set("statusCategory", jsObject{{"key", category}})
		}
		f.set("status", status)
		if i.JiraIssueType != "" {
			f.set("issuetype", jsObject{{"name", i.JiraIssueType}})
		}
		fixVersions := []jsObject{}
		for _, id := range i.JiraReleaseIDs {
			if version, ok := versions[id]; ok {
				fixVersions = append(fixVersions, version)
			}
		}
		if len(fixVersions) != 0 {
			f.set("fixVersions", fixVersions)
		}
		labels := append([]string{}, i.Labels...)
		f.set("labels", labels)
		f.set("duedate", jsOptional(i.DueAt))
		f.set("resolutiondate", jsOptional(i.EndAt))
		f.set("updated", jsOptional(i.ModifiedAt))
		if len(i.Timetracking) > 0 {
			f.set("timetracking", i.Timetracking)
		}
		if i.CreatedAt != "" {
			f.set("created", i.CreatedAt)
		}
		if i.Owner != "" {
			a := jsObject{}
			a.set("name", i.Owner)
			f.set("assignee", a)
		}
		if r := jsOr(i.RequestedBy, i.Creator).(string); r != "" {
			o := jsObject{}
			o.set("displayName", r)
			f.set("reporter", o)
		}
		if k, ok := keys[i.ParentCardID]; ok {
			p := jsObject{}
			p.set("key", k)
			f.set("parent", p)
		}
		if entries := checklistEntries(i); len(entries) > 0 {
			subs := []any{}
			for _, x := range entries {
				cat := jsObject{}
				cat.set("key", map[bool]string{true: "done", false: "new"}[x.Done])
				st := jsObject{}
				st.set("statusCategory", cat)
				sf := jsObject{}
				sf.set("summary", x.Title)
				sf.set("status", st)
				sub := jsObject{}
				sub.set("fields", sf)
				subs = append(subs, sub)
			}
			f.set("subtasks", subs)
		}
		if len(i.Comments) > 0 {
			cs := []any{}
			for _, c := range i.Comments {
				author := jsObject{}
				author.set("name", jsOptional(c.Author))
				author.set("displayName", jsOptional(c.DisplayName))
				o := jsObject{}
				o.set("body", c.Text)
				o.set("author", author)
				o.set("created", jsOptional(c.Date))
				cs = append(cs, o)
			}
			comment := jsObject{}
			comment.set("comments", cs)
			f.set("comment", comment)
		}
		for _, field := range i.CustomFields {
			f.set(fieldKeys[field.Key], field.Value)
		}
		for _, estimate := range i.JiraEstimate {
			f.set(estimate.Key, estimate.Value)
		}
		issue := jsObject{}
		issue.set("key", keys[i.CardID])
		issue.set("fields", f)
		issues = append(issues, issue)
	}
	board := jsObject{}
	board.set("name", d.Board.Title)
	out := jsObject{}
	out.set("board", board)
	if mapping := d.JiraEstimate; mapping != nil {
		if mapping.name != "" {
			names.set(mapping.key, mapping.name)
		}
		out.set("wekanScrumMapping", jsObject{{"estimateFieldId", mapping.key}, {"estimateUnit", mapping.unit}})
		out.set("schema", jsObject{{mapping.key, jsObject{{"type", "number"}}}})
	}
	if len(names) > 0 {
		out.set("names", names)
	}
	out.set("issues", issues)
	return out
}
