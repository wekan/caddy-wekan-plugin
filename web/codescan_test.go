package web

// Code scanning, 2026-11-21: go/email-injection (#26), js/xss-through-dom
// (#105-#107), go/request-forgery (#108), go/incorrect-integer-conversion
// (#159-#161) and go/unvalidated-url-redirection (#162); #163 is
// TestNoFastHashOfPasswords'. Each fix has a test of what it does, a negative
// test of what it refuses, and a scan for the shape it removed, so the shape
// cannot come back anywhere. funcSource is query_values_test.go's.

import (
	"context"
	"math"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Code scanning #162: a Restore goes back to the board's own History page,
// with the page's query, never anywhere else.
func TestHistoryBack(t *testing.T) {
	page := "/b/B1/history"
	for back, want := range map[string]string{
		page:                          page,
		page + "?page=2&q=a%20b":      page + "?page=2&q=a%20b",
		"":                            page,
		"//evil.example/b/B1/history": page,
		"https://evil.example/":       page,
		"/b/B2/history":               page,
		page + "@evil.example":        page,
		page + "/../../../x":          page,
		page + "x":                    page,
		"/\\evil.example":             page,
		" " + page:                    page,
	} {
		if got := historyBack(page, back); got != want {
			t.Errorf("historyBack(%q) = %q, want %q", back, got, want)
		}
	}
	if strings.Contains(funcSource(t, "history_view.go", "historyRestorePage"), "http.Redirect(w, r, back") {
		t.Error("negative: the form's value is redirected to as it came")
	}
}

// Code scanning #108: a download is an http or https address, checked on the
// very text requested - also where the SSRF guard is off for a test.
func TestFetchImportedAttachmentOnlyHTTP(t *testing.T) {
	for _, ok := range []string{"http://127.0.0.1:1234/x?y=1#z", "https://example.com", "HTTPS://example.com/a/b"} {
		if !outboundHTTPURL.MatchString(ok) {
			t.Errorf("%q is an http(s) address", ok)
		}
	}
	for _, bad := range []string{"", "file:///etc/passwd", "gopher://127.0.0.1:70/x", "ftp://example.com/x", "javascript:alert(1)", "//evil.example/x", "http:///x", "http://exa mple.com/", "/relative"} {
		if outboundHTTPURL.MatchString(bad) {
			t.Errorf("negative: %q is no http(s) address", bad)
		}
	}
	webhookAllowLoopbackForTests = true
	t.Cleanup(func() { webhookAllowLoopbackForTests = false })
	for _, bad := range []string{"file:///etc/passwd", "gopher://127.0.0.1:70/x", "ftp://127.0.0.1/x"} {
		_, _, err := fetchImportedAttachment(context.Background(), bad, nil)
		if blocked, ok := err.(errAttachmentBlocked); !ok || !strings.Contains(blocked.reason, "Only HTTP and HTTPS") {
			t.Errorf("negative: %s refused before it is requested: %v", bad, err)
		}
	}
}

// Code scanning #26: what goes in a header is made of headerBytes - no control
// character, so never a header or a body of its own.
func TestMailHeadersHaveNoControlCharacters(t *testing.T) {
	for _, ok := range []string{"a@example.com", "Jöns <j@example.com>", "=?utf-8?q?Hyv=C3=A4?=", "a\tb"} {
		if got, good := headerText(ok); !good || got != ok {
			t.Errorf("headerText(%q) = %q, %v", ok, got, good)
		}
	}
	for _, bad := range []string{"a\r\nBcc: x@example.com", "a\nb", "a\rb", "a\x00b", "a\x7fb", "a\x1bb"} {
		if got, good := headerText(bad); good || got != "" {
			t.Errorf("negative: headerText(%q) = %q, %v", bad, got, good)
		}
	}
	msg, err := buildMail("Support <s@example.com>", mailMessage{To: []string{"a@example.com", "b@example.com"}, Subject: "x\x01y", Text: "t"}, time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	s := string(msg)
	if !strings.Contains(s, "From: \"Support\" <s@example.com>\r\nTo: a@example.com, b@example.com\r\nSubject: =?utf-8?q?x=01y?=\r\n") {
		t.Errorf("the headers: %q", s)
	}
	if _, err := buildMail("s@example.com", mailMessage{To: []string{"a@example.com\x00"}, Subject: "x"}, time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)); err == nil {
		t.Error("negative: an address with a NUL")
	}
	src := funcSource(t, "mail.go", "buildMail")
	if strings.Contains(src, "strings.Join(m.To") || !strings.Contains(src, "headerText(") {
		t.Error("negative: the To header is written from the addresses as they came")
	}
}

// Code scanning #159-#161: a JavaScript number in an import is never an
// integer cut at 64 bits, and a WIP limit is converted only when an int
// holds it on every platform.
func TestImportNumbersAreNeverCut(t *testing.T) {
	for s, want := range map[string]float64{"0x10": 16, "0b101": 5, "0o17": 15, "0X7FFFFFFF": math.MaxInt32,
		"0x" + strings.Repeat("F", 20): math.Pow(2, 80)} {
		if got, ok := jsToNumber(s); !ok || got != want {
			t.Errorf("jsToNumber(%q) = %v, %v; want %v (as JavaScript's Number)", s, got, ok, want)
		}
	}
	if n, ok := jsToNumber("0xG"); ok || !math.IsNaN(n) {
		t.Errorf("negative: 0xG is NaN: %v %v", n, ok)
	}
	limit := func(v any) int {
		return columnLimit(jsObject{{"columns", []any{jsObject{{"title", "A"}, {"wip_limit", v}}}}}, "A")
	}
	for v, want := range map[any]int{5.0: 5, "7": 7, "0x7FFFFFFF": math.MaxInt32, 1e20: 0, 2147483648.0: 0, 2.5: 0, -3.0: 0, "0x" + strings.Repeat("F", 20): 0} {
		if got := limit(v); got != want {
			t.Errorf("columnLimit of %v = %d, want %d", v, got, want)
		}
	}
	if strings.Contains(funcSource(t, "import_plan.go", "jsToNumber"), "strconv.ParseUint(") ||
		strings.Contains(funcSource(t, "import_general.go", "columnLimit"), "int64(n)") {
		t.Error("negative: a 64-bit integer converted further")
	}
}

// Code scanning #105-#107: an address the page script puts together for an
// href or a form's action is made of encoded parts - what an attribute says is
// a name in the address, never a piece of address of its own. (A data-url the
// server wrote whole, used as it is, is the server's address, not put together.)
func TestScriptAddressesAreEncoded(t *testing.T) {
	// raw is a value read from the page; it must be encodeURIComponent's.
	raw := regexp.MustCompile(`[\w.]*(?:getAttribute\([^)]*\)|dataset\.\w+)`)
	encoded := func(line string) bool {
		if !strings.Contains(line, " + ") {
			return true
		}
		for _, m := range raw.FindAllStringIndex(line, -1) {
			if !strings.HasSuffix(line[:m[0]], "encodeURIComponent(") {
				return false
			}
		}
		return !strings.Contains(line, ".toString()")
	}
	src, err := os.ReadFile("static/weddy.js")
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	for i, line := range strings.Split(string(src), "\n") {
		if (strings.Contains(line, "setAttribute('href',") || strings.Contains(line, "setAttribute('action',") || strings.Contains(line, "url = '/")) && !encoded(line) {
			offenders = append(offenders, "weddy.js:"+itoa(i+1)+" "+strings.TrimSpace(line))
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("an address made of a page's text as it is:\n%s", strings.Join(offenders, "\n"))
	}
	for _, old := range []string{
		`a.setAttribute('href', url + '?' + q.toString());`,
		`if (f && t) f.setAttribute('action', t.getAttribute('data-card-url') + encodeURIComponent(menuChecklist) + '/delete');`,
		`form.setAttribute('action', '/b/' + form.dataset.board + '/cards/' + form.dataset.card + '/checklists/' + encodeURIComponent(menuChecklist) + '/' + t.getAttribute('data-path'));`,
	} {
		if encoded(old) {
			t.Errorf("the scan misses the old shape %s", old)
		}
	}
	pug, _ := os.ReadFile("templates/board.pug")
	if strings.Contains(string(pug), "data-card-url") {
		t.Error("negative: a whole address in an attribute, for the script to use as it is")
	}
}
