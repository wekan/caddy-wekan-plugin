package web

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"math"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// An .xlsx workbook's sheets as rows of text, as Node.js WeKan's Excel import
// reads them with ExcelJS (models/lib/csvImportMapping.js cellValue): shared
// and inline strings, numbers as written, booleans, a formula's last result,
// and a date cell - a number in a date format - as ISO text. Standard library
// only, with limits, since the workbook is the uploader's.

type xlsxSheet struct {
	Name string
	Rows [][]string
}

const (
	maxXlsxPart  = 64 << 20
	maxXlsxCells = 2_000_000
)

func readXlsxPart(z *zip.Reader, name string) ([]byte, error) {
	for _, f := range z.File {
		if f.Name == name {
			r, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer r.Close()
			data, err := io.ReadAll(io.LimitReader(r, maxXlsxPart+1))
			if err != nil {
				return nil, err
			}
			if len(data) > maxXlsxPart {
				return nil, errors.New("a part of the workbook is too large")
			}
			return data, nil
		}
	}
	return nil, errors.New("the workbook has no " + name)
}

var cellRef = regexp.MustCompile(`^([A-Z]{1,3})([0-9]+)$`)

// colIndex is a cell reference's column: A -> 0, AA -> 26.
func colIndex(ref string) (int, int, bool) {
	m := cellRef.FindStringSubmatch(ref)
	if m == nil {
		return 0, 0, false
	}
	c := 0
	for _, ch := range m[1] {
		c = c*26 + int(ch-'A'+1)
	}
	row, err := strconv.Atoi(m[2])
	if err != nil || row < 1 || row > 1_048_576 {
		return 0, 0, false
	}
	return c - 1, row - 1, true
}

// dateFormat: Excel's built-in date formats, or a custom one with d, m or y
// outside quotes and brackets.
func dateFormat(id int, custom map[int]string) bool {
	if (id >= 14 && id <= 22) || (id >= 45 && id <= 47) {
		return true
	}
	f, ok := custom[id]
	if !ok {
		return false
	}
	f = regexp.MustCompile(`"[^"]*"|\[[^\]]*\]|\\.`).ReplaceAllString(f, "")
	return strings.ContainsAny(strings.ToLower(f), "dmy")
}

// excelDate is an Excel serial day as a time (the 1900 system).
func excelDate(v float64) time.Time {
	days, frac := math.Modf(v)
	t := time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC).AddDate(0, 0, int(days))
	return t.Add(time.Duration(math.Round(frac*86400000)) * time.Millisecond)
}

func readXlsx(data []byte) ([]xlsxSheet, error) {
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, errors.New("not an .xlsx workbook")
	}
	wbData, err := readXlsxPart(z, "xl/workbook.xml")
	if err != nil {
		return nil, err
	}
	var wb struct {
		Sheets []struct {
			Name string `xml:"name,attr"`
			RID  string `xml:"http://schemas.openxmlformats.org/officeDocument/2006/relationships id,attr"`
		} `xml:"sheets>sheet"`
	}
	if err := xml.Unmarshal(wbData, &wb); err != nil {
		return nil, err
	}
	targets := map[string]string{}
	if relData, err := readXlsxPart(z, "xl/_rels/workbook.xml.rels"); err == nil {
		var rels struct {
			Rel []struct {
				ID     string `xml:"Id,attr"`
				Target string `xml:"Target,attr"`
			} `xml:"Relationship"`
		}
		_ = xml.Unmarshal(relData, &rels)
		for _, r := range rels.Rel {
			t := r.Target
			if strings.HasPrefix(t, "/") {
				t = strings.TrimPrefix(t, "/")
			} else {
				t = path.Join("xl", t)
			}
			targets[r.ID] = t
		}
	}
	var shared []string
	if ssData, err := readXlsxPart(z, "xl/sharedStrings.xml"); err == nil {
		var sst struct {
			SI []struct {
				T string `xml:"t"`
				R []struct {
					T string `xml:"t"`
				} `xml:"r"`
			} `xml:"si"`
		}
		if err := xml.Unmarshal(ssData, &sst); err != nil {
			return nil, err
		}
		for _, si := range sst.SI {
			text := si.T
			for _, r := range si.R {
				text += r.T
			}
			shared = append(shared, text)
		}
	}
	// Which styles are dates.
	var dateStyles []bool
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
			dateStyles = append(dateStyles, dateFormat(x.NumFmt, custom))
		}
	}
	cells := 0
	sheets := []xlsxSheet{}
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
					R  string `xml:"r,attr"`
					T  string `xml:"t,attr"`
					S  int    `xml:"s,attr"`
					V  string `xml:"v"`
					IS struct {
						T string `xml:"t"`
						R []struct {
							T string `xml:"t"`
						} `xml:"r"`
					} `xml:"is"`
				} `xml:"c"`
			} `xml:"sheetData>row"`
		}
		if err := xml.Unmarshal(sdata, &ws); err != nil {
			return nil, err
		}
		rows := [][]string{}
		for _, row := range ws.Rows {
			for _, c := range row.C {
				col, r, ok := colIndex(c.R)
				if !ok || col >= maxTableColumns || r >= 200_000 {
					continue
				}
				if cells++; cells > maxXlsxCells {
					return nil, errors.New("the workbook has too many cells")
				}
				var text string
				switch c.T {
				case "s":
					if n, err := strconv.Atoi(c.V); err == nil && n >= 0 && n < len(shared) {
						text = shared[n]
					}
				case "inlineStr":
					text = c.IS.T
					for _, part := range c.IS.R {
						text += part.T
					}
				case "b":
					text = map[string]string{"1": "true", "0": "false"}[c.V]
				case "e":
				case "str":
					text = c.V
				default:
					text = c.V
					if f, err := strconv.ParseFloat(c.V, 64); err == nil && c.S >= 0 && c.S < len(dateStyles) && dateStyles[c.S] {
						text = excelDate(f).Format("2006-01-02T15:04:05.000Z")
					}
				}
				for len(rows) <= r {
					rows = append(rows, []string{})
				}
				for len(rows[r]) <= col {
					rows[r] = append(rows[r], "")
				}
				rows[r][col] = text
			}
		}
		sheets = append(sheets, xlsxSheet{Name: sh.Name, Rows: rows})
	}
	if len(sheets) == 0 {
		return nil, errors.New("the workbook has no sheet")
	}
	return sheets, nil
}

// firstTable is the first sheet with something in it, its empty rows
// skipped: rows[0] is its header.
func firstTable(sheets []xlsxSheet) (string, [][]string) {
	for _, sh := range sheets {
		rows := [][]string{}
		for _, row := range sh.Rows {
			for _, c := range row {
				if strings.TrimSpace(c) != "" {
					rows = append(rows, row)
					break
				}
			}
		}
		if len(rows) > 0 {
			return sh.Name, rows
		}
	}
	return "", nil
}
