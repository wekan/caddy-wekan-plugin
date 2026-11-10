package web

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"math"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// An .xlsx workbook's cells as Node.js WeKan's workbook readers
// (server/lib/*Workbook.js) get them from ExcelJS: each sheet's rows as
// row.values.slice(1) - undefinedValue where a column has no value - with a
// string, a number, a boolean, a jsDate for a number in a date format, and
// ExcelJS's objects for rich text ({richText}), a hyperlink ({text,
// hyperlink}), a formula ({formula or sharedFormula, result}) and an error
// ({error}). Rows without a value are skipped and the gap is an empty row,
// as WeKan's readers fill it. TestXlsxValuesAreExcelJS checks it against
// ExcelJS itself.

type xlsxValueSheet struct {
	Name string
	Rows [][]any
}

var (
	excelJSDefaultFormats = map[int]string{0: "General", 1: "0", 2: "0.00", 3: "#,##0", 4: "#,##0.00", 9: "0%", 10: "0.00%",
		11: "0.00E+00", 12: "# ?/?", 13: "# ??/??", 14: "mm-dd-yy", 15: "d-mmm-yy", 16: "d-mmm", 17: "mmm-yy",
		18: "h:mm AM/PM", 19: "h:mm:ss AM/PM", 20: "h:mm", 21: "h:mm:ss", 22: `m/d/yy "h":mm`, 37: "#,##0 ;(#,##0)",
		38: "#,##0 ;[Red](#,##0)", 39: "#,##0.00 ;(#,##0.00)", 40: "#,##0.00 ;[Red](#,##0.00)", 45: "mm:ss",
		46: "[h]:mm:ss", 47: "mmss.0", 48: "##0.0E+0", 49: "@"}
	excelJSBrackets = regexp.MustCompile(`\[[^\]]*]`)
	excelJSQuotes   = regexp.MustCompile(`"[^"]*"`)
	excelJSDateChar = regexp.MustCompile(`[ymdhMsb]+`)
)

// excelJSIsDateFmt is ExcelJS's utils.isDateFmt.
func excelJSIsDateFmt(fmt string) bool {
	if fmt == "" {
		return false
	}
	fmt = excelJSQuotes.ReplaceAllString(excelJSBrackets.ReplaceAllString(fmt, ""), "")
	return excelJSDateChar.MatchString(fmt)
}

// excelJSDate is ExcelJS's utils.excelToDate.
func excelJSDate(v float64, date1904 bool) jsDate {
	if date1904 {
		v += 1462
	}
	ms := math.Floor((v-25569)*86400000 + 0.5)
	return jsDate(time.UnixMilli(int64(ms)).UTC())
}

type xlsxRun struct {
	T string `xml:"t"`
}

type xlsxText struct {
	T *string   `xml:"t"`
	R []xlsxRun `xml:"r"`
}

// value is a shared or inline string: ExcelJS's rich text when it has runs.
func (t xlsxText) value() any {
	if len(t.R) > 0 {
		runs := []any{}
		for _, r := range t.R {
			o := jsObject{}
			o.set("text", r.T)
			runs = append(runs, o)
		}
		o := jsObject{}
		o.set("richText", runs)
		return o
	}
	if t.T != nil {
		return *t.T
	}
	return ""
}

func readXlsxValues(data []byte) ([]xlsxValueSheet, error) {
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, errors.New("not an .xlsx workbook")
	}
	wbData, err := readXlsxPart(z, "xl/workbook.xml")
	if err != nil {
		return nil, err
	}
	var wb struct {
		Pr struct {
			Date1904 string `xml:"date1904,attr"`
		} `xml:"workbookPr"`
		Sheets []struct {
			Name string `xml:"name,attr"`
			RID  string `xml:"http://schemas.openxmlformats.org/officeDocument/2006/relationships id,attr"`
		} `xml:"sheets>sheet"`
	}
	if err := xml.Unmarshal(wbData, &wb); err != nil {
		return nil, err
	}
	date1904 := wb.Pr.Date1904 == "1" || wb.Pr.Date1904 == "true"
	rels := func(part string) map[string]string {
		dir, file := path.Split(part)
		out := map[string]string{}
		relData, err := readXlsxPart(z, dir+"_rels/"+file+".rels")
		if err != nil {
			return out
		}
		var r struct {
			Rel []struct {
				ID         string `xml:"Id,attr"`
				Target     string `xml:"Target,attr"`
				TargetMode string `xml:"TargetMode,attr"`
			} `xml:"Relationship"`
		}
		_ = xml.Unmarshal(relData, &r)
		for _, rel := range r.Rel {
			t := rel.Target
			if rel.TargetMode != "External" {
				if strings.HasPrefix(t, "/") {
					t = strings.TrimPrefix(t, "/")
				} else {
					t = path.Join(strings.TrimSuffix(dir, "/"), t)
				}
			}
			out[rel.ID] = t
		}
		return out
	}
	targets := rels("xl/workbook.xml")
	var shared []any
	if ssData, err := readXlsxPart(z, "xl/sharedStrings.xml"); err == nil {
		var sst struct {
			SI []xlsxText `xml:"si"`
		}
		if err := xml.Unmarshal(ssData, &sst); err != nil {
			return nil, err
		}
		for _, si := range sst.SI {
			shared = append(shared, si.value())
		}
	}
	// Each style's number format, as ExcelJS's getStyleModel finds it.
	var styleFmts []string
	if stData, err := readXlsxPart(z, "xl/styles.xml"); err == nil {
		var st struct {
			NumFmts []struct {
				ID   int    `xml:"numFmtId,attr"`
				Code string `xml:"formatCode,attr"`
			} `xml:"numFmts>numFmt"`
			Xfs []struct {
				NumFmt int `xml:"numFmtId,attr"`
			} `xml:"cellXfs>xf"`
		}
		_ = xml.Unmarshal(stData, &st)
		custom := map[int]string{}
		for _, f := range st.NumFmts {
			custom[f.ID] = f.Code
		}
		for _, x := range st.Xfs {
			f := ""
			if x.NumFmt != 0 {
				if c, ok := custom[x.NumFmt]; ok && c != "" {
					f = c
				} else {
					f = excelJSDefaultFormats[x.NumFmt]
				}
			}
			styleFmts = append(styleFmts, f)
		}
	}
	isDate := func(s int) bool { return s > 0 && s < len(styleFmts) && excelJSIsDateFmt(styleFmts[s]) }
	cells := 0
	var sheets []xlsxValueSheet
	for i, sh := range wb.Sheets {
		target := targets[sh.RID]
		if target == "" {
			target = "xl/worksheets/sheet" + strconv.Itoa(i+1) + ".xml"
		}
		sdata, err := readXlsxPart(z, target)
		if err != nil {
			continue
		}
		var ws struct {
			Rows []struct {
				C []struct {
					R string  `xml:"r,attr"`
					T string  `xml:"t,attr"`
					S int     `xml:"s,attr"`
					V *string `xml:"v"`
					F *struct {
						Text string `xml:",chardata"`
						T    string `xml:"t,attr"`
						Ref  string `xml:"ref,attr"`
						SI   string `xml:"si,attr"`
					} `xml:"f"`
					IS *xlsxText `xml:"is"`
				} `xml:"c"`
			} `xml:"sheetData>row"`
			Links []struct {
				Ref string `xml:"ref,attr"`
				RID string `xml:"http://schemas.openxmlformats.org/officeDocument/2006/relationships id,attr"`
			} `xml:"hyperlinks>hyperlink"`
		}
		if err := xml.Unmarshal(sdata, &ws); err != nil {
			return nil, err
		}
		links := map[string]string{}
		sheetRels := rels(target)
		for _, l := range ws.Links {
			if t, ok := sheetRels[l.RID]; ok {
				links[l.Ref] = t
			}
		}
		masters := map[string]string{} // a shared formula's si: its master's address
		byRow := map[int][]any{}
		maxRow := -1
		for _, row := range ws.Rows {
			for _, c := range row.C {
				col, r, ok := colIndex(c.R)
				if !ok || col >= maxTableColumns || r >= 200_000 {
					continue
				}
				if cells++; cells > maxXlsxCells {
					return nil, errors.New("the workbook has too many cells")
				}
				v, hasV := "", c.V != nil
				if hasV {
					v = *c.V
				}
				number := func(s string) any {
					f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
					if err != nil {
						return math.NaN()
					}
					if isDate(c.S) {
						return excelJSDate(f, date1904)
					}
					return f
				}
				var value any = undefinedValue{}
				switch {
				case c.F != nil:
					o := jsObject{}
					if c.F.T == "shared" && c.F.Ref == "" {
						if m, ok := masters[c.F.SI]; ok {
							o.set("sharedFormula", m)
						}
					} else {
						o.set("formula", c.F.Text)
						if c.F.T == "shared" {
							masters[c.F.SI] = strings.ToUpper(c.R)
							o.set("ref", c.F.Ref)
							o.set("shareType", "shared")
						}
					}
					if hasV {
						switch c.T {
						case "str", "s", "inlineStr":
							o.set("result", v)
						case "b":
							o.set("result", v != "0")
						case "e":
							e := jsObject{}
							e.set("error", v)
							o.set("result", e)
						default:
							o.set("result", number(v))
						}
					}
					value = o
				case c.T == "s":
					if n, err := strconv.Atoi(v); err == nil && n >= 0 && n < len(shared) {
						value = shared[n]
					}
				case c.T == "inlineStr":
					if c.IS != nil {
						value = c.IS.value()
					}
				case c.T == "str":
					if hasV {
						value = v
					}
				case c.T == "b":
					if hasV {
						value = v != "0"
					}
				case c.T == "e":
					if hasV {
						e := jsObject{}
						e.set("error", v)
						value = e
					}
				case c.T == "d":
					if t, ok := jsDateV8(v); ok {
						value = jsDate(t)
					}
				default:
					if hasV {
						value = number(v)
					}
				}
				if _, undef := value.(undefinedValue); undef {
					continue // ExcelJS's Null cell: no value
				}
				if target, ok := links[strings.ToUpper(c.R)]; ok {
					h := jsObject{}
					h.set("text", value)
					h.set("hyperlink", target)
					value = h
				}
				cellsOf := byRow[r]
				for len(cellsOf) <= col {
					cellsOf = append(cellsOf, undefinedValue{})
				}
				cellsOf[col] = value
				byRow[r] = cellsOf
				if r > maxRow {
					maxRow = r
				}
			}
		}
		rows := [][]any{}
		for r := 0; r <= maxRow; r++ {
			if byRow[r] == nil {
				rows = append(rows, []any{})
				continue
			}
			rows = append(rows, byRow[r])
		}
		sheets = append(sheets, xlsxValueSheet{Name: sh.Name, Rows: rows})
	}
	if len(sheets) == 0 {
		return nil, errors.New("the workbook has no sheet")
	}
	return sheets, nil
}
