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

// TestImportPlaneIsWeKans: each Plane export in testdata/import/plane - its
// JSON, CSV and workbook files, the export .zip - imports as WeKan's reader
// and parser read it (generate-plane.mjs), people included; what WeKan
// refuses Weddy refuses.
func TestImportPlaneIsWeKans(t *testing.T) { testImportFiles(t, "plane") }

// TestImportNotionIsWeKans: each Notion export in testdata/import/notion -
// export .zips, a zip of zips, single database CSVs - imports as WeKan's
// archive reader and parser read it (generate-notion.mjs), people included,
// and what WeKan refuses Weddy refuses.
func TestImportNotionIsWeKans(t *testing.T) { testImportFiles(t, "notion") }

// testImportFiles compares each testdata/import/<source>/ file's import with
// <source>-<name>.parsed.golden and its people with <source>-people.golden.
func testImportFiles(t *testing.T, source string) {
	files, _ := filepath.Glob("testdata/import/" + source + "/*")
	if len(files) == 0 {
		t.Fatal("no " + source + " files")
	}
	peopleRaw, _ := os.ReadFile("testdata/import/" + source + "-people.golden")
	var people map[string][]struct{ Key, Name string }
	_ = json.Unmarshal(peopleRaw, &people)
	for _, file := range files {
		name := source + "-" + strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
		data, _ := os.ReadFile(file)
		want, err := os.ReadFile("testdata/import/" + name + ".parsed.golden")
		if err != nil {
			t.Fatal(err)
		}
		w, _ := decodeJS(want)
		got, err := importTextParsers[source](string(data))
		if e, ok := jsGet(w, "error").(string); ok {
			if err == nil || err.Error() != e {
				t.Errorf("%s: WeKan refuses it (%s), Weddy: %v", name, e, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if g, x := string(jsJSON(got, "  ")), string(jsJSON(w, "  ")); g != x {
			t.Errorf("%s:\nWeddy %s\nWeKan %s", name, g, x)
		}
		ps := importPeople(importTasks(got))
		if len(ps) != len(people[name]) {
			t.Errorf("%s people: %v, WeKan %v", name, ps, people[name])
		}
		for i, p := range people[name] {
			if i < len(ps) && (ps[i].key != p.Key || ps[i].name != p.Name) {
				t.Errorf("%s person %d: %+v, WeKan %+v", name, i, ps[i], p)
			}
		}
	}
}
