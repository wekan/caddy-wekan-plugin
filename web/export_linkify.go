package web

import (
	"html"
	"sort"
	"strings"
	"sync"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"golang.org/x/net/idna"
)

// linkify-it 6, as markdown-it uses it (its defaults: no fuzzy links, fuzzy
// e-mail, no URL credentials), built from the same pattern sources on
// regexp2, which has JavaScript's lookarounds. Texts are matched as UTF-16
// code units, as JavaScript reads them, so uc.micro's surrogate pairs work.

type linkifyRE struct {
	schemaSearch, schemaAtStart, http, relative, mailto, mailHostSearch, mailName *regexp2.Regexp
}

var (
	linkifyOnce sync.Once
	linkifyRes  linkifyRE
)

// linkifyTlds is unpackTlds.
func linkifyTlds() []string {
	out := strings.Split("biz|com|edu|gov|net|org|pro|web|xxx|aero|asia|coop|info|museum|name|shop|рф", "|")
	for _, item := range strings.Split("a:cdefgilmnoqrstuwxz|b:abdefghijmnorstvwyz|c:acdfghiklmnoruvwxyz|d:ejkmoz|e:cegrstu|f:ijkmor|g:abdefghilmnpqrstuwy|h:kmnrtu|i:delmnoqrst|j:emop|k:eghimnprwyz|l:abcikrstuvy|m:acdeghklmnopqrstuvwxyz|n:acefgilopruz|o:m|p:aefghklmnrstwy|q:a|r:eosuw|s:abcdeghijklmnortuvxyz|t:cdfghjklmnortvwz|u:agksyz|v:aceginu|w:fs|y:et|z:amw", "|") {
		p := strings.Index(item, ":")
		for _, c := range item[p+1:] {
			out = append(out, item[:p]+string(c))
		}
	}
	return out
}

func linkifyCompile() linkifyRE {
	any_, cc, z, p := ucMicro["Any"], ucMicro["Cc"], ucMicro["Z"], ucMicro["P"]
	zpcc := z + "|" + p + "|" + cc
	zcc := z + "|" + cc
	seps := `[><｜]`
	pseudo := `(?:(?!` + seps + `|` + zpcc + `)` + any_ + `)`
	ip4 := `(?:(?:25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9][0-9]|[0-9])[.]){3}(?:25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9][0-9]|[0-9])`
	h16 := `[0-9A-Fa-f]{1,4}`
	ls32 := `(?:(?:` + h16 + `:` + h16 + `)|` + ip4 + `)`
	ip6 := `(?:(?:` + h16 + `:){6}` + ls32 + `|::(?:` + h16 + `:){5}` + ls32 + `|(?:` + h16 + `)?::(?:` + h16 + `:){4}` + ls32 +
		`|(?:(?:` + h16 + `:){0,1}` + h16 + `)?::(?:` + h16 + `:){3}` + ls32 + `|(?:(?:` + h16 + `:){0,2}` + h16 + `)?::(?:` + h16 + `:){2}` + ls32 +
		`|(?:(?:` + h16 + `:){0,3}` + h16 + `)?::` + h16 + `:` + ls32 + `|(?:(?:` + h16 + `:){0,4}` + h16 + `)?::` + ls32 +
		`|(?:(?:` + h16 + `:){0,5}` + h16 + `)?::` + h16 + `|(?:(?:` + h16 + `:){0,6}` + h16 + `)?::)`
	ip6URL := `\[` + ip6 + `\]`
	ip6Mail := `\[IPv6:` + ip6 + `\]`
	port := `(?::(?:6(?:[0-4]\d{3}|5(?:[0-4]\d{2}|5(?:[0-2]\d|3[0-5])))|[1-5]?\d{1,4}))?`
	hostTerm := `(?=$|` + seps + `|` + zpcc + `)(?!-|_|:\d|\.-|\.(?!$|` + zpcc + `))`
	pathTerm := zpcc + `|` + seps
	nested := func(open, close string) string {
		atom := `(?:(?!` + zcc + `|` + open + `|` + close + `).)`
		pair := open + atom + `{0,1000}` + close
		for level := 2; level <= 4; level++ {
			pair = open + `(?:` + atom + `|` + pair + `){0,1000}` + close
		}
		return pair
	}
	path := `(?:[/?#](?:` + nested(`\[`, `\]`) + `|` + nested(`\(`, `\)`) + `|` + nested(`\{`, `\}`) +
		`|\"(?:(?!` + zcc + `|["]).){1,100}\"|\'(?:(?!` + zcc + `|[']).){1,100}\'|\'(?=` + pseudo + `|[-])|\.{2,20}[:]?[a-zA-Z0-9%/&]|\.(?!` + zcc + `|[.]|$)|` +
		`\-{1,20}|` + `,(?!` + zcc + `|$)|;(?!` + zcc + `|$)|\!{1,20}(?!` + zcc + `|[!]|$)|\?(?!` + zcc + `|[?]|$)|` +
		`(?:)[\\/:%@#&=_~*]|(?!` + pathTerm + `).){1,10000}|\/)?`
	mailName := "[-!#$%&'*+/=?^_`{|}~a-zA-Z0-9](?:[-!#$%&'*+/=?^_`{|}~a-zA-Z0-9]|[.](?=[-!#$%&'*+/=?^_`{|}~a-zA-Z0-9])){0,63}"
	xn := `xn--[a-z0-9\-]{1,59}`
	tlds := linkifyTlds()
	sort.Slice(tlds, func(i, j int) bool { return utf16Less(tlds[i], tlds[j]) })
	var uniq []string
	for i, t := range tlds {
		if i == 0 || t != tlds[i-1] {
			uniq = append(uniq, t)
		}
	}
	for i, j := 0, len(uniq)-1; i < j; i, j = i+1, j-1 {
		uniq[i], uniq[j] = uniq[j], uniq[i]
	}
	_ = strings.Join(uniq, "|") // the fuzzy link host, unused: no fuzzy links
	domainRoot := `(?:` + xn + `|` + pseudo + `{1,63})`
	domain := `(?:` + xn + `|(?:` + pseudo + `)|(?:` + pseudo + `(?:-|` + pseudo + `){0,61}` + pseudo + `))`
	urlHostPort := `(?:` + ip6URL + `|(?:(?:(?:` + domain + `)\.){0,10}` + domain + `))` + port + hostTerm
	mailHost := `(?:` + ip6Mail + `|(?:(?:(?:` + domain + `)\.){0,4}` + domain + `))` + hostTerm
	fuzzyMailHost := `(?:` + ip6Mail + `|(?:(?:(?:` + domain + `)[.]){1,4}` + domainRoot + `))` + hostTerm
	schemaSearch := `(^|(?!_)(?:[><｜]|` + zpcc + `))(http:|https:|ftp:|//|mailto:)`
	opt := regexp2.ECMAScript | regexp2.IgnoreCase
	return linkifyRE{
		schemaSearch:   regexp2.MustCompile(schemaSearch, opt),
		schemaAtStart:  regexp2.MustCompile(`^`+schemaSearch, opt),
		http:           regexp2.MustCompile(`^(?:\/\/`+urlHostPort+path+`)`, opt),
		relative:       regexp2.MustCompile(`^(?:(?:localhost|`+ip6URL+`|(?:(?:`+domain+`)[.]){1,10}`+domainRoot+`)`+port+hostTerm+path+`)`, opt),
		mailto:         regexp2.MustCompile(`^(?:`+mailName+`@`+mailHost+`)`, opt),
		mailHostSearch: regexp2.MustCompile(`@`+fuzzyMailHost, opt),
		mailName:       regexp2.MustCompile(`(?:^|`+seps+`|"|\(|`+zcc+`)(`+mailName+`)$`, regexp2.ECMAScript),
	}
}

func linkifyRegexps() *linkifyRE {
	linkifyOnce.Do(func() { linkifyRes = linkifyCompile() })
	return &linkifyRes
}

// utf16Less is JavaScript's default sort order.
func utf16Less(a, b string) bool {
	x, y := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(x) && i < len(y); i++ {
		if x[i] != y[i] {
			return x[i] < y[i]
		}
	}
	return len(x) < len(y)
}

// u16 is a text's UTF-16 code units, one a rune, for regexp2.
func u16(s string) []rune {
	u := utf16.Encode([]rune(s))
	r := make([]rune, len(u))
	for i, c := range u {
		r[i] = rune(c)
	}
	return r
}

func u16String(r []rune) string {
	u := make([]uint16, len(r))
	for i, c := range r {
		u[i] = uint16(c)
	}
	return string(utf16.Decode(u))
}

func reFind(re *regexp2.Regexp, r []rune, at int) *regexp2.Match {
	if at > len(r) {
		return nil
	}
	m, err := re.FindRunesMatchStartingAt(r, at)
	if err != nil {
		return nil
	}
	return m
}

// linkifyValidate is a schema's validate: the length after it, or 0.
func linkifyValidate(r []rune, schema string, pos int) int {
	re := linkifyRegexps()
	if end := pos + 10000; end < len(r) {
		r = r[:end]
	}
	// A sticky match at pos: the validators have no ^ of their own, so one
	// anchored at the start of the text from pos is the same.
	if pos > len(r) {
		return 0
	}
	at := r[pos:]
	switch strings.ToLower(schema) {
	case "http:", "https:", "ftp:":
		if m := reFind(re.http, at, 0); m != nil {
			return m.RuneLength
		}
	case "//":
		if m := reFind(re.relative, at, 0); m != nil {
			if pos >= 3 && (r[pos-3] == ':' || r[pos-3] == '/') {
				return 0
			}
			return m.RuneLength
		}
	case "mailto:":
		if m := reFind(re.mailto, at, 0); m != nil {
			return m.RuneLength
		}
	}
	return 0
}

type linkifyMatch struct {
	schema           string
	index, lastIndex int // UTF-16
	text, url        string
}

func linkifyNew(r []rune, schema string, index, last int) linkifyMatch {
	raw := u16String(r[index:last])
	m := linkifyMatch{schema: strings.ToLower(schema), index: index, lastIndex: last, text: raw, url: raw}
	if m.schema == "" {
		m.url = "http://" + m.url
	}
	if m.schema == "mailto:" && !strings.HasPrefix(strings.ToLower(m.url), "mailto:") {
		m.url = "mailto:" + m.url
	}
	return m
}

func linkifyMailName(r []rune, at int) (string, bool) {
	from := max(0, at-65)
	m := reFind(linkifyRegexps().mailName, r[from:at], 0)
	if m == nil {
		return "", false
	}
	return u16String(m.GroupByNumber(1).Runes()), true
}

// linkifyTest is LinkifyIt.test.
func linkifyTest(s string) bool {
	r := u16(s)
	if len(r) == 0 {
		return false
	}
	re := linkifyRegexps()
	for m := reFind(re.schemaSearch, r, 0); m != nil; m = reFind(re.schemaSearch, r, m.RuneIndex+max(m.RuneLength, 1)) {
		g := m.GroupByNumber(2)
		if linkifyValidate(r, u16String(g.Runes()), m.RuneIndex+m.RuneLength) > 0 {
			return true
		}
	}
	if strings.Contains(s, "@") {
		for m := reFind(re.mailHostSearch, r, 0); m != nil; m = reFind(re.mailHostSearch, r, m.RuneIndex+max(m.RuneLength, 1)) {
			if _, ok := linkifyMailName(r, m.RuneIndex); ok {
				return true
			}
		}
	}
	return false
}

// linkifyMatchAll is LinkifyIt.match, with no fuzzy links.
func linkifyMatchAll(s string) []linkifyMatch {
	r := u16(s)
	if len(r) == 0 {
		return nil
	}
	re := linkifyRegexps()
	type cand struct {
		schema           string
		index, lastIndex int
	}
	var result []linkifyMatch
	var email *cand
	var prefix *cand
	emailDone, schemaDone := false, false
	mailAt, schemaAt := 0, 0
	pos := 0
	for {
		scanFrom := max(pos-1, 0)
		if !emailDone && (email == nil || email.index < pos) {
			if mailAt < scanFrom {
				mailAt = scanFrom
			}
			for {
				m := reFind(re.mailHostSearch, r, mailAt)
				if m == nil {
					emailDone, email = true, nil
					break
				}
				mailAt = m.RuneIndex + max(m.RuneLength, 1)
				name, ok := linkifyMailName(r, m.RuneIndex)
				if !ok {
					continue
				}
				nl := len(utf16.Encode([]rune(name)))
				email = &cand{"mailto:", m.RuneIndex - nl, m.RuneIndex + m.RuneLength}
				if email.index >= pos {
					break
				}
				if mailAt < scanFrom {
					mailAt = scanFrom
				}
			}
		}
		fuzzy := email
		var schemaCand *cand
		if !schemaDone {
			for {
				if prefix == nil {
					if schemaAt < scanFrom {
						schemaAt = scanFrom
					}
					m := reFind(re.schemaSearch, r, schemaAt)
					if m == nil {
						schemaDone = true
						break
					}
					schemaAt = m.RuneIndex + max(m.RuneLength, 1)
					g1, g2 := m.GroupByNumber(1), m.GroupByNumber(2)
					prefix = &cand{u16String(g2.Runes()), m.RuneIndex + g1.RuneLength, m.RuneIndex + m.RuneLength}
				}
				if prefix.index < pos {
					prefix = nil
					continue
				}
				if fuzzy != nil && prefix.index > fuzzy.index {
					break
				}
				pr := prefix
				prefix = nil
				if n := linkifyValidate(r, pr.schema, pr.lastIndex); n > 0 {
					schemaCand = &cand{pr.schema, pr.index, pr.lastIndex + n}
					break
				}
			}
		}
		c := schemaCand
		if c == nil || email != nil && (email.index < c.index || email.index == c.index && email.lastIndex > c.lastIndex) {
			c = email
		}
		if c == nil {
			break
		}
		if c == email {
			email = nil
		}
		result = append(result, linkifyNew(r, c.schema, c.index, c.lastIndex))
		pos = c.lastIndex
	}
	return result
}

// linkifyMatchAtStart is LinkifyIt.matchAtStart.
func linkifyMatchAtStart(s string) (linkifyMatch, bool) {
	r := u16(s)
	if len(r) == 0 {
		return linkifyMatch{}, false
	}
	m := reFind(linkifyRegexps().schemaAtStart, r, 0)
	if m == nil || m.RuneIndex != 0 {
		return linkifyMatch{}, false
	}
	g1, g2 := m.GroupByNumber(1), m.GroupByNumber(2)
	n := linkifyValidate(r, u16String(g2.Runes()), m.RuneLength)
	if n == 0 {
		return linkifyMatch{}, false
	}
	return linkifyNew(r, u16String(g2.Runes()), m.RuneIndex+g1.RuneLength, m.RuneIndex+m.RuneLength+n), true
}

// --- markdown-it's link normalizing

var mdRecodeHost = map[string]bool{"http:": true, "https:": true, "mailto:": true}

// mdURLHost is where mdurl.parse finds a URL's hostname: [start, end), or
// -1 with none, and its protocol.
func mdURLHost(u string) (string, int, int) {
	proto := ""
	rest := u
	if i := strings.Index(rest, ":"); i > 0 && isURLScheme(rest[:i]) {
		proto = rest[:i+1]
		rest = rest[i+1:]
	}
	start := len(u) - len(rest)
	slashes := strings.HasPrefix(rest, "//")
	lp := strings.ToLower(proto)
	if slashes && lp != "javascript:" {
		start += 2
		rest = rest[2:]
	} else if !(proto != "" && !map[string]bool{"http:": true, "https:": true, "ftp:": true, "gopher:": true, "file:": true}[lp]) {
		return proto, -1, -1
	}
	end := len(rest)
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		end = i
	}
	host := rest[:end]
	if at := strings.LastIndex(host, "@"); at >= 0 {
		start += at + 1
		host = host[at+1:]
	}
	if c := strings.LastIndex(host, ":"); c >= 0 && !strings.Contains(host, "]") {
		host = host[:c]
	}
	return proto, start, start + len(host)
}

func isURLScheme(s string) bool {
	for i, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && (c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.')) {
			return false
		}
	}
	return s != ""
}

// mdNormalizeLink is normalizeLink: the host in punycode, then mdurl.encode.
func mdNormalizeLink(u string) string {
	proto, s, e := mdURLHost(u)
	if s >= 0 && e > s && (proto == "" || mdRecodeHost[proto]) && !strings.HasPrefix(u[s:e], "[") {
		if a, err := idna.Punycode.ToASCII(u[s:e]); err == nil {
			u = u[:s] + a + u[e:]
		}
	}
	const keep = ";/?:@&=+$,-_.!~*'()#"
	var b strings.Builder
	for i := 0; i < len(u); i++ {
		c := u[i]
		switch {
		// An IPv6 host keeps its brackets, as markdown-it's does.
		case s >= 0 && i >= s && i < e && (c == '[' || c == ']'):
			b.WriteByte(c)
		case c == '%' && i+2 < len(u) && isHex(u[i+1]) && isHex(u[i+2]):
			b.WriteString(u[i : i+3])
			i += 2
		case c < 0x80 && (c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte(keep, c) >= 0):
			b.WriteByte(c)
		default:
			b.WriteString("%" + strings.ToUpper(hexByte(c)))
		}
	}
	return b.String()
}

func hexByte(c byte) string {
	const h = "0123456789abcdef"
	return string([]byte{h[c>>4], h[c&15]})
}

func isHex(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }

// mdNormalizeLinkText is normalizeLinkText: the host from punycode, then
// mdurl.decode keeping ;/?:@&=+$,#%.
func mdNormalizeLinkText(u string) string {
	if proto, s, e := mdURLHost(u); s >= 0 && e > s && (proto == "" || mdRecodeHost[proto]) {
		if a, err := idna.Punycode.ToUnicode(u[s:e]); err == nil {
			u = u[:s] + a + u[e:]
		}
	}
	const keep = ";/?:@&=+$,#%"
	var b strings.Builder
	for i := 0; i < len(u); {
		if u[i] != '%' || i+2 >= len(u) || !isHex(u[i+1]) || !isHex(u[i+2]) {
			b.WriteByte(u[i])
			i++
			continue
		}
		// A run of %XX: decoded where it is UTF-8, kept where it is not.
		var raw []byte
		j := i
		for j+2 < len(u) && u[j] == '%' && isHex(u[j+1]) && isHex(u[j+2]) {
			raw = append(raw, unhex(u[j+1])<<4|unhex(u[j+2]))
			j += 3
		}
		for k := 0; k < len(raw); {
			c := raw[k]
			if c < 0x80 {
				if strings.IndexByte(keep, c) >= 0 {
					b.WriteString(u[i+k*3 : i+k*3+3])
				} else {
					b.WriteByte(c)
				}
				k++
				continue
			}
			r, n := utf8.DecodeRune(raw[k:])
			if r == utf8.RuneError && n <= 1 {
				b.WriteString(u[i+k*3 : i+k*3+3])
				k++
				continue
			}
			b.WriteRune(r)
			k += n
		}
		i = j
	}
	return b.String()
}

func unhex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	}
	return c - 'A' + 10
}

// mdValidateLink is validateLink.
func mdValidateLink(u string) bool {
	s := strings.ToLower(jsTrim(u))
	if mdBadProtocol.MatchString(s) {
		return mdGoodData.MatchString(s)
	}
	return true
}

// --- the inline rule: a link at "://", taken out of the text before it

// mdLinkifyNode is the inline rule's link: href, and the text it shows.
type mdLinkifyNode struct {
	ast.BaseInline
	href, label, raw string
}

var kindMdLinkify = ast.NewNodeKind("MdLinkify")

func (n *mdLinkifyNode) Kind() ast.NodeKind            { return kindMdLinkify }
func (n *mdLinkifyNode) Dump(source []byte, level int) {}

type mdLinkifyParser struct{}

func (mdLinkifyParser) Trigger() []byte { return []byte{':'} }

func isSchemeChar(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.'
}

func (mdLinkifyParser) Parse(parent ast.Node, block text.Reader, pc parser.Context) ast.Node {
	line, seg := block.PeekLine()
	if len(line) < 3 || line[1] != '/' || line[2] != '/' {
		return nil
	}
	last, ok := parent.LastChild().(*ast.Text)
	if !ok || last.Segment.Stop != seg.Start || last.SoftLineBreak() || last.HardLineBreak() {
		return nil
	}
	src := block.Source()
	pos := seg.Start
	// markdown-it's pending text: what came after the text's last escape
	// or entity, which are tokens of their own.
	pendingStart := last.Segment.Start
	for i := last.Segment.Start; i < last.Segment.Stop; i++ {
		if src[i] == '\\' && i+1 < last.Segment.Stop {
			_, n := utf8.DecodeRune(src[i+1 : last.Segment.Stop])
			i += n
			pendingStart = i + 1
		} else if src[i] == '&' {
			if m := mdEntity.Find(src[i:last.Segment.Stop]); m != nil && html.UnescapeString(string(m)) != string(m) {
				i += len(m) - 1
				pendingStart = i + 1
			}
		}
	}
	pending := last.Segment.Stop - pendingStart
	protoMin := pos - min(10, pending, pos)
	protoStart := pos
	for protoStart > protoMin && isSchemeChar(src[protoStart-1]) {
		protoStart--
	}
	if protoStart == pos || !(src[protoStart] >= 'A' && src[protoStart] <= 'Z' || src[protoStart] >= 'a' && src[protoStart] <= 'z') {
		return nil
	}
	protoLen := pos - protoStart
	m, ok := linkifyMatchAtStart(string(src[protoStart:seg.Stop]))
	if !ok {
		return nil
	}
	url := m.url
	if len(url) <= protoLen {
		return nil
	}
	url = strings.TrimRight(url, "*")
	full := mdNormalizeLink(url)
	if !mdValidateLink(full) {
		return nil
	}
	last.Segment = last.Segment.WithStop(last.Segment.Stop - protoLen)
	if last.Segment.Len() == 0 {
		parent.RemoveChild(parent, last)
	}
	block.Advance(len(url) - protoLen)
	return &mdLinkifyNode{href: full, label: mdNormalizeLinkText(url), raw: url}
}

// mdCoreLinkify is the core linkify rule: links in the text tokens of an
// inline, outside the links it has.
func mdCoreLinkify(inline *mdTok) {
	if !linkifyTest(inline.content) {
		return
	}
	toks := inline.children
	type repl struct {
		index int
		nodes []*mdTok
	}
	var repls []repl
	htmlLink := 0
	for i := len(toks) - 1; i >= 0; i-- {
		t := toks[i]
		if t.typ == "link_close" {
			i--
			for i >= 0 && toks[i].level != t.level && toks[i].typ != "link_open" {
				i--
			}
			continue
		}
		if t.typ == "html_inline" {
			if mdHTMLLinkOpen.MatchString(t.content) && htmlLink > 0 {
				htmlLink--
			}
			if mdHTMLLinkClose.MatchString(t.content) {
				htmlLink++
			}
		}
		if htmlLink > 0 || t.typ != "text" || !linkifyTest(t.content) {
			continue
		}
		links := linkifyMatchAll(t.content)
		if len(links) > 0 && links[0].index == 0 && i > 0 && toks[i-1].typ == "text_special" {
			links = links[1:]
		}
		text := u16(t.content)
		var nodes []*mdTok
		level, lastPos := t.level, 0
		for _, l := range links {
			full := mdNormalizeLink(l.url)
			if !mdValidateLink(full) {
				continue
			}
			label := l.text
			switch {
			case l.schema == "":
				label = strings.TrimPrefix(mdNormalizeLinkText("http://"+label), "http://")
			case l.schema == "mailto:" && !strings.HasPrefix(strings.ToLower(label), "mailto:"):
				label = strings.TrimPrefix(mdNormalizeLinkText("mailto:"+label), "mailto:")
			default:
				label = mdNormalizeLinkText(label)
			}
			if l.index > lastPos {
				nodes = append(nodes, &mdTok{typ: "text", content: u16String(text[lastPos:l.index]), level: level})
			}
			nodes = append(nodes, &mdTok{typ: "link_open", href: full, info: "auto", level: level},
				&mdTok{typ: "text", content: label, level: level + 1},
				&mdTok{typ: "link_close", info: "auto", level: level})
			lastPos = l.lastIndex
		}
		if lastPos < len(text) {
			nodes = append(nodes, &mdTok{typ: "text", content: u16String(text[lastPos:]), level: level})
		}
		repls = append(repls, repl{i, nodes})
	}
	if len(repls) == 0 {
		return
	}
	var out []*mdTok
	ri := len(repls) - 1
	for i, t := range toks {
		if ri >= 0 && repls[ri].index == i {
			out = append(out, repls[ri].nodes...)
			ri--
			continue
		}
		out = append(out, t)
	}
	inline.children = out
}
