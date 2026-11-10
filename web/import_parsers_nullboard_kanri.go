package web

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// Nullboard's .nbx (text) and Kanri's JSON export, as Node.js WeKan's
// nullboardFormat.js and kanriFormat.js parse them - checked against WeKan's
// own parsers.

func init() {
	importTextParsers["nullboard"] = parseNullboard
	importParsers["kanri"] = parseKanri
}

func nbCheck(board any, at string) error {
	obj, ok := board.(jsObject)
	if !ok {
		return errors.New("Nullboard file" + at + ": a board must be a JSON object")
	}
	for _, k := range []string{"format", "id", "revision", "title", "lists"} {
		if _, has := jsLookup(obj, k); !has {
			return errors.New("Nullboard file" + at + ": required board properties are missing (" + k + ")")
		}
	}
	_, lists := jsGet(obj, "lists").([]any)
	if !jsTruthy(jsGet(obj, "id")) || !jsTruthy(jsGet(obj, "revision")) || !lists {
		return errors.New("Nullboard file" + at + ": required board properties are empty")
	}
	if n, ok := jsToNumber(jsGetOrNil(obj, "format")); !ok || n != 20190412 {
		return errors.New(`Nullboard file` + at + `: unsupported blob format "` + jsStr(jsGetOrUndef(obj, "format")) + `", expecting "20190412"`)
	}
	return nil
}

// splitNoteText is WeKan's: the first non-blank line the title, the rest
// the description.
func splitNoteText(text string) (string, string) {
	lines := jsLines(text)
	for n, l := range lines {
		if jsTrim(l) != "" {
			return jsTrim(l), jsTrimEnd(strings.Join(lines[n+1:], "\n"))
		}
	}
	return "", ""
}

func parseNullboard(text string) (jsObject, error) {
	data, err := decodeJS([]byte(strings.TrimPrefix(text, "\ufeff")))
	if err != nil {
		return nil, errors.New("Nullboard file is not valid JSON")
	}
	boards, isArr := data.([]any)
	if !isArr {
		boards = []any{data}
	}
	if len(boards) == 0 {
		return nil, errors.New("Nullboard file holds no board")
	}
	for n, b := range boards {
		at := ""
		if isArr {
			at = " board " + strconv.Itoa(n+1)
		}
		if err := nbCheck(b, at); err != nil {
			return nil, err
		}
	}
	board := boards[0]
	unsupported := []any{}
	for n, other := range boards[1:] {
		unsupported = append(unsupported, importUnsupportedObj("/"+strconv.Itoa(n+1),
			`Nullboard board "`+jsOrString(jsGet(other, "title"))+`" is not imported: one import creates one board, import its own export`))
	}
	var columns []string
	tasks := []any{}
	collapsed := 0
	for li, list := range jsGet(board, "lists").([]any) {
		at := "/lists/" + strconv.Itoa(li)
		named, _ := jsGet(list, "title").(string)
		base := jsTrim(named)
		if base == "" {
			base = "Untitled list"
		}
		title := base
		for n := 2; contains(columns, title); n++ {
			title = base + " (" + strconv.Itoa(n) + ")"
		}
		if jsTrim(named) != "" && title != base {
			unsupported = append(unsupported, importUnsupportedObj(at+"/title", `a second Nullboard list "`+base+`" is imported as "`+title+`"`))
		}
		columns = append(columns, title)
		notes, ok := jsGet(list, "notes").([]any)
		if _, has := jsLookup(list, "notes"); jsTruthy(list) && has && !ok {
			unsupported = append(unsupported, importUnsupportedObj(at+"/notes", "Nullboard list notes are not an array and are not imported"))
		}
		for ni, note := range notes {
			path := at + "/notes/" + strconv.Itoa(ni)
			text, ok := jsGet(note, "text").(string)
			if note == nil || !ok {
				unsupported = append(unsupported, importUnsupportedObj(path, "a Nullboard note without text is not imported"))
				continue
			}
			cardTitle, desc := splitNoteText(text)
			if cardTitle == "" {
				unsupported = append(unsupported, importUnsupportedObj(path, "an empty Nullboard note is not imported"))
				continue
			}
			if jsGet(note, "min") == true {
				collapsed++
			}
			t := jsObject{}
			t.set("title", cardTitle)
			t.set("description", desc)
			t.set("column_name", title)
			t.set("swimlane_name", "Default")
			tags := []any{}
			if jsGet(note, "raw") == true {
				tags = append(tags, "raw")
			}
			t.set("tags", tags)
			tasks = append(tasks, t)
		}
	}
	if collapsed > 0 {
		unsupported = append(unsupported, importUnsupportedObj("/lists/notes/min",
			strconv.Itoa(collapsed)+" collapsed Nullboard note(s) are imported expanded: a WeKan card has no collapsed state"))
	}
	name := "Imported Nullboard"
	if s, ok := jsGet(board, "title").(string); ok && jsTrim(s) != "" {
		name = jsTrim(s)
	}
	cols := []any{}
	for _, c := range columns {
		o := jsObject{}
		o.set("title", c)
		cols = append(cols, o)
	}
	return importDoc(name, cols, tasks, []any{}, unsupported), nil
}

// --- Kanri

// kanriText is text(): a string, a finite number as text, else ”.
func kanriText(v any) string { return importText(v) }

// kanriISODate is kanriFormat.js's isoDate: nil for none, false for what
// is not a date.
func kanriISODate(v any) (string, int) {
	if v == nil || v == "" {
		return "", 0 // none
	}
	if _, isUndef := v.(undefinedValue); isUndef {
		return "", 0
	}
	switch x := v.(type) {
	case string:
		if t, ok := jsParseDate(x); ok {
			return isoDate(t), 1
		}
	case float64:
		if t, ok := importedEpochMillis(x); ok {
			return isoDate(t), 1
		}
	case bool:
		if x {
			return isoDate(importEpochZero(1)), 1
		}
		return isoDate(importEpochZero(0)), 1
	}
	return "", -1 // not a date
}

// kanriTagColor is WeKan's: a tag's color, or its style's background-color.
func kanriTagColor(tag any) string {
	if _, ok := tag.(jsObject); !ok {
		return ""
	}
	if c, ok := jsGet(tag, "color").(string); ok && jsTrim(c) != "" {
		return jsTrim(c)
	}
	if m := kanriStyleBG.FindStringSubmatch(kanriText(jsGet(tag, "style"))); m != nil {
		return jsTrim(m[1])
	}
	return ""
}

var kanriStyleBG = jsRegexp(`(?i)background-color\s*:\s*([^;]+)`)

var kanriClassToWekan = map[string]string{"bg-pink-600": "pink", "bg-red-600": "red", "bg-orange-600": "orange",
	"bg-yellow-600": "yellow", "bg-green-600": "green", "bg-teal-600": "#0d9488", "bg-blue-600": "blue", "bg-purple-600": "purple"}

// wekanColorOf is WeKan's: a palette name for a hex WeKan draws, else the hex.
func wekanColorOf(hex string) string {
	for name, h := range wekanColorHex {
		if h == hex {
			return name
		}
	}
	return hex
}

// kanriCardColor is WeKan's: "" for none, false (ok=false) for what is not
// a color Kanri writes.
func kanriCardColor(v any) (string, bool) {
	c := jsTrim(kanriText(v))
	if c == "" || c == "bg-elevation-2" {
		return "", true
	}
	if w, ok := kanriClassToWekan[c]; ok {
		return w, true
	}
	if h := kanriHex(c); h != "" {
		return wekanColorOf(h), true
	}
	return "", false
}

func parseKanri(data any) (jsObject, error) {
	obj, ok := data.(jsObject)
	if !ok {
		return nil, errors.New("Kanri export must be a JSON object")
	}
	unsupported := []any{}
	boards, all := jsGet(obj, "boards").([]any)
	if !all {
		one, err := parseKanriBoard(obj, "", "Default", &unsupported, 20000)
		if err != nil {
			return nil, err
		}
		return importDoc(one.name, titles(one.columns), one.tasks, []any{}, unsupported), nil
	}
	if len(boards) == 0 {
		return nil, errors.New("Kanri export has no boards")
	}
	var settings []string
	for _, k := range []string{"activeTheme", "colors", "savedCustomTheme", "pins", "boardSortingOption", "columnZoomLevel"} {
		v, has := jsLookup(obj, k)
		if a, isArr := v.([]any); has && v != nil && !(isArr && len(a) == 0) {
			settings = append(settings, k)
		}
	}
	if len(settings) > 0 {
		unsupported = append(unsupported, importUnsupportedObj("/", "Kanri app settings ("+strings.Join(settings, ", ")+") belong to the app, not to a board, and are not imported"))
	}
	var columns, lanes []string
	tasks := []any{}
	laneColumns := jsObject{}
	cards := 0
	for n, board := range boards {
		at := "/boards/" + strconv.Itoa(n)
		if _, ok := board.(jsObject); !ok {
			unsupported = append(unsupported, importUnsupportedObj(at, "a Kanri board that is not an object is not imported"))
			continue
		}
		base := jsTrim(kanriText(jsGet(board, "title")))
		if base == "" {
			base = "Kanri board " + strconv.Itoa(n+1)
		}
		lane := base
		for k := 2; contains(lanes, lane); k++ {
			lane = base + " (" + strconv.Itoa(k) + ")"
		}
		one, err := parseKanriBoard(board, at, lane, &unsupported, 20000-cards)
		if err != nil {
			return nil, err
		}
		cards += len(one.tasks)
		lanes = append(lanes, lane)
		laneColumns.set(lane, toAny(one.columns))
		for _, c := range one.columns {
			if !contains(columns, c) {
				columns = append(columns, c)
			}
		}
		tasks = append(tasks, one.tasks...)
	}
	if len(lanes) == 0 {
		return nil, errors.New("Kanri export has no boards")
	}
	if len(lanes) == 1 {
		for _, t := range tasks {
			o := t.(jsObject)
			o.set("swimlane_name", "Default")
		}
		return importDoc(lanes[0], titles(columns), tasks, []any{}, unsupported), nil
	}
	out := jsObject{}
	b := jsObject{}
	b.set("name", "Kanri")
	out.set("board", b)
	out.set("columns", titles(columns))
	sw := []any{}
	for _, l := range lanes {
		o := jsObject{}
		o.set("name", l)
		sw = append(sw, o)
	}
	out.set("swimlanes", sw)
	out.set("swimlane_columns", laneColumns)
	out.set("tasks", tasks)
	out.set("warnings", []any{})
	out.set("unsupported", unsupported)
	return out, nil
}

func titles(names []string) []any {
	out := []any{}
	for _, n := range names {
		o := jsObject{}
		o.set("title", n)
		out = append(out, o)
	}
	return out
}

type kanriParsed struct {
	name    string
	columns []string
	tasks   []any
}

func parseKanriBoard(board any, at, lane string, unsupported *[]any, limit int) (kanriParsed, error) {
	columnsIn, ok := jsGet(board, "columns").([]any)
	if !ok {
		columnsIn, ok = jsGet(board, "lists").([]any)
	}
	if !ok {
		return kanriParsed{}, errors.New("Kanri board needs a columns array")
	}
	count := 0
	for _, c := range columnsIn {
		if cl, ok := jsGet(c, "cards").([]any); c != nil && ok {
			count += len(cl)
		}
	}
	if count > limit {
		return kanriParsed{}, errors.New("Kanri export has more than 20000 cards")
	}
	push := func(path, reason string) { *unsupported = append(*unsupported, importUnsupportedObj(path, reason)) }
	if bg, ok := jsGet(board, "background").(jsObject); ok {
		if src := jsTrim(kanriText(jsGet(bg, "src"))); src != "" {
			push(at+"/background", `Kanri background image "`+src+`" is a file on the computer that exported it and is not imported`)
		}
	}
	var columns []string
	tasks := []any{}
	used := map[string]bool{}
	for ci, column := range columnsIn {
		columnAt := at + "/columns/" + strconv.Itoa(ci)
		if _, ok := column.(jsObject); !ok {
			push(columnAt, "a Kanri column that is not an object is not imported")
			continue
		}
		base := jsTrim(kanriText(jsGet(column, "title")))
		if base == "" {
			base = "Untitled"
		}
		title := base
		for n := 2; contains(columns, title); n++ {
			title = base + " (" + strconv.Itoa(n) + ")"
		}
		if title != base {
			push(columnAt+"/title", `Kanri has two columns named "`+base+`"; this one is imported as "`+title+`"`)
		}
		columns = append(columns, title)
		cl, _ := jsGet(column, "cards").([]any)
		for k, card := range cl {
			cardAt := columnAt + "/cards/" + strconv.Itoa(k)
			if _, ok := card.(jsObject); !ok {
				push(cardAt, "a Kanri card that is not an object is not imported")
				continue
			}
			t := jsObject{}
			t.set("title", jsTrim(kanriText(jsGet(card, "name"))))
			t.set("description", kanriText(jsGet(card, "description")))
			t.set("column_name", title)
			t.set("swimlane_name", lane)
			// tags is the task's fifth key, as WeKan's literal makes it, and
			// filled below.
			t.set("tags", []any{})
			tags := []any{}
			if id := kanriText(jsGet(card, "id")); id != "" {
				t.set("ref", id)
			}
			due, state := kanriISODate(jsGetOrUndef(card, "dueDate"))
			if state == 1 {
				t.set("date_due", due)
			} else if state == -1 {
				push(cardAt+"/dueDate", `Kanri due date "`+kanriText(jsGet(card, "dueDate"))+`" is not a date`)
			}
			if state == 1 && jsGet(card, "isDueDateCompleted") == true {
				t.set("due_complete", true)
			}
			if state == 1 && jsGet(card, "isDueDateCounterRelative") == true {
				push(cardAt+"/isDueDateCounterRelative", "Kanri shows this due date as a countdown; WeKan shows the date")
			}
			if color, ok := kanriCardColor(jsGet(card, "color")); color != "" {
				t.set("color", color)
			} else if !ok {
				push(cardAt+"/color", `Kanri card color "`+kanriText(jsGet(card, "color"))+`" is not a color WeKan can show`)
			}
			tl, _ := jsGet(card, "tags").([]any)
			for _, tag := range tl {
				name := jsTrim(kanriText(jsGet(tag, "text")))
				dup := false
				for _, x := range tags {
					dup = dup || jsGet(x, "name") == name
				}
				if name == "" || dup {
					continue
				}
				raw := kanriTagColor(tag)
				hex := ""
				if raw != "" {
					hex = kanriHex(raw)
				}
				if raw != "" && hex == "" {
					push(cardAt+"/tags", `Kanri tag "`+name+`" color "`+raw+`" is not a color WeKan can show`)
				}
				o := jsObject{}
				o.set("name", name)
				if hex != "" {
					o.set("color", wekanColorOf(hex))
				}
				tags = append(tags, o)
				used[name] = true
			}
			t.set("tags", tags)
			var items []any
			il, _ := jsGet(card, "tasks").([]any)
			for _, it := range il {
				if _, ok := it.(jsObject); !ok || jsTrim(kanriText(jsGet(it, "name"))) == "" {
					continue
				}
				o := jsObject{}
				o.set("title", jsTrim(kanriText(jsGet(it, "name"))))
				o.set("done", jsGet(it, "finished") == true)
				items = append(items, o)
			}
			if len(items) > 0 {
				cl := jsObject{}
				cl.set("title", "Tasks")
				cl.set("items", items)
				t.set("checklists", []any{cl})
			}
			tasks = append(tasks, t)
		}
	}
	var unusedTags []string
	gt, _ := jsGet(board, "globalTags").([]any)
	for _, tag := range gt {
		if n := jsTrim(kanriText(jsGet(tag, "text"))); n != "" && !used[n] && !contains(unusedTags, n) {
			unusedTags = append(unusedTags, n)
		}
	}
	if len(unusedTags) > 0 {
		push(at+"/globalTags", "Kanri tags no card uses are not imported: "+strings.Join(unusedTags, ", "))
	}
	name := jsTrim(kanriText(jsGet(board, "title")))
	if name == "" {
		name = "Imported Kanri board"
	}
	return kanriParsed{name: name, columns: columns, tasks: tasks}, nil
}

// importedEpochMillis is new Date(n) for a number: milliseconds since 1970,
// within Date's range.
func importedEpochMillis(n float64) (time.Time, bool) {
	if n != n || n > 8.64e15 || n < -8.64e15 {
		return time.Time{}, false
	}
	return time.UnixMilli(int64(n)).UTC(), true
}

// importEpochZero is new Date(true) or new Date(false): 1 ms or 0 ms.
func importEpochZero(ms int64) time.Time { return time.UnixMilli(ms).UTC() }
