package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// xlsxPlain is a cell value as generate-xlsx.mjs writes ExcelJS's: a hole
// as {hole}, a Date as {date}, an object's fonts left out.
func xlsxPlain(v any) any {
	switch x := v.(type) {
	case undefinedValue:
		return map[string]any{"hole": true}
	case jsDate:
		return map[string]any{"date": jsISO(time.Time(x))}
	case jsObject:
		m := map[string]any{}
		for _, kv := range x {
			m[kv.Key] = xlsxPlain(kv.Value)
		}
		return m
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = xlsxPlain(e)
		}
		return out
	}
	return v
}

// TestXlsxValuesAreExcelJS: Weddy's typed reader gives a workbook's cells
// as ExcelJS gives them to WeKan's workbook readers
// (testdata/import/generate-xlsx.mjs): the values, their types, the holes
// and the skipped rows.
func TestXlsxValuesAreExcelJS(t *testing.T) {
	data, err := os.ReadFile("testdata/import/xlsx/values.xlsx")
	if err != nil {
		t.Fatal(err)
	}
	sheets, err := readXlsxValues(data)
	if err != nil {
		t.Fatal(err)
	}
	var got []any
	for _, s := range sheets {
		rows := []any{}
		for _, r := range s.Rows {
			rows = append(rows, xlsxPlain(append([]any{}, r...)))
		}
		got = append(got, map[string]any{"name": s.Name, "rows": rows})
	}
	raw, _ := json.Marshal(got)
	var gotPlain, want any
	_ = json.Unmarshal(raw, &gotPlain)
	gold, _ := os.ReadFile("testdata/import/xlsx-values.golden")
	_ = json.Unmarshal(gold, &want)
	if !reflect.DeepEqual(gotPlain, want) {
		g, _ := json.Marshal(gotPlain)
		w, _ := json.Marshal(want)
		t.Errorf("Weddy\n%s\nExcelJS\n%s", g, w)
	}
}

// TestImportWorkbooksAreWeKans: each workbook in testdata/import/xlsx,
// written by WeKan's own workbook writers or built cell by cell, imports as
// WeKan's reader and parser read it (generate-xlsx.mjs) - and a workbook
// WeKan refuses, Weddy refuses too.
func TestImportWorkbooksAreWeKans(t *testing.T) {
	files, _ := filepath.Glob("testdata/import/xlsx/*-*.xlsx")
	if len(files) == 0 {
		t.Fatal("no workbooks")
	}
	for _, file := range files {
		base := strings.TrimSuffix(filepath.Base(file), ".xlsx")
		source, _, _ := strings.Cut(base, "-")
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile("testdata/import/" + base + ".parsed.golden")
		if err != nil {
			t.Fatal(err)
		}
		w, _ := decodeJS(want)
		got, err := importTextParsers[source](string(data))
		if e, ok := jsGet(w, "error").(string); ok {
			if err == nil {
				t.Errorf("%s: WeKan refuses it (%s), Weddy imports it", base, e)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", base, err)
			continue
		}
		if g, x := string(jsJSON(got, "  ")), string(jsJSON(w, "  ")); g != x {
			t.Errorf("%s:\nWeddy %s\nWeKan %s", base, g, x)
		}
	}
}
