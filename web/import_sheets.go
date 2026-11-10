package web

import (
	"slices"
	"sync"
	"time"
)

// The boards of a workbook, as Node.js WeKan's Excel import finds them
// (models/lib/csvImportMapping.js excelSheetsToBoards, findHeaderRow and
// scoreHeaderRow; server/lib/excelBoardWorkbook.js): every sheet whose header
// is found is a board, its rows from the header down. A sheet without one -
// WeKan's Activity sheet, an empty or notes sheet - is skipped and named.
// "Export all boards" writes one sheet per board, and a WeKan Excel export
// puts its board's title, description, dates and members above the table.

// tableExportOnlyKeys are EXPORT_ONLY_KEYS: columns WeKan's exports write
// that are not imported but still say a row is a header.
var tableExportOnlyKeys = []string{"number", "last-activity", "voting"}

// activitySheetKeys are ACTIVITY_SHEET_KEYS: the Excel export's Activity
// sheet, recognised and skipped.
var activitySheetKeys = []string{"activity", "card"}

// headerLanguage is one language's header names: name -> the keys it is the
// translation of (translationIndex), languages in importHeaderNames' order.
type headerLanguage struct {
	tag   string
	names map[string][]string
}

var headerIndexOnce sync.Once
var headerIndexCache []headerLanguage

// headerIndex is importHeaderNames and translationIndex: every language's
// value of HEADER_I18N_KEYS, normalized.
func headerIndex() []headerLanguage {
	headerIndexOnce.Do(func() {
		keys := []string{}
		for _, f := range tableFields {
			if !slices.Contains(keys, f.ExportKey) {
				keys = append(keys, f.ExportKey)
			}
		}
		for _, k := range append(append([]string{}, tableExportOnlyKeys...), activitySheetKeys...) {
			if !slices.Contains(keys, k) {
				keys = append(keys, k)
			}
		}
		for _, l := range Languages() {
			table := translations.table(l.Tag)
			hl := headerLanguage{tag: l.Tag, names: map[string][]string{}}
			for _, k := range keys {
				// pickHeaderTranslations: a non-empty value only.
				if v := table[k]; jsTrim(v) != "" {
					if n := normalizeHeader(v); n != "" {
						hl.names[n] = append(hl.names[n], k)
					}
				}
			}
			headerIndexCache = append(headerIndexCache, hl)
		}
	})
	return headerIndexCache
}

// tableAliasField is fieldByAlias: an English header name's field, the first
// field naming it.
func tableAliasField(name string) string {
	for _, f := range tableFields {
		if slices.Contains(tableAliases[f.Field], name) {
			return f.Field
		}
	}
	return ""
}

// scoreHeaderRow is how much a row looks like a header: how many of its
// cells are a known column name, whether one is the title, and how many are
// the Activity sheet's.
func scoreHeaderRow(row []string) (score int, hasTitle bool, activity int) {
	index := headerIndex()
	names := make([]string, len(row))
	for i, c := range row {
		names[i] = normalizeHeader(c)
	}
	best := bestHeaderLanguage(names, index)
	for _, n := range names {
		if n == "" {
			continue
		}
		var keys []string
		if best >= 0 && index[best].names[n] != nil {
			keys = index[best].names[n]
		} else {
			for _, hl := range index {
				keys = append(keys, hl.names[n]...)
			}
		}
		alias := tableAliasField(n)
		if len(keys) > 0 || alias != "" || isCustomFieldHeader(n) {
			score++
		}
		if alias == "title" || slices.Contains(keys, "title") {
			hasTitle = true
		}
		if slices.ContainsFunc(keys, func(k string) bool { return slices.Contains(activitySheetKeys, k) }) {
			activity++
		}
	}
	return
}

// findHeaderRow is the row of a sheet that is its header: row 1 when it has
// a title column, else the row among the first 30 with the most known names
// - at least four, so a stray "Title" in a sheet's notes is not one. -1 is
// none.
func findHeaderRow(rows [][]string) int {
	best, bestScore := -1, 0
	for i := 0; i < min(len(rows), 30); i++ {
		score, hasTitle, activity := scoreHeaderRow(rows[i])
		if !hasTitle || activity >= 2 {
			continue
		}
		if i == 0 && score >= 1 {
			return 0
		}
		if score >= 4 && score > bestScore {
			best, bestScore = i, score
		}
	}
	return best
}

// sheetCellValue is cellValue: a formula cell is its last result, rich text
// its text, a hyperlink its text, a date stays a date, an error is empty.
func sheetCellValue(v any) any {
	switch x := v.(type) {
	case nil, undefinedValue:
		return ""
	case jsDate:
		return x
	case float64, bool, string:
		return jsStr(x)
	case jsObject:
		if runs, ok := jsGet(x, "richText").([]any); ok {
			out := ""
			for _, r := range runs {
				if jsTruthy(jsGet(r, "text")) {
					out += jsStr(jsGet(r, "text"))
				}
			}
			return out
		}
		_, formula := jsLookup(x, "formula")
		_, shared := jsLookup(x, "sharedFormula")
		if formula || shared {
			return sheetCellValue(jsGetOrUndef(x, "result"))
		}
		if t, ok := jsLookup(x, "text"); ok {
			return sheetCellValue(t)
		}
		if _, ok := jsLookup(x, "error"); ok {
			return ""
		}
	}
	return jsStr(v)
}

// sheetCellString is cellString: a date as ISO 8601.
func sheetCellString(v any) string {
	if d, ok := v.(jsDate); ok {
		return jsISO(time.Time(d))
	}
	if s, ok := v.(string); ok {
		return s
	}
	return jsStr(v)
}

func sheetBlank(v any) bool { return jsTrim(sheetCellString(v)) == "" }

// sheetBoard is one board of a workbook: its title (from A1 above its
// header, else the sheet's name when the workbook has several), its sheet
// and its rows, rows[0] the header, every cell text.
type sheetBoard struct {
	Title string     `json:"title"`
	Sheet string     `json:"sheet"`
	Rows  [][]string `json:"rows"`
}

// excelSheetBoards is excelSheetsToBoards. When no sheet has a header, the
// first sheet with a value is one board read from its first row.
func excelSheetBoards(sheets []xlsxValueSheet) (boards []sheetBoard, skipped []string) {
	asText := func(rows [][]any) [][]string {
		out := make([][]string, len(rows))
		for i, row := range rows {
			out[i] = make([]string, len(row))
			for k, c := range row {
				out[i][k] = sheetCellString(c)
			}
		}
		return out
	}
	nonBlank := func(row []any) bool { return slices.ContainsFunc(row, func(c any) bool { return !sheetBlank(c) }) }
	values := make([][][]any, len(sheets))
	for n, sh := range sheets {
		for _, row := range sh.Rows {
			cells := make([]any, len(row))
			for k, c := range row {
				cells[k] = sheetCellValue(c)
			}
			values[n] = append(values[n], cells)
		}
	}
	for n, sh := range sheets {
		rows := values[n]
		at := findHeaderRow(asText(rows))
		if at == -1 {
			skipped = append(skipped, sh.Name)
			continue
		}
		title := ""
		if at > 0 && len(rows) > 0 {
			var top []any
			for _, c := range rows[0] {
				if !sheetBlank(c) {
					top = append(top, c)
				}
			}
			if len(top) == 1 {
				title = jsTrim(sheetCellString(top[0]))
			}
		}
		var body [][]any
		for i, row := range rows[at:] {
			if i == 0 || nonBlank(row) {
				body = append(body, row)
			}
		}
		if title == "" && len(sheets) > 1 {
			title = sh.Name
		}
		boards = append(boards, sheetBoard{Title: title, Sheet: sh.Name, Rows: asText(body)})
	}
	if len(boards) == 0 {
		for n, sh := range sheets {
			if !slices.ContainsFunc(values[n], nonBlank) {
				continue
			}
			var body [][]any
			for _, row := range values[n] {
				if nonBlank(row) {
					body = append(body, row)
				}
			}
			boards = append(boards, sheetBoard{Sheet: sh.Name, Rows: asText(body)})
			skipped = slices.DeleteFunc(skipped, func(name string) bool { return name == sh.Name })
			break
		}
	}
	return boards, skipped
}

// sameSheetHeader is mappingForSheet's test: the mapping the person
// confirmed for the first board applies to a sheet with the same header;
// another sheet's columns are read by their names.
func sameSheetHeader(boards []sheetBoard, i int) bool {
	if i >= len(boards) || len(boards) == 0 || len(boards[i].Rows) == 0 || len(boards[0].Rows) == 0 {
		return false
	}
	return slices.Equal(boards[i].Rows[0], boards[0].Rows[0])
}
