package web

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// monday.com's board export workbook: Node.js WeKan's readMondayWorkbook
// (server/lib/mondayWorkbook.js) and parseMondaySheets
// (models/lib/mondayFormat.js) - its groups and statuses, items and
// subitems with their people, dates, timeline, tags and other columns, and
// the updates sheet's comments.

func init() { importTextParsers["monday"] = parseMondayWorkbook }

var (
	mondayRoles = []struct {
		role string
		re   *regexp.Regexp
	}{
		{"title", regexp.MustCompile(`(?i)^name$`)},
		{"status", regexp.MustCompile(`(?i)^status$`)},
		{"group", regexp.MustCompile(`(?i)^group$`)},
		{"people", regexp.MustCompile(`(?i)^(person|people|owner|owners|assignee|assignees)$`)},
		{"due", regexp.MustCompile(`(?i)^(date|due date|due|deadline)$`)},
		{"timeline", regexp.MustCompile(`(?i)^timeline$`)},
		{"tags", regexp.MustCompile(`(?i)^tags?$`)},
		{"id", regexp.MustCompile(`(?i)^item id$`)},
		{"description", regexp.MustCompile(`(?i)^(long text|description|notes)$`)},
	}
	mondayMonths   = map[string]int{"jan": 0, "feb": 1, "mar": 2, "apr": 3, "may": 4, "jun": 5, "jul": 6, "aug": 7, "sep": 8, "oct": 9, "nov": 10, "dec": 11}
	mondayISO      = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})$`)
	mondayLong     = jsRegexp(`(?i)^(\d{1,2})/([A-Za-z]{3})/(\d{4})\s+(\d{1,2}):(\d{2}):(\d{2})\s*(AM|PM)$`)
	mondayRange    = jsRegexp(`\s+-\s+`)
	mondayNumber   = regexp.MustCompile(`^-?\d+(\.\d+)?$`)
	mondayHeaderRe = regexp.MustCompile(`(?i)^name$`)
	mondaySubitems = regexp.MustCompile(`(?i)^subitems$`)
	mondayUpdates  = regexp.MustCompile(`(?i)-updates$`)
)

func mondayRoleOf(title string) string {
	for _, r := range mondayRoles {
		if r.re.MatchString(jsTrim(title)) {
			return r.role
		}
	}
	return ""
}

// mondayCellText is the module's cellText: a Date as its day.
func mondayCellText(v any) string {
	if d, ok := v.(jsDate); ok {
		return jsISO(time.Time(d))[:10]
	}
	if o, ok := v.(jsObject); ok {
		if _, rich := jsGet(o, "richText").([]any); !rich {
			if t := jsGetOrUndef(o, "text"); !isUndef(t) {
				return mondayCellText(t)
			}
			if r := jsGetOrUndef(o, "result"); !isUndef(r) {
				return mondayCellText(r)
			}
		}
	}
	return excelCellText(v)
}

func mondayDate(text string) (string, bool) {
	value := jsTrim(text)
	n := func(s string) int { v, _ := strconv.Atoi(s); return v }
	if m := mondayISO.FindStringSubmatch(value); m != nil {
		y := n(m[1])
		if y < 100 {
			y += 1900 // Date.UTC's
		}
		d := time.Date(y, time.Month(n(m[2])), n(m[3]), 0, 0, 0, 0, time.UTC)
		if d.Day() != n(m[3]) || int(d.Month()) != n(m[2]) {
			return "", false
		}
		return jsISO(d), true
	}
	m := mondayLong.FindStringSubmatch(value)
	if m == nil {
		return "", false
	}
	month, ok := mondayMonths[strings.ToLower(m[2])]
	if !ok {
		return "", false
	}
	hour := n(m[4]) % 12
	if strings.EqualFold(m[7], "pm") {
		hour += 12
	}
	y := n(m[3])
	if y < 100 {
		y += 1900
	}
	return jsISO(time.Date(y, time.Month(month+1), n(m[1]), hour, n(m[5]), n(m[6]), 0, time.UTC)), true
}

func parseMondayWorkbook(text string) (jsObject, error) {
	data, err := excelWorkbookBytes(text, "monday.com")
	if err != nil {
		return nil, err
	}
	sheets, err := readXlsxValues(data)
	if err != nil {
		return nil, err
	}
	return parseMondaySheets(sheets)
}

func mondayTexts(rows [][]any) [][]string {
	out := make([][]string, len(rows))
	for i, r := range rows {
		out[i] = []string{}
		for _, c := range r {
			out[i] = append(out[i], mondayCellText(c))
		}
	}
	return out
}

func parseMondaySheets(sheets []xlsxValueSheet) (jsObject, error) {
	if len(sheets) == 0 {
		return nil, errors.New("monday.com workbook is empty")
	}
	rows := mondayTexts(sheets[0].Rows)
	cell := func(row []string, i int) string {
		if i < 0 || i >= len(row) {
			return ""
		}
		return row[i]
	}
	isHeader := func(row []string) bool { return mondayHeaderRe.MatchString(cell(row, 0)) }
	filledOf := func(row []string) int {
		n := 0
		for _, c := range row {
			if c != "" {
				n++
			}
		}
		return n
	}
	firstHeader := -1
	for i, r := range rows {
		if isHeader(r) {
			firstHeader = i
			break
		}
	}
	if firstHeader == -1 {
		return nil, errors.New(`monday.com workbook has no header row starting with "Name"`)
	}
	unsupported := []any{}
	// Row 1 is the board's name when it stands alone above the first group.
	boardName := ""
	if firstHeader >= 2 && filledOf(rows[0]) >= 1 {
		boardName = cell(rows[0], 0)
	}
	var tasks []jsObject
	var header, subHeader []string
	group := ""
	var parent jsObject
	itemFrom := func(cells, titles []string, at string) jsObject {
		t := jsObject{}
		t.set("title", "")
		t.set("description", "")
		tags := []any{}
		t.set("tags", tags)
		t.set("swimlane_name", jsOr(group, "Default"))
		custom := jsObject{}
		status := ""
		for column, title := range titles {
			value := cell(cells, column)
			if title == "" || value == "" {
				continue
			}
			switch mondayRoleOf(title) {
			case "title":
				t.set("title", value)
			case "status":
				status = value
			case "group":
				t.set("swimlane_name", value)
			case "people":
				people := splitTrimmed(value, ",")
				if len(people) > 0 {
					t.set("owner_username", people[0])
				} else {
					t.set("owner_username", undefinedValue{})
				}
				if len(people) > 1 {
					t.set("assignees", people[1:])
				}
			case "due":
				if due, ok := mondayDate(value); ok {
					t.set("date_due", due)
				} else {
					unsupported = append(unsupported, importUnsupportedObj(at+"/"+title, `monday.com date "`+value+`" is not YYYY-MM-DD`))
				}
			case "timeline":
				parts := mondayRange.Split(value, -1)
				from, fok := mondayDate(parts[0])
				to, tok := "", false
				if len(parts) > 1 {
					to, tok = mondayDate(parts[1])
				}
				if fok {
					t.set("date_started", from)
				}
				if _, has := jsLookup(t, "date_due"); tok && !has {
					t.set("date_due", to)
				}
				if !fok || !tok {
					unsupported = append(unsupported, importUnsupportedObj(at+"/"+title, `monday.com timeline "`+value+`" is not "YYYY-MM-DD - YYYY-MM-DD"`))
				}
			case "tags":
				tags = append(tags, splitTrimmed(value, ",")...)
				t.set("tags", tags)
			case "id":
				t.set("ref", value)
			case "description":
				t.set("description", value)
			default:
				if mondayNumber.MatchString(value) {
					f, _ := strconv.ParseFloat(value, 64)
					custom.set(title, f)
				} else {
					custom.set(title, value)
				}
			}
		}
		if jsGet(t, "title") == "" {
			unsupported = append(unsupported, importUnsupportedObj(at, "a monday.com item without a name is not imported"))
			return nil
		}
		// Without an Item ID column, the row stands in for it, so a subitem
		// can still find its parent.
		if !jsTruthy(jsGet(t, "ref")) {
			t.set("ref", "monday"+at)
		}
		t.set("status", status)
		if len(custom) > 0 {
			t.set("custom_fields", jsOrder(custom))
		}
		return t
	}
	for index, row := range rows {
		at := "/row/" + strconv.Itoa(index+1)
		filled := filledOf(row)
		if filled == 0 {
			subHeader = nil
			continue
		}
		if index == 0 && boardName != "" {
			continue
		}
		if isHeader(row) {
			header, subHeader, parent = row, nil, nil
			continue
		}
		// The subitem header's first column, "Subitems", is the subitem's name.
		if cell(row, 0) == "" && mondaySubitems.MatchString(cell(row, 1)) {
			subHeader = append([]string{"Name"}, row[min(2, len(row)):]...)
			continue
		}
		if subHeader != nil && cell(row, 0) == "" && cell(row, 1) != "" {
			if sub := itemFrom(row[1:], subHeader, at); sub != nil {
				if parent != nil {
					sub.set("parent_ref", jsGet(parent, "ref"))
				} else {
					sub.set("parent_ref", nil)
				}
				tasks = append(tasks, sub)
			}
			continue
		}
		if filled == 1 && cell(row, 0) != "" && (header == nil || index+1 < len(rows) && isHeader(rows[index+1])) {
			group = cell(row, 0)
			continue
		}
		if cell(row, 0) == "" {
			continue // a summary row
		}
		if header == nil {
			unsupported = append(unsupported, importUnsupportedObj(at, "a row before the first header row is not imported"))
			continue
		}
		subHeader = nil
		parent = itemFrom(row, header, at)
		if parent != nil {
			tasks = append(tasks, parent)
		}
	}
	// Lists: the Status values when the board has a Status column, else groups.
	byStatus := false
	for _, t := range tasks {
		byStatus = byStatus || jsGet(t, "status") != ""
	}
	var columns, swimlanes []string
	for i := range tasks {
		t := tasks[i]
		var column string
		switch {
		case byStatus:
			column = jsOr(jsGet(t, "status"), "No status").(string)
		case jsGet(t, "swimlane_name") == "Default":
			column = "Items"
		default:
			column = jsGet(t, "swimlane_name").(string)
		}
		t.set("column_name", column)
		if !byStatus {
			t.set("swimlane_name", "Default")
		}
		t.del("status")
		tasks[i] = t
		columns = addOnce(columns, column)
	}
	for _, t := range tasks {
		swimlanes = addOnce(swimlanes, jsGet(t, "swimlane_name").(string))
	}
	// The updates sheet: comments on the item named by id, else by name.
	for _, sheet := range sheets[1:] {
		if !mondayUpdates.MatchString(sheet.Name) {
			continue
		}
		urows := mondayTexts(sheet.Rows)
		uheaderAt := -1
		for i, r := range urows {
			if contains(r, "Item Name") && contains(r, "Update Content") {
				uheaderAt = i
				break
			}
		}
		if uheaderAt != -1 {
			uh := csvHeader(urows[uheaderAt])
			for index, row := range urows[uheaderAt+1:] {
				get := func(name string) string { return cell(row, uh.col(name)) }
				text := get("Update Content")
				if text == "" {
					continue
				}
				target := -1
				if id := get("Item ID"); id != "" {
					for i, t := range tasks {
						if jsGet(t, "ref") == id {
							target = i
							break
						}
					}
				}
				if target == -1 {
					for i, t := range tasks {
						if jsGet(t, "title") == get("Item Name") {
							target = i
							break
						}
					}
				}
				if target == -1 {
					unsupported = append(unsupported, importUnsupportedObj("/updates/row/"+strconv.Itoa(uheaderAt+index+2), "an update for an item that is not in the board sheet"))
					continue
				}
				c := jsObject{}
				c.set("text", text)
				if u := get("User"); u != "" {
					c.set("author", u)
				}
				if d, ok := mondayDate(get("Created At")); ok {
					c.set("date", d)
				}
				comments, _ := jsGet(tasks[target], "comments").([]any)
				tasks[target].set("comments", append(comments, c))
			}
		}
		break
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
	all := []any{}
	for _, t := range tasks {
		all = append(all, t)
	}
	name := jsOr(boardName, sheets[0].Name, "Imported monday.com board").(string)
	out := importDoc(name, csvColumns(columns), all, []any{}, unsupported)
	out.set("swimlanes", lanes)
	return out, nil
}
