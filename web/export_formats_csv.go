package web

import (
	"math/big"
	"strconv"
	"strings"
	"time"
)

// Leo's outline and the CSV that MeisterTask, Todoist, Linear and TickTick
// import, as Node.js WeKan's leoOutlineFormat.js, meistertaskCsvFormat.js,
// todoistCsvFormat.js, linearCsvFormat.js and ticktickCsvFormat.js write
// them - checked byte for byte against those (TestExternalFormatsAreWeKans).

// csvField is the formatters' field(): quoted when it holds a comma, a quote
// or a line break.
func csvField(v string) string {
	if strings.ContainsAny(v, "\",\r\n") {
		return `"` + strings.ReplaceAll(v, `"`, `""`) + `"`
	}
	return v
}

func writeCSVRows(rows [][]string, field func(string) string, eol string) string {
	lines := make([]string, len(rows))
	for i, cells := range rows {
		out := make([]string, len(cells))
		for k, c := range cells {
			out[k] = field(c)
		}
		lines[i] = strings.Join(out, ",")
	}
	return strings.Join(lines, eol) + eol
}

// --- Leo

var leoEscape = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")

// formatLeo is WeKan's: lists, cards and checklists as nested nodes, each
// numbered as it is made - a node before its children.
func formatLeo(d extBoard) string {
	next := 0
	var bodies []string
	type kid func(indent string) string
	var node func(headline, body string, marked bool, kids []kid, indent string) string
	node = func(headline, body string, marked bool, kids []kid, indent string) string {
		next++
		gnx := "wekan.1." + strconv.Itoa(next)
		if body != "" {
			bodies = append(bodies, `<t tx="`+gnx+`">`+leoEscape.Replace(body)+"</t>")
		}
		mark := ""
		if marked {
			mark = ` a="M"`
		}
		open := indent + `<v t="` + gnx + `"` + mark + "><vh>" + leoEscape.Replace(headline) + "</vh>"
		if len(kids) == 0 {
			return open + "</v>"
		}
		parts := make([]string, len(kids))
		for k, f := range kids {
			parts[k] = f(indent + "  ")
		}
		return open + "\n" + strings.Join(parts, "\n") + "\n" + indent + "</v>"
	}
	var vnodes []string
	for _, l := range d.Lists {
		var cards []kid
		for _, i := range d.Items {
			if i.ListTitle != l.Title {
				continue
			}
			i := i
			cards = append(cards, func(indent string) string {
				var cls []kid
				for _, c := range i.Checklists {
					c := c
					cls = append(cls, func(ind string) string {
						var xs []kid
						for _, x := range c.Items {
							x := x
							xs = append(xs, func(in string) string { return node(x.Title, "", x.Done, nil, in) })
						}
						return node(c.Title, "", false, xs, ind)
					})
				}
				return node(i.Title, i.Description, isClosedList(i.ListTitle), cls, indent)
			})
		}
		vnodes = append(vnodes, node(l.Title, "", false, cards, ""))
	}
	lines := []string{`<?xml version="1.0" encoding="utf-8"?>`, `<leo_file xmlns:leo="http://leoeditor.com/namespaces/leo-python-editor/1.1">`,
		`<leo_header file_format="2" wekan_board="` + leoEscape.Replace(d.Board.Title) + `"/>`, "<vnodes>"}
	lines = append(lines, vnodes...)
	lines = append(lines, "</vnodes>", "<tnodes>")
	lines = append(lines, bodies...)
	lines = append(lines, "</tnodes>", "</leo_file>", "")
	return strings.Join(lines, "\n")
}

// --- MeisterTask

func formatMeisterTaskCsv(d extBoard) string {
	project := d.Board.Title
	if project == "" {
		project = "WeKan board"
	}
	rows := [][]string{{"project", "section", "name", "notes", "due_date", "status", "tags"}}
	for _, i := range d.Items {
		section := i.ListTitle
		if section == "" {
			section = "No section"
		}
		name := jsTrim(i.Title)
		if name == "" {
			name = "Untitled"
		}
		due := ""
		if t := parseISO(i.DueAt); !t.IsZero() {
			due = t.Format("2006-01-02T15:04:05") + "+00:00"
		}
		status := "1"
		if i.EndAt != "" {
			status = "2"
		}
		var tags []string
		for _, l := range i.Labels {
			if t := jsTrim(strings.ReplaceAll(l, ";", ",")); t != "" {
				tags = append(tags, t)
			}
		}
		rows = append(rows, []string{project, section, name, i.Description, due, status, strings.Join(tags, "; ")})
	}
	return writeCSVRows(rows, csvField, "\r\n")
}

// --- Todoist

var todoistColumns = []string{"TYPE", "CONTENT", "DESCRIPTION", "PRIORITY", "INDENT", "AUTHOR", "RESPONSIBLE",
	"DATE", "DATE_LANG", "TIMEZONE", "DURATION", "DURATION_UNIT", "DEADLINE", "DEADLINE_LANG"}

// todoistDate is csvDate(): "2026-10-10", or with the time when it is not
// midnight UTC.
func todoistDate(iso string) string {
	t := parseISO(iso)
	if t.IsZero() {
		return ""
	}
	if c := t.Format("15:04"); c != "00:00" {
		return t.Format("2006-01-02") + " " + c
	}
	return t.Format("2006-01-02")
}

func formatTodoistCsv(d extBoard) string {
	row := func(values map[string]string) []string {
		out := make([]string, len(todoistColumns))
		for k, c := range todoistColumns {
			out[k] = values[c]
		}
		return out
	}
	rows := [][]string{todoistColumns, row(map[string]string{"TYPE": "meta", "CONTENT": "view_style=board"})}
	section := func(i extItem) string {
		if i.ListTitle == "" {
			return "No section"
		}
		return i.ListTitle
	}
	var lists []string
	for _, i := range d.Items {
		if !contains(lists, section(i)) {
			lists = append(lists, section(i))
		}
	}
	for _, list := range lists {
		if list != "No section" {
			rows = append(rows, row(map[string]string{"TYPE": "section", "CONTENT": list}))
		}
		for _, i := range d.Items {
			if section(i) != list {
				continue
			}
			priority := ""
			var tags []string
			for _, l := range i.Labels {
				if len(l) == 2 && l[0] == 'p' && l[1] >= '1' && l[1] <= '3' {
					if priority == "" {
						priority = l[1:]
					}
					continue
				}
				tags = append(tags, "@"+jsCollapse(jsTrim(l), "_"))
			}
			if priority == "" {
				priority = "4"
			}
			title := jsTrim(jsCollapse(i.Title, " "))
			if title == "" {
				title = "Untitled"
			}
			v := map[string]string{"TYPE": "task", "CONTENT": strings.Join(append([]string{title}, tags...), " "), "DESCRIPTION": i.Description,
				"PRIORITY": priority, "INDENT": "1", "RESPONSIBLE": i.Owner}
			start, due := todoistDate(i.StartAt), todoistDate(i.DueAt)
			switch {
			case start != "" && due != "":
				v["DATE"], v["DEADLINE"] = start, due
			case due != "":
				v["DATE"] = due
			}
			if v["DATE"] != "" {
				v["DATE_LANG"], v["TIMEZONE"] = "en", "UTC"
			}
			if v["DEADLINE"] != "" {
				v["DEADLINE_LANG"] = "en"
			}
			rows = append(rows, row(v))
			for _, x := range checklistEntries(i) {
				// Todoist exports no completed tasks, so a finished item is left out too.
				if !x.Done {
					rows = append(rows, row(map[string]string{"TYPE": "task", "CONTENT": x.Title, "PRIORITY": "4", "INDENT": "2"}))
				}
			}
			for _, c := range i.Comments {
				if jsTrim(c.Text) != "" {
					rows = append(rows, row(map[string]string{"TYPE": "note", "CONTENT": c.Text, "AUTHOR": c.Author}))
				}
			}
		}
	}
	return writeCSVRows(rows, csvField, "\r\n")
}

// --- Linear

var linearColumns = []string{"ID", "Team", "Title", "Description", "Status", "Estimate", "Priority", "Project ID",
	"Project", "Creator", "Assignee", "Labels", "Cycle Number", "Cycle Name", "Cycle Start", "Cycle End", "Created",
	"Updated", "Started", "Triaged", "Completed", "Canceled", "Archived", "Due Date", "Parent issue", "Initiatives",
	"Project Milestone ID", "Project Milestone", "SLA Status"}

// linearField is Linear's field(): a value a spreadsheet would read as a
// formula starts with "'".
func linearField(v string) string {
	if r := []rune(v); len(r) > 0 && strings.ContainsRune("+-=@∑√∏<>＜＞≤≥＝≠±÷×", r[0]) {
		v = "'" + v
	}
	return csvField(v)
}

func formatLinearCsv(d extBoard) string {
	rows := [][]string{linearColumns}
	for _, i := range d.Items {
		title := jsTrim(i.Title)
		if title == "" {
			title = "Untitled"
		}
		status := i.ListTitle
		if status == "" {
			status = "No status"
		}
		team := ""
		if i.SwimlaneTitle != "" && i.SwimlaneTitle != "Default" {
			team = i.SwimlaneTitle
		}
		creator := i.Creator
		if creator == "" {
			creator = i.RequestedBy
		}
		labels := make([]string, len(i.Labels))
		for k, l := range i.Labels {
			labels[k] = strings.ReplaceAll(l, ",", " ")
		}
		due := ""
		if i.DueAt != "" {
			due = isoDate(parseISO(i.DueAt))[:10]
		}
		v := map[string]string{"ID": i.CardID, "Team": team, "Title": title, "Description": i.Description, "Status": status,
			"Priority": "No priority", "Creator": creator, "Assignee": i.Owner, "Labels": strings.Join(labels, ", "),
			"Created": isoDate(parseISO(i.CreatedAt)), "Started": isoDate(parseISO(i.StartAt)), "Completed": isoDate(parseISO(i.EndAt)),
			"Due Date": due, "Parent issue": i.ParentCardID}
		cells := make([]string, len(linearColumns))
		for k, c := range linearColumns {
			cells[k] = v[c]
		}
		rows = append(rows, cells)
	}
	return writeCSVRows(rows, linearField, "\r\n")
}

// --- TickTick

var ticktickColumns = []string{"Folder Name", "List Name", "Title", "Kind", "Tags", "Content", "Is Check list",
	"Start Date", "Due Date", "Reminder", "Repeat", "Priority", "Status", "Created Time", "Completed Time", "Order",
	"Timezone", "Is All Day", "Is Floating", "Column Name", "Column Order", "View Mode", "taskId", "parentId", "projectKind"}

func ticktickQuoted(v string) string { return `"` + strings.ReplaceAll(v, `"`, `""`) + `"` }

func ticktickStamp(iso string) string {
	if t := parseISO(iso); !t.IsZero() {
		return t.Format("2006-01-02T15:04:05") + "+0000"
	}
	return ""
}

// formatTickTickCsv is WeKan's: a backup TickTick's Import backup reads, the
// board as one TickTick list and its lists as that list's kanban columns.
func formatTickTickCsv(d extBoard, now time.Time) string {
	listName := d.Board.Title
	if listName == "" {
		listName = "WeKan board"
	}
	order := d.listTitles()
	ids := map[string]string{}
	for k, i := range d.Items {
		ids[i.CardID] = strconv.Itoa(k + 1) // a Map's set: the last card of an id wins
	}
	header := make([]string, len(ticktickColumns))
	for k, c := range ticktickColumns {
		header[k] = ticktickQuoted(c)
	}
	lines := []string{ticktickQuoted("Date: " + now.UTC().Format("2006-01-02") + "+0000"), ticktickQuoted("Version: 7.2"),
		ticktickQuoted("Status: \n0 Normal\n1 Completed\n2 Archived"), strings.Join(header, ",")}
	for k, i := range d.Items {
		entries := checklistEntries(i)
		var content []string
		if i.Description != "" {
			content = append(content, i.Description)
		}
		for _, x := range entries {
			mark := "▫"
			if x.Done {
				mark = "▪"
			}
			if s := mark + x.Title; s != "" {
				content = append(content, s)
			}
		}
		var labels []string
		for _, l := range i.Labels {
			if t := jsTrim(strings.ReplaceAll(l, ",", " ")); t != "" {
				labels = append(labels, t)
			}
		}
		wontDo := contains(labels, "won't do")
		var tags []string
		for _, l := range labels {
			if l != "won't do" {
				tags = append(tags, l)
			}
		}
		allDay := true
		for _, v := range []string{i.StartAt, i.DueAt} {
			if v != "" && parseISO(v).Format("15:04:05") != "00:00:00" {
				allDay = false
			}
		}
		title := jsTrim(i.Title)
		if title == "" {
			title = "Untitled"
		}
		kind, check := "TEXT", "N"
		if len(entries) > 0 {
			kind, check = "CHECKLIST", "Y"
		}
		status := "0"
		if wontDo {
			status = "-1"
		} else if i.EndAt != "" {
			status = "1"
		}
		column := i.ListTitle
		if column == "" {
			column = "Tasks"
		}
		columnOrder := 0
		for n, t := range order {
			if t == i.ListTitle {
				columnOrder = n
				break
			}
		}
		taskID := ids[i.CardID]
		if taskID == "" {
			taskID = strconv.Itoa(k + 1)
		}
		parent := ""
		if i.ParentCardID != "" {
			parent = ids[i.ParentCardID]
		}
		v := map[string]string{"List Name": listName, "Title": title, "Kind": kind, "Tags": strings.Join(tags, ", "),
			"Content": strings.Join(content, "\n"), "Is Check list": check, "Start Date": ticktickStamp(i.StartAt),
			"Due Date": ticktickStamp(i.DueAt), "Priority": "0", "Status": status, "Created Time": ticktickStamp(i.CreatedAt),
			"Order": strconv.FormatInt(int64(k)*1099511627776, 10), "Timezone": "UTC", "Is Floating": "false",
			"Column Name": column, "Column Order": strconv.Itoa(columnOrder), "View Mode": "kanban", "taskId": taskID,
			"parentId": parent, "projectKind": "TASK"}
		if i.EndAt != "" {
			v["Completed Time"] = ticktickStamp(i.EndAt)
		}
		if i.StartAt != "" || i.DueAt != "" {
			v["Is All Day"] = strconv.FormatBool(allDay)
		}
		cells := make([]string, len(ticktickColumns))
		for n, c := range ticktickColumns {
			cells[n] = ticktickQuoted(v[c])
		}
		lines = append(lines, strings.Join(cells, ","))
	}
	return strings.Join(lines, "\n") + "\n"
}

// repeatCells is repeat(value, count).
func repeatCells(v string, n int) []string {
	out := []string{}
	for k := 0; k < n; k++ {
		out = append(out, v)
	}
	return out
}

// monthDayYear is "Jun 2, 2026", read at UTC, as Pivotal and Quire write a date.
func monthDayYear(iso string) string {
	t := parseISO(iso)
	if t.IsZero() {
		return ""
	}
	return t.Format("Jan") + " " + strconv.Itoa(t.Day()) + ", " + strconv.Itoa(t.Year())
}

// uniqueNonEmpty is [...new Set(values.filter(Boolean))].
func uniqueNonEmpty(values []string) []string {
	out := []string{}
	for _, v := range values {
		if v != "" && !contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// --- ClickUp

var clickupColumns = []string{"Task ID", "Task Link", "Task Type", "Task Custom ID", "Task Name", "Task Content", "Status",
	"Date created", "Date created Text", "Due date", "Due date Text", "Start date", "Start date Text", "Parent ID", "Subtask IDs",
	"Attachments", "Assignees", "Tags", "Priority", "List Name", "Folder Name/Path", "Space Name", "Time Estimated",
	"Time Estimated Text", "Checklists", "Comments", "Assigned Comments", "Time Spent", "Time Spent Text", "Rolled Up Time",
	"Rolled Up Time Text", "Checklist"}

// clickupBracket is bracket(): "[A,B]", each without , [ or ].
func clickupBracket(values []string) string {
	if len(values) == 0 {
		return ""
	}
	clean := strings.NewReplacer(",", " ", "[", " ", "]", " ")
	out := make([]string, len(values))
	for k, v := range values {
		out[k] = jsTrim(clean.Replace(v))
	}
	return "[" + strings.Join(out, ",") + "]"
}

func clickupMillis(iso string) string {
	if t := parseISO(iso); !t.IsZero() {
		return strconv.FormatInt(t.UnixMilli(), 10)
	}
	return ""
}

func formatClickUpCsv(d extBoard) string {
	ids := map[string]bool{}
	for _, i := range d.Items {
		ids[i.CardID] = true
	}
	children := map[string][]string{}
	for _, i := range d.Items {
		if i.ParentCardID != "" && ids[i.ParentCardID] {
			children[i.ParentCardID] = append(children[i.ParentCardID], i.CardID)
		}
	}
	rows := [][]string{clickupColumns}
	for _, i := range d.Items {
		name := jsTrim(i.Title)
		if name == "" {
			name = "Untitled"
		}
		status := i.ListTitle
		if status == "" {
			status = "No status"
		}
		parent := ""
		if i.ParentCardID != "" && ids[i.ParentCardID] {
			parent = i.ParentCardID
		}
		list := ""
		if i.SwimlaneTitle != "" && i.SwimlaneTitle != "Default" {
			list = i.SwimlaneTitle
		}
		var checklist []string
		for _, x := range checklistEntries(i) {
			checklist = append(checklist, strings.ReplaceAll(x.Title, ",", " "))
		}
		v := map[string]string{"Task ID": i.CardID, "Task Type": "Task", "Task Name": name, "Task Content": i.Description, "Status": status,
			"Date created": clickupMillis(i.CreatedAt), "Date created Text": isoDate(parseISO(i.CreatedAt)),
			"Due date": clickupMillis(i.DueAt), "Due date Text": isoDate(parseISO(i.DueAt)),
			"Start date": clickupMillis(i.StartAt), "Start date Text": isoDate(parseISO(i.StartAt)),
			"Parent ID": parent, "Subtask IDs": clickupBracket(children[i.CardID]), "Attachments": "[]",
			"Assignees": clickupBracket(people(i)), "Tags": clickupBracket(i.Labels), "List Name": list,
			"Space Name": d.Board.Title, "Checklist": strings.Join(checklist, ",")}
		cells := make([]string, len(clickupColumns))
		for k, c := range clickupColumns {
			cells[k] = v[c]
		}
		rows = append(rows, cells)
	}
	return writeCSVRows(rows, csvField, "\r\n")
}

// --- Pivotal Tracker

var (
	pivotalStates  = []string{"unscheduled", "unstarted", "planned", "started", "finished", "delivered", "accepted", "rejected"}
	pivotalTypes   = []string{"feature", "bug", "chore", "epic", "release"}
	pivotalColumns = []string{"Title", "Labels", "Type", "Estimate", "Priority", "Current State",
		"Created at", "Accepted at", "Deadline", "Requested By", "Description"}
)

// spaceAroundCommas is s.replace(/\s*,\s*/g, ' ').
func spaceAroundCommas(s string) string {
	rs := []rune(s)
	var b strings.Builder
	for i := 0; i < len(rs); {
		j := i
		for j < len(rs) && jsSpace(rs[j]) {
			j++
		}
		if j < len(rs) && rs[j] == ',' {
			j++
			for j < len(rs) && jsSpace(rs[j]) {
				j++
			}
			b.WriteString(" ")
			i = j
			continue
		}
		if j > i {
			b.WriteString(string(rs[i:j]))
			i = j
			continue
		}
		b.WriteRune(rs[i])
		i++
	}
	return b.String()
}

func formatPivotalCsv(d extBoard) string {
	owners := func(i extItem) []string {
		p := uniqueNonEmpty(append([]string{i.Owner}, i.Assignees...))
		if len(p) > 5 {
			p = p[:5]
		}
		return p
	}
	tasks := func(i extItem) []extChecklistItem {
		var out []extChecklistItem
		for _, x := range checklistEntries(i) {
			if jsTrim(x.Title) != "" {
				out = append(out, x)
			}
		}
		return out
	}
	comments := func(i extItem) []extComment {
		var out []extComment
		for _, c := range i.Comments {
			if jsTrim(c.Text) != "" {
				out = append(out, c)
			}
		}
		return out
	}
	nOwners, nComments, nTasks := 1, 0, 0
	for _, i := range d.Items {
		nOwners, nComments, nTasks = max(nOwners, len(owners(i))), max(nComments, len(comments(i))), max(nTasks, len(tasks(i)))
	}
	header := append(append(append([]string{}, pivotalColumns...), repeatCells("Owned By", nOwners)...), repeatCells("Comment", nComments)...)
	for k := 0; k < nTasks; k++ {
		header = append(header, "Task", "Task Status")
	}
	rows := [][]string{header}
	guard := func(v string) string {
		if strings.HasPrefix(v, "=") {
			return "'" + v
		}
		return v
	}
	for _, i := range d.Items {
		var labels []string
		for _, l := range i.Labels {
			if t := jsTrim(l); t != "" {
				labels = append(labels, t)
			}
		}
		typeLabel := ""
		for _, l := range labels {
			if contains(pivotalTypes, strings.ToLower(l)) {
				typeLabel = l
				break
			}
		}
		typ := "feature"
		if typeLabel != "" {
			typ = strings.ToLower(typeLabel)
		}
		var rest []string
		for _, l := range labels {
			if l != typeLabel {
				rest = append(rest, spaceAroundCommas(l))
			}
		}
		state := strings.ToLower(jsTrim(i.ListTitle))
		if !contains(pivotalStates, state) {
			state = "unstarted"
			if i.EndAt != "" {
				state = "accepted"
			}
		}
		title := jsTrim(i.Title)
		if title == "" {
			title = "Untitled"
		}
		accepted, deadline := "", ""
		if state == "accepted" {
			accepted = monthDayYear(i.EndAt)
		}
		if typ == "release" {
			deadline = monthDayYear(i.DueAt)
		}
		requested := i.RequestedBy
		if requested == "" {
			requested = i.Creator
		}
		row := []string{title, strings.Join(rest, ", "), typ, "", "", state, monthDayYear(i.CreatedAt), accepted, deadline, requested, i.Description}
		owned := owners(i)
		row = append(append(row, owned...), repeatCells("", nOwners-len(owned))...)
		said := comments(i)
		for _, c := range said {
			if when := monthDayYear(c.Date); c.Author != "" && when != "" {
				row = append(row, c.Text+" ("+c.Author+" - "+when+")")
			} else {
				row = append(row, c.Text)
			}
		}
		row = append(row, repeatCells("", nComments-len(said))...)
		done := tasks(i)
		for _, x := range done {
			st := "not completed"
			if x.Done {
				st = "completed"
			}
			row = append(row, jsTrim(x.Title), st)
		}
		row = append(row, repeatCells("", (nTasks-len(done))*2)...)
		for k := range row {
			row[k] = guard(row[k])
		}
		rows = append(rows, row)
	}
	return writeCSVRows(rows, csvField, "\r\n")
}

// --- Quire

func formatQuireCsv(d extBoard) string {
	numbers := map[string]string{}
	for k, i := range d.Items {
		numbers[i.CardID] = "#" + strconv.Itoa(k+1) // a Map's set: the last card of an id wins
	}
	clean := func(v string) string { return jsTrim(strings.ReplaceAll(v, ",", " ")) }
	who := func(i extItem) []string {
		var out []string
		for _, p := range append([]string{i.Owner}, i.Assignees...) {
			out = append(out, clean(p))
		}
		return uniqueNonEmpty(out)
	}
	tags := func(i extItem) []string {
		var out []string
		for _, l := range i.Labels {
			out = append(out, clean(l))
		}
		return uniqueNonEmpty(out)
	}
	nWho, nTags := 1, 1
	for _, i := range d.Items {
		nWho, nTags = max(nWho, len(who(i))), max(nTags, len(tags(i)))
	}
	header := append(append([]string{"ID", "Parent", "Name", "Status", "Completed", "Priority", "Start", "Due"},
		repeatCells("Assignee", nWho)...), repeatCells("Tag", nTags)...)
	header = append(header, "Created", "Created by", "Description")
	rows := [][]string{header}
	for k, i := range d.Items {
		name := jsTrim(i.Title)
		if name == "" {
			name = "Untitled"
		}
		status := jsTrim(i.ListTitle)
		if status == "" {
			status = "To-Do"
		}
		parent := ""
		if i.ParentCardID != "" {
			parent = numbers[i.ParentCardID]
		}
		row := []string{"#" + strconv.Itoa(k+1), parent, name, status, monthDayYear(i.EndAt), "Medium", monthDayYear(i.StartAt), monthDayYear(i.DueAt)}
		w, t := who(i), tags(i)
		row = append(append(row, w...), repeatCells("", nWho-len(w))...)
		row = append(append(row, t...), repeatCells("", nTags-len(t))...)
		by := i.RequestedBy
		if by == "" {
			by = i.Creator
		}
		row = append(row, monthDayYear(i.CreatedAt), by, i.Description)
		rows = append(rows, row)
	}
	quire := func(v string) string {
		if strings.HasPrefix(v, "+") || strings.HasPrefix(v, "-") || strings.HasPrefix(v, "=") || strings.HasPrefix(v, "@") {
			v = "'" + v
		}
		return csvField(v)
	}
	return writeCSVRows(rows, quire, "\r\n")
}

// --- Redmine

var redmineRelations = []string{"Related to", "Blocks", "Blocked by", "Is duplicate of", "Has duplicate"}

var redmineRelationFor = map[string]string{"related-to": "Related to", "blocks": "Blocks", "is-blocked-by": "Blocked by",
	"duplicates": "Is duplicate of", "is-duplicated-by": "Has duplicate", "fixes": "Related to", "is-fixed-by": "Related to"}

var redmineColumns = append([]string{"Unique ID", "Project", "Tracker", "Status", "Priority", "Subject", "Description",
	"Author", "Assignee", "Category", "Target version", "Start date", "Due date", "Estimated time", "% Done", "Spent time",
	"Created", "Closed", "Parent task"}, redmineRelations...)

// jsToFixed2 is Number.prototype.toFixed(2): the nearest, a tie to the
// larger - not Go's ties to even.
func jsToFixed2(v float64) string {
	r := new(big.Rat).SetFloat64(v)
	if r == nil {
		return ""
	}
	r.Mul(r, big.NewRat(100, 1))
	n := new(big.Int).Div(r.Num(), r.Denom()) // floor for v >= 0
	if r.Sign() < 0 && new(big.Rat).SetInt(n).Cmp(r) != 0 {
		n.Sub(n, big.NewInt(1))
	}
	frac := new(big.Rat).Sub(r, new(big.Rat).SetInt(n))
	if frac.Cmp(big.NewRat(1, 2)) >= 0 {
		n.Add(n, big.NewInt(1))
	}
	neg := n.Sign() < 0
	digits := new(big.Int).Abs(n).String()
	for len(digits) < 3 {
		digits = "0" + digits
	}
	out := digits[:len(digits)-2] + "." + digits[len(digits)-2:]
	if neg {
		out = "-" + out
	}
	return out
}

func formatRedmineCsv(d extBoard) string {
	ids := map[string]bool{}
	for _, i := range d.Items {
		if i.CardID != "" {
			ids[i.CardID] = true
		}
	}
	rows := [][]string{redmineColumns}
	for _, i := range d.Items {
		relations := map[string][]string{}
		for _, dep := range i.Dependencies {
			if !ids[dep.CardID] {
				continue
			}
			name := redmineRelationFor[dep.Type]
			if name == "" {
				name = "Related to"
			}
			relations[name] = append(relations[name], dep.CardID)
		}
		project := ""
		if i.SwimlaneTitle != "" && i.SwimlaneTitle != "Default" {
			project = i.SwimlaneTitle
		}
		tracker := ""
		if len(i.Labels) > 0 {
			tracker = i.Labels[0]
		}
		status := i.ListTitle
		if status == "" {
			status = "New"
		}
		subject := jsTrim(i.Title)
		if subject == "" {
			subject = "Untitled"
		}
		author := i.RequestedBy
		if author == "" {
			author = i.Creator
		}
		minute := func(iso string) string {
			if t := parseISO(iso); !t.IsZero() {
				return t.Format("2006-01-02 15:04")
			}
			return ""
		}
		spent := ""
		if i.SpentTime > 0 {
			spent = jsToFixed2(i.SpentTime)
		}
		parent := ""
		if i.ParentCardID != "" && ids[i.ParentCardID] {
			parent = i.ParentCardID
		}
		v := map[string]string{"Unique ID": i.CardID, "Project": project, "Tracker": tracker, "Status": status, "Subject": subject,
			"Description": i.Description, "Author": author, "Assignee": i.Owner, "Start date": obsidianDay(i.StartAt),
			"Due date": obsidianDay(i.DueAt), "Spent time": spent, "Created": minute(i.CreatedAt), "Closed": minute(i.EndAt),
			"Parent task": parent}
		for _, name := range redmineRelations {
			v[name] = strings.Join(relations[name], ", ")
		}
		cells := make([]string, len(redmineColumns))
		for k, c := range redmineColumns {
			cells[k] = v[c]
		}
		rows = append(rows, cells)
	}
	return "\ufeff" + writeCSVRows(rows, csvField, "\r\n")
}

// --- Notion

func formatNotionCsv(d extBoard) string {
	lanes := map[string]bool{}
	lane := func(i extItem) string {
		if i.SwimlaneTitle == "" {
			return "Default"
		}
		return i.SwimlaneTitle
	}
	for _, i := range d.Items {
		lanes[lane(i)] = true
	}
	columns := []string{"Name", "Status", "Assignee", "Tags", "Start", "Due", "Description"}
	if len(lanes) > 1 {
		columns = append(columns, "Swimlane")
	}
	usDate := func(iso string) string {
		if t := parseISO(iso); !t.IsZero() {
			return t.Format("01/02/2006")
		}
		return ""
	}
	clean := func(v string) string { return jsTrim(strings.ReplaceAll(v, ",", " ")) }
	rows := [][]string{columns}
	for _, i := range d.Items {
		var who, tags []string
		for _, p := range append([]string{i.Owner}, i.Assignees...) {
			who = append(who, clean(p))
		}
		for _, l := range i.Labels {
			if t := clean(l); t != "" {
				tags = append(tags, t)
			}
		}
		name := jsTrim(i.Title)
		if name == "" {
			name = "Untitled"
		}
		v := map[string]string{"Name": name, "Status": i.ListTitle, "Assignee": strings.Join(uniqueNonEmpty(who), ", "),
			"Tags": strings.Join(tags, ", "), "Start": usDate(i.StartAt), "Due": usDate(i.DueAt), "Description": i.Description,
			"Swimlane": lane(i)}
		cells := make([]string, len(columns))
		for k, c := range columns {
			cells[k] = v[c]
		}
		rows = append(rows, cells)
	}
	return writeCSVRows(rows, csvField, "\r\n")
}
