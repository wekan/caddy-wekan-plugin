package web

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Microsoft Planner's "Export plan to Excel" workbook: Node.js WeKan's
// readPlannerWorkbook (server/lib/plannerWorkbook.js) and parsePlannerRows
// (models/lib/plannerFormat.js) - its Tasks sheet's rows, buckets as lists,
// people, labels, progress, priority, checklist and dates, in the day order
// the file's own dates give away.

func init() { importTextParsers["planner"] = parsePlannerWorkbook }

const maxPlannerRows = 20000

var (
	plannerProgress = []string{"Not started", "In progress", "Completed"}
	plannerPriority = []string{"Urgent", "Important", "Medium", "Low"}
	plannerNumeric  = regexp.MustCompile(`^(\d{1,2})([/.-])(\d{1,2})([/.-])(\d{4})$`) // \2 checked by hand
	plannerISO      = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}(T[\d:.]+Z?)?$`)
	plannerDone     = regexp.MustCompile(`^(\d+)\s*/\s*\d+$`)
	plannerYes      = regexp.MustCompile(`(?i)^(true|yes)$`)
)

// excelCellText is the cellText of WeKan's workbook parsers: a Date as ISO,
// rich text's runs, a hyperlink's text, a formula's result, anything else as
// trimmed text.
func excelCellText(v any) string {
	switch x := v.(type) {
	case nil, undefinedValue:
		return ""
	case jsDate:
		return jsISO(time.Time(x))
	case jsObject:
		if runs, ok := jsGet(x, "richText").([]any); ok {
			var b strings.Builder
			for _, r := range runs {
				b.WriteString(jsStr(jsOr(jsGet(r, "text"), "")))
			}
			return b.String()
		}
		if t := jsGetOrUndef(x, "text"); !isUndef(t) {
			return excelCellText(t)
		}
		if r := jsGetOrUndef(x, "result"); !isUndef(r) {
			return excelCellText(r)
		}
		return ""
	}
	return jsTrim(jsStr(v))
}

func isUndef(v any) bool { _, ok := v.(undefinedValue); return ok }

// excelWorkbookBytes is a workbook the import page uploaded (its bytes) or
// importBoard was given ({ excelBase64 }, decoded by importInput).
func excelWorkbookBytes(text, name string) ([]byte, error) {
	if text == "" {
		return nil, errors.New(name + " workbook is empty")
	}
	return []byte(text), nil
}

func plannerSplit(text string) []any { return splitTrimmed(text, ";") }

// plannerDateOrder is WeKan's: dmy when a date says so, mdy otherwise.
func plannerDateOrder(texts []string) (order string, conflict, guessed bool) {
	dayFirst, monthFirst, ambiguous := false, false, false
	for _, t := range texts {
		m := plannerNumeric.FindStringSubmatch(jsTrim(t))
		if m == nil || m[2] != m[4] {
			continue
		}
		first, _ := strconv.Atoi(m[1])
		second, _ := strconv.Atoi(m[3])
		switch {
		case m[2] == ".":
			dayFirst = true
		case first > 12:
			dayFirst = true
		case second > 12:
			monthFirst = true
		case first != second:
			ambiguous = true
		}
	}
	switch {
	case dayFirst && monthFirst:
		return "mdy", true, false
	case dayFirst:
		return "dmy", false, false
	}
	return "mdy", false, ambiguous && !monthFirst
}

func plannerDate(text, order string) (string, bool) {
	value := jsTrim(text)
	if value == "" {
		return "", false
	}
	if plannerISO.MatchString(value) {
		if len(value) == 10 {
			value += "T00:00:00.000Z"
		}
		t, ok := jsDateTime(value)
		if !ok {
			return "", false
		}
		return jsISO(t), true
	}
	m := plannerNumeric.FindStringSubmatch(value)
	if m == nil || m[2] != m[4] {
		return "", false
	}
	n := func(s string) int { v, _ := strconv.Atoi(s); return v }
	month, day := n(m[1]), n(m[3])
	if order == "dmy" {
		month, day = n(m[3]), n(m[1])
	}
	year := n(m[5])
	if year < 100 {
		year += 1900 // Date.UTC's
	}
	d := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
	if int(d.Month()) != month || d.Day() != day {
		return "", false
	}
	return jsISO(d), true
}

func parsePlannerWorkbook(text string) (jsObject, error) {
	data, err := excelWorkbookBytes(text, "Planner")
	if err != nil {
		return nil, err
	}
	sheets, err := readXlsxValues(data)
	if err != nil {
		return nil, err
	}
	sheet := sheets[0]
	for _, s := range sheets {
		if s.Name == "Tasks" {
			sheet = s
			break
		}
	}
	return parsePlannerRows(sheet.Rows)
}

func parsePlannerRows(rows [][]any) (jsObject, error) {
	if len(rows) == 0 {
		return nil, errors.New("Planner workbook is empty")
	}
	if len(rows) > maxPlannerRows {
		return nil, errors.New("Planner workbook has more than " + strconv.Itoa(maxPlannerRows) + " rows")
	}
	texts := make([][]string, len(rows))
	for i, r := range rows {
		for _, c := range r {
			texts[i] = append(texts[i], excelCellText(c))
		}
	}
	headerAt := -1
	for i, r := range texts {
		if contains(r, "Task Name") && contains(r, "Bucket Name") {
			headerAt = i
			break
		}
	}
	if headerAt == -1 {
		return nil, errors.New("Planner workbook needs the Task Name and Bucket Name columns")
	}
	header := csvHeader(texts[headerAt])
	meta := map[string]string{}
	for _, r := range texts[:headerAt] {
		if len(r) > 0 && r[0] != "" {
			v := ""
			if len(r) > 1 {
				v = r[1]
			}
			meta[r[0]] = v
		}
	}
	get := func(row []string, name string) string {
		i := header.col(name)
		if i == -1 || i >= len(row) {
			return ""
		}
		return row[i]
	}
	type bodyRow struct {
		row    []string
		number int
	}
	var body []bodyRow
	for i, r := range texts[headerAt+1:] {
		for _, c := range r {
			if c != "" {
				body = append(body, bodyRow{r, headerAt + i + 2})
				break
			}
		}
	}
	var dateTexts []string
	for _, b := range body {
		for _, name := range []string{"Created Date", "Start Date", "Due Date", "Completed Date"} {
			dateTexts = append(dateTexts, get(b.row, name))
		}
	}
	order, conflict, guessed := plannerDateOrder(dateTexts)
	unsupported := []any{}
	switch {
	case conflict:
		unsupported = append(unsupported, importUnsupportedObj("/dates", "Planner dates mix day/month and month/day order; read as month/day"))
	case guessed:
		unsupported = append(unsupported, importUnsupportedObj("/dates", "Planner dates could be day/month or month/day; read as month/day, Planner's default"))
	}
	var columns []string
	tasks := []any{}
	for _, b := range body {
		row, at := b.row, "/row/"+strconv.Itoa(b.number)
		title := get(row, "Task Name")
		if title == "" {
			unsupported = append(unsupported, importUnsupportedObj(at, "a Planner row without a task name is not imported"))
			continue
		}
		bucket := jsOr(get(row, "Bucket Name"), "No bucket").(string)
		columns = addOnce(columns, bucket)
		people := plannerSplit(get(row, "Assigned To"))
		date := func(name string) string {
			text := get(row, name)
			v, ok := plannerDate(text, order)
			if text != "" && !ok {
				unsupported = append(unsupported, importUnsupportedObj(at+"/"+name, `Planner date "`+text+`" is not a calendar date`))
			}
			return v
		}
		custom := jsObject{}
		if p := get(row, "Progress"); p != "" {
			if contains(plannerProgress, p) {
				custom.set("Progress", p)
			} else {
				unsupported = append(unsupported, importUnsupportedObj(at+"/Progress", `Planner progress "`+p+`" is not one of `+strings.Join(plannerProgress, ", ")))
			}
		}
		if p := get(row, "Priority"); p != "" {
			if contains(plannerPriority, p) {
				custom.set("Priority", p)
			} else {
				unsupported = append(unsupported, importUnsupportedObj(at+"/Priority", `Planner priority "`+p+`" is not one of `+strings.Join(plannerPriority, ", ")))
			}
		}
		if c := get(row, "Completed By"); c != "" {
			custom.set("Completed By", c)
		}
		items := plannerSplit(get(row, "Checklist Items"))
		if m := plannerDone.FindStringSubmatch(get(row, "Completed Checklist Items")); len(items) > 0 && m != nil {
			if n, _ := strconv.Atoi(m[1]); n > 0 {
				unsupported = append(unsupported, importUnsupportedObj(at+"/Completed Checklist Items",
					"Planner exports how many checklist items are done ("+get(row, "Completed Checklist Items")+"), not which; all are imported as not done"))
			}
		}
		if plannerYes.MatchString(get(row, "Is Recurring")) {
			unsupported = append(unsupported, importUnsupportedObj(at+"/Is Recurring", "a recurring Planner task is imported once; its repeat has no place in the export"))
		}
		due := date("Due Date")
		t := jsObject{}
		t.set("title", title)
		t.set("description", get(row, "Description"))
		t.set("column_name", bucket)
		t.set("swimlane_name", "Default")
		t.set("tags", plannerSplit(get(row, "Labels")))
		if v := get(row, "Task ID"); v != "" {
			t.set("ref", v)
		}
		if len(people) > 0 {
			t.set("owner_username", people[0])
		}
		if len(people) > 1 {
			t.set("assignees", people[1:])
		}
		if v := get(row, "Created By"); v != "" {
			t.set("requested_by", v)
		}
		if due != "" {
			t.set("date_due", due)
		}
		for _, f := range []struct{ key, col string }{{"date_started", "Start Date"}, {"date_creation", "Created Date"}, {"date_end", "Completed Date"}} {
			if v := date(f.col); v != "" {
				t.set(f.key, v)
			}
		}
		if len(custom) > 0 {
			t.set("custom_fields", custom)
		}
		if len(items) > 0 {
			its := []any{}
			for _, i := range items {
				o := jsObject{}
				o.set("title", i)
				o.set("done", false)
				its = append(its, o)
			}
			cl := jsObject{}
			cl.set("title", "Checklist")
			cl.set("items", its)
			t.set("checklists", []any{cl})
		}
		tasks = append(tasks, t)
	}
	return importDoc(jsOr(meta["Plan name"], "Imported Microsoft Planner"), csvColumns(columns), tasks, []any{}, unsupported), nil
}
