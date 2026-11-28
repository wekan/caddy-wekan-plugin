package web

import (
	"errors"
	"math"
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
	return comments, total > float64(len(items))
}

func parseJira(data any) (jsObject, error) {
	issues, ok := data.([]any)
	if !ok {
		issues, ok = jsGet(data, "issues").([]any)
	}
	if !ok {
		return nil, errors.New("invalid Jira issue export")
	}
	tasks, columns, losses := []any{}, []any{}, []any{}
	seen := map[string]bool{}
	categories := map[string]string{}
	for index, issue := range issues {
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
		comments, incomplete := jiraImportComments(fields)
		if len(comments) > 0 {
			task.set("comments", comments)
		}
		if incomplete {
			losses = append(losses, importUnsupportedObj("/issues/"+strconv.Itoa(index)+"/fields/comment",
				"the Jira export contains only part of this issue's comments"))
		}
		tasks = append(tasks, task)
	}
	for i, raw := range columns {
		column := raw.(jsObject)
		if category := categories[strOf(jsGet(column, "title"))]; category != "" {
			column.set("jira_category", category)
			columns[i] = column
		}
	}
	return jsObject{
		{"board", jsObject{{"name", jsOr(jsGet(jsGet(data, "board"), "name"), "Imported Jira Board")}}},
		{"columns", columns}, {"swimlanes", []any{jsObject{{"name", "Default"}}}}, {"tasks", tasks}, {"unsupported", losses},
	}, nil
}
