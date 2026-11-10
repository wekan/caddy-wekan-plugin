package web

import (
	"bytes"
	"context"
	"html"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestGuessTableMapping(t *testing.T) {
	m := guessTableMapping([]string{"Title", "Description", "Status", "Tags", "Due date", "Notes2"})
	for f, want := range map[string]int{"title": 0, "description": 1, "list": 2, "labels": 3, "dueAt": 4} {
		if got, ok := m.Columns[f]; !ok || got != want {
			t.Errorf("%s: %d %v, want %d", f, got, ok, want)
		}
	}
	// A WeKan export made in Finnish names its columns in Finnish.
	fi := guessTableMapping([]string{tr("fi", "title"), tr("fi", "description"), tr("fi", "list")})
	if fi.Columns["title"] != 0 || fi.Columns["list"] != 2 {
		t.Errorf("Finnish headers: %+v", fi.Columns)
	}
	// No known title: the first ordinary column is the title.
	if m := guessTableMapping([]string{"Tehtävät ja muut", "Status"}); m.Columns["title"] != 0 || m.Columns["list"] != 1 {
		t.Errorf("first column: %+v", m.Columns)
	}
	if _, ok := guessTableMapping([]string{"CustomField-Size-text"}).Columns["title"]; ok {
		t.Error("negative: a custom field column is never the title")
	}
}

func TestCheckTableMapping(t *testing.T) {
	ok := tableMapping{Columns: map[string]int{"title": 0, "list": 1}, CustomFields: []int{2}}
	if err := checkTableMapping(ok, 3); err != nil {
		t.Fatal(err)
	}
	for name, m := range map[string]tableMapping{
		"no such column":          {Columns: map[string]int{"title": 3}},
		"negative column":         {Columns: map[string]int{"title": -1}},
		"no such field":           {Columns: map[string]int{"$set": 0}},
		"a field as custom field": {Columns: map[string]int{"title": 0}, CustomFields: []int{0}},
		"a custom field twice":    {Columns: map[string]int{"title": 0}, CustomFields: []int{1, 1}},
	} {
		if checkTableMapping(m, 3) == nil {
			t.Errorf("negative: %s is accepted", name)
		}
	}
}

func TestCSVRowsAndCells(t *testing.T) {
	for text, want := range map[string]string{"a,b\n1,2": ",", "a\tb,c\n": "\t", "a;b;c,d\n": ";"} {
		if got := string(csvSeparatorOf(text)); got != want {
			t.Errorf("separator of %q: %q", text, got)
		}
	}
	rows, err := csvRows("\uFEFFTitle;Status\n\n\"A; b\";Done\n")
	if err != nil || len(rows) != 2 || rows[1][0] != "A; b" || rows[0][0] != "Title" {
		t.Fatalf("%q %v", rows, err)
	}
	if l := splitLabels("bug-red ui-blue, plain"); len(l) != 3 || l[0] != (Label{Name: "bug", Color: "red"}) || l[2].Color != "black" {
		t.Errorf("labels: %+v", l)
	}
	if d, ok := cellDate("2026-03-04T05:06:07.000Z"); !ok || d.Day() != 4 {
		t.Error("ISO date")
	}
	if _, ok := cellDate("someday"); ok {
		t.Error("negative: no date")
	}
}

// Weddy's own Excel export reads back: an .xlsx with inline strings, and one
// with shared strings and a date-formatted number.
func TestReadXlsx(t *testing.T) {
	data, err := xlsxOf("Board", [][]any{{"Title", "Status"}, {"One", "Doing"}})
	if err != nil {
		t.Fatal(err)
	}
	sheets, err := readXlsx(data)
	if err != nil || len(sheets) != 1 || sheets[0].Name != "Board" {
		t.Fatalf("%+v %v", sheets, err)
	}
	if rows := sheets[0].Rows; len(rows) != 2 || rows[1][1] != "Doing" {
		t.Fatalf("rows: %q", rows)
	}
	if !dateFormat(14, nil) || dateFormat(2, nil) || !dateFormat(164, map[int]string{164: "yyyy-mm-dd"}) || dateFormat(165, map[int]string{165: `"day"0`}) {
		t.Error("date formats")
	}
	if d := excelDate(46000.5); d.Format(time.RFC3339) != "2025-12-09T12:00:00Z" {
		t.Errorf("serial day: %s", d)
	}
	if _, err := readXlsx([]byte("not a zip")); err == nil {
		t.Error("negative: not a workbook")
	}
	if _, _, ok := colIndex("A0"); ok {
		t.Error("negative: no row 0")
	}
	if c, r, ok := colIndex("AB12"); !ok || c != 27 || r != 11 {
		t.Errorf("AB12: %d %d", c, r)
	}
}

func TestImportPageExcelAndCSV(t *testing.T) {
	w := newTestWeKan(t)
	ann := w.seedUser(t, "ann", "pw")
	c := w.client(t)
	w.signIn(t, c, "ann", "pw")
	ctx := context.Background()

	res, page := get(t, c, w.srv.URL+"/import/excel")
	if res.StatusCode != 200 || !strings.Contains(page, `class="js-import-file"`) || !strings.Contains(page, `accept=".xlsx"`) {
		t.Fatalf("the first step: %d %s", res.StatusCode, page)
	}
	if res, _ := get(t, c, w.srv.URL+"/import/nosuch"); res.StatusCode != 404 {
		t.Errorf("negative: an import Weddy has no page for: %d", res.StatusCode)
	}
	xlsx, _ := xlsxOf("Board", [][]any{{"Title", "Description", "Status", "Members"}, {"ExcelCardOne", "imported from excel", "ToDoExcel", "ann stranger"}})
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	f, _ := mw.CreateFormFile("file", "board.xlsx")
	_, _ = f.Write(xlsx)
	_ = mw.Close()
	req, _ := http.NewRequest("POST", w.srv.URL+"/import/excel", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	page = body(t, res)
	if res.StatusCode != 200 || !strings.Contains(page, "js-csv-mapping-import") {
		t.Fatalf("the mapping step: %d %s", res.StatusCode, page)
	}
	if !regexp.MustCompile(`<option value="2" selected[^>]*>Status \(ToDoExcel\)`).MatchString(page) {
		t.Errorf("Status is guessed as the list: %s", page)
	}
	rows := html.UnescapeString(regexp.MustCompile(`(?s)<textarea name="rows"[^>]*>(.*?)</textarea>`).FindStringSubmatch(page)[1])
	form := url.Values{"rows": {rows}, "map-title": {"0"}, "map-description": {"1"}, "map-list": {"2"}, "map-members": {"3"}}
	res, _ = c.PostForm(w.srv.URL+"/import/excel/board", form)
	if res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(res.Header.Get("Location"), "/b/") {
		t.Fatalf("the board opens: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	var card bson.M
	if err := w.db.Collection("cards").FindOne(ctx, bson.M{"title": "ExcelCardOne"}).Decode(&card); err != nil {
		t.Fatal("the card is made")
	}
	var list bson.M
	_ = w.db.Collection("lists").FindOne(ctx, bson.M{"_id": card["listId"]}).Decode(&list)
	if list["title"] != "ToDoExcel" || card["description"] != "imported from excel" {
		t.Errorf("its list and description: %v %v", list["title"], card["description"])
	}
	if m, _ := card["members"].(bson.A); len(m) != 1 || m[0] != ann.ID {
		t.Errorf("negative: only the importer's own name is a member, never a stranger looked up: %v", card["members"])
	}

	// CSV pasted, with no list column: every card into the one list named.
	csvText := "Name,Notes\nFirst,a\nSecond,b\n"
	res, page = post(t, c, w.srv.URL+"/import/csv", url.Values{"text": {csvText}})
	if res.StatusCode != 200 || !strings.Contains(page, "js-csv-mapping-list-missing") {
		t.Fatalf("csv mapping: %d %s", res.StatusCode, page)
	}
	rows = `[["Name","Notes"],["First","a"],["Second","b"]]`
	res, _ = c.PostForm(w.srv.URL+"/import/csv/board", url.Values{"rows": {rows}, "map-title": {"0"}, "listName": {"Inbox"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("csv board: %d", res.StatusCode)
	}
	var first bson.M
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"title": "Second"}).Decode(&first)
	n, _ := w.db.Collection("lists").CountDocuments(ctx, bson.M{"boardId": first["boardId"]})
	if n != 1 {
		t.Errorf("one list for every card, not one a row: %d", n)
	}

	// Negatives: no title column, a column the file has not, and import off.
	if res, page := post(t, c, w.srv.URL+"/import/csv/board", url.Values{"rows": {rows}, "listName": {"Inbox"}}); res.StatusCode != 400 || !strings.Contains(page, tr("en", "csv-mapping-title-required")) {
		t.Errorf("negative: no title column: %d", res.StatusCode)
	}
	if res, _ := post(t, c, w.srv.URL+"/import/csv/board", url.Values{"rows": {rows}, "map-title": {"9"}, "listName": {"x"}}); res.StatusCode != 400 {
		t.Errorf("negative: a column the file has not: %d", res.StatusCode)
	}
	if res, _ := post(t, c, w.srv.URL+"/import/csv/board", url.Values{"rows": {rows}, "map-title": {"0"}}); res.StatusCode != 400 {
		t.Errorf("negative: no list column and no list name: %d", res.StatusCode)
	}
	// Signed out: the sign-in page.
	if res, _ := get(t, w.client(t), w.srv.URL+"/import/csv"); res.StatusCode != http.StatusSeeOther {
		t.Errorf("negative: signed out: %d", res.StatusCode)
	}
	_, _ = w.db.Collection("settings").UpdateOne(ctx, bson.M{}, bson.M{"$set": bson.M{"disableAllImport": true}})
	if res, _ := post(t, c, w.srv.URL+"/import/csv/board", url.Values{"rows": {rows}, "map-title": {"0"}, "listName": {"x"}}); res.StatusCode != 403 {
		t.Errorf("negative: import off: %d", res.StatusCode)
	}
}

func post(t *testing.T, c *http.Client, u string, form url.Values) (*http.Response, string) {
	t.Helper()
	res, err := c.PostForm(u, form)
	if err != nil {
		t.Fatal(err)
	}
	return res, body(t, res)
}
