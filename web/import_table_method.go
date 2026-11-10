package web

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
)

// importBoard's csv and excel sources, as Node.js WeKan's models/import.js
// reads them: a CSV/TSV text's rows as an array of arrays, a workbook as
// { excelBase64 } - every sheet with a header its own board - and
// data.csvMapping, the columns the import page's mapping step confirmed,
// checked against the file's header. With no mapping the header names say
// which column is which (resolveCsvMapping).

// tableMappingMaxColumns is csvImportMapping.js's MAX_COLUMNS.
const tableMappingMaxColumns = 10000

// tableMappingOf is isCsvMappingShape and the mapping it accepts: an object
// of only columns (field -> column index), listName (text, at most 255) and
// customFieldColumns (at most 1000 indexes). ok is false for any other shape.
func tableMappingOf(v any) (m tableMapping, ok bool) {
	o, isObj := v.(jsObject)
	if !isObj {
		return m, false
	}
	isIndex := func(x any) (int, bool) {
		f, isNum := x.(float64)
		if !isNum || f != float64(int(f)) || f < 0 || f >= tableMappingMaxColumns {
			return 0, false
		}
		return int(f), true
	}
	m.Columns = map[string]int{}
	for _, kv := range o {
		switch kv.Key {
		case "columns":
			cols, isObj := kv.Value.(jsObject)
			if !isObj {
				return m, false
			}
			for _, c := range cols {
				i, ok := isIndex(c.Value)
				if !ok || !slices.ContainsFunc(tableFields, func(f tableField) bool { return f.Field == c.Key }) {
					return m, false
				}
				m.Columns[c.Key] = i
			}
		case "listName":
			name, isText := kv.Value.(string)
			if !isText || utf16Len(name) > 255 {
				return m, false
			}
			// validateCsvMapping keeps it trimmed.
			m.ListName = jsTrim(name)
		case "customFieldColumns":
			list, isArr := kv.Value.([]any)
			if !isArr || len(list) > 1000 {
				return m, false
			}
			for _, x := range list {
				i, ok := isIndex(x)
				if !ok {
					return m, false
				}
				m.CustomFields = append(m.CustomFields, i)
			}
		default:
			return m, false
		}
	}
	return m, true
}

// importedTableRows are the rows as CsvCreator reads them: each cell as
// cellString writes it, the empty rows after the header left out.
func importedTableRows(v any) ([][]string, bool) {
	list, isArr := v.([]any)
	if !isArr || len(list) == 0 {
		return nil, false
	}
	var rows [][]string
	for i, r := range list {
		cells, isArr := r.([]any)
		if !isArr {
			return nil, false
		}
		row := make([]string, len(cells))
		filled := false
		for k, c := range cells {
			if c != nil && !isUndef(c) {
				row[k] = jsStr(c)
				filled = filled || jsTrim(row[k]) != ""
			}
		}
		if i == 0 || filled {
			rows = append(rows, row)
		}
	}
	return rows, true
}

// methodImportTable is importBoard for csv and excel.
func (s *Server) methodImportTable(r *http.Request, u *User, source string, board any, rawData json.RawMessage) (any, error) {
	// The argument audit first, as WeKan's check(data.csvMapping, ...).
	var mapping *tableMapping
	if data, err := decodeJS(rawData); err == nil {
		if v, has := jsLookup(data, "csvMapping"); has && v != nil && !isUndef(v) {
			m, ok := tableMappingOf(v)
			if !ok {
				return nil, &methodError{"Match.Error", "Match failed"}
			}
			mapping = &m
		}
	}
	if s.importDisabled(r.Context()) {
		return nil, &methodError{"import-disabled", "Import is disabled by the administrator."}
	}
	badShape := &methodError{"invalid-import-format", "Invalid " + source + " import document shape"}
	type part struct {
		title string
		rows  [][]string
	}
	var parts []part
	switch source {
	case "csv":
		// validateImportSourceShape: a non-empty array of arrays.
		rows, ok := importedTableRows(board)
		if !ok {
			return nil, badShape
		}
		parts = []part{{rows: rows}}
	case "excel":
		b64, _ := jsGet(board, "excelBase64").(string)
		if b64 == "" {
			return nil, badShape
		}
		if len(b64) > (MaxImportBytes+2)/3*4 {
			return nil, &methodError{"import-too-large", "Import/export limit exceeded"}
		}
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, &methodError{"invalid-import-format", "The workbook is not an .xlsx file"}
		}
		sheets, err := readXlsxValues(raw)
		if err != nil {
			return nil, &methodError{"invalid-import-format", err.Error()}
		}
		boards, _ := excelSheetBoards(sheets)
		if len(boards) == 0 {
			return nil, &methodError{"invalid-import-format", "The workbook has no rows"}
		}
		for _, b := range boards {
			parts = append(parts, part{b.Title, b.Rows})
		}
	}
	lang := languageOf(r, u).Tag
	var first string
	var firstHeader []string
	for i, p := range parts {
		// What the rows carry, checked as every import's is (sanitizeImported).
		cells := make([]any, len(p.rows))
		for k, row := range p.rows {
			c := make([]any, len(row))
			for n, v := range row {
				c[n] = v
			}
			cells[k] = c
		}
		clean, err := s.secureImport(r, u, source, cells)
		if err != nil {
			return nil, &methodError{"import-too-large", "Import/export limit exceeded"}
		}
		rows, _ := importedTableRows(clean)
		if len(rows) == 0 {
			return nil, &methodError{"error-csv-schema", "error-csv-schema"}
		}
		// mappingForSheet: the mapping for the sheets with the first one's
		// header, the header names for any other and for no mapping.
		if i == 0 {
			firstHeader = rows[0]
		}
		m := guessTableMapping(rows[0])
		if mapping != nil && (i == 0 || slices.Equal(rows[0], firstHeader)) {
			m = *mapping
			if err := checkTableMapping(m, len(rows[0])); err != nil {
				return nil, &methodError{"invalid-import-mapping", err.Error()}
			}
		}
		b, err := s.importTable(r.Context(), u, p.title, rows, m, tr(lang, "scrum-category-todo"))
		switch {
		case errors.Is(err, errBoardCreation):
			return nil, &methodError{"not-authorized", err.Error()}
		case err != nil:
			return nil, &methodError{"invalid-import-mapping", err.Error()}
		}
		if first == "" {
			first = b.ID
		}
	}
	return first, nil
}
