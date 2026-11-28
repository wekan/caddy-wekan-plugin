package web

import (
	"errors"
	"math"
	"strings"
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

func parseJira(data any) (jsObject, error) {
	issues, ok := data.([]any)
	if !ok {
		issues, ok = jsGet(data, "issues").([]any)
	}
	if !ok {
		return nil, errors.New("invalid Jira issue export")
	}
	tasks, columns := []any{}, []any{}
	seen := map[string]bool{}
	for _, issue := range issues {
		if _, ok := issue.(jsObject); !ok {
			return nil, errors.New("invalid Jira issue")
		}
		fields := jsGet(issue, "fields")
		if _, ok := fields.(jsObject); !ok {
			return nil, errors.New("invalid Jira issue fields")
		}
		key, title := strOf(jsGet(issue, "key")), strOf(jsGet(fields, "summary"))
		if key != "" {
			title = strings.TrimSpace("[" + key + "] " + title)
		}
		column := jsOr(strOf(jsGet(jsGet(fields, "status"), "name")), "Imported").(string)
		if !seen[column] {
			seen[column] = true
			columns = append(columns, jsObject{{"title", column}})
		}
		task := jsObject{}
		spent := jsGet(jsGet(fields, "timetracking"), "timeSpentSeconds")
		if spent == nil {
			spent = jsGet(fields, "timespent")
		}
		if spent != nil {
			value, ok := spent.(float64)
			if !ok || !(value >= 0 && value <= 9007199254740991) || math.Trunc(value) != value {
				return nil, errors.New("invalid Jira timeSpentSeconds")
			}
			task.set("spentTime", value/3600)
		}
		task.set("externalId", key)
		task.set("title", jsOr(title, "Imported issue"))
		task.set("description", jiraDescription(jsGet(fields, "description")))
		task.set("column_name", column)
		task.set("swimlane_name", "Default")
		due, defined := jsLookup(fields, "duedate")
		task.setDefined("date_due", due, defined)
		owner, defined := jsOrDefined(jsGet(fields, "assignee"), "accountId", "name", "emailAddress")
		task.setDefined("owner_username", owner, defined)
		requester, defined := jsOrDefined(jsGet(fields, "reporter"), "displayName", "name", "emailAddress")
		task.setDefined("requested_by", requester, defined)
		task.set("tags", jsOr(jsGet(fields, "labels"), []any{}))
		tasks = append(tasks, task)
	}
	return jsObject{
		{"board", jsObject{{"name", jsOr(jsGet(jsGet(data, "board"), "name"), "Imported Jira Board")}}},
		{"columns", columns}, {"swimlanes", []any{jsObject{{"name", "Default"}}}}, {"tasks", tasks},
	}, nil
}
