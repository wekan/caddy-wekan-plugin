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

// jsRegexp compiles a pattern whose \s and \S mean JavaScript's.
func jsRegexp(pattern string) *regexp.Regexp {
	p := strings.ReplaceAll(pattern, `\S`, `[^`+jsWS+`]`)
	p = strings.ReplaceAll(p, `\s`, `[`+jsWS+`]`)
	return regexp.MustCompile(p)
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
