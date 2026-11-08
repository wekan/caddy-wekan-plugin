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
