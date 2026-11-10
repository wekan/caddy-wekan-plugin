package web

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Plane's issue export: Node.js WeKan's readPlaneImport
// (server/lib/planeArchive.js) and parsePlaneExport
// (models/lib/planeFormat.js) - its JSON, CSV or XLSX files, alone or in
// the export .zip: states as lists, projects as swimlanes, people, labels,
// cycles, modules, dates, priority, estimate, links, comments, parents and
// relations.

func init() { importTextParsers["plane"] = parsePlaneText }

const (
	maxPlaneZipBytes   = 64 << 20
	maxPlaneFileBytes  = 64 << 20
	maxPlaneInflated   = 128 << 20
	maxPlaneZipEntries = 1000
	maxPlaneIssues     = 20000
	maxPlaneText       = 64 << 20
)

var (
	planeFields = []string{"project_name", "project_identifier", "parent", "identifier", "sequence_id", "name",
		"state_name", "priority", "assignees", "subscribers", "created_by_name", "start_date", "target_date",
		"completed_at", "created_at", "updated_at", "archived_at", "estimate", "labels", "cycles", "modules", "links",
		"relations", "comments", "sub_issues_count", "link_count", "attachment_count", "is_draft"}
	planePriorities = map[string]string{"urgent": "Urgent", "high": "High", "medium": "Medium", "low": "Low"}
	planeRelations  = map[string][2]string{"blocked_by": {"is-blocked-by", "blocks"}, "relates_to": {"related-to", "related-to"},
		"duplicate": {"duplicates", "is-duplicated-by"}}
	planeInverse   = map[string]string{"is-blocked-by": "blocks", "blocks": "is-blocked-by", "related-to": "related-to", "duplicates": "is-duplicated-by", "is-duplicated-by": "duplicates"}
	planeLists     = []string{"assignees", "subscribers", "labels", "cycles", "modules", "links", "relations", "comments"}
	planeNameLists = []string{"assignees", "subscribers", "labels", "cycles", "modules"}
	planeDerived   = map[string]bool{"sequence_id": true, "sub_issues_count": true, "link_count": true}
	planeDay       = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	planeDateTime  = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})[T ](\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?)(Z|[+-]\d{2}:?\d{2})?$`)
	planeTrue      = regexp.MustCompile(`(?i)^true$`)
	planeWebLink   = regexp.MustCompile(`(?i)^(https?:|mailto:)`)
	planeHidden    = regexp.MustCompile(`(^|/)(__MACOSX|\.)`)
	planeExt       = regexp.MustCompile(`\.([a-zA-Z]+)$`)
)

func planeUnformula(v any) any {
	s, ok := v.(string)
	if !ok || len(s) < 2 || s[0] != '\'' {
		return v
	}
	if strings.ContainsRune("=+-@\t\r\n", rune(s[1])) {
		return s[1:]
	}
	return v
}

func planeStr(v any) string {
	if v == nil || isUndef(v) {
		return ""
	}
	return jsStr(v)
}

func planeHeaderKey(h any) string {
	return strings.ReplaceAll(jsLower(jsTrim(planeStr(planeUnformula(planeStr(h))))), " ", "_")
}

func planeDate(v any) (string, bool) {
	text := jsTrim(planeStr(v))
	if text == "" {
		return "", false
	}
	if planeDay.MatchString(text) {
		t, ok := jsDateTime(text + "T00:00:00.000Z")
		if !ok || jsISO(t)[:10] != text {
			return "", false
		}
		return jsISO(t), true
	}
	m := planeDateTime.FindStringSubmatch(text)
	if m == nil {
		return "", false
	}
	t, ok := jsDateTime(m[1] + "T" + m[2] + jsOr(m[3], "Z").(string))
	if !ok {
		return "", false
	}
	return jsISO(t), true
}

func planeJSONIssues(text, at string) ([]any, error) {
	v, err := decodeJS([]byte(text))
	if err != nil || !json.Valid([]byte(text)) {
		return nil, errors.New("Plane export " + at + " is not valid JSON")
	}
	list, ok := v.([]any)
	if !ok {
		return nil, errors.New("Plane export " + at + " is not a list of issues")
	}
	return list, nil
}

func planeTableIssues(rows [][]any, format, at string, report func(path, reason string)) ([]any, error) {
	var cells [][]any
	for _, r := range rows {
		for _, c := range r {
			if jsTrim(planeStr(c)) != "" {
				cells = append(cells, r)
				break
			}
		}
	}
	if len(cells) == 0 {
		return nil, nil
	}
	var header []string
	for _, h := range cells[0] {
		header = append(header, planeHeaderKey(h))
	}
	if !contains(header, "name") || !contains(header, "state_name") {
		return nil, errors.New("Plane export " + at + " needs the Name and State Name columns")
	}
	var out []any
	for index, row := range cells[1:] {
		issue := jsObject{}
		issue.set("__format", format)
		issue.set("__row", float64(index+2))
		for column, key := range header {
			if key == "" {
				continue
			}
			var value any = undefinedValue{}
			if column < len(row) {
				value = planeUnformula(row[column])
			}
			if contains(planeLists, key) {
				text := jsTrim(planeStr(value))
				switch {
				case format == "csv":
					list := []any{}
					if text != "" {
						v, err := decodeJS([]byte(text))
						if err != nil || !json.Valid([]byte(text)) {
							report(at+"/row/"+strconv.Itoa(index+2)+"/"+key, "Plane "+key+` "`+jsSlice16(text, 80)+`" is not a JSON list`)
						} else if l, ok := v.([]any); ok {
							list = l
						}
					}
					value = list
				case contains(planeNameLists, key):
					list := []any{}
					if text != "" {
						for _, n := range strings.Split(text, ", ") {
							if n = jsTrim(n); n != "" {
								list = append(list, n)
							}
						}
					}
					value = list
				default:
					if text != "" {
						report(at+"/row/"+strconv.Itoa(index+2)+"/"+key, "Plane's XLSX export writes "+key+" as Python text, not JSON; it is not imported (use the JSON or CSV export)")
					}
					value = []any{}
				}
			}
			issue.set(key, value)
		}
		out = append(out, issue)
	}
	return out, nil
}

// planeCellValue is planeArchive.js's cellValue: a workbook cell as the
// parser reads it - numbers, booleans and text as they are.
func planeCellValue(v any) any {
	switch x := v.(type) {
	case nil, undefinedValue:
		return ""
	case jsDate:
		return jsISO(time.Time(x))
	case jsObject:
		if runs, ok := jsGet(x, "richText").([]any); ok {
			var b strings.Builder
			for _, r := range runs {
				b.WriteString(planeStr(jsOr(jsGet(r, "text"), "")))
			}
			return b.String()
		}
		if t := jsGetOrUndef(x, "text"); !isUndef(t) {
			return planeStr(t)
		}
		if r := jsGetOrUndef(x, "result"); !isUndef(r) {
			return planeCellValue(r)
		}
		return ""
	}
	return v
}

func readPlaneWorkbook(data []byte) ([][]any, error) {
	if len(data) == 0 {
		return nil, errors.New("Plane workbook is empty")
	}
	if len(data) > maxPlaneFileBytes {
		return nil, errors.New("Plane workbook is larger than " + strconv.Itoa(maxPlaneFileBytes) + " bytes")
	}
	if !bytes.HasPrefix(data, []byte("PK\x03\x04")) && !bytes.HasPrefix(data, []byte("PK\x05\x06")) {
		return nil, errors.New("Plane workbook is not an .xlsx file")
	}
	sheets, err := readXlsxValues(data)
	if err != nil {
		return nil, errors.New("Plane workbook is not a readable .xlsx file")
	}
	var rows [][]any
	for _, r := range sheets[0].Rows {
		if len(r) == 0 {
			continue // a gap: ExcelJS's eachRow does not visit it
		}
		row := make([]any, len(r))
		for i, c := range r {
			if isUndef(c) {
				row[i] = undefinedValue{} // a hole, as map() leaves it
			} else {
				row[i] = planeCellValue(c)
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

type planeFile struct {
	name, format, content string
	rows                  [][]any
}

func readPlaneArchive(raw []byte) ([]planeFile, error) {
	if len(raw) == 0 {
		return nil, errors.New("Plane export is empty")
	}
	if len(raw) > maxPlaneZipBytes {
		return nil, errors.New("Plane export is larger than " + strconv.Itoa(maxPlaneZipBytes) + " bytes")
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, errors.New("Plane export is not a readable .zip file")
	}
	var entries []*zip.File
	for _, f := range zr.File {
		if !f.FileInfo().IsDir() {
			entries = append(entries, f)
		}
	}
	if len(entries) > maxPlaneZipEntries {
		return nil, errors.New("Plane export has more than " + strconv.Itoa(maxPlaneZipEntries) + " files")
	}
	for _, f := range entries {
		if f.Name == "[Content_Types].xml" {
			return nil, errors.New("Plane export is an .xlsx workbook, not the export .zip; choose the .zip or the workbook inside it")
		}
	}
	type exported struct {
		f      *zip.File
		format string
	}
	var files []exported
	for _, f := range entries {
		if planeHidden.MatchString(f.Name) {
			continue
		}
		if m := planeExt.FindStringSubmatch(f.Name); m != nil {
			if fm := strings.ToLower(m[1]); fm == "json" || fm == "csv" || fm == "xlsx" {
				files = append(files, exported{f, fm})
			}
		}
	}
	if len(files) == 0 {
		return nil, errors.New("Plane export has no .json, .csv or .xlsx file")
	}
	budget := maxPlaneInflated
	var out []planeFile
	for _, e := range files {
		if e.f.UncompressedSize64 > maxPlaneFileBytes {
			return nil, errors.New("Plane " + e.f.Name + " is too large")
		}
		limit := maxPlaneFileBytes
		if budget < limit {
			limit = budget
		}
		b, err := readBounded(e.f, int64(limit))
		if errors.Is(err, errTooLarge) {
			return nil, errors.New("Plane " + e.f.Name + " is too large")
		}
		if err != nil {
			return nil, errors.New("Plane " + e.f.Name + " could not be read")
		}
		budget -= len(b)
		name := e.f.Name[strings.LastIndex(e.f.Name, "/")+1:]
		if e.format == "xlsx" {
			rows, err := readPlaneWorkbook(b)
			if err != nil {
				return nil, err
			}
			out = append(out, planeFile{name: name, format: "xlsx", rows: rows})
		} else {
			out = append(out, planeFile{name: name, format: e.format, content: strings.ToValidUTF8(string(b), "�")})
		}
	}
	return out, nil
}

// parsePlaneText is the import page's and importBoard's reading: the export
// .zip, a workbook from it, or the text of its JSON or CSV file.
func parsePlaneText(text string) (jsObject, error) {
	if strings.HasPrefix(text, "PK\x03\x04") || strings.HasPrefix(text, "PK\x05\x06") {
		if zr, err := zip.NewReader(strings.NewReader(text), int64(len(text))); err == nil {
			for _, f := range zr.File {
				if f.Name == "[Content_Types].xml" {
					rows, err := readPlaneWorkbook([]byte(text))
					if err != nil {
						return nil, err
					}
					return parsePlaneFiles([]planeFile{{name: "workbook.xlsx", format: "xlsx", rows: rows}})
				}
			}
		}
		files, err := readPlaneArchive([]byte(text))
		if err != nil {
			return nil, err
		}
		return parsePlaneFiles(files)
	}
	return parsePlaneExport(text, nil)
}

func parsePlaneFiles(files []planeFile) (jsObject, error) { return parsePlaneExport(nil, files) }

func planeNames(v any) []string {
	var out []string
	for _, item := range taigaList(v) {
		if s := jsTrim(planeStr(item)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func parsePlaneExport(input any, files []planeFile) (jsObject, error) {
	unsupported := []any{}
	report := func(path, reason string) { unsupported = append(unsupported, importUnsupportedObj(path, reason)) }
	var issues []any
	switch {
	case files != nil:
		if len(files) == 0 {
			return nil, errors.New("Plane export has no .json, .csv or .xlsx file")
		}
		for _, f := range files {
			at := "/" + jsOr(f.name, "file").(string)
			var part []any
			var err error
			switch f.format {
			case "json":
				part, err = planeJSONIssues(strings.TrimPrefix(f.content, "\ufeff"), at)
			case "csv":
				rows, cerr := jsReadCsv(f.content, "Plane", ',')
				if cerr != nil {
					return nil, cerr
				}
				part, err = planeTableIssues(planeRows(rows), "csv", at, report)
			case "xlsx":
				part, err = planeTableIssues(f.rows, "xlsx", at, report)
			}
			if err != nil {
				return nil, err
			}
			issues = append(issues, part...)
		}
	default:
		text, _ := input.(string)
		if utf16Len(text) > maxPlaneText {
			return nil, errors.New("Plane export is too large")
		}
		text = jsTrim(strings.TrimPrefix(text, "\ufeff"))
		if text == "" {
			return nil, errors.New("Plane export is empty")
		}
		var err error
		if strings.HasPrefix(text, "[") {
			issues, err = planeJSONIssues(text, "text")
		} else {
			rows, cerr := jsReadCsv(text, "Plane", ',')
			if cerr != nil {
				return nil, cerr
			}
			issues, err = planeTableIssues(planeRows(rows), "csv", "/csv", report)
		}
		if err != nil {
			return nil, err
		}
	}
	if len(issues) > maxPlaneIssues {
		return nil, errors.New("Plane export has more than " + strconv.Itoa(maxPlaneIssues) + " issues")
	}
	if len(issues) == 0 {
		return nil, errors.New("Plane export has no issues")
	}
	// The serializer has no description: say so once rather than per issue.
	report("/description", "Plane's export does not include issue descriptions; cards are imported without them")
	var projectKeys []string
	projectNames := map[string]string{}
	unknown := map[string]bool{}
	var columns []string
	var tasks []jsObject
	relations := map[string]bool{}
	updated := false
	for index, raw := range issues {
		at := "/issues/" + strconv.Itoa(index)
		if r := jsGet(raw, "__row"); jsTruthy(r) {
			at = "/row/" + jsStr(r)
		}
		obj, ok := raw.(jsObject)
		if !ok {
			report(at, "a Plane issue that is not an object is not imported")
			continue
		}
		for _, kv := range obj {
			if !strings.HasPrefix(kv.Key, "__") && !contains(planeFields, kv.Key) {
				unknown[kv.Key] = true
			}
		}
		text := func(key string) string { return jsTrim(planeStr(planeUnformula(jsGetOrUndef(obj, key)))) }
		title := text("name")
		if title == "" {
			report(at, "a Plane issue without a name is not imported")
			continue
		}
		identifierKey := text("project_identifier")
		projectName := jsOr(text("project_name"), identifierKey, "Default").(string)
		projectKey := jsOr(identifierKey, projectName).(string)
		if _, has := projectNames[projectKey]; !has {
			projectKeys = append(projectKeys, projectKey)
			projectNames[projectKey] = projectName
		}
		state := jsOr(text("state_name"), "No state").(string)
		columns = addOnce(columns, state)
		sequence := text("sequence_id")
		ref := text("identifier")
		if ref == "" && identifierKey != "" && sequence != "" {
			ref = identifierKey + "-" + sequence
		}
		date := func(key string) string {
			value := text(key)
			iso, ok := planeDate(value)
			if value != "" && !ok {
				report(at+"/"+key, `Plane date "`+jsSlice16(value, 40)+`" is not an ISO 8601 date`)
			}
			return iso
		}
		custom := jsObject{}
		priority := jsLower(text("priority"))
		if p, ok := planePriorities[priority]; ok {
			custom.set("Priority", p)
		} else if jsPrototypeKeys[priority] {
			custom.set("Priority", undefinedValue{}) // PLANE_PRIORITIES["constructor"]: a function, left out of JSON
		} else if priority != "" && priority != "none" {
			report(at+"/priority", `Plane priority "`+priority+`" is not one of urgent, high, medium, low or none`)
		}
		if estimate := text("estimate"); estimate != "" {
			if n, ok := jsToNumber(estimate); ok && !math.IsInf(n, 0) {
				custom.set("Estimate", n)
			} else {
				custom.set("Estimate", estimate)
			}
		}
		if cycles := planeNames(jsGet(obj, "cycles")); len(cycles) > 0 {
			custom.set("Cycle", strings.Join(cycles, ", "))
		}
		if modules := planeNames(jsGet(obj, "modules")); len(modules) > 0 {
			custom.set("Module", strings.Join(modules, ", "))
		}
		people := uniqStrings(planeNames(jsGet(obj, "assignees")))
		watchers := uniqStrings(planeNames(jsGet(obj, "subscribers")))
		archived := text("archived_at")
		if archived != "" {
			date("archived_at")
		}
		if text("updated_at") != "" {
			updated = true
		}
		if d := jsGet(obj, "is_draft"); d == true || planeTrue.MatchString(jsTrim(planeStr(d))) {
			report(at+"/is_draft", "a Plane draft issue is imported as an ordinary card")
		}
		if n, ok := jsToNumber(text("attachment_count")); ok && !math.IsInf(n, 0) && n > 0 {
			report(at+"/attachment_count", jsStr(n)+" attachment(s) are not in Plane's export and are not imported")
		}
		var lines []string
		for i, link := range taigaList(jsGet(obj, "links")) {
			var u any = link
			if spIsObject(link) {
				u = jsGetOrUndef(link, "url")
			}
			url := jsTrim(planeStr(u))
			if !planeWebLink.MatchString(url) {
				if url != "" {
					report(at+"/links/"+strconv.Itoa(i), `Plane link "`+jsSlice16(url, 80)+`" is not an http(s) or mailto link and is not imported`)
				}
				continue
			}
			title := ""
			if spIsObject(link) {
				title = jsTrim(planeStr(jsGetOrUndef(link, "title")))
			}
			lines = append(lines, "- "+markdownLinkText(jsOr(title, url).(string), url))
		}
		description := ""
		if len(lines) > 0 {
			description = "Links:\n" + strings.Join(lines, "\n")
		}
		var comments []any
		for _, c := range taigaList(jsGet(obj, "comments")) {
			if !spIsObject(c) {
				continue
			}
			text := planeStr(jsGetOrUndef(c, "comment"))
			if jsTrim(text) == "" {
				continue
			}
			o := jsObject{}
			o.set("text", text)
			if a := jsTrim(planeStr(jsGetOrUndef(c, "created_by"))); a != "" {
				o.set("author", a)
			}
			if d, ok := planeDate(jsGetOrUndef(c, "created_at")); ok {
				o.set("date", d)
			}
			comments = append(comments, o)
		}
		var deps []any
		for _, rel := range taigaList(jsGet(obj, "relations")) {
			if !spIsObject(rel) {
				continue
			}
			kind := jsTrim(planeStr(jsGetOrUndef(rel, "type")))
			other := jsTrim(planeStr(jsGetOrUndef(rel, "issue")))
			mapped, ok := planeRelations[kind]
			if !ok {
				report(at+"/relations", `Plane "`+kind+`" relations have no WeKan dependency type and are not imported`)
				continue
			}
			typ := mapped[0]
			if jsGet(rel, "direction") == "incoming" {
				typ = mapped[1]
			}
			if ref == "" || other == "" {
				continue
			}
			key := func(from, to, t string) string {
				if t == "related-to" {
					pair := []string{from, to}
					sort.Strings(pair)
					return "related:" + strings.Join(pair, ":")
				}
				return from + ":" + t + ":" + to
			}
			// Plane lists a relation on both issues; it is kept once.
			if relations[key(ref, other, typ)] || relations[key(other, ref, planeInverse[typ])] {
				continue
			}
			relations[key(ref, other, typ)] = true
			d := jsObject{}
			d.set("ref", other)
			d.set("type", typ)
			deps = append(deps, d)
		}
		parent := text("parent")
		created, started, due, ended := date("created_at"), date("start_date"), date("target_date"), date("completed_at")
		t := jsObject{}
		t.set("title", title)
		t.set("description", description)
		t.set("column_name", state)
		t.set("swimlane_key", projectKey)
		t.set("tags", uniqStrings(planeNames(jsGet(obj, "labels"))))
		if ref != "" {
			t.set("ref", ref)
		}
		if parent != "" {
			t.set("parent_ref", parent)
		}
		if len(people) > 0 {
			t.set("owner_username", people[0])
		}
		if len(people) > 1 {
			t.set("assignees", people[1:])
		}
		if len(watchers) > 0 {
			t.set("watchers", watchers)
		}
		if r := text("created_by_name"); r != "" {
			t.set("requested_by", r)
		}
		for _, d := range []struct{ key, value string }{{"date_creation", created}, {"date_started", started}, {"date_due", due}, {"date_end", ended}} {
			if d.value != "" {
				t.set(d.key, d.value)
			}
		}
		if archived != "" {
			t.set("archived", true)
		}
		if len(custom) > 0 {
			t.set("custom_fields", custom)
		}
		if len(comments) > 0 {
			t.set("comments", comments)
		}
		if len(deps) > 0 {
			t.set("dependencies", deps)
		}
		tasks = append(tasks, t)
	}
	if updated {
		report("/updated_at", "Plane's updated_at has no WeKan place and is not imported")
	}
	var keys []string
	for k := range unknown {
		if !planeDerived[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		report("/fields/"+k, `Plane field "`+k+`" has no WeKan place and is not imported`)
	}
	// Swimlanes: one per project when there are several; a project name that
	// two identifiers share is told apart by the identifier.
	swimlaneOf := map[string]string{}
	for _, k := range projectKeys {
		name := projectNames[k]
		switch {
		case len(projectKeys) == 1:
			swimlaneOf[k] = "Default"
		default:
			same := 0
			for _, other := range projectKeys {
				if projectNames[other] == name {
					same++
				}
			}
			if same > 1 {
				swimlaneOf[k] = name + " (" + k + ")"
			} else {
				swimlaneOf[k] = name
			}
		}
	}
	all := []any{}
	for _, t := range tasks {
		t.set("swimlane_name", swimlaneOf[jsGet(t, "swimlane_key").(string)])
		t.del("swimlane_key")
		all = append(all, t)
	}
	var lanes []string
	for _, k := range projectKeys {
		lanes = addOnce(lanes, swimlaneOf[k])
	}
	laneObjs := []any{}
	for _, l := range lanes {
		o := jsObject{}
		o.set("name", l)
		laneObjs = append(laneObjs, o)
	}
	name := "Imported Plane issues"
	if len(projectKeys) == 1 {
		name = projectNames[projectKeys[0]]
	}
	if len(columns) == 0 {
		columns = []string{"No state"}
	}
	out := importDoc(name, csvColumns(columns), all, []any{}, unsupported)
	out.set("swimlanes", laneObjs)
	return out, nil
}

// planeRows is a CSV's rows as table cells.
func planeRows(rows [][]string) [][]any {
	out := make([][]any, len(rows))
	for i, r := range rows {
		out[i] = make([]any, len(r))
		for j, c := range r {
			out[i][j] = c
		}
	}
	return out
}
