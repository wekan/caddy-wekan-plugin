package web

import (
	"bytes"
	"html"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	east "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// Markdown as Node.js WeKan's exports read it (models/lib/exportMarkdown.js):
// blocks of styled runs, parsed with markdown-it's options - html, linkify,
// typographer and breaks. goldmark parses; its tree is turned into
// markdown-it's token stream - text runs between the other tokens, escapes
// and entities as text_special, nesting levels - and markdown-it's own
// typographer rules (replacements and smartquotes) are applied to it, so the
// exports agree with WeKan's about what is emphasis, a list, a link and a
// curly quote. TestExportMarkdownIsWeKans checks it against WeKan.

// mdRun is a run: { text, bold, italic, code, strike, link }.
type mdRun struct {
	Text   string `json:"text"`
	Bold   bool   `json:"bold"`
	Italic bool   `json:"italic"`
	Code   bool   `json:"code"`
	Strike bool   `json:"strike"`
	Link   string `json:"link"`
}

// mdBlock is a block of markdownBlocks. Quote and Index are pointers, as
// WeKan's blocks have them only where they set them.
type mdBlock struct {
	Type     string  `json:"type"`
	Level    int     `json:"level"`
	Runs     []mdRun `json:"runs,omitempty"`
	Text     *string `json:"text,omitempty"`
	Language *string `json:"language,omitempty"`
	Quote    *int    `json:"quote,omitempty"`
	Index    *int    `json:"index,omitempty"`
	hasRuns  bool
}

// MarshalJSON writes a block's own keys, as WeKan's: a list item's runs even
// when it has none.
func (b mdBlock) MarshalJSON() ([]byte, error) {
	o := jsObject{}
	o.set("type", b.Type)
	o.set("level", b.Level)
	switch b.Type {
	case "code":
		o.set("text", *b.Text)
		o.set("language", *b.Language)
	case "rule":
	default:
		o.set("runs", orEmpty(b.Runs))
		if b.Quote != nil {
			o.set("quote", *b.Quote)
		}
		if b.Index != nil {
			o.set("index", *b.Index)
		}
	}
	return o.MarshalJSON()
}

// mdTok is a markdown-it token.
type mdTok struct {
	typ, tag, content, href, info string
	level, start                  int
	hasStart                      bool
	children                      []*mdTok
}

// exportMarkdownParser is goldmark with markdown-it's strikethrough and
// tables, and its inline linkify rule (export_linkify.go) for linkify.
var exportMarkdownParser = goldmark.New(goldmark.WithExtensions(extension.Strikethrough, extension.Table),
	goldmark.WithParserOptions(parser.WithInlineParsers(util.Prioritized(mdLinkifyParser{}, 50))))

var (
	mdHTMLLinkOpen  = regexp.MustCompile(`(?i)^<a[>\s]`)
	mdHTMLLinkClose = regexp.MustCompile(`(?i)^</a\s*>`)
)

// mdPushRun is pushRun: adjacent runs of one style are one.
func mdPushRun(runs []mdRun, t string, style mdRun) []mdRun {
	if t == "" {
		return runs
	}
	if n := len(runs); n > 0 {
		last := &runs[n-1]
		if last.Bold == style.Bold && last.Italic == style.Italic && last.Code == style.Code && last.Strike == style.Strike && last.Link == style.Link {
			last.Text += t
			return runs
		}
	}
	style.Text = t
	return append(runs, style)
}

// mdRunsOf is runsOf: an inline token's children as runs.
func mdRunsOf(inline *mdTok) []mdRun {
	runs := []mdRun{}
	if inline == nil {
		return runs
	}
	var style mdRun
	linkDepth := 0
	for _, t := range inline.children {
		switch t.typ {
		case "strong_open":
			style.Bold = true
		case "strong_close":
			style.Bold = false
		case "em_open":
			style.Italic = true
		case "em_close":
			style.Italic = false
		case "s_open":
			style.Strike = true
		case "s_close":
			style.Strike = false
		case "link_open":
			linkDepth++
			style.Link = t.href
		case "link_close":
			linkDepth = max(0, linkDepth-1)
			if linkDepth == 0 {
				style.Link = ""
			}
		case "code_inline":
			s := style
			s.Code = true
			runs = mdPushRun(runs, t.content, s)
		case "softbreak", "hardbreak":
			runs = mdPushRun(runs, "\n", style)
		case "image":
			alt := t.content
			if alt == "" {
				alt = "image"
			}
			runs = mdPushRun(runs, "["+alt+"]", style)
		default:
			runs = mdPushRun(runs, t.content, style)
		}
	}
	return runs
}

// --- goldmark's tree as markdown-it's tokens

var mdEntity = regexp.MustCompile(`^&(?:#[xX][0-9a-fA-F]{1,6}|#[0-9]{1,7}|[A-Za-z][A-Za-z0-9]{1,31});`)

func mdASCIIPunct(c byte) bool {
	return c >= 0x21 && c <= 0x2f || c >= 0x3a && c <= 0x40 || c >= 0x5b && c <= 0x60 || c >= 0x7b && c <= 0x7e
}

type mdInline struct {
	src       []byte
	toks      []*mdTok
	pending   strings.Builder
	level     int
	linkLevel int
}

func (p *mdInline) flush() {
	if p.pending.Len() > 0 {
		p.toks = append(p.toks, &mdTok{typ: "text", content: p.pending.String(), level: p.level})
		p.pending.Reset()
	}
}

func (p *mdInline) push(t *mdTok) {
	p.flush()
	if strings.HasSuffix(t.typ, "_close") {
		p.level--
	}
	t.level = p.level
	if strings.HasSuffix(t.typ, "_open") {
		p.level++
	}
	p.toks = append(p.toks, t)
}

// text is raw source text: escapes and entities are markdown-it's
// text_special tokens, the rest pending text.
func (p *mdInline) text(raw string) {
	for i := 0; i < len(raw); {
		c := raw[i]
		if c == '\\' && i+1 < len(raw) && mdASCIIPunct(raw[i+1]) {
			p.push(&mdTok{typ: "text_special", content: string(raw[i+1])})
			i += 2
			continue
		}
		// markdown-it's escape rule takes the character after any other
		// backslash too, as one "\\x" token.
		if c == '\\' && i+1 < len(raw) && raw[i+1] != '\n' {
			_, n := utf8.DecodeRuneInString(raw[i+1:])
			p.push(&mdTok{typ: "text_special", content: raw[i : i+1+n]})
			i += 1 + n
			continue
		}
		if c == '&' {
			if m := mdEntity.FindString(raw[i:]); m != "" {
				if dec := html.UnescapeString(m); dec != m {
					p.push(&mdTok{typ: "text_special", content: dec})
					i += len(m)
					continue
				}
			}
		}
		p.pending.WriteByte(c)
		i++
	}
}

// lineBreak is markdown-it's newline rule: the spaces before it trimmed.
func (p *mdInline) lineBreak(hard bool) {
	s := strings.TrimRight(p.pending.String(), " ")
	p.pending.Reset()
	p.pending.WriteString(s)
	if hard {
		p.push(&mdTok{typ: "hardbreak"})
	} else {
		p.push(&mdTok{typ: "softbreak"})
	}
}

func mdPlainText(n ast.Node, src []byte) string {
	var b strings.Builder
	_ = ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch t := c.(type) {
		case *ast.Text:
			b.Write(t.Segment.Value(src))
		case *ast.String:
			b.Write(t.Value)
		}
		return ast.WalkContinue, nil
	})
	return b.String()
}

var mdBadProtocol = regexp.MustCompile(`(?i)^(vbscript|javascript|file|data):`)
var mdGoodData = regexp.MustCompile(`(?i)^data:image/(gif|png|jpeg|webp);`)

func (p *mdInline) node(n ast.Node) {
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		switch t := c.(type) {
		case *ast.Text:
			p.text(string(t.Segment.Value(p.src)))
			if t.SoftLineBreak() || t.HardLineBreak() {
				p.lineBreak(t.HardLineBreak())
			}
		case *ast.String:
			p.pending.Write(t.Value)
		case *ast.Emphasis:
			kind := "em"
			if t.Level == 2 {
				kind = "strong"
			}
			p.push(&mdTok{typ: kind + "_open"})
			p.node(t)
			p.push(&mdTok{typ: kind + "_close"})
		case *east.Strikethrough:
			// markdown-it strikes ~~text~~ only; goldmark ~text~ too, which
			// stays text, tildes and all.
			if !mdDoubleTilde(t, p.src) {
				p.pending.WriteString("~")
				p.node(t)
				p.pending.WriteString("~")
				continue
			}
			p.push(&mdTok{typ: "s_open"})
			p.node(t)
			p.push(&mdTok{typ: "s_close"})
		case *ast.CodeSpan:
			var b strings.Builder
			for k := t.FirstChild(); k != nil; k = k.NextSibling() {
				if tx, ok := k.(*ast.Text); ok {
					b.Write(tx.Segment.Value(p.src))
				} else if s, ok := k.(*ast.String); ok {
					b.Write(s.Value)
				}
			}
			p.push(&mdTok{typ: "code_inline", content: b.String()})
		case *ast.Link:
			dest := string(t.Destination)
			if mdBadProtocol.MatchString(dest) && !mdGoodData.MatchString(dest) {
				// validateLink refuses it: the source stays text.
				p.pending.WriteString("[")
				p.node(t)
				p.pending.WriteString("](" + dest + ")")
				continue
			}
			p.push(&mdTok{typ: "link_open", href: mdNormalizeLink(dest)})
			p.linkLevel++
			p.node(t)
			p.linkLevel--
			p.push(&mdTok{typ: "link_close"})
		case *ast.AutoLink:
			// markdown-it's autolink rule: <url> and <e-mail>.
			url := string(t.Label(p.src))
			href := url
			if t.AutoLinkType == ast.AutoLinkEmail {
				href = "mailto:" + url
			}
			full := mdNormalizeLink(href)
			if !mdValidateLink(full) {
				p.pending.WriteString("<" + url + ">")
				continue
			}
			p.push(&mdTok{typ: "link_open", href: full, info: "auto"})
			p.pending.WriteString(mdNormalizeLinkText(url))
			p.push(&mdTok{typ: "link_close", info: "auto"})
		case *mdLinkifyNode:
			// The inline rule does not run inside a link's text.
			if p.linkLevel > 0 {
				p.pending.WriteString(t.raw)
				continue
			}
			p.push(&mdTok{typ: "link_open", href: t.href, info: "auto"})
			p.pending.WriteString(t.label)
			p.push(&mdTok{typ: "link_close", info: "auto"})
		case *ast.Image:
			p.push(&mdTok{typ: "image", content: mdPlainText(t, p.src)})
		case *ast.RawHTML:
			var b strings.Builder
			for i := 0; i < t.Segments.Len(); i++ {
				seg := t.Segments.At(i)
				b.Write(seg.Value(p.src))
			}
			p.push(&mdTok{typ: "html_inline", content: b.String()})
			// An <a> is a link: the inline linkify rule waits for its end.
			if mdHTMLLinkOpen.MatchString(b.String()) {
				p.linkLevel++
			}
			if mdHTMLLinkClose.MatchString(b.String()) {
				p.linkLevel--
			}
		default:
			p.node(c)
		}
	}
}

// mdDoubleTilde is whether a strikethrough's text is opened by "~~".
func mdDoubleTilde(n ast.Node, src []byte) bool {
	for c := n.FirstChild(); c != nil; c = c.FirstChild() {
		if t, ok := c.(*ast.Text); ok {
			start := t.Segment.Start
			return start >= 2 && src[start-1] == '~' && src[start-2] == '~'
		}
	}
	return true
}

func mdLines(n ast.Node, src []byte) string {
	var b bytes.Buffer
	lines := n.Lines()
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		b.Write(seg.Value(src))
	}
	return b.String()
}

func mdInlineTok(n ast.Node, src []byte) *mdTok {
	p := &mdInline{src: src}
	p.node(n)
	p.flush()
	tok := &mdTok{typ: "inline", content: strings.TrimRight(mdLines(n, src), "\n"), children: p.toks}
	mdCoreLinkify(tok)
	mdReplacements(tok)
	mdSmartquotes(tok)
	return tok
}

func mdBlockToks(n ast.Node, src []byte, out []*mdTok) []*mdTok {
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		switch t := c.(type) {
		case *ast.Heading:
			out = append(out, &mdTok{typ: "heading_open", tag: "h" + strconv.Itoa(t.Level)}, mdInlineTok(t, src), &mdTok{typ: "heading_close"})
		case *ast.Paragraph, *ast.TextBlock:
			out = append(out, &mdTok{typ: "paragraph_open"}, mdInlineTok(t, src), &mdTok{typ: "paragraph_close"})
		case *ast.List:
			if t.IsOrdered() {
				out = append(out, &mdTok{typ: "ordered_list_open", start: t.Start, hasStart: t.Start != 1})
				out = mdBlockToks(t, src, out)
				out = append(out, &mdTok{typ: "ordered_list_close"})
			} else {
				out = append(out, &mdTok{typ: "bullet_list_open"})
				out = mdBlockToks(t, src, out)
				out = append(out, &mdTok{typ: "bullet_list_close"})
			}
		case *ast.ListItem:
			out = append(out, &mdTok{typ: "list_item_open"})
			out = mdBlockToks(t, src, out)
			out = append(out, &mdTok{typ: "list_item_close"})
		case *ast.Blockquote:
			out = append(out, &mdTok{typ: "blockquote_open"})
			out = mdBlockToks(t, src, out)
			out = append(out, &mdTok{typ: "blockquote_close"})
		case *ast.FencedCodeBlock:
			info := ""
			if t.Info != nil {
				info = string(t.Info.Segment.Value(src))
			}
			out = append(out, &mdTok{typ: "fence", content: mdLines(t, src), info: info})
		case *ast.CodeBlock:
			out = append(out, &mdTok{typ: "code_block", content: mdLines(t, src)})
		case *ast.ThematicBreak:
			out = append(out, &mdTok{typ: "hr"})
		case *east.Table:
			// markdown-it's table tokens: markdownBlocks draws none.
		case *ast.HTMLBlock:
			content := mdLines(t, src)
			if t.HasClosure() {
				content += string(t.ClosureLine.Value(src))
			}
			out = append(out, &mdTok{typ: "html_block", content: content})
		default:
			out = mdBlockToks(c, src, out)
		}
	}
	return out
}

// markdownBlocks is markdownBlocks.
func markdownBlocks(source string) []mdBlock {
	if jsTrim(source) == "" {
		return []mdBlock{}
	}
	src := []byte(source)
	doc := exportMarkdownParser.Parser().Parse(text.NewReader(src))
	tokens := mdBlockToks(doc, src, nil)
	intp := func(n int) *int { return &n }
	strp := func(s string) *string { return &s }
	var blocks []mdBlock
	listDepth, quoteDepth, pending := -1, 0, false
	var ordered []*int
	for i := 0; i < len(tokens); i++ {
		t := tokens[i]
		switch t.typ {
		case "heading_open":
			lv, _ := strconv.Atoi(t.tag[1:])
			if lv == 0 {
				lv = 1
			}
			blocks = append(blocks, mdBlock{Type: "heading", Level: lv, Runs: mdRunsOf(tokens[i+1]), hasRuns: true})
			i += 2
		case "paragraph_open":
			runs := mdRunsOf(tokens[i+1])
			if pending {
				if n := len(blocks); n > 0 && (blocks[n-1].Type == "bullet" || blocks[n-1].Type == "ordered") && len(blocks[n-1].Runs) == 0 {
					blocks[n-1].Runs = runs
				} else {
					blocks = append(blocks, mdBlock{Type: "paragraph", Level: max(0, listDepth) + 1, Runs: runs, Quote: intp(quoteDepth), hasRuns: true})
				}
				pending = false
			} else if len(runs) > 0 {
				blocks = append(blocks, mdBlock{Type: "paragraph", Level: max(0, listDepth), Runs: runs, Quote: intp(quoteDepth), hasRuns: true})
			}
			i += 2
		case "bullet_list_open":
			listDepth++
			ordered = append(ordered, nil)
		case "ordered_list_open":
			listDepth++
			start := 1
			if t.hasStart {
				start = t.start
			}
			ordered = append(ordered, intp(start))
		case "bullet_list_close", "ordered_list_close":
			listDepth--
			ordered = ordered[:len(ordered)-1]
		case "list_item_open":
			isOrdered := len(ordered) > 0 && ordered[len(ordered)-1] != nil
			b := mdBlock{Type: "bullet", Level: max(0, listDepth), Runs: []mdRun{}, Quote: intp(quoteDepth), hasRuns: true}
			if isOrdered {
				b.Type = "ordered"
				b.Index = intp(*ordered[len(ordered)-1])
				*ordered[len(ordered)-1]++
			}
			blocks = append(blocks, b)
			pending = true
		case "blockquote_open":
			quoteDepth++
		case "blockquote_close":
			quoteDepth = max(0, quoteDepth-1)
		case "fence", "code_block":
			blocks = append(blocks, mdBlock{Type: "code", Level: max(0, listDepth), Text: strp(strings.TrimSuffix(t.content, "\n")), Language: strp(jsTrim(t.info))})
		case "hr":
			blocks = append(blocks, mdBlock{Type: "rule"})
		case "html_block":
			blocks = append(blocks, mdBlock{Type: "paragraph", Level: max(0, listDepth), Runs: []mdRun{{Text: jsTrim(t.content)}}, Quote: intp(quoteDepth), hasRuns: true})
		}
	}
	out := []mdBlock{}
	for _, b := range blocks {
		if b.Type != "paragraph" || len(b.Runs) > 0 {
			out = append(out, b)
		}
	}
	return out
}

// markdownRuns is markdownRuns: the whole source as one list of runs.
func markdownRuns(source string) []mdRun {
	runs := []mdRun{}
	for i, b := range markdownBlocks(source) {
		if i > 0 {
			runs = mdPushRun(runs, "\n", mdRun{})
		}
		indent := strings.Repeat("    ", b.Level)
		switch b.Type {
		case "bullet":
			runs = mdPushRun(runs, indent+"• ", mdRun{})
		case "ordered":
			runs = mdPushRun(runs, indent+strconv.Itoa(*b.Index)+". ", mdRun{})
		case "rule":
			runs = mdPushRun(runs, "———", mdRun{})
			continue
		case "code":
			runs = mdPushRun(runs, *b.Text, mdRun{Code: true})
			continue
		}
		if b.Quote != nil && *b.Quote > 0 {
			runs = mdPushRun(runs, strings.Repeat("> ", *b.Quote), mdRun{Italic: true})
		}
		for _, r := range b.Runs {
			if b.Type == "heading" {
				r.Bold = true
			}
			runs = mdPushRun(runs, r.Text, r)
		}
	}
	return runs
}

// markdownPlainText is markdownPlainText.
func markdownPlainText(source string) string {
	var b strings.Builder
	for _, r := range markdownRuns(source) {
		b.WriteString(r.Text)
	}
	return b.String()
}

// --- markdown-it's typographer rules, on its tokens

var (
	mdRareRE      = regexp.MustCompile(`\+-|\.\.|\?\?\?\?|!!!!|,,|--`)
	mdScopedRE    = regexp.MustCompile(`(?i)\((c|tm|r)\)`)
	mdScopedAbbr  = map[string]string{"c": "©", "r": "®", "tm": "™"}
	mdEllipsis    = regexp.MustCompile(`\.{2,}`)
	mdPunctEllips = regexp.MustCompile(`([?!])…`)
	mdManyPunct   = regexp.MustCompile(`([?!]){4,}`)
	mdManyComma   = regexp.MustCompile(`,{2,}`)
)

// mdReplaceDashes is replace_rare's dashes: lookaheads JavaScript has and
// Go's regexp has not, so by hand, with ^ and $ per line (/gm).
func mdReplaceDashes(s string) string {
	r := []rune(s)
	var out []rune
	isSpace := func(c rune) bool { return mdJSSpaceRune(c) }
	// em-dash: (^|[^-])---(?=[^-]|$)
	for i := 0; i < len(r); {
		if i+3 <= len(r) && string(r[i:i+3]) == "---" && (i == 0 || r[i-1] != '-' || r[i-1] == '\n') && (i+3 == len(r) || r[i+3] != '-') {
			out = append(out, '—')
			i += 3
			continue
		}
		out = append(out, r[i])
		i++
	}
	r, out = out, nil
	// en-dash: (^|\s)--(?=\s|$)
	for i := 0; i < len(r); {
		if i+2 <= len(r) && r[i] == '-' && r[i+1] == '-' && (i == 0 || isSpace(r[i-1])) && (i+2 == len(r) || isSpace(r[i+2])) {
			out = append(out, '–')
			i += 2
			continue
		}
		out = append(out, r[i])
		i++
	}
	r, out = out, nil
	// en-dash: (^|[^-\s])--(?=[^-\s]|$)
	for i := 0; i < len(r); {
		if i+2 <= len(r) && r[i] == '-' && r[i+1] == '-' && (i == 0 || r[i-1] == '\n' || (r[i-1] != '-' && !isSpace(r[i-1]))) &&
			(i+2 == len(r) || r[i+2] == '\n' || (r[i+2] != '-' && !isSpace(r[i+2]))) {
			out = append(out, '–')
			i += 2
			continue
		}
		out = append(out, r[i])
		i++
	}
	return string(out)
}

func mdJSSpaceRune(c rune) bool { return jsSpace(c) || c == ' ' || c == ' ' }

// mdReplacements is the replacements rule.
func mdReplacements(inline *mdTok) {
	scoped, rare := mdScopedRE.MatchString(inline.content), mdRareRE.MatchString(inline.content)
	inAuto := 0
	for i := len(inline.children) - 1; i >= 0; i-- {
		t := inline.children[i]
		if t.typ == "text" && inAuto == 0 {
			if scoped {
				t.content = mdScopedRE.ReplaceAllStringFunc(t.content, func(m string) string { return mdScopedAbbr[strings.ToLower(m[1:len(m)-1])] })
			}
		}
		if t.typ == "link_open" && t.info == "auto" {
			inAuto--
		}
		if t.typ == "link_close" && t.info == "auto" {
			inAuto++
		}
	}
	if !rare {
		return
	}
	inAuto = 0
	for i := len(inline.children) - 1; i >= 0; i-- {
		t := inline.children[i]
		if t.typ == "text" && inAuto == 0 && mdRareRE.MatchString(t.content) {
			s := strings.ReplaceAll(t.content, "+-", "±")
			s = mdEllipsis.ReplaceAllString(s, "…")
			s = mdPunctEllips.ReplaceAllString(s, "$1..")
			s = mdManyPunct.ReplaceAllStringFunc(s, func(m string) string { return strings.Repeat(m[len(m)-1:], 3) })
			s = mdManyComma.ReplaceAllString(s, ",")
			t.content = mdReplaceDashes(s)
		}
		if t.typ == "link_open" && t.info == "auto" {
			inAuto--
		}
		if t.typ == "link_close" && t.info == "auto" {
			inAuto++
		}
	}
}

func mdIsWhiteSpace(c uint16) bool {
	if c >= 8192 && c <= 8202 {
		return true
	}
	switch c {
	case 9, 10, 11, 12, 13, 32, 160, 5760, 8239, 8287, 12288:
		return true
	}
	return false
}

func mdIsPunct(c uint16) bool {
	if c < 128 && mdASCIIPunct(byte(c)) {
		return true
	}
	if utf16.IsSurrogate(rune(c)) {
		return false
	}
	return unicode.IsPunct(rune(c)) || unicode.IsSymbol(rune(c))
}

// mdSmartquotes is the smartquotes rule (process_inlines), in UTF-16 as
// JavaScript reads it.
func mdSmartquotes(inline *mdTok) {
	if !strings.ContainsAny(inline.content, `'"`) {
		return
	}
	toks := inline.children
	type opener struct {
		tok, pos, level, prev int
		single                bool
	}
	type repl struct {
		pos int
		ch  string
	}
	var stack []opener
	headSingle, headDouble := -1, -1
	replacements := map[int][]repl{}
	truncate := func(n int) {
		for len(stack) > n {
			it := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if it.single {
				headSingle = it.prev
			} else {
				headDouble = it.prev
			}
		}
	}
	u16 := func(s string) []uint16 { return utf16.Encode([]rune(s)) }
	for i, t := range toks {
		j := len(stack) - 1
		for ; j >= 0; j-- {
			if stack[j].level <= t.level {
				break
			}
		}
		truncate(j + 1)
		if t.typ != "text" {
			continue
		}
		txt := u16(t.content)
	outer:
		for pos := 0; pos < len(txt); {
			k := pos
			for k < len(txt) && txt[k] != '\'' && txt[k] != '"' {
				k++
			}
			if k >= len(txt) {
				break
			}
			canOpen, canClose := true, true
			pos = k + 1
			single := txt[k] == '\''
			last := uint16(32)
			if k-1 >= 0 {
				last = txt[k-1]
			} else {
				for j := i - 1; j >= 0; j-- {
					if toks[j].typ == "softbreak" || toks[j].typ == "hardbreak" {
						break
					}
					if toks[j].content == "" {
						continue
					}
					c := u16(toks[j].content)
					last = c[len(c)-1]
					break
				}
			}
			next := uint16(32)
			if pos < len(txt) {
				next = txt[pos]
			} else {
				for j := i + 1; j < len(toks); j++ {
					if toks[j].typ == "softbreak" || toks[j].typ == "hardbreak" {
						break
					}
					if toks[j].content == "" {
						continue
					}
					next = u16(toks[j].content)[0]
					break
				}
			}
			lastP, nextP := mdIsPunct(last), mdIsPunct(next)
			lastW, nextW := mdIsWhiteSpace(last), mdIsWhiteSpace(next)
			if nextW {
				canOpen = false
			} else if nextP && !(lastW || lastP) {
				canOpen = false
			}
			if lastW {
				canClose = false
			} else if lastP && !(nextW || nextP) {
				canClose = false
			}
			if next == '"' && txt[k] == '"' && last >= '0' && last <= '9' {
				canClose, canOpen = false, false
			}
			if canOpen && canClose {
				canOpen, canClose = lastP, nextP
			}
			if !canOpen && !canClose {
				if single {
					replacements[i] = append(replacements[i], repl{k, "’"})
				}
				continue
			}
			if canClose {
				h := headDouble
				if single {
					h = headSingle
				}
				if h >= 0 && stack[h].level == t.level {
					it := stack[h]
					open, close := "“", "”"
					if single {
						open, close = "‘", "’"
					}
					replacements[i] = append(replacements[i], repl{k, close})
					replacements[it.tok] = append(replacements[it.tok], repl{it.pos, open})
					truncate(h)
					continue outer
				}
			}
			if canOpen {
				if len(stack) >= 1000 {
					return
				}
				prev := headDouble
				if single {
					prev = headSingle
				}
				stack = append(stack, opener{tok: i, pos: k, level: t.level, prev: prev, single: single})
				if single {
					headSingle = len(stack) - 1
				} else {
					headDouble = len(stack) - 1
				}
			} else if canClose && single {
				replacements[i] = append(replacements[i], repl{k, "’"})
			}
		}
	}
	for i, rs := range replacements {
		sort.SliceStable(rs, func(a, b int) bool { return rs[a].pos < rs[b].pos })
		txt := u16(toks[i].content)
		var out []uint16
		last := 0
		for _, r := range rs {
			out = append(out, txt[last:r.pos]...)
			out = append(out, u16(r.ch)...)
			last = r.pos + 1
		}
		out = append(out, txt[last:]...)
		toks[i].content = string(utf16.Decode(out))
	}
}
