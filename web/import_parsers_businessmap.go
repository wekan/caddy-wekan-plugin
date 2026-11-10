package web

import (
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A Businessmap (Kanbanize) cards workbook: Node.js WeKan's
// readBusinessmapWorkbook (server/lib/businessmapWorkbook.js) and
// parseBusinessmapSheets (models/lib/businessmapFormat.js) - columns as
// lists, lanes (or workflows) as swimlanes, its fields, dates, comments, and
// its Links and Parent columns as the card tree and dependencies.

func init() { importTextParsers["businessmap"] = parseBusinessmapWorkbook }

var (
	businessmapPriorities = []string{"low", "average", "high", "critical"}
	businessmapLinkTypes  = map[string]string{"parent": "parents", "parents": "parents", "child": "children", "children": "children",
		"relative": "relatives", "relatives": "relatives", "predecessor": "predecessors", "predecessors": "predecessors",
		"successor": "successors", "successors": "successors"}
	businessmapRoles = []struct {
		role string
		re   *regexp.Regexp
	}{
		{"title", regexp.MustCompile(`(?i)^title$`)}, {"description", regexp.MustCompile(`(?i)^description$`)},
		{"cardId", regexp.MustCompile(`(?i)^card id$`)}, {"customId", regexp.MustCompile(`(?i)^custom card id$`)},
		{"priority", regexp.MustCompile(`(?i)^priority$`)}, {"owner", regexp.MustCompile(`(?i)^owner$`)},
		{"coOwners", regexp.MustCompile(`(?i)^co-?owners?$`)}, {"color", regexp.MustCompile(`(?i)^colou?r$`)},
		{"size", regexp.MustCompile(`(?i)^size$`)}, {"tags", regexp.MustCompile(`(?i)^tags?$`)},
		{"deadline", regexp.MustCompile(`(?i)^deadline$`)}, {"template", regexp.MustCompile(`(?i)^template$`)},
		{"type", regexp.MustCompile(`(?i)^type( name)?$`)}, {"column", regexp.MustCompile(`(?i)^column( name)?$`)},
		{"lane", regexp.MustCompile(`(?i)^lane( name)?$`)}, {"workflow", regexp.MustCompile(`(?i)^workflow name$`)},
		{"boardName", regexp.MustCompile(`(?i)^board name$`)}, {"boardId", regexp.MustCompile(`(?i)^board id$`)},
		{"comment", regexp.MustCompile(`(?i)^comments?( \d+)?$`)}, {"links", regexp.MustCompile(`(?i)^links$`)},
		{"parent", regexp.MustCompile(`(?i)^parent( card id)?$`)}, {"created", regexp.MustCompile(`(?i)^(created at|creation date)$`)},
		{"archived", regexp.MustCompile(`(?i)^(archived at|archivation date)$`)}, {"start", regexp.MustCompile(`(?i)^start date$`)},
		{"end", regexp.MustCompile(`(?i)^end date$`)}, {"plannedStart", regexp.MustCompile(`(?i)^planned start$`)},
		{"plannedEnd", regexp.MustCompile(`(?i)^planned end$`)}, {"track", regexp.MustCompile(`(?i)^track$`)},
	}
	businessmapFieldOfRole = map[string]string{"cardId": "Card ID", "customId": "Custom Card ID", "priority": "Priority", "size": "Size",
		"type": "Type", "workflow": "Workflow name", "plannedStart": "Planned Start", "plannedEnd": "Planned End", "track": "Track"}
	businessmapYMD     = regexp.MustCompile(`^(\d{4})-(\d{1,2})-(\d{1,2})$`)
	businessmapMDY     = regexp.MustCompile(`^(\d{1,2})/(\d{1,2})/(\d{4}|\d{2})$`)
	businessmapDMY     = regexp.MustCompile(`^(\d{1,2})-(\d{1,2})-(\d{4})$`)
	businessmapSpaces  = jsRegexp(`\s+`)
	businessmapNameSep = regexp.MustCompile(`[,;]`)
	businessmapLinkRe  = jsRegexp(`^([A-Za-z]+)\s*:\s*(` + jsDotU + `*)$`)
	businessmapIDSep   = jsRegexp(`[,\s]+`)
	businessmapHex     = regexp.MustCompile(`^#?([0-9a-fA-F]{6})$`)
	businessmapGeneric = jsRegexp(`(?i)^(sheet\s*\d*|export|search results?)$`)
	businessmapFrom    = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	businessmapTo      = time.Date(2037, 12, 31, 0, 0, 0, 0, time.UTC).Add(24 * time.Hour)
)

func businessmapRole(title string) string {
	for _, r := range businessmapRoles {
		if r.re.MatchString(jsTrim(title)) {
			return r.role
		}
	}
	return ""
}

// businessmapCellText is the module's cellText: a Date as its day, rich
// text trimmed.
func businessmapCellText(v any) string {
	if d, ok := v.(jsDate); ok {
		return jsISO(time.Time(d))[:10]
	}
	if o, ok := v.(jsObject); ok {
		if _, rich := jsGet(o, "richText").([]any); rich {
			return jsTrim(excelCellText(o))
		}
		if t := jsGetOrUndef(o, "text"); !isUndef(t) {
			return businessmapCellText(t)
		}
		if r := jsGetOrUndef(o, "result"); !isUndef(r) {
			return businessmapCellText(r)
		}
		return ""
	}
	return excelCellText(v)
}

func businessmapUTCDay(y, m, d int) (time.Time, bool) {
	t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
	if y < 100 || t.Year() != y || int(t.Month()) != m || t.Day() != d {
		return time.Time{}, false // Date.UTC moves 0-99 to the 1900s
	}
	return t, true
}

func businessmapDate(v any, deadline bool) (string, bool) {
	var date time.Time
	ok := false
	if d, is := v.(jsDate); is {
		date, ok = time.Time(d), true
	} else if d, is := jsGet(v, "result").(jsDate); is {
		date, ok = time.Time(d), true
	}
	if !ok {
		text := businessmapCellText(v)
		n := func(s string) int { x, _ := strconv.Atoi(s); return x }
		if m := businessmapYMD.FindStringSubmatch(text); m != nil {
			date, ok = businessmapUTCDay(n(m[1]), n(m[2]), n(m[3]))
		} else if m := businessmapMDY.FindStringSubmatch(text); m != nil {
			y := n(m[3])
			if len(m[3]) == 2 {
				y += 2000
			}
			date, ok = businessmapUTCDay(y, n(m[1]), n(m[2]))
		} else if m := businessmapDMY.FindStringSubmatch(text); m != nil {
			date, ok = businessmapUTCDay(n(m[3]), n(m[2]), n(m[1]))
		}
	}
	if !ok {
		return "", false
	}
	if deadline && (date.Before(businessmapFrom) || !date.Before(businessmapTo)) {
		return "", false
	}
	return jsISO(date), true
}

func businessmapTags(text string) []any {
	if strings.Contains(text, ",") {
		return splitTrimmed(text, ",")
	}
	out := []any{}
	for _, p := range businessmapSpaces.Split(text, -1) {
		if p = jsTrim(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func businessmapNames(text string) []any {
	re := businessmapSpaces
	if businessmapNameSep.MatchString(text) {
		re = businessmapNameSep
	}
	out := []any{}
	for _, p := range re.Split(text, -1) {
		if p = jsTrim(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func businessmapFieldValue(raw any) any {
	if f, ok := raw.(float64); ok {
		return f
	}
	text := businessmapCellText(raw)
	if mondayNumber.MatchString(text) {
		f, _ := strconv.ParseFloat(text, 64)
		return f
	}
	return text
}

type businessmapLinks struct {
	parents, children, relatives, predecessors, successors, unknown []string
}

func parseBusinessmapLinks(text string) *businessmapLinks {
	l := &businessmapLinks{}
	for _, p := range strings.Split(text, ";") {
		part := jsTrim(p)
		if part == "" {
			continue
		}
		m := businessmapLinkRe.FindStringSubmatch(part)
		typ := ""
		if m != nil {
			typ = businessmapLinkTypes[strings.ToLower(m[1])]
		}
		if typ == "" {
			l.unknown = append(l.unknown, part)
			continue
		}
		var ids []string
		for _, id := range businessmapIDSep.Split(m[2], -1) {
			if id = jsTrim(id); id != "" {
				ids = append(ids, id)
			}
		}
		switch typ {
		case "parents":
			l.parents = append(l.parents, ids...)
		case "children":
			l.children = append(l.children, ids...)
		case "relatives":
			l.relatives = append(l.relatives, ids...)
		case "predecessors":
			l.predecessors = append(l.predecessors, ids...)
		case "successors":
			l.successors = append(l.successors, ids...)
		}
	}
	return l
}

func parseBusinessmapWorkbook(text string) (jsObject, error) {
	data, err := excelWorkbookBytes(text, "Businessmap")
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(text, "\xd0\xcf\x11\xe0") {
		return nil, errors.New("Businessmap workbook is an .xls file; open it in a spreadsheet program and save it as .xlsx")
	}
	sheets, err := readXlsxValues(data)
	if err != nil {
		return nil, err
	}
	return parseBusinessmapSheets(sheets)
}

func parseBusinessmapSheets(sheets []xlsxValueSheet) (jsObject, error) {
	if len(sheets) == 0 {
		return nil, errors.New("Businessmap workbook is empty")
	}
	hasText := func(row []any) bool {
		for _, c := range row {
			if businessmapCellText(c) != "" {
				return true
			}
		}
		return false
	}
	isHeader := func(row []any) bool {
		for _, c := range row {
			switch businessmapRole(businessmapCellText(c)) {
			case "title", "cardId", "customId":
				return true
			}
		}
		return false
	}
	sheetIndex, headerAt := -1, -1
	for i, s := range sheets {
		for j, r := range s.Rows {
			if j >= 20 {
				break
			}
			if isHeader(r) {
				sheetIndex, headerAt = i, j
				break
			}
		}
		if sheetIndex != -1 {
			break
		}
	}
	if sheetIndex == -1 {
		return nil, errors.New("Businessmap workbook has no header row with Title or Card ID. Headers are read by their English names; " +
			"a file exported from an account in another language needs its headers renamed to the English ones (Title, Column, Lane, Owner, Deadline ...)")
	}
	unsupported := []any{}
	report := func(path, reason string) { unsupported = append(unsupported, importUnsupportedObj(path, reason)) }
	for i, s := range sheets {
		if i == sheetIndex {
			continue
		}
		for _, r := range s.Rows {
			if hasText(r) {
				report("/sheet/"+s.Name, `the "`+s.Name+`" sheet is not imported; Links and Parent columns of the card sheet are`)
				break
			}
		}
	}
	sheetName, rows := sheets[sheetIndex].Name, sheets[sheetIndex].Rows
	var header, roles []string
	for _, c := range rows[headerAt] {
		t := businessmapCellText(c)
		header = append(header, t)
		roles = append(roles, businessmapRole(t))
	}
	if headerAt > 0 {
		report("/row/1", strconv.Itoa(headerAt)+" row(s) above the header are not imported")
	}
	if contains(roles, "boardId") {
		report("/Board ID", "Board ID names a Businessmap board; the cards are imported into one new board")
	}
	type pending struct {
		index int
		links *businessmapLinks
		at    string
	}
	var tasks []jsObject
	var pendingLinks []pending
	boardName := ""
	for offset, row := range rows[headerAt+1:] {
		at := "/row/" + strconv.Itoa(headerAt+offset+2)
		if !hasText(row) {
			continue
		}
		t := jsObject{}
		t.set("title", "")
		t.set("description", "")
		tags := []any{}
		t.set("tags", tags)
		custom := jsObject{}
		var comments []any
		column, lane, workflow := "", "", ""
		var links *businessmapLinks
		for index, title := range header {
			var raw any = undefinedValue{}
			if index < len(row) {
				raw = row[index]
			}
			value := businessmapCellText(raw)
			if title == "" || value == "" {
				continue
			}
			where := at + "/" + title
			switch role := roles[index]; role {
			case "title":
				t.set("title", value)
			case "description":
				t.set("description", value)
			case "cardId":
				t.set("ref", value)
				custom.set("Card ID", businessmapFieldValue(raw))
			case "customId":
				custom.set("Custom Card ID", value)
			case "priority":
				if contains(businessmapPriorities, jsLower(value)) {
					custom.set("Priority", jsLower(value))
				} else {
					custom.set("Priority", value)
					report(where, `Businessmap priority "`+value+`" is not low, average, high or critical; kept as written`)
				}
			case "owner":
				t.set("owner_username", value)
			case "coOwners":
				t.set("assignees", businessmapNames(value))
			case "color":
				if m := businessmapHex.FindStringSubmatch(value); m != nil {
					t.set("color", "#"+strings.ToLower(m[1]))
				} else {
					report(where, `Businessmap color "`+value+`" is not a hex color like #067DB7`)
				}
			case "size":
				if n, ok := jsToNumber(value); ok && n >= 0 && !isInf(n) {
					custom.set("Size", n)
				} else {
					report(where, `Businessmap size "`+value+`" is not a number`)
				}
			case "tags":
				tags = append(tags, businessmapTags(value)...)
				t.set("tags", tags)
			case "deadline":
				if due, ok := businessmapDate(raw, true); ok {
					t.set("date_due", due)
				} else {
					report(where, `Businessmap deadline "`+value+`" is not MM/DD/YYYY, DD-MM-YYYY or YYYY-MM-DD between 1970 and 2037`)
				}
			case "template":
				report(where, `the Businessmap card template "`+value+`" is not imported`)
			case "type", "plannedStart", "plannedEnd", "track":
				custom.set(businessmapFieldOfRole[role], value)
			case "workflow":
				workflow = value
				custom.set("Workflow name", value)
			case "column":
				column = value
			case "lane":
				lane = value
			case "boardName":
				if boardName == "" {
					boardName = value
				}
			case "boardId":
			case "comment":
				c := jsObject{}
				c.set("text", value)
				comments = append(comments, c)
			case "links":
				links = parseBusinessmapLinks(value)
			case "parent":
				t.set("parent_ref", value)
			case "created", "start", "end":
				field := map[string]string{"created": "date_creation", "start": "date_started", "end": "date_end"}[role]
				if d, ok := businessmapDate(raw, false); ok {
					t.set(field, d)
				} else {
					report(where, `Businessmap date "`+value+`" is not MM/DD/YYYY, DD-MM-YYYY or YYYY-MM-DD`)
				}
			case "archived":
				t.set("archived", true)
				report(where, "the card is archived; its Businessmap archive date is not kept")
			default:
				custom.set(title, businessmapFieldValue(raw))
			}
		}
		ref, _ := jsGet(t, "ref").(string)
		if jsGet(t, "title") == "" {
			if ref == "" {
				report(at, "a Businessmap row without a Title or Card ID is not imported")
				continue
			}
			t.set("title", "Card "+ref)
			report(at, `a blank Title (it keeps the existing title in Businessmap); named "Card `+ref+`"`)
		}
		if cid := jsGet(custom, "Custom Card ID"); ref == "" && jsTruthy(cid) {
			t.set("ref", jsStr(cid))
		}
		t.set("column_name", jsOr(column, "No column"))
		t.set("swimlane_name", jsOr(lane, workflow, "Default"))
		if len(comments) > 0 {
			t.set("comments", comments)
		}
		if len(custom) > 0 {
			t.set("custom_fields", jsOrder(custom))
		}
		if links != nil {
			pendingLinks = append(pendingLinks, pending{len(tasks), links, at})
		}
		tasks = append(tasks, t)
	}
	// Links: Parents and Children are subtasks, Predecessors and Successors
	// blocking dependencies, Relatives related ones; a relation written on
	// both cards is kept once.
	byRef := map[string]int{}
	for i, t := range tasks {
		if r, _ := jsGet(t, "ref").(string); r != "" {
			if _, has := byRef[r]; !has {
				byRef[r] = i
			}
		}
	}
	seen := map[string]bool{}
	addDependency := func(index int, ref, typ, key string) {
		if seen[key] {
			return
		}
		seen[key] = true
		d := jsObject{}
		d.set("ref", ref)
		d.set("type", typ)
		deps, _ := jsGet(tasks[index], "dependencies").([]any)
		tasks[index].set("dependencies", append(deps, d))
	}
	for _, p := range pendingLinks {
		task := tasks[p.index]
		where := p.at + "/Links"
		for _, part := range p.links.unknown {
			report(where, `Businessmap link "`+part+`" is not Parents, Children, Relatives, Predecessors or Successors`)
		}
		taskRef, _ := jsGet(task, "ref").(string)
		for _, ref := range p.links.parents {
			if pr, _ := jsGet(tasks[p.index], "parent_ref").(string); pr == "" {
				tasks[p.index].set("parent_ref", ref)
			} else if pr != ref {
				report(where, "a further parent "+ref+"; a WeKan card has one parent")
			}
		}
		for _, ref := range p.links.children {
			child, ok := byRef[ref]
			switch {
			case !ok:
				report(where, "child card "+ref+" is not part of this import")
			case !jsTruthy(jsGet(tasks[child], "parent_ref")):
				if taskRef != "" {
					tasks[child].set("parent_ref", taskRef)
				} else {
					tasks[child].set("parent_ref", undefinedValue{})
				}
			case jsGet(tasks[child], "parent_ref") != jsOr(taskRef, undefinedValue{}):
				report(where, "child card "+ref+" already has another parent")
			}
		}
		self := jsOr(taskRef, "#"+strconv.Itoa(p.index)).(string)
		for _, ref := range p.links.predecessors {
			addDependency(p.index, ref, "is-blocked-by", "block:"+ref+">"+self)
		}
		for _, ref := range p.links.successors {
			if target, ok := byRef[ref]; !ok || taskRef == "" {
				addDependency(p.index, ref, "blocks", "block:"+self+">"+ref)
			} else {
				addDependency(target, self, "is-blocked-by", "block:"+self+">"+ref)
			}
		}
		for _, ref := range p.links.relatives {
			pair := []string{self, ref}
			sort.Strings(pair)
			addDependency(p.index, ref, "related-to", "rel:"+strings.Join(pair, "|"))
		}
	}
	var columns, swimlanes []string
	all := []any{}
	for _, t := range tasks {
		columns = addOnce(columns, jsGet(t, "column_name").(string))
		swimlanes = addOnce(swimlanes, jsGet(t, "swimlane_name").(string))
		all = append(all, t)
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
	generic := sheetName == "" || businessmapGeneric.MatchString(sheetName)
	name := boardName
	if name == "" && !generic {
		name = sheetName
	}
	out := importDoc(jsOr(name, "Imported Businessmap board"), csvColumns(columns), all, []any{}, unsupported)
	out.set("swimlanes", lanes)
	return out, nil
}
