package web

import (
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Focalboard's board archive (board.jsonl): Node.js WeKan's parseFocalboard
// (models/lib/focalboardFormat.js) - the board's group-by select property as
// the lists, its other selects as labels, its first date as the card's dates,
// its text-like properties as custom fields, and a card's content blocks as
// its description, checklist and comments, in the card's own order.

func init() { importTextParsers["focalboard"] = parseFocalboard }

const maxFocalboardBlocks = 50000

var (
	focalboardCustomTypes   = map[string]bool{"text": true, "number": true, "email": true, "url": true, "phone": true, "checkbox": true}
	focalboardComputedTypes = map[string]bool{"createdTime": true, "updatedTime": true, "createdBy": true, "updatedBy": true}
	focalboardHeading       = regexp.MustCompile(`^h[1-3]$`)
	focalboardLabels        = regexp.MustCompile(`(?i)^labels$`)
	errFocalboardType       = errors.New("Cannot read properties of null")
)

// jsStrictEq is === between two JSON values: objects and arrays are never
// the same value once parsed.
func jsStrictEq(a, b any) bool {
	switch a.(type) {
	case nil:
		return b == nil
	case string, float64, bool:
		return a == b
	}
	return false
}

// jsTimeValue is new Date(Number(ms)).toISOString() for a positive finite
// ms: false for none, and an error where toISOString throws (past
// 8.64e15 ms).
func focalboardISO(ms any) (string, bool, error) {
	n, ok := jsToNumber(ms)
	if _, isUndef := ms.(undefinedValue); isUndef {
		ok = false
	}
	if !ok || math.IsInf(n, 0) || !(n > 0) {
		return "", false, nil
	}
	if n > 8.64e15 {
		return "", false, errors.New("Invalid time value")
	}
	t := time.UnixMilli(int64(math.Trunc(n))).UTC()
	return jsISO(t), true, nil
}

func parseFocalboard(text string) (doc jsObject, err error) {
	// What would throw in WeKan's parser - options that are not a list, an
	// option that is null - is the file's error here too.
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				doc, err = nil, e
				return
			}
			panic(r)
		}
	}()
	var lines []string
	for _, l := range jsLines(text) {
		if l = jsTrim(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > maxFocalboardBlocks {
		return nil, errors.New("Focalboard archive has more than " + strconv.Itoa(maxFocalboardBlocks) + " lines")
	}
	type row struct {
		typ  string
		data any
		line int
	}
	var rows []row
	for index, line := range lines {
		v, derr := decodeJS([]byte(line))
		if derr != nil || !json.Valid([]byte(line)) {
			return nil, errors.New("Focalboard line " + strconv.Itoa(index+1) + " is not JSON")
		}
		_, isObj := v.(jsObject)
		_, isArr := v.([]any)
		if index == 0 && isObj {
			_, hasType := jsLookup(v, "type")
			if !hasType || !jsTruthy(jsGet(v, "type")) {
				if n, ok := jsGet(v, "version").(float64); ok && n == math.Trunc(n) && !math.IsInf(n, 0) {
					continue
				}
			}
		}
		typ, typeIsString := jsGet(v, "type").(string)
		data := jsGet(v, "data")
		_, dataObj := data.(jsObject)
		_, dataArr := data.([]any)
		if !(isObj || isArr) || !typeIsString || !jsTruthy(data) || !(dataObj || dataArr) {
			return nil, errors.New("Focalboard line " + strconv.Itoa(index+1) + " is not an archive line")
		}
		rows = append(rows, row{typ, data, index + 1})
	}
	unsupported := []any{}
	var board any
	var blocks []any
	members := 0
	for _, r := range rows {
		at := "/line/" + strconv.Itoa(r.line)
		switch {
		case r.typ == "board" || (board == nil && r.typ == "block" && jsGet(r.data, "type") == "board"):
			if board != nil {
				unsupported = append(unsupported, importUnsupportedObj(at, "a second board in one file is not imported"))
				continue
			}
			if r.typ == "board" {
				board = r.data
			} else {
				b := append(jsObject{}, r.data.(jsObject)...)
				fields := jsGet(r.data, "fields")
				b.set("description", jsOr(jsGet(fields, "description"), ""))
				b.set("cardProperties", jsGetOrUndef(fields, "cardProperties"))
				board = b
			}
		case r.typ == "block":
			blocks = append(blocks, r.data)
		case r.typ == "boardMember":
			members++
		default:
			unsupported = append(unsupported, importUnsupportedObj(at, "archive line type "+r.typ+" has no WeKan field"))
		}
	}
	if board == nil {
		return nil, errors.New("Focalboard archive has no board line")
	}
	if members > 0 {
		unsupported = append(unsupported, importUnsupportedObj("/boardMember", strconv.Itoa(members)+" Focalboard member(s): Focalboard accounts are not WeKan's"))
	}
	var properties []any
	if list, ok := jsGet(board, "cardProperties").([]any); ok {
		for _, p := range list {
			_, idOK := jsGet(p, "id").(string)
			_, nameOK := jsGet(p, "name").(string)
			if jsTruthy(p) && idOK && nameOK {
				properties = append(properties, p)
			}
		}
	}
	byID := map[string]int{} // the property's index: the last of an id, as a Map keeps it
	for i, p := range properties {
		byID[jsGet(p, "id").(string)] = i
	}
	propByID := func(id any) int {
		if s, ok := id.(string); ok {
			if i, ok := byID[s]; ok {
				return i
			}
		}
		return -1
	}
	var live []any
	for _, b := range blocks {
		_, idOK := jsGet(b, "id").(string)
		del, _ := jsToNumber(jsGetOrUndef(b, "deleteAt"))
		if _, undef := jsGetOrUndef(b, "deleteAt").(undefinedValue); undef {
			del = math.NaN()
		}
		if jsTruthy(b) && idOK && !(del > 0) {
			live = append(live, b)
		}
	}
	groupBy := -1
	for _, b := range live {
		if jsGet(b, "type") == "view" && jsGet(jsGet(b, "fields"), "viewType") == "board" {
			if i := propByID(jsGet(jsGet(b, "fields"), "groupById")); i != -1 && jsGet(properties[i], "type") == "select" {
				groupBy = i
				break
			}
		}
	}
	viewFound := groupBy != -1
	if !viewFound {
		for i, p := range properties {
			if jsGet(p, "type") == "select" {
				groupBy = i
				break
			}
		}
	}
	optionsOf := func(property any) []any {
		o := jsGet(property, "options")
		if !jsTruthy(o) {
			return nil
		}
		list, ok := o.([]any)
		if !ok {
			panic(errors.New("(property.options || []).find is not a function"))
		}
		return list
	}
	option := func(property, id any) any {
		for _, item := range optionsOf(property) {
			if jsTruthy(item) && jsStrictEq(jsGet(item, "id"), id) {
				return item
			}
		}
		return nil
	}
	children := map[string][]any{}
	for _, b := range live {
		if p, ok := jsGet(b, "parentId").(string); ok {
			children[p] = append(children[p], b)
		}
	}
	tasks := []any{}
	var columnNames []any
	for _, card := range live {
		if jsGet(card, "type") != "card" {
			continue
		}
		cardID := jsGet(card, "id").(string)
		at := "/card/" + cardID
		fields := jsGet(card, "fields")
		if jsGet(fields, "isTemplate") == true {
			unsupported = append(unsupported, importUnsupportedObj(at, "a card template is not imported"))
			continue
		}
		var entries jsObject
		switch v := jsGet(fields, "properties").(type) {
		case jsObject:
			entries = v
		case []any:
			for i, e := range v {
				entries = append(entries, struct {
					Key   string
					Value any
				}{strconv.Itoa(i), e})
			}
		}
		tags := []any{}
		custom := jsObject{}
		var column any = "Cards"
		if groupBy != -1 {
			column = "No " + jsGet(properties[groupBy], "name").(string)
		}
		var from, to string
		hasDates := false
		for _, kv := range entries {
			id, value := kv.Key, kv.Value
			pi := propByID(id)
			if pi == -1 {
				unsupported = append(unsupported, importUnsupportedObj(at+"/properties/"+id, "a value of a property the board does not define"))
				continue
			}
			property := properties[pi]
			name := jsGet(property, "name").(string)
			typ := jsGet(property, "type")
			if pi == groupBy {
				if chosen := option(property, value); chosen != nil {
					column = jsOr(jsGetOrUndef(chosen, "value"), column)
				}
				continue
			}
			tag := func(chosen any) any {
				if focalboardLabels.MatchString(jsTrim(name)) {
					return jsGetOrUndef(chosen, "value")
				}
				return name + ":" + jsStr(jsGetOrUndef(chosen, "value"))
			}
			switch {
			case typ == "select":
				if chosen := option(property, value); chosen != nil {
					tags = append(tags, tag(chosen))
				}
				continue
			case typ == "multiSelect":
				ids, ok := value.([]any)
				if !ok {
					ids = []any{value}
				}
				for _, id2 := range ids {
					if chosen := option(property, id2); chosen != nil {
						tags = append(tags, tag(chosen))
					}
				}
				continue
			case typ == "date":
				r := value
				bad := false
				if s, ok := value.(string); ok {
					var derr error
					r, derr = decodeJS([]byte(s))
					bad = derr != nil || !json.Valid([]byte(s))
				}
				_, rObj := r.(jsObject)
				_, rArr := r.([]any)
				f, fok := "", false
				if !bad && jsTruthy(r) && (rObj || rArr) {
					var ierr error
					if f, fok, ierr = focalboardISO(jsGetOrUndef(r, "from")); ierr != nil {
						return nil, ierr
					}
				}
				if !fok {
					unsupported = append(unsupported, importUnsupportedObj(at+"/properties/"+id, name+" is not a Focalboard date"))
					continue
				}
				t, tok, ierr := focalboardISO(jsGetOrUndef(r, "to"))
				if ierr != nil {
					return nil, ierr
				}
				if !tok {
					t = ""
				}
				if !hasDates {
					from, to, hasDates = f, t, true
				} else {
					custom.set(name, f)
				}
				continue
			}
			if s, ok := typ.(string); ok && focalboardCustomTypes[s] {
				if _, undef := value.(undefinedValue); value != "" && value != nil && !undef {
					custom.set(name, jsStr(value))
				}
				continue
			}
			if s, ok := typ.(string); ok && focalboardComputedTypes[s] {
				continue
			}
			unsupported = append(unsupported, importUnsupportedObj(at+"/properties/"+name, "Focalboard "+jsStr(jsGetOrUndef(property, "type"))+" property "+name+" has no WeKan field"))
		}
		// Content, in the card's own order, then anything it does not list.
		own := children[cardID]
		var order []any
		var flat func(v any)
		flat = func(v any) {
			if list, ok := v.([]any); ok {
				for _, item := range list {
					if inner, ok := item.([]any); ok {
						flat(inner)
					} else {
						order = append(order, item)
					}
				}
			}
		}
		flat(jsGet(fields, "contentOrder"))
		var sorted []any
		for _, id := range order {
			for _, b := range own {
				if jsStrictEq(jsGet(b, "id"), id) {
					sorted = append(sorted, b)
					break
				}
			}
		}
		var rest []any
		for _, b := range own {
			listed := false
			for _, id := range order {
				listed = listed || jsStrictEq(jsGet(b, "id"), id)
			}
			if !listed {
				rest = append(rest, b)
			}
		}
		created := func(b any) float64 {
			if _, undef := jsGetOrUndef(b, "createAt").(undefinedValue); undef {
				return math.NaN()
			}
			n, _ := jsToNumber(jsGet(b, "createAt"))
			return n
		}
		jsSort(rest, func(a, b any) float64 { return created(a) - created(b) })
		sorted = append(sorted, rest...)
		var description []string
		items, comments := []any{}, []any{}
		for _, b := range sorted {
			content, _ := jsGet(b, "title").(string)
			btype, _ := jsGet(b, "type").(string)
			switch {
			case btype == "text":
				if content != "" {
					description = append(description, content)
				}
			case focalboardHeading.MatchString(btype):
				if content != "" {
					description = append(description, strings.Repeat("#", int(btype[1]-'0'))+" "+content)
				}
			case btype == "divider":
				description = append(description, "---")
			case btype == "checkbox":
				if content != "" {
					it := jsObject{}
					it.set("title", content)
					it.set("done", jsGet(jsGet(b, "fields"), "value") == true)
					items = append(items, it)
				}
			case btype == "comment":
				if content != "" {
					c := jsObject{}
					c.set("text", content)
					d, ok, ierr := focalboardISO(jsGetOrUndef(b, "createAt"))
					if ierr != nil {
						return nil, ierr
					}
					if ok {
						c.set("date", d)
					}
					comments = append(comments, c)
				}
			case btype == "image" || btype == "attachment":
				unsupported = append(unsupported, importUnsupportedObj(at+"/"+jsGet(b, "id").(string), "a Focalboard "+btype+": the file is in the archive, not in board.jsonl"))
			default:
				unsupported = append(unsupported, importUnsupportedObj(at+"/"+jsGet(b, "id").(string), "Focalboard "+jsStr(jsGetOrUndef(b, "type"))+" block has no WeKan field"))
			}
		}
		title := "Imported card"
		if s, ok := jsGet(card, "title").(string); ok && jsTrim(s) != "" {
			title = jsTrim(s)
		}
		t := jsObject{}
		t.set("ref", cardID)
		t.set("title", title)
		t.set("description", strings.Join(description, "\n\n"))
		t.set("column_name", column)
		t.set("swimlane_name", "Default")
		t.set("tags", tags)
		if hasDates {
			if to != "" {
				t.set("date_started", from)
				t.set("date_due", to)
			} else {
				t.set("date_due", from)
			}
		}
		c, ok, ierr := focalboardISO(jsGetOrUndef(card, "createAt"))
		if ierr != nil {
			return nil, ierr
		}
		if ok {
			t.set("date_creation", c)
		}
		if len(items) > 0 {
			cl := jsObject{}
			cl.set("title", "Checklist")
			cl.set("items", items)
			t.set("checklists", []any{cl})
		}
		if len(comments) > 0 {
			t.set("comments", comments)
		}
		if len(custom) > 0 {
			t.set("custom_fields", jsOrder(custom))
		}
		tasks = append(tasks, t)
		columnNames = append(columnNames, column)
	}
	// Lists in the group-by property's own option order, then any others.
	var candidates []any
	if groupBy != -1 {
		for _, item := range optionsOf(properties[groupBy]) {
			if item == nil {
				panic(errFocalboardType)
			}
			candidates = append(candidates, jsGetOrUndef(item, "value"))
		}
	}
	candidates = append(candidates, columnNames...)
	same := func(a, b any) bool {
		_, ua := a.(undefinedValue)
		_, ub := b.(undefinedValue)
		if ua || ub {
			return ua && ub
		}
		return jsStrictEq(a, b) || (a == nil && b == nil)
	}
	columns := []any{}
	var unique []any
	for _, c := range candidates {
		dup := false
		for _, u := range unique {
			dup = dup || same(u, c)
		}
		if !dup {
			unique = append(unique, c)
		}
	}
	for _, c := range unique {
		used := groupBy != -1
		for _, n := range columnNames {
			used = used || same(n, c)
		}
		if used {
			o := jsObject{}
			o.set("title", c)
			columns = append(columns, o)
		}
	}
	b := jsObject{}
	name := "Imported Focalboard"
	if s, ok := jsGet(board, "title").(string); ok && jsTrim(s) != "" {
		name = jsTrim(s)
	}
	b.set("name", name)
	if d, ok := jsGet(board, "description").(string); ok && d != "" {
		b.set("description", d)
	}
	out := importDoc(name, columns, tasks, []any{}, unsupported)
	out.set("board", b)
	return out, nil
}
