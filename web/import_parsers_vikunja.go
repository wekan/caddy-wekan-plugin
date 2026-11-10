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
	"unicode/utf16"
)

// Vikunja's user data export: Node.js WeKan's parseVikunjaExport
// (models/lib/vikunjaFormat.js) on what server/lib/vikunjaArchive.js reads
// from its .zip - the projects under one parent as swimlanes, the first
// kanban view's buckets as lists, tasks with their labels, people, dates,
// priority, percent done, description and checklists, comments, subtasks
// and relations. The text of data.json alone imports too.

func init() { importTextParsers["vikunja"] = parseVikunjaText }

const (
	maxVikunjaZipBytes   = 64 << 20
	maxVikunjaDataBytes  = 64 << 20
	maxVikunjaSmallBytes = 1 << 20
	maxVikunjaEntries    = 20000
	maxVikunjaTasks      = 20000
)

var (
	vikunjaMinVersion  = [3]int{0, 20, 1}
	vikunjaPriorities  = []string{"", "Low", "Medium", "High", "Urgent", "DO NOW"}
	vikunjaVersionRe   = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:[-+.~][0-9A-Za-z.+~-]*)?$`)
	vikunjaFilesRe     = regexp.MustCompile(`^files/\d+$`)
	vikunjaRelation    = map[string]string{"blocking": "blocks", "blocked": "is-blocked-by", "related": "related-to", "duplicateof": "duplicates", "duplicates": "is-duplicated-by"}
	vikunjaInverseType = map[string]string{"blocks": "is-blocked-by", "is-blocked-by": "blocks", "duplicates": "is-duplicated-by", "is-duplicated-by": "duplicates", "related-to": "related-to"}
)

// vikunjaNum is the module's num(): a finite number, or what Number() makes
// of anything but "" and null, when that is finite.
func vikunjaNum(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, !math.IsInf(x, 0) && !math.IsNaN(x)
	case nil, undefinedValue:
		return 0, false
	case string:
		if x == "" {
			return 0, false
		}
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	}
	n, ok := jsToNumber(jsStr(v))
	return n, ok && !math.IsInf(n, 0)
}

// vikunjaKey is a Map key: SameValueZero, so 1 and "1" are two keys, and
// an object is only ever itself.
func vikunjaKey(v any) string {
	switch x := v.(type) {
	case float64:
		return "n" + jsStr(x)
	case string:
		return "s" + x
	case bool:
		return "b" + jsStr(x)
	case nil:
		return "null"
	case undefinedValue:
		return "undefined"
	}
	return "object" // never the key of a lookup by id
}

// vikunjaSame is === between two values read from the file, undefined
// included: undefined === undefined, and objects are never the same.
func vikunjaSame(a, b any) bool {
	k := vikunjaKey(a)
	return k != "object" && k == vikunjaKey(b)
}

func vikunjaDate(v any) string {
	s, ok := v.(string)
	if !ok || s == "" || strings.HasPrefix(s, "0001-01-01") {
		return ""
	}
	t, ok := jsDateV8(s)
	if !ok {
		return ""
	}
	return jsISO(t)
}

func vikunjaUsername(user any) string {
	if !spIsObject(user) {
		return ""
	}
	return jsOr(jsTrim(spText(jsGet(user, "username"))), jsTrim(spText(jsGet(user, "name")))).(string)
}

func vikunjaPosition(v any) float64 {
	if n, ok := vikunjaNum(jsGetOrUndef(v, "position")); ok {
		return n
	}
	return 0
}

func vikunjaObjects(v any) []any {
	var out []any
	for _, x := range taigaList(v) {
		if spIsObject(x) {
			out = append(out, x)
		}
	}
	return out
}

func vikunjaSortByPosition(items []any) []any {
	out := append([]any{}, items...)
	jsSort(out, func(a, b any) float64 { return vikunjaPosition(a) - vikunjaPosition(b) })
	return out
}

type vikunjaLayout struct {
	titles   []string
	columnOf func(task any) string
	position func(task any, index int) float64
}

func vikunjaProjectLayout(project any, at string, report func(path, reason string)) vikunjaLayout {
	views := vikunjaObjects(jsGet(project, "views"))
	buckets := vikunjaObjects(jsGet(project, "buckets"))
	var kanbans []any
	for _, v := range views {
		if k := jsGet(v, "view_kind"); k == "kanban" || k == 3.0 {
			kanbans = append(kanbans, v)
		}
	}
	kanbans = vikunjaSortByPosition(kanbans)
	var view any
	if len(kanbans) > 0 {
		view = kanbans[0]
	}
	if len(kanbans) > 1 {
		report(at+"/views", `only the first kanban view's buckets ("`+spText(jsGet(view, "title"))+`") become lists; `+strconv.Itoa(len(kanbans)-1)+" other kanban view(s) are not imported")
	}
	var own []any
	switch {
	case view != nil:
		for _, b := range buckets {
			if vikunjaSame(jsGetOrUndef(b, "project_view_id"), jsGetOrUndef(view, "id")) {
				own = append(own, b)
			}
		}
		// A bucket without a view in a file that has views: older than its views.
		if len(own) == 0 {
			for _, b := range buckets {
				if !jsTruthy(jsGet(b, "project_view_id")) {
					own = append(own, b)
				}
			}
		}
	case len(views) == 0:
		own = buckets // the 0.21-0.23 layout: buckets belong to the project
	}
	own = vikunjaSortByPosition(own)
	titles := map[string]string{}
	var titleOrder []string
	for _, b := range own {
		title := jsOr(jsTrim(spText(jsGet(b, "title"))), "Untitled").(string)
		key := vikunjaKey(jsGetOrUndef(b, "id"))
		if _, seen := titles[key]; !seen {
			titleOrder = append(titleOrder, key)
		}
		titles[key] = title
		if n, ok := vikunjaNum(jsGetOrUndef(b, "limit")); ok && n > 0 {
			report(at+"/buckets/"+jsStr(jsGetOrUndef(b, "id"))+"/limit", `bucket "`+title+`" has a limit of `+jsStr(jsGetOrUndef(b, "limit"))+" tasks; WIP limits are not imported")
		}
	}
	var doneBucket, defaultBucket any = undefinedValue{}, undefinedValue{}
	if view != nil && jsTruthy(jsGet(view, "done_bucket_id")) {
		doneBucket = jsGet(view, "done_bucket_id")
	} else {
		for _, b := range own {
			if jsTruthy(jsGet(b, "is_done_bucket")) {
				doneBucket = jsGetOrUndef(b, "id")
				break
			}
		}
	}
	if view != nil && jsTruthy(jsGet(view, "default_bucket_id")) {
		defaultBucket = jsGet(view, "default_bucket_id")
	} else if len(own) > 0 {
		defaultBucket = jsGetOrUndef(own[0], "id")
	}
	bucketOf := map[string]any{}
	positionOf := map[string]any{}
	if view != nil {
		if links, ok := jsGet(project, "task_buckets").([]any); ok {
			for _, l := range links {
				if !spIsObject(l) {
					continue
				}
				pv := jsGetOrUndef(l, "project_view_id")
				if _, undef := pv.(undefinedValue); !undef && !vikunjaSame(pv, jsGetOrUndef(view, "id")) {
					continue
				}
				k := vikunjaKey(jsGetOrUndef(l, "task_id"))
				if _, has := bucketOf[k]; !has {
					bucketOf[k] = jsGetOrUndef(l, "bucket_id")
				}
			}
		}
		if positions, ok := jsGet(project, "positions").([]any); ok {
			for _, p := range positions {
				if spIsObject(p) && vikunjaSame(jsGetOrUndef(p, "project_view_id"), jsGetOrUndef(view, "id")) {
					if n, ok := vikunjaNum(jsGetOrUndef(p, "position")); ok {
						positionOf[vikunjaKey(jsGetOrUndef(p, "task_id"))] = n
					} else {
						positionOf[vikunjaKey(jsGetOrUndef(p, "task_id"))] = undefinedValue{}
					}
				}
			}
		}
	}
	title := func(id any) (string, bool) { t, ok := titles[vikunjaKey(id)]; return t, ok }
	columnOf := func(task any) string {
		bucket := jsGetOrUndef(task, "bucket_id")
		if b, ok := bucketOf[vikunjaKey(jsGetOrUndef(task, "id"))]; ok {
			bucket = b
		}
		if t, ok := title(bucket); ok {
			return t
		}
		if jsTruthy(jsGet(task, "done")) {
			if t, ok := title(doneBucket); ok {
				return t
			}
		}
		if t, ok := title(defaultBucket); ok {
			return t
		}
		if len(own) > 0 {
			t, _ := title(jsGetOrUndef(own[0], "id"))
			return t
		}
		// A project without buckets: Vikunja's own default buckets.
		if jsTruthy(jsGet(task, "done")) {
			return "Done"
		}
		return "To-Do"
	}
	position := func(task any, index int) float64 {
		if p, ok := positionOf[vikunjaKey(jsGetOrUndef(task, "id"))].(float64); ok {
			return p
		}
		if n, ok := vikunjaNum(jsGetOrUndef(task, "kanban_position")); ok {
			return n
		}
		if n, ok := vikunjaNum(jsGetOrUndef(task, "position")); ok {
			return n
		}
		return float64(index)
	}
	var unique []string
	for _, k := range titleOrder {
		unique = addOnce(unique, titles[k])
	}
	return vikunjaLayout{titles: unique, columnOf: columnOf, position: position}
}

func vikunjaRelationKey(from, to, typ string) string {
	if typ == "related-to" {
		pair := []string{from, to}
		sort.Strings(pair)
		return "related:" + strings.Join(pair, ":")
	}
	return from + ":" + to + ":" + typ
}

type vikunjaState struct {
	parentOf    map[string]string
	relations   map[string]bool
	labelColors bool
}

func vikunjaID(v any) string {
	if v == nil {
		return ""
	}
	if _, undef := v.(undefinedValue); undef {
		return ""
	}
	return jsStr(v)
}

func vikunjaParseTask(task any, at, swimlane, column string, archived bool, report func(path, reason string), state *vikunjaState) jsObject {
	title := jsTrim(spText(jsGet(task, "title")))
	if title == "" {
		report(at, "a Vikunja task without a title is not imported")
		return nil
	}
	ref := vikunjaID(jsGetOrUndef(task, "id"))
	text, checklists := vikunjaHTMLToText(jsGet(task, "description"), true)
	var people []any
	for _, a := range taigaList(jsGet(task, "assignees")) {
		if u := vikunjaUsername(a); u != "" {
			people = append(people, u)
		}
	}
	custom := jsObject{}
	if p, ok := vikunjaNum(jsGetOrUndef(task, "priority")); ok && p != 0 {
		if p == math.Trunc(p) && p >= 1 && p <= 5 {
			custom.set("Priority", vikunjaPriorities[int(p)])
		} else {
			report(at+"/priority", "Vikunja priority "+jsStr(jsGetOrUndef(task, "priority"))+" is not one of 0-5")
		}
	}
	if p, ok := vikunjaNum(jsGetOrUndef(task, "percent_done")); ok && p > 0 {
		custom.set("Percent Done", jsRound(math.Min(p, 1)*100))
	}
	if jsGet(task, "done") == true {
		custom.set("Done", true)
	}
	labels := vikunjaObjects(jsGet(task, "labels"))
	var labelTitles []string
	for _, l := range labels {
		if jsTrim(spText(jsGet(l, "hex_color"))) != "" {
			state.labelColors = true
		}
		labelTitles = append(labelTitles, jsTrim(spText(jsGet(l, "title"))))
	}
	end := vikunjaDate(jsGet(task, "end_date"))
	if end == "" && jsGet(task, "done") == true {
		end = vikunjaDate(jsGet(task, "done_at"))
	}
	t := jsObject{}
	t.set("title", title)
	t.set("description", text)
	t.set("column_name", column)
	t.set("swimlane_name", swimlane)
	t.set("tags", taigaUniq(labelTitles))
	if ref != "" {
		t.set("ref", ref)
	}
	if len(people) > 0 {
		t.set("owner_username", people[0])
	}
	if len(people) > 1 {
		t.set("assignees", people[1:])
	}
	if u := vikunjaUsername(jsGet(task, "created_by")); u != "" {
		t.set("requested_by", u)
	}
	for _, d := range []struct{ key, value string }{{"date_due", vikunjaDate(jsGet(task, "due_date"))}, {"date_started", vikunjaDate(jsGet(task, "start_date"))},
		{"date_end", end}, {"date_creation", vikunjaDate(jsGet(task, "created"))}} {
		if d.value != "" {
			t.set(d.key, d.value)
		}
	}
	if c := jsTrim(spText(jsGet(task, "hex_color"))); c != "" {
		t.set("color", strings.TrimPrefix(c, "#"))
	}
	if archived {
		t.set("archived", true)
	}
	if len(custom) > 0 {
		t.set("custom_fields", custom)
	}
	if len(checklists) > 0 {
		var cls []any
		for _, c := range checklists {
			o := jsObject{}
			o.set("title", c.title)
			o.set("items", c.items)
			cls = append(cls, o)
		}
		t.set("checklists", cls)
	}
	var comments []any
	for _, c := range vikunjaObjects(jsGet(task, "comments")) {
		text, _ := vikunjaHTMLToText(jsGet(c, "comment"), false)
		if text == "" {
			continue
		}
		o := jsObject{}
		o.set("text", text)
		o.set("author", vikunjaUsername(jsGet(c, "author")))
		if d := vikunjaDate(jsGet(c, "created")); d != "" {
			o.set("date", d)
		}
		comments = append(comments, o)
	}
	if len(comments) > 0 {
		t.set("comments", comments)
	}
	// Relations: subtask/parenttask make the card tree, the rest dependencies.
	var deps []any
	if related, ok := jsGet(task, "related_tasks").(jsObject); ok {
		for _, kv := range related {
			var ids []string
			for _, other := range vikunjaObjects(kv.Value) {
				if id := jsGetOrUndef(other, "id"); id != nil {
					if _, undef := id.(undefinedValue); !undef {
						ids = append(ids, jsStr(id))
					}
				}
			}
			if len(ids) == 0 {
				continue
			}
			switch typ := vikunjaRelation[kv.Key]; {
			case kv.Key == "parenttask":
				t.set("parent_ref", ids[0])
				if len(ids) > 1 {
					report(at+"/related_tasks/parenttask", "a card has one parent; "+strconv.Itoa(len(ids)-1)+" further parent task(s) are not linked")
				}
			case kv.Key == "subtask":
				for _, child := range ids {
					if _, has := state.parentOf[child]; !has {
						state.parentOf[child] = ref
					}
				}
			case typ != "":
				for _, other := range ids {
					if ref == "" || state.relations[vikunjaRelationKey(ref, other, typ)] || state.relations[vikunjaRelationKey(other, ref, vikunjaInverseType[typ])] {
						continue
					}
					state.relations[vikunjaRelationKey(ref, other, typ)] = true
					d := jsObject{}
					d.set("ref", other)
					d.set("type", typ)
					deps = append(deps, d)
				}
			default:
				report(at+"/related_tasks/"+kv.Key, `Vikunja "`+kv.Key+`" relations have no WeKan dependency type and are not imported`)
			}
		}
	}
	if len(deps) > 0 {
		t.set("dependencies", deps)
	}
	// What has no place on a WeKan card.
	for _, a := range vikunjaObjects(jsGet(task, "attachments")) {
		file := jsGet(a, "file")
		if !spIsObject(file) {
			file = jsObject{}
		}
		fileID := vikunjaID(jsGetOrUndef(file, "id"))
		name := jsOr(spText(jsGet(file, "name")), "file "+fileID).(string)
		report(at+"/attachments/"+vikunjaID(jsGetOrUndef(a, "id")), `attachment "`+name+`" is not imported; its bytes are files/`+fileID+" in the export")
	}
	if n := len(taigaList(jsGet(task, "reminders"))) + len(taigaList(jsGet(task, "reminder_dates"))); n > 0 {
		report(at+"/reminders", strconv.Itoa(n)+" reminder(s) are not imported")
	}
	ra, raOK := vikunjaNum(jsGetOrUndef(task, "repeat_after"))
	rm, rmOK := vikunjaNum(jsGetOrUndef(task, "repeat_mode"))
	if raOK && ra > 0 || rmOK && rm == 1 {
		report(at+"/repeat_after", "a repeating Vikunja task is imported once; its repeat is not imported")
	}
	reactions := 0
	switch r := jsGet(task, "reactions").(type) {
	case jsObject:
		reactions = len(r)
	case []any:
		reactions = len(r)
	}
	if reactions > 0 {
		report(at+"/reactions", "reactions are not imported")
	}
	return t
}

// readVikunjaArchive is WeKan's: the export .zip's VERSION, data.json, the
// count of its files and of its saved filters, every read bounded.
func readVikunjaArchive(raw []byte) (jsObject, error) {
	if len(raw) == 0 {
		return nil, errors.New("Vikunja export is empty")
	}
	if len(raw) > maxVikunjaZipBytes {
		return nil, errors.New("Vikunja export is larger than " + strconv.Itoa(maxVikunjaZipBytes) + " bytes")
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, errors.New("Vikunja export is not a readable .zip file")
	}
	var entries []*zip.File
	for _, f := range zr.File {
		if !f.FileInfo().IsDir() {
			entries = append(entries, f)
		}
	}
	if len(entries) > maxVikunjaEntries {
		return nil, errors.New("Vikunja export has more than " + strconv.Itoa(maxVikunjaEntries) + " files")
	}
	named := func(name string) *zip.File {
		for _, f := range entries {
			if f.Name == name {
				return f
			}
		}
		return nil
	}
	dataEntry, versionEntry, filtersEntry := named("data.json"), named("VERSION"), named("filters.json")
	if dataEntry == nil {
		return nil, errors.New("Vikunja export has no data.json")
	}
	if versionEntry == nil {
		return nil, errors.New("Vikunja export has no VERSION file")
	}
	if dataEntry.UncompressedSize64 > maxVikunjaDataBytes {
		return nil, errors.New("Vikunja data.json is too large")
	}
	read := func(f *zip.File, max int64) (string, error) {
		b, err := readBounded(f, max)
		if err != nil {
			return "", errors.New("Vikunja " + f.Name + " could not be read")
		}
		return string(b), nil
	}
	version, err := read(versionEntry, maxVikunjaSmallBytes)
	if err != nil {
		return nil, err
	}
	data, err := read(dataEntry, maxVikunjaDataBytes)
	if err != nil {
		return nil, err
	}
	filters := 0
	if filtersEntry != nil {
		if text, err := read(filtersEntry, maxVikunjaSmallBytes); err == nil {
			if v, err := decodeJS([]byte(text)); err == nil && json.Valid([]byte(text)) {
				filters = len(taigaList(v))
			}
		}
	}
	files := 0
	for _, f := range entries {
		if vikunjaFilesRe.MatchString(f.Name) {
			files++
		}
	}
	a := jsObject{}
	a.set("version", jsTrim(version))
	a.set("data", data)
	a.set("files", float64(files))
	a.set("filters", float64(filters))
	return a, nil
}

// parseVikunjaText is the import page's and importBoard's reading: a .zip's
// bytes as the archive, else the text of data.json.
func parseVikunjaText(text string) (jsObject, error) {
	if strings.HasPrefix(text, "PK\x03\x04") || strings.HasPrefix(text, "PK\x05\x06") {
		archive, err := readVikunjaArchive([]byte(text))
		if err != nil {
			return nil, err
		}
		return parseVikunjaExport(archive)
	}
	return parseVikunjaExport(text)
}

func vikunjaCheckVersion(v any, report func(path, reason string)) error {
	value := jsTrim(jsStr(jsOr(v, "")))
	if value == "dev" {
		report("/VERSION", "a Vikunja development build wrote this export; it is read as the current layout")
		return nil
	}
	m := vikunjaVersionRe.FindStringSubmatch(value)
	if m == nil {
		return errors.New(`Vikunja export VERSION "` + jsSlice16(value, 40) + `" is not a Vikunja version`)
	}
	for i := 0; i < 3; i++ {
		n, _ := strconv.Atoi(m[i+1])
		if n > vikunjaMinVersion[i] {
			return nil
		}
		if n < vikunjaMinVersion[i] {
			return errors.New("Vikunja export version " + value + " is older than 0.20.1, which Vikunja's own import refuses too")
		}
	}
	return nil
}

// parseVikunjaExport is WeKan's: the text of data.json, its parsed array,
// or the archive { version, data, files, filters }.
func parseVikunjaExport(input any) (jsObject, error) {
	data := input
	var version any = undefinedValue{}
	var archive jsObject
	if a, ok := input.(jsObject); ok {
		archive = a
		version = jsGetOrUndef(a, "version")
		data = jsGetOrUndef(a, "data")
	}
	if s, ok := data.(string); ok {
		if jsTrim(s) == "" {
			return nil, errors.New("Vikunja export is empty")
		}
		if len(utf16.Encode([]rune(s))) > maxVikunjaDataBytes {
			return nil, errors.New("Vikunja data.json is larger than " + strconv.Itoa(maxVikunjaDataBytes) + " characters")
		}
		v, err := decodeJS([]byte(s))
		if err != nil || !json.Valid([]byte(s)) {
			return nil, errors.New("Vikunja data.json is not valid JSON")
		}
		data = v
	}
	if _, undef := data.(undefinedValue); undef || data == nil {
		return nil, errors.New("Vikunja export is empty")
	}
	list, ok := data.([]any)
	if !ok {
		return nil, errors.New("Vikunja data.json must be an array of projects")
	}
	unsupported := []any{}
	report := func(path, reason string) { unsupported = append(unsupported, importUnsupportedObj(path, reason)) }
	if _, undef := version.(undefinedValue); archive != nil && !undef {
		if err := vikunjaCheckVersion(version, report); err != nil {
			return nil, err
		}
	}
	// -1 is Vikunja's Favorites pseudo project, which its own import skips too.
	var projects []any
	for _, p := range list {
		if spIsObject(p) && jsGet(p, "id") != -1.0 {
			projects = append(projects, p)
		}
	}
	if len(projects) == 0 {
		return nil, errors.New("Vikunja export has no projects")
	}
	count := 0
	for _, p := range projects {
		count += len(taigaList(jsGet(p, "tasks")))
	}
	if count > maxVikunjaTasks {
		return nil, errors.New("Vikunja export has more than " + strconv.Itoa(maxVikunjaTasks) + " tasks")
	}
	// Projects are compared by identity: their index in projects.
	var chosen []int
	for i, p := range projects {
		if len(taigaList(jsGet(p, "tasks"))) > 0 {
			chosen = append(chosen, i)
		}
	}
	if len(chosen) == 0 {
		for i, p := range projects {
			if len(taigaList(jsGet(p, "buckets"))) > 0 {
				chosen = append(chosen, i)
			}
		}
	}
	if len(chosen) == 0 {
		chosen = []int{0}
	}
	byID := map[string]int{}
	for i, p := range projects {
		byID[vikunjaKey(jsGetOrUndef(p, "id"))] = i // the last of an id, as a Map keeps it
	}
	parents := map[string]any{}
	var parentOrder []string
	for _, i := range chosen {
		pid := jsOr(jsGetOrUndef(projects[i], "parent_project_id"), 0.0)
		k := vikunjaKey(pid)
		if _, seen := parents[k]; !seen {
			parentOrder = append(parentOrder, k)
		}
		parents[k] = pid
	}
	var parentID any = 0.0
	if len(parents) == 1 {
		parentID = parents[parentOrder[0]]
	}
	parent := -1
	if jsTruthy(parentID) {
		if i, ok := byID[vikunjaKey(parentID)]; ok {
			parent = i
		}
	}
	if parent != -1 {
		chosen = nil
		for i, p := range projects {
			if vikunjaSame(jsGetOrUndef(p, "parent_project_id"), parentID) {
				chosen = append(chosen, i)
			}
		}
	}
	imported := map[int]bool{}
	for _, i := range chosen {
		imported[i] = true
	}
	for i, p := range projects {
		if imported[i] || i == parent {
			continue
		}
		what := "has no tasks and is not imported"
		if len(taigaList(jsGet(p, "tasks"))) > 0 {
			what = "is not under the same parent project and is not imported"
		}
		report("/projects/"+jsStr(jsGetOrUndef(p, "id")), `project "`+spText(jsGet(p, "title"))+`" `+what)
	}
	boardName := jsTrim(spText(jsGet(projects[chosen[0]], "title")))
	swimlaneOf := map[int]string{}
	projectTitle := func(i int) string {
		return jsOr(jsTrim(spText(jsGet(projects[i], "title"))), "Project "+jsStr(jsGetOrUndef(projects[i], "id"))).(string)
	}
	if parent != -1 || len(chosen) > 1 {
		boardName = "Imported Vikunja"
		if parent != -1 {
			boardName = jsOr(jsTrim(spText(jsGet(projects[parent], "title"))), "Imported Vikunja").(string)
		}
		seen := map[string]int{}
		for _, i := range chosen {
			seen[projectTitle(i)]++
		}
		for _, i := range chosen {
			title := projectTitle(i)
			if seen[title] > 1 {
				title += " (" + jsStr(jsGetOrUndef(projects[i], "id")) + ")"
			}
			swimlaneOf[i] = title
		}
	} else {
		swimlaneOf[chosen[0]] = "Default"
	}
	var columns []string
	tasks := []any{}
	state := &vikunjaState{parentOf: map[string]string{}, relations: map[string]bool{}}
	for _, i := range chosen {
		project := projects[i]
		at := "/projects/" + jsStr(jsGetOrUndef(project, "id"))
		layout := vikunjaProjectLayout(project, at, report)
		for _, t := range layout.titles {
			columns = addOnce(columns, t)
		}
		if jsTrim(spText(jsGet(project, "hex_color"))) != "" {
			report(at+"/hex_color", "the project color is not imported")
		}
		if jsTrim(spText(jsGet(project, "description"))) != "" {
			report(at+"/description", "the project description is not imported")
		}
		bg := jsGet(project, "background_information")
		bgFile, bgOK := vikunjaNum(jsGetOrUndef(project, "background_file_id"))
		if spIsObject(bg) && jsTruthy(jsGet(bg, "id")) || bgOK && bgFile > 0 {
			report(at+"/background_information", "the project background image is not imported")
		}
		archived := jsGet(project, "is_archived") == true
		if archived {
			report(at+"/is_archived", "the project is archived; its tasks are imported as archived cards")
		}
		type positioned struct {
			task     any
			index    int
			position float64
		}
		var items []any
		for index, task := range vikunjaObjects(jsGet(project, "tasks")) {
			items = append(items, positioned{task, index, layout.position(task, index)})
		}
		jsSort(items, func(a, b any) float64 {
			x, y := a.(positioned), b.(positioned)
			if d := x.position - y.position; d != 0 && !math.IsNaN(d) {
				return d
			}
			return float64(x.index - y.index)
		})
		for _, it := range items {
			task := it.(positioned).task
			column := layout.columnOf(task)
			columns = addOnce(columns, column)
			parsed := vikunjaParseTask(task, at+"/tasks/"+vikunjaID(jsGetOrUndef(task, "id")), swimlaneOf[i], column, archived, report, state)
			if parsed != nil {
				tasks = append(tasks, parsed)
			}
		}
	}
	for n, task := range tasks {
		t := task.(jsObject)
		_, hasParent := jsLookup(t, "parent_ref")
		ref, _ := jsGet(t, "ref").(string)
		if p, ok := state.parentOf[ref]; !hasParent && ref != "" && ok {
			t.set("parent_ref", p)
			tasks[n] = t
		}
	}
	if state.labelColors {
		report("/labels", "Vikunja label colors are not imported; labels get WeKan's default color")
	}
	if archive != nil {
		if f, ok := vikunjaNum(jsGetOrUndef(archive, "filters")); ok && f > 0 {
			report("/filters.json", jsStr(jsGet(archive, "filters"))+" saved filter(s) are not imported")
		}
	}
	if len(columns) == 0 {
		columns = []string{"To-Do"}
	}
	var lanes []string
	for _, i := range chosen {
		lanes = addOnce(lanes, swimlaneOf[i])
	}
	laneObjs := []any{}
	for _, l := range lanes {
		o := jsObject{}
		o.set("name", l)
		laneObjs = append(laneObjs, o)
	}
	out := importDoc(jsOr(boardName, "Imported Vikunja"), csvColumns(columns), tasks, []any{}, unsupported)
	out.set("swimlanes", laneObjs)
	return out, nil
}
