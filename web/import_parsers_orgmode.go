package web

import (
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// Org mode TODO outlines: Node.js WeKan's parseOrgMode
// (models/lib/orgModeFormat.js) - level 1 headings are lists, level 2 cards
// with their keyword, priority, tags, planning dates, CREATED property,
// body and checkboxes, and deeper headings checklists and their items.

func init() { importTextParsers["orgmode"] = parseOrgMode }

const (
	maxOrgLength   = 16 * 1024 * 1024 // UTF-16 units, as JavaScript's length
	maxOrgHeadings = 100000
)

var (
	orgHeading   = jsRegexp(`^(\*+)\s+(` + jsDotU + `*)$`)
	orgTags      = jsRegexp(`\s+(:[^\s:]+(?::[^\s:]+)*:)\s*$`)
	orgPrioRe    = jsRegexp(`^\[#([A-Z0-9])\]\s*`)
	orgStampRe   = jsRegexp(`[<\[](\d{4})-(\d{2})-(\d{2})(?:\s+[^\s\d>\]]+)?(?:\s+(\d{1,2}):(\d{2})(?:-\d{1,2}:\d{2})?)?((?:\s+[.+]?\+\d+[hdwmy]|\s+-{1,2}\d+[hdwmy])*)\s*[>\]]`)
	orgCheckbox  = jsRegexp(`^\s*(?:[-+]|\d+[.)])\s+\[([ Xx-])\]\s+(` + jsDotU + `*)$`)
	orgKeywords  = jsRegexp(`(?i)^#\+(?:TODO|SEQ_TODO|TYP_TODO):\s*(` + jsDotU + `*)$`)
	orgParen     = jsRegexp(`\(` + jsDotU + `*\)$`)
	orgTitleMeta = jsRegexp(`(?i)^#\+TITLE:\s*(` + jsDotU + `*)$`)
	orgFirstWord = jsRegexp(`^(\S+)(?:\s+|$)`)
	orgEnd       = jsRegexp(`(?i)^:END:$`)
	orgCreated   = jsRegexp(`(?i)^:CREATED:\s*(` + jsDotU + `*)$`)
	orgDrawer    = jsRegexp(`^:([A-Za-z_-]+):$`)
	orgPlanning  = jsRegexp(`^(SCHEDULED|DEADLINE|CLOSED):`)
	orgPlanItem  = jsRegexp(`(SCHEDULED|DEADLINE|CLOSED):\s*([<\[][^>\]]*[>\]])`)
	orgIndent    = jsRegexp(`^ {0,3}`)
	orgEdgeLines = jsRegexp(`^\n+|\n+$`)
)

// orgTimestamp is WeKan's: "<2026-10-10 Sat 14:30>" as ISO (UTC), with its
// repeater or delay so it can be reported.
func orgTimestamp(text string) (iso, extra string, ok bool) {
	m := orgStampRe.FindStringSubmatch(text)
	if m == nil {
		return "", "", false
	}
	n := func(s string) int { v, _ := strconv.Atoi(s); return v }
	year := n(m[1])
	if year < 100 { // Date.UTC's two-digit years
		year += 1900
	}
	d := time.Date(year, time.Month(n(m[2])), n(m[3]), n(m[4]), n(m[5]), 0, 0, time.UTC)
	if int(d.Month()) != n(m[2]) {
		return "", "", false
	}
	return d.Format("2006-01-02T15:04:05.000Z"), jsTrim(m[6]), true
}

func orgReadKeywords(lines []string) (todo, done []string) {
	for _, line := range lines {
		m := orgKeywords.FindStringSubmatch(jsTrim(line))
		if m == nil {
			continue
		}
		var words []string
		for _, w := range strings.FieldsFunc(m[1], jsSpace) {
			words = append(words, orgParen.ReplaceAllString(w, ""))
		}
		bar := -1
		for i, w := range words {
			if w == "|" {
				bar = i
				break
			}
		}
		if bar == -1 {
			if len(words) > 0 {
				todo = append(todo, words[:len(words)-1]...)
				done = append(done, words[len(words)-1])
			}
		} else {
			todo = append(todo, words[:bar]...)
			done = append(done, words[bar+1:]...)
		}
	}
	if len(todo) == 0 && len(done) == 0 {
		return []string{"TODO"}, []string{"DONE"}
	}
	return todo, done
}

type orgNode struct {
	level, line int
	body        []string
	done        bool
	priority    string
	tags        []string
	title       string
	children    []*orgNode
}

func orgHas(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func parseOrgMode(text string) (jsObject, error) {
	if len(utf16.Encode([]rune(text))) > maxOrgLength {
		return nil, errors.New("Org file is too large")
	}
	lines := jsLines(strings.TrimPrefix(text, "\ufeff"))
	todo, done := orgReadKeywords(lines)
	unsupported := []any{}
	var headings []*orgNode
	raws := map[*orgNode]string{}
	title := ""
	for index, line := range lines {
		if m := orgHeading.FindStringSubmatch(line); m != nil {
			if len(headings) >= maxOrgHeadings {
				return nil, errors.New("Org file has too many headings")
			}
			h := &orgNode{level: len(m[1]), line: index + 1, body: []string{}}
			raws[h] = m[2]
			headings = append(headings, h)
		} else if len(headings) > 0 {
			last := headings[len(headings)-1]
			last.body = append(last.body, line)
		} else if meta := orgTitleMeta.FindStringSubmatch(jsTrim(line)); meta != nil {
			title = jsTrim(meta[1])
		} else if t := jsTrim(line); t != "" && !strings.HasPrefix(t, "#+") {
			unsupported = append(unsupported, importUnsupportedObj("/line/"+strconv.Itoa(index+1), "text before the first heading has no WeKan field"))
		}
	}
	if len(headings) == 0 {
		return nil, errors.New("Not an Org outline: no headings")
	}
	for _, h := range headings {
		rest, keyword := raws[h], ""
		if m := orgFirstWord.FindStringSubmatch(rest); m != nil && (orgHas(todo, m[1]) || orgHas(done, m[1])) {
			keyword, rest = m[1], rest[len(m[0]):]
		}
		if m := orgPrioRe.FindStringSubmatch(rest); m != nil {
			h.priority, rest = m[1], rest[len(m[0]):]
		}
		h.tags = []string{}
		if idx := orgTags.FindStringSubmatchIndex(rest); idx != nil {
			for _, t := range strings.Split(rest[idx[2]:idx[3]], ":") {
				if t != "" {
					h.tags = append(h.tags, t)
				}
			}
			rest = rest[:idx[0]]
		}
		h.done = keyword != "" && orgHas(done, keyword)
		h.title = jsTrim(rest)
	}

	readBody := func(h *orgNode, at string) (dates map[string]string, checkboxes []any, description string) {
		dates = map[string]string{}
		var text []string
		drawer := ""
		for _, line := range h.body {
			trimmed := jsTrim(line)
			if drawer != "" {
				if orgEnd.MatchString(trimmed) {
					drawer = ""
					continue
				}
				if p := orgCreated.FindStringSubmatch(trimmed); drawer == "PROPERTIES" && p != nil {
					if iso, _, ok := orgTimestamp(p[1]); ok {
						dates["created"] = iso
					}
				}
				continue
			}
			if m := orgDrawer.FindStringSubmatch(trimmed); m != nil {
				drawer = strings.ToUpper(m[1])
				continue
			}
			if len(text) == 0 && len(checkboxes) == 0 && orgPlanning.MatchString(trimmed) {
				for _, m := range orgPlanItem.FindAllStringSubmatch(trimmed, -1) {
					word := m[1]
					iso, extra, ok := orgTimestamp(m[2])
					if !ok {
						unsupported = append(unsupported, importUnsupportedObj(at+"/"+word, "Org "+word+" is not a date"))
						continue
					}
					if extra != "" {
						unsupported = append(unsupported, importUnsupportedObj(at+"/"+word, "Org repeater or delay "+extra+" is not kept"))
					}
					dates[strings.ToLower(word)] = iso
				}
				continue
			}
			if m := orgCheckbox.FindStringSubmatch(line); m != nil {
				item := jsObject{}
				item.set("title", jsTrim(m[2]))
				item.set("done", strings.EqualFold(m[1], "x"))
				checkboxes = append(checkboxes, item)
				continue
			}
			text = append(text, orgIndent.ReplaceAllString(line, ""))
		}
		return dates, checkboxes, orgEdgeLines.ReplaceAllString(strings.Join(text, "\n"), "")
	}

	var roots, stack []*orgNode
	for _, h := range headings {
		for len(stack) > 0 && stack[len(stack)-1].level >= h.level {
			stack = stack[:len(stack)-1]
		}
		if len(stack) > 0 {
			p := stack[len(stack)-1]
			p.children = append(p.children, h)
		} else {
			roots = append(roots, h)
		}
		stack = append(stack, h)
	}
	var descendants func(n *orgNode) []*orgNode
	descendants = func(n *orgNode) []*orgNode {
		var out []*orgNode
		for _, c := range n.children {
			out = append(append(out, c), descendants(c)...)
		}
		return out
	}
	item := func(n *orgNode) jsObject {
		o := jsObject{}
		o.set("title", n.title)
		o.set("done", n.done)
		return o
	}

	var columns []string
	tasks := []any{}
	for _, list := range roots {
		name := list.title
		if name == "" {
			name = "Imported"
		}
		if !orgHas(columns, name) {
			columns = append(columns, name)
		}
		for _, line := range list.body {
			if t := jsTrim(line); t != "" && !orgDrawer.MatchString(t) {
				unsupported = append(unsupported, importUnsupportedObj("/line/"+strconv.Itoa(list.line), "text under a list heading has no WeKan field"))
				break
			}
		}
		for _, c := range list.children {
			at := "/line/" + strconv.Itoa(c.line)
			dates, boxes, description := readBody(c, at)
			checklists := []any{}
			items := append([]any{}, boxes...)
			for _, k := range c.children {
				if len(k.children) == 0 {
					items = append(items, item(k))
				}
			}
			if len(items) > 0 {
				cl := jsObject{}
				cl.set("title", jsOr(c.title, "Checklist"))
				cl.set("items", items)
				checklists = append(checklists, cl)
			}
			for _, k := range c.children {
				if len(k.children) > 0 {
					its := []any{}
					for _, d := range descendants(k) {
						its = append(its, item(d))
					}
					cl := jsObject{}
					cl.set("title", k.title)
					cl.set("items", its)
					checklists = append(checklists, cl)
				}
			}
			tags := []any{}
			if c.priority != "" {
				tags = append(tags, "priority:"+c.priority)
			}
			for _, t := range c.tags {
				tags = append(tags, t)
			}
			if c.done {
				tags = append(tags, "done")
			}
			t := jsObject{}
			t.set("title", jsOr(c.title, "Imported heading"))
			t.set("description", description)
			t.set("column_name", name)
			t.set("swimlane_name", "Default")
			t.set("tags", tags)
			for _, d := range []struct{ from, to string }{{"deadline", "date_due"}, {"scheduled", "date_started"}, {"closed", "date_end"}, {"created", "date_creation"}} {
				if v := dates[d.from]; v != "" {
					t.set(d.to, v)
				}
			}
			if len(checklists) > 0 {
				t.set("checklists", checklists)
			}
			tasks = append(tasks, t)
		}
	}
	cols := []any{}
	for _, c := range columns {
		o := jsObject{}
		o.set("title", c)
		cols = append(cols, o)
	}
	return importDoc(jsOr(title, "Imported Org outline"), cols, tasks, []any{}, unsupported), nil
}
