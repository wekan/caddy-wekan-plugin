package web

import (
	"bytes"
	"context"
	"html"
	"html/template"
	"regexp"
	"strings"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	gmhtml "github.com/yuin/goldmark/renderer/html"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// WeKan's viewer (packages/markdown, client/components/main/editor.js
// +viewer): a text written in Markdown, shown as HTML. goldmark's own
// HTML is let through the UGC policy of bluemonday, as WeKan's goes through
// DOMPurify: formatting stays, a script, an event handler or a dangerous
// address does not - what a user typed is shown, never run.
var markdown = goldmark.New(goldmark.WithExtensions(extension.Strikethrough, extension.Linkify, extension.Table, extension.TaskList),
	goldmark.WithRendererOptions(gmhtml.WithUnsafe()))

// sanitizer is what WeKan's viewer lets through of HTML a user wrote
// (WeKan sanitizes its Markdown with DOMPurify): text formatting, lists,
// tables, quotes, code and links to safe addresses - never a script, an
// event handler or a javascript: address.
var sanitizer = bluemonday.UGCPolicy()

// textPolicy is Admin Panel's text display settings: links shown as plain
// text (renderLinksAsPlainText), or the source shown, never rendered
// (alwaysShowCodeAsText).
type textPolicy struct {
	PlainLinks, PlainSource bool
}

func (s *Server) textPolicy(ctx context.Context) textPolicy {
	var set struct {
		PlainLinks  bool `bson:"renderLinksAsPlainText"`
		PlainSource bool `bson:"alwaysShowCodeAsText"`
	}
	_ = s.db.Collection("settings").FindOne(ctx, bson.M{}).Decode(&set)
	return textPolicy{PlainLinks: set.PlainLinks, PlainSource: set.PlainSource}
}

var (
	shortcodePattern = regexp.MustCompile(`:([a-z0-9_+\-]+):`)
	codeBlock        = regexp.MustCompile(`(?s)<pre.*?</pre>|<code.*?</code>`)
	textBetweenTags  = regexp.MustCompile(`>[^<]*<`)
	anchorTag        = regexp.MustCompile(`</?a(\s[^>]*)?>`)
)

// emojify is markdown-it-emoji's: a :shortcode: in the text - never in code,
// never in a tag - becomes its emoji.
func emojify(htmlText string) string {
	replace := func(seg string) string {
		return textBetweenTags.ReplaceAllStringFunc(">"+seg+"<", func(t string) string {
			return shortcodePattern.ReplaceAllStringFunc(t, func(m string) string {
				if e, ok := emojiShortcodes[m[1:len(m)-1]]; ok {
					return e
				}
				return m
			})
		})
	}
	var out strings.Builder
	last := 0
	for _, loc := range codeBlock.FindAllStringIndex(htmlText, -1) {
		seg := replace(htmlText[last:loc[0]])
		out.WriteString(seg[1 : len(seg)-1])
		out.WriteString(htmlText[loc[0]:loc[1]])
		last = loc[1]
	}
	seg := replace(htmlText[last:])
	out.WriteString(seg[1 : len(seg)-1])
	return out.String()
}

// viewer is a text as WeKan's +viewer shows it, under Admin Panel's policy.
func viewer(text string, policy ...textPolicy) template.HTML {
	var p textPolicy
	if len(policy) > 0 {
		p = policy[0]
	}
	if p.PlainSource {
		return template.HTML(`<div class="viewer"><pre>` + html.EscapeString(text) + `</pre></div>`)
	}
	var b bytes.Buffer
	if err := markdown.Convert([]byte(text), &b); err != nil {
		return template.HTML(`<span class="viewer">` + html.EscapeString(text) + `</span>`)
	}
	out := emojify(strings.TrimSpace(sanitizer.Sanitize(b.String())))
	if p.PlainLinks {
		out = anchorTag.ReplaceAllString(out, "")
	}
	// A one-line title is one paragraph: shown inline, as WeKan's viewer does.
	if strings.HasPrefix(out, "<p>") && strings.HasSuffix(out, "</p>") && strings.Count(out, "<p>") == 1 {
		return template.HTML(`<span class="viewer">` + strings.TrimSuffix(strings.TrimPrefix(out, "<p>"), "</p>") + `</span>`)
	}
	return template.HTML(`<div class="viewer">` + out + `</div>`)
}
