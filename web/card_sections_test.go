package web

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestSectionOrderOf(t *testing.T) {
	got := sectionOrderOf(bson.A{"description", "nope", "labels", "description", 3}, "card")
	if got[0] != "description" || got[1] != "labels" || len(got) != len(cardSettings.Order["card"]) {
		t.Fatalf("known ids once, then the rest: %v", got)
	}
	if got := sectionOrderOf(nil, "minicard"); got[0] != "scrum" {
		t.Fatalf("the minicard's default starts with Scrum: %v", got)
	}
}

// A row that modifies another follows it, wherever that one is moved.
func TestRowsForSide(t *testing.T) {
	keys := func(rows []*cardSettingsRow) string {
		var out []string
		for _, r := range rows {
			out = append(out, r.Key)
		}
		return " " + strings.Join(out, " ") + " "
	}
	got := keys(rowsForSide("card", applyCardLayout([]string{"dueDate", "receivedDate"})))
	if !strings.Contains(got, " dueDate dueFromTitle atRisk ") && !strings.Contains(got, " dueDate atRisk dueFromTitle ") {
		t.Fatalf("the due date's helpers under it: %s", got)
	}
	if strings.Index(got, " dueDate ") > strings.Index(got, " receivedDate ") {
		t.Fatalf("the stored order: %s", got)
	}
	if strings.Contains(got, " listTitle ") == false {
		t.Fatal("every row of the side")
	}
}

func TestCardSettingsOrderWrites(t *testing.T) {
	// An unknown id is normalised away, as WeKan's sectionOrder does.
	clean, ok := cardSettingsBoardWrite(map[string]any{"cardSettingsSectionOrder": []any{"description", "labels", "nope"}})
	if !ok || clean["cardSettingsSectionOrder"].([]string)[0] != "description" || len(clean["cardSettingsSectionOrder"].([]string)) != len(cardSettings.Order["card"]) {
		t.Fatalf("normalised: %v", clean)
	}
	clean, ok = cardSettingsBoardWrite(map[string]any{"cardSettingsSectionOrder": []any{"description", "labels"}, "cardFieldOrder": []any{"dueDate", "receivedDate", "bogus"}})
	if !ok {
		t.Fatal("both orders are Card Settings'")
	}
	if o := clean["cardSettingsSectionOrder"].([]string); o[0] != "description" || len(o) != len(cardSettings.Order["card"]) {
		t.Fatalf("the section order, whole: %v", o)
	}
	if o := strings.Join(clean["cardFieldOrder"].([]string), " "); strings.Contains(o, "bogus") || strings.Index(o, "dueDate") > strings.Index(o, "receivedDate") {
		t.Fatalf("the field order, canonical: %s", o)
	}
	for _, bad := range []map[string]any{{"cardFieldOrder": "dueDate"}, {"cardSettingsSectionOrder": []any{1}}, {"minicardFieldOrders": []any{"labels"}}, {"minicardFieldOrder": []any{"labels", 2}}} {
		if _, ok := cardSettingsBoardWrite(bad); ok {
			t.Fatalf("refused: %v", bad)
		}
	}
	// The minicard's field order is its own, in the minicard's layout
	// (MINICARD_LAYOUT): writable since the Minicard pane's rows are dragged.
	clean, ok = cardSettingsBoardWrite(map[string]any{"minicardFieldOrder": []any{"endDate", "receivedDate"}})
	if o := strings.Join(clean["minicardFieldOrder"].([]string), " "); !ok || !strings.HasPrefix(o, "dueComplete cardNumber cover endDate receivedDate startDate dueDate scrumSprint") ||
		strings.Index(o, "endDate") > strings.Index(o, "receivedDate") || !strings.Contains(o, "swimlaneName") {
		t.Fatalf("the minicard's order, canonical in its layout: %s", o)
	}
}

// The opened card draws its sections in the Card pane's order, its dates in
// the row order; the minicard's order is the minicard's own.
func TestOpenedCardFollowsTheCardPane(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Order")
	ac := w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	cardURL := w.srv.URL + "/b/" + rb.ID + "/order/" + rb.Card
	sections := func(html string) []string {
		var out []string
		for _, m := range regexp.MustCompile(`class="card-details-section card-details-section-[A-Za-z]+" data-card-section="([A-Za-z]+)"`).FindAllStringSubmatch(html, -1) {
			out = append(out, m[1])
		}
		return out
	}
	_, html := get(t, ac, cardURL)
	got := sections(html)
	if len(got) < 5 || got[0] != "labels" {
		t.Fatalf("a default board: Labels first: %v", got)
	}
	if !strings.Contains(html, `js-toggle-card-section" data-section="date" role="button" tabindex="0"><i class="fa fa-calendar"`) {
		t.Fatal("each section headed with its icon")
	}
	if _, out := methodCall(t, w, ac, "/boards/update", `[{"_id":"`+rb.ID+`"},{"$set":{"cardSettingsSectionOrder":["description","labels"],"cardFieldOrder":["dueDate","receivedDate"]}}]`); out["error"] != nil {
		t.Fatalf("the board admin orders: %v", out)
	}
	_, html = get(t, ac, cardURL)
	got = sections(html)
	if got[0] != "description" || got[1] != "labels" {
		t.Fatalf("the card follows: %v", got)
	}
	if strings.Index(html, `card-details-item-due`) > strings.Index(html, `card-details-item-received`) {
		t.Fatal("Due above Received on the card")
	}
	var b bson.M
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": rb.ID}).Decode(&b)
	if b["minicardSettingsSectionOrder"] != nil {
		t.Fatal("the minicard's order is its own")
	}
	// The pane shows the stored order too.
	if strings.Index(html, `js-card-settings-pane card-settings-pane-card" data-side="card"`) < 0 {
		t.Fatal("the Card pane")
	}
	pane := html[strings.Index(html, `data-side="card" data-board=`):]
	if strings.Index(pane, `data-section="description"`) > strings.Index(pane, `data-section="labels"`) {
		t.Fatal("the Card pane in the card's order")
	}
}

// The Minicard pane's share: half unless the user set it, within bounds.
func TestCardSettingsSplitOf(t *testing.T) {
	u := &User{}
	if cardSettingsSplitOf(nil) != "0.5000" || cardSettingsSplitOf(u) != "0.5000" {
		t.Fatal("half by default")
	}
	u.Profile.CardSettingsSplit = 0.7
	if cardSettingsSplitOf(u) != "0.7000" {
		t.Fatal("the user's own")
	}
	u.Profile.CardSettingsSplit = 5
	if cardSettingsSplitOf(u) != "0.9000" {
		t.Fatal("never past the bounds")
	}
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Split")
	ac := w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	_, html := get(t, ac, w.srv.URL+"/b/"+rb.ID+"/split")
	for _, want := range []string{`js-card-settings-panes" style="--card-settings-split: 0.5000"`, `js-card-settings-splitter" role="separator"`,
		`button class="js-date-popup-resize"`, `js-toggle-card-settings-pane"`, `js-card-settings-pane-body"`} {
		if !strings.Contains(html, want) {
			t.Fatalf("Card Settings has %s", want)
		}
	}
}

// Card Settings' hamburgers: ask for a move reason and the board's Subtasks
// Settings, a board admin's; a subtasks board the admin may not write, or a
// landing list not on it, is SubtaskDepositBleed's shape - refused, recorded.
func TestCardSettingsPanels(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	w.seedUser(t, "bob", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Panels")
	foreign := newRESTBoard(t, w, login(t, w, "bob"), "Bobs")
	mine := newRESTBoard(t, w, login(t, w, "ann"), "Deposit")
	ac := w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	write := func(set string) map[string]any {
		_, out := methodCall(t, w, ac, "/boards/update", `[{"_id":"`+rb.ID+`"},{"$set":`+set+`}]`)
		return out
	}
	if out := write(`{"askForMoveReason":true,"presentParentTask":"prefix-with-parent"}`); out["error"] != nil {
		t.Fatalf("the admin's: %v", out)
	}
	for _, bad := range []string{`{"presentParentTask":"anything"}`, `{"askForMoveReason":"yes"}`, `{"subtasksDefaultBoardId":"../x"}`} {
		if out := write(bad); out["reason"] != "Access denied" {
			t.Fatalf("%s refused: %v", bad, out)
		}
	}
	if out := write(`{"subtasksDefaultBoardId":"` + mine.ID + `","subtasksDefaultListId":"` + mine.List + `"}`); out["error"] != nil {
		t.Fatalf("a board of the admin's, its list: %v", out)
	}
	var b bson.M
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": rb.ID}).Decode(&b)
	if b["askForMoveReason"] != true || b["presentParentTask"] != "prefix-with-parent" || b["subtasksDefaultBoardId"] != mine.ID || b["subtasksDefaultListId"] != mine.List {
		t.Fatalf("written: %v", b)
	}
	// A new board resets the landing list, which was the old board's.
	if out := write(`{"subtasksDefaultBoardId":null}`); out["error"] != nil {
		t.Fatalf("none: %v", out)
	}
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": rb.ID}).Decode(&b)
	if b["subtasksDefaultListId"] != nil {
		t.Fatal("the old board's list is reset")
	}
	if out := write(`{"subtasksDefaultListId":"` + mine.List + `"}`); out["reason"] != "Access denied" {
		t.Fatalf("a list not on the subtasks board (now this one): %v", out)
	}
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"username": "ann"}, bson.M{"$set": bson.M{"loginDisabled": false}})
	ac = w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	if out := write(`{"subtasksDefaultBoardId":"` + foreign.ID + `"}`); out["reason"] != "Access denied" {
		t.Fatalf("a board the admin may not write: %v", out)
	}
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": rb.ID}).Decode(&b)
	if b["subtasksDefaultBoardId"] == foreign.ID {
		t.Fatal("not written")
	}
	if n, _ := w.db.Collection("eventlog").CountDocuments(ctx, bson.M{"bleed": "SubtaskDepositBleed", "source": "ddp:boards.update"}); n == 0 {
		t.Fatal("recorded as SubtaskDepositBleed")
	}
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"username": "ann"}, bson.M{"$set": bson.M{"loginDisabled": false}})
	ac = w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	_, html := get(t, ac, w.srv.URL+"/b/"+rb.ID+"/panels")
	for _, want := range []string{`data-panel="section-card"`, `data-panel="section-subtasks"`, `js-open-card-settings-section-menu"`, `class="board-subtask-settings"`, `js-ask-move-reason"`} {
		if !strings.Contains(html, want) {
			t.Fatalf("the page has %s", want)
		}
	}
}

// Card Settings' Custom Fields: the board's fields, by sort then name, each
// side's checkbox its own flag (alwaysOnCard on the card, showOnCard on the
// minicard); a board admin's only.
func TestCardSettingsCustomFields(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Fields")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": rb.ID}, bson.M{"$push": bson.M{"members": bson.M{"userId": bob.ID, "isAdmin": false, "isActive": true}}})
	_, _ = w.db.Collection("customFields").InsertMany(ctx, []any{
		bson.M{"_id": "cfB", "boardIds": bson.A{rb.ID}, "name": "Beta", "type": "text", "showOnCard": true, "alwaysOnCard": false},
		bson.M{"_id": "cfA", "boardIds": bson.A{rb.ID}, "name": "Alpha", "type": "text", "showOnCard": false, "alwaysOnCard": true, "sort": 0},
	})
	ac := w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	_, html := get(t, ac, w.srv.URL+"/b/"+rb.ID+"/fields")
	card := html[strings.Index(html, `data-side="card" data-board=`):]
	mini := html[strings.Index(html, `data-side="minicard" data-board=`):]
	field := func(pane, id string) string {
		at := strings.Index(pane, `js-card-settings-custom-field" data-id="`+id+`"`)
		if at < 0 {
			t.Fatalf("field %s listed", id)
		}
		return pane[at : at+400]
	}
	if strings.Index(card, `data-id="cfA"`) > strings.Index(card, `data-id="cfB"`) {
		t.Fatal("by sort: Alpha (sort 0) first")
	}
	if !strings.Contains(field(card, "cfA"), `aria-checked="true"`) || strings.Contains(field(card, "cfB"), `aria-checked="true"`) {
		t.Fatal("the card's checkbox is alwaysOnCard")
	}
	if strings.Contains(field(mini, "cfA"), `aria-checked="true"`) || !strings.Contains(field(mini, "cfB"), `aria-checked="true"`) {
		t.Fatal("the minicard's is showOnCard")
	}
	if !strings.Contains(html, `js-card-settings-add-custom-field"`) || !strings.Contains(html, `js-card-settings-field-form" data-id="cfA"`) {
		t.Fatal("the + and each field's form")
	}
	bc := w.client(t)
	body(t, w.signIn(t, bc, "bob", "pw"))
	if _, html := get(t, bc, w.srv.URL+"/b/"+rb.ID+"/fields"); strings.Contains(html, "js-card-settings-custom-field\"") {
		t.Fatal("a member who is no admin gets no field list")
	}
}

// WeKan's setBoardDragging and Card's Draggable table: a board admin's, an
// unknown kind or a foreign board refused; checklist items made undraggable
// say so on the opened card.
func TestSetBoardDragging(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	w.seedUser(t, "bob", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Drag")
	foreign := newRESTBoard(t, w, login(t, w, "bob"), "Foreign")
	ac := w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	call := func(args string) map[string]any {
		_, out := methodCall(t, w, ac, "setBoardDragging", args)
		return out
	}
	if out := call(`["` + foreign.ID + `","card",false]`); out["error"] != "not-authorized" {
		t.Fatalf("a board the caller is no admin of: %v", out)
	}
	if out := call(`["` + rb.ID + `","unknown",false]`); out["error"] != "invalid-setting" {
		t.Fatalf("an unknown kind: %v", out)
	}
	if out := call(`["` + rb.ID + `","card","false"]`); out["error"] == nil {
		t.Fatal("a boolean only")
	}
	if out := call(`["` + rb.ID + `","item",false]`); out["error"] != nil {
		t.Fatalf("the admin's: %v", out)
	}
	var b bson.M
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": rb.ID}).Decode(&b)
	if b["allowsChecklistItemDragging"] != false || b["allowsCardDragging"] != nil {
		t.Fatalf("only that kind: %v", b)
	}
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": foreign.ID}).Decode(&b)
	if b["allowsCardDragging"] == false {
		t.Fatal("the foreign board untouched")
	}
	_, _ = w.db.Collection("checklists").InsertOne(ctx, bson.M{"_id": "cl1", "cardId": rb.Card, "boardId": rb.ID, "title": "Locked", "sort": 0})
	_, html := get(t, ac, w.srv.URL+"/b/"+rb.ID+"/drag/"+rb.Card)
	if !strings.Contains(html, `class="board-drag-settings"`) || !strings.Contains(html, `js-board-drag-setting" type="checkbox" data-kind="card" checked`) ||
		strings.Contains(html, `data-kind="item" checked`) {
		t.Fatal("the Draggable table, items unticked")
	}
	if !strings.Contains(html, `data-drag="off"`) {
		t.Fatal("the opened card's checklist items stay put")
	}
}
