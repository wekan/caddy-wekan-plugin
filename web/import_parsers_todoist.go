package web

import (
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Todoist's project CSV: Node.js WeKan's readCsv and parseTodoistCsv
// (models/lib/todoistCsvFormat.js) - sections as lists, tasks as cards with
// their @labels, priority, owner and dates, deeper tasks as the card's
// Sub-tasks checklist, notes as its comments. readCsv is the CSV reader
// WeKan's other CSV imports share.

func init() { importTextParsers["todoist"] = parseTodoistCsv }

const maxTodoistRows = 20000

var (
	todoistLabel    = jsRegexp(`(^|\s)@([^\s@]+)`)
	todoistISODay   = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})(?:[ T](\d{2}):(\d{2}))?$`)
	todoistUTC      = regexp.MustCompile(`(?i)^(UTC|GMT|Etc/UTC)$`)
	todoistPersonID = jsRegexp(`\s*\(\d+\)\s*$`)
	jsIntPrefix     = regexp.MustCompile(`^[+-]?\d+`)
)

// jsReadCsv is WeKan's readCsv: RFC 4180 fields, a quote opening only an
// empty field, rows of nothing but white space left out.
func jsReadCsv(text, name string, sep rune) ([][]string, error) {
	source := strings.TrimPrefix(text, "\ufeff")
	var rows [][]string
	var row []string
	var field strings.Builder
	quoted := false
	tooMany := errors.New(name + " CSV has more than " + strconv.Itoa(maxTodoistRows) + " rows")
	for i := 0; i < len(source); {
		ch, size := utf8.DecodeRuneInString(source[i:])
		next := byte(0)
		if i+size < len(source) {
			next = source[i+size]
		}
		i += size
		if quoted {
			switch {
			case ch == '"' && next == '"':
				field.WriteByte('"')
				i++
			case ch == '"':
				quoted = false
			default:
				field.WriteRune(ch)
			}
			continue
		}
		switch {
		case ch == '"' && field.Len() == 0:
			quoted = true
		case ch == sep:
			row = append(row, field.String())
			field.Reset()
		case ch == '\n' || ch == '\r':
			if ch == '\r' && next == '\n' {
				i++
			}
			rows = append(rows, append(row, field.String()))
			row, field = nil, strings.Builder{}
		default:
			field.WriteRune(ch)
		}
		if len(rows) > maxTodoistRows {
			return nil, tooMany
		}
	}
	if quoted {
		return nil, errors.New(name + " CSV has an unclosed quote")
	}
	if field.Len() > 0 || len(row) > 0 {
		rows = append(rows, append(row, field.String()))
	}
	out := [][]string{}
	for _, cells := range rows {
		for _, c := range cells {
			if jsTrim(c) != "" {
				out = append(out, cells)
				break
			}
		}
	}
	return out, nil
}

// jsParseInt is Number.parseInt(s, 10): the leading whole number after white
// space, or false (NaN).
func jsParseInt(s string) (int, bool) {
	m := jsIntPrefix.FindString(strings.TrimLeftFunc(s, jsSpace))
	if m == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(m, 64)
	if err != nil || math.IsInf(f, 0) {
		return 0, false
	}
	return int(f), true
}

// jsDateUTC is new Date(Date.UTC(y, m-1, d, h, min)).toISOString(), with
// false when the month rolled over - the check WeKan's date readers make.
func jsDateUTC(y, m, d, h, min int) (string, bool) {
	if y >= 0 && y < 100 {
		y += 1900
	}
	t := time.Date(y, time.Month(m), d, h, min, 0, 0, time.UTC)
	if int(t.Month()) != m {
		return "", false
	}
	return t.Format("2006-01-02T15:04:05.000Z"), true
}

func todoistReadDate(value, timezone string) (string, bool) {
	m := todoistISODay.FindStringSubmatch(jsTrim(value))
	if m == nil {
		return "", false
	}
	if m[4] != "" && timezone != "" && !todoistUTC.MatchString(jsTrim(timezone)) {
		return "", false
	}
	n := func(s string) int { v, _ := strconv.Atoi(s); return v }
	return jsDateUTC(n(m[1]), n(m[2]), n(m[3]), n(m[4]), n(m[5]))
}

// todoistPerson is "Name (123)" as its name; "" when there is none.
func todoistPerson(value string) string {
	return jsTrim(todoistPersonID.ReplaceAllString(jsTrim(value), ""))
}

// jsUpper and jsLower are toUpperCase and toLowerCase. Go's lower case of
// U+0130 (İ) is a plain i, JavaScript's an i with a combining dot above, so a
// header like "Prİority" is not taken for "priority" as WeKan would not.
func jsUpper(s string) string { return strings.ToUpper(s) }
func jsLower(s string) string { return strings.ToLower(strings.ReplaceAll(s, "\u0130", "i\u0307")) }

// csvHeader is a CSV's first row as names to look up.
type csvHeader []string

func (h csvHeader) col(name string) int {
	for i, c := range h {
		if c == name {
			return i
		}
	}
	return -1
}

func (h csvHeader) cell(cells []string, name string) string {
	i := h.col(name)
	if i == -1 || i >= len(cells) {
		return ""
	}
	return jsTrim(cells[i])
}

// pullTags takes each re match out of text, its group 1 kept, and returns
// group 2 of each.
func pullTags(re *regexp.Regexp, text string) (string, []any) {
	tags := []any{}
	var b strings.Builder
	last := 0
	for _, idx := range re.FindAllStringSubmatchIndex(text, -1) {
		b.WriteString(text[last:idx[0]])
		b.WriteString(text[idx[2]:idx[3]])
		tags = append(tags, text[idx[4]:idx[5]])
		last = idx[1]
	}
	b.WriteString(text[last:])
	return b.String(), tags
}

func parseTodoistCsv(text string) (jsObject, error) {
	rows, err := jsReadCsv(text, "Todoist", ',')
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, errors.New("Todoist CSV is empty")
	}
	header := csvHeader{}
	for _, c := range rows[0] {
		header = append(header, jsUpper(jsTrim(c)))
	}
	if header.col("TYPE") == -1 || header.col("CONTENT") == -1 {
		return nil, errors.New("Todoist CSV needs the TYPE and CONTENT columns")
	}
	var columns []string
	addColumn := func(name string) {
		for _, c := range columns {
			if c == name {
				return
			}
		}
		columns = append(columns, name)
	}
	tasks, unsupported := []any{}, []any{}
	list := ""
	type open struct {
		task     jsObject
		comments []any
		items    []any
	}
	var cards []*open
	var current *open
	for index, cells := range rows[1:] {
		at := "/row/" + strconv.Itoa(index+2)
		typ := jsLower(header.cell(cells, "TYPE"))
		content := header.cell(cells, "CONTENT")
		switch typ {
		case "meta":
			continue
		case "section":
			list = content
			if list == "" {
				list = "No section"
			}
			addColumn(list)
			current = nil
			continue
		case "note":
			if current == nil {
				unsupported = append(unsupported, importUnsupportedObj(at, "a note with no task above it is not imported"))
				continue
			}
			if content != "" {
				c := jsObject{}
				c.set("text", content)
				if a := todoistPerson(header.cell(cells, "AUTHOR")); a != "" {
					c.set("author", a)
				}
				current.comments = append(current.comments, c)
			}
			continue
		case "task":
		default:
			name := typ
			if name == "" {
				name = "(empty)"
			}
			unsupported = append(unsupported, importUnsupportedObj(at, "Todoist row type "+name+" is not imported"))
			continue
		}
		title, labels := pullTags(todoistLabel, content)
		title = jsTrim(jsCollapse(title, " "))
		if title == "" {
			title = "Imported task"
		}
		indent, ok := jsParseInt(header.cell(cells, "INDENT"))
		if !ok || indent == 0 {
			indent = 1
		}
		if indent > 1 {
			if current == nil {
				unsupported = append(unsupported, importUnsupportedObj(at, "a sub-task with no task above it is not imported"))
				continue
			}
			item := jsObject{}
			item.set("title", title)
			item.set("done", false)
			current.items = append(current.items, item)
			continue
		}
		if list == "" {
			list = "No section"
			addColumn(list)
		}
		if p, ok := jsParseInt(header.cell(cells, "PRIORITY")); ok && p >= 1 && p <= 3 {
			labels = append([]any{"p" + strconv.Itoa(p)}, labels...)
		}
		timezone := header.cell(cells, "TIMEZONE")
		dateText, deadlineText := header.cell(cells, "DATE"), header.cell(cells, "DEADLINE")
		date, hasDate := todoistReadDate(dateText, timezone)
		deadline, hasDeadline := todoistReadDate(deadlineText, timezone)
		if dateText != "" && !hasDate {
			unsupported = append(unsupported, importUnsupportedObj(at+"/DATE", `Todoist date "`+dateText+`" is not a calendar date`))
		}
		if deadlineText != "" && !hasDeadline {
			unsupported = append(unsupported, importUnsupportedObj(at+"/DEADLINE", `Todoist deadline "`+deadlineText+`" is not a calendar date`))
		}
		if header.cell(cells, "DURATION") != "" {
			unsupported = append(unsupported, importUnsupportedObj(at+"/DURATION", "Todoist duration has no WeKan field"))
		}
		t := jsObject{}
		t.set("title", title)
		t.set("description", header.cell(cells, "DESCRIPTION"))
		t.set("column_name", list)
		t.set("swimlane_name", "Default")
		t.set("tags", labels)
		if owner := todoistPerson(header.cell(cells, "RESPONSIBLE")); owner != "" {
			t.set("owner_username", owner)
		}
		switch {
		case hasDeadline:
			t.set("date_due", deadline)
			if hasDate {
				t.set("date_started", date)
			}
		case hasDate:
			t.set("date_due", date)
		}
		current = &open{task: t}
		cards = append(cards, current)
	}
	for _, c := range cards {
		if len(c.comments) > 0 {
			c.task.set("comments", c.comments)
		}
		if len(c.items) > 0 {
			cl := jsObject{}
			cl.set("title", "Sub-tasks")
			cl.set("items", c.items)
			c.task.set("checklists", []any{cl})
		}
		tasks = append(tasks, c.task)
	}
	cols := []any{}
	for _, c := range columns {
		o := jsObject{}
		o.set("title", c)
		cols = append(cols, o)
	}
	return importDoc("Imported Todoist", cols, tasks, []any{}, unsupported), nil
}
