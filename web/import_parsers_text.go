package web

import (
	"regexp"
	"strconv"
	"strings"
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
			t.set("title", m[2])
			t.set("description", "")
			t.set("column_name", currentList)
			t.set("swimlane_name", "Default")
			tags := []any{}
			if strings.ToLower(m[1]) == "x" {
				tags = append(tags, "done")
			}
			t.set("tags", tags)
			tasks = append(tasks, t)
			current = t
			continue
		}
		if current != nil && mdIndent.MatchString(line) {
			d := jsGet(current, "description").(string)
			if d != "" {
				d += "\n" + jsTrim(line)
			} else {
				d = jsTrim(line)
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
