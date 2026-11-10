package web

import (
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The tools' Excel imports - Wrike's and Teamwork.com's templates - as
// Node.js WeKan's wrikeFormat.js and teamworkFormat.js write their rows and
// server/lib/wrikeWorkbook.js and teamworkWorkbook.js their workbooks. The
// rows are checked against WeKan's formatters (testdata/external), the
// workbooks by reading them back.

var (
	wrikeWorkflowColumns = []string{"Default task workflow", "Default project workflow", "Workflow", "Status", "Custom Status"}
	wrikeExportColumns   = append(append([]string{"Key", "Parent Task", "Title"}, wrikeWorkflowColumns...),
		"Priority", "Assigned To", "Start Date", "Duration", "End Date", "Depends On", "Start Date Constraint", "Description")
	teamworkColumns = []string{"Tasklist", "Task", "Description", "Assign to", "Start date", "Due date",
		"Priority", "Estimated time", "Tags", "Status"}
)

type workbookRows struct {
	Sheet string  `json:"sheet,omitempty"`
	Name  string  `json:"name,omitempty"`
	Rows  [][]any `json:"rows"`
}

// formatWrikeRows is WeKan's: the import template on a "Tasks" sheet, a
// folder row above each swimlane's cards but Default's, sequential Keys, each
// card's list as its custom status in the workflow named after the board.
func formatWrikeRows(d extBoard, rules []bson.M) workbookRows {
	var lists []bson.M
	if len(d.Lists) > 0 {
		for _, l := range d.Lists {
			lists = append(lists, bson.M{"title": l.Title, "color": l.Color})
		}
	} else {
		var titles []string
		for _, i := range d.Items {
			if i.ListTitle != "" && !contains(titles, i.ListTitle) {
				titles = append(titles, i.ListTitle)
				lists = append(lists, bson.M{"title": i.ListTitle})
			}
		}
	}
	wf, statusOf := wrikeWorkflowParts(d.Board.Title, lists, rules)
	workflowName := wf[1].Value.([]any)[0].(jsObject)[0].Value.(string)
	rows := [][]any{toAny(wrikeExportColumns)}
	lane := func(i extItem) string {
		if i.SwimlaneTitle != "" && i.SwimlaneTitle != "Default" {
			return i.SwimlaneTitle
		}
		return "Default"
	}
	var lanes []string
	for _, i := range d.Items {
		if !contains(lanes, lane(i)) {
			lanes = append(lanes, lane(i))
		}
	}
	// Default first: its cards need no folder row (a stable sort).
	if n := indexOf(lanes, "Default"); n > 0 {
		lanes = append(append([]string{"Default"}, lanes[:n]...), lanes[n+1:]...)
	}
	keyOf := map[string]int{}
	type entry struct {
		folder string
		item   *extItem
		key    int
	}
	var order []entry
	next := 1
	for _, ln := range lanes {
		if ln != "Default" {
			order = append(order, entry{folder: ln, key: next})
			next++
		}
		for n := range d.Items {
			if lane(d.Items[n]) != ln {
				continue
			}
			keyOf[d.Items[n].CardID] = next
			order = append(order, entry{item: &d.Items[n], key: next})
			next++
		}
	}
	for _, e := range order {
		if e.item == nil {
			var parts []string
			for _, p := range strings.Split(e.folder, "/") {
				if t := jsTrim(p); t != "" {
					parts = append(parts, t)
				}
			}
			path := strings.Join(parts, "/")
			if path == "" {
				path = "Folder"
			}
			row := make([]any, len(wrikeExportColumns))
			for c, name := range wrikeExportColumns {
				switch name {
				case "Key":
					row[c] = e.key
				case "Title":
					row[c] = "/" + path + "/"
				case "Default task workflow":
					row[c] = workflowName
				default:
					row[c] = ""
				}
			}
			rows = append(rows, row)
			continue
		}
		i := *e.item
		title := jsTrim(i.Title)
		if title == "" {
			title = "Untitled"
		}
		var parent any = ""
		if n, ok := keyOf[i.ParentCardID]; ok && i.ParentCardID != "" {
			parent = n
		}
		statusName, group := statusOf(i.ListTitle)
		v := map[string]any{"Key": e.key, "Parent Task": parent, "Title": title, "Workflow": workflowName, "Status": group,
			"Custom Status": statusName, "Priority": "", "Assigned To": strings.Join(people(i), ", "), "Start Date": obsidianDay(i.StartAt),
			"Duration": "", "End Date": obsidianDay(i.DueAt), "Depends On": "", "Start Date Constraint": "", "Description": i.Description}
		row := make([]any, len(wrikeExportColumns))
		for c, name := range wrikeExportColumns {
			if x, ok := v[name]; ok {
				row[c] = x
			} else {
				row[c] = ""
			}
		}
		rows = append(rows, row)
	}
	return workbookRows{Sheet: "Tasks", Rows: rows}
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for k, s := range ss {
		out[k] = s
	}
	return out
}

func indexOf(list []string, s string) int {
	for k, x := range list {
		if x == s {
			return k
		}
	}
	return -1
}

// teamworkDay is a date as a Date at 00:00 UTC, or "".
func teamworkDay(iso string) any {
	t := parseISO(iso)
	if t.IsZero() {
		return ""
	}
	return jsDate(time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC))
}

// formatTeamworkSheet is WeKan's: Teamwork.com's task import template, a
// subtask after its parent with one "-" a level when both are in one list.
func formatTeamworkSheet(d extBoard) workbookRows {
	byID := map[string]int{}
	for n, i := range d.Items {
		byID[i.CardID] = n // a Map's set: the last card of an id wins
	}
	parentOf := func(n int) int {
		i := d.Items[n]
		p, ok := byID[i.ParentCardID]
		if i.ParentCardID == "" || !ok || p == n || d.Items[p].ListTitle != i.ListTitle {
			return -1
		}
		return p
	}
	children := map[int][]int{}
	var roots []int
	for n := range d.Items {
		if p := parentOf(n); p >= 0 {
			children[p] = append(children[p], n)
		} else {
			roots = append(roots, n)
		}
	}
	rows := [][]any{toAny(teamworkColumns)}
	seen := map[int]bool{}
	var write func(n, depth int)
	write = func(n, depth int) {
		if seen[n] {
			return
		}
		seen[n] = true
		i := d.Items[n]
		title := strings.TrimLeftFunc(i.Title, func(r rune) bool { return r == '-' || r == '#' || r == '>' || jsSpace(r) })
		if title = jsTrim(title); title == "" {
			title = "Untitled"
		}
		list := i.ListTitle
		if list == "" {
			list = "Tasks"
		}
		var tags []string
		for _, l := range i.Labels {
			if t := jsTrim(strings.ReplaceAll(l, ",", " ")); t != "" {
				tags = append(tags, t)
			}
		}
		status := "Active"
		if i.DueComplete || i.EndAt != "" {
			status = "Complete"
		}
		rows = append(rows, []any{list, strings.Repeat("-", depth) + title, i.Description, strings.Join(people(i), ", "),
			teamworkDay(i.StartAt), teamworkDay(i.DueAt), "", "", strings.Join(tags, ", "), status})
		for _, c := range children[n] {
			write(c, depth+1)
		}
	}
	for _, n := range roots {
		write(n, 0)
	}
	for n := range d.Items {
		write(n, 0) // a parent loop leaves cards no root reaches
	}
	name := d.Board.Title
	if name == "" {
		name = "Tasks"
	}
	return workbookRows{Name: name, Rows: rows}
}

// The workbooks, as WeKan's writers write them.
func wrikeWorkbook(r workbookRows) ([]byte, error) {
	var dates []int
	for c, h := range r.Rows[0] {
		if s, _ := h.(string); contains([]string{"start date", "end date", "start date constraint"}, strings.ToLower(jsTrim(s))) {
			dates = append(dates, c)
		}
	}
	return xlsxBook([]xlsxSpec{{Name: "Tasks", Rows: r.Rows, BoldRow: 1, DateCols: dates}})
}

func teamworkWorkbook(r workbookRows) ([]byte, error) {
	title := xlsxSheetName(jsTrim(strings.Map(func(c rune) rune {
		if strings.ContainsRune(`\/?*[]:`, c) {
			return ' '
		}
		return c
	}, r.Name)), "Tasks")
	return xlsxBook([]xlsxSpec{{Name: title, Rows: r.Rows, BoldRow: 1}})
}
