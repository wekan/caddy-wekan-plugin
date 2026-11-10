package web

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/dlclark/regexp2/v2"
	"golang.org/x/text/unicode/norm"
)

// The PDF of a card or a board, as Node.js WeKan's models/lib/pdfDocument.js
// writes it: the card document (export_card_document.go) as lines -
// documentToLines - paginated, and written by buildPdfBuffer in the four
// Courier faces with WinAnsiEncoding, Windows-1252 text, section bars,
// label columns and JPEG and PNG pictures. Text is measured, cut and wrapped
// in UTF-16 code units, as JavaScript's strings are.
// TestPdfDocumentIsWeKans compares the lines and the bytes with WeKan's.

const (
	pdfPageWidth  = 595
	pdfPageHeight = 842
	pdfMargin     = 50
	pdfLineHeight = 14
	pdfFontSize   = 10
	pdfTextWidth  = 90
)

type pdfRun struct {
	Text   string `json:"text"`
	Bold   bool   `json:"bold"`
	Italic bool   `json:"italic"`
}

// pdfLine is a line of the document: a plain string (IsString), or an object
// with only the keys it has, as WeKan's lines.
type pdfLine struct {
	IsString         bool
	Text             string
	HasText          bool
	Bold, Bar        bool
	HasBold          bool
	Runs             []pdfRun
	HasRuns          bool
	Span             int
	ImageRow         []cdImage
	ImageCaptions    []string
	LabelRow         []any
	LabelTitle       *string
	Progress         *cdProgress
	AttachmentCells  []any
	AttachmentHeader bool
}

func (l pdfLine) MarshalJSON() ([]byte, error) {
	if l.IsString {
		return json.Marshal(l.Text)
	}
	o := jsObject{}
	if l.LabelRow != nil {
		o.set("labelRow", l.LabelRow)
		o.set("labelTitle", *l.LabelTitle)
	}
	if l.Progress != nil {
		o.set("progress", l.Progress)
	}
	if l.AttachmentCells != nil {
		o.set("attachmentCells", l.AttachmentCells)
		if l.AttachmentHeader {
			o.set("attachmentHeader", true)
		}
	}
	if l.ImageRow != nil {
		o.set("imageRow", l.ImageRow)
		o.set("span", l.Span)
		o.set("imageCaptions", l.ImageCaptions)
	}
	if l.HasText {
		o.set("text", l.Text)
	}
	if l.HasBold {
		o.set("bold", l.Bold)
	}
	if l.Bar {
		o.set("bar", true)
	}
	if l.HasRuns {
		runs := l.Runs
		if runs == nil {
			runs = []pdfRun{}
		}
		o.set("runs", runs)
	}
	return o.MarshalJSON()
}

func pdfPlain(text string, bold bool) pdfLine {
	return pdfLine{Text: text, HasText: true, Bold: bold, HasBold: true}
}

func pdfBar(text string) pdfLine {
	return pdfLine{Text: text, HasText: true, Bold: true, HasBold: true, Bar: true}
}

func pdfRuns(runs []pdfRun) pdfLine { return pdfLine{Runs: runs, HasRuns: true} }

// --- UTF-16 strings, as JavaScript's

type u16s []uint16

func toU16(s string) u16s  { return utf16.Encode([]rune(s)) }
func (u u16s) str() string { return string(utf16.Decode(u)) }
func (u u16s) slice(a, b int) u16s {
	if a < 0 {
		a = 0
	}
	if b > len(u) {
		b = len(u)
	}
	if a >= b {
		return nil
	}
	return u[a:b]
}

func len16(s string) int { return len(toU16(s)) }

// slice16 is String.prototype.slice(a, b) on UTF-16 units.
func slice16(s string, a, b int) string { return toU16(s).slice(a, b).str() }

// --- Windows-1252

var cp1252High = map[rune]byte{'€': 0x80, '‚': 0x82, 'ƒ': 0x83, '„': 0x84, '…': 0x85, '†': 0x86, '‡': 0x87, 'ˆ': 0x88, '‰': 0x89,
	'Š': 0x8A, '‹': 0x8B, 'Œ': 0x8C, 'Ž': 0x8E, '‘': 0x91, '’': 0x92, '“': 0x93, '”': 0x94, '•': 0x95, '–': 0x96, '—': 0x97,
	'˜': 0x98, '™': 0x99, 'š': 0x9A, '›': 0x9B, 'œ': 0x9C, 'ž': 0x9E, 'Ÿ': 0x9F}

var pdfTransliterate = map[rune]string{'đ': "d", 'Đ': "D", 'ħ': "h", 'Ħ': "H", 'ı': "i", 'ł': "l", 'Ł': "L", 'ŋ': "n", 'Ŋ': "N",
	'ø': "o", 'Ø': "O", 'ŧ': "t", 'Ŧ': "T", 'œ': "œ", 'Œ': "Œ", 'ẞ': "SS", 'ﬁ': "fi", 'ﬂ': "fl", '−': "-", '\u00a0': " ",
	'\u200b': "", '\ufeff': ""}

// cp1252Char is cp1252Char: the byte of a character, judged by its first
// UTF-16 unit as JavaScript's charCodeAt(0).
func cp1252Char(r rune) (byte, bool) {
	code := r
	if r > 0xFFFF {
		code = rune(utf16.Encode([]rune{r})[0])
	}
	if code >= 0x20 && code <= 0x7E || code >= 0xA0 && code <= 0xFF {
		return byte(code), true
	}
	if b, ok := cp1252High[r]; ok {
		return b, true
	}
	return 0, false
}

// encodeWinAnsi is encodeWinAnsi: text as Windows-1252 bytes.
func encodeWinAnsi(v string) []byte {
	var out []byte
	for _, ch := range v {
		if ch == '\n' {
			out = append(out, '\n')
			continue
		}
		if b, ok := cp1252Char(ch); ok {
			out = append(out, b)
			continue
		}
		if m, ok := pdfTransliterate[ch]; ok {
			for _, c := range m {
				if b, ok := cp1252Char(c); ok {
					out = append(out, b)
				} else {
					out = append(out, '?')
				}
			}
			continue
		}
		wrote := false
		for _, s := range norm.NFD.String(string(ch)) {
			if s >= 0x300 && s <= 0x36F {
				continue
			}
			if b, ok := cp1252Char(s); ok {
				out = append(out, b)
				wrote = true
			}
		}
		if !wrote {
			out = append(out, '?')
		}
	}
	return out
}

// --- markdown, flattened, with JavaScript's regular expressions

type jsRe struct {
	re   *regexp2.Regexp
	repl string
	fn   func(m *regexp2.Match) string
}

func jsReplace(s string, rules []jsRe) string {
	for _, r := range rules {
		var out string
		var err error
		if r.fn != nil {
			out, err = r.re.ReplaceFunc(s, func(m regexp2.Match) string { return r.fn(&m) }, -1, -1)
		} else {
			out, err = r.re.Replace(s, r.repl, -1, -1)
		}
		if err == nil {
			s = out
		}
	}
	return s
}

func mre(p string) *regexp2.Regexp {
	return regexp2.MustCompile(p, regexp2.ECMAScript|regexp2.Multiline)
}

func linkRepl(m *regexp2.Match) string {
	text, url := m.GroupByNumber(1).String(), m.GroupByNumber(2).String()
	if text != "" {
		return text + " (" + url + ")"
	}
	return url
}

var (
	pdfBlockRules = []jsRe{
		{re: mre(`\r`), repl: ""},
		{re: mre("^```[^\\n]*\\n?"), repl: ""},
		{re: mre(`^~~~[^\n]*\n?`), repl: ""},
		{re: mre(`!\[([^\]]*)\]\(([^)\s]+)[^)]*\)`), fn: linkRepl},
		{re: mre(`\[([^\]]*)\]\(([^)\s]+)[^)]*\)`), fn: linkRepl},
		{re: mre(`^\s{0,3}(#{1,6})\s+`), repl: ""},
		{re: mre(`^\s{0,3}>\s?`), repl: ""},
		{re: mre(`^\s{0,3}([-*+])\s+`), repl: "- "},
	}
	pdfInlineRules = []jsRe{
		{re: mre(`\*\*\*(\S(?:[\s\S]*?\S)?)\*\*\*`), repl: "$1"},
		{re: mre(`\*\*(\S(?:[\s\S]*?\S)?)\*\*`), repl: "$1"},
		{re: mre(`\*(\S(?:[\s\S]*?\S)?)\*`), repl: "$1"},
		{re: mre(`(^|[\s([{"'])___(\S(?:[\s\S]*?\S)?)___(?=[\s)\]}"'.,!?:;]|$)`), repl: "$1$2"},
		{re: mre(`(^|[\s([{"'])__(\S(?:[\s\S]*?\S)?)__(?=[\s)\]}"'.,!?:;]|$)`), repl: "$1$2"},
		{re: mre(`(^|[\s([{"'])_(\S(?:[\s\S]*?\S)?)_(?=[\s)\]}"'.,!?:;]|$)`), repl: "$1$2"},
		{re: mre(`~~(\S(?:[\s\S]*?\S)?)~~`), repl: "$1"},
		{re: mre("`([^`]+)`"), repl: "$1"},
	}
	pdfTailRules = []jsRe{
		{re: mre(`^\s{0,3}([-*_])\s*(\1\s*){2,}$`), repl: ""},
		{re: mre(`[ \t]+$`), repl: ""},
	}
	pdfHTMLRules = []jsRe{{re: mre(`<[^>]*>`), repl: " "}, {re: mre(`\r`), repl: ""}, {re: mre(`\t`), repl: " "}}
)

// flattenMarkdown is flattenMarkdown: markdown as words.
func flattenMarkdown(v string) string {
	return jsReplace(jsReplace(jsReplace(v, pdfBlockRules), pdfInlineRules), pdfTailRules)
}

func normalizePdfText(v string) string { return flattenMarkdown(jsReplace(v, pdfHTMLRules)) }

func flattenMarkdownBlocks(v string) string {
	return jsReplace(jsReplace(v, pdfBlockRules), pdfTailRules)
}

// --- wrapping

// JavaScript's \s is Unicode's spaces; regexp2's ECMAScript mode would
// narrow it to ASCII.
var jsWhite = regexp2.MustCompile(`\s+`, regexp2.None)

func jsSplitSpace(s string) []string {
	var out []string
	last := 0
	for m, _ := jsWhite.FindStringMatch(s); m != nil; m, _ = jsWhite.FindNextMatch(m) {
		i, n := m.ByteRange()
		out = append(out, s[last:i])
		last = i + n
	}
	return append(out, s[last:])
}

// wrapLine is wrapLine, widths in UTF-16 units.
func wrapLine(line string, width int) []string {
	if line == "" {
		return []string{""}
	}
	var words []string
	for _, w := range jsSplitSpace(line) {
		if w != "" {
			words = append(words, w)
		}
	}
	if len(words) == 0 {
		return []string{""}
	}
	var wrapped []string
	cur := ""
	for _, w := range words {
		if cur == "" {
			cur = w
			continue
		}
		if len16(cur+" "+w) <= width {
			cur = cur + " " + w
			continue
		}
		wrapped = append(wrapped, cur)
		cur = w
	}
	if cur != "" {
		wrapped = append(wrapped, cur)
	}
	var out []string
	for _, it := range wrapped {
		if len16(it) <= width {
			out = append(out, it)
			continue
		}
		u := toU16(it)
		for i := 0; i < len(u); i += width {
			out = append(out, u.slice(i, i+width).str())
		}
	}
	return out
}

func wrapTextBlock(text, indent string) []string {
	var out []string
	for _, l := range strings.Split(normalizePdfText(text), "\n") {
		for _, w := range wrapLine(l, pdfTextWidth-len16(indent)) {
			out = append(out, indent+w)
		}
	}
	return out
}

// --- the line kinds

func pdfColumns(pairs [][]any) pdfLine {
	var cells [][]any
	for _, p := range pairs {
		if len(p) > 0 {
			cells = append(cells, p)
		}
	}
	if len(cells) == 0 {
		return pdfPlain("", false)
	}
	cw := max(12, pdfTextWidth/len(cells))
	var runs []pdfRun
	for i, c := range cells {
		room := cw - 1
		head := pdfStr(pairAt(c, 0)) + ": "
		text := head + pdfStr(pairAt(c, 1))
		clipped := text
		if len16(text) > room {
			clipped = slice16(text, 0, room-1) + "…"
		}
		hl := min(len16(head), len16(clipped))
		runs = append(runs, pdfRun{Text: slice16(clipped, 0, hl), Bold: true}, pdfRun{Text: slice16(clipped, hl, len16(clipped))})
		if pad := cw - len16(clipped); pad > 0 && i < len(cells)-1 {
			runs = append(runs, pdfRun{Text: strings.Repeat(" ", pad)})
		}
	}
	return pdfRuns(runs)
}

func pairAt(p []any, i int) any {
	if i < len(p) {
		return p[i]
	}
	return nil
}

// pdfStr is String(value ?? ”).
func pdfStr(v any) string {
	if v == nil || isUndef(v) {
		return ""
	}
	if d, ok := v.(jsDate); ok {
		return cdString(time.Time(d))
	}
	return cdString(v)
}

func pdfColumnRows(pairs [][]any) []pdfLine {
	var cells [][]any
	for _, p := range pairs {
		if len(p) > 0 {
			cells = append(cells, p)
		}
	}
	if len(cells) == 0 {
		return []pdfLine{pdfPlain("", false)}
	}
	cw := max(12, pdfTextWidth/len(cells))
	type chunk struct {
		text string
		head int
	}
	var wrapped [][]chunk
	height := 0
	for _, c := range cells {
		head, body := pdfStr(pairAt(c, 0))+": ", pdfStr(pairAt(c, 1))
		room := cw - 1
		var cs []chunk
		if len16(head)+len16(body) <= room {
			cs = append(cs, chunk{head + body, len16(head)})
		} else {
			hu, bu := toU16(head), toU16(body)
			for off := 0; off < len(hu); off += room {
				t := hu.slice(off, off+room).str()
				cs = append(cs, chunk{t, len16(t)})
			}
			for off := 0; off < len(bu); off += room {
				cs = append(cs, chunk{bu.slice(off, off+room).str(), 0})
			}
		}
		if len(cs) == 0 {
			cs = []chunk{{"", 0}}
		}
		wrapped = append(wrapped, cs)
		height = max(height, len(cs))
	}
	out := make([]pdfLine, height)
	for r := 0; r < height; r++ {
		var runs []pdfRun
		for i, cs := range wrapped {
			ch := chunk{"", 0}
			if r < len(cs) {
				ch = cs[r]
			}
			if ch.head > 0 {
				runs = append(runs, pdfRun{Text: slice16(ch.text, 0, ch.head), Bold: true})
			}
			runs = append(runs, pdfRun{Text: slice16(ch.text, ch.head, len16(ch.text))})
			if i < len(wrapped)-1 {
				runs = append(runs, pdfRun{Text: strings.Repeat(" ", max(0, cw-len16(ch.text)))})
			}
		}
		out[r] = pdfRuns(runs)
	}
	return out
}

// documentToLines is documentToLines.
func documentToLines(doc []cdBlock, attachmentHeadings []string) []pdfLine {
	var out []pdfLine
	push := func(l ...pdfLine) { out = append(out, l...) }
	section := ""
	runsOf := func(runs []mdRun) []pdfRun {
		out := []pdfRun{}
		for _, r := range runs {
			out = append(out, pdfRun{r.Text, r.Bold, r.Italic})
		}
		return out
	}
	markdownLines := func(blocks []mdBlock) {
		for _, b := range blocks {
			pad := strings.Repeat("  ", b.Level)
			switch b.Type {
			case "rule":
				push(pdfPlain(pad+strings.Repeat("-", 20), false))
				continue
			case "code":
				for _, cl := range strings.Split(*b.Text, "\n") {
					push(pdfPlain(pad+"    "+cl, false))
				}
				continue
			}
			marker := ""
			switch {
			case b.Type == "bullet":
				marker = "- "
			case b.Type == "ordered":
				marker = strconv.Itoa(*b.Index) + ". "
			case b.Quote != nil && *b.Quote > 0:
				marker = "> "
			}
			runs := runsOf(b.Runs)
			if b.Type == "heading" {
				for i := range runs {
					runs[i].Bold = true
				}
			}
			var text strings.Builder
			for _, r := range runs {
				text.WriteString(r.Text)
			}
			uniform := true
			for _, r := range runs {
				uniform = uniform && r.Bold == runs[0].Bold && r.Italic == runs[0].Italic
			}
			if uniform {
				styled := len(runs) > 0 && (runs[0].Bold || runs[0].Italic)
				for _, it := range wrapTextBlock(marker+text.String(), pad) {
					if styled {
						push(pdfRuns([]pdfRun{{it, runs[0].Bold, runs[0].Italic}}))
					} else {
						push(pdfLine{IsString: true, Text: it})
					}
				}
			} else {
				push(pdfRuns(append([]pdfRun{{Text: pad + marker}}, runs...)))
			}
		}
	}
	for _, b := range doc {
		switch b.Type {
		case "title":
			var t strings.Builder
			for _, r := range b.Runs {
				t.WriteString(r.Text)
			}
			push(pdfPlain(t.String(), true), pdfPlain(strings.Repeat("=", pdfTextWidth), false))
		case "section":
			section = b.Key
			push(pdfLine{IsString: true}, pdfBar(b.Title))
		case "meta":
			if len(b.LabelDetails) > 0 {
				var ordinary [][]any
				for _, p := range b.Pairs {
					if pairAt(p, 0) != b.LabelTitle {
						ordinary = append(ordinary, p)
					}
				}
				if len(ordinary) > 0 {
					push(pdfColumnRows(ordinary)...)
				}
				for i := 0; i < len(b.LabelDetails); i += 5 {
					labels := b.LabelDetails[i:min(i+5, len(b.LabelDetails))]
					title := ""
					if i == 0 {
						title = b.LabelTitle
						if title == "" {
							title = "Labels"
						}
					}
					names := make([]string, len(labels))
					for k, l := range labels {
						names[k] = cdOr(cdGet(l, "name"))
					}
					lt := title
					push(pdfLine{LabelRow: labels, LabelTitle: &lt, Runs: pdfColumns([][]any{{title, strings.Join(names, ", ")}}).Runs, HasRuns: true})
				}
			} else {
				push(pdfColumnRows(b.Pairs)...)
			}
		case "text":
			markdownLines(b.Blocks)
		case "note":
			if len(b.Runs) > 0 {
				push(pdfRuns(runsOf(b.Runs)))
			}
			if b.Progress != nil {
				push(pdfLine{Progress: b.Progress, Text: strconv.Itoa(b.Progress.Done) + "/" + strconv.Itoa(b.Progress.Total), HasText: true})
			}
		case "list":
			hasAtt := false
			for _, it := range b.Items {
				hasAtt = hasAtt || it.Attachment != nil
			}
			if section == "attachments" && hasAtt {
				heads := attachmentHeadings
				if len(heads) == 0 {
					heads = []string{"#", "Name", "Size", "Type", "Uploaded", "Uploader"}
				}
				hc := make([]any, len(heads))
				for i, h := range heads {
					hc[i] = h
				}
				push(pdfLine{AttachmentCells: hc, AttachmentHeader: true, Text: strings.Join(heads, " | "), HasText: true})
				for i, it := range b.Items {
					a := cdAttachment{}
					if it.Attachment != nil {
						a = *it.Attachment
					}
					cells := []any{float64(i + 1), a.Name, a.Size, a.Type, a.Uploaded, a.Uploader}
					parts := make([]string, len(cells))
					for k, c := range cells {
						parts[k] = cdString(c)
					}
					push(pdfLine{AttachmentCells: cells, Text: strings.Join(parts, " | "), HasText: true})
				}
				continue
			}
			for _, it := range b.Items {
				marker := it.Marker
				if marker == "" {
					marker = "-"
				}
				runs := runsOf(it.Runs)
				if len(runs) == 0 {
					runs = []pdfRun{{}}
				}
				push(pdfRuns(append([]pdfRun{{Text: strings.Repeat("  ", it.Level) + marker + " "}}, runs...)))
			}
		case "rows":
			for _, row := range b.Rows {
				var first strings.Builder
				if len(row) > 0 {
					for _, r := range row[0] {
						first.WriteString(r.Text)
					}
				}
				runs := []pdfRun{{Text: first.String() + "  ", Italic: true}}
				if len(row) > 1 {
					runs = append(runs, runsOf(row[1])...)
				}
				push(pdfRuns(runs))
			}
		case "images":
			for i := 0; i < len(b.Images); i += 3 {
				row := b.Images[i:min(i+3, len(b.Images))]
				caps := make([]string, len(row))
				for k, im := range row {
					caps[k] = im.Name
				}
				push(pdfLine{ImageRow: row, Span: 9, ImageCaptions: caps, Text: "", HasText: true})
			}
		}
	}
	return out
}

// paginateLines is paginateLines.
func paginateLines(lines []pdfLine) [][]pdfLine {
	per := (pdfPageHeight - pdfMargin*2) / pdfLineHeight
	var pages [][]pdfLine
	var page []pdfLine
	for _, l := range lines {
		span := max(1, l.Span)
		if len(page) > 0 && len(page)+span > per {
			pages = append(pages, page)
			page = nil
		}
		page = append(page, l)
		for r := 1; r < span; r++ {
			page = append(page, pdfPlain("", false))
		}
	}
	if len(page) > 0 {
		pages = append(pages, page)
	}
	if len(pages) == 0 {
		return [][]pdfLine{{pdfPlain("No data", false)}}
	}
	return pages
}

// --- pictures

type pdfImage struct {
	width, height int
	data          []byte
	filter        string
}

func jpegSize(d []byte) (int, int, bool) {
	if len(d) < 4 || d[0] != 0xFF || d[1] != 0xD8 {
		return 0, 0, false
	}
	off := 2
	for off+8 < len(d) {
		if d[off] != 0xFF {
			off++
			continue
		}
		m := d[off+1]
		off += 2
		if m == 0xD8 || m == 0xD9 {
			continue
		}
		l := int(binary.BigEndian.Uint16(d[off:]))
		if l < 2 || off+l > len(d) {
			return 0, 0, false
		}
		if m >= 0xC0 && m <= 0xC3 || m >= 0xC5 && m <= 0xC7 || m >= 0xC9 && m <= 0xCB || m >= 0xCD && m <= 0xCF {
			return int(binary.BigEndian.Uint16(d[off+5:])), int(binary.BigEndian.Uint16(d[off+3:])), true
		}
		off += l
	}
	return 0, 0, false
}

func paeth(a, b, c int) int {
	p := a + b - c
	pa, pb, pc := abs(p-a), abs(p-b), abs(p-c)
	if pa <= pb && pa <= pc {
		return a
	}
	if pb <= pc {
		return b
	}
	return c
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func pngImage(d []byte) *pdfImage {
	sig := []byte{137, 80, 78, 71, 13, 10, 26, 10}
	if len(d) < 33 || !bytes.Equal(d[:8], sig) {
		return nil
	}
	off, width, height, depth, ctype := 8, 0, 0, 0, -1
	var idat bytes.Buffer
	var palette []byte
	for off+12 <= len(d) {
		l := int(binary.BigEndian.Uint32(d[off:]))
		typ := string(d[off+4 : off+8])
		if off+8+l > len(d) {
			return nil
		}
		chunk := d[off+8 : off+8+l]
		switch typ {
		case "IHDR":
			if len(chunk) < 13 {
				return nil
			}
			width, height = int(binary.BigEndian.Uint32(chunk)), int(binary.BigEndian.Uint32(chunk[4:]))
			depth, ctype = int(chunk[8]), int(chunk[9])
			if chunk[12] != 0 {
				return nil
			}
		case "PLTE":
			palette = chunk
		case "IDAT":
			idat.Write(chunk)
		}
		if typ == "IEND" {
			break
		}
		off += l + 12
	}
	channels := map[int]int{0: 1, 2: 3, 3: 1, 4: 2, 6: 4}[ctype]
	if width == 0 || height == 0 || depth != 8 || idat.Len() == 0 || channels == 0 || ctype == 3 && palette == nil {
		return nil
	}
	zr, err := zlib.NewReader(&idat)
	if err != nil {
		return nil
	}
	var packed bytes.Buffer
	if _, err := packed.ReadFrom(zr); err != nil {
		return nil
	}
	p := packed.Bytes()
	stride := width * channels
	if len(p) < (stride+1)*height {
		return nil
	}
	prev := make([]byte, stride)
	at := 0
	rgb := make([]byte, 0, width*height*3)
	pal := func(i int) int {
		if i < len(palette) {
			return int(palette[i])
		}
		return 0
	}
	for y := 0; y < height; y++ {
		filter := p[at]
		at++
		raw := p[at : at+stride]
		at += stride
		row := make([]byte, stride)
		for x := 0; x < stride; x++ {
			left, up, ul := 0, int(prev[x]), 0
			if x >= channels {
				left, ul = int(row[x-channels]), int(prev[x-channels])
			}
			var pred int
			switch filter {
			case 0:
			case 1:
				pred = left
			case 2:
				pred = up
			case 3:
				pred = (left + up) / 2
			case 4:
				pred = paeth(left, up, ul)
			default:
				return nil
			}
			row[x] = byte((int(raw[x]) + pred) & 255)
		}
		for x := 0; x < width; x++ {
			var r, g, b int
			alpha := 255
			switch ctype {
			case 0, 4:
				r = int(row[x*channels])
				g, b = r, r
			case 3:
				i := int(row[x]) * 3
				r, g, b = pal(i), pal(i+1), pal(i+2)
			default:
				r, g, b = int(row[x*channels]), int(row[x*channels+1]), int(row[x*channels+2])
			}
			if ctype == 4 {
				alpha = int(row[x*channels+1])
			}
			if ctype == 6 {
				alpha = int(row[x*channels+3])
			}
			mix := func(c int) byte { return byte(math.Floor(float64(c*alpha+255*(255-alpha))/255 + 0.5)) }
			rgb = append(rgb, mix(r), mix(g), mix(b))
		}
		prev = row
	}
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	_, _ = zw.Write(rgb)
	_ = zw.Close()
	return &pdfImage{width, height, z.Bytes(), "/FlateDecode"}
}

func preparePdfImage(im cdImage) *pdfImage {
	if len(im.Data) == 0 {
		return nil
	}
	kind := strings.ToLower(im.Ext)
	if strings.Contains(kind, "jpeg") || strings.Contains(kind, "jpg") {
		if w, h, ok := jpegSize(im.Data); ok {
			return &pdfImage{w, h, im.Data, "/DCTDecode"}
		}
		return nil
	}
	if strings.Contains(kind, "png") {
		return pngImage(im.Data)
	}
	return nil
}

// --- the file

func escapePdfText(b []byte) []byte {
	var out []byte
	for _, c := range b {
		if c == '\\' || c == '(' || c == ')' {
			out = append(out, '\\')
		}
		out = append(out, c)
	}
	return out
}

// jsNum is a number as a JavaScript template literal writes it.
func jsNum(f float64) string { return jsNumber(f) }

// buildPdfBuffer is buildPdfBuffer: the lines as a PDF.
func buildPdfBuffer(lines []pdfLine) []byte {
	for i := range lines {
		if lines[i].IsString {
			lines[i] = pdfPlain(lines[i].Text, false)
		}
	}
	pages := paginateLines(lines)
	var objects [][]byte
	add := func(b []byte) int { objects = append(objects, b); return len(objects) }
	catalogID, pagesID := add(nil), add(nil)
	fontID := add([]byte("<< /Type /Font /Subtype /Type1 /BaseFont /Courier /Encoding /WinAnsiEncoding >>"))
	boldID := add([]byte("<< /Type /Font /Subtype /Type1 /BaseFont /Courier-Bold /Encoding /WinAnsiEncoding >>"))
	italicID := add([]byte("<< /Type /Font /Subtype /Type1 /BaseFont /Courier-Oblique /Encoding /WinAnsiEncoding >>"))
	boldItalicID := add([]byte("<< /Type /Font /Subtype /Type1 /BaseFont /Courier-BoldOblique /Encoding /WinAnsiEncoding >>"))
	fontFor := func(bold, italic bool) string {
		switch {
		case bold && italic:
			return "F4"
		case bold:
			return "F2"
		case italic:
			return "F3"
		}
		return "F1"
	}
	var pageIDs []int
	for _, pl := range pages {
		type placed struct {
			index, column, id int
			prep              *pdfImage
			name              string
			hasColumn         bool
		}
		var images []placed
		for i, item := range pl {
			for col, im := range item.ImageRow {
				prep := preparePdfImage(im)
				if prep == nil {
					continue
				}
				var obj bytes.Buffer
				obj.WriteString("<< /Type /XObject /Subtype /Image /Width " + strconv.Itoa(prep.width) + " /Height " + strconv.Itoa(prep.height) +
					" /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter " + prep.filter + " /Length " + strconv.Itoa(len(prep.data)) + " >>\nstream\n")
				obj.Write(prep.data)
				obj.WriteString("\nendstream")
				id := add(obj.Bytes())
				images = append(images, placed{i, col, id, prep, "Im" + strconv.Itoa(len(images)+1), true})
			}
		}
		var cmds []string
		for i, item := range pl {
			if !item.Bar {
				continue
			}
			baseline := pdfPageHeight - pdfMargin - pdfFontSize - i*pdfLineHeight
			cmds = append(cmds, "0.85 g", strconv.Itoa(pdfMargin-4)+" "+strconv.Itoa(baseline-3)+" "+strconv.Itoa(pdfPageWidth-(pdfMargin-4)*2)+" "+strconv.Itoa(pdfLineHeight)+" re f", "0 g")
		}
		for _, e := range images {
			gap := 10.0
			maxW := (float64(pdfPageWidth-pdfMargin*2) - gap*2) / 3
			maxH := float64(pdfLineHeight * 7)
			scale := math.Min(math.Min(maxW/float64(e.prep.width), maxH/float64(e.prep.height)), 1)
			w := math.Max(1, math.Floor(float64(e.prep.width)*scale+0.5))
			h := math.Max(1, math.Floor(float64(e.prep.height)*scale+0.5))
			x := float64(pdfMargin) + float64(e.column)*(maxW+gap)
			y := float64(pdfPageHeight-pdfMargin-pdfFontSize-(e.index+1)*pdfLineHeight) - h + pdfLineHeight
			cmds = append(cmds, "q", jsNum(w)+" 0 0 "+jsNum(h)+" "+jsNum(x)+" "+jsNum(y)+" cm", "/"+e.name+" Do", "Q")
		}
		var text bytes.Buffer
		text.WriteString(strings.Join(append(cmds, "BT", "/F1 "+strconv.Itoa(pdfFontSize)+" Tf", strconv.Itoa(pdfLineHeight)+" TL",
			"1 0 0 1 "+strconv.Itoa(pdfMargin)+" "+strconv.Itoa(pdfPageHeight-pdfMargin-pdfFontSize)+" Tm"), "\n"))
		cur := "F1"
		draw := func(t string, bold, italic bool) {
			if w := fontFor(bold, italic); w != cur {
				text.WriteString("\n/" + w + " " + strconv.Itoa(pdfFontSize) + " Tf")
				cur = w
			}
			text.WriteString("\n(")
			text.Write(escapePdfText(encodeWinAnsi(t)))
			text.WriteString(") Tj")
		}
		for i, item := range pl {
			if i > 0 {
				text.WriteString("\nT*")
			}
			if item.HasRuns {
				if len(item.Runs) == 0 {
					draw("", false, false)
				}
				for _, r := range item.Runs {
					draw(r.Text, r.Bold, r.Italic)
				}
				continue
			}
			draw(item.Text, item.Bold, false)
		}
		text.WriteString("\nET")
		stream := text.Bytes()
		var content bytes.Buffer
		content.WriteString("<< /Length " + strconv.Itoa(len(stream)) + " >>\nstream\n")
		content.Write(stream)
		content.WriteString("\nendstream")
		contentID := add(content.Bytes())
		var xo []string
		for _, e := range images {
			xo = append(xo, "/"+e.name+" "+strconv.Itoa(e.id)+" 0 R")
		}
		pageIDs = append(pageIDs, add([]byte("<< /Type /Page /Parent "+strconv.Itoa(pagesID)+" 0 R /MediaBox [0 0 "+strconv.Itoa(pdfPageWidth)+" "+strconv.Itoa(pdfPageHeight)+"] "+
			"/Resources << /Font << /F1 "+strconv.Itoa(fontID)+" 0 R /F2 "+strconv.Itoa(boldID)+" 0 R /F3 "+strconv.Itoa(italicID)+" 0 R /F4 "+strconv.Itoa(boldItalicID)+" 0 R >> "+
			"/XObject << "+strings.Join(xo, " ")+" >> >> /Contents "+strconv.Itoa(contentID)+" 0 R >>")))
	}
	objects[catalogID-1] = []byte("<< /Type /Catalog /Pages " + strconv.Itoa(pagesID) + " 0 R >>")
	kids := make([]string, len(pageIDs))
	for i, id := range pageIDs {
		kids[i] = strconv.Itoa(id) + " 0 R"
	}
	objects[pagesID-1] = []byte("<< /Type /Pages /Kids [" + strings.Join(kids, " ") + "] /Count " + strconv.Itoa(len(pageIDs)) + " >>")
	var pdf bytes.Buffer
	pdf.WriteString("%PDF-1.4\n")
	offsets := []int{0}
	for i, o := range objects {
		offsets = append(offsets, pdf.Len())
		pdf.WriteString(strconv.Itoa(i+1) + " 0 obj\n")
		pdf.Write(o)
		pdf.WriteString("\nendobj\n")
	}
	xref := pdf.Len()
	pdf.WriteString("xref\n0 " + strconv.Itoa(len(objects)+1) + "\n0000000000 65535 f \n")
	for _, off := range offsets[1:] {
		pdf.WriteString(leftPad10(off) + " 00000 n \n")
	}
	pdf.WriteString("trailer\n<< /Size " + strconv.Itoa(len(objects)+1) + " /Root " + strconv.Itoa(catalogID) + " 0 R >>\nstartxref\n" + strconv.Itoa(xref) + "\n%%EOF")
	return pdf.Bytes()
}

func leftPad10(n int) string {
	s := strconv.Itoa(n)
	return strings.Repeat("0", max(0, 10-len(s))) + s
}

// pdfInlineMarkers are INLINE_MARKERS, longest first.
var pdfInlineMarkers = []struct {
	mark         string
	bold, italic bool
	word         bool
}{{"***", true, true, false}, {"___", true, true, true}, {"**", true, false, false}, {"__", true, false, true},
	{"~~", false, false, false}, {"*", false, true, false}, {"_", false, true, true}, {"`", false, false, false}}

func isOpensWord(c uint16) bool { return mdIsWhiteSpace(c) || strings.ContainsRune(`([{"'`, rune(c)) }
func isClosesWord(c uint16) bool {
	return mdIsWhiteSpace(c) || strings.ContainsRune(`)]}"'.,!?:;`, rune(c))
}

func u16Index(u u16s, sub u16s, from int) int {
	for i := from; i+len(sub) <= len(u); i++ {
		if equalU16(u[i:i+len(sub)], sub) {
			return i
		}
	}
	return -1
}

func equalU16(a, b u16s) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// inlineRuns is inlineRuns: one line's emphasis as runs.
func inlineRuns(value string) []pdfRun {
	text := toU16(value)
	var runs []pdfRun
	push := func(chunk string, bold, italic bool) {
		if chunk == "" {
			return
		}
		if n := len(runs); n > 0 && runs[n-1].Bold == bold && runs[n-1].Italic == italic {
			runs[n-1].Text += chunk
			return
		}
		runs = append(runs, pdfRun{chunk, bold, italic})
	}
	var plain u16s
	for i := 0; i < len(text); {
		matched := false
		for _, m := range pdfInlineMarkers {
			mk := toU16(m.mark)
			if i+len(mk) > len(text) || !equalU16(text[i:i+len(mk)], mk) {
				continue
			}
			from := i + len(mk)
			close := u16Index(text, mk, from)
			if close == -1 || close == from {
				continue
			}
			content := text[from:close]
			if mdIsWhiteSpace(content[0]) || mdIsWhiteSpace(content[len(content)-1]) {
				continue
			}
			if m.word {
				if i > 0 && !isOpensWord(text[i-1]) {
					continue
				}
				if after := close + len(mk); after < len(text) && !isClosesWord(text[after]) {
					continue
				}
			}
			push(plain.str(), false, false)
			plain = nil
			for _, r := range inlineRuns(content.str()) {
				push(r.Text, r.Bold || m.bold, r.Italic || m.italic)
			}
			i = close + len(mk)
			matched = true
			break
		}
		if !matched {
			plain = append(plain, text[i])
			i++
		}
	}
	push(plain.str(), false, false)
	if len(runs) == 0 {
		return []pdfRun{{}}
	}
	return runs
}

// wrapRichTextBlock is wrapRichTextBlock: the text wrapped to the page with
// its emphasis kept as runs.
func wrapRichTextBlock(text, indent string) []pdfLine {
	width := pdfTextWidth - len16(indent)
	source := flattenMarkdownBlocks(jsReplace(text, pdfHTMLRules))
	var out []pdfLine
	for _, raw := range strings.Split(source, "\n") {
		type word struct {
			text         string
			bold, italic bool
			space        bool
		}
		var words []word
		for _, r := range inlineRuns(raw) {
			for _, part := range jsSplitSpaceKeep(r.Text) {
				if part == "" {
					continue
				}
				if jsAllSpace(part) {
					words = append(words, word{space: true})
				} else {
					words = append(words, word{text: part, bold: r.Bold, italic: r.Italic})
				}
			}
		}
		var cur []pdfRun
		length := 0
		flush := func() {
			out = append(out, pdfRuns(append([]pdfRun{{Text: indent}}, cur...)))
			cur, length = nil, 0
		}
		add := func(w pdfRun) {
			if n := len(cur); n > 0 && cur[n-1].Bold == w.Bold && cur[n-1].Italic == w.Italic {
				cur[n-1].Text += w.Text
			} else {
				cur = append(cur, w)
			}
			length += len16(w.Text)
		}
		for _, w := range words {
			if w.space {
				if length > 0 && length < width {
					add(pdfRun{Text: " "})
				}
				continue
			}
			rest := pdfRun{w.text, w.bold, w.italic}
			for len16(rest.Text) > width {
				if length > 0 {
					flush()
				}
				add(pdfRun{slice16(rest.Text, 0, width), rest.Bold, rest.Italic})
				rest.Text = slice16(rest.Text, width, len16(rest.Text))
				flush()
			}
			if rest.Text == "" {
				continue
			}
			if length+len16(rest.Text) > width {
				if n := len(cur); n > 0 && strings.HasSuffix(cur[n-1].Text, " ") {
					cur[n-1].Text = strings.TrimRight(cur[n-1].Text, " ")
				}
				flush()
			}
			add(rest)
		}
		flush()
	}
	return out
}

// jsSplitSpaceKeep is split(/(\s+)/): the parts and the spaces between.
func jsSplitSpaceKeep(s string) []string {
	var out []string
	last := 0
	for m, _ := jsWhite.FindStringMatch(s); m != nil; m, _ = jsWhite.FindNextMatch(m) {
		i, n := m.ByteRange()
		out = append(out, s[last:i], s[i:i+n])
		last = i + n
	}
	return append(out, s[last:])
}

func jsAllSpace(s string) bool {
	m, _ := jsWhite.FindStringMatch(s)
	if m == nil {
		return false
	}
	i, n := m.ByteRange()
	return i == 0 && n == len(s)
}
