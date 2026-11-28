package web

import (
	"errors"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

func init() { importParsers["jira"] = parseJira }

func jiraDescription(value any) string {
	var out strings.Builder
	var walk func(any, int)
	walk = func(node any, depth int) {
		if depth > 64 {
			return
		}
		kind := strOf(jsGet(node, "type"))
		switch kind {
		case "text":
			out.WriteString(strOf(jsGet(node, "text")))
		case "hardBreak":
			out.WriteByte('\n')
		case "mention", "emoji":
			out.WriteString(strOf(jsGet(jsGet(node, "attrs"), "text")))
		case "inlineCard":
			out.WriteString(strOf(jsGet(jsGet(node, "attrs"), "url")))
		default:
			children, _ := jsGet(node, "content").([]any)
			for _, child := range children {
				walk(child, depth+1)
			}
			switch kind {
			case "paragraph", "heading", "listItem", "codeBlock", "tableRow":
				if out.Len() == 0 || out.String()[out.Len()-1] != '\n' {
					out.WriteByte('\n')
				}
			}
		}
	}
	if text, ok := value.(string); ok {
		return text
	}
	walk(value, 0)
	return strings.TrimRight(out.String(), "\n")
}

func jiraImportDate(value any) any {
	if text, ok := value.(string); ok {
		if date, err := time.Parse("2006-01-02T15:04:05.999999999-0700", text); err == nil {
			return jsDate(date.UTC())
		}
	}
	return value
}

func jiraImportComments(fields any) ([]any, bool) {
	commentPage := jsGet(fields, "comment")
	items, _ := jsGet(commentPage, "comments").([]any)
	comments := []any{}
	for _, item := range items {
		text := jiraDescription(jsGet(item, "body"))
		if strings.TrimSpace(text) == "" {
			continue
		}
		person := jsGet(item, "author")
		author, _ := jsOrDefined(person, "accountId", "name", "emailAddress", "displayName")
		comments = append(comments, jsObject{{"text", text}, {"author", author},
			{"authorName", strOf(jsGet(person, "displayName"))}, {"date", jiraImportDate(jsGet(item, "created"))}})
	}
	total, _ := jsGet(commentPage, "total").(float64)
	start, _ := jsGet(commentPage, "startAt").(float64)
	return comments, start > 0 || total > float64(len(items))
}

func jiraPlanningField(data any, schemaType, displayName string) string {
	if schema, ok := jsGet(data, "schema").(jsObject); ok {
		for _, field := range schema {
			if jiraEstimateKey.MatchString(field.Key) && jsGet(field.Value, "custom") == schemaType {
				return field.Key
			}
		}
	}
	if names, ok := jsGet(data, "names").(jsObject); ok {
		for _, field := range names {
			if jiraEstimateKey.MatchString(field.Key) && strings.EqualFold(strings.TrimSpace(strOf(field.Value)), displayName) {
				return field.Key
			}
		}
	}
	return ""
}

func jiraIssueID(value any) string {
	switch id := value.(type) {
	case string:
		return id
	case float64:
		return jsNumber(id)
	}
	return ""
}

var jiraSprintSeparator = regexp.MustCompile(`,[a-zA-Z]+=`)
var jiraSprintID = regexp.MustCompile(`^[0-9]+$`)

func jiraSprintRecord(raw any) jsObject {
	if text, ok := raw.(string); ok {
		text = strings.TrimSpace(text)
		start := strings.IndexByte(text, '[')
		if start < 0 || !strings.HasSuffix(text, "]") {
			return nil
		}
		body := text[start+1 : len(text)-1]
		legacy := jsObject{}
		offset := 0
		for _, match := range jiraSprintSeparator.FindAllStringIndex(body, -1) {
			if key, value, ok := strings.Cut(body[offset:match[0]], "="); ok {
				legacy.set(key, value)
			}
			offset = match[0] + 1
		}
		if key, value, ok := strings.Cut(body[offset:], "="); ok {
			legacy.set(key, value)
		}
		raw = legacy
	}
	id := jiraIssueID(jsGet(raw, "id"))
	name := strings.TrimSpace(strOf(jsGet(raw, "name")))
	state := strings.ToLower(strOf(jsGet(raw, "state")))
	if !jiraSprintID.MatchString(id) || name == "" || (state != "future" && state != "active" && state != "closed") {
		return nil
	}
	goal := strOf(jsGet(raw, "goal"))
	if goal == "<null>" {
		goal = ""
	}
	return jsObject{{"recordId", id}, {"name", jsSlice16(name, 200)}, {"state", state}, {"goal", goal},
		{"plannedStart", jiraImportDate(jsGet(raw, "startDate"))}, {"plannedEnd", jiraImportDate(jsGet(raw, "endDate"))}}
}

var jiraImportTimeEstimates = [...]struct{ kind, name, nested, flat string }{
	{"original", "Jira original estimate (hours)", "originalEstimateSeconds", "timeoriginalestimate"},
	{"remaining", "Jira remaining estimate (hours)", "remainingEstimateSeconds", "timeestimate"},
}

func jiraImportTime(fields any, nested, flat string) (hours float64, present bool, err error) {
	raw := jsGet(jsGet(fields, "timetracking"), nested)
	if raw == nil {
		raw = jsGet(fields, flat)
	}
	if raw == nil {
		return 0, false, nil
	}
	seconds, ok := raw.(float64)
	if !ok || !(seconds >= 0 && seconds <= 9007199254740991) || math.Trunc(seconds) != seconds {
		return 0, false, errors.New("invalid Jira " + nested)
	}
	return seconds / 3600, true, nil
}

func jiraImportEstimate(data any) (*jiraEstimateMapping, error) {
	schema, _ := jsGet(data, "schema").(jsObject)
	var mapping jiraEstimateMapping
	mapping.unit = "points"
	if raw, present := jsLookup(data, "wekanScrumMapping"); present {
		config, ok := raw.(jsObject)
		if !ok {
			return nil, errors.New("invalid Jira estimate mapping")
		}
		for _, field := range config {
			if field.Key != "estimateFieldId" && field.Key != "estimateUnit" {
				return nil, errors.New("invalid Jira estimate mapping")
			}
		}
		mapping.key = strOf(jsGet(config, "estimateFieldId"))
		if !jiraEstimateKey.MatchString(mapping.key) {
			return nil, errors.New("invalid Jira estimate field ID")
		}
		if value, present := jsLookup(config, "estimateUnit"); present {
			unit, ok := value.(string)
			mapping.unit = strings.TrimSpace(unit)
			if !ok || mapping.unit == "" || utf16Len(unit) > 80 || strings.ContainsRune(unit, 0) {
				return nil, errors.New("invalid Jira estimate unit")
			}
		}
	} else {
		for _, field := range schema {
			if !jiraEstimateKey.MatchString(field.Key) || jsGet(field.Value, "type") != "number" ||
				jsGet(field.Value, "custom") != "com.pyxis.greenhopper.jira:jsw-story-points" {
				continue
			}
			if mapping.key != "" {
				return nil, nil
			}
			mapping.key = field.Key
		}
	}
	if mapping.key == "" {
		return nil, nil
	}
	if field, present := jsLookup(schema, mapping.key); present && jsGet(field, "type") != "number" {
		return nil, errors.New("Jira estimate field schema must be numeric")
	}
	return &mapping, nil
}

func parseJira(data any) (jsObject, error) {
	issues, ok := data.([]any)
	if !ok {
		issues, ok = jsGet(data, "issues").([]any)
	}
	if !ok {
		return nil, errors.New("invalid Jira issue export")
	}
	estimate, err := jiraImportEstimate(data)
	if err != nil {
		return nil, err
	}
	estimateSeen := false
	tasks, columns, losses := []any{}, []any{}, []any{}
	seen := map[string]bool{}
	releases, sprints := []any{}, []any{}
	sprintStates := map[string]string{}
	seenReleases := map[string]bool{}
	categories := map[string]string{}
	byKey, byID := map[string]string{}, map[string]string{}
	type rankedIssue struct {
		index int
		rank  string
	}
	var ranked []rankedIssue
	rankField := jiraPlanningField(data, "com.pyxis.greenhopper.jira:gh-lexo-rank", "Rank")
	sprintField := jiraPlanningField(data, "com.pyxis.greenhopper.jira:gh-sprint", "Sprint")
	epicField := jiraPlanningField(data, "com.pyxis.greenhopper.jira:gh-epic-link", "Epic Link")
	names := jsGet(data, "names")
	fieldNames, usedNames := map[string]string{}, map[string]bool{}
	for _, estimate := range jiraImportTimeEstimates {
		usedNames[estimate.name] = true
	}
	if estimate != nil {
		usedNames[fieldName(jsOr(strOf(jsGet(names, estimate.key)), estimate.key).(string))] = true
	}
	for index, issue := range issues {
		if _, ok := issue.(jsObject); !ok {
			return nil, errors.New("invalid Jira issue")
		}
		fields := jsGet(issue, "fields")
		if _, ok := fields.(jsObject); !ok {
			return nil, errors.New("invalid Jira issue fields")
		}
		key, title := strOf(jsGet(issue, "key")), strOf(jsGet(fields, "summary"))
		ref := strconv.Itoa(index + 1)
		if key != "" {
			if byKey[key] != "" {
				return nil, errors.New("duplicate Jira issue key")
			}
			byKey[key] = ref
		}
		if id := jiraIssueID(jsGet(issue, "id")); id != "" {
			if byID[id] != "" {
				return nil, errors.New("duplicate Jira issue ID")
			}
			byID[id] = ref
		}
		if key != "" {
			title = strings.TrimSpace("[" + key + "] " + title)
		}
		column := jsOr(strOf(jsGet(jsGet(fields, "status"), "name")), "Imported").(string)
		if !seen[column] {
			seen[column] = true
			columns = append(columns, jsObject{{"title", column}})
		}
		category := strOf(jsGet(jsGet(jsGet(fields, "status"), "statusCategory"), "key"))
		switch category {
		case "new":
			category = "todo"
		case "indeterminate":
			category = "doing"
		case "done":
		default:
			category = ""
		}
		if category != "" {
			if previous := categories[column]; previous != "" && previous != category {
				return nil, errors.New("Jira statuses with the same name have conflicting Scrum categories")
			}
			categories[column] = category
		}
		task := jsObject{}
		if raw := jsGet(jsGet(fields, "issuetype"), "name"); raw != nil {
			issueType, ok := raw.(string)
			if !ok || utf16Len(issueType) > 100 || strings.ContainsRune(issueType, 0) {
				return nil, errors.New("invalid Jira issue type")
			}
			task.set("jira_issue_type", issueType)
		}
		if estimate != nil {
			if value, present := jsLookup(fields, estimate.key); present {
				estimateSeen = true
				if value != nil {
					number, ok := value.(float64)
					if !ok || !(number >= 0 && number <= 1e12) {
						return nil, errors.New("invalid Jira estimate value")
					}
				}
				task.set("jira_estimate", value)
			}
		}
		spent, present, err := jiraImportTime(fields, "timeSpentSeconds", "timespent")
		if err != nil {
			return nil, err
		}
		if present {
			task.set("spentTime", spent)
		}
		customFields := jsObject{}
		for _, estimate := range jiraImportTimeEstimates {
			hours, present, err := jiraImportTime(fields, estimate.nested, estimate.flat)
			if err != nil {
				return nil, err
			}
			if present {
				customFields.set(estimate.name, hours)
			}
		}
		for _, field := range fields.(jsObject) {
			id := field.Key
			if !jiraEstimateKey.MatchString(id) || id == sprintField || id == rankField || id == epicField ||
				(estimate != nil && id == estimate.key) {
				continue
			}
			value := field.Value
			if object, ok := value.(jsObject); ok {
				value = jsGet(object, "value")
			}
			switch value.(type) {
			case string, bool, float64:
			default:
				continue
			}
			name := fieldNames[id]
			if name == "" {
				name = fieldName(strOf(jsGet(names, id)))
				if name == "" {
					name = id
				}
				base := name
				for n := 2; usedNames[name]; n++ {
					suffix := " (" + strconv.Itoa(n) + ")"
					name = jsSlice16(base, maxCustomFieldNameRunes-len(suffix)) + suffix
				}
				fieldNames[id], usedNames[name] = name, true
			}
			customFields.set(name, value)
		}
		if len(customFields) > 0 {
			task.set("custom_fields", customFields)
		}
		task.set("externalId", key)
		task.set("ref", ref)
		task.set("title", jsOr(title, "Imported issue"))
		task.set("description", jiraDescription(jsGet(fields, "description")))
		task.set("column_name", column)
		task.set("swimlane_name", "Default")
		for _, date := range [][2]string{{"duedate", "date_due"}, {"created", "date_creation"}, {"updated", "date_modification"}, {"resolutiondate", "date_end"}} {
			value, defined := jsLookup(fields, date[0])
			task.setDefined(date[1], jiraImportDate(value), defined)
		}
		if category == "done" {
			task.set("due_complete", true)
		}
		owner, defined := jsOrDefined(jsGet(fields, "assignee"), "accountId", "name", "emailAddress")
		task.setDefined("owner_username", owner, defined)
		requester, defined := jsOrDefined(jsGet(fields, "reporter"), "displayName", "name", "emailAddress")
		task.setDefined("requested_by", requester, defined)
		tags := []any{}
		labels, _ := jsGet(fields, "labels").([]any)
		for _, label := range labels {
			if name := strOf(label); name != "" {
				tags = append(tags, name)
			}
		}
		if priority := strOf(jsGet(jsGet(fields, "priority"), "name")); priority != "" {
			tags = append(tags, "priority:"+priority)
		}
		components, _ := jsGet(fields, "components").([]any)
		for _, component := range components {
			if name := strOf(jsGet(component, "name")); name != "" {
				tags = append(tags, name)
			}
		}
		task.set("tags", jsUniq(tags))
		if sprintField != "" {
			raw := jsGet(fields, sprintField)
			listed, ok := raw.([]any)
			if !ok && raw != nil {
				listed = []any{raw}
			}
			selected := ""
			first := ""
			multiple := false
			path := "/issues/" + strconv.Itoa(index) + "/fields/" + sprintField
			for _, entry := range listed {
				record := jiraSprintRecord(entry)
				if record == nil {
					losses = append(losses, importUnsupportedObj(path, "unrecognized Jira sprint skipped"))
					continue
				}
				id := strOf(jsGet(record, "recordId"))
				state := strOf(jsGet(record, "state"))
				if sprintStates[id] == "" {
					sprintStates[id] = state
					switch state {
					case "closed":
						losses = append(losses, importUnsupportedObj(path, "closed Jira sprint "+id+" skipped because no close snapshot is available"))
					default:
						sprints = append(sprints, record)
						if state == "active" {
							losses = append(losses, importUnsupportedObj(path, "active Jira sprint "+id+" imported as planned"))
						}
					}
				}
				if state != "closed" && sprintStates[id] != "closed" {
					if first == "" {
						first = id
					}
					multiple = multiple || id != first
					selected = id
				}
			}
			if multiple {
				losses = append(losses, importUnsupportedObj(path, "multiple Jira sprint assignments; kept sprint "+selected))
			}
			if selected != "" {
				task.set("jira_sprint", selected)
			}
		}
		versions, _ := jsGet(fields, "fixVersions").([]any)
		releaseRefs := []string{}
		cardReleases := map[string]bool{}
		for _, version := range versions {
			id := jiraIssueID(jsGet(version, "id"))
			name := strings.TrimSpace(strOf(jsGet(version, "name")))
			if id == "" || name == "" {
				continue
			}
			if !seenReleases[id] {
				seenReleases[id] = true
				state := "planned"
				if jsGet(version, "released") == true {
					state = "released"
				}
				releases = append(releases, jsObject{{"id", id}, {"name", jsSlice16(name, 200)},
					{"state", state}, {"notes", strOf(jsGet(version, "description"))},
					{"date", jiraImportDate(jsGet(version, "releaseDate"))}})
			}
			if !cardReleases[id] {
				cardReleases[id] = true
				releaseRefs = append(releaseRefs, id)
			}
		}
		if len(releaseRefs) > maxCardReleases {
			return nil, errors.New("too many Jira fix versions on one issue")
		}
		if len(releaseRefs) > 0 {
			task.set("jira_release_refs", releaseRefs)
		}
		subtasks, _ := jsGet(fields, "subtasks").([]any)
		items := []any{}
		for _, subtask := range subtasks {
			details := jsGet(subtask, "fields")
			title := strings.TrimSpace(strOf(jsGet(details, "summary")))
			if key := strOf(jsGet(subtask, "key")); key != "" {
				title = strings.TrimSpace("[" + key + "] " + title)
			}
			if title == "" {
				continue
			}
			category := jsGet(jsGet(jsGet(details, "status"), "statusCategory"), "key")
			items = append(items, jsObject{{"title", title}, {"done", category == "done"}})
		}
		if len(items) > 0 {
			task.set("checklists", []any{jsObject{{"title", "Subtasks"}, {"items", items}}})
		}
		files, _ := jsGet(fields, "attachment").([]any)
		attachments := []any{}
		for _, file := range files {
			if _, ok := file.(jsObject); ok {
				attachments = append(attachments, jsObject{{"name", strOf(jsGet(file, "filename"))},
					{"url", strOf(jsGet(file, "content"))}})
			}
		}
		if len(attachments) > 0 {
			task.set("attachments", attachments)
		}
		comments, incomplete := jiraImportComments(fields)
		if len(comments) > 0 {
			task.set("comments", comments)
		}
		if incomplete {
			losses = append(losses, importUnsupportedObj("/issues/"+strconv.Itoa(index)+"/fields/comment",
				"the Jira export contains only part of this issue's comments"))
		}
		if rank := strOf(jsGet(fields, rankField)); rankField != "" && rank != "" {
			ranked = append(ranked, rankedIssue{index, rank})
		}
		tasks = append(tasks, task)
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].rank < ranked[j].rank })
	for i, issue := range ranked {
		task := tasks[issue.index].(jsObject)
		task.set("jira_backlog_rank", i+1)
		tasks[issue.index] = task
	}
	seenLinks := map[[2]string]string{}

	for i, issue := range issues {
		fields := jsGet(issue, "fields")
		parent, direct := jsGet(fields, "parent").(jsObject)
		field, target, ref := "parent", "", ""
		if direct {
			key := strOf(jsGet(parent, "key"))
			id := jiraIssueID(jsGet(parent, "id"))
			target = jsOr(key, id).(string)
			ref = jsOr(byID[id], byKey[key]).(string)
		} else if epicField != "" {
			field = epicField
			target = strOf(jsGet(fields, epicField))
			ref = byKey[target]
		}
		if ref != "" {
			task := tasks[i].(jsObject)
			task.set("parent_ref", ref)
			tasks[i] = task
		} else if target != "" {
			losses = append(losses, importUnsupportedObj("/issues/"+strconv.Itoa(i)+"/fields/"+field,
				"parent "+strconv.Quote(target)+" is not part of this import"))
		}
		if raw := jsGet(fields, "issuelinks"); raw != nil {
			task := tasks[i].(jsObject)
			sourceRef := strOf(jsGet(task, "ref"))
			path := "/issues/" + strconv.Itoa(i) + "/fields/issuelinks"
			links, ok := raw.([]any)
			if !ok {
				losses = append(losses, importUnsupportedObj(path, "invalid Jira issue links"))
			}
			var deps []any
			for n, link := range links {
				linkPath := path + "/" + strconv.Itoa(n)
				outward, inward := jsGet(link, "outwardIssue"), jsGet(link, "inwardIssue")
				if (outward == nil) == (inward == nil) {
					losses = append(losses, importUnsupportedObj(linkPath, "invalid Jira issue link direction"))
					continue
				}
				target := outward
				if inward != nil {
					target = inward
				}
				targetRef := jsOr(byID[jiraIssueID(jsGet(target, "id"))], byKey[strOf(jsGet(target, "key"))]).(string)
				if targetRef == "" || targetRef == sourceRef {
					losses = append(losses, importUnsupportedObj(linkPath, "linked issue is outside this import or is the same issue"))
					continue
				}
				kind := "related-to"
				switch name := strings.ToLower(strings.TrimSpace(strOf(jsGet(jsGet(link, "type"), "name")))); name {
				case "blocks":
					kind = "blocks"
					if inward != nil {
						kind = "is-blocked-by"
					}
				case "duplicate", "duplicates":
					kind = "duplicates"
					if inward != nil {
						kind = "is-duplicated-by"
					}
				case "fixes":
					kind = "fixes"
					if inward != nil {
						kind = "is-fixed-by"
					}
				case "relates":
				default:
					losses = append(losses, importUnsupportedObj(linkPath, "unrecognized Jira link type imported as related-to"))
				}
				pair := [2]string{sourceRef, targetRef}
				previous := seenLinks[pair]
				if previous == "" {
					previous = importDependencyInverse[seenLinks[[2]string{targetRef, sourceRef}]]
				}
				if previous != "" {
					if previous != kind {
						losses = append(losses, importUnsupportedObj(linkPath, "additional Jira link skipped; kept the first dependency between these issues"))
					}
					continue
				}
				seenLinks[pair] = kind
				deps = append(deps, jsObject{{"ref", targetRef}, {"type", kind}})
			}
			if len(deps) > 0 {
				task.set("dependencies", deps)
				tasks[i] = task
			}
		}
	}
	for i, raw := range columns {
		column := raw.(jsObject)
		if category := categories[strOf(jsGet(column, "title"))]; category != "" {
			column.set("jira_category", category)
			columns[i] = column
		}
	}
	out := jsObject{
		{"board", jsObject{{"name", jsOr(jsGet(jsGet(data, "board"), "name"), "Imported Jira Board")}}},
		{"columns", columns}, {"swimlanes", []any{jsObject{{"name", "Default"}}}}, {"tasks", tasks}, {"unsupported", losses}, {"jira_releases", releases}, {"jira_sprints", sprints},
	}
	if estimate != nil {
		if !estimateSeen && jsGet(jsGet(jsGet(data, "schema"), estimate.key), "type") != "number" {
			return nil, errors.New("Jira estimate field is absent from the imported issues")
		}
		name := jsOr(strOf(jsGet(jsGet(data, "names"), estimate.key)), estimate.key)
		out.set("jira_estimate_mapping", jsObject{{"key", estimate.key}, {"unit", estimate.unit}, {"name", name}})
	}
	return out, nil
}
