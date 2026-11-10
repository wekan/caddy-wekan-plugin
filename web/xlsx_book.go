package web

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// A workbook of one or more sheets, as the tools' Excel imports read the ones
// WeKan writes with ExcelJS: text as inline strings, numbers as numbers,
// dates as date cells shown yyyy-mm-dd, true and false as booleans, and the
// header row bold.

// jsDate is a JavaScript Date in a formatter's rows: a date cell in the
// workbook, and toISOString() in JSON, as JSON.stringify writes it.
type jsDate time.Time

func (d jsDate) MarshalJSON() ([]byte, error) {
	return []byte(`"` + time.Time(d).UTC().Format("2006-01-02T15:04:05.000Z") + `"`), nil
}

type xlsxSpec struct {
	Name     string
	Rows     [][]any
	BoldRow  int   // 1-based; 0 for none
	DateCols []int // 0-based columns whose yyyy-mm-dd text is a date cell
	// BoldRows are more bold rows (1-based), Merges the merged ranges
	// ("A1:H1"), and DateTime writes every date cell as yyyy/mm/dd hh:mm:ss,
	// as WeKan's ExporterExcel columns do.
	BoldRows []int
	Merges   []string
	DateTime bool
}

// xlsxSheetName is WeKan's: no \ / ? * [ ] :, at most 31 characters.
func xlsxSheetName(name, fallback string) string {
	n := strings.Map(func(r rune) rune {
		if strings.ContainsRune(`\/?*[]:`, r) {
			return ' '
		}
		return r
	}, name)
	n = jsSlice16(n, 31)
	if n == "" {
		return fallback
	}
	return n
}

func xlsxColumn(i int) string {
	name := ""
	for i++; i > 0; i = (i - 1) / 26 {
		name = string(rune('A'+(i-1)%26)) + name
	}
	return name
}

// excelSerial is a time as Excel's day number (1900 system, UTC).
func excelSerial(t time.Time) float64 {
	return float64(t.UTC().Sub(time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC))) / float64(24*time.Hour)
}

func xlsxBook(sheets []xlsxSpec) ([]byte, error) {
	esc := func(s string) string {
		var b bytes.Buffer
		_ = xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	isoDay := func(v any) (time.Time, bool) {
		s, ok := v.(string)
		if !ok || len(s) != 10 {
			return time.Time{}, false
		}
		t, err := time.Parse("2006-01-02", s)
		return t, err == nil
	}
	sheetXML := func(sp xlsxSpec) string {
		var b strings.Builder
		b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
			`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
		for i, row := range sp.Rows {
			fmt.Fprintf(&b, `<row r="%d">`, i+1)
			for j, cell := range row {
				ref := xlsxColumn(j) + strconv.Itoa(i+1)
				style := ""
				if sp.BoldRow == i+1 || slices.Contains(sp.BoldRows, i+1) {
					style = ` s="1"`
				}
				dateCol := false
				for _, c := range sp.DateCols {
					dateCol = dateCol || c == j
				}
				if d, ok := isoDay(cell); ok && dateCol && sp.BoldRow != i+1 {
					cell = jsDate(d)
				}
				switch v := cell.(type) {
				case nil:
					continue
				case jsDate:
					ds := "2"
					if sp.DateTime {
						ds = "3"
					}
					fmt.Fprintf(&b, `<c r="%s" s="%s"><v>%s</v></c>`, ref, ds, strconv.FormatFloat(excelSerial(time.Time(v)), 'f', -1, 64))
				case bool:
					n := "0"
					if v {
						n = "1"
					}
					fmt.Fprintf(&b, `<c r="%s"%s t="b"><v>%s</v></c>`, ref, style, n)
				case int:
					fmt.Fprintf(&b, `<c r="%s"%s><v>%d</v></c>`, ref, style, v)
				case int64:
					fmt.Fprintf(&b, `<c r="%s"%s><v>%d</v></c>`, ref, style, v)
				case float64:
					if math.IsNaN(v) || math.IsInf(v, 0) {
						continue
					}
					fmt.Fprintf(&b, `<c r="%s"%s><v>%s</v></c>`, ref, style, strconv.FormatFloat(v, 'f', -1, 64))
				default:
					s := cellText(v)
					if s == "" {
						continue // ExcelJS writes no cell for ''
					}
					fmt.Fprintf(&b, `<c r="%s"%s t="inlineStr"><is><t xml:space="preserve">%s</t></is></c>`, ref, style, esc(s))
				}
			}
			b.WriteString(`</row>`)
		}
		b.WriteString(`</sheetData>`)
		if len(sp.Merges) > 0 {
			fmt.Fprintf(&b, `<mergeCells count="%d">`, len(sp.Merges))
			for _, m := range sp.Merges {
				b.WriteString(`<mergeCell ref="` + m + `"/>`)
			}
			b.WriteString(`</mergeCells>`)
		}
		b.WriteString(`</worksheet>`)
		return b.String()
	}
	var overrides, sheetEntries, rels strings.Builder
	files := map[string]string{}
	var order []string
	for i, sp := range sheets {
		n := strconv.Itoa(i + 1)
		path := "xl/worksheets/sheet" + n + ".xml"
		files[path] = sheetXML(sp)
		order = append(order, path)
		overrides.WriteString(`<Override PartName="/` + path + `" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`)
		sheetEntries.WriteString(`<sheet name="` + esc(sp.Name) + `" sheetId="` + n + `" r:id="rId` + n + `"/>`)
		rels.WriteString(`<Relationship Id="rId` + n + `" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet` + n + `.xml"/>`)
	}
	stylesID := "rId" + strconv.Itoa(len(sheets)+1)
	rels.WriteString(`<Relationship Id="` + stylesID + `" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>`)
	files["[Content_Types].xml"] = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
		`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/>` +
		`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>` +
		`<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>` + overrides.String() + `</Types>`
	files["_rels/.rels"] = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
		`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>`
	files["xl/workbook.xml"] = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" ` +
		`xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets>` + sheetEntries.String() + `</sheets></workbook>`
	files["xl/_rels/workbook.xml.rels"] = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` + rels.String() + `</Relationships>`
	files["xl/styles.xml"] = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">` +
		`<numFmts count="2"><numFmt numFmtId="164" formatCode="yyyy-mm-dd"/><numFmt numFmtId="165" formatCode="yyyy/mm/dd hh:mm:ss"/></numFmts>` +
		`<fonts count="2"><font><sz val="11"/><name val="Calibri"/></font><font><b/><sz val="11"/><name val="Calibri"/></font></fonts>` +
		`<fills count="2"><fill><patternFill patternType="none"/></fill><fill><patternFill patternType="gray125"/></fill></fills>` +
		`<borders count="1"><border><left/><right/><top/><bottom/><diagonal/></border></borders>` +
		`<cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs>` +
		`<cellXfs count="4"><xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/>` +
		`<xf numFmtId="0" fontId="1" fillId="0" borderId="0" xfId="0" applyFont="1"/>` +
		`<xf numFmtId="164" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1"/>` +
		`<xf numFmtId="165" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1"/></cellXfs></styleSheet>`
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	for _, name := range append([]string{"[Content_Types].xml", "_rels/.rels", "xl/workbook.xml", "xl/_rels/workbook.xml.rels", "xl/styles.xml"}, order...) {
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
