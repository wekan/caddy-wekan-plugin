package web

import "strconv"

// Jira's issues collection, as Node.js WeKan's externalExportFormatters.js
// jira writes it - the shape its Jira import reads back: each card an issue
// keyed WEKAN-n, its list the status, labels, dates, people, its parent, its
// checklists as subtasks, its comments, and the time spent as Jira's time
// tracking. Weddy leaves custom fields out of exports (WeKan lets them out
// only after its admin-only check) and has no sprints yet, so the estimate,
// the custom fields and the Scrum metadata are not here.

func init() { apiFormatters["jira"] = formatJira }

// jsOptional is a value JSON.stringify writes, or undefined (left out) for
// what WeKan's item has none of.
func jsOptional(s string) any {
	if s == "" {
		return undefinedValue{}
	}
	return s
}

func formatJira(d extBoard) any {
	keys := map[string]string{}
	for idx, i := range d.Items {
		keys[i.CardID] = "WEKAN-" + strconv.Itoa(idx+1)
	}
	issues := []any{}
	for _, i := range d.Items {
		status := jsObject{}
		status.set("name", i.ListTitle)
		f := jsObject{}
		f.set("summary", i.Title)
		f.set("description", i.Description)
		f.set("status", status)
		labels := append([]string{}, i.Labels...)
		f.set("labels", labels)
		f.set("duedate", jsOptional(i.DueAt))
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
		issue := jsObject{}
		issue.set("key", keys[i.CardID])
		issue.set("fields", f)
		issues = append(issues, issue)
	}
	board := jsObject{}
	board.set("name", d.Board.Title)
	out := jsObject{}
	out.set("board", board)
	out.set("issues", issues)
	return out
}
