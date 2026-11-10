package web

import (
	"context"
	"io"
	"net/http"
	"slices"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// A board's Excel export in WeKan's table layout (ExporterExcel.js): the
// title in A1, the description in row 3, dates and members in row 5, the
// header in row 7 in the user's language, a card a row, and the Activity
// sheet of comments - and it reads back as WeKan's Excel import reads it,
// the board's own sheet a board and Activity skipped.
func TestExcelExportIsWeKansTable(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	ann := w.seedUser(t, "ann", "pw")
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": ann.ID}, bson.M{"$set": bson.M{"profile.language": "fi"}})
	a := login(t, w, "ann")
	b := newRESTBoard(t, w, a, "Taulu")
	due := time.Date(2026, 10, 10, 8, 30, 0, 0, time.UTC)
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"description": "Mitä taulu on"}})
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": b.Card}, bson.M{"$set": bson.M{"title": "Kortti", "description": "Kuvaus",
		"dueAt": due, "members": bson.A{ann.ID, "ghostuser000001"}, "spentTime": 2.5, "requestedBy": "Bea"}})
	call(t, "POST", w.srv.URL+"/api/boards/"+b.ID+"/cards/"+b.Card+"/comments", a, `{"comment":"Valmis"}`, nil)
	// The table: Card details unticked (exportExcel.js boardExcelExporter).
	req, _ := http.NewRequest("GET", w.srv.URL+"/api/boards/"+b.ID+"/exportExcel?fields=board-header,people,dates", nil)
	req.Header.Set("Authorization", "Bearer "+a)
	res, err := testClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(res.Body)
	res.Body.Close()
	sheets, err := readXlsxValues(data)
	if err != nil || len(sheets) != 2 {
		t.Fatalf("two sheets: %v %d", err, len(sheets))
	}
	cell := func(sheet, row, col int) string {
		rows := sheets[sheet].Rows
		if row-1 >= len(rows) || col-1 >= len(rows[row-1]) {
			return ""
		}
		return sheetCellString(sheetCellValue(rows[row-1][col-1]))
	}
	fi := func(k string) string { return tr("fi", k) }
	for _, c := range []struct {
		sheet, row, col int
		want            string
	}{
		{0, 1, 1, "Taulu"}, {0, 3, 1, fi("description")}, {0, 3, 2, "Mitä taulu on"}, {0, 5, 1, fi("createdAt")},
		{0, 5, 3, fi("modifiedAt")}, {0, 5, 5, fi("members")}, {0, 5, 6, "ann"},
		{0, 7, 1, fi("number")}, {0, 7, 2, fi("title")}, {0, 7, 20, fi("spent-time-hours")},
		{0, 8, 1, "1"}, {0, 8, 2, "Kortti"}, {0, 8, 3, "Kuvaus"}, {0, 8, 5, "ann"}, {0, 8, 10, "2026-10-10T08:30:00.000Z"},
		{0, 8, 15, "ann undefined "}, {0, 8, 16, "Bea"}, {0, 8, 19, "false"}, {0, 8, 20, "2.5"},
		{1, 1, 1, "Taulu"}, {1, 3, 2, fi("activity")}, {1, 4, 1, "1"}, {1, 4, 2, "Valmis"}, {1, 4, 3, "Kortti"}, {1, 4, 4, "ann"},
	} {
		if got := cell(c.sheet, c.row, c.col); got != c.want {
			t.Errorf("sheet %d row %d col %d: %q, not %q", c.sheet+1, c.row, c.col, got, c.want)
		}
	}
	if sheets[1].Name != fi("activity") {
		t.Errorf("the Activity sheet is named in Finnish: %q", sheets[1].Name)
	}
	boards, skipped := excelSheetBoards(sheets)
	if len(boards) != 1 || boards[0].Title != "Taulu" || !slices.Equal(skipped, []string{fi("activity")}) {
		t.Fatalf("it reads back as one board, Activity skipped: %+v %q", boards, skipped)
	}
	m := guessTableMapping(boards[0].Rows[0]).Columns
	if m["title"] != 1 || m["list"] != 11 || m["dueAt"] != 9 {
		t.Errorf("WeKan's Excel layout, by position: %v", m)
	}
	// Negative: a title over 31 characters is cut at a word, as WeKan's.
	if got := excelSheetTitle("A board with a title much longer than thirty one"); got != "A board with a title much ..." {
		t.Errorf("sheet title: %q", got)
	}
	if got := excelSheetTitle("Averyveryverylongtitlewithoutanyspacesatall"); got != "Averyveryverylongtitlewitho ..." {
		t.Errorf("one word: %q", got)
	}
}
