package web

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// A string-template custom field's display, as Node.js WeKan's
// models/lib/customFieldStringTemplate.js: each non-blank value of the field
// put through its format, joined by its separator. A format names only
// %{value} or {%value} and the card's, board's, list's and swimlane's titles
// the same way, optionally |urlencode, or applies a ${"regex": ..., "flags":
// ..., "replace": ...} - never a document's other properties. Anything else,
// or a malformed token, stays visible as it was written.
//
// The regex runs on Go's RE2, which never backtracks, so a catastrophic
// pattern - WeKan's ReDoS of 2026-10-02 - cannot stall Weddy, and no time
// limit is needed. RE2 has no lookaround or backreference; a pattern using
// them does not compile, and its token stays as written, as WeKan leaves a
// pattern it cannot run.

var templateTokens = regexp.MustCompile(`\$\{(?:[^"{}]|"(?:\\.|[^"\\])*")*\}|%\{[^{}]*\}|\{%[^{}]*\}`)

var templateContextKeys = map[string]bool{"card.title": true, "board.title": true, "list.title": true, "swimlane.title": true}

const (
	maxTemplateRegex = 200
	maxTemplateValue = 10000
)

// encodeURIComponent is JavaScript's: everything but A-Z a-z 0-9 - _ . ! ~ * ' ( ).
func encodeURIComponent(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.IndexByte("-_.!~*'()", c) >= 0 {
			b.WriteByte(c)
		} else {
			b.WriteString("%" + strings.ToUpper(strconv.FormatInt(int64(c)|0x100, 16)[1:]))
		}
	}
	return b.String()
}

// jsReplacement is a JavaScript replacement string - $&, $1..$99, $$ - for one match.
func jsReplacement(repl, value string, m []int) string {
	var b strings.Builder
	for i := 0; i < len(repl); i++ {
		if repl[i] != '$' || i+1 >= len(repl) {
			b.WriteByte(repl[i])
			continue
		}
		next := repl[i+1]
		switch {
		case next == '$':
			b.WriteByte('$')
			i++
		case next == '&':
			b.WriteString(value[m[0]:m[1]])
			i++
		case next >= '0' && next <= '9':
			// Two digits when that group exists, as JavaScript reads them.
			n, width := int(next-'0'), 1
			if i+2 < len(repl) && repl[i+2] >= '0' && repl[i+2] <= '9' {
				if two := n*10 + int(repl[i+2]-'0'); two >= 1 && two < len(m)/2 {
					n, width = two, 2
				}
			}
			if n >= 1 && n < len(m)/2 {
				if m[2*n] >= 0 {
					b.WriteString(value[m[2*n]:m[2*n+1]])
				}
				i += width
			} else {
				b.WriteByte('$')
			}
		default:
			b.WriteByte('$')
		}
	}
	return b.String()
}

// templateRegexReplace is String.prototype.replace with new RegExp(regex,
// flags): flag g replaces every match, i, m and s are RE2's own. ok is false
// for flags or a pattern it cannot run.
func templateRegexReplace(value, pattern, flags, repl string) (string, bool) {
	prefix, global := "", false
	for _, f := range flags {
		switch f {
		case 'g':
			global = true
		case 'i', 'm', 's':
			prefix += string(f)
		case 'u':
		default:
			return "", false
		}
	}
	if prefix != "" {
		prefix = "(?" + prefix + ")"
	}
	re, err := regexp.Compile(prefix + pattern)
	if err != nil {
		return "", false
	}
	var b strings.Builder
	last := 0
	for _, m := range re.FindAllStringSubmatchIndex(value, -1) {
		b.WriteString(value[last:m[0]])
		b.WriteString(jsReplacement(repl, value, m))
		last = m[1]
		if !global {
			break
		}
	}
	b.WriteString(value[last:])
	return b.String(), true
}

// formatStringTemplate is WeKan's formatStringTemplate.
func formatStringTemplate(raw any, format, separator string, context map[string]string) string {
	values, ok := raw.([]any)
	if !ok {
		return ""
	}
	var out []string
	for _, v := range values {
		value, ok := v.(string)
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}
		out = append(out, templateTokens.ReplaceAllStringFunc(format, func(token string) string {
			if strings.HasPrefix(token, "${") {
				var spec map[string]any
				if json.Unmarshal([]byte(token[1:]), &spec) == nil {
					regex, rok := spec["regex"].(string)
					repl, pok := spec["replace"].(string)
					flags, fok := spec["flags"].(string)
					if _, has := spec["flags"]; !has {
						fok = true
					}
					if !rok || !pok || !fok {
						return token
					}
					if len([]rune(regex)) > maxTemplateRegex || len([]rune(value)) > maxTemplateValue {
						return token
					}
					if res, ok := templateRegexReplace(value, regex, flags, repl); ok {
						return res
					}
					return token
				}
				return token // not a regex object: WeKan leaves it as written
			}
			parts := strings.Split(strings.ToLower(token[2:len(token)-1]), "|")
			name := parts[0]
			if len(parts) > 2 || (len(parts) == 2 && parts[1] != "urlencode") {
				return token
			}
			var result string
			switch {
			case name == "value":
				result = value
			case templateContextKeys[name]:
				r, has := context[name]
				if !has {
					return token
				}
				result = r
			default:
				return token
			}
			if len(parts) == 2 {
				if !utf8.ValidString(result) {
					return token // JavaScript's encodeURIComponent throws on a lone surrogate
				}
				return encodeURIComponent(result)
			}
			return result
		}))
	}
	return strings.Join(out, separator)
}
