package web

import (
	"math"
	"strconv"
	"strings"
)

// A card document drawn on a worksheet, as Node.js WeKan's
// models/server/renderCardDocumentExcel.js draws it with ExcelJS: six
// columns, a merged row a block, fills, borders, row heights, pictures and
// page breaks. It draws onto xSheet, a model of the cells and their styles
// with ExcelJS's own keys, which xlsx_styled.go writes as a workbook.
// TestCardExcelIsWeKans compares the model with ExcelJS's worksheet.

// xCell is a cell: its value - a string, a number, a jsDate or rich text -
// and its style, each part an object with ExcelJS's keys.
type xCell struct {
	Value     any
	Font      jsObject
	Fill      jsObject
	Border    jsObject
	Alignment jsObject
	NumFmt    string
}

// xRich is ExcelJS's { richText: [{ text, font }] }.
type xRich []xRun

type xRun struct {
	Text string
	Font jsObject
}

func (c *xCell) json() jsObject {
	o := jsObject{}
	switch v := c.Value.(type) {
	case jsDate:
		d := jsObject{}
		d.set("date", v)
		o.set("value", d)
	case xRich:
		runs := []any{}
		for _, r := range v {
			x := jsObject{}
			x.set("text", r.Text)
			x.set("font", r.Font)
			runs = append(runs, x)
		}
		rt := jsObject{}
		rt.set("richText", runs)
		o.set("value", rt)
	case nil:
		o.set("value", nil)
	default:
		o.set("value", v)
	}
	if c.Font != nil {
		o.set("font", c.Font)
	}
	if c.Fill != nil {
		o.set("fill", c.Fill)
	}
	if c.Border != nil {
		o.set("border", c.Border)
	}
	if c.Alignment != nil {
		o.set("alignment", c.Alignment)
	}
	if c.NumFmt != "" {
		o.set("numFmt", c.NumFmt)
	}
	return o
}

// xImage is a picture anchored at a cell's top left (0-based col, row).
type xImage struct {
	Col, Row      int
	Width, Height int
	Ext           string
	Data          []byte
}

// xSheet is a worksheet: cells by address, merged ranges, row heights,
// column widths, pictures and row breaks.
type xSheet struct {
	Name    string
	cells   map[string]*xCell
	Merges  []string
	Heights map[int]float64
	Widths  []float64
	Images  []xImage
	Breaks  []int
}

func newXSheet(name string) *xSheet {
	return &xSheet{Name: name, cells: map[string]*xCell{}, Heights: map[int]float64{}}
}

func (s *xSheet) cell(ref string) *xCell {
	c := s.cells[ref]
	if c == nil {
		c = &xCell{}
		s.cells[ref] = c
	}
	return c
}

func (s *xSheet) merge(r string) { s.Merges = append(s.Merges, r) }

func colLetter(i int) string { return string(rune('A' + i)) }

func xObj(kv ...any) jsObject {
	o := jsObject{}
	for i := 0; i+1 < len(kv); i += 2 {
		o.set(kv[i].(string), kv[i+1])
	}
	return o
}

func xBorder(style string) jsObject {
	s := func() jsObject { return xObj("style", style) }
	return xObj("top", s(), "left", s(), "bottom", s(), "right", s())
}

func xSolid(argb string) jsObject {
	return xObj("type", "pattern", "pattern", "solid", "fgColor", xObj("argb", argb))
}

var xGray = func() jsObject { return xSolid("FFD9D9D9") }

// xLabelColors is LABEL_COLORS.
var xLabelColors = map[string]string{
	"white": "FFFFFFFF", "green": "FF008000", "yellow": "FFFFFF00", "orange": "FFFFA500",
	"red": "FFFF0000", "purple": "FF800080", "blue": "FF0000FF", "sky": "FF87CEEB",
	"lime": "FF00FF00", "pink": "FFFFC0CB", "black": "FF000000", "silver": "FFC0C0C0",
	"peachpuff": "FFFFDAB9", "crimson": "FFDC143C", "plum": "FFDDA0DD",
	"darkgreen": "FF006400", "slateblue": "FF6A5ACD", "magenta": "FFFF00FF",
	"gold": "FFFFD700", "navy": "FF000080", "gray": "FF808080",
	"saddlebrown": "FF8B4513", "paleturquoise": "FFAFEEEE", "mistyrose": "FFFFE4E1",
	"indigo": "FF4B0082",
}

// xTextColor is textColor: black on a light background, white on a dark.
func xTextColor(bg string) string {
	hex := bg[2:]
	var l [3]float64
	for i := 0; i < 3; i++ {
		n, _ := strconv.ParseUint(hex[i*2:i*2+2], 16, 8)
		v := float64(n) / 255
		if v <= 0.03928 {
			l[i] = v / 12.92
		} else {
			l[i] = math.Pow((v+0.055)/1.055, 2.4)
		}
	}
	if 0.2126*l[0]+0.7152*l[1]+0.0722*l[2] > 0.179 {
		return "FF000000"
	}
	return "FFFFFFFF"
}

// xRichValue is richValue: plain text for one unstyled run, else rich text.
func xRichValue(runs []mdRun, fontName, prefix string) any {
	type run struct {
		mdRun
		short bool // the prefix run: no strike, code or link of its own
	}
	var values []run
	if prefix != "" {
		values = append(values, run{mdRun{Text: prefix}, true})
	}
	for _, r := range runs {
		values = append(values, run{r, false})
	}
	if len(values) == 0 {
		return ""
	}
	if v := values[0]; len(values) == 1 && !v.Bold && !v.Italic && !v.Strike && !v.Code && v.Link == "" {
		return v.Text
	}
	out := xRich{}
	for _, v := range values {
		name := fontName
		if v.Code {
			name = "Courier New"
		}
		f := xObj("name", name, "size", 10, "bold", v.Bold, "italic", v.Italic, "strike", v.Strike)
		if v.Link != "" {
			f.set("underline", true)
			f.set("color", xObj("argb", "FF0563C1"))
		}
		out = append(out, xRun{Text: v.Text, Font: f})
	}
	return out
}

// xBlockRuns is blockRuns.
func xBlockRuns(b mdBlock) (string, []mdRun) {
	prefix := ""
	switch {
	case b.Type == "bullet":
		prefix = "• "
	case b.Type == "ordered":
		prefix = strconv.Itoa(*b.Index) + ". "
	case b.Quote != nil && *b.Quote > 0:
		prefix = "> "
	}
	return strings.Repeat("    ", b.Level) + prefix, b.Runs
}

// cardExcelOptions are renderCardDocumentExcel's options.
type cardExcelOptions struct {
	FontName           string
	ProgressColor      string
	AttachmentHeadings []string
}

// renderCardDocumentExcel is renderCardDocumentExcel: the document from
// startRow; the row after it and the rows a page breaks after.
func renderCardDocumentExcel(ws *xSheet, startRow int, doc []cdBlock, o cardExcelOptions) (int, []int) {
	font := o.FontName
	if font == "" {
		font = "Arial"
	}
	row := startRow
	section := ""
	var breaks []int
	r := func() string { return strconv.Itoa(row) }
	type style struct {
		font, alignment, fill, border jsObject
		height                        float64
	}
	merge := func(value any, st style) {
		ws.merge("A" + r() + ":F" + r())
		c := ws.cell("A" + r())
		c.Value = value
		f := xObj("name", font, "size", 10)
		for _, kv := range st.font {
			f.set(kv.Key, kv.Value)
		}
		c.Font = f
		a := xObj("vertical", "middle", "horizontal", "left", "wrapText", true)
		for _, kv := range st.alignment {
			a.set(kv.Key, kv.Value)
		}
		c.Alignment = a
		if st.fill != nil {
			c.Fill = st.fill
		}
		if st.border != nil {
			c.Border = st.border
		}
		h := st.height
		if h == 0 {
			h = 20
		}
		ws.Heights[row] = h
		row++
	}
	label := func(ref string, v any) {
		c := ws.cell(ref)
		c.Value = cdString(v) + ":"
		c.Font = xObj("name", font, "size", 10, "bold", true)
		c.Alignment = xObj("vertical", "middle", "horizontal", "right")
		c.Border = xBorder("thin")
	}
	value := func(ref string, content any, raw any) {
		c := ws.cell(ref)
		if d, ok := raw.(jsDate); ok {
			c.Value = d
			c.NumFmt = "yyyy-mm-dd hh:mm"
		} else {
			c.Value = content
		}
		c.Font = xObj("name", font, "size", 10)
		c.Alignment = xObj("vertical", "middle", "horizontal", "left", "wrapText", true)
		c.Border = xBorder("thin")
	}
	pairAt := func(p []any, i int) any {
		if i < len(p) {
			return p[i]
		}
		return nil
	}
	cols := [][2]string{{"A", "B"}, {"C", "D"}, {"E", "F"}}
	for _, b := range doc {
		switch b.Type {
		case "title":
			merge(xRichValue(b.Runs, font, ""), style{font: xObj("size", 16, "bold", true), height: 40})
		case "section":
			section = b.Key
			if row > startRow+14 {
				breaks = append(breaks, row-1)
			}
			merge(b.Title, style{font: xObj("size", 11, "bold", true), fill: xGray(), border: xBorder("medium"), height: 22, alignment: xObj("wrapText", false)})
		case "meta":
			if len(b.LabelDetails) > 0 {
				var ordinary [][]any
				for _, p := range b.Pairs {
					if pairAt(p, 0) != b.LabelTitle {
						ordinary = append(ordinary, p)
					}
				}
				if len(ordinary) > 0 {
					for i, p := range ordinary {
						label(cols[i][0]+r(), pairAt(p, 0))
						value(cols[i][1]+r(), pairAt(p, 1), pairAt(p, 2))
					}
					ws.Heights[row] = 20
					row++
				}
				lc := []string{"B", "C", "D", "E", "F"}
				for off := 0; off < len(b.LabelDetails); off += len(lc) {
					labels := b.LabelDetails[off:min(off+len(lc), len(b.LabelDetails))]
					title := ""
					if off == 0 {
						title = b.LabelTitle
						if title == "" {
							title = "Labels"
						}
					}
					label("A"+r(), title)
					for i, e := range labels {
						c := ws.cell(lc[i] + r())
						bg := xLabelColors[cdString(cdGet(e, "color"))]
						if bg == "" {
							bg = "FFC0C0C0"
						}
						c.Value = cdOr(cdGet(e, "name"))
						c.Font = xObj("name", font, "size", 10, "bold", true, "color", xObj("argb", xTextColor(bg)))
						c.Fill = xSolid(bg)
						c.Alignment = xObj("vertical", "middle", "horizontal", "center")
						c.Border = xBorder("thin")
					}
					for _, col := range lc[len(labels):] {
						ws.cell(col + r()).Border = xBorder("thin")
					}
					ws.Heights[row] = 20
					row++
				}
				continue
			}
			for i, p := range b.Pairs {
				if i >= 3 {
					break
				}
				label(cols[i][0]+r(), pairAt(p, 0))
				value(cols[i][1]+r(), pairAt(p, 1), pairAt(p, 2))
			}
			ws.Heights[row] = 20
			row++
		case "text":
			for _, mb := range b.Blocks {
				prefix, runs := xBlockRuns(mb)
				st := style{border: xBorder("thin")}
				if mb.Type == "heading" {
					st.font = xObj("bold", true)
				}
				merge(xRichValue(runs, font, prefix), st)
			}
		case "note":
			merge(xRichValue(b.Runs, font, ""), style{font: xObj("bold", true), border: xBorder("thin")})
			if b.Progress != nil {
				total := max(0, b.Progress.Total)
				done := max(0, min(total, b.Progress.Done))
				filled := 0
				if total > 0 {
					filled = int(math.Floor(float64(done)/float64(total)*6 + 0.5))
				}
				for i := 0; i < 6; i++ {
					c := ws.cell(colLetter(i) + r())
					if i == 0 {
						c.Value = strconv.Itoa(done) + "/" + strconv.Itoa(total)
					} else {
						c.Value = ""
					}
					c.Border = xBorder("thin")
					if i < filled {
						pc := o.ProgressColor
						if pc == "" {
							pc = "2980B9"
						}
						c.Fill = xSolid("FF" + pc)
					}
				}
				ws.Heights[row] = 12
				row++
			}
		case "list":
			hasAttachment := false
			for _, it := range b.Items {
				hasAttachment = hasAttachment || it.Attachment != nil
			}
			if section == "attachments" && hasAttachment {
				heads := o.AttachmentHeadings
				if len(heads) == 0 {
					heads = []string{"#", "Name", "Size", "Type", "Uploaded", "Uploader"}
				}
				for i, h := range heads {
					c := ws.cell(colLetter(i) + r())
					c.Value = h
					c.Font = xObj("name", font, "size", 9, "bold", true)
					c.Fill = xGray()
					c.Border = xBorder("thin")
				}
				row++
				for n, it := range b.Items {
					a := cdAttachment{}
					if it.Attachment != nil {
						a = *it.Attachment
					}
					for i, v := range []any{float64(n + 1), a.Name, a.Size, a.Type, a.Uploaded, a.Uploader} {
						c := ws.cell(colLetter(i) + r())
						c.Value = v
						c.Font = xObj("name", font, "size", 9)
						c.Alignment = xObj("vertical", "middle", "horizontal", "left", "wrapText", true)
						c.Border = xBorder("thin")
					}
					row++
				}
			} else {
				for _, it := range b.Items {
					marker := it.Marker
					if marker == "" {
						marker = "-"
					}
					merge(xRichValue(it.Runs, font, strings.Repeat("    ", it.Level)+marker+" "), style{border: xBorder("thin")})
				}
			}
		case "rows":
			for _, cells := range b.Rows {
				ws.merge("A" + r() + ":B" + r())
				ws.merge("C" + r() + ":F" + r())
				value("A"+r(), xRichValue(cells[0], font, ""), nil)
				value("C"+r(), xRichValue(cells[1], font, ""), nil)
				row++
			}
		case "images":
			var imgs []cdImage
			for _, im := range b.Images {
				if len(im.Data) > 0 && (im.Ext == "jpeg" || im.Ext == "png" || im.Ext == "gif" || im.Ext == "bmp") {
					imgs = append(imgs, im)
				}
			}
			for off := 0; off < len(imgs); off += 6 {
				part := imgs[off:min(off+6, len(imgs))]
				ws.Heights[row] = 95
				for i, im := range part {
					ws.Images = append(ws.Images, xImage{Col: i, Row: row - 1, Width: 105, Height: 115, Ext: im.Ext, Data: im.Data})
				}
				row++
				for i, im := range part {
					c := ws.cell(colLetter(i) + r())
					c.Value = im.Name
					c.Font = xObj("name", font, "size", 8)
					c.Alignment = xObj("horizontal", "center", "vertical", "middle")
				}
				ws.Heights[row] = 16
				row++
			}
		}
	}
	return row, breaks
}
