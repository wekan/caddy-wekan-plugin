package web

import (
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Obsidian Kanban plugin boards: Node.js WeKan's parseObsidianKanban
// (models/lib/obsidianKanbanFormat.js), line for line - lanes with their
// card limits, a **Complete** lane, cards with their tags, dates in the
// board's date format, the Tasks plugin's dates, priorities, ids and
// dependencies, Dataview fields, a body's checklist, and the archive.

func init() { importTextParsers["obsidian"] = parseObsidianKanban }

// jsDot is JavaScript's "." without the u flag: one UTF-16 unit that is not a
// line terminator - so never a character outside the Basic Multilingual Plane.
const jsDot = `[^\n\r\x{2028}\x{2029}\x{10000}-\x{10FFFF}]`

// jsDotU is "." with the u flag, or where only line terminators matter.
const jsDotU = `[^\n\r\x{2028}\x{2029}]`

var (
	obsidianPriority = []struct{ Emoji, Name string }{
		{"🔺", "Highest"}, {"⏫", "High"}, {"🔼", "Medium"}, {"🔽", "Low"}, {"⏬", "Lowest"},
	}
	obsidianTaskDates = []struct{ Emoji, Field string }{
		{"📅", "date_due"}, {"🛫", "date_started"}, {"➕", "date_creation"}, {"✅", "date_end"}, {"⏳", "scheduled"}, {"❌", "cancelled"},
	}
	obsidianBoardKinds = map[string]bool{"board": true, "basic": true, "table": true, "list": true}
	obsidianLane       = jsRegexp(`^##\s+(` + jsDotU + `*?)\s*$`)
	obsidianLaneLimit  = jsRegexp(`^(` + jsDotU + `*?)\s*\((\d+)\)$`)
	obsidianCard       = jsRegexp(`^[-*+]\s+\[(` + jsDot + `)\]\s?(` + jsDotU + `*)$`)
	obsidianFrontLine  = jsRegexp(`^([\w-]+):\s*(` + jsDotU + `*?)\s*$`)
	obsidianQuotes     = regexp.MustCompile(`^["']|["']$`)
	obsidianSettingEnd = jsRegexp(`%%\s*$`)
	obsidianBody       = regexp.MustCompile(`^( {4}|\t)`)
	obsidianBr         = jsRegexp(`(?i)<br\s*/?>`)
	obsidianRepeat     = jsRegexp(`🔁\x{FE0F}?\s*([^📅🛫➕✅⏳❌🆔⛔#]*)`)
	obsidianID         = jsRegexp(`🆔\x{FE0F}?\s*([\w-]+)`)
	obsidianDepends    = jsRegexp(`⛔\x{FE0F}?\s*([\w-]+(?:\s*,\s*[\w-]+)*)`)
	obsidianBlock      = jsRegexp(`\s\^([\w-]+)\s*$`)
	obsidianTag        = jsRegexp(`(^|\s)#([^\s#]+)`)
	obsidianField      = jsRegexp(`^\s*([\w][\w -]*?)::\s*(` + jsDotU + `+?)\s*$`)
	obsidianItem       = jsRegexp(`^[-*+]\s+\[(` + jsDot + `)\]\s+(` + jsDotU + `+?)\s*$`)
	obsidianTimeShape  = regexp.MustCompile(`^(\d{1,2}):(\d{2})$`)
	obsidianDateToken  = regexp.MustCompile(`YYYY|MM|M|DD|D`)
	obsidianSettingKey = regexp.MustCompile(`^(append-archive-date|archive-date-format|archive-date-separator|archive-with-date|date-colors|date-display-format|date-format|date-picker-week-start|date-trigger|link-date-to-daily-note|max-archive-size|move-dates|show-relative-date|time-format|time-trigger|full-list-lane-width|hide-card-count|lane-width|list-collapse|table-sizing|inline-metadata-position|metadata-keys|move-tags|move-task-metadata|tag-action|tag-colors|tag-sort|new-card-insertion-method|new-line-trigger|new-note-folder|new-note-template|show-add-list|show-archive-all|show-board-settings|show-checkboxes|show-search|show-set-view|show-view-as-markdown)$`)
	errObsidianOption  = errors.New("text.replace is not a function")
)

// jsEscapeRegExp is the module's escapeRegExp: its characters, as a pattern.
func jsEscapeRegExp(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`.*+?^${}()|[]\`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// jsReplaceFirst is String.prototype.replace with a string pattern.
func jsReplaceFirst(s, old, rep string) string { return strings.Replace(s, old, rep, 1) }

// obsidianDate is WeKan's: a date in the board's date-format, as ISO, or
// false when it does not match or is no real day.
func obsidianDate(text, format, clock string) (string, bool) {
	var tokens []string
	pattern := obsidianDateToken.ReplaceAllStringFunc(jsEscapeRegExp(format), func(t string) string {
		tokens = append(tokens, t)
		switch {
		case t == "YYYY":
			return `(\d{4})`
		case len(t) == 2:
			return `(\d{2})`
		}
		return `(\d{1,2})`
	})
	re, err := regexp.Compile("^" + pattern + "$")
	if err != nil {
		return "", false
	}
	m := re.FindStringSubmatch(jsTrim(text))
	has := func(letter byte) int {
		for i, t := range tokens {
			if t[0] == letter {
				return i
			}
		}
		return -1
	}
	if m == nil || has('Y') == -1 || has('M') == -1 || has('D') == -1 {
		return "", false
	}
	part := func(letter byte) int { n, _ := strconv.Atoi(m[has(letter)+1]); return n }
	hours, minutes := 0, 0
	if c := obsidianTimeShape.FindStringSubmatch(clock); c != nil {
		hours, _ = strconv.Atoi(c[1])
		minutes, _ = strconv.Atoi(c[2])
	}
	year := part('Y')
	if year < 100 { // Date.UTC's two-digit years
		year += 1900
	}
	d := time.Date(year, time.Month(part('M')), part('D'), hours, minutes, 0, 0, time.UTC)
	if int(d.Month()) != part('M') || d.Day() != part('D') {
		return "", false
	}
	return d.Format("2006-01-02T15:04:05.000Z"), true
}

type obsidianOptions struct {
	format, trigger, timeTrigger any
}

func (o *jsObject) del(k string) {
	for n := range *o {
		if (*o)[n].Key == k {
			*o = append((*o)[:n], (*o)[n+1:]...)
			return
		}
	}
}

func obsidianParseCard(first string, body []string, opt obsidianOptions, at string, unsupported *[]any) (jsObject, error) {
	custom, card := jsObject{}, jsObject{}
	tags := []any{}
	parts := obsidianBr.Split(first, -1)
	text, titleRest := parts[0], parts[1:]
	trigger, ok1 := opt.trigger.(string)
	timeTrigger, ok2 := opt.timeTrigger.(string)
	if !ok1 || !ok2 {
		return nil, errObsidianOption
	}
	dateRe := jsRegexp(jsEscapeRegExp(trigger) + `(?:\{([^}]*)\}|\[\[([^\]]*)\]\])`)
	timeRe := jsRegexp(jsEscapeRegExp(timeTrigger) + `\{([^}]*)\}`)
	timeM := timeRe.FindStringSubmatch(text)
	if timeM != nil {
		text = jsReplaceFirst(text, timeM[0], " ")
	}
	if idx := dateRe.FindStringSubmatchIndex(text); idx != nil {
		whole := text[idx[0]:idx[1]]
		value := ""
		if idx[2] >= 0 {
			value = text[idx[2]:idx[3]]
		} else if idx[4] >= 0 {
			value = text[idx[4]:idx[5]]
		}
		text = jsReplaceFirst(text, whole, " ")
		format, ok := opt.format.(string)
		if !ok {
			return nil, errObsidianOption
		}
		clock := ""
		if timeM != nil {
			clock = timeM[1]
		}
		if iso, ok := obsidianDate(value, format, clock); ok {
			card.set("date_due", iso)
		} else {
			*unsupported = append(*unsupported, importUnsupportedObj(at, `date "`+value+`" does not match the board's date format `+format))
		}
	}
	for _, td := range obsidianTaskDates {
		m := jsRegexp(regexp.QuoteMeta(td.Emoji) + `\x{FE0F}?\s*(\d{4}-\d{2}-\d{2})`).FindStringSubmatch(text)
		if m == nil {
			continue
		}
		text = jsReplaceFirst(text, m[0], " ")
		iso, ok := obsidianDate(m[1], "YYYY-MM-DD", "")
		switch {
		case td.Field == "cancelled":
			*unsupported = append(*unsupported, importUnsupportedObj(at, "a cancelled date (❌) has no WeKan place"))
		case td.Field == "scheduled":
			if ok {
				card.set("scheduled", iso)
			}
		default:
			if _, has := jsLookup(card, td.Field); ok && !has {
				card.set(td.Field, iso)
			}
		}
	}
	if s, has := jsLookup(card, "scheduled"); has {
		if _, started := jsLookup(card, "date_started"); !started {
			card.set("date_started", s)
		}
		card.del("scheduled")
	}
	for _, p := range obsidianPriority {
		if strings.Contains(text, p.Emoji) {
			custom.set("Priority", p.Name)
			text = strings.ReplaceAll(text, p.Emoji, " ")
		}
	}
	if m := obsidianRepeat.FindStringSubmatch(text); m != nil {
		text = jsReplaceFirst(text, m[0], " ")
		*unsupported = append(*unsupported, importUnsupportedObj(at, `recurrence "🔁 `+jsTrim(m[1])+`" is imported once`))
	}
	if m := obsidianID.FindStringSubmatch(text); m != nil {
		card.set("ref", m[1])
		text = jsReplaceFirst(text, m[0], " ")
	}
	if m := obsidianDepends.FindStringSubmatch(text); m != nil {
		deps := []any{}
		for _, ref := range strings.Split(m[1], ",") {
			d := jsObject{}
			d.set("ref", jsTrim(ref))
			d.set("type", "is-blocked-by")
			deps = append(deps, d)
		}
		card.set("dependencies", deps)
		text = jsReplaceFirst(text, m[0], " ")
	}
	if idx := obsidianBlock.FindStringSubmatchIndex(text); idx != nil {
		if _, has := jsLookup(card, "ref"); !has {
			card.set("ref", text[idx[2]:idx[3]])
		}
		text = text[:idx[0]]
	}
	// text.replace(/(^|\s)#([^\s#]+)/g, ...): each tag out, its space kept.
	var b strings.Builder
	last := 0
	for _, idx := range obsidianTag.FindAllStringSubmatchIndex(text, -1) {
		b.WriteString(text[last:idx[0]])
		b.WriteString(text[idx[2]:idx[3]])
		tags = append(tags, text[idx[4]:idx[5]])
		last = idx[1]
	}
	b.WriteString(text[last:])
	text = b.String()
	description := strings.Join(append(append([]string{}, titleRest...), body...), "\n")
	var kept []string
	items := []any{}
	for _, line := range strings.Split(description, "\n") {
		if m := obsidianField.FindStringSubmatch(line); m != nil {
			custom.set(jsTrim(m[1]), m[2])
			continue
		}
		if m := obsidianItem.FindStringSubmatch(line); m != nil {
			item := jsObject{}
			item.set("title", m[2])
			item.set("done", m[1] != " ")
			items = append(items, item)
			continue
		}
		kept = append(kept, line)
	}
	title := jsTrim(jsCollapse(text, " "))
	if title == "" {
		title = "Untitled"
	}
	task := jsObject{}
	task.set("title", title)
	task.set("description", jsTrim(strings.Join(kept, "\n")))
	task.set("tags", tags)
	for _, kv := range card {
		task.set(kv.Key, kv.Value)
	}
	if len(custom) > 0 {
		task.set("custom_fields", jsOrder(custom))
	}
	if len(items) > 0 {
		cl := jsObject{}
		cl.set("title", "Checklist")
		cl.set("items", items)
		task.set("checklists", []any{cl})
	}
	return task, nil
}

func parseObsidianKanban(text string) (jsObject, error) {
	lines := jsLines(strings.TrimPrefix(text, "\ufeff"))
	unsupported := []any{}
	// The frontmatter.
	if jsTrim(lines[0]) != "---" {
		return nil, errors.New("An Obsidian Kanban board starts with its --- frontmatter")
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if jsTrim(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end == -1 {
		return nil, errors.New("The Obsidian Kanban frontmatter has no closing ---")
	}
	values := jsObject{}
	for _, line := range lines[1:end] {
		if m := obsidianFrontLine.FindStringSubmatch(line); m != nil {
			values.set(m[1], obsidianQuotes.ReplaceAllString(m[2], ""))
		}
	}
	values = jsOrder(values)
	if kind, _ := jsGet(values, "kanban-plugin").(string); !obsidianBoardKinds[kind] {
		return nil, errors.New("The frontmatter does not say kanban-plugin: board; this is not an Obsidian Kanban board")
	}
	for _, kv := range values {
		if kv.Key != "kanban-plugin" && !obsidianSettingKey.MatchString(kv.Key) {
			unsupported = append(unsupported, importUnsupportedObj("/frontmatter/"+kv.Key, `frontmatter "`+kv.Key+`" has no WeKan place`))
		}
	}
	bodyStart := end + 1
	// The %% kanban:settings ... %% footer.
	var settings any = jsObject{}
	stop := len(lines)
	for i, line := range lines {
		if jsTrim(line) == "%% kanban:settings" {
			stop = i
			raw := jsTrim(strings.ReplaceAll(obsidianSettingEnd.ReplaceAllString(strings.Join(lines[i+1:], "\n"), ""), "```", ""))
			if v, err := decodeJS([]byte(raw)); err == nil && json.Valid([]byte(raw)) && jsTruthy(v) {
				settings = v
			}
			break
		}
	}
	option := func(key string, fallback string) any {
		if v := jsGet(settings, key); jsTruthy(v) {
			return v
		}
		if v := jsGet(values, key); jsTruthy(v) {
			return v
		}
		return fallback
	}
	opt := obsidianOptions{format: option("date-format", "YYYY-MM-DD"), trigger: option("date-trigger", "@"), timeTrigger: option("time-trigger", "@@")}

	columns := []any{}
	hasColumn := func(title string) bool {
		for _, c := range columns {
			if jsGet(c, "title") == title {
				return true
			}
		}
		return false
	}
	tasks := []any{}
	var current jsObject // the lane, as its column object
	complete, archived, afterBreak := false, false, false
	type openCard struct {
		first, at, lane string
		done, archived  bool
	}
	var card *openCard
	var body []string
	flush := func() error {
		if card == nil {
			return nil
		}
		task, err := obsidianParseCard(card.first, body, opt, card.at, &unsupported)
		if err != nil {
			return err
		}
		task.set("column_name", card.lane)
		task.set("swimlane_name", "Default")
		if card.done {
			tags := jsGet(task, "tags").([]any)
			found := false
			for _, t := range tags {
				found = found || t == "done"
			}
			if !found {
				task.set("tags", append(tags, "done"))
			}
		}
		if card.archived {
			task.set("archived", true)
		}
		tasks = append(tasks, task)
		card, body = nil, nil
		return nil
	}
	for index := bodyStart; index < stop; index++ {
		line := lines[index]
		at := "/line/" + strconv.Itoa(index+1)
		if card != nil && obsidianBody.MatchString(line) {
			body = append(body, obsidianBody.ReplaceAllString(line, ""))
			continue
		}
		if card != nil && jsTrim(line) == "" {
			if err := flush(); err != nil {
				return nil, err
			}
			continue
		}
		trimmed := jsTrim(line)
		if trimmed == "" {
			continue
		}
		if trimmed == "***" {
			if err := flush(); err != nil {
				return nil, err
			}
			afterBreak = true
			continue
		}
		if m := obsidianLane.FindStringSubmatch(line); m != nil {
			if err := flush(); err != nil {
				return nil, err
			}
			if afterBreak {
				archived = true
				continue
			}
			title := m[1]
			limit := obsidianLaneLimit.FindStringSubmatch(m[1])
			if limit != nil {
				title = limit[1]
			}
			if title == "" {
				title = "Untitled"
			}
			current = jsObject{}
			current.set("title", title)
			if limit != nil {
				n, _ := strconv.ParseFloat(limit[2], 64)
				current.set("wip_limit", n)
			}
			if !hasColumn(title) {
				columns = append(columns, current)
			}
			complete = false
			continue
		}
		if trimmed == "**Complete**" {
			complete = true
			continue
		}
		if m := obsidianCard.FindStringSubmatch(line); m != nil {
			if err := flush(); err != nil {
				return nil, err
			}
			if !archived && current == nil {
				current = jsObject{}
				current.set("title", "Untitled")
				columns = append(columns, current)
			}
			home := current
			if archived {
				switch {
				case current != nil:
				case len(columns) > 0:
					home = columns[0].(jsObject)
				default:
					home = jsObject{}
					home.set("title", "Archive")
				}
				if !hasColumn(jsGet(home, "title").(string)) {
					columns = append(columns, home)
				}
			}
			card = &openCard{first: m[2], at: at, lane: jsGet(home, "title").(string), done: complete || m[1] != " ", archived: archived}
			continue
		}
		unsupported = append(unsupported, importUnsupportedObj(at, "line is not a lane, a card or a card's body"))
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return importDoc("Imported Obsidian Kanban board", columns, tasks, []any{}, unsupported), nil
}
