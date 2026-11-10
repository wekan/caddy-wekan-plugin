package web

import (
	"bytes"
	"context"
	"encoding/json"
	"html"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The boards of a workbook and each board's guessed mapping, as Node.js
// WeKan's Excel import finds them (testdata/import/generate-sheets.mjs): a
// WeKan export's title above its Finnish header in row 7, the Activity,
// notes and empty sheets skipped, and a workbook with no header read from
// its first sheet with a value.
func TestExcelSheetBoardsAreWeKans(t *testing.T) {
	raw, err := os.ReadFile("testdata/import/sheets.golden")
	if err != nil {
		t.Fatal(err)
	}
	var golden map[string]struct {
		Boards   []sheetBoard `json:"boards"`
		Skipped  []string     `json:"skipped"`
		Mappings []struct {
			Columns map[string]int `json:"columns"`
		} `json:"mappings"`
	}
	var headers struct {
		Headers []struct {
			Header  []string `json:"header"`
			Mapping struct {
				Columns map[string]int `json:"columns"`
			} `json:"mapping"`
		} `json:"headers"`
	}
	if err := json.Unmarshal(raw, &headers); err != nil || len(headers.Headers) == 0 {
		t.Fatal("the headers", err)
	}
	for _, h := range headers.Headers {
		if got := guessTableMapping(h.Header).Columns; !reflect.DeepEqual(got, h.Mapping.Columns) {
			t.Errorf("%q: mapping %v, not WeKan's %v", h.Header, got, h.Mapping.Columns)
		}
	}
	var whole map[string]json.RawMessage
	_ = json.Unmarshal(raw, &whole)
	delete(whole, "headers")
	raw, _ = json.Marshal(whole)
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	for name, want := range golden {
		data, err := os.ReadFile("testdata/import/sheets/" + name)
		if err != nil {
			t.Fatal(err)
		}
		sheets, err := readXlsxValues(data)
		if err != nil {
			t.Fatal(err)
		}
		boards, skipped := excelSheetBoards(sheets)
		if !reflect.DeepEqual(boards, want.Boards) {
			g, _ := json.Marshal(boards)
			w, _ := json.Marshal(want.Boards)
			t.Errorf("%s: boards\n%s\nnot WeKan's\n%s", name, g, w)
		}
		if !reflect.DeepEqual(skipped, want.Skipped) {
			t.Errorf("%s: skipped %q, not %q", name, skipped, want.Skipped)
		}
		for i, b := range boards {
			if i < len(want.Mappings) {
				if got := guessTableMapping(b.Rows[0]).Columns; !reflect.DeepEqual(got, want.Mappings[i].Columns) {
					t.Errorf("%s board %d: mapping %v, not WeKan's %v", name, i, got, want.Mappings[i].Columns)
				}
			}
		}
	}
}

// Negative: a header is never taken from a row without a title column, nor
// from the Activity sheet's, nor below row 1 with fewer than four names.
func TestFindHeaderRowNegative(t *testing.T) {
	for _, rows := range [][][]string{
		{{"Notes"}, {"Title", "x"}},
		{{"Activity", "Card", "Title"}},
		{{"Description", "List", "Labels", "Swimlane"}},
		{},
	} {
		if at := findHeaderRow(rows); at != -1 {
			t.Errorf("%q: header at %d", rows, at)
		}
	}
	if at := findHeaderRow([][]string{{"Board"}, {}, {"Title", "Description", "List", "Labels"}}); at != 2 {
		t.Errorf("four names in row 3 are its header: %d", at)
	}
}

// The import page with a workbook of many sheets, as WeKan's: the mapping
// step names the boards and the skipped sheets, and Import makes a board of
// each - the confirmed mapping for the sheets with the first one's header,
// the column names for another - and opens the first.
func TestImportPageWorkbookOfManySheets(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	c := w.client(t)
	w.signIn(t, c, "ann", "pw")
	ctx := context.Background()
	data, err := os.ReadFile("testdata/import/sheets/sheets.xlsx")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	f, _ := mw.CreateFormFile("file", "sheets.xlsx")
	_, _ = f.Write(data)
	_ = mw.Close()
	req, _ := http.NewRequest("POST", w.srv.URL+"/import/excel", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, res)
	if res.StatusCode != 200 {
		t.Fatalf("the mapping step: %d", res.StatusCode)
	}
	if !strings.Contains(page, `js-csv-mapping-boards">`+tr("en", "csv-mapping-boards")+` Board One title, Second, Other columns, Same header board<`) {
		t.Errorf("the step names the boards: %s", page)
	}
	if !strings.Contains(page, `js-csv-mapping-skipped">`+tr("en", "csv-mapping-skipped-sheets")+` Activity, Notes, Empty<`) {
		t.Errorf("the step names the skipped sheets")
	}
	field := func(name string) string {
		m := regexp.MustCompile(`(?s)<textarea name="` + name + `"[^>]*>(.*?)</textarea>`).FindStringSubmatch(page)
		if m == nil {
			t.Fatalf("no %s", name)
		}
		return html.UnescapeString(m[1])
	}
	title := html.UnescapeString(regexp.MustCompile(`name="boardTitle" value="([^"]*)"`).FindStringSubmatch(page)[1])
	if title != "Board One title" {
		t.Errorf("the first board's title is its A1: %q", title)
	}
	// Title, Description and List chosen; no list for Board One's empty rows.
	form := url.Values{"rows": {field("rows")}, "sheets": {field("sheets")}, "boardTitle": {title},
		"map-title": {"1"}, "map-description": {"2"}, "map-list": {"3"}, "listName": {"Inbox"}}
	res, _ = c.PostForm(w.srv.URL+"/import/excel/board", form)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("the import: %d", res.StatusCode)
	}
	listOf := func(boardTitle, cardTitle string) string {
		var b, card, list bson.M
		if err := w.db.Collection("boards").FindOne(ctx, bson.M{"title": boardTitle}).Decode(&b); err != nil {
			t.Errorf("the board %q is made", boardTitle)
			return ""
		}
		if err := w.db.Collection("cards").FindOne(ctx, bson.M{"boardId": b["_id"], "title": cardTitle}).Decode(&card); err != nil {
			t.Errorf("%q has the card %q", boardTitle, cardTitle)
			return ""
		}
		_ = w.db.Collection("lists").FindOne(ctx, bson.M{"_id": card["listId"]}).Decode(&list)
		return strOf(list["title"])
	}
	var first bson.M
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"title": "Board One title"}).Decode(&first)
	if loc := res.Header.Get("Location"); !strings.HasPrefix(loc, "/b/"+strOf(first["_id"])+"/") {
		t.Errorf("the first board opens: %s", loc)
	}
	for _, c := range []struct{ board, card, list string }{
		{"Board One title", "Rich card", "To do"}, {"Board One title", "Linked", "Done"},
		{"Second", "Two A", "Doing"}, {"Second", "Two B", "Doing"},
		// Its own columns, read by their names: Status is the list.
		{"Other columns", "Three", "Backlog"},
		// The first board's header: the confirmed mapping.
		{"Same header board", "Copied", "To do"},
	} {
		if got := listOf(c.board, c.card); got != c.list {
			t.Errorf("%s / %s: list %q, not %q", c.board, c.card, got, c.list)
		}
	}
	for _, name := range []string{"Activity", "Notes", "Empty"} {
		if n, _ := w.db.Collection("boards").CountDocuments(ctx, bson.M{"title": name}); n != 0 {
			t.Errorf("negative: the %s sheet is no board", name)
		}
	}
	// Negative: a CSV import takes no other boards with it.
	before, _ := w.db.Collection("boards").CountDocuments(ctx, bson.M{})
	res, _ = c.PostForm(w.srv.URL+"/import/csv/board", url.Values{"rows": {`[["Title"],["Solo"]]`}, "map-title": {"0"},
		"listName": {"Inbox"}, "sheets": {field("sheets")}})
	after, _ := w.db.Collection("boards").CountDocuments(ctx, bson.M{})
	if res.StatusCode != http.StatusSeeOther || after != before+1 {
		t.Errorf("negative: a CSV import is one board: %d, %d -> %d", res.StatusCode, before, after)
	}
}
