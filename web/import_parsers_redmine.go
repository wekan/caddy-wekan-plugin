package web

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Redmine's issues CSV: Node.js WeKan's parseRedmineCsv
// (models/lib/redmineCsvFormat.js) - its English column names or its
// importer's keys, ',' or ';' between fields, numeric dates in the order
// the file's own dates give away, statuses as lists, projects as swimlanes,
// its other columns as custom fields, and its relations - from "Related
// issues" or the importer's columns - once each.

func init() { importTextParsers["redmine"] = parseRedmineCsv }

type redmineRelation struct {
	key, label, typ string
	lossy           bool
}

var (
	redmineFields = []struct{ field, label string }{
		{"id", "#"}, {"project", "Project"}, {"tracker", "Tracker"}, {"parent", "Parent task"}, {"parentSubject", "Parent task subject"},
		{"status", "Status"}, {"priority", "Priority"}, {"subject", "Subject"}, {"author", "Author"}, {"assignee", "Assignee"},
		{"watchers", "Watchers"}, {"updated", "Updated"}, {"category", "Category"}, {"version", "Target version"},
		{"start", "Start date"}, {"due", "Due date"}, {"estimated", "Estimated time"}, {"remaining", "Estimated remaining time"},
		{"totalEstimated", "Total estimated time"}, {"spent", "Spent time"}, {"totalSpent", "Total spent time"}, {"done", "% Done"},
		{"created", "Created"}, {"closed", "Closed"}, {"lastUpdatedBy", "Last updated by"}, {"relations", "Related issues"},
		{"files", "Files"}, {"description", "Description"}, {"lastNotes", "Last notes"}, {"private", "Private"}, {"uniqueId", "Unique ID"},
	}
	redmineImportRelations = []redmineRelation{
		{"relation_relates", "Related to", "related-to", false}, {"relation_duplicates", "Is duplicate of", "duplicates", false},
		{"relation_duplicated", "Has duplicate", "is-duplicated-by", false}, {"relation_blocks", "Blocks", "blocks", false},
		{"relation_blocked", "Blocked by", "is-blocked-by", false}, {"relation_precedes", "Precedes", "related-to", true},
		{"relation_follows", "Follows", "related-to", true}, {"relation_copied_to", "Copied to", "related-to", true},
		{"relation_copied_from", "Copied from", "related-to", true},
	}
	redmineImportKeys = []struct{ key, field string }{
		{"tracker", "tracker"}, {"subject", "subject"}, {"description", "description"}, {"status", "status"}, {"priority", "priority"},
		{"category", "category"}, {"assigned_to", "assignee"}, {"fixed_version", "version"}, {"is_private", "private"},
		{"parent_issue_id", "parent"}, {"start_date", "start"}, {"due_date", "due"}, {"estimated_hours", "estimated"},
		{"done_ratio", "done"}, {"unique_id", "uniqueId"},
	}
	redmineReported = []string{"updated", "lastUpdatedBy", "files", "private", "remaining", "totalEstimated", "totalSpent"}
	redmineCustom   = []struct{ field, name string }{
		{"priority", "Priority"}, {"category", "Category"}, {"version", "Target version"}, {"estimated", "Estimated time"}, {"done", "% Done"},
	}
	redmineInverse = map[string]string{"related-to": "related-to", "blocks": "is-blocked-by", "is-blocked-by": "blocks",
		"duplicates": "is-duplicated-by", "is-duplicated-by": "duplicates", "fixes": "is-fixed-by", "is-fixed-by": "fixes"}
	redmineISOZoned  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T` + jsDotU + `*(Z|[+-]\d{2}:?\d{2})$`)
	redmineSplit     = jsRegexp(`[\sT]+`)
	redmineYMD       = regexp.MustCompile(`^(\d{4})[-/](\d{1,2})[-/](\d{1,2})$`)
	redmineDMY       = regexp.MustCompile(`^(\d{1,2})[.-](\d{1,2})[.-](\d{4})$`)
	redmineSlashDate = regexp.MustCompile(`^(\d{1,2})/(\d{1,2})/(\d{4})$`)
	redmineSlashHead = regexp.MustCompile(`^(\d{1,2})/(\d{1,2})/\d{4}`)
	redmineTime      = jsRegexp(`^(\d{1,2}):(\d{2})(?::(\d{2}))?(?:\s*([AaPp])\.?[Mm]\.?)?$`)
	redmineNumberRe  = regexp.MustCompile(`^-?\d+(?:[.,]\d+)?$`)
	redmineRelPart   = jsRegexp(`^(` + jsDotU + `+?)\s+(?:\(([^)]*)\)\s+)?#(\d+)$`)
	redmineColPart   = jsRegexp(`^(#?\d+|` + jsDotU + `+?)(?:\s+(-?\d+)d)?$`)
	redmineLines     = regexp.MustCompile(`\r?\n`)
)

func redmineLabel(field string) string {
	for _, f := range redmineFields {
		if f.field == field {
			return f.label
		}
	}
	return ""
}

// redmineSeparator is WeKan's: ';' when the header row has more semicolons
// than commas outside quotes.
func redmineSeparator(text string) rune {
	quoted := false
	commas, semicolons := 0, 0
	for _, ch := range strings.TrimPrefix(text, "\ufeff") {
		switch {
		case ch == '"':
			quoted = !quoted
		case !quoted && (ch == '\n' || ch == '\r'):
			goto done
		case !quoted && ch == ',':
			commas++
		case !quoted && ch == ';':
			semicolons++
		}
	}
done:
	if semicolons > commas {
		return ';'
	}
	return ','
}

// redmineDate is WeKan's: an ISO date-time with a zone, or a numeric date in
// one of Redmine's formats with an optional time, 12 or 24 hours, at UTC.
func redmineDate(value, slash string) (string, bool) {
	text := jsTrim(value)
	if text == "" {
		return "", false
	}
	if redmineISOZoned.MatchString(text) {
		t, ok := jsDateTime(text)
		if !ok {
			return "", false
		}
		return jsISO(t), true
	}
	parts := redmineSplit.Split(text, -1)
	dayPart, rest := parts[0], parts[1:]
	var y, m, d string
	if x := redmineYMD.FindStringSubmatch(dayPart); x != nil {
		y, m, d = x[1], x[2], x[3]
	} else if x := redmineDMY.FindStringSubmatch(dayPart); x != nil {
		d, m, y = x[1], x[2], x[3]
	} else if x := redmineSlashDate.FindStringSubmatch(dayPart); x != nil {
		m, d = x[1], x[2]
		if slash == "dmy" {
			m, d = x[2], x[1]
		}
		y = x[3]
	} else {
		return "", false
	}
	n := func(s string) int { v, _ := strconv.Atoi(s); return v }
	hours, minutes, seconds := 0, 0, 0
	if len(rest) > 0 {
		tm := redmineTime.FindStringSubmatch(strings.Join(rest, " "))
		if tm == nil {
			return "", false
		}
		hours, minutes, seconds = n(tm[1]), n(tm[2]), n(tm[3])
		if tm[4] != "" {
			if hours < 1 || hours > 12 {
				return "", false
			}
			hours %= 12
			if strings.EqualFold(tm[4], "p") {
				hours += 12
			}
		}
		if hours > 23 || minutes > 59 || seconds > 59 {
			return "", false
		}
	}
	year := n(y)
	if year < 100 {
		year += 1900 // Date.UTC's
	}
	t := time.Date(year, time.Month(n(m)), n(d), hours, minutes, seconds, 0, time.UTC)
	if int(t.Month()) != n(m) || t.Day() != n(d) {
		return "", false
	}
	return jsISO(t), true
}

func redmineSlashOrder(values []string) string {
	dayFirst, monthFirst := false, false
	for _, v := range values {
		m := redmineSlashHead.FindStringSubmatch(jsTrim(v))
		if m == nil {
			continue
		}
		if a, _ := strconv.Atoi(m[1]); a > 12 {
			dayFirst = true
		}
		if b, _ := strconv.Atoi(m[2]); b > 12 {
			monthFirst = true
		}
	}
	if dayFirst && !monthFirst {
		return "dmy"
	}
	return "mdy"
}

func redmineNumber(value string) (float64, bool) {
	text := jsTrim(value)
	if !redmineNumberRe.MatchString(text) {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.Replace(text, ",", ".", 1), 64)
	return f, err == nil
}

func redmineIssueRef(v string) string { return strings.TrimPrefix(jsTrim(v), "#") }

func parseRedmineCsv(text string) (jsObject, error) {
	rows, err := jsReadCsv(text, "Redmine", redmineSeparator(text))
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, errors.New("Redmine CSV is empty")
	}
	header := csvHeader{}
	for _, c := range rows[0] {
		header = append(header, jsTrim(c))
	}
	type entry struct {
		field    string
		relation *redmineRelation
	}
	byLabel := map[string]entry{}
	for _, f := range redmineFields {
		byLabel[jsLower(f.label)] = entry{field: f.field}
	}
	for _, k := range redmineImportKeys {
		byLabel[k.key] = entry{field: k.field}
	}
	for i := range redmineImportRelations {
		r := &redmineImportRelations[i]
		byLabel[jsLower(r.label)] = entry{relation: r}
		byLabel[r.key] = entry{relation: r}
	}
	known := make([]*entry, len(header))
	anyKnown := false
	for i, name := range header {
		if e, ok := byLabel[jsLower(name)]; ok {
			e := e
			known[i] = &e
			anyKnown = true
		}
	}
	if !anyKnown {
		return nil, errors.New("Redmine CSV headers are not Redmine's English column names. Redmine writes the headers in the " +
			"language of the user who exported, so export again with English as your language (My account), or rename the " +
			"headers to English (#, Subject, Status, ...)")
	}
	col := map[string]int{}
	for i, e := range known {
		if e != nil && e.field != "" {
			if _, set := col[e.field]; !set {
				col[e.field] = i
			}
		}
	}
	if _, ok := col["subject"]; !ok {
		return nil, errors.New("Redmine CSV needs the Subject column")
	}
	type relCol struct {
		index int
		rel   *redmineRelation
	}
	var relationCols []relCol
	type customCol struct {
		index int
		name  string
	}
	var customCols []customCol
	for i, e := range known {
		if e != nil && e.relation != nil {
			relationCols = append(relationCols, relCol{i, e.relation})
		}
		if header[i] != "" && e == nil {
			customCols = append(customCols, customCol{i, header[i]})
		}
	}
	get := func(cells []string, field string) string {
		i, ok := col[field]
		if !ok || i >= len(cells) {
			return ""
		}
		return jsTrim(cells[i])
	}
	raw := func(cells []string, i int) string {
		if i >= len(cells) {
			return ""
		}
		return jsTrim(cells[i])
	}
	unsupported := []any{}
	for _, f := range redmineReported {
		if _, ok := col[f]; ok {
			label := redmineLabel(f)
			unsupported = append(unsupported, importUnsupportedObj("/columns/"+label, `Redmine column "`+label+`" has no WeKan place and is not imported`))
		}
	}
	_, hasParentSubject := col["parentSubject"]
	_, hasParent := col["parent"]
	if hasParentSubject && !hasParent {
		unsupported = append(unsupported, importUnsupportedObj("/columns/Parent task subject", `Redmine column "Parent task subject" names a parent without its id and is not imported`))
	}
	var dateValues []string
	for _, cells := range rows[1:] {
		for _, f := range []string{"start", "due", "created", "closed"} {
			dateValues = append(dateValues, get(cells, f))
		}
	}
	slash := redmineSlashOrder(dateValues)

	type link struct {
		from      int
		ref       string
		rel       *redmineRelation
		delay, at string
	}
	var columns, swimlanes []string
	var tasks []jsObject
	var links []link
	for index, cells := range rows[1:] {
		at := "/row/" + strconv.Itoa(index+2)
		title := get(cells, "subject")
		if title == "" {
			unsupported = append(unsupported, importUnsupportedObj(at, "a Redmine row without a subject is not imported"))
			continue
		}
		status := jsOr(get(cells, "status"), "New").(string)
		columns = addOnce(columns, status)
		project := jsOr(get(cells, "project"), "Default").(string)
		swimlanes = addOnce(swimlanes, project)
		date := func(field string) string {
			value := get(cells, field)
			iso, ok := redmineDate(value, slash)
			if value != "" && !ok {
				unsupported = append(unsupported, importUnsupportedObj(at+"/"+redmineLabel(field), `Redmine date "`+value+`" is not in a numeric date format`))
			}
			return iso
		}
		custom := jsObject{}
		for _, c := range redmineCustom {
			value := get(cells, c.field)
			if value == "" {
				continue
			}
			if c.field == "estimated" || c.field == "done" {
				if n, ok := redmineNumber(value); ok {
					custom.set(c.name, n)
				} else {
					unsupported = append(unsupported, importUnsupportedObj(at+"/"+c.name, "Redmine "+c.name+` "`+value+`" is not a number`))
				}
			} else {
				custom.set(c.name, value)
			}
		}
		for _, c := range customCols {
			if v := raw(cells, c.index); v != "" {
				custom.set(c.name, v)
			}
		}
		ref := jsOr(get(cells, "id"), get(cells, "uniqueId")).(string)
		spentText := get(cells, "spent")
		spent, hasSpent := 0.0, false
		if spentText != "" {
			spent, hasSpent = redmineNumber(spentText)
			if !hasSpent {
				unsupported = append(unsupported, importUnsupportedObj(at+"/Spent time", `Redmine Spent time "`+spentText+`" is not a number`))
			}
		}
		tracker := get(cells, "tracker")
		owner := get(cells, "assignee")
		watchers := []any{}
		for _, w := range redmineLines.Split(get(cells, "watchers"), -1) {
			if w = jsTrim(w); w != "" {
				watchers = append(watchers, w)
			}
		}
		notes := get(cells, "lastNotes")
		parent := redmineIssueRef(get(cells, "parent"))
		taskIndex := len(tasks)
		if combined := get(cells, "relations"); combined != "" {
			for _, p := range splitTrimmed(combined, ",") {
				part := p.(string)
				m := redmineRelPart.FindStringSubmatch(part)
				var rel *redmineRelation
				if m != nil {
					for i := range redmineImportRelations {
						if jsLower(redmineImportRelations[i].label) == jsLower(jsTrim(m[1])) {
							rel = &redmineImportRelations[i]
							break
						}
					}
				}
				if rel == nil {
					unsupported = append(unsupported, importUnsupportedObj(at+"/Related issues", `Redmine relation "`+part+`" is not one of Redmine's English relation names`))
					continue
				}
				links = append(links, link{taskIndex, m[3], rel, m[2], at + "/Related issues"})
			}
		}
		for _, rc := range relationCols {
			value := raw(cells, rc.index)
			if value == "" {
				continue
			}
			for _, p := range splitTrimmed(value, ",") {
				m := redmineColPart.FindStringSubmatch(p.(string))
				links = append(links, link{taskIndex, redmineIssueRef(m[1]), rc.rel, m[2], at + "/" + header[rc.index]})
			}
		}
		closed := date("closed")
		t := jsObject{}
		t.set("title", title)
		t.set("description", get(cells, "description"))
		t.set("column_name", status)
		t.set("swimlane_name", project)
		tags := []any{}
		if tracker != "" {
			tags = append(tags, tracker)
		}
		t.set("tags", tags)
		if ref != "" {
			t.set("ref", redmineIssueRef(ref))
		}
		if parent != "" {
			t.set("parent_ref", parent)
		}
		if owner != "" {
			t.set("owner_username", owner)
		}
		if a := get(cells, "author"); a != "" {
			t.set("requested_by", a)
		}
		if len(watchers) > 0 {
			t.set("watchers", watchers)
		}
		for _, f := range []struct{ key, field string }{{"date_creation", "created"}, {"date_started", "start"}, {"date_due", "due"}} {
			if v := date(f.field); v != "" {
				t.set(f.key, v)
			}
		}
		if closed != "" {
			t.set("date_end", closed)
		}
		if hasSpent && spent > 0 {
			t.set("spent_hours", spent)
		}
		if notes != "" {
			c := jsObject{}
			c.set("text", notes)
			t.set("comments", []any{c})
		}
		if len(custom) > 0 {
			t.set("custom_fields", jsOrder(custom))
		}
		tasks = append(tasks, t)
	}
	// Redmine lists a relation on both of its issues; WeKan keeps it once, on
	// the first card seen.
	refs := map[string]int{}
	for i, t := range tasks {
		if r, _ := jsGet(t, "ref").(string); r != "" {
			refs[r] = i
		}
	}
	seen := map[string]bool{}
	for _, l := range links {
		if l.rel.lossy {
			unsupported = append(unsupported, importUnsupportedObj(l.at, `Redmine relation "`+l.rel.label+`" has no WeKan type and is imported as related-to`))
		}
		if l.delay != "" {
			unsupported = append(unsupported, importUnsupportedObj(l.at, `the delay of Redmine relation "`+l.rel.label+`" (`+l.delay+`) is not imported`))
		}
		if target, ok := refs[l.ref]; ok {
			key := strconv.Itoa(l.from) + ">" + strconv.Itoa(target) + ":" + l.rel.typ
			if seen[key] || seen[strconv.Itoa(target)+">"+strconv.Itoa(l.from)+":"+redmineInverse[l.rel.typ]] {
				continue
			}
			seen[key] = true
		}
		d := jsObject{}
		d.set("ref", l.ref)
		d.set("type", l.rel.typ)
		deps, _ := jsGet(tasks[l.from], "dependencies").([]any)
		tasks[l.from].set("dependencies", append(deps, d))
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
	out := importDoc("Imported Redmine issues", csvColumns(columns), all, []any{}, unsupported)
	out.set("swimlanes", lanes)
	return out, nil
}
