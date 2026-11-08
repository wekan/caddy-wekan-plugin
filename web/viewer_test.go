package web

import (
	"html/template"
	"strings"
	"testing"
)

// WeKan's viewer: Markdown shown as HTML; what a user typed is never run.
func TestViewerIsSafe(t *testing.T) {
	if got := string(viewer("[Wekan](https://example.invalid/x) **bold**")); !strings.Contains(got, `href="https://example.invalid/x"`) || !strings.Contains(got, ">Wekan</a>") || !strings.Contains(got, "<strong>bold</strong>") {
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
	if !strings.Contains(got, "<h1>") || !strings.Contains(got, `href="https://example.com/"`) || !strings.Contains(got, "👍") ||
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

// HTML a user wrote: formatting stays, a script or an event handler never.
func TestViewerSanitizesHTML(t *testing.T) {
	got := string(viewer(`# Demo <strong>allowed</strong> <img src="x" onerror="window.attack=1"> <script>alert(1)</script> <a href="javascript:alert(1)">j</a>`))
	if !strings.Contains(got, "<strong>allowed</strong>") {
		t.Fatalf("formatting stays: %s", got)
	}
	for _, bad := range []string{"onerror", "<script", "javascript:"} {
		if strings.Contains(got, bad) {
			t.Errorf("negative: %s is gone: %s", bad, got)
		}
	}
}

// An activity's sentence and names are escaped; only the value the viewer
// shows is HTML, and a %s in a name is never filled again.
func TestHTMLMessage(t *testing.T) {
	got := string(htmlMessage("changed title to %s of %s", template.HTML("<strong>x</strong>"), template.HTML(template.HTMLEscapeString("<b>%s</b>"))))
	if got != "changed title to <strong>x</strong> of &lt;b&gt;%s&lt;/b&gt;" {
		t.Fatalf("got %s", got)
	}
	if got := string(htmlMessage("a %s b %s")); got != "a  b " {
		t.Fatalf("negative: a missing argument leaves nothing: %q", got)
	}
}
