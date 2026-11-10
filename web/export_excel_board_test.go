package web

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// A board's Excel export with Card details, WeKan's default
// (ExporterExcelBoard.js): the board's title, members and dates, a gray
// heading a swimlane and a list, and every card in the card layout - its
// sections, a checklist's progress, a dropdown's name, an attached picture -
// with dates in the reader's format and zone; the selection leaves sections
// out; a board with admin-only custom fields is exported by its admins only.
func TestExcelExportCardDetails(t *testing.T) {
	w := newTestWeKan(t)
	w.s.FilesDir = filepath.Join(t.TempDir(), "files")
	ctx := context.Background()
	ann := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	a, bt := login(t, w, "ann"), login(t, w, "bob")
	b := newRESTBoard(t, w, a, "Kanban")
	setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: true})
	due := time.Date(2026, 10, 10, 21, 30, 0, 0, time.UTC)
	_, _ = w.db.Collection("customFields").InsertOne(ctx, bson.M{"_id": "fieldPriority01", "boardIds": bson.A{b.ID}, "name": "Priority", "type": "dropdown", // inserted, not a filter
		"settings": bson.M{"dropdownItems": bson.A{bson.M{"_id": "itemHigh000001", "name": "High"}}}})
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": b.Card}, bson.M{"$set": bson.M{"title": "Write the plan",
		"description": "**Bold** plan", "dueAt": due, "members": bson.A{ann.ID},
		"customFields": bson.A{bson.M{"_id": "fieldPriority01", "value": "itemHigh000001"}}}})
	_, _ = w.db.Collection("checklists").InsertOne(ctx, bson.M{"_id": "checklistAAAA01", "cardId": b.Card, "boardId": b.ID, "title": "Steps", "sort": 0.0}) // inserted, not a filter
	for i, done := range []bool{true, false} {
		_, _ = w.db.Collection("checklistItems").InsertOne(ctx, bson.M{"_id": "itemAAAAAAAAA0" + string(rune('1'+i)), "checklistId": "checklistAAAA01", // inserted, not a filter
			"cardId": b.Card, "title": []string{"First", "Second"}[i], "isFinished": done, "sort": float64(i)})
	}
	call(t, "POST", w.srv.URL+"/api/boards/"+b.ID+"/cards/"+b.Card+"/comments", a, `{"comment":"A *remark*"}`, nil)
	png := []byte("\x89PNG\r\n\x1a\nnot really a picture")
	_ = os.MkdirAll(w.s.attachmentsDir(), 0o755)
	_ = os.WriteFile(filepath.Join(w.s.attachmentsDir(), "attachPicture01"), png, 0o644)
	_, _ = w.db.Collection("attachments").InsertOne(ctx, bson.M{"_id": "attachPicture01", "name": "photo.png", "type": "image/png", // inserted, not a filter
		"size": int64(len(png)), "userId": ann.ID, "meta": bson.M{"boardId": b.ID, "cardId": b.Card}})
	get := func(query, token string) (int, []byte) {
		req, _ := http.NewRequest("GET", w.srv.URL+"/api/boards/"+b.ID+"/exportExcel"+query, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := testClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, _ := io.ReadAll(res.Body)
		return res.StatusCode, data
	}
	texts := func(data []byte) string {
		sheets, err := readXlsxValues(data)
		if err != nil || len(sheets) != 1 {
			t.Fatalf("one sheet: %v", err)
		}
		var out []string
		for _, row := range sheets[0].Rows {
			for _, c := range row {
				if s := sheetCellString(sheetCellValue(c)); s != "" {
					out = append(out, s)
				}
			}
		}
		return strings.Join(out, "\n")
	}
	code, data := get("?dateFormat=DD-MM-YYYY&tz=Europe/Helsinki", a)
	if code != 200 {
		t.Fatalf("the export: %d %s", code, data)
	}
	all := texts(data)
	for _, want := range []string{"Kanban", "Members:", "Swimlane: Default", "List: L (1)", "Write the plan", "Board:", "Due:",
		"Description", "Bold plan", "Priority:", "High", "Checklists", "Steps", "1/2", "[x] First", "[ ] Second",
		"Comments", "ann: A remark", "Attachments", "photo.png", "Uploaded At"} {
		if !strings.Contains(all, want) {
			t.Errorf("the export has %q:\n%s", want, all)
		}
	}
	// The comment's date in the reader's format and zone: no " UTC".
	if strings.Contains(all, " UTC") || !strings.Contains(all, "-"+time.Now().UTC().Format("2006")+" ") {
		t.Errorf("dates in DD-MM-YYYY, Helsinki time:\n%s", all)
	}
	if strings.Contains(all, "itemHigh000001") {
		t.Error("negative: a dropdown is its item's name, not its id")
	}
	if string(mustUnzip(t, data, "xl/media/image1.png")) != string(png) {
		t.Error("the picture is embedded")
	}
	// Without a zone, UTC and saying so.
	if _, data = get("", a); !strings.Contains(texts(data), " UTC") {
		t.Error("UTC, said")
	}
	// The selection: no comments, no board header.
	_, data = get("?fields=card-details,labels,people,board-info,dates,description,checklists", a)
	// No board header: the swimlane straight under the title.
	if s := texts(data); strings.Contains(s, "ann: A remark") || !strings.HasPrefix(s, "Kanban\nSwimlane: Default\n") || !strings.Contains(s, "Steps") {
		t.Errorf("negative: the selection leaves comments and the header out:\n%s", s)
	}
	// adminOnlyCustomFields: a board with an admin-only field, its admins only.
	_, _ = w.db.Collection("customFields").InsertOne(ctx, bson.M{"_id": "fieldSecret0001", "boardIds": bson.A{b.ID}, "name": "Salary", "type": "text", "adminOnly": true}) // inserted, not a filter
	if code, _ := get("", bt); code != 403 {
		t.Errorf("negative: a member who is not an admin: %d", code)
	}
	if code, _ := get("?fields=description", bt); code != 403 {
		t.Errorf("negative: nor the table: %d", code)
	}
	if code, _ := get("", a); code != 200 {
		t.Errorf("the board's admin: %d", code)
	}
}
