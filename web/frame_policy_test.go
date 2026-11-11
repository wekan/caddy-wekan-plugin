package web

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestFramingHeaders(t *testing.T) {
	if h := framingHeaders(false, "https://a.example"); h != nil {
		t.Errorf("off: %v", h)
	}
	h := framingHeaders(true, "")
	if h["Content-Security-Policy"] != "frame-ancestors 'self'" || h["X-Frame-Options"] != "SAMEORIGIN" {
		t.Errorf("on: %v", h)
	}
	h = framingHeaders(true, "https://Portal.example/path?x, http://intra.example:8080 https://portal.example")
	if h["Content-Security-Policy"] != "frame-ancestors 'self' https://portal.example http://intra.example:8080" {
		t.Errorf("trusted: %q", h["Content-Security-Policy"])
	}
	if _, ok := h["X-Frame-Options"]; ok {
		t.Error("X-Frame-Options cannot name another origin, so it is left off when TRUSTED_URL names one")
	}
	// Negative: what is not an http(s) origin never widens the policy.
	for _, bad := range []string{"*", "javascript:alert(1)", "'unsafe-inline'", "data:text/html,x", "https://user@evil.example", "//evil.example", "evil.example"} {
		if got := framingHeaders(true, bad)["Content-Security-Policy"]; got != "frame-ancestors 'self'" {
			t.Errorf("%q widened it: %q", bad, got)
		}
	}
}

func TestFramingHeadersFromEnv(t *testing.T) {
	for v, on := range map[string]bool{"": true, "true": true, "1": true, "false": false, " FALSE ": false} {
		t.Setenv("BROWSER_POLICY_ENABLED", v)
		t.Setenv("TRUSTED_URL", "")
		if got := framingHeadersFromEnv() != nil; got != on {
			t.Errorf("BROWSER_POLICY_ENABLED=%q: on %v, want %v", v, got, on)
		}
	}
}

// Every kind of answer carries it - a page, a static file, the API, a 404 -
// and a file keeps the stricter policy of its own, also on a 304.
func TestFrameBleedEveryResponse(t *testing.T) {
	t.Setenv("BROWSER_POLICY_ENABLED", "")
	t.Setenv("TRUSTED_URL", "")
	w := newTestWeKan(t)
	c := w.client(t)
	for _, p := range []string{"/sign-in", "/sign-up", "/static/", "/api/boards", "/no-such-page", "/cdn/storage/attachments/none"} {
		res, err := c.Get(w.srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'self'") {
			t.Errorf("%s (%d): Content-Security-Policy %q", p, res.StatusCode, csp)
		}
		if xfo := res.Header.Get("X-Frame-Options"); xfo != "SAMEORIGIN" {
			t.Errorf("%s (%d): X-Frame-Options %q", p, res.StatusCode, xfo)
		}
	}

	// A file's own policy replaces it: the avatar route, with its 304.
	h := http.Header{}
	setFramingHeaders(headerWriter{h}, w.s.framing)
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	if v := h.Values("Content-Security-Policy"); len(v) != 1 || v[0] != "default-src 'none'; sandbox" {
		t.Errorf("a route's own policy: %v", v)
	}
}

func TestFrameBleedOffAndTrusted(t *testing.T) {
	t.Setenv("BROWSER_POLICY_ENABLED", "false")
	w := newTestWeKan(t)
	res, err := w.client(t).Get(w.srv.URL + "/sign-in")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.Header.Get("Content-Security-Policy") != "" || res.Header.Get("X-Frame-Options") != "" {
		t.Errorf("BROWSER_POLICY_ENABLED=false still restricts framing: %v", res.Header)
	}

	t.Setenv("BROWSER_POLICY_ENABLED", "true")
	t.Setenv("TRUSTED_URL", "https://portal.example")
	w = newTestWeKan(t)
	res, err = w.client(t).Get(w.srv.URL + "/sign-in")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.Header.Get("Content-Security-Policy") != "frame-ancestors 'self' https://portal.example" || res.Header.Get("X-Frame-Options") != "" {
		t.Errorf("TRUSTED_URL: %v", res.Header)
	}
}

// Negative, over the whole package: no route ADDS a second policy (two CSP
// headers are both enforced, and the app-wide one would then hold on a file
// too), and nothing else in it answers HTTP past Server.ServeHTTP, where the
// headers are set.
func TestFrameBleedNowhereBypassed(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	add := regexp.MustCompile(`\.Add\(\s*"(?i:content-security-policy|x-frame-options)"`)
	serve := regexp.MustCompile(`func \([^)]*\) ServeHTTP\(`)
	listen := regexp.MustCompile(`http\.(ListenAndServe|Serve)\(|&http\.Server\{`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		if add.MatchString(src) {
			t.Errorf("%s adds a framing header instead of setting it", f)
		}
		if f != "server.go" && serve.MatchString(src) {
			t.Errorf("%s has a ServeHTTP of its own, which would answer without FrameBleed's headers", f)
		}
		if listen.MatchString(src) {
			t.Errorf("%s serves HTTP itself, past Server.ServeHTTP", f)
		}
	}
	src, _ := os.ReadFile("server.go")
	if !regexp.MustCompile(`func \(s \*Server\) ServeHTTP\(w http.ResponseWriter, r \*http.Request\) \{\n\tsetFramingHeaders\(w, s.framing\)\n\ts.mux.ServeHTTP\(w, r\)`).Match(src) {
		t.Error("Server.ServeHTTP no longer sets the framing headers before every route")
	}
}

type headerWriter struct{ h http.Header }

func (w headerWriter) Header() http.Header         { return w.h }
func (w headerWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w headerWriter) WriteHeader(int)             {}
