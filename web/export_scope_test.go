package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The JSON export's scope and selection, as WeKan's streaming exporter
// writes them (models/exporter.js): a list's, a swimlane's or one card's
// cards, with the lists and swimlanes they are in, their activities and no
// rules; one checklist on its card; the sections the selection leaves out
// as empty arrays. A checklist of another board's card exports nothing,
// and is recorded for Admin Panel -> Problems.
func TestExportScopeAndSelection(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	a := login(t, w, "ann")
	b := newRESTBoard(t, w, a, "Scoped")
	other := newRESTBoard(t, w, a, "Other")
	var l2 struct {
		ID string `json:"_id"`
	}
	call(t, "POST", w.srv.URL+"/api/boards/"+b.ID+"/lists", a, `{"title":"L2"}`, &l2)
	var c2 struct {
		ID string `json:"_id"`
	}
	call(t, "POST", w.srv.URL+"/api/boards/"+b.ID+"/lists/"+l2.ID+"/cards", a, `{"title":"Second","swimlaneId":"`+b.Swimlane+`"}`, &c2)
	call(t, "POST", w.srv.URL+"/api/boards/"+b.ID+"/cards/"+b.Card+"/comments", a, `{"comment":"On the first"}`, nil)
	for _, cl := range []struct{ id, card, board string }{{"checklistOne001", b.Card, b.ID}, {"checklistTwo001", c2.ID, b.ID}, {"checklistElse01", other.Card, other.ID}} {
		_, _ = w.db.Collection("checklists").InsertOne(ctx, bson.M{"_id": cl.id, "cardId": cl.card, "boardId": cl.board, "title": cl.id})             // inserted, not a filter
		_, _ = w.db.Collection("checklistItems").InsertOne(ctx, bson.M{"_id": cl.id + "i", "checklistId": cl.id, "cardId": cl.card, "title": "item"}) // inserted, not a filter
	}
	_, _ = w.db.Collection("rules").InsertOne(ctx, bson.M{"_id": "ruleOfTheBoard1", "boardId": b.ID, "title": "R"}) // inserted, not a filter
	export := func(query string) map[string][]map[string]any {
		req, _ := http.NewRequest("GET", w.srv.URL+"/api/boards/"+b.ID+"/export"+query, nil)
		req.Header.Set("Authorization", "Bearer "+a)
		res, err := testClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, _ := io.ReadAll(res.Body)
		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err != nil || res.StatusCode != 200 {
			t.Fatalf("%s: %d %v", query, res.StatusCode, err)
		}
		out := map[string][]map[string]any{}
		for _, k := range []string{"cards", "lists", "swimlanes", "comments", "checklists", "checklistItems", "rules", "activities", "attachments", "customFields"} {
			arr, ok := raw[k].([]any)
			if !ok {
				t.Fatalf("%s: %s is an array, empty or not", query, k)
			}
			for _, x := range arr {
				out[k] = append(out[k], x.(map[string]any))
			}
		}
		return out
	}
	titles := func(rows []map[string]any) []string {
		var out []string
		for _, r := range rows {
			out = append(out, strOf(r["title"]))
		}
		return out
	}
	all := export("")
	if len(all["cards"]) != 2 || len(all["lists"]) != 2 || len(all["rules"]) != 1 || len(all["checklists"]) != 2 {
		t.Fatalf("the whole board: %v %v %d %d", titles(all["cards"]), titles(all["lists"]), len(all["rules"]), len(all["checklists"]))
	}
	byList := export("?listId=" + l2.ID)
	if got := titles(byList["cards"]); len(got) != 1 || got[0] != "Second" || len(byList["lists"]) != 1 || len(byList["swimlanes"]) != 1 || len(byList["rules"]) != 0 || len(byList["comments"]) != 0 {
		t.Errorf("a list's: cards %v, %d lists, %d swimlanes, %d rules, %d comments", got, len(byList["lists"]), len(byList["swimlanes"]), len(byList["rules"]), len(byList["comments"]))
	}
	for _, a := range byList["activities"] {
		if strOf(a["cardId"]) != c2.ID {
			t.Errorf("negative: a list's export has another card's or the board's activity: %v", a)
		}
	}
	byCard := export("?cardId=" + b.Card)
	if got := titles(byCard["cards"]); len(got) != 1 || len(byCard["comments"]) != 1 || len(byCard["checklists"]) != 1 {
		t.Errorf("a card's: %v, %d comments, %d checklists", got, len(byCard["comments"]), len(byCard["checklists"]))
	}
	byChecklist := export("?checklistId=checklistTwo001")
	if got := titles(byChecklist["cards"]); len(got) != 1 || got[0] != "Second" || len(byChecklist["checklists"]) != 1 || len(byChecklist["checklistItems"]) != 1 {
		t.Errorf("a checklist's: its card %v, %d checklists, %d items", got, len(byChecklist["checklists"]), len(byChecklist["checklistItems"]))
	}
	byLane := export("?swimlaneId=" + b.Swimlane)
	if len(byLane["cards"]) != 2 || len(byLane["swimlanes"]) != 1 {
		t.Errorf("a swimlane's: %d cards, %d swimlanes", len(byLane["cards"]), len(byLane["swimlanes"]))
	}
	// Negative: another board's checklist exports nothing of either board,
	// and is recorded.
	stolen := export("?checklistId=checklistElse01")
	if len(stolen["cards"]) != 0 || len(stolen["checklists"]) != 0 || len(stolen["checklistItems"]) != 0 {
		t.Errorf("negative: another board's checklist: %d cards, %d checklists", len(stolen["cards"]), len(stolen["checklists"]))
	}
	if n, _ := w.db.Collection("eventlog").CountDocuments(ctx, bson.M{"bleed": "CrossBoardExportBleed", "cwe": "CWE-639"}); n != 1 {
		t.Errorf("the attempt is in Admin Panel -> Problems: %d", n)
	}
	// Negative: an id that is not one is no scope.
	if odd := export("?listId=" + "x%7B%24ne%3A1%7D"); len(odd["cards"]) != 2 {
		t.Errorf("negative: a malformed scope is the whole board: %d", len(odd["cards"]))
	}
	// The selection: what it leaves out is an empty array.
	sel := export("?fields=description")
	if len(sel["cards"]) != 2 || len(sel["comments"]) != 0 || len(sel["checklists"]) != 0 || len(sel["activities"]) != 0 || len(sel["customFields"]) != 0 {
		t.Errorf("the selection: %d cards, %d comments, %d checklists, %d activities", len(sel["cards"]), len(sel["comments"]), len(sel["checklists"]), len(sel["activities"]))
	}
}

// The Excel export's scope, both layouts (ExporterExcelBoard, ExporterExcel):
// a list's cards on a sheet named for the board and the list, under the
// list's heading, with no board header; the table, the list's rows only.
func TestExcelExportScope(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	a := login(t, w, "ann")
	b := newRESTBoard(t, w, a, "Scoped")
	var l2, c2 struct {
		ID string `json:"_id"`
	}
	call(t, "POST", w.srv.URL+"/api/boards/"+b.ID+"/lists", a, `{"title":"L2"}`, &l2)
	call(t, "POST", w.srv.URL+"/api/boards/"+b.ID+"/lists/"+l2.ID+"/cards", a, `{"title":"Second","swimlaneId":"`+b.Swimlane+`"}`, &c2)
	sheet := func(query string) xlsxValueSheet {
		req, _ := http.NewRequest("GET", w.srv.URL+"/api/boards/"+b.ID+"/exportExcel"+query, nil)
		req.Header.Set("Authorization", "Bearer "+a)
		res, err := testClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, _ := io.ReadAll(res.Body)
		sheets, err := readXlsxValues(data)
		if err != nil || len(sheets) == 0 {
			t.Fatalf("%s: %d %v", query, res.StatusCode, err)
		}
		return sheets[0]
	}
	text := func(sh xlsxValueSheet) []string {
		var out []string
		for _, row := range sh.Rows {
			for _, c := range row {
				if s := sheetCellString(sheetCellValue(c)); s != "" {
					out = append(out, s)
				}
			}
		}
		return out
	}
	has := func(list []string, s string) bool {
		for _, x := range list {
			if x == s {
				return true
			}
		}
		return false
	}
	byList := sheet("?listId=" + l2.ID)
	got := text(byList)
	if byList.Name != "Scoped - L2" || len(got) == 0 || got[0] != "List: L2" || !has(got, "Second") || has(got, "C") || has(got, "Members:") || has(got, "List: L2 (1)") {
		t.Errorf("a list's card layout: sheet %q, %q", byList.Name, got)
	}
	byLane := text(sheet("?swimlaneId=" + b.Swimlane))
	if len(byLane) == 0 || byLane[0] != "Swimlane: Default" || has(byLane, "Swimlane: Default\nSwimlane: Default") || !has(byLane, "List: L (1)") || !has(byLane, "List: L2 (1)") {
		t.Errorf("a swimlane's card layout: %q", byLane)
	}
	table := text(sheet("?listId=" + l2.ID + "&fields=description"))
	if !has(table, "Second") || has(table, "C") {
		t.Errorf("a list's table: %q", table)
	}
}
