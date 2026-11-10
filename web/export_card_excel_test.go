package web

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// cardDocumentsForTest are the documents of testdata/export/card-documents.json.
func cardDocumentsForTest(t *testing.T) map[string][]cdBlock {
	raw, _ := os.ReadFile("testdata/export/card-documents.json")
	var cases []struct {
		Name   string            `json:"name"`
		Fields []string          `json:"fields"`
		Users  map[string]string `json:"users"`
		Data   map[string]any    `json:"data"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	translations.load()
	out := map[string][]cdBlock{}
	for _, c := range cases {
		d := cdRevive(c.Data).(map[string]any)
		in := cardExportInput{Board: d["board"], List: d["list"], Card: d["card"], Swimlane: d["swimlane"],
			Checklists: cdList(d["checklists"]), ChecklistItems: cdList(d["checklistItems"]), Subtasks: cdList(d["subtasks"]),
			Comments: cdList(d["comments"]), Attachments: cdList(d["attachments"]), CustomFieldsByID: map[string]any{}}
		if m, ok := d["customFieldsById"].(map[string]any); ok {
			in.CustomFieldsByID = m
		}
		users := c.Users
		out[c.Name] = buildExportCardDocument(in, cardExportOptions{Fields: c.Fields,
			UserName: func(id string) string {
				if n := users[id]; n != "" {
					return n
				}
				return id
			},
			FormatDate: func(v any) string {
				if tm, ok := v.(time.Time); ok {
					return tm.UTC().Format("2006-01-02T15:04:05.000Z") + " UTC"
				}
				return ""
			},
			CustomValue: func(_, v any) any { return v },
			Translate:   cardDocTranslate("en"),
		})
	}
	return out
}

// Each card document drawn on a worksheet from row 3, as Node.js WeKan's
// renderCardDocumentExcel draws it (generate-card-excel.mjs): every cell's
// value and style, the merged ranges, the row heights, the next row and
// the page breaks.
func TestCardExcelIsWeKans(t *testing.T) {
	raw, _ := os.ReadFile("testdata/export/card-excel.golden")
	var golden map[string]struct {
		Cells   map[string]any     `json:"cells"`
		Merges  []string           `json:"merges"`
		Heights map[string]float64 `json:"heights"`
		Row     int                `json:"row"`
		Breaks  []int              `json:"pageBreakRows"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil || len(golden) == 0 {
		t.Fatal(err)
	}
	for name, doc := range cardDocumentsForTest(t) {
		want := golden[name]
		ws := newXSheet("Card")
		row, breaks := renderCardDocumentExcel(ws, 3, doc, cardExcelOptions{FontName: "Arial", ProgressColor: "2E7D32",
			AttachmentHeadings: []string{"#", "Name", "Size", "Type", "Uploaded at", "Uploaded by"}})
		if row != want.Row || !reflect.DeepEqual(append([]int{}, breaks...), append([]int{}, want.Breaks...)) {
			t.Errorf("%s: row %d breaks %v, not %d %v", name, row, breaks, want.Row, want.Breaks)
		}
		merges := append([]string{}, ws.Merges...)
		sort.Strings(merges)
		if !reflect.DeepEqual(merges, want.Merges) {
			t.Errorf("%s: merges %v\nnot %v", name, merges, want.Merges)
		}
		heights := map[string]float64{}
		for r, h := range ws.Heights {
			heights[strconv.Itoa(r)] = h
		}
		if !reflect.DeepEqual(heights, want.Heights) {
			t.Errorf("%s: heights %v\nnot %v", name, heights, want.Heights)
		}
		if len(ws.cells) != len(want.Cells) {
			t.Errorf("%s: %d cells, not %d", name, len(ws.cells), len(want.Cells))
		}
		for ref, w := range want.Cells {
			c := ws.cells[ref]
			if c == nil {
				t.Errorf("%s %s: no cell, WeKan's %v", name, ref, w)
				continue
			}
			g, _ := json.Marshal(c.json())
			var got any
			_ = json.Unmarshal(g, &got)
			if !reflect.DeepEqual(got, w) {
				wj, _ := json.Marshal(w)
				t.Errorf("%s %s:\n%s\nnot WeKan's\n%s", name, ref, g, wj)
			}
		}
	}
}

// The rendered card written as a workbook reads back cell for cell - text,
// numbers, rich text as its text, dates as dates - with its merged ranges,
// row heights, label fills and page breaks in the sheet.
func TestCardExcelWorkbook(t *testing.T) {
	doc := cardDocumentsForTest(t)["full"]
	ws := newXSheet("Card")
	ws.Widths = []float64{18, 30, 18, 30, 18, 30}
	row, breaks := renderCardDocumentExcel(ws, 1, doc, cardExcelOptions{FontName: "Arial"})
	ws.Breaks = breaks
	data, err := xlsxStyledBook([]*xSheet{ws})
	if err != nil {
		t.Fatal(err)
	}
	sheets, err := readXlsxValues(data)
	if err != nil || len(sheets) != 1 || sheets[0].Name != "Card" {
		t.Fatalf("one sheet: %v %d", err, len(sheets))
	}
	got := func(ref string) any {
		c, r := xRef(ref)
		rows := sheets[0].Rows
		if r-1 >= len(rows) || c >= len(rows[r-1]) {
			return nil
		}
		return rows[r-1][c]
	}
	for ref, c := range ws.cells {
		want := c.Value
		v := got(ref)
		switch w := want.(type) {
		case xRich:
			var text strings.Builder
			for _, run := range w {
				text.WriteString(run.Text)
			}
			if sheetCellString(sheetCellValue(v)) != text.String() {
				t.Errorf("%s: %q, not the rich text %q", ref, sheetCellString(sheetCellValue(v)), text.String())
			}
		case jsDate:
			if d, ok := v.(jsDate); !ok || !time.Time(d).Equal(time.Time(w)) {
				t.Errorf("%s: %v, not the date %v", ref, v, w)
			}
		case string:
			if w == "" {
				continue
			}
			if v != w {
				t.Errorf("%s: %v, not %q", ref, v, w)
			}
		case float64:
			if v != w {
				t.Errorf("%s: %v, not %v", ref, v, w)
			}
		}
	}
	sheet := string(mustUnzip(t, data, "xl/worksheets/sheet1.xml"))
	for _, m := range ws.Merges {
		if !strings.Contains(sheet, `<mergeCell ref="`+m+`"/>`) {
			t.Errorf("the merge %s", m)
		}
	}
	if !strings.Contains(sheet, `<row r="1" ht="40" customHeight="1">`) || !strings.Contains(sheet, `<col min="2" max="2" width="30" customWidth="1"/>`) {
		t.Error("the title row's height and the columns' widths")
	}
	if len(breaks) == 0 || !strings.Contains(sheet, `<brk id="`+strconv.Itoa(breaks[0])+`" max="16383" man="1"/>`) {
		t.Errorf("the page breaks: %v", breaks)
	}
	styles := string(mustUnzip(t, data, "xl/styles.xml"))
	for _, want := range []string{`<fgColor rgb="FFFF0000"/>`, `<fgColor rgb="FFD9D9D9"/>`, `formatCode="yyyy-mm-dd hh:mm"`, `<left style="medium">`, `<rFont`} {
		if !strings.Contains(styles+sheet, want) {
			t.Errorf("the styles have %s", want)
		}
	}
	if row < 40 {
		t.Errorf("the card's rows: %d", row)
	}
	// A picture is embedded, as a drawing of the sheet.
	png := []byte("\x89PNG\r\n\x1a\nfake")
	ws2 := newXSheet("Pics")
	renderCardDocumentExcel(ws2, 1, []cdBlock{{Type: "images", Images: []cdImage{{Name: "p.png", Ext: "png", Data: png}, {Name: "x.tif", Ext: "tiff", Data: png}}}}, cardExcelOptions{})
	pics, _ := xlsxStyledBook([]*xSheet{ws2})
	if string(mustUnzip(t, pics, "xl/media/image1.png")) != string(png) || !strings.Contains(string(mustUnzip(t, pics, "xl/drawings/drawing1.xml")), `r:embed="rId1"`) {
		t.Error("the picture is in the workbook")
	}
	if len(ws2.Images) != 1 {
		t.Errorf("negative: a kind ExcelJS cannot embed is not: %d", len(ws2.Images))
	}
}
