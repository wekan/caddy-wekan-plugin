package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// importBoard's csv and excel sources, as WeKan's: rows or { excelBase64 }
// with data.csvMapping checked against the header - a column the file does
// not have is invalid-import-mapping, a mapping of another shape a Match
// error, and neither makes a board - and a workbook of many sheets a board
// a sheet, the first one's id answered.
func TestImportBoardCsvAndExcel(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	c := w.client(t)
	body(t, w.signIn(t, c, "ann", "pw"))
	call := func(args any) map[string]any {
		raw, _ := json.Marshal(args)
		req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/importBoard", bytes.NewReader(raw))
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
	boards := func() int64 { n, _ := w.db.Collection("boards").CountDocuments(ctx, bson.M{}); return n }
	rows := []any{[]any{"Title", "List"}, []any{"A", "Doing"}, []any{"", nil}, []any{"B", 7}}
	for _, m := range []any{map[string]any{"columns": map[string]any{"title": 5}}, map[string]any{"columns": map[string]any{"nope": 0}},
		map[string]any{"columns": map[string]any{"title": "0"}}, map[string]any{"extra": 1}, map[string]any{"columns": map[string]any{"title": 0.5}},
		map[string]any{"listName": 3}, map[string]any{"customFieldColumns": []any{0}, "columns": map[string]any{"title": 0}}} {
		if out := call([]any{rows, map[string]any{"csvMapping": m}, "csv"}); out["result"] != nil || out["error"] == nil {
			t.Errorf("negative: the mapping %v is refused: %v", m, out)
		}
	}
	if out := call([]any{rows, map[string]any{"csvMapping": map[string]any{"columns": map[string]any{"title": 5}}}, "csv"}); out["error"] != "invalid-import-mapping" {
		t.Errorf("a column the file has not: invalid-import-mapping: %v", out)
	}
	for _, bad := range []any{[]any{}, []any{"Title"}, "Title\nA", map[string]any{}} {
		if out := call([]any{bad, map[string]any{}, "csv"}); out["error"] != "invalid-import-format" {
			t.Errorf("negative: %v is no CSV import: %v", bad, out)
		}
	}
	if n := boards(); n != 0 {
		t.Fatalf("negative: no board is made: %d", n)
	}
	out := call([]any{rows, map[string]any{"csvMapping": map[string]any{"columns": map[string]any{"title": 0, "list": 1}}}, "csv"})
	id, _ := out["result"].(string)
	var b bson.M
	if err := w.db.Collection("boards").FindOne(ctx, bson.M{"_id": id}).Decode(&b); err != nil {
		t.Fatalf("the board: %v", out)
	}
	if title := strOf(b["title"]); !strings.HasPrefix(title, "Imported Board ") || !strings.HasSuffix(title, " GMT+0000 (Coordinated Universal Time)") {
		t.Errorf("CsvCreator's title: %q", title)
	}
	if n, _ := w.db.Collection("cards").CountDocuments(ctx, bson.M{"boardId": id}); n != 2 {
		t.Errorf("two cards, the empty row left out: %d", n)
	}
	var seven bson.M
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"boardId": id, "title": "B"}).Decode(&seven)
	var list bson.M
	_ = w.db.Collection("lists").FindOne(ctx, bson.M{"_id": seven["listId"]}).Decode(&list)
	if list["title"] != "7" {
		t.Errorf("a number cell is its text: %v", list["title"])
	}
	// No mapping: the header names say it.
	if out := call([]any{[]any{[]any{"Name", "Status"}, []any{"Guessed", "Later"}}, map[string]any{}, "csv"}); out["result"] == nil {
		t.Errorf("no mapping: guessed: %v", out)
	}
	// A workbook: a board a sheet.
	data, _ := os.ReadFile("testdata/import/sheets/sheets.xlsx")
	before := boards()
	out = call([]any{map[string]any{"excelBase64": base64.StdEncoding.EncodeToString(data)},
		map[string]any{"csvMapping": map[string]any{"columns": map[string]any{"title": 1, "list": 3}, "listName": " Inbox "}}, "excel"})
	id, _ = out["result"].(string)
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": id}).Decode(&b)
	if b["title"] != "Board One title" || boards() != before+4 {
		t.Errorf("four boards, the first answered: %v %v %d", out, b["title"], boards()-before)
	}
	for _, bad := range []any{map[string]any{"excelBase64": ""}, map[string]any{"excelBase64": "bm90IGEgemlw"}, []any{}} {
		if out := call([]any{bad, map[string]any{}, "excel"}); out["error"] != "invalid-import-format" {
			t.Errorf("negative: %v is no workbook: %v", bad, out)
		}
	}
	// Admin Panel's import switch holds for these too.
	_, _ = w.db.Collection("settings").UpdateOne(ctx, bson.M{}, bson.M{"$set": bson.M{"disableAllImport": true}}, options.UpdateOne().SetUpsert(true))
	if out := call([]any{rows, map[string]any{}, "csv"}); out["error"] != "import-disabled" {
		t.Errorf("negative: import disabled: %v", out)
	}
}
