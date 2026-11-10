package web

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// The export markdown of every case in testdata/export/markdown-cases.json,
// as Node.js WeKan's exportMarkdown.js answers it (generate-markdown.mjs):
// its blocks, its one-cell runs and its plain text.
func TestExportMarkdownIsWeKans(t *testing.T) {
	raw, err := os.ReadFile("testdata/export/markdown.golden")
	if err != nil {
		t.Fatal(err)
	}
	var golden []struct {
		Source string          `json:"source"`
		Blocks json.RawMessage `json:"blocks"`
		Runs   json.RawMessage `json:"runs"`
		Plain  string          `json:"plain"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil || len(golden) < 30 {
		t.Fatalf("the golden: %v %d", err, len(golden))
	}
	same := func(got any, want json.RawMessage) (bool, string) {
		g, _ := json.Marshal(got)
		var a, b any
		_ = json.Unmarshal(g, &a)
		_ = json.Unmarshal(want, &b)
		return reflect.DeepEqual(a, b), string(g)
	}
	for _, c := range golden {
		if ok, g := same(markdownBlocks(c.Source), c.Blocks); !ok {
			t.Errorf("%q blocks:\n%s\nnot WeKan's\n%s", c.Source, g, c.Blocks)
			continue
		}
		if ok, g := same(markdownRuns(c.Source), c.Runs); !ok {
			t.Errorf("%q runs:\n%s\nnot WeKan's\n%s", c.Source, g, c.Runs)
		}
		if got := markdownPlainText(c.Source); got != c.Plain {
			t.Errorf("%q plain: %q, not %q", c.Source, got, c.Plain)
		}
	}
}
