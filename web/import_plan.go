package web

import (
	"bytes"
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// What an import from another tool writes for one normalized task, as Node.js
// WeKan's models/lib/importedTaskPlan.js decides it - the same decisions, in
// the same shape, so testdata/import's golden (WeKan's own output) holds for
// Weddy too. The normalized task is what each tool's parser gives
// (externalParsers.js): title, description, column_name, swimlane_name,
// date_due, owner, tags, and the optional date_started, date_end,
// date_creation, archived, color, due_complete, spent_hours, assignees,
// checklists, comments, watchers, ref, parent_ref, dependencies and
// custom_fields.

// --- JSON as JavaScript reads it: objects keep their keys in order.

// decodeJS decodes JSON with every object as a jsObject in JavaScript's key
// order: integer-like keys ascending first, then the rest as written.
func decodeJS(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeJSValue(dec)
	if err != nil {
		return nil, err
	}
	return v, nil
}

func decodeJSValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := jsObject{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				v, err := decodeJSValue(dec)
				if err != nil {
					return nil, err
				}
				o.set(kt.(string), v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return jsOrder(o), nil
		case '[':
			a := []any{}
			for dec.More() {
				v, err := decodeJSValue(dec)
				if err != nil {
					return nil, err
				}
				a = append(a, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return a, nil
		}
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return nil, err
		}
		return f, nil
	}
	return tok, nil
}

var jsIndexKey = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

// jsOrder puts an object's array-index keys first, ascending, as JavaScript
// orders them.
func jsOrder(o jsObject) jsObject {
	var idx, rest jsObject
	for _, kv := range o {
		if jsIndexKey.MatchString(kv.Key) && len(kv.Key) < 10 {
			idx = append(idx, kv)
		} else {
			rest = append(rest, kv)
		}
	}
	sort.SliceStable(idx, func(i, j int) bool {
		a, _ := strconv.Atoi(idx[i].Key)
		b, _ := strconv.Atoi(idx[j].Key)
		return a < b
	})
	return append(idx, rest...)
}

// get is o[k]: nil when o is not an object or has no k.
func jsGet(o any, k string) any {
	obj, ok := o.(jsObject)
	if !ok {
		return nil
	}
	for _, kv := range obj {
		if kv.Key == k {
			return kv.Value
		}
	}
	return nil
}

// jsTruthy is JavaScript's Boolean(v).
func jsTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0 && !math.IsNaN(x)
	case string:
		return x != ""
	}
	return true
}

// jsStr is String(v) for the values JSON has (decodeJS).
func jsStr(v any) string {
	switch x := v.(type) {
	case undefinedValue:
		return "undefined"
	case nil:
		return "null"
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return jsNumber(x)
	case []any:
		parts := make([]string, len(x))
		for k, e := range x {
			if e != nil {
				parts[k] = jsStr(e)
			}
		}
		return strings.Join(parts, ",")
	}
	return "[object Object]"
}

// --- importedTaskPlan.js

var importDigits = regexp.MustCompile(`^\d+$`)

// jsDateFormats are the date strings JavaScript's Date reads that the tools'
// parsers give: ISO forms, a date alone at UTC, a time without a zone read at
// UTC (WeKan's server runs at UTC).
var jsDateFormats = []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02T15:04", "2006-01-02 15:04:05",
	"2006-01-02 15:04", "2006-01-02", "2006-01", "2006", time.RFC1123, time.RFC1123Z}

func jsParseDate(s string) (time.Time, bool) {
	for _, f := range jsDateFormats {
		if t, err := time.Parse(f, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// importedDate is WeKan's: unix seconds (or milliseconds when too large to be
// seconds) as a number or a digit string, else a date string; nothing for
// none, "0", or what does not parse.
func importedDate(v any) (time.Time, bool) {
	switch x := v.(type) {
	case nil, bool:
		return time.Time{}, false
	case jsDate:
		return time.Time(x), true
	case float64:
		return importedEpoch(x)
	case string:
		if x == "" {
			return time.Time{}, false
		}
		if importDigits.MatchString(x) {
			n, err := strconv.ParseFloat(x, 64)
			if err != nil {
				return time.Time{}, false
			}
			return importedEpoch(n)
		}
		return jsParseDate(x)
	}
	return time.Time{}, false
}

func importedEpoch(n float64) (time.Time, bool) {
	if math.IsNaN(n) || math.IsInf(n, 0) || n <= 0 {
		return time.Time{}, false
	}
	ms := n
	if n < 1e11 {
		ms = n * 1000
	}
	if ms > 8.64e15 {
		return time.Time{}, false
	}
	return time.UnixMilli(int64(ms)).UTC(), true
}

var importHex6 = regexp.MustCompile(`^#?([0-9a-fA-F]{6})$`)

// importedColor is WeKan's: a palette name, or '#rrggbb' (a hex without '#'
// too).
func importedColor(v any, allowed []string) string {
	s, ok := v.(string)
	if !ok || s == "" {
		return ""
	}
	if name := strings.ToLower(s); contains(allowed, name) {
		return name
	}
	if m := importHex6.FindStringSubmatch(s); m != nil {
		return "#" + strings.ToLower(m[1])
	}
	return ""
}

// importText is text(): a string, or a finite number as text, else ”.
func importText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		if !math.IsNaN(x) && !math.IsInf(x, 0) {
			return jsNumber(x)
		}
	}
	return ""
}

type importComment struct {
	Text      string  `json:"text"`
	UserID    *string `json:"userId"`
	CreatedAt *jsDate `json:"createdAt,omitempty"`
}

type importChecklistItem struct {
	Title      string `json:"title"`
	IsFinished bool   `json:"isFinished"`
	Sort       int    `json:"sort"`
}

type importChecklist struct {
	Title string                `json:"title"`
	Sort  int                   `json:"sort"`
	Items []importChecklistItem `json:"items"`
}

func importedComment(c any, members map[string]string) *importComment {
	body := jsTrim(importText(jsGet(c, "text")))
	if body == "" {
		return nil
	}
	author := jsTrim(importText(jsGet(c, "author")))
	shown := jsTrim(importText(jsGet(c, "authorName")))
	if shown == "" {
		shown = author
	}
	out := &importComment{Text: body}
	if id := members[author]; author != "" && id != "" {
		out.UserID = &id
	} else if shown != "" {
		out.Text = shown + ": " + body
	}
	if t, ok := importedDate(jsGet(c, "date")); ok {
		d := jsDate(t)
		out.CreatedAt = &d
	}
	return out
}

func importedChecklists(v any) []importChecklist {
	list, _ := v.([]any)
	out := []importChecklist{}
	for n, c := range list {
		title := jsTrim(importText(jsGet(c, "title")))
		if title == "" {
			title = "Checklist"
		}
		items := []importChecklistItem{}
		src, _ := jsGet(c, "items").([]any)
		for _, it := range src {
			if t := jsTrim(importText(jsGet(it, "title"))); t != "" {
				items = append(items, importChecklistItem{Title: t, IsFinished: jsTruthy(jsGet(it, "done")), Sort: len(items)})
			}
		}
		if len(items) > 0 {
			out = append(out, importChecklist{Title: title, Sort: n, Items: items})
		}
	}
	return out
}

type importPlan struct {
	Card           jsObject          `json:"card"`
	MemberIDs      []string          `json:"memberIds"`
	WatcherIDs     []string          `json:"watcherIds"`
	UnwatchedCount int               `json:"unwatchedCount"`
	Checklists     []importChecklist `json:"checklists"`
	Comments       []importComment   `json:"comments"`
}

// importKey is a source user key: a non-empty string or a finite number.
func importKey(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, x != ""
	case float64:
		if !math.IsNaN(x) && !math.IsInf(x, 0) {
			return jsNumber(x), true
		}
	}
	return "", false
}

// planImportedTask is WeKan's: the card, its members and watchers by the
// members mapping (a watcher only when one of the board's members), its
// checklists and comments.
func planImportedTask(task any, members map[string]string, allowed []string, boardMemberIDs []string) importPlan {
	card := jsObject{}
	title := importText(jsGet(task, "title"))
	if title == "" {
		title = "Imported task"
	}
	card.set("title", title)
	card.set("description", importText(jsGet(task, "description")))
	card.set("archived", jsGet(task, "archived") == true)
	if v := jsGet(task, "requested_by"); jsTruthy(v) {
		card.set("requestedBy", jsStr(v))
	}
	if v := jsGet(task, "assigned_by"); jsTruthy(v) {
		card.set("assignedBy", jsStr(v))
	}
	hasDue := false
	for _, f := range [][2]string{{"dueAt", "date_due"}, {"startAt", "date_started"}, {"endAt", "date_end"}, {"createdAt", "date_creation"}} {
		if t, ok := importedDate(jsGet(task, f[1])); ok {
			card.set(f[0], jsDate(t))
			hasDue = hasDue || f[0] == "dueAt"
		}
	}
	if c := importedColor(jsGet(task, "color"), allowed); c != "" {
		card.set("color", c)
	}
	if jsGet(task, "due_complete") == true && hasDue {
		card.set("dueComplete", true)
	}
	if v := jsGet(task, "spent_hours"); v != nil {
		if n, ok := jsToNumber(v); ok && n > 0 && !math.IsInf(n, 0) {
			card.set("spentTime", n)
		}
	}
	owner := jsGet(task, "owner_id")
	if !jsTruthy(owner) {
		owner = jsGet(task, "owner_username")
	}
	if !jsTruthy(owner) {
		owner = jsGet(task, "owner_name")
	}
	keys := []any{owner}
	if a, ok := jsGet(task, "assignees").([]any); ok {
		keys = append(keys, a...)
	}
	memberIDs := []string{}
	for _, k := range keys {
		if key, ok := importKey(k); ok {
			if id := members[key]; id != "" && !contains(memberIDs, id) {
				memberIDs = append(memberIDs, id)
			}
		}
	}
	var watcherKeys []string
	if a, ok := jsGet(task, "watchers").([]any); ok {
		for _, k := range a {
			if key, ok := importKey(k); ok {
				watcherKeys = append(watcherKeys, key)
			}
		}
	}
	watcherIDs := []string{}
	for _, k := range watcherKeys {
		if id := members[k]; id != "" && contains(boardMemberIDs, id) && !contains(watcherIDs, id) {
			watcherIDs = append(watcherIDs, id)
		}
	}
	watched := 0
	for _, k := range watcherKeys {
		if id := members[k]; id != "" && contains(watcherIDs, id) {
			watched++
		}
	}
	comments := []importComment{}
	if a, ok := jsGet(task, "comments").([]any); ok {
		for _, c := range a {
			if ic := importedComment(c, members); ic != nil {
				comments = append(comments, *ic)
			}
		}
	}
	return importPlan{Card: card, MemberIDs: memberIDs, WatcherIDs: watcherIDs, UnwatchedCount: len(watcherKeys) - watched,
		Checklists: importedChecklists(jsGet(task, "checklists")), Comments: comments}
}

// jsToNumber is Number(v) for JSON's values; false when it is NaN.
func jsToNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, !math.IsNaN(x)
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case nil:
		return 0, true
	case string:
		s := jsTrim(x)
		switch {
		case s == "":
			return 0, true
		case s == "Infinity" || s == "+Infinity":
			return math.Inf(1), true
		case s == "-Infinity":
			return math.Inf(-1), true
		case jsDecimal.MatchString(s):
			n, err := strconv.ParseFloat(s, 64)
			return n, err == nil
		case jsRadix.MatchString(s):
			n, err := strconv.ParseUint(s[2:], map[byte]int{'x': 16, 'X': 16, 'o': 8, 'O': 8, 'b': 2, 'B': 2}[s[1]], 64)
			return float64(n), err == nil
		}
	}
	return math.NaN(), false
}

var (
	jsDecimal = regexp.MustCompile(`^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$`)
	jsRadix   = regexp.MustCompile(`^0([xX][0-9a-fA-F]+|[oO][0-7]+|[bB][01]+)$`)
)

type importLabel struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

// planImportedLabels is WeKan's: a board label per tag name, its first usable
// color, else black.
func planImportedLabels(tasks []any, allowed []string) []importLabel {
	out := []importLabel{}
	seen := map[string]bool{}
	for _, t := range tasks {
		tags, _ := jsGet(t, "tags").([]any)
		for _, tag := range tags {
			name := ""
			switch x := tag.(type) {
			case string:
				name = x
			case jsObject:
				name, _ = jsGet(x, "name").(string)
			}
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			color := importedColor(jsGet(tag, "color"), allowed)
			if color == "" {
				color = "black"
			}
			out = append(out, importLabel{Name: name, Color: color})
		}
	}
	return out
}

const (
	maxImportedCustomFields  = 50
	maxCustomFieldNameRunes  = 100
	maxCustomFieldTextLength = 10000
)

var importDependencyTypes = []string{"related-to", "blocks", "is-blocked-by", "fixes", "is-fixed-by", "duplicates", "is-duplicated-by"}

// customFieldValue is WeKan's: a number, a boolean, or text; nil for none.
func customFieldValue(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		if x == "" {
			return nil
		}
		return jsSlice16(x, maxCustomFieldTextLength)
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil
		}
		return x
	case bool:
		return x
	case []any:
		var parts []string
		for _, e := range x {
			if cv := customFieldValue(e); cv != nil {
				parts = append(parts, jsStr(cv))
			}
		}
		if len(parts) == 0 {
			return nil
		}
		return jsSlice16(strings.Join(parts, ", "), maxCustomFieldTextLength)
	case jsObject:
		for _, k := range []string{"name", "title", "value", "display_value"} {
			if n := jsGet(x, k); jsTruthy(n) {
				if _, isObj := n.(jsObject); isObj {
					return nil
				}
				if _, isArr := n.([]any); isArr {
					return nil
				}
				return customFieldValue(n)
			}
		}
		return nil
	}
	return nil
}

type importUnsupported struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type importField struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type importFieldsPlan struct {
	Fields      []importField       `json:"fields"`
	Unsupported []importUnsupported `json:"unsupported"`
}

func fieldName(raw string) string { return jsSlice16(jsTrim(raw), maxCustomFieldNameRunes) }

// planImportedCustomFields is WeKan's: a custom field per source field name,
// typed from the values present - all numbers number, all booleans checkbox,
// else text.
func planImportedCustomFields(tasks []any) importFieldsPlan {
	var names []string
	values := map[string][]any{}
	unsupported := []importUnsupported{}
	for _, t := range tasks {
		fields, ok := jsGet(t, "custom_fields").(jsObject)
		if !ok {
			continue
		}
		for _, kv := range fields {
			name := fieldName(kv.Key)
			value := customFieldValue(kv.Value)
			if name == "" || value == nil {
				continue
			}
			if _, ok := values[name]; !ok {
				if len(names) >= maxImportedCustomFields {
					if len(unsupported) == 0 {
						unsupported = append(unsupported, importUnsupported{Path: "/custom_fields",
							Reason: "more than " + strconv.Itoa(maxImportedCustomFields) + " custom fields; the rest are not imported"})
					}
					continue
				}
				names = append(names, name)
				values[name] = nil
			}
			values[name] = append(values[name], value)
		}
	}
	out := importFieldsPlan{Fields: []importField{}, Unsupported: unsupported}
	for _, n := range names {
		allNum, allBool := true, true
		for _, v := range values[n] {
			_, isNum := v.(float64)
			_, isBool := v.(bool)
			allNum, allBool = allNum && isNum, allBool && isBool
		}
		typ := "text"
		if allNum {
			typ = "number"
		} else if allBool {
			typ = "checkbox"
		}
		out.Fields = append(out.Fields, importField{Name: n, Type: typ})
	}
	return out
}

type importFieldValue struct {
	Name  string `json:"name"`
	Value any    `json:"value"`
}

// importedCustomFieldValues is WeKan's: the values a card stores for its
// task, given the planned fields.
func importedCustomFieldValues(task any, fields []importField) []importFieldValue {
	src, ok := jsGet(task, "custom_fields").(jsObject)
	out := []importFieldValue{}
	if !ok {
		return out
	}
	byName := map[string]importField{}
	for _, f := range fields {
		byName[f.Name] = f
	}
	for _, kv := range src {
		f, ok := byName[fieldName(kv.Key)]
		value := customFieldValue(kv.Value)
		if !ok || value == nil {
			continue
		}
		if f.Type == "text" {
			value = jsStr(value)
		}
		out = append(out, importFieldValue{Name: f.Name, Value: value})
	}
	return out
}

type importParentLink struct {
	Index  int `json:"index"`
	Parent int `json:"parent"`
}

type importDep struct {
	Target int    `json:"target"`
	Type   string `json:"type"`
}

type importDepLink struct {
	Index int         `json:"index"`
	Deps  []importDep `json:"deps"`
}

type importLinksPlan struct {
	Parents      []importParentLink  `json:"parents"`
	Dependencies []importDepLink     `json:"dependencies"`
	Unsupported  []importUnsupported `json:"unsupported"`
}

// importRef is a ref as String(ref), and false for none (undefined, null, ”).
func importRef(v any) (string, bool) {
	if v == nil {
		return "", false
	}
	if s, ok := v.(string); ok && s == "" {
		return "", false
	}
	return jsStr(v), true
}

// planImportedLinks is WeKan's: parent and dependency links between the
// import's own tasks, as task indexes; anything else reported.
func planImportedLinks(tasks []any) importLinksPlan {
	byRef := map[string]int{}
	out := importLinksPlan{Parents: []importParentLink{}, Dependencies: []importDepLink{}, Unsupported: []importUnsupported{}}
	for n, t := range tasks {
		ref, ok := importRef(jsGet(t, "ref"))
		if !ok {
			continue
		}
		if _, dup := byRef[ref]; dup {
			out.Unsupported = append(out.Unsupported, importUnsupported{Path: "/tasks/" + strconv.Itoa(n) + "/ref",
				Reason: "duplicate source id; links resolve to the first item"})
			continue
		}
		byRef[ref] = n
	}
	cycle := func(index, parent int) bool {
		seen := map[int]bool{index: true}
		current, ok := parent, true
		for ok {
			if seen[current] {
				return true
			}
			seen[current] = true
			ref, has := importRef(jsGet(tasks[current], "parent_ref"))
			if !has {
				return false
			}
			current, ok = byRef[ref]
		}
		return false
	}
	for n, t := range tasks {
		if t == nil {
			continue
		}
		if pref, ok := importRef(jsGet(t, "parent_ref")); ok {
			path := "/tasks/" + strconv.Itoa(n) + "/parent_ref"
			if parent, found := byRef[pref]; !found {
				out.Unsupported = append(out.Unsupported, importUnsupported{Path: path, Reason: "parent is not part of this import"})
			} else if parent == n || cycle(n, parent) {
				out.Unsupported = append(out.Unsupported, importUnsupported{Path: path, Reason: "parent link would form a cycle"})
			} else {
				out.Parents = append(out.Parents, importParentLink{Index: n, Parent: parent})
			}
		}
		var deps []importDep
		list, _ := jsGet(t, "dependencies").([]any)
		for _, d := range list {
			target, found := -1, false
			if d != nil {
				target, found = byRef[jsStr(jsGet(d, "ref"))]
				if jsGet(d, "ref") == nil {
					target, found = byRef["undefined"]
				}
			}
			if !found {
				out.Unsupported = append(out.Unsupported, importUnsupported{Path: "/tasks/" + strconv.Itoa(n) + "/dependencies",
					Reason: "linked item is not part of this import"})
				continue
			}
			dup := target == n
			for _, x := range deps {
				dup = dup || x.Target == target
			}
			if dup {
				continue
			}
			typ, _ := jsGet(d, "type").(string)
			if !contains(importDependencyTypes, typ) {
				typ = "related-to"
			}
			deps = append(deps, importDep{Target: target, Type: typ})
		}
		if len(deps) > 0 {
			out.Dependencies = append(out.Dependencies, importDepLink{Index: n, Deps: deps})
		}
	}
	return out
}
