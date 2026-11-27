package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// diffHistoryFields gives what WeKan's own diffFields gives, run by Node.
func TestHistoryDiffMatchesWeKan(t *testing.T) {
	module, _ := filepath.Abs("../../../../models/lib/changeHistoryGroups.js")
	node, err := exec.LookPath("node")
	if _, statErr := os.Stat(module); err != nil || statErr != nil {
		t.Skip("needs node and WeKan's models/lib/changeHistoryGroups.js")
	}
	d1 := time.Date(2026, 1, 2, 3, 4, 5, 6e6, time.UTC)
	cases := []struct {
		before, after map[string]any
		fields        []string
	}{
		{map[string]any{"title": "a"}, map[string]any{"title": "b"}, []string{"title", "modifiedAt"}},
		{map[string]any{}, map[string]any{"dueAt": d1}, []string{"dueAt"}},
		{map[string]any{"dueAt": d1}, map[string]any{"dueAt": nil}, []string{"dueAt"}},
		{map[string]any{"labelIds": []any{"x"}}, map[string]any{"labelIds": []any{"x", "y"}}, []string{"labelIds"}},
		{map[string]any{"customFields": []any{}}, map[string]any{"customFields": []any{map[string]any{"_id": "f", "value": d1}}}, []string{"customFields"}},
		{map[string]any{"title": "same"}, map[string]any{"title": "same"}, []string{"title"}},
		{map[string]any{"sort": 1.0, "archived": false}, map[string]any{"sort": 2.0, "archived": true}, []string{"sort", "archived"}},
		{map[string]any{"spentTime": 1.5, "color": "red"}, map[string]any{"spentTime": 2.0, "color": ""}, []string{"spentTime", "color"}},
	}
	var in []any
	for _, c := range cases {
		in = append(in, map[string]any{"before": jsonForNode(c.before), "after": jsonForNode(c.after), "fields": c.fields})
	}
	raw, _ := json.Marshal(in)
	script := `const m = require(process.argv[1]);
const revive = v => Array.isArray(v) ? v.map(revive) : v && typeof v === 'object'
  ? (Object.keys(v).length === 1 && v.$date ? new Date(v.$date) : Object.fromEntries(Object.entries(v).map(([k, e]) => [k, revive(e)]))) : v;
const cases = revive(JSON.parse(require('fs').readFileSync(0, 'utf8')));
process.stdout.write(JSON.stringify(cases.map(c => m.diffFields('card', c.before, c.after, c.fields)
  .map(x => ({ group: x.group, changeType: x.changeType, previousContent: x.previousContent, newContent: x.newContent })))));`
	cmd := exec.Command(node, "-e", script, module)
	cmd.Stdin = strings.NewReader(string(raw))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var want []any
	_ = json.Unmarshal(out, &want)
	for i, c := range cases {
		got := []any{}
		for _, ch := range diffHistoryFields("card", c.before, c.after, c.fields) {
			got = append(got, map[string]any{"group": ch.Group, "changeType": ch.ChangeType, "previousContent": ch.PreviousContent, "newContent": ch.NewContent})
		}
		gj, _ := json.Marshal(got)
		var gv any
		_ = json.Unmarshal(gj, &gv)
		if canonical(gv) != canonical(want[i]) {
			t.Errorf("case %d:\n Weddy %s\n WeKan %s", i, canonical(gv), canonical(want[i]))
		}
	}
}

func TestHistoryValueRoundTrip(t *testing.T) {
	d := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	for _, v := range []any{"t", nil, d, []any{"a"}, map[string]any{"x": d, "n": []any{d, 1.0}}} {
		c := historyContent("f", v, true).(map[string]any)
		raw, _ := json.Marshal(c)
		var back map[string]any
		_ = json.Unmarshal(raw, &back)
		got, ok := historyValue(back)
		if !ok || canonical(got) != canonical(plain(v)) {
			t.Errorf("%v came back as %v", v, got)
		}
	}
}

// A card's title edited through updateRecorded is a row, which undo and
// redo put back and forward.
func TestCardEditUndo(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Edits")
	var ann User
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"username": "ann"}).Decode(&ann)
	// A card as WeKan makes one has a description, empty: undoing an "added"
	// field (no previous content) is not applicable, as in WeKan.
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": rb.Card}, bson.M{"$set": bson.M{"description": ""}})
	if err := w.s.updateRecorded(ctx, ann.ID, "card", rb.Card, bson.M{"$set": bson.M{"title": "Renamed", "description": "New text", "modifiedAt": time.Now()}}); err != nil {
		t.Fatal(err)
	}
	rows := historyRows(t, w, rb.ID)
	if len(rows) != 2 || rows[0]["batchId"] == nil || rows[0]["batchId"] != rows[1]["batchId"] {
		t.Fatalf("two groups, one batch: %v", len(rows))
	}
	c := w.client(t)
	body(t, w.signIn(t, c, "ann", "pw"))
	call := func(method string) map[string]any {
		req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/"+url.PathEscape(method), strings.NewReader(`["`+rb.ID+`"]`))
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
	title := func() string {
		var d bson.M
		_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": rb.Card}).Decode(&d)
		return docString(d, "title")
	}
	call("changeHistory.undoLast")
	call("changeHistory.undoLast")
	if title() == "Renamed" {
		t.Errorf("undo puts the title back: %s", title())
	}
	call("changeHistory.redoLast")
	call("changeHistory.redoLast")
	if title() != "Renamed" {
		t.Errorf("redo: %s", title())
	}
	if f := verifyWithWeKan(t, historyRows(t, w, rb.ID)); len(f) != 0 {
		t.Errorf("WeKan's audit: %v", f)
	}
	// Negative: a write that changes nothing recordable is no row.
	n := len(historyRows(t, w, rb.ID))
	_ = w.s.updateRecorded(ctx, ann.ID, "card", rb.Card, bson.M{"$set": bson.M{"modifiedAt": time.Now()}})
	if len(historyRows(t, w, rb.ID)) != n {
		t.Error("negative: bookkeeping recorded")
	}
}

// Negative, over the whole package: a card write is recorded in the change
// history, or it is one of these, each with why. A new direct write fails
// until it goes through cardsUpdateOne or is listed here with its reason.
var unrecordedCardWrites = map[string]string{
	"api_cards.go:apiCopyCard":              "the copy's own fields, set on a card just made",
	"api.go:apiAddCard":                     "the new card's fields, set as it is made",
	"api_members.go:addCardFrom":            "the new card's fields, set as it is made",
	"cardmove.go:copyCardToPage":            "the copy's own fields, set on a card just made",
	"server.go:addCard":                     "the new card's fields, set as it is made",
	"server.go:AddCardWith":                 "the new card's fields, set as it is made",
	"rules_engine.go:ruleSortList":          "sort only, never recorded (NEVER_RECORD)",
	"attachments.go:attachmentCoverPage":    "coverId, not a recorded field",
	"attachments.go:copyCardAttachments":    "a copy's attachments, on a card just made",
	"attachments.go:softDeleteAttachmentAs": "the card's cover, unset with its attachment; the delete is the attachment's lifecycle row",
	"attachments.go:uploadAttachment":       "the attachment's own added row, a later step",
	"scrum.go:applyScrumRolloverRow":        "the card's Scrum metadata, recorded in the close's Scrum History row (op.record)",
	"scrum_merge.go:importIntoBoard":        "the card's Scrum metadata, recorded in the import's Scrum History row (op.record)",
	"export_ics.go:apiImportICS":            "an import, whose author WeKan's hooks do not have either",
	"import_general.go:cards":               "an import, whose author WeKan's hooks do not have either",
	"history_undo.go:applyHistoryRow":       "undo and redo themselves, recorded as their restored row",
	"history_undo.go:applyCardPosition":     "undo and redo of a move, recorded as their restored row",
	"history_undo.go:softRemoveList":        "a list's soft delete, recorded as its lifecycle row",
	"server.go:watchCardPage":               "watchers, not a recorded field",
	"collection_updates.go:addToAllCards":   "WeKan's addToAllCards: an always-on field, empty, appended to cards - no value of a card changes, and WeKan records none",
}

func TestEveryCardWriteIsRecorded(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	write := regexp.MustCompile(`Collection\("cards"\)\.Update(One|Many)\(`)
	fn := regexp.MustCompile(`^func (?:\([^)]*\) )?([A-Za-z_]+)\(`)
	found := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		current := ""
		for _, line := range strings.Split(string(b), "\n") {
			if m := fn.FindStringSubmatch(line); m != nil {
				current = m[1]
			}
			if write.MatchString(line) && current != "cardsUpdateOne" {
				key := f + ":" + current
				found[key] = true
				if _, ok := unrecordedCardWrites[key]; !ok {
					t.Errorf("%s writes a card without recording it: use cardsUpdateOne, or list it with why", key)
				}
			}
		}
	}
	for key := range unrecordedCardWrites {
		if !found[key] {
			t.Errorf("%s is listed but writes no card any more: take it off the list", key)
		}
	}
}

// A move is one position row; undo puts the card back, across boards too.
func TestCardMoveUndo(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	at := login(t, w, "ann")
	a := newRESTBoard(t, w, at, "From")
	b := newRESTBoard(t, w, at, "To")
	var ann User
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"username": "ann"}).Decode(&ann)
	other := "list" + randomID()[:10]
	_, _ = w.db.Collection("lists").InsertOne(ctx, bson.M{"_id": other, "boardId": a.ID, "title": "Other", "archived": false, "sort": 2.0})
	var board Board
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": a.ID}).Decode(&board)
	if err := w.s.MoveCards(ctx, &board, &ann, []Move{{CardID: a.Card, ListID: other, SwimlaneID: a.Swimlane, Sort: 5}}); err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	for _, r := range historyRows(t, w, a.ID) {
		if r["group"] == "position" {
			rows = append(rows, r)
		}
	}
	if len(rows) != 1 || rows[0]["changeType"] != "moved" {
		t.Fatalf("one moved row: %v", len(rows))
	}
	if nc := rows[0]["newContent"].(map[string]any); nc["listId"] != other || nc["lastMoveReason"] != "" {
		t.Errorf("the new position: %v", nc)
	}
	c := w.client(t)
	body(t, w.signIn(t, c, "ann", "pw"))
	call := func(method, board string) {
		req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/"+url.PathEscape(method), strings.NewReader(`["`+board+`"]`))
		req.Header.Set("Content-Type", "application/json")
		if res, err := c.Do(req); err == nil {
			res.Body.Close()
		}
	}
	card := func() bson.M {
		var d bson.M
		_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": a.Card}).Decode(&d)
		return d
	}
	call("changeHistory.undoLast", a.ID)
	if card()["listId"] != a.List {
		t.Errorf("undo moves it back: %v", card()["listId"])
	}
	call("changeHistory.redoLast", a.ID)
	if card()["listId"] != other {
		t.Errorf("redo moves it again: %v", card()["listId"])
	}
	var raw bson.M
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": a.Card}).Decode(&raw)
	var to Board
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": b.ID}).Decode(&to)
	if err := w.s.moveCardTo(ctx, &ann, raw, &board, &to, b.Swimlane, b.List); err != nil {
		t.Fatal(err)
	}
	if card()["boardId"] != b.ID {
		t.Fatal("moved to the other board")
	}
	call("changeHistory.undoLast", b.ID)
	if d := card(); d["boardId"] != a.ID || d["listId"] != other {
		t.Errorf("undo brings it back to its board and list: %v %v", d["boardId"], d["listId"])
	}
	for _, id := range []string{a.ID, b.ID} {
		if f := verifyWithWeKan(t, historyRows(t, w, id)); len(f) != 0 {
			t.Errorf("WeKan's audit of %s: %v", id, f)
		}
	}
}

// The same for lists, swimlanes, checklists, items and comments: an update
// is recorded (entityUpdateOne), and a sub-entity added or removed is
// recorded (insertRecorded, deleteOneRecorded) - or listed with why.
var unrecordedEntityWrites = map[string]string{
	"api_cards.go:copyChecklists:checklists":                      "a copy's checklists, on a card just made",
	"api_cards.go:copyChecklists:checklistItems":                  "a copy's items, on a card just made",
	"api_more.go:apiDeleteChecklist:checklistItems":               "a deleted checklist's items, removed with it unrecorded as WeKan's ChecklistItems.direct (#6816)",
	"card_checklist_writes.go:deleteChecklistPage:checklistItems": "a deleted checklist's items, removed with it unrecorded as WeKan's ChecklistItems.direct (#6816)",
	"methods.go:methodRemoveChecklist:checklistItems":             "a deleted checklist's items, removed with it unrecorded as WeKan's ChecklistItems.direct (#6816)",
	"api_lanes.go:moveCardTo:card_comments":                       "a moved card's comments follow it; the move is its position row",
	"api_lanes.go:moveCardTo:checklists":                          "a moved card's checklists follow it; the move is its position row",
	"board_objects_move.go:methodMoveBoardObjects:checklistItems": "items following their checklist in a move",
	"card_checklist_writes.go:moveChecklistPage:checklistItems":   "items following their checklist to another card",
	"history_undo.go:applyHistoryRow:lists":                       "undo and redo themselves, recorded as their restored row",
	"history_undo.go:softRemoveList:lists":                        "a list's soft delete, recorded as its lifecycle row",
	"import_general.go:cards:card_comments":                       "an import, whose author WeKan's hooks do not have either",
	"import_general.go:cards:checklistItems":                      "an import, whose author WeKan's hooks do not have either",
	"import_general.go:cards:checklists":                          "an import, whose author WeKan's hooks do not have either",
	"import_scoped.go:importChecklists:checklistItems":            "an import, whose author WeKan's hooks do not have either",
	"import_scoped.go:importChecklists:checklists":                "an import, whose author WeKan's hooks do not have either",
	"import_scoped.go:importComments:card_comments":               "an import, whose author WeKan's hooks do not have either",
	"board_teams.go:teamLeaveCleanup:lists":                       "a member the teams no longer bring, off the lists' watchers - not a recorded field",
	"members.go:memberLeavesBoard:lists":                          "a removed member's list watching, not a recorded field",
	"rule_copy.go:ruleCopyCard:card_comments":                     "a rule's copy, on a card just made",
	"rules_engine.go:performAction:checklistItems":                "a deleted checklist's items, which go with it - the checklist is the row",
	"webhooks_out.go:webhookReply:card_comments":                  "a two-way webhook's answer, which has no WeKan user",
}

func TestEveryEntityWriteIsRecorded(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	write := regexp.MustCompile(`Collection\("(lists|swimlanes|checklists|checklistItems|card_comments)"\)\.(Update(?:One|Many))\(|Collection\("(checklists|checklistItems|card_comments)"\)\.(InsertOne|DeleteOne|DeleteMany)\(`)
	fn := regexp.MustCompile(`^func (?:\([^)]*\) )?([A-Za-z_]+)\(`)
	helpers := map[string]bool{"entityUpdateOne": true, "insertRecorded": true, "deleteOneRecorded": true}
	found := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		current := ""
		for _, line := range strings.Split(string(b), "\n") {
			if m := fn.FindStringSubmatch(line); m != nil {
				current = m[1]
			}
			for _, m := range write.FindAllStringSubmatch(line, -1) {
				if helpers[current] {
					continue
				}
				coll := m[1] + m[3]
				key := f + ":" + current + ":" + coll
				found[key] = true
				if _, ok := unrecordedEntityWrites[key]; !ok {
					t.Errorf("%s writes without recording it: use entityUpdateOne / insertRecorded / deleteOneRecorded, or list it with why", key)
				}
			}
		}
	}
	for key := range unrecordedEntityWrites {
		if !found[key] {
			t.Errorf("%s is listed but writes no such thing any more: take it off the list", key)
		}
	}
}

// A checklist added and deleted, and an item, are rows with the document.
func TestChecklistLifecycleRecorded(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Lists")
	var ann User
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"username": "ann"}).Decode(&ann)
	if _, err := w.s.insertRecorded(ctx, ann.ID, "checklist", bson.M{"_id": "cl1", "cardId": rb.Card, "boardId": rb.ID, "title": "Todo", "createdAt": time.Now()}); err != nil {
		t.Fatal(err)
	}
	_, _ = w.s.entityUpdateOne(ctx, ann.ID, "checklist", bson.M{"_id": "cl1"}, bson.M{"$set": bson.M{"title": "Done"}})
	_, _ = w.s.deleteOneRecorded(ctx, ann.ID, "checklist", bson.M{"_id": "cl1"})
	var kinds []string
	for _, r := range historyRows(t, w, rb.ID) {
		kinds = append(kinds, docString(r, "entityType")+":"+docString(r, "changeType"))
		if r["entityType"] == "checklist" && r["changeType"] == "removed" {
			doc := r["previousContent"].(map[string]any)["document"].(map[string]any)
			if doc["title"] != "Done" || doc["cardId"] != rb.Card {
				t.Errorf("the removed document: %v", doc)
			}
		}
	}
	if strings.Join(kinds, " ") != "checklist:added checklist:edited checklist:removed" {
		t.Errorf("the rows: %v", kinds)
	}
	if f := verifyWithWeKan(t, historyRows(t, w, rb.ID)); len(f) != 0 {
		t.Errorf("WeKan's audit: %v", f)
	}
}

// An attachment uploaded, deleted and restored is in its card's History,
// named by its file; Restore on the removal brings it back, and undo of a
// delete too. An import's attachment is no row (negative).
func TestAttachmentHistory(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.s.FilesDir = t.TempDir()
	w.seedUser(t, "ann", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Files")
	var ann User
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"username": "ann"}).Decode(&ann)
	var b Board
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": rb.ID}).Decode(&b)
	var c Card
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": rb.Card}).Decode(&c)
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/", nil)
	a, err := w.s.StoreAttachment(r, &b, &c, &ann, "report.pdf", "application/pdf", strings.NewReader("%PDF-1.4 x"))
	if err != nil {
		t.Fatal(err)
	}
	rows := func(change string) []map[string]any {
		var out []map[string]any
		for _, row := range historyRows(t, w, rb.ID) {
			if row["entityType"] == "attachment" && row["changeType"] == change {
				out = append(out, row)
			}
		}
		return out
	}
	if len(rows("added")) != 1 {
		t.Fatalf("the upload is a row")
	}
	deleted := func() bool {
		var d bson.M
		_ = w.db.Collection("attachments").FindOne(ctx, bson.M{"_id": a.ID}).Decode(&d)
		return d["deletedAt"] != nil
	}
	if _, err := w.s.softDeleteAttachmentBatch(r, a, &ann); err != nil || !deleted() {
		t.Fatal("deleted")
	}
	removed := rows("removed")
	if len(removed) != 1 || removed[0]["previousContent"].(map[string]any)["name"] != "report.pdf" || removed[0]["newContent"] != nil {
		t.Fatalf("the delete, named by its file: %v", removed)
	}
	cl := w.client(t)
	body(t, w.signIn(t, cl, "ann", "pw"))
	call := func(method, args string) map[string]any {
		req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/"+url.PathEscape(method), strings.NewReader(args))
		req.Header.Set("Content-Type", "application/json")
		res, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		return out
	}
	if out := call("changeHistory.restore", `[["`+docString(removed[0], "_id")+`"]]`); canonical(out["result"]) != `{"restored":1,"skipped":0}` || deleted() {
		t.Fatalf("Restore on the removal brings it back: %v", out)
	}
	// Deleted again, then undone.
	_, _ = w.s.softDeleteAttachmentBatch(r, a, &ann)
	call("changeHistory.undoLast", `["`+rb.ID+`"]`)
	if deleted() {
		t.Error("undo of the delete brings it back")
	}
	if f := verifyWithWeKan(t, historyRows(t, w, rb.ID)); len(f) != 0 {
		t.Errorf("WeKan's audit: %v", f)
	}
	// Negative: an imported attachment is no row.
	n := len(rows("added"))
	_, _ = w.db.Collection("attachments").InsertOne(ctx, bson.M{"_id": "imported01", "name": "old.txt", "meta": bson.M{"boardId": rb.ID, "cardId": rb.Card, "source": "import"}})
	if len(rows("added")) != n {
		t.Error("negative: an import recorded")
	}
}

func TestRESTTimeCorrectionHistory(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	token := login(t, w, "ann")
	board := newRESTBoard(t, w, token, "Time")
	path := w.srv.URL + "/api/boards/" + board.ID + "/lists/" + board.List + "/cards/" + board.Card
	for _, body := range []string{`{"spentTime":-1}`, `{"spentTime":"NaN"}`, `{"isOverTime":[]}`} {
		if code := call(t, "PUT", path, token, body, nil); code != 400 {
			t.Fatalf("invalid correction %s returned %d", body, code)
		}
	}
	if code := call(t, "PUT", path, token, `{"spentTime":"1.5","isOverTime":true}`, nil); code != 200 {
		t.Fatalf("correct time returned %d", code)
	}
	got := map[string]any{}
	ids := map[string]string{}
	for _, row := range historyRows(t, w, board.ID) {
		if row["group"] == "dates" {
			content, _ := row["newContent"].(map[string]any)
			field := docString(content, "field")
			got[field] = content["value"]
			ids[field] = docString(row, "_id")
		}
	}
	if got["spentTime"] != 1.5 || got["isOvertime"] != true {
		t.Fatalf("time history = %v", got)
	}
	if code := call(t, "PUT", path, token, `{"spentTime":0,"isOverTime":false}`, nil); code != 200 {
		t.Fatalf("clear time returned %d", code)
	}
	c := w.client(t)
	body(t, w.signIn(t, c, "ann", "pw"))
	restore := w.srv.URL + "/b/" + board.ID + "/history/restore"
	response, _ := post(t, c, restore, url.Values{"id": {ids["spentTime"], ids["isOvertime"]}})
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("restore returned %d", response.StatusCode)
	}
	card := w.s.findDoc(context.Background(), "cards", bson.M{"_id": eq(board.Card)})
	if card["spentTime"] != 1.5 || card["isOvertime"] != true {
		t.Fatalf("restored values = %v, %v", card["spentTime"], card["isOvertime"])
	}
}

func TestRESTDateClearHistory(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	token := login(t, w, "ann")
	board := newRESTBoard(t, w, token, "Dates")
	path := w.srv.URL + "/api/boards/" + board.ID + "/lists/" + board.List + "/cards/" + board.Card
	const date = "2027-01-01T10:00:00Z"
	for _, data := range []string{`{"startAt":"` + date + `","dueAt":"` + date + `"}`, `{"dueAt":null}`} {
		if code := call(t, "PUT", path, token, data, nil); code != 200 {
			t.Fatalf("date edit returned %d", code)
		}
	}
	var dueID string
	var dates, clears int
	for _, row := range historyRows(t, w, board.ID) {
		if row["group"] != "dates" {
			continue
		}
		dates++
		content, _ := row["newContent"].(map[string]any)
		prior, _ := row["previousContent"].(map[string]any)
		switch {
		case content["field"] == "dueAt" && content["value"] != nil:
			dueID = docString(row, "_id")
		case prior["field"] == "dueAt" && row["changeType"] == "removed" && content["value"] == nil:
			clears++
		}
	}
	if dates != 3 || clears != 1 || dueID == "" {
		t.Fatalf("date rows = %d, cleared due dates = %d, original due row = %q", dates, clears, dueID)
	}
	c := w.client(t)
	body(t, w.signIn(t, c, "ann", "pw"))
	_, html := get(t, c, w.srv.URL+"/b/"+board.ID+"/history?group=dates")
	if !strings.Contains(html, `class="history-content">—</td>`) {
		t.Fatal("cleared due date should display an empty value")
	}
	response, _ := post(t, c, w.srv.URL+"/b/"+board.ID+"/history/restore", url.Values{"id": {dueID}})
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("restore returned %d", response.StatusCode)
	}
	card := w.s.findDoc(context.Background(), "cards", bson.M{"_id": eq(board.Card)})
	if got, ok := card["dueAt"].(time.Time); !ok || got.UTC().Format(time.RFC3339) != date {
		t.Fatalf("restored due date = %v", card["dueAt"])
	}
}

func TestRESTContainerTitleHistory(t *testing.T) {
	for _, tc := range []struct{ scope, collection string }{
		{"list", "lists"}, {"swimlane", "swimlanes"},
	} {
		t.Run(tc.scope, func(t *testing.T) {
			testRESTContainerTitleHistory(t, tc.scope, tc.collection)
		})
	}
}

func testRESTContainerTitleHistory(t *testing.T, scope, collection string) {
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	token := login(t, w, "ann")
	board := newRESTBoard(t, w, token, "Lists")
	entityID := board.List
	if scope == "swimlane" {
		entityID = board.Swimlane
	}
	path := w.srv.URL + "/api/boards/" + board.ID + "/" + collection + "/" + entityID
	for _, data := range []string{`{"title":"First list title"}`, `{"title":"Second list title"}`} {
		if code := call(t, "PUT", path, token, data, nil); code != 200 {
			t.Fatalf("rename returned %d", code)
		}
	}
	var id string
	for _, row := range historyRows(t, w, board.ID) {
		content, _ := row["newContent"].(map[string]any)
		if row["entityType"] == scope && row["entityId"] == entityID && content["value"] == "First list title" {
			id = docString(row, "_id")
		}
	}
	if id == "" {
		t.Fatal("expected a History row for the rename")
	}
	c := w.client(t)
	body(t, w.signIn(t, c, "ann", "pw"))
	back := "/b/" + board.ID + "/history?" + scope + "=" + entityID
	_, html := get(t, c, w.srv.URL+back)
	if !strings.Contains(html, "First list title") || !strings.Contains(html, "Second list title") {
		t.Fatal("expected both titles in scoped History")
	}
	response, _ := post(t, c, w.srv.URL+"/b/"+board.ID+"/history/restore", url.Values{"id": {id}, "back": {back}})
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("restore returned %d", response.StatusCode)
	}
	doc := w.s.findDoc(context.Background(), collection, bson.M{"_id": eq(entityID)})
	if title := docString(doc, "title"); title != "First list title" {
		t.Fatalf("restored title = %q", title)
	}
}

func TestRESTListColorHistory(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	token := login(t, w, "ann")
	board := newRESTBoard(t, w, token, "List colors")
	path := w.srv.URL + "/api/boards/" + board.ID + "/lists/" + board.List
	if code := call(t, "PUT", path, token, `{"color":"#123"}`, nil); code != 400 {
		t.Fatalf("invalid color returned %d", code)
	}
	for _, payload := range []string{`{"color":"red"}`, `{"color":"#00aaff"}`} {
		if code := call(t, "PUT", path, token, payload, nil); code != 200 {
			t.Fatalf("color edit returned %d", code)
		}
	}
	if code := call(t, "PUT", path, token, `{"color":null,"title":"Renamed without recoloring"}`, nil); code != 200 {
		t.Fatalf("rename with a null color returned %d", code)
	}
	if code := call(t, "PUT", path, token, `{"color":""}`, nil); code != 404 {
		t.Fatalf("empty list edit returned %d", code)
	}
	if doc := w.s.findDoc(context.Background(), "lists", bson.M{"_id": eq(board.List)}); docString(doc, "color") != "#00aaff" {
		t.Fatal("optional color values should leave the list color intact")
	}
	for _, edit := range []struct {
		body    string
		starred bool
	}{
		{`{"starred":true}`, true},
		{`{"starred":false}`, false},
	} {
		if code := call(t, "PUT", path, token, edit.body, nil); code != 200 {
			t.Fatalf("starred edit returned %d", code)
		}
		if doc := w.s.findDoc(context.Background(), "lists", bson.M{"_id": eq(board.List)}); doc["starred"] != edit.starred {
			t.Fatalf("expected starred = %v", edit.starred)
		}
	}
	if code := call(t, "PUT", path, token, `{"starred":"invalid"}`, nil); code != 400 {
		t.Fatalf("invalid starred value returned %d", code)
	}
	var id string
	for _, row := range historyRows(t, w, board.ID) {
		content, _ := row["newContent"].(map[string]any)
		if row["entityId"] == board.List && row["group"] == "title" && content["field"] == "color" && content["value"] == "red" {
			id = docString(row, "_id")
		}
	}
	if id == "" {
		t.Fatal("expected a list color history entry")
	}
	c := w.client(t)
	body(t, w.signIn(t, c, "ann", "pw"))
	back := "/b/" + board.ID + "/history?list=" + board.List
	response, _ := post(t, c, w.srv.URL+"/b/"+board.ID+"/history/restore", url.Values{"id": {id}, "back": {back}})
	row := w.s.findDoc(context.Background(), "lists", bson.M{"_id": eq(board.List)})
	if response.StatusCode != http.StatusSeeOther || docString(row, "color") != "red" {
		t.Fatalf("restore returned %d, color %q", response.StatusCode, docString(row, "color"))
	}
}

func TestRESTListWIPHistory(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	token := login(t, w, "ann")
	board := newRESTBoard(t, w, token, "WIP")
	path := w.srv.URL + "/api/boards/" + board.ID + "/lists/" + board.List
	if code := call(t, "PUT", path, token, `{"wipLimit":{"value":1.5}}`, nil); code != 400 {
		t.Fatalf("invalid WIP limit returned %d", code)
	}
	if code := call(t, "PUT", path, token, `{"wipLimit":{"value":3,"enabled":true,"soft":false}}`, nil); code != 200 {
		t.Fatalf("WIP edit returned %d", code)
	}
	for _, row := range historyRows(t, w, board.ID) {
		content, _ := row["newContent"].(map[string]any)
		if row["entityId"] == board.List && row["group"] == "title" && content["field"] == "wipLimit" {
			value, _ := content["value"].(map[string]any)
			if historyNumber(value["value"]) == 3 && value["enabled"] == true && value["soft"] == false {
				return
			}
		}
	}
	t.Fatal("expected the WIP limit in list History")
}
