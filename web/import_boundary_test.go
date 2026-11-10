package web

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// TestImportBoundaryIsWeKans: Weddy's import boundary gives what Node.js
// WeKan's sanitizeTransferValue with sanitizeInput gives
// (testdata/import/generate-sanitize.mjs) - the same text kept, the same
// markup, script URLs and CSS removed, the same prototype keys dropped, the
// key order kept, and the same warnings for Admin Panel -> Problems.
func TestImportBoundaryIsWeKans(t *testing.T) {
	in, err := os.ReadFile("testdata/import/sanitize-cases.json")
	if err != nil {
		t.Fatal(err)
	}
	gold, err := os.ReadFile("testdata/import/sanitize.golden")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := decodeJS(in)
	if err != nil {
		t.Fatal(err)
	}
	var want struct {
		Strings  []string
		Transfer struct {
			Value    json.RawMessage
			Warnings []transferWarning
		}
	}
	if err := json.Unmarshal(gold, &want); err != nil {
		t.Fatal(err)
	}
	for i, s := range jsGet(doc, "strings").([]any) {
		got, _, err := transfer(s, false, true)
		if err != nil {
			t.Fatal(err)
		}
		if got != want.Strings[i] {
			t.Errorf("%q: Weddy %q, WeKan %q", s, got, want.Strings[i])
		}
	}
	got, warnings, err := transfer(jsGet(doc, "transfer"), false, true)
	if err != nil {
		t.Fatal(err)
	}
	gotJSON := jsJSON(got, "")
	wantDoc, _ := decodeJS(want.Transfer.Value)
	wantJSON := jsJSON(wantDoc, "")
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("transfer:\nWeddy %s\nWeKan %s", gotJSON, wantJSON)
	}
	if !reflect.DeepEqual(warnings, want.Transfer.Warnings) {
		t.Errorf("warnings:\nWeddy %v\nWeKan %v", warnings, want.Transfer.Warnings)
	}
}

// TestImportBoundaryLimits: a document past WeKan's limits is refused, not
// written - nested deeper than 40, an array over 100,000, a string over 4 MB.
func TestImportBoundaryLimits(t *testing.T) {
	var deep any = "x"
	for i := 0; i < 45; i++ {
		deep = jsObject{{"k", deep}}
	}
	long := make([]byte, transferMaxString+1)
	for i := range long {
		long[i] = 'a'
	}
	for name, v := range map[string]any{
		"depth":  deep,
		"array":  make([]any, transferMaxArray+1),
		"string": jsObject{{"title", string(long)}},
	} {
		if _, _, err := transfer(v, false, true); err == nil {
			t.Errorf("%s: accepted past the limit", name)
		}
	}
	// A short ordinary board passes untouched, with nothing to report.
	ok := jsObject{{"title", "Plain board"}, {"lists", []any{jsObject{{"title", "a < b"}}}}}
	got, warnings, err := transfer(ok, false, true)
	if err != nil || len(warnings) != 0 || !reflect.DeepEqual(got, ok) {
		t.Errorf("ordinary board changed: %v %v %v", got, warnings, err)
	}
}

// TestImportBoundaryOnThePage: what an imported file carries is cleaned
// before it is written - a Markdown text before its parser reads it, a Leo
// outline after (its tags are the outline) - and what was removed is one
// ImportExportSanitization row in Admin Panel -> Problems.
func TestImportBoundaryOnThePage(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	c := w.client(t)
	body(t, w.signIn(t, c, "ann", "pw"))
	res, err := c.PostForm(w.srv.URL+"/import/markdown", url.Values{"text": {"# Garden\n## To Do\n- [ ] Plant <script>alert(1)</script>now\n"}})
	if err != nil || res.StatusCode != http.StatusSeeOther {
		t.Fatalf("the import opens the new board: %v %d", err, res.StatusCode)
	}
	var b Board
	if err := w.db.Collection("boards").FindOne(ctx, bson.M{"title": "Garden"}).Decode(&b); err != nil {
		t.Fatal(err)
	}
	if n, _ := w.db.Collection("cards").CountDocuments(ctx, bson.M{"boardId": b.ID, "title": "Plant now"}); n != 1 {
		t.Error("the script went, its card stayed")
	}
	leo := `<?xml version="1.0"?><leo_file><vnodes><v t="a"><vh>Shed</vh><v t="b"><vh>Roof &lt;img src=x onerror=alert(1)&gt;</vh></v></v></vnodes>` +
		`<tnodes><t tx="a"></t><t tx="b">fix &amp; paint &lt;b&gt;soon&lt;/b&gt;</t></tnodes></leo_file>`
	res, err = c.PostForm(w.srv.URL+"/import/leo", url.Values{"text": {leo}})
	if err != nil || res.StatusCode != http.StatusSeeOther {
		t.Fatalf("the Leo import opens the new board: %v %d", err, res.StatusCode)
	}
	var cards []bson.M
	cur, _ := w.db.Collection("cards").Find(ctx, bson.M{"title": bson.M{"$regex": "^Roof"}})
	_ = cur.All(ctx, &cards)
	for _, card := range cards {
		if strings.Contains(docString(card, "title"), "<") || strings.Contains(docString(card, "description"), "<b>") {
			t.Errorf("markup in an imported outline card: %v", card)
		}
	}
	var row bson.M
	if err := w.db.Collection("eventlog").FindOne(ctx, bson.M{"bleed": "ImportExportSanitization", "source": "import:markdown"}).Decode(&row); err != nil {
		t.Fatalf("no Problems row for the removed script: %v", err)
	}
	if row["action"] != "sanitized" || row["category"] != "Import/export validation" {
		t.Errorf("the row: %v", row)
	}
	if err := w.db.Collection("eventlog").FindOne(ctx, bson.M{"bleed": "ImportExportSanitization", "source": "import:leo"}).Err(); err != nil {
		t.Errorf("no Problems row for the outline: %v", err)
	}
	// An ordinary import reports nothing.
	_, _ = c.PostForm(w.srv.URL+"/import/markdown", url.Values{"text": {"# Plain\n## A\n- [ ] a < b\n"}})
	if n, _ := w.db.Collection("eventlog").CountDocuments(ctx, bson.M{"bleed": "ImportExportSanitization"}); n != 2 {
		t.Errorf("an ordinary import was recorded: %d rows", n)
	}
}

// TestEveryImportPassesTheBoundary is the negative test: no import path
// anywhere in Weddy reaches a parser or a board writer with a file's data
// that has not passed secureImport. A function that calls a source parser
// out of importParsers or importTextParsers, writes with ImportBoard or
// importBoardWith, or
// hands a parsed board to importGeneral must itself call secureImport - or,
// for a copy of WeKan's own board, plainDocs. And no import cleans without
// recording, through sanitizeTransfer(..., false).
func TestEveryImportPassesTheBoundary(t *testing.T) {
	fset := token.NewFileSet()
	files, _ := filepath.Glob("*.go")
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if regexp.MustCompile(`sanitizeTransfer\([^()]*,\s*false\)`).Match(src) {
			t.Errorf("%s: an import cleaned by sanitizeTransfer, unrecorded - use secureImport", name)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Name.Name == "secureImport" || fn.Name.Name == "ImportBoard" {
				continue
			}
			parsers := map[string]bool{}
			needs, secured, copies := "", false, false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.AssignStmt:
					if ix, ok := x.Rhs[0].(*ast.IndexExpr); ok && len(x.Rhs) == 1 {
						if id, ok := ix.X.(*ast.Ident); ok && (id.Name == "importParsers" || id.Name == "importTextParsers") {
							if l, ok := x.Lhs[0].(*ast.Ident); ok && l.Name != "_" {
								parsers[l.Name] = true
							}
						}
					}
				case *ast.CallExpr:
					switch fun := x.Fun.(type) {
					case *ast.IndexExpr:
						if id, ok := fun.X.(*ast.Ident); ok && (id.Name == "importParsers" || id.Name == "importTextParsers") {
							needs = "calls " + id.Name
						}
					case *ast.Ident:
						if parsers[fun.Name] {
							needs = "calls a source parser"
						}
						if fun.Name == "plainDocs" {
							copies = true
						}
					case *ast.SelectorExpr:
						switch fun.Sel.Name {
						case "ImportBoard", "importBoardWith", "importGeneral":
							needs = "calls " + fun.Sel.Name
						case "secureImport":
							secured = true
						}
					}
				}
				return true
			})
			if needs != "" && !secured && !(copies && fn.Name.Name == "apiCopyBoard") {
				t.Errorf("%s: %s %s without secureImport", name, fn.Name.Name, needs)
			}
		}
	}
}

// TestImportBoardMethod is WeKan's importBoard method for the sources Weddy
// parses: what is not the source's document is refused as
// invalid-import-format without creating a board (WeKan's
// leo-outline-import.e2e.js), a JSON source given text and a text source
// given an object too, and a real outline becomes a board, cleaned.
func TestImportBoardMethod(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	c := w.client(t)
	body(t, w.signIn(t, c, "ann", "pw"))
	callImport := func(args string) map[string]any {
		req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/importBoard", strings.NewReader(args))
		req.Header.Set("Content-Type", "application/json")
		res, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		return out
	}
	for _, args := range []string{
		`["# Markdown\n- [ ] x", {}, "leo"]`,
		`["<leo_file></leo_file>", {}, "leo"]`,
		`["", {}, "leo"]`,
		`[{"vnodes": []}, {}, "leo"]`,
		`["   ", {}, "markdown"]`,
		`["{\"tasks\": []}", {}, "kanboard"]`,
		`[{"columns": "x"}, {}, "kanri"]`,
	} {
		if out := callImport(args); out["error"] != "invalid-import-format" {
			t.Errorf("%s: %v", args, out)
		}
	}
	if n, _ := w.db.Collection("boards").CountDocuments(ctx, bson.M{}); n != 0 {
		t.Fatalf("a refused import created %d boards", n)
	}
	leo := `<leo_file><vnodes><v t="a"><vh>Shed</vh><v t="b"><vh>Roof</vh></v></v></vnodes><tnodes><t tx="b">fix &amp; a &lt; b &lt;script&gt;x&lt;/script&gt;</t></tnodes></leo_file>`
	arg, _ := json.Marshal([]any{leo, map[string]any{}, "leo", nil})
	out := callImport(string(arg))
	id, _ := out["result"].(string)
	if id == "" {
		t.Fatalf("a Leo outline: %v", out)
	}
	var card bson.M
	if err := w.db.Collection("cards").FindOne(ctx, bson.M{"boardId": id, "title": "Roof"}).Decode(&card); err != nil {
		t.Fatal(err)
	}
	if got := docString(card, "description"); got != "fix &amp; a &lt; b" {
		t.Errorf("the body, cleaned as WeKan's: %q", got)
	}
	if out := callImport(`["x", {}, "trello"]`); out["error"] != "not-implemented" {
		t.Errorf("a source Weddy does not parse here: %v", out)
	}
}
