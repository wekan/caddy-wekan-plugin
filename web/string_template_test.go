package web

import (
	"strings"
	"testing"
	"time"
)

// WeKan's tests/customFieldStringTemplate.test.cjs, case for case, and its
// tests/templateRegexDos.test.cjs's pattern.
func TestFormatStringTemplate(t *testing.T) {
	ctx := map[string]string{"card.title": "Card", "board.title": "Board", "list.title": "List", "swimlane.title": "Lane"}
	for _, c := range []struct {
		raw              any
		format, sep, out string
		ctx              map[string]string
	}{
		{[]any{"one", "", " ", "two"}, "%{VALUE}", ", ", "one, two", nil},
		{nil, "%{value}", "", "", nil},
		{[]any{nil, 42.0, "x"}, "%{value}", "", "x", nil},
		{[]any{"a", "b"}, "%{card.title}/{%board.title}/%{LIST.TITLE}/{%swimlane.title}:%{value}", ";", "Card/Board/List/Lane:a;Card/Board/List/Lane:b", ctx},
		{[]any{"Ö & # ? / $&"}, "https://example.org/?v=%{value|urlencode}&c=%{card.title|urlencode}", "",
			"https://example.org/?v=%C3%96%20%26%20%23%20%3F%20%2F%20%24%26&c=Card%20%26%20title", map[string]string{"card.title": "Card & title"}},
		{[]any{"\xed\xa0\x80"}, "%{value|urlencode}", "", "%{value|urlencode}", nil},
		// Negative: unknown, missing and arbitrary paths stay literal.
		{[]any{"x"}, "%{card.secret} %{constructor} %{card.title.toUpperCase()} %{swimlane.title} %{value|eval} %{value|urlencode|eval}", "",
			"%{card.secret} %{constructor} %{card.title.toUpperCase()} %{swimlane.title} %{value|eval} %{value|urlencode|eval}", map[string]string{"card.secret": "secret"}},
		// Negative: what is inserted is not read again.
		{[]any{"%{card.title} $&"}, "%{value} / %{card.title}", "", "%{card.title} $& / %{value}", map[string]string{"card.title": "%{value}"}},
		{[]any{"aaaBB"}, `${"regex":"(a{2,3})","flags":"gi","replace":"[$1]"}`, "", "[aaa]BB", nil},
		{[]any{"abc"}, `${"regex":"abc","replace":"123"}`, "", "123", nil},
		{[]any{`a"b`}, `${"regex":"a\"b","replace":"ok"}`, "", "ok", nil},
		{[]any{"a-a-a"}, `${"regex":"a","replace":"b"}`, "", "b-a-a", nil}, // no g: the first only
		{[]any{"a-a-a"}, `${"regex":"a","flags":"g","replace":"<$&>"}`, "", "<a>-<a>-<a>", nil},
	} {
		if got := formatStringTemplate(c.raw, c.format, c.sep, c.ctx); got != c.out {
			t.Errorf("%q on %v: %q, want %q", c.format, c.raw, got, c.out)
		}
	}
	// Negative: a regex it cannot run stays visible.
	for _, spec := range []string{`${"regex":"[","replace":"x"}`, `${"regex":"a","flags":"bad","replace":"b"}`,
		`${"regex":"a"}`, `${bad}`, `${"regex":2,"replace":"b"}`, `${value}`, `${"regex":"(?<=a)b","replace":"x"}`} {
		if got := formatStringTemplate([]any{"a"}, spec, "", nil); got != spec {
			t.Errorf("negative: %q stays as written: %q", spec, got)
		}
	}
	// Negative: over the caps, the token stays; and a catastrophic pattern
	// (WeKan's ReDoS) answers at once on RE2.
	long := `${"regex":"` + strings.Repeat("a", maxTemplateRegex+1) + `","replace":"x"}`
	if got := formatStringTemplate([]any{"a"}, long, "", nil); got != long {
		t.Error("negative: a pattern over 200 characters is not run")
	}
	start := time.Now()
	_ = formatStringTemplate([]any{strings.Repeat("a", 5000) + "!"}, `${"regex":"(a+)+$","replace":"x"}`, "", nil)
	if time.Since(start) > time.Second {
		t.Errorf("negative: (a+)+$ takes %s", time.Since(start))
	}
}
