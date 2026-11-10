package web

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"math"
	"os"
	"testing"
	"time"
)

// TestJSDateTime: Weddy's jsDateTime reads an ISO date-time as V8's new
// Date reads it at UTC (testdata/import/generate-dates.mjs) - the days that
// roll over, 24:00, the zones it takes and the shapes it refuses.
func TestJSDateTime(t *testing.T) {
	raw, err := os.ReadFile("testdata/import/dates.golden")
	if err != nil {
		t.Fatal(err)
	}
	gold, err := decodeJS(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range gold.(jsObject) {
		got, ok := jsDateTime(kv.Key)
		switch want := kv.Value.(type) {
		case nil:
			if ok {
				t.Errorf("%q: Weddy %s, V8 no date", kv.Key, jsISO(got))
			}
		case string:
			if !ok || jsISO(got) != want {
				t.Errorf("%q: Weddy %v %s, V8 %s", kv.Key, ok, jsISO(got), want)
			}
		}
	}
}

// TestJSJSONNegativeZero: JSON.stringify writes -0 as 0 - an estimate of
// "-0" in an imported file, say - and so does Weddy, at the top, in an array,
// in a map and in an ordered object; Go's encoder alone writes -0.
func TestJSJSONNegativeZero(t *testing.T) {
	neg := math.Copysign(0, -1)
	o := jsObject{}
	o.set("a", neg)
	if got := string(jsJSON([]any{neg, o, map[string]any{"b": neg}}, "")); got != `[0,{"a":0},{"b":0}]` {
		t.Errorf("got %s", got)
	}
	if got := string(jsJSON(neg, "")); got != "0" {
		t.Errorf("got %s", got)
	}
	if got := string(jsJSON([]any{1.5, -2.0, 1e21, 1e-7}, "")); got != `[1.5,-2,1e+21,1e-7]` {
		t.Errorf("other numbers changed: %s", got)
	}
}

// TestJSJSONUndefined: JSON.stringify leaves out an object's key whose value
// is undefined and writes undefined in an array as null - a Focalboard
// comment without a date, an option without a value.
func TestJSJSONUndefined(t *testing.T) {
	o := jsObject{}
	o.set("text", "a")
	o.set("date", undefinedValue{})
	o.set("n", 1.0)
	if got := string(jsJSON([]any{o, undefinedValue{}}, "")); got != `[{"text":"a","n":1},null]` {
		t.Errorf("got %s", got)
	}
	only := jsObject{}
	only.set("title", undefinedValue{})
	if got := string(jsJSON(only, "")); got != `{}` {
		t.Errorf("got %s", got)
	}
}

// TestJSISOYears: toISOString writes a year past 9999 with a sign and six
// digits - 8.64e15 ms, the last time JavaScript has, is +275760.
func TestJSISOYears(t *testing.T) {
	for ms, want := range map[int64]string{8.64e15: "+275760-09-13T00:00:00.000Z", 1: "1970-01-01T00:00:00.001Z", 253402300800000: "+010000-01-01T00:00:00.000Z"} {
		if got := jsISO(time.UnixMilli(ms)); got != want {
			t.Errorf("%d: %s, want %s", ms, got, want)
		}
	}
}

// TestJSSort: jsSort orders as V8's Array.prototype.sort does, also with a
// comparator that returns NaN (testdata/import/generate-sort.mjs).
func TestJSSort(t *testing.T) {
	raw, err := os.ReadFile("testdata/import/sort.golden")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Input  []*float64
		Output []int
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	type entry struct {
		id  int
		key float64
	}
	for n, c := range cases {
		items := make([]any, len(c.Input))
		for i, k := range c.Input {
			key := math.NaN()
			if k != nil {
				key = *k
			}
			items[i] = entry{i, key}
		}
		jsSort(items, func(a, b any) float64 { return a.(entry).key - b.(entry).key })
		for i, it := range items {
			if it.(entry).id != c.Output[i] {
				t.Errorf("case %d: Weddy %v, V8 %v", n, items, c.Output)
				break
			}
		}
	}
}

// TestVikunjaHTMLIsWeKans: Weddy reads Vikunja's TipTap HTML into text and
// checklists as WeKan's vikunjaHtmlToText does
// (testdata/import/generate-vikunja-html.mjs).
func TestVikunjaHTMLIsWeKans(t *testing.T) {
	raw, err := os.ReadFile("testdata/import/vikunja-html.golden")
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		Text       string
		Checklists []struct {
			Title string
			Items []struct {
				Title string
				Done  bool
			}
		}
	}
	var cases []struct {
		HTML    string
		With    result
		Without result
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		for _, want := range []struct {
			checklists bool
			r          result
		}{{true, c.With}, {false, c.Without}} {
			text, lists := vikunjaHTMLToText(c.HTML, want.checklists)
			if text != want.r.Text {
				t.Errorf("%q (checklists %v): text\nWeddy %q\nWeKan %q", c.HTML, want.checklists, text, want.r.Text)
			}
			if len(lists) != len(want.r.Checklists) {
				t.Errorf("%q (checklists %v): %d checklists, WeKan %d", c.HTML, want.checklists, len(lists), len(want.r.Checklists))
				continue
			}
			for i, l := range lists {
				w := want.r.Checklists[i]
				if l.title != w.Title || len(l.items) != len(w.Items) {
					t.Errorf("%q: checklist %d %q %d items, WeKan %q %d", c.HTML, i, l.title, len(l.items), w.Title, len(w.Items))
					continue
				}
				for j, it := range l.items {
					if jsGet(it, "title") != w.Items[j].Title || jsGet(it, "done") != w.Items[j].Done {
						t.Errorf("%q: item %d %v, WeKan %+v", c.HTML, j, it, w.Items[j])
					}
				}
			}
		}
	}
}

// TestJSDateV8Days: a date alone is V8's UTC midnight, a day past its
// month's end rolled over and a month 13 or a day 0 no date.
func TestJSDateV8Days(t *testing.T) {
	for in, want := range map[string]string{"2026-02-30": "2026-03-02T00:00:00.000Z", "0050-01-01": "0050-01-01T00:00:00.000Z",
		"2026-13-01": "", "2026-00-10": "", "2026-10-00": "", "2026-10-32": ""} {
		got, ok := jsDateV8(in)
		if want == "" && ok || want != "" && (!ok || jsISO(got) != want) {
			t.Errorf("%s: %v %s, want %q", in, ok, jsISO(got), want)
		}
	}
}

// TestVikunjaArchiveIsWeKans: a Vikunja export .zip - VERSION, data.json,
// filters.json and its files - reads as WeKan's parse of what its archive
// reader takes from it (testdata/import/generate-vikunja-html.mjs): the
// version checked as WeKan checks it, the saved filters reported. Negative:
// a .zip without data.json or VERSION is refused.
func TestVikunjaArchiveIsWeKans(t *testing.T) {
	raw, err := os.ReadFile("testdata/import/vikunja-archive.golden")
	if err != nil {
		t.Fatal(err)
	}
	gold, _ := decodeJS(raw)
	data, _ := os.ReadFile("testdata/import/vikunja-edge.json")
	zipOf := func(files map[string]string) string {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		for name, text := range files {
			w, _ := zw.Create(name)
			_, _ = w.Write([]byte(text))
		}
		_ = zw.Close()
		return buf.String()
	}
	for _, kv := range gold.(jsObject) {
		z := zipOf(map[string]string{"VERSION": kv.Key + "\n", "data.json": string(data), "filters.json": "[{},{},{}]",
			"files/1": "a", "files/2": "b", "files/x": "not counted"})
		got, err := importTextParsers["vikunja"](z)
		if e, isErr := jsGet(kv.Value, "error").(string); isErr {
			if err == nil || err.Error() != e {
				t.Errorf("%s: Weddy %v, WeKan %q", kv.Key, err, e)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", kv.Key, err)
		}
		if g, w := string(jsJSON(got, "")), string(jsJSON(kv.Value, "")); g != w {
			t.Errorf("%s:\nWeddy %s\nWeKan %s", kv.Key, g, w)
		}
	}
	for name, files := range map[string]map[string]string{"no data.json": {"VERSION": "v0.24.1"}, "no VERSION": {"data.json": "[]"}} {
		if _, err := importTextParsers["vikunja"](zipOf(files)); err == nil {
			t.Errorf("negative: a .zip with %s is imported", name)
		}
	}
}
