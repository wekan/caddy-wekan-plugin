package web

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// TickTick's backup CSV: Node.js WeKan's parseTickTickCsv
// (models/lib/ticktickCsvFormat.js) - its lists as swimlanes, its Kanban
// columns as lists, the content's ▫ and ▪ lines as a checklist, priorities,
// the folder, "won't do", and the dates of its own backup format.

func init() { importTextParsers["ticktick"] = parseTickTickCsv }

var (
	ticktickPriority = map[string]string{"1": "Low", "3": "Medium", "5": "High"}
	ticktickStampRe  = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})([+-]\d{2}):?(\d{2})$`)
)

// ticktickDate is WeKan's: a backup timestamp as ISO, or - for an all-day
// date - that day, in the task's time zone when Go knows it, at UTC midnight.
func ticktickDate(value string, allDay bool, timezone string) (string, bool) {
	m := ticktickStampRe.FindStringSubmatch(jsTrim(value))
	if m == nil {
		return "", false
	}
	t, ok := jsDateTime(m[1] + m[2] + ":" + m[3])
	if !ok {
		return "", false
	}
	if !allDay {
		return jsISO(t), true
	}
	day := jsISO(t)[:10]
	if timezone != "" {
		if loc, err := time.LoadLocation(timezone); err == nil {
			day = t.In(loc).Format("2006-01-02")
		}
	}
	return day + "T00:00:00.000Z", true
}

func parseTickTickCsv(text string) (jsObject, error) {
	rows, err := jsReadCsv(text, "TickTick", ',')
	if err != nil {
		return nil, err
	}
	headerAt := -1
	for i, cells := range rows {
		if len(cells) > 0 && jsTrim(cells[0]) == "Folder Name" {
			headerAt = i
			break
		}
	}
	if headerAt == -1 {
		return nil, errors.New(`TickTick backup has no header row starting with "Folder Name"`)
	}
	header := csvHeader{}
	for _, c := range rows[headerAt] {
		header = append(header, jsTrim(c))
	}
	if header.col("Title") == -1 || header.col("List Name") == -1 {
		return nil, errors.New("TickTick backup needs the List Name and Title columns")
	}
	get := func(cells []string, name string) string { return header.cell(cells, name) }
	var columns, swimlanes []string
	tasks, unsupported := []any{}, []any{}
	for index, cells := range rows[headerAt+1:] {
		at := "/row/" + strconv.Itoa(headerAt+index+2)
		title := get(cells, "Title")
		if title == "" {
			unsupported = append(unsupported, importUnsupportedObj(at, "a TickTick row without a title is not imported"))
			continue
		}
		lane := jsOr(get(cells, "List Name"), "Default").(string)
		swimlanes = addOnce(swimlanes, lane)
		list := jsOr(get(cells, "Column Name"), "Tasks").(string)
		columns = addOnce(columns, list)
		allDay := get(cells, "Is All Day") == "true"
		date := func(name string) string {
			value := get(cells, name)
			iso, ok := ticktickDate(value, allDay && (name == "Start Date" || name == "Due Date"), get(cells, "Timezone"))
			if value != "" && !ok {
				unsupported = append(unsupported, importUnsupportedObj(at+"/"+name, `TickTick date "`+value+`" is not in its backup format`))
			}
			return iso
		}
		items := []any{}
		var description []string
		content := ""
		if i := header.col("Content"); i != -1 && i < len(cells) {
			content = cells[i]
		}
		for _, line := range jsLines(content) {
			if strings.HasPrefix(line, "▫") || strings.HasPrefix(line, "▪") {
				item := jsObject{}
				item.set("title", jsTrim(line[len("▫"):]))
				item.set("done", strings.HasPrefix(line, "▪"))
				items = append(items, item)
			} else {
				description = append(description, line)
			}
		}
		tags := splitTrimmed(get(cells, "Tags"), ",")
		custom := jsObject{}
		priority := get(cells, "Priority")
		if p := ticktickPriority[priority]; p != "" {
			custom.set("Priority", p)
		} else if priority != "" && priority != "0" {
			unsupported = append(unsupported, importUnsupportedObj(at+"/Priority", `TickTick priority "`+priority+`" is not 0, 1, 3 or 5`))
		}
		if f := get(cells, "Folder Name"); f != "" {
			custom.set("Folder", f)
		}
		status := jsOr(get(cells, "Status"), "0").(string)
		switch status {
		case "-1":
			tags = append(tags, "won't do")
		case "0", "1", "2":
		default:
			unsupported = append(unsupported, importUnsupportedObj(at+"/Status", `TickTick status "`+status+`" is not 0, 1 or 2; imported as open`))
		}
		if get(cells, "Reminder") != "" {
			unsupported = append(unsupported, importUnsupportedObj(at+"/Reminder", "TickTick reminders have no WeKan place"))
		}
		if r := get(cells, "Repeat"); r != "" {
			unsupported = append(unsupported, importUnsupportedObj(at+"/Repeat", `repeating task "`+r+`" is imported once`))
		}
		ended := ""
		if status == "1" || status == "2" {
			ended = date("Completed Time")
		}
		t := jsObject{}
		t.set("title", title)
		t.set("description", jsTrim(strings.Join(description, "\n")))
		t.set("column_name", list)
		t.set("swimlane_name", lane)
		t.set("tags", tags)
		if v := get(cells, "taskId"); v != "" {
			t.set("ref", v)
		}
		if v := get(cells, "parentId"); v != "" {
			t.set("parent_ref", v)
		}
		for _, f := range []struct{ key, col string }{{"date_started", "Start Date"}, {"date_due", "Due Date"}, {"date_creation", "Created Time"}} {
			if v := date(f.col); v != "" {
				t.set(f.key, v)
			}
		}
		if ended != "" {
			t.set("date_end", ended)
		}
		if status == "2" {
			t.set("archived", true)
		}
		if len(items) > 0 {
			cl := jsObject{}
			cl.set("title", "Checklist")
			cl.set("items", items)
			t.set("checklists", []any{cl})
		}
		if len(custom) > 0 {
			t.set("custom_fields", custom)
		}
		tasks = append(tasks, t)
	}
	if len(swimlanes) == 0 {
		swimlanes = []string{"Default"}
	}
	lanes := []any{}
	for _, s := range swimlanes {
		o := jsObject{}
		o.set("name", s)
		lanes = append(lanes, o)
	}
	out := importDoc("Imported TickTick backup", csvColumns(columns), tasks, []any{}, unsupported)
	out.set("swimlanes", lanes)
	return out, nil
}
