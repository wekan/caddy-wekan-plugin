package web

import (
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
)

// WeKan's submit-on-Enter: the member's own preference, flipped by
// toggleSubmitOnEnter and carried on every page, off at first.
func TestSubmitOnEnterPreference(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	w.seedUser(t, "rob", "pw")
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	rob := w.client(t)
	body(t, w.signIn(t, rob, "rob", "pw"))
	attr := func(c *http.Client) string {
		_, html := get(t, c, w.srv.URL+"/")
		m := regexp.MustCompile(`<body data-submit-on-enter="([^"]*)"`).FindStringSubmatch(html)
		if m == nil {
			t.Fatal("the page carries the preference")
		}
		return m[1]
	}
	if attr(ann) != "" {
		t.Fatal("off at first: Ctrl/Cmd+Enter saves")
	}
	toggle := func() {
		req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/toggleSubmitOnEnter", strings.NewReader(`[]`))
		req.Header.Set("Content-Type", "application/json")
		if res, err := ann.Do(req); err != nil || res.StatusCode != 200 {
			t.Fatal("toggleSubmitOnEnter")
		}
	}
	toggle()
	if attr(ann) != "true" {
		t.Error("on after the toggle")
	}
	if attr(rob) != "" {
		t.Error("negative: one member's choice, not another's")
	}
	toggle()
	if attr(ann) != "" {
		t.Error("off again")
	}
	_, html := get(t, ann, w.srv.URL+"/")
	if !strings.Contains(html, "js-change-settings") || !strings.Contains(html, "js-toggle-submit-on-enter") {
		t.Error("Member Settings' Change Settings and its toggle")
	}
}

// Negative, tree-wide: no editor keeps a fixed key of its own - every
// Enter goes through the one rule that reads the preference.
func TestNoEditorHasItsOwnEnterKey(t *testing.T) {
	src, err := os.ReadFile("static/weddy.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	if n := strings.Count(s, "event.key === 'Enter'") + strings.Count(s, `event.key === "Enter"`); n != 0 {
		t.Errorf("an editor with its own Enter handling: %d", n)
	}
	if !strings.Contains(s, "function isSubmitKey(event)") || !strings.Contains(s, "data-submit-on-enter") {
		t.Error("the one rule, reading the preference")
	}
}
