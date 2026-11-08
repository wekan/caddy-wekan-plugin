package web

import (
	"bytes"
	"html/template"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

// WeKan's viewer (client/components/main/editor.js +viewer): a text written
// in Markdown, shown as HTML. goldmark's own renderer leaves raw HTML out and
// drops a link to a dangerous address (javascript: and the like), so what a
// user typed is shown, never run.
var markdown = goldmark.New(goldmark.WithExtensions(extension.Strikethrough, extension.Linkify, extension.Table, extension.TaskList))

// viewer is a text as WeKan's +viewer shows it, in a .viewer element.
func viewer(text string) template.HTML {
	var b bytes.Buffer
	if err := markdown.Convert([]byte(text), &b); err != nil {
		return template.HTML(`<div class="viewer">` + template.HTMLEscapeString(text) + `</div>`)
	}
	out := strings.TrimSpace(b.String())
	// A one-line title is one paragraph: shown inline, as WeKan's viewer does.
	if strings.HasPrefix(out, "<p>") && strings.HasSuffix(out, "</p>") && strings.Count(out, "<p>") == 1 {
		out = strings.TrimSuffix(strings.TrimPrefix(out, "<p>"), "</p>")
	}
	return template.HTML(`<span class="viewer">` + out + `</span>`)
}
