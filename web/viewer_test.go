package web

import (
	"strings"
	"testing"
)

// WeKan's viewer: Markdown shown as HTML; what a user typed is never run.
func TestViewerIsSafe(t *testing.T) {
	if got := string(viewer("[Wekan](https://example.invalid/x) **bold**")); !strings.Contains(got, `<a href="https://example.invalid/x">Wekan</a>`) || !strings.Contains(got, "<strong>bold</strong>") {
		t.Fatalf("a link and emphasis: %s", got)
	}
	for _, evil := range []string{`<script>alert(1)</script>`, `<img src=x onerror=alert(1)>`, `[x](javascript:alert(1))`, `<a href="javascript:alert(1)">x</a>`} {
		got := string(viewer(evil))
		if strings.Contains(got, "<script") || strings.Contains(got, "onerror") || strings.Contains(got, "javascript:") {
			t.Errorf("negative: %q is not run: %s", evil, got)
		}
	}
	if got := string(viewer("one line")); got != `<span class="viewer">one line</span>` {
		t.Fatalf("a one-line title is inline: %s", got)
	}
}

// Emoji, and Admin Panel's text policies, as WeKan's viewer.
func TestViewerPolicies(t *testing.T) {
	text := "# Demo [card](https://example.com/) :thumbsup: :heart: :tada: `:tada:`"
	got := string(viewer(text))
	if !strings.Contains(got, "<h1>") || !strings.Contains(got, `<a href="https://example.com/">card</a>`) || !strings.Contains(got, "👍") ||
		!strings.Contains(got, "❤️") || !strings.Contains(got, "<code>:tada:</code>") {
		t.Fatalf("formatted, with emoji - never in code: %s", got)
	}
	if got := string(viewer(text, textPolicy{PlainLinks: true})); strings.Contains(got, "<a ") || !strings.Contains(got, "card") || !strings.Contains(got, "<h1>") {
		t.Fatalf("plain links: the text, not the link: %s", got)
	}
	got = string(viewer(text, textPolicy{PlainSource: true}))
	if !strings.Contains(got, "<pre># Demo [card](https://example.com/) :thumbsup: :heart: :tada: `:tada:`</pre>") || strings.Contains(got, "<h1>") {
		t.Fatalf("plain source: the source, escaped: %s", got)
	}
	if got := string(viewer("<b>x</b>", textPolicy{PlainSource: true})); !strings.Contains(got, "&lt;b&gt;") {
		t.Fatalf("negative: the source is escaped: %s", got)
	}
	if got := string(viewer(`[:tada:](https://x.example/:tada:)`)); strings.Contains(got, "x.example/🎉") {
		t.Fatalf("negative: never inside a tag: %s", got)
	}
}
