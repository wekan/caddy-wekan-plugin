package web

// Code scanning, 2026-11-21, go/sql-injection (#1-#174): every value a filter
// is given from a request, a stored document or an import is a copy the code
// made - queryText's bytes, queryID's id characters, a number - compared as a
// value, never a document or an operator. Each fix has a test of what it does,
// a negative test of what it refuses, and a scan of the whole package for the
// shape it removed, so the shape cannot come back anywhere.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// funcSource is the source of the function or method name in file.
func funcSource(t *testing.T, file, name string) string {
	t.Helper()
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	fs := token.NewFileSet()
	f, err := parser.ParseFile(fs, file, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == name {
			return string(src[fs.Position(fn.Pos()).Offset:fs.Position(fn.End()).Offset])
		}
	}
	t.Fatalf("%s has no func %s", file, name)
	return ""
}

func TestFilterValuesAreCopiedText(t *testing.T) {
	for _, v := range []string{"", "olga", "a@example.com", "pässwörd", "\xff\xfe invalid UTF-8", `{"$ne":""}`, "$where"} {
		if got := queryText(v); got != v {
			t.Errorf("queryText(%q) = %q: every byte the same", v, got)
		}
		if got := eq(v)["$eq"]; got != v {
			t.Errorf("eq(%q) is $eq of %q", v, got)
		}
	}
	for _, id := range []string{"ZaR7q3xHdQ9wW2sKp", "board940e4fb88965", "a_b-c", strings.Repeat("Z", 64)} {
		if got, ok := queryID(id); !ok || got != id {
			t.Errorf("queryID(%q) = %q, %v", id, got, ok)
		}
	}
	for _, bad := range []string{"", `{"$ne":""}`, "$where", "a.b", "a b", strings.Repeat("a", 65), "a\x00", "ä"} {
		if got, ok := queryID(bad); ok || got != "" {
			t.Errorf("negative: queryID(%q) = %q, %v", bad, got, ok)
		}
	}
	if m, ok := inIDs([]string{"a", "b_c"}); !ok || !slices.Equal(m["$in"].([]string), []string{"a", "b_c"}) {
		t.Errorf("inIDs of ids: %v %v", m, ok)
	}
	if m, ok := inIDs([]string{"a", "$ne"}); ok || m != nil {
		t.Errorf("negative: inIDs with a value that is no id: %v %v", m, ok)
	}
	if got := dbIn(bson.A{"a", bson.M{"$ne": ""}, "a.b", int32(3), "b"})["$in"].([]string); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("negative: dbIn keeps only the ids: %v", got)
	}
	for _, c := range []struct {
		got  bson.M
		want string
	}{
		{exactFold("a+b@x.io"), `^a\+b@x\.io$`},
		{containsFold("a.b*"), `a\.b\*`},
		{prefixFold("(jan"), `^\(jan`},
	} {
		if c.got["$regex"] != c.want || c.got["$options"] != "i" {
			t.Errorf("a text matched as text: %v, want %q", c.got, c.want)
		}
	}

	// Every filter helper builds its value through queryText or queryID.
	for _, h := range []struct{ file, fn, through string }{
		{"model.go", "eq", "queryText("},
		{"model.go", "exactFold", "queryText("},
		{"model.go", "containsFold", "queryText("},
		{"model.go", "prefixFold", "queryText("},
		{"model.go", "inIDs", "queryID("},
		{"export.go", "dbIn", "queryID("},
	} {
		if !strings.Contains(funcSource(t, h.file, h.fn), h.through) {
			t.Errorf("%s: %s makes its value without %s", h.file, h.fn, h.through)
		}
	}
	if strings.Contains(funcSource(t, "model.go", "queryText"), "range v") {
		t.Error("queryText copies bytes, not runes: an invalid UTF-8 byte stays itself")
	}

	// Nowhere in the package is a quoted text a pattern on its own: a $regex
	// or a bson.Regex made of a quoted text is made through queryText - or is
	// one of the helpers above.
	quoted := regexp.MustCompile(`("\$regex":|bson\.Regex\{Pattern:)\s*[^,}]*(regexp\.QuoteMeta|jsEscapeRegExp)\(`)
	files, _ := filepath.Glob("*.go")
	var offenders []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, _ := os.ReadFile(f)
		for i, line := range strings.Split(string(src), "\n") {
			if quoted.MatchString(line) && !strings.Contains(line, "queryText(") {
				offenders = append(offenders, f+":"+itoa(i+1)+" "+strings.TrimSpace(line))
			}
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("a quoted text as a pattern, not through queryText or a helper:\n%s", strings.Join(offenders, "\n"))
	}
	for _, old := range []string{
		`rx := bson.M{"$regex": regexp.QuoteMeta(search), "$options": "i"}`,
		`rx := bson.M{"$regex": "^" + regexp.QuoteMeta(q), "$options": "i"}`,
		`rx := bson.Regex{Pattern: regexp.QuoteMeta(f.Text), Options: "i"}`,
	} {
		if !quoted.MatchString(old) {
			t.Errorf("the scan misses the old shape %s", old)
		}
	}
}

// A stored revision is matched as the number or the text it is, and never a
// stored document as a filter of its own.
func TestScrumRevisionSelector(t *testing.T) {
	if got := scrumRevisionSelector(map[string]any{}); got["scrumRevision"].(bson.M)["$exists"] != false {
		t.Errorf("none stored: %v", got)
	}
	if got := scrumRevisionSelector(map[string]any{"scrumRevision": nil}); got["scrumRevision"] != nil {
		t.Errorf("a stored null: %v", got)
	}
	for _, v := range []any{int32(3), int64(3), 3, 3.0, true} {
		if got := scrumRevisionSelector(map[string]any{"scrumRevision": v}); got["scrumRevision"].(bson.M)["$eq"] != v {
			t.Errorf("%T %v is matched with $eq: %v", v, v, got)
		}
	}
	if got := scrumRevisionSelector(map[string]any{"scrumRevision": "3"}); got["scrumRevision"].(bson.M)["$eq"] != "3" {
		t.Errorf("a revision stored as text: %v", got)
	}
	for _, v := range []any{bson.M{"$ne": 0}, map[string]any{"$gt": -1}, bson.A{1}} {
		got := scrumRevisionSelector(map[string]any{"scrumRevision": v})
		if _, has := got["scrumRevision"]; has || len(got["_id"].(bson.M)["$in"].(bson.A)) != 0 {
			t.Errorf("negative: %v stored as the revision is no filter, and matches nothing: %v", v, got)
		}
	}
}

// The label filter is the board's own label ids, whatever the address says.
func TestBoardFilterLabelsAreTheBoards(t *testing.T) {
	b := &Board{Labels: []Label{{ID: "L1"}, {ID: "L2"}}}
	f := parseBoardFilter(map[string][]string{"labels": {"L1,$ne,L1,L3," + noLabel}, "xlabels": {"L2,{\"$gt\":\"\"}"}}, b)
	if !slices.Equal(f.Labels, []string{"L1"}) || !f.NoLabel || !slices.Equal(f.Excluded, []string{"L2"}) {
		t.Fatalf("only the board's labels, once each: %+v", f)
	}
	if f := parseBoardFilter(map[string][]string{"labels": {"$ne"}}, b); len(f.Labels) != 0 || f.active() {
		t.Fatalf("negative: an unknown label is no filter: %+v", f)
	}
}
