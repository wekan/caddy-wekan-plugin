package web

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// xlsxStyled writes xSheets (export_card_excel.go) as an .xlsx workbook: each
// cell's font, fill, border, alignment and number format as a cell style,
// rich text as inline strings, merged ranges, row heights, column widths,
// row breaks, A4 page setup and pictures in a drawing.

type xlsxStyles struct {
	fonts, fills, borders, xfs []string
	numFmts                    []string
	index                      map[string]int
}

func newXlsxStyles() *xlsxStyles {
	s := &xlsxStyles{index: map[string]int{}}
	s.fonts = []string{`<font><sz val="11"/><name val="Calibri"/></font>`}
	s.fills = []string{`<fill><patternFill patternType="none"/></fill>`, `<fill><patternFill patternType="gray125"/></fill>`}
	s.borders = []string{`<border><left/><right/><top/><bottom/><diagonal/></border>`}
	s.xfs = []string{`<xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/>`}
	return s
}

func xEsc(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func xGetBool(o jsObject, k string) bool { v, _ := jsGet(o, k).(bool); return v }

func xGetNum(o jsObject, k string) (float64, bool) {
	switch v := jsGet(o, k).(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	}
	return 0, false
}

func xArgb(o jsObject, k string) string {
	if c, ok := jsGet(o, k).(jsObject); ok {
		s, _ := jsGet(c, "argb").(string)
		return s
	}
	return ""
}

// fontXML is a font, as an <rPr> (rich text run) or a <font>.
func fontXML(f jsObject, tag string) string {
	var b strings.Builder
	b.WriteString("<" + tag + ">")
	if xGetBool(f, "bold") {
		b.WriteString(`<b/>`)
	}
	if xGetBool(f, "italic") {
		b.WriteString(`<i/>`)
	}
	if xGetBool(f, "strike") {
		b.WriteString(`<strike/>`)
	}
	if xGetBool(f, "underline") {
		b.WriteString(`<u/>`)
	}
	if n, ok := xGetNum(f, "size"); ok {
		b.WriteString(`<sz val="` + strconv.FormatFloat(n, 'f', -1, 64) + `"/>`)
	}
	if c := xArgb(f, "color"); c != "" {
		b.WriteString(`<color rgb="` + c + `"/>`)
	}
	name, _ := jsGet(f, "name").(string)
	if name == "" {
		name = "Calibri"
	}
	if tag == "rPr" {
		b.WriteString(`<rFont val="` + xEsc(name) + `"/>`)
	} else {
		b.WriteString(`<name val="` + xEsc(name) + `"/>`)
	}
	b.WriteString("</" + tag + ">")
	return b.String()
}

func (s *xlsxStyles) id(list *[]string, x string) int {
	for i, e := range *list {
		if e == x {
			return i
		}
	}
	*list = append(*list, x)
	return len(*list) - 1
}

// style is a cell's style id.
func (s *xlsxStyles) style(c *xCell) int {
	if c.Font == nil && c.Fill == nil && c.Border == nil && c.Alignment == nil && c.NumFmt == "" {
		if _, ok := c.Value.(jsDate); !ok {
			return 0
		}
	}
	fontID, fillID, borderID, numID := 0, 0, 0, 0
	if c.Font != nil {
		fontID = s.id(&s.fonts, fontXML(c.Font, "font"))
	}
	if c.Fill != nil {
		if argb := xArgb(c.Fill, "fgColor"); argb != "" {
			fillID = s.id(&s.fills, `<fill><patternFill patternType="solid"><fgColor rgb="`+argb+`"/><bgColor indexed="64"/></patternFill></fill>`)
		}
	}
	if c.Border != nil {
		var b strings.Builder
		b.WriteString("<border>")
		for _, side := range []string{"left", "right", "top", "bottom"} {
			if o, ok := jsGet(c.Border, side).(jsObject); ok {
				st, _ := jsGet(o, "style").(string)
				b.WriteString(`<` + side + ` style="` + st + `"><color indexed="64"/></` + side + `>`)
			} else {
				b.WriteString(`<` + side + `/>`)
			}
		}
		b.WriteString("<diagonal/></border>")
		borderID = s.id(&s.borders, b.String())
	}
	numFmt := c.NumFmt
	if _, ok := c.Value.(jsDate); ok && numFmt == "" {
		numFmt = "yyyy-mm-dd"
	}
	if numFmt != "" {
		numID = 164 + s.id(&s.numFmts, numFmt)
	}
	var a strings.Builder
	if c.Alignment != nil {
		a.WriteString("<alignment")
		if v, _ := jsGet(c.Alignment, "horizontal").(string); v != "" {
			a.WriteString(` horizontal="` + v + `"`)
		}
		if v, _ := jsGet(c.Alignment, "vertical").(string); v != "" {
			if v == "middle" {
				v = "center"
			}
			a.WriteString(` vertical="` + v + `"`)
		}
		if xGetBool(c.Alignment, "wrapText") {
			a.WriteString(` wrapText="1"`)
		}
		a.WriteString("/>")
	}
	xf := fmt.Sprintf(`<xf numFmtId="%d" fontId="%d" fillId="%d" borderId="%d" xfId="0"`, numID, fontID, fillID, borderID)
	if numID > 0 {
		xf += ` applyNumberFormat="1"`
	}
	if fontID > 0 {
		xf += ` applyFont="1"`
	}
	if fillID > 0 {
		xf += ` applyFill="1"`
	}
	if borderID > 0 {
		xf += ` applyBorder="1"`
	}
	if a.Len() > 0 {
		xf += ` applyAlignment="1">` + a.String() + `</xf>`
	} else {
		xf += `/>`
	}
	return s.id(&s.xfs, xf)
}

func (s *xlsxStyles) xml() string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">`)
	if len(s.numFmts) > 0 {
		fmt.Fprintf(&b, `<numFmts count="%d">`, len(s.numFmts))
		for i, f := range s.numFmts {
			fmt.Fprintf(&b, `<numFmt numFmtId="%d" formatCode="%s"/>`, 164+i, xEsc(f))
		}
		b.WriteString(`</numFmts>`)
	}
	part := func(tag string, list []string) {
		fmt.Fprintf(&b, `<%s count="%d">%s</%s>`, tag, len(list), strings.Join(list, ""), tag)
	}
	part("fonts", s.fonts)
	part("fills", s.fills)
	part("borders", s.borders)
	b.WriteString(`<cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs>`)
	part("cellXfs", s.xfs)
	b.WriteString(`</styleSheet>`)
	return b.String()
}

// xRef splits "AB12" into column (0-based) and row.
func xRef(ref string) (int, int) {
	i := 0
	col := 0
	for i < len(ref) && ref[i] >= 'A' && ref[i] <= 'Z' {
		col = col*26 + int(ref[i]-'A'+1)
		i++
	}
	row, _ := strconv.Atoi(ref[i:])
	return col - 1, row
}

func (s *xSheet) sheetXML(st *xlsxStyles, drawing bool) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">`)
	b.WriteString(`<sheetPr><pageSetUpPr fitToPage="1"/></sheetPr>`)
	if len(s.Widths) > 0 {
		b.WriteString(`<cols>`)
		for i, w := range s.Widths {
			fmt.Fprintf(&b, `<col min="%d" max="%d" width="%s" customWidth="1"/>`, i+1, i+1, strconv.FormatFloat(w, 'f', -1, 64))
		}
		b.WriteString(`</cols>`)
	}
	rows := map[int][]string{}
	for ref := range s.cells {
		_, r := xRef(ref)
		rows[r] = append(rows[r], ref)
	}
	for r := range s.Heights {
		if _, ok := rows[r]; !ok {
			rows[r] = nil
		}
	}
	var nums []int
	for r := range rows {
		nums = append(nums, r)
	}
	sort.Ints(nums)
	b.WriteString(`<sheetData>`)
	for _, r := range nums {
		refs := rows[r]
		sort.Slice(refs, func(i, j int) bool { a, _ := xRef(refs[i]); c, _ := xRef(refs[j]); return a < c })
		fmt.Fprintf(&b, `<row r="%d"`, r)
		if h, ok := s.Heights[r]; ok {
			fmt.Fprintf(&b, ` ht="%s" customHeight="1"`, strconv.FormatFloat(h, 'f', -1, 64))
		}
		b.WriteString(`>`)
		for _, ref := range refs {
			c := s.cells[ref]
			sid := st.style(c)
			sa := ""
			if sid > 0 {
				sa = fmt.Sprintf(` s="%d"`, sid)
			}
			switch v := c.Value.(type) {
			case nil:
				fmt.Fprintf(&b, `<c r="%s"%s/>`, ref, sa)
			case jsDate:
				fmt.Fprintf(&b, `<c r="%s"%s><v>%s</v></c>`, ref, sa, strconv.FormatFloat(excelSerial(time.Time(v)), 'f', -1, 64))
			case float64, int, int32, int64:
				f, _ := toFloat(v)
				if math.IsNaN(f) || math.IsInf(f, 0) {
					fmt.Fprintf(&b, `<c r="%s"%s/>`, ref, sa)
					continue
				}
				fmt.Fprintf(&b, `<c r="%s"%s><v>%s</v></c>`, ref, sa, strconv.FormatFloat(f, 'f', -1, 64))
			case bool:
				n := "0"
				if v {
					n = "1"
				}
				fmt.Fprintf(&b, `<c r="%s"%s t="b"><v>%s</v></c>`, ref, sa, n)
			case xRich:
				fmt.Fprintf(&b, `<c r="%s"%s t="inlineStr"><is>`, ref, sa)
				for _, run := range v {
					b.WriteString(`<r>` + fontXML(run.Font, "rPr") + `<t xml:space="preserve">` + xEsc(run.Text) + `</t></r>`)
				}
				b.WriteString(`</is></c>`)
			default:
				text := cdString(v)
				if text == "" {
					fmt.Fprintf(&b, `<c r="%s"%s/>`, ref, sa)
					continue
				}
				fmt.Fprintf(&b, `<c r="%s"%s t="inlineStr"><is><t xml:space="preserve">%s</t></is></c>`, ref, sa, xEsc(text))
			}
		}
		b.WriteString(`</row>`)
	}
	b.WriteString(`</sheetData>`)
	if len(s.Merges) > 0 {
		fmt.Fprintf(&b, `<mergeCells count="%d">`, len(s.Merges))
		for _, m := range s.Merges {
			b.WriteString(`<mergeCell ref="` + m + `"/>`)
		}
		b.WriteString(`</mergeCells>`)
	}
	b.WriteString(`<pageMargins left="0.5" right="0.5" top="0.75" bottom="0.75" header="0.3" footer="0.3"/>`)
	b.WriteString(`<pageSetup paperSize="9" orientation="portrait" fitToWidth="1" fitToHeight="0"/>`)
	if len(s.Breaks) > 0 {
		seen := map[int]bool{}
		var br []int
		for _, r := range s.Breaks {
			if !seen[r] {
				seen[r] = true
				br = append(br, r)
			}
		}
		fmt.Fprintf(&b, `<rowBreaks count="%d" manualBreakCount="%d">`, len(br), len(br))
		for _, r := range br {
			fmt.Fprintf(&b, `<brk id="%d" max="16383" man="1"/>`, r)
		}
		b.WriteString(`</rowBreaks>`)
	}
	if drawing {
		b.WriteString(`<drawing r:id="rId1"/>`)
	}
	b.WriteString(`</worksheet>`)
	return b.String()
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}

// xlsxStyledBook is the workbook of sheets.
func xlsxStyledBook(sheets []*xSheet) ([]byte, error) {
	st := newXlsxStyles()
	files := map[string]string{}
	var order []string
	var overrides, entries, rels strings.Builder
	media := 0
	types := map[string]bool{}
	for i, s := range sheets {
		n := strconv.Itoa(i + 1)
		path := "xl/worksheets/sheet" + n + ".xml"
		files[path] = s.sheetXML(st, len(s.Images) > 0)
		order = append(order, path)
		overrides.WriteString(`<Override PartName="/` + path + `" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`)
		entries.WriteString(`<sheet name="` + xEsc(s.Name) + `" sheetId="` + n + `" r:id="rId` + n + `"/>`)
		rels.WriteString(`<Relationship Id="rId` + n + `" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet` + n + `.xml"/>`)
		if len(s.Images) == 0 {
			continue
		}
		dpath := "xl/drawings/drawing" + n + ".xml"
		files["xl/worksheets/_rels/sheet"+n+".xml.rels"] = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
			`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/drawing" Target="../drawings/drawing` + n + `.xml"/></Relationships>`
		overrides.WriteString(`<Override PartName="/` + dpath + `" ContentType="application/vnd.openxmlformats-officedocument.drawing+xml"/>`)
		var d, drels strings.Builder
		d.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><xdr:wsDr xmlns:xdr="http://schemas.openxmlformats.org/drawingml/2006/spreadsheetDrawing" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">`)
		drels.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`)
		for k, im := range s.Images {
			media++
			ext := im.Ext
			name := "image" + strconv.Itoa(media) + "." + ext
			files["xl/media/"+name] = string(im.Data)
			types[ext] = true
			rid := "rId" + strconv.Itoa(k+1)
			drels.WriteString(`<Relationship Id="` + rid + `" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/image" Target="../media/` + name + `"/>`)
			// 9525 EMU a pixel.
			fmt.Fprintf(&d, `<xdr:oneCellAnchor><xdr:from><xdr:col>%d</xdr:col><xdr:colOff>0</xdr:colOff><xdr:row>%d</xdr:row><xdr:rowOff>0</xdr:rowOff></xdr:from>`+
				`<xdr:ext cx="%d" cy="%d"/><xdr:pic><xdr:nvPicPr><xdr:cNvPr id="%d" name="Picture %d"/><xdr:cNvPicPr><a:picLocks noChangeAspect="1"/></xdr:cNvPicPr></xdr:nvPicPr>`+
				`<xdr:blipFill><a:blip r:embed="%s"/><a:stretch><a:fillRect/></a:stretch></xdr:blipFill><xdr:spPr><a:xfrm><a:off x="0" y="0"/><a:ext cx="%d" cy="%d"/></a:xfrm><a:prstGeom prst="rect"><a:avLst/></a:prstGeom></xdr:spPr></xdr:pic><xdr:clientData/></xdr:oneCellAnchor>`,
				im.Col, im.Row, im.Width*9525, im.Height*9525, k+2, k+1, rid, im.Width*9525, im.Height*9525)
		}
		d.WriteString(`</xdr:wsDr>`)
		drels.WriteString(`</Relationships>`)
		files[dpath] = d.String()
		files["xl/drawings/_rels/drawing"+n+".xml.rels"] = drels.String()
	}
	stylesID := "rId" + strconv.Itoa(len(sheets)+1)
	rels.WriteString(`<Relationship Id="` + stylesID + `" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>`)
	var defaults strings.Builder
	for _, ext := range []string{"png", "jpeg", "gif", "bmp"} {
		if types[ext] {
			defaults.WriteString(`<Default Extension="` + ext + `" ContentType="image/` + ext + `"/>`)
		}
	}
	files["[Content_Types].xml"] = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
		`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/>` + defaults.String() +
		`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>` +
		`<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>` + overrides.String() + `</Types>`
	files["_rels/.rels"] = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
		`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>`
	files["xl/workbook.xml"] = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" ` +
		`xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets>` + entries.String() + `</sheets></workbook>`
	files["xl/_rels/workbook.xml.rels"] = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` + rels.String() + `</Relationships>`
	files["xl/styles.xml"] = st.xml()
	var names []string
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	head := []string{"[Content_Types].xml", "_rels/.rels", "xl/workbook.xml", "xl/_rels/workbook.xml.rels", "xl/styles.xml"}
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	written := map[string]bool{}
	for _, name := range append(append(head, order...), names...) {
		if written[name] {
			continue
		}
		written[name] = true
		f, err := z.Create(name)
		if err != nil {
			return nil, err
		}
		if _, err := f.Write([]byte(files[name])); err != nil {
			return nil, err
		}
	}
	if err := z.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
