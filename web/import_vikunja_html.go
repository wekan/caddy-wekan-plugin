package web

import (
	"regexp"
	"strconv"
	"strings"
)

// Vikunja's descriptions and comments are TipTap HTML: Node.js WeKan's
// vikunjaHtmlToText (models/lib/vikunjaFormat.js) reads them with a small
// tokenizer into WeKan's Markdown text, and a TipTap task list into a
// checklist. Nothing here is rendered, and the import boundary cleans the
// result again.

var (
	vikunjaEntities = map[string]string{"amp": "&", "lt": "<", "gt": ">", "quot": `"`, "apos": "'", "nbsp": " ", "#39": "'"}
	vikunjaEntityRe = regexp.MustCompile(`&(#x[0-9a-fA-F]{1,6}|#\d{1,7}|[a-zA-Z]{2,8}|#39);`)
	vikunjaTag      = regexp.MustCompile(`<!--(?s:.)*?(?:-->|$)|<(/?)([a-zA-Z][a-zA-Z0-9]*)([^<>]*)>|([^<]+)|<`)
	vikunjaAttr     = regexp.MustCompile(`([^\s=>/]+)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+)))?`)
	vikunjaHasTag   = regexp.MustCompile(`<[a-zA-Z!/]`)
	vikunjaSpaces   = jsRegexp(`\s+`)
	vikunjaHeading  = regexp.MustCompile(`^h[1-6]$`)
	vikunjaHashes   = jsRegexp(`^#+\s*`)
	vikunjaHref     = regexp.MustCompile(`(?i)^(https?:|mailto:)`)
	vikunjaSrc      = regexp.MustCompile(`(?i)^https?:`)
	vikunjaTailWS   = regexp.MustCompile(`[ \t]+$`)
	vikunjaBlankRun = regexp.MustCompile(`\n{3,}`)
	vikunjaBlock    = map[string]bool{"p": true, "div": true, "blockquote": true, "h1": true, "h2": true, "h3": true, "h4": true,
		"h5": true, "h6": true, "table": true, "tr": true, "hr": true}
	vikunjaInline    = map[string]string{"strong": "**", "b": "**", "em": "_", "i": "_", "s": "~~", "del": "~~", "strike": "~~", "code": "`"}
	markdownTitleBad = regexp.MustCompile(`[\\\[\]]`)
)

func vikunjaDecodeEntities(text string) string {
	return vikunjaEntityRe.ReplaceAllStringFunc(text, func(whole string) string {
		name := whole[1 : len(whole)-1]
		if name[0] == '#' {
			var code int64
			var err error
			if name[1] == 'x' || name[1] == 'X' {
				code, err = strconv.ParseInt(name[2:], 16, 64)
			} else {
				code, err = strconv.ParseInt(name[1:], 10, 64)
			}
			if err == nil && code > 0 && code <= 0x10ffff {
				return string(rune(code))
			}
			return whole
		}
		if v, ok := vikunjaEntities[name]; ok {
			return v
		}
		return whole
	})
}

// vikunjaAttributes reads a tag's attributes, from at most its first 4096
// characters, names in lower case.
func vikunjaAttributes(text string) map[string]string {
	out := map[string]string{}
	source := jsSlice16(text, 4096)
	for _, m := range vikunjaAttr.FindAllStringSubmatchIndex(source, -1) {
		value := ""
		for _, g := range []int{2, 3, 4} {
			if m[2*g] >= 0 {
				value = source[m[2*g]:m[2*g+1]]
				break
			}
		}
		out[jsLower(source[m[2]:m[3]])] = vikunjaDecodeEntities(value)
	}
	return out
}

// markdownLinkTitle and markdownLinkURL are WeKan's models/lib/markdownLink.js.
func markdownLinkTitle(text string) string {
	return markdownTitleBad.ReplaceAllStringFunc(text, func(c string) string { return `\` + c })
}

func markdownLinkURL(url string) string {
	l := markdownLinkText("", url)
	return l[len("[]("):strings.LastIndex(l, ")")]
}

type vikunjaChecklist struct {
	title string
	items []any
}

// vikunjaHTMLToText is WeKan's vikunjaHtmlToText: the text, and - with
// wantChecklists - its task lists as checklists; without, task lists stay
// "- [x] item" lines.
func vikunjaHTMLToText(html any, wantChecklists bool) (string, []*vikunjaChecklist) {
	source := spText(html)
	if jsTrim(source) == "" {
		return "", nil
	}
	// Older Vikunja versions stored Markdown, not HTML.
	if !vikunjaHasTag.MatchString(source) {
		return jsTrim(source), nil
	}
	out := ""
	var checklists []*vikunjaChecklist
	type openList struct {
		ordered, task bool
		count         int
	}
	var lists []*openList
	type taskItem struct {
		title string
		done  bool
	}
	var item *taskItem
	var checklist *vikunjaChecklist
	taskDepth, pre := 0, 0
	type headingAt struct{ start, level int }
	var heading *headingAt
	type lastHead struct {
		start int
		text  string
	}
	var lastHeading *lastHead
	var links []string
	trimTail := func(chars string) {
		end := len(out)
		for end > 0 && strings.IndexByte(chars, out[end-1]) >= 0 {
			end--
		}
		out = out[:end]
	}
	write := func(text string) {
		if item != nil {
			item.title += text
			return
		}
		out += text
		if jsTrim(text) != "" {
			lastHeading = nil
		}
	}
	breakBlock := func() {
		if item != nil {
			item.title += " "
			return
		}
		trimTail(" \t")
		if out != "" && !strings.HasSuffix(out, "\n\n") {
			if strings.HasSuffix(out, "\n") {
				out += "\n"
			} else {
				out += "\n\n"
			}
		}
	}
	newline := func() {
		if item != nil {
			item.title += " "
			return
		}
		trimTail(" \t")
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
	}
	finishItem := func(indentDepth int) {
		title := jsTrim(vikunjaSpaces.ReplaceAllString(item.title, " "))
		if title == "" {
			return
		}
		if checklist != nil {
			o := jsObject{}
			o.set("title", title)
			o.set("done", item.done)
			checklist.items = append(checklist.items, o)
			return
		}
		mark := " "
		if item.done {
			mark = "x"
		}
		if indentDepth < 0 {
			indentDepth = 0
		}
		out += strings.Repeat("  ", indentDepth) + "- [" + mark + "] " + title + "\n"
	}
	for _, m := range vikunjaTag.FindAllStringSubmatchIndex(source, -1) {
		whole := source[m[0]:m[1]]
		if m[8] >= 0 || whole == "<" {
			value := whole
			if m[8] >= 0 {
				value = source[m[8]:m[9]]
			}
			value = vikunjaDecodeEntities(value)
			if pre == 0 {
				value = vikunjaSpaces.ReplaceAllString(value, " ")
			}
			write(value)
			continue
		}
		if m[4] < 0 {
			continue // a comment
		}
		closing := source[m[2]:m[3]] == "/"
		name := jsLower(source[m[4]:m[5]])
		attrs := map[string]string{}
		if !closing {
			attrs = vikunjaAttributes(source[m[6]:m[7]])
		}
		var current *openList
		if len(lists) > 0 {
			current = lists[len(lists)-1]
		}
		switch {
		case name == "ul" || name == "ol":
			if closing {
				var ended *openList
				if len(lists) > 0 {
					ended, lists = lists[len(lists)-1], lists[:len(lists)-1]
				}
				if ended != nil && ended.task {
					taskDepth--
					if taskDepth == 0 {
						if !wantChecklists {
							newline()
						}
						checklist = nil
					}
				} else {
					breakBlock()
				}
				continue
			}
			task := attrs["data-type"] == "taskList"
			lists = append(lists, &openList{ordered: name == "ol", task: task})
			if task {
				taskDepth++
				if taskDepth == 1 && wantChecklists {
					title := "Checklist"
					// A heading with nothing after it names the list below it.
					if lastHeading != nil && lastHeading.text != "" {
						title = lastHeading.text
						if lastHeading.start < len(out) {
							out = out[:lastHeading.start]
						}
						trimTail(" \t\r\n")
					}
					checklist = &vikunjaChecklist{title: title}
					checklists = append(checklists, checklist)
				} else if taskDepth == 1 {
					breakBlock()
				}
			} else if item == nil {
				newline()
			}
			lastHeading = nil
		case name == "li":
			if current != nil && current.task {
				if closing {
					if item != nil {
						finishItem(taskDepth - 1)
						item = nil
					}
					continue
				}
				// A nested task item ends the item it is inside of.
				if item != nil {
					finishItem(taskDepth - 2)
				}
				item = &taskItem{done: attrs["data-checked"] == "true"}
				continue
			}
			if closing {
				continue
			}
			if current != nil {
				current.count++
			}
			newline()
			plain := 0
			for _, l := range lists {
				if !l.task {
					plain++
				}
			}
			indent := strings.Repeat("  ", max(0, plain-1))
			marker := "-"
			if current != nil && current.ordered {
				marker = strconv.Itoa(current.count) + "."
			}
			write(indent + marker + " ")
		case vikunjaHeading.MatchString(name):
			if !closing {
				breakBlock()
				heading = &headingAt{start: len(out), level: int(name[1] - '0')}
				write(strings.Repeat("#", heading.level) + " ")
			} else if heading != nil && item == nil {
				rest := ""
				if heading.start < len(out) {
					rest = out[heading.start:]
				}
				text := jsTrim(vikunjaHashes.ReplaceAllString(rest, ""))
				lastHeading = &lastHead{start: heading.start, text: text}
				heading = nil
				breakBlock()
			}
		case name == "br":
			if item != nil {
				item.title += " "
			} else {
				out += "\n"
			}
		case name == "pre":
			if closing {
				pre--
				write("\n```")
				breakBlock()
			} else {
				breakBlock()
				write("```\n")
				pre++
			}
		case name == "a":
			if closing {
				href := ""
				if len(links) > 0 {
					href, links = links[len(links)-1], links[:len(links)-1]
				}
				if href != "" {
					write("](" + markdownLinkURL(href) + ")")
				}
			} else {
				href := ""
				if vikunjaHref.MatchString(attrs["href"]) {
					href = attrs["href"]
				}
				links = append(links, href)
				if href != "" {
					write("[")
				}
			}
		case name == "img":
			if vikunjaSrc.MatchString(attrs["src"]) {
				write("![" + markdownLinkTitle(attrs["alt"]) + "](" + markdownLinkURL(attrs["src"]) + ")")
			}
		case vikunjaInline[name] != "" && !(name == "code" && pre > 0):
			write(vikunjaInline[name])
		case vikunjaBlock[name]:
			if name == "p" && item != nil {
				if !closing && item.title != "" {
					item.title += " "
				}
				continue
			}
			if name == "p" && len(lists) > 0 {
				continue // a list item's paragraph
			}
			breakBlock()
			if name == "hr" && !closing {
				write("---")
				breakBlock()
			}
		}
		// label, input, span and anything else: their text only.
	}
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		lines[i] = vikunjaTailWS.ReplaceAllString(l, "")
	}
	text := jsTrim(vikunjaBlankRun.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
	var kept []*vikunjaChecklist
	for _, c := range checklists {
		if len(c.items) > 0 {
			kept = append(kept, c)
		}
	}
	return text, kept
}
