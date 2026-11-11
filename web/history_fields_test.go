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
	_ = w.s.updateRecorded(ctx, ann.ID, "card", rb.Card, bson.M{"$set": bson.M{"modifiedAt": time.Now(), "sort": 9.0}})
	if len(historyRows(t, w, rb.ID)) != n {
		t.Error("negative: bookkeeping recorded")
	}
}

// Negative, over the whole package: a card write is recorded in the change
// history, or it is one of these, each with why. A new direct write fails
// until it goes through cardsUpdateOne or is listed here with its reason.
var unrecordedCardWrites = map[string]string{
	"api_cards.go:apiCopyCard":                     "the copy's own fields, set on a card just made",
	"api.go:apiAddCard":                            "the new card's fields, set as it is made",
	"api_members.go:addCardFrom":                   "the new card's fields, set as it is made",
	"cardmove.go:copyCardToPage":                   "the copy's own fields, set on a card just made",
	"server.go:addCard":                            "the new card's fields, set as it is made",
	"server.go:AddCardWith":                        "the new card's fields, set as it is made",
	"api_lanes.go:moveCardTo":                      "a move: WeKan records it as one position row (Card.move) - a later step",
	"server.go:MoveCards":                          "a move: WeKan records it as one position row (Card.move) - a later step",
	"board_objects_move.go:methodMoveBoardObjects": "a move: WeKan records it as one position row - a later step",
	"rules_engine.go:ruleMoveCard":                 "a rule's move, a position row - a later step",
	"rules_engine.go:ruleSortList":                 "sort only, never recorded (NEVER_RECORD)",
	"api_more.go:apiDeleteCustomField":             "the field's values leave with its definition, which is the change",
	"attachments.go:attachmentCoverPage":           "coverId, not a recorded field",
	"attachments.go:copyCardAttachments":           "a copy's attachments, on a card just made",
	"attachments.go:softDeleteAttachmentBatch":     "the attachment's own soft delete, its lifecycle row a later step",
	"attachments.go:uploadAttachment":              "the attachment's own added row, a later step",
	"export_ics.go:apiImportICS":                   "an import, whose author WeKan's hooks do not have either",
	"import_general.go:cards":                      "an import, whose author WeKan's hooks do not have either",
	"history_undo.go:applyHistoryRow":              "undo and redo themselves, recorded as their restored row",
	"history_undo.go:softRemoveList":               "a list's soft delete, recorded as its lifecycle row",
	"members.go:removeBoardMember":                 "the board's people, recorded as the board's own change",
	"server.go:restoreAllArchivedCardsPage":        "Restore all - lifecycle rows a later step",
	"server.go:setPoker":                           "planning poker votes, recorded with the poker group - a later step",
	"server.go:watchCardPage":                      "watchers, not a recorded field",
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
