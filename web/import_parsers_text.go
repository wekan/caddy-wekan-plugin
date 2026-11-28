package web

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The sources whose file is text, not JSON - WeKan's check(board, String):
// each parser reads the text as Node.js WeKan's does, checked against
// WeKan's own (TestImportParsersAreWeKans).
var importTextParsers = map[string]func(string) (jsObject, error){
	"markdown": parseMarkdownKanban,
}

// jsWS is JavaScript's \s as a character class body: Unicode's white space
// and the byte order mark (Go's \s is ASCII only).
const jsWS = `\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`

// jsRegexp compiles a pattern whose \s and \S mean JavaScript's: outside a
// character class as a class of their own, inside one as its members (\S
// inside a class is not supported, and refused).
func jsRegexp(pattern string) *regexp.Regexp {
	var b strings.Builder
	inClass := false
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		if c == '\\' && i+1 < len(pattern) {
			next := pattern[i+1]
			switch {
			case next == 's' && inClass:
				b.WriteString(jsWS)
			case next == 's':
				b.WriteString("[" + jsWS + "]")
			case next == 'S' && inClass:
				panic("jsRegexp: \\S inside a character class")
			case next == 'S':
				b.WriteString("[^" + jsWS + "]")
			default:
				b.WriteByte(c)
				b.WriteByte(next)
			}
			i++
			continue
		}
		switch c {
		case '[':
			inClass = true
		case ']':
			inClass = false
		}
		b.WriteByte(c)
	}
	return regexp.MustCompile(b.String())
}

var (
	mdTitle   = jsRegexp(`^#\s+(.+?)\s*$`)
	mdList    = jsRegexp(`^##\s+(.+?)\s*$`)
	mdTask    = jsRegexp(`^[-*]\s*(?:\[([ xX])\]\s*)?(.+?)\s*$`)
	mdIndent  = jsRegexp(`^\s+\S`)
	mdHeading = jsRegexp(`^#{1,6}\s`)
)

var (
	mdTag         = jsRegexp(`(^|\s)#([A-Za-z][\w/-]*)`)
	mdDue         = jsRegexp(`@\{(\d{4}-\d{2}-\d{2})\}|📅\s*(\d{4}-\d{2}-\d{2})|\bdue:(\d{4}-\d{2}-\d{2})`)
	mdTime        = regexp.MustCompile(`@@\{(\d{1,2}):(\d{2})\}`)
	mdSpace       = jsRegexp(`\s+`)
	mdItem        = jsRegexp(`^\s+[-*+]\s+\[([ xX])\]\s+(.+?)\s*$`)
	mdEscapedItem = jsRegexp(`^\\[-*+]\s+\[`)
)

func markdownCardLine(text string, done bool) (title string, tags []any, due string, invalid string) {
	tags = []any{}
	seen := map[string]bool{}
	if done {
		tags = append(tags, "done")
		seen["done"] = true
	}
	if day := mdDue.FindStringSubmatch(text); day != nil {
		iso := day[1] + day[2] + day[3]
		hour, minute := "00", "00"
		clock := mdTime.FindStringSubmatch(text)
		if clock != nil {
			hour, minute = clock[1], clock[2]
			if len(hour) == 1 {
				hour = "0" + hour
			}
		}
		if date, err := time.Parse("2006-01-02T15:04", iso+"T"+hour+":"+minute); err == nil {
			due = date.Format("2006-01-02T15:04:05.000Z")
		} else {
			invalid = day[0]
		}
		text = strings.Replace(text, day[0], " ", 1)
		if clock != nil {
			text = strings.Replace(text, clock[0], " ", 1)
		}
	}
	var body strings.Builder
	last := 0
	for _, match := range mdTag.FindAllStringSubmatchIndex(text, -1) {
		body.WriteString(text[last:match[2]])
		body.WriteString(text[match[2]:match[3]])
		tag := text[match[4]:match[5]]
		if !seen[tag] {
			tags = append(tags, tag)
			seen[tag] = true
		}
		last = match[1]
	}
	body.WriteString(text[last:])
	return jsTrim(mdSpace.ReplaceAllString(body.String(), " ")), tags, due, invalid
}

// parseMarkdownKanban is WeKan's: "# Board", "## List", "- [ ] card" with
// indented lines as its description.
func parseMarkdownKanban(text string) (jsObject, error) {
	tasks, unsupported := []any{}, []any{}
	boardName, sawTitle := "Imported Markdown board", false
	currentList := "Imported"
	var current jsObject
	for n, line := range jsLines(text) {
		if m := mdTitle.FindStringSubmatch(line); m != nil && !sawTitle {
			boardName, sawTitle, current = m[1], true, nil
			continue
		}
		if m := mdList.FindStringSubmatch(line); m != nil {
			currentList, current = m[1], nil
			continue
		}
		if m := mdTask.FindStringSubmatch(line); m != nil {
			t := jsObject{}
			title, tags, due, invalid := markdownCardLine(m[2], strings.EqualFold(m[1], "x"))
			if title == "" {
				title = m[2]
			}
			if invalid != "" {
				unsupported = append(unsupported, importUnsupportedObj("/"+strconv.Itoa(n), invalid+" is not a date"))
			}
			t.set("title", title)
			t.set("description", "")
			t.set("column_name", currentList)
			t.set("swimlane_name", "Default")
			t.set("tags", tags)
			if due != "" {
				t.set("date_due", due)
			}
			tasks = append(tasks, t)
			current = t
			continue
		}
		if item := mdItem.FindStringSubmatch(line); current != nil && item != nil {
			entry := jsObject{{"title", item[2]}, {"done", strings.EqualFold(item[1], "x")}}
			if lists, ok := jsGet(current, "checklists").([]any); ok {
				checklist := lists[0].(jsObject)
				checklist.set("items", append(jsGet(checklist, "items").([]any), entry))
			} else {
				current.set("checklists", []any{jsObject{{"title", "Checklist"}, {"items", []any{entry}}}})
				tasks[len(tasks)-1] = current
			}
			continue
		}
		if current != nil && mdIndent.MatchString(line) {
			body := jsTrim(line)
			if mdEscapedItem.MatchString(body) {
				body = body[1:]
			}
			d := jsGet(current, "description").(string)
			if d != "" {
				d += "\n" + body
			} else {
				d = body
			}
			// current is the task in tasks: set it there.
			for k, kv := range current {
				if kv.Key == "description" {
					current[k].Value = d
				}
			}
			continue
		}
		if jsTrim(line) != "" && !mdHeading.MatchString(line) {
			unsupported = append(unsupported, importUnsupportedObj("/"+strconv.Itoa(n), "line matched no known markdown-kanban construct"))
		}
	}
	return importDoc(boardName, uniqColumns(tasks), tasks, []any{}, unsupported), nil
}
