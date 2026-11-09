package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// addRule stores a rule of a board: its trigger and action, as WeKan keeps them.
func addRule(t *testing.T, w *testWeKan, boardID, id string, trigger, action bson.M, enabled bool) {
	t.Helper()
	ctx := context.Background()
	trigger["_id"], action["_id"] = id+"t", id+"a"
	if _, ok := trigger["boardId"]; !ok {
		trigger["boardId"] = boardID
	}
	if _, ok := action["boardId"]; !ok {
		action["boardId"] = boardID
	}
	_, _ = w.db.Collection("triggers").InsertOne(ctx, trigger)
	_, _ = w.db.Collection("actions").InsertOne(ctx, action)
	_, _ = w.db.Collection("rules").InsertOne(ctx, bson.M{"_id": id, "boardId": boardID, "title": id, "triggerId": id + "t", "actionId": id + "a", "enabled": enabled})
}

func TestRulesRunOnActivities(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Rules")
	other, _ := w.s.CreateBoard(ctx, u, "Other")
	todo, _ := w.s.AddList(ctx, b, "Todo")
	done, _ := w.s.AddList(ctx, b, "Done")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	// When a card is added to Todo, label it; when it is moved to Done, archive it.
	addRule(t, w, b.ID, "rlabel", bson.M{"activityType": "createCard", "listName": "Todo", "swimlaneName": "*", "userId": "*", "cardTitle": "*"},
		bson.M{"actionType": "addLabel", "labelId": "label0000000001"}, true)
	addRule(t, w, b.ID, "rarch", bson.M{"activityType": "moveCard", "listName": "Done", "oldListName": "*", "userId": "*", "swimlaneName": "*"},
		bson.M{"actionType": "archive"}, true)
	addRule(t, w, b.ID, "roff", bson.M{"activityType": "createCard", "listName": "*"}, bson.M{"actionType": "setColor", "selectedColor": "red"}, false)
	addRule(t, w, other.ID, "rother", bson.M{"activityType": "createCard", "boardId": "*"}, bson.M{"actionType": "setColor", "selectedColor": "blue"}, true)
	c, _ := w.s.AddCard(ctx, b, u, todo.ID, sw.ID, "Alpha")
	var got Card
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": c.ID}).Decode(&got)
	if !slices.Contains(got.LabelIDs, "label0000000001") {
		t.Fatalf("a card added to Todo is labelled: %v", got.LabelIDs)
	}
	var raw bson.M
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": c.ID}).Decode(&raw)
	if raw["color"] != nil {
		t.Fatalf("negative: a disabled rule, and another board's rule, never fire: %v", raw["color"])
	}
	if err := w.s.MoveCards(ctx, b, u, []Move{{CardID: c.ID, ListID: done.ID, SwimlaneID: sw.ID, Sort: 0}}); err != nil {
		t.Fatal(err)
	}
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": c.ID}).Decode(&got)
	if !got.Archived {
		t.Fatal("a card moved to Done is archived")
	}
	// Two rules starting each other stop at WeKan's depth, and it is recorded.
	addRule(t, w, b.ID, "rloop1", bson.M{"activityType": "archivedCard"}, bson.M{"actionType": "unarchive"}, true)
	addRule(t, w, b.ID, "rloop2", bson.M{"activityType": "restoredCard"}, bson.M{"actionType": "archive"}, true)
	c2, _ := w.s.AddCard(ctx, b, u, todo.ID, sw.ID, "Loop")
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": c2.ID}, bson.M{"$set": bson.M{"archived": true}})
	w.s.activity(ctx, u.ID, "archivedCard", bson.M{"boardId": b.ID, "cardId": c2.ID})
	var row struct {
		Count int64 `bson:"count"`
	}
	_ = w.db.Collection("eventlog").FindOne(ctx, bson.M{"bleed": "BypassBleed", "source": "rules"}).Decode(&row)
	if row.Count < 1 {
		t.Fatal("a rule loop is stopped and recorded")
	}
}

func TestRuleCrossBoardMoveNeedsWrite(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Mine")
	foreign, _ := w.s.CreateBoard(ctx, bob, "Foreign")
	l, _ := w.s.AddList(ctx, b, "Todo")
	_, _ = w.s.AddList(ctx, foreign, "Todo")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	addRule(t, w, b.ID, "rmove", bson.M{"activityType": "createCard", "listName": "*"},
		bson.M{"actionType": "moveCardToTop", "boardId": foreign.ID, "listName": "Todo", "swimlaneName": "*"}, true)
	c, _ := w.s.AddCard(ctx, b, u, l.ID, sw.ID, "Stays")
	var got Card
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": c.ID}).Decode(&got)
	if got.BoardID != b.ID {
		t.Fatal("negative: RuleBleed - never onto a board the user may not change")
	}
	var row struct {
		Count int64 `bson:"count"`
	}
	_ = w.db.Collection("eventlog").FindOne(ctx, bson.M{"bleed": "CanaryBleed", "source": "canary:rule.cross-board-write"}).Decode(&row)
	if row.Count != 1 {
		t.Fatalf("the attempt is recorded: %d", row.Count)
	}
}

func TestRunButtonRule(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Buttons")
	victim, _ := w.s.CreateBoard(ctx, bob, "Private")
	mk := func(board *Board, owner *User) *Card {
		l, _ := w.s.AddList(ctx, board, "L")
		var sw Swimlane
		_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": board.ID}).Decode(&sw)
		c, _ := w.s.AddCard(ctx, board, owner, l.ID, sw.ID, "C")
		return c
	}
	mine, theirs := mk(b, u), mk(victim, bob)
	addRule(t, w, b.ID, "rbutton", bson.M{"activityType": "button"}, bson.M{"actionType": "archive"}, true)
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	call := func(args string) (int, map[string]any) {
		req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/"+url.PathEscape("rules.runButton"), strings.NewReader(args))
		req.Header.Set("Content-Type", "application/json")
		res, err := ann.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}
	archived := func(id string) bool {
		var c Card
		_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": id}).Decode(&c)
		return c.Archived
	}
	if _, out := call(`["rbutton","` + theirs.ID + `"]`); out["error"] != "not-authorized" || archived(theirs.ID) {
		t.Fatalf("negative: a button rule cannot archive a foreign private card: %v", out)
	}
	var row struct {
		Count int64 `bson:"count"`
	}
	_ = w.db.Collection("eventlog").FindOne(ctx, bson.M{"bleed": "RuleButtonBleed"}).Decode(&row)
	if row.Count != 1 {
		t.Fatalf("recorded: %d", row.Count)
	}
	if _, out := call(`["rbutton","` + mine.ID + `"]`); out["result"] != true || !archived(mine.ID) {
		t.Fatalf("the board's own card: %v", out)
	}
	if _, out := call(`["nosuchrule","` + mine.ID + `"]`); out["error"] != "not-found" {
		t.Fatalf("negative: no such rule: %v", out)
	}
}

// WeKan's boardButtons and cardButtons: the button rules, for whoever may
// change the board; a disabled one is not shown.
func TestRuleButtonsShown(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	ro := w.seedUser(t, "rob", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Buttons")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$push": bson.M{"members": bson.M{"userId": ro.ID, "isActive": true, "isReadOnly": true}}})
	l, _ := w.s.AddList(ctx, b, "L")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	c, _ := w.s.AddCard(ctx, b, u, l.ID, sw.ID, "C")
	addRule(t, w, b.ID, "rboard", bson.M{"activityType": "button"}, bson.M{"actionType": "archive"}, true)
	addRule(t, w, b.ID, "rcard", bson.M{"activityType": "button"}, bson.M{"actionType": "archive"}, true)
	addRule(t, w, b.ID, "rgone", bson.M{"activityType": "button"}, bson.M{"actionType": "archive"}, false)
	for id, set := range map[string]bson.M{"rboard": {"buttonType": "board", "buttonLabel": "Do the thing"},
		"rcard": {"buttonType": "card", "buttonLabel": "Card thing"}, "rgone": {"buttonType": "board", "buttonLabel": "Hidden"}} {
		_, _ = w.db.Collection("rules").UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": set})
	}
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	cardURL := w.srv.URL + "/b/" + b.ID + "/" + b.Slug + "/" + c.ID
	_, html := get(t, ann, cardURL)
	if !strings.Contains(html, "js-run-board-button") || !strings.Contains(html, "Do the thing") || !strings.Contains(html, "js-run-card-button") || !strings.Contains(html, "Card thing") {
		t.Fatal("the board's and the card's buttons")
	}
	if strings.Contains(html, "Hidden") {
		t.Fatal("negative: a disabled rule has no button")
	}
	rob := w.client(t)
	body(t, w.signIn(t, rob, "rob", "pw"))
	if _, html := get(t, rob, cardURL); strings.Contains(html, "js-run-board-button") || strings.Contains(html, "js-run-card-button") {
		t.Fatal("negative: none for a read-only member")
	}
}

// WeKan's rules.createRule: a board admin's rule; an action naming a board the
// creator may not change is refused (RuleBleed), one they may is kept.
func TestCreateRuleMethod(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Rules")
	mine, _ := w.s.CreateBoard(ctx, u, "Writable target")
	private, _ := w.s.CreateBoard(ctx, bob, "Private target")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$push": bson.M{"members": bson.M{"userId": bob.ID, "isActive": true}}})
	call := func(cl *http.Client, args string) map[string]any {
		req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/rules.createRule", strings.NewReader(args))
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
	move := func(target string) string {
		return `{"actionType":"moveCardToTop","boardId":"` + target + `","listName":"List A","swimlaneName":"Default"}`
	}
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	if out := call(ann, `["`+b.ID+`","Denied",{"activityType":"createCard"},`+move(private.ID)+`]`); out["error"] != "not-authorized" {
		t.Fatalf("negative: a board the creator may not change: %v", out)
	}
	if n, _ := w.db.Collection("rules").CountDocuments(ctx, bson.M{"title": "Denied"}); n != 0 {
		t.Fatal("negative: no rule saved")
	}
	out := call(ann, `["`+b.ID+`","Allowed",{"activityType":"createCard"},`+move(mine.ID)+`]`)
	res, _ := out["result"].(map[string]any)
	if res == nil || res["_id"] == nil {
		t.Fatalf("a writable destination: %v", out)
	}
	var act bson.M
	_ = w.db.Collection("actions").FindOne(ctx, bson.M{"_id": res["actionId"]}).Decode(&act)
	if act["boardId"] != mine.ID {
		t.Fatalf("#5536: the destination is kept: %v", act["boardId"])
	}
	bc := w.client(t)
	body(t, w.signIn(t, bc, "bob", "pw"))
	if out := call(bc, `["`+b.ID+`","Mine",{"activityType":"createCard"},{"actionType":"archive"}]`); out["error"] != "not-authorized" {
		t.Fatalf("negative: not a board admin: %v", out)
	}
}

// WeKan's Rules page: a board admin's list and wizard; nothing for anybody else.
func TestRulesPage(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Rules")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$push": bson.M{"members": bson.M{"userId": bob.ID, "isActive": true}}})
	addRule(t, w, b.ID, "rpage", bson.M{"activityType": "createCard"}, bson.M{"actionType": "archive"}, true)
	_, _ = w.db.Collection("rules").UpdateOne(ctx, bson.M{"_id": "rpage"}, bson.M{"$set": bson.M{"title": "Archive new"}})
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	rulesURL := w.srv.URL + "/b/" + b.ID + "/" + b.Slug + "/rules"
	res, html := get(t, ann, rulesURL)
	for _, want := range []string{"rules-page", "rules-lists-item", "Archive new", `id="ruleTitle"`, "js-goto-trigger", "js-add-create-trigger", "js-add-move-direction-trigger", `value="back"`, "js-add-text-contains-trigger", `id="text-contains-value"`, "js-add-gen-move-action", "js-set-date-action"} {
		if res.StatusCode != 200 || !strings.Contains(html, want) {
			t.Errorf("the Rules page has %s (%d)", want, res.StatusCode)
		}
	}
	bc := w.client(t)
	body(t, w.signIn(t, bc, "bob", "pw"))
	if _, html := get(t, bc, rulesURL); strings.Contains(html, "rules-lists-item") || strings.Contains(html, "js-goto-trigger") {
		t.Fatal("negative: a member who is not an admin sees no rules")
	}
	call := func(cl *http.Client, name, args string) map[string]any {
		req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/"+name, strings.NewReader(args))
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
	if out := call(bc, "rules.setEnabled", `["rpage",false]`); out["error"] != "not-authorized" {
		t.Fatalf("negative: not an admin: %v", out)
	}
	if out := call(ann, "rules.setEnabled", `["rpage",false]`); out["result"] != true {
		t.Fatalf("disabled: %v", out)
	}
	var rule bson.M
	_ = w.db.Collection("rules").FindOne(ctx, bson.M{"_id": "rpage"}).Decode(&rule)
	if rule["enabled"] != false {
		t.Fatal("the rule is disabled")
	}
	if out := call(bc, "rules.deleteRule", `["rpage"]`); out["error"] != "not-authorized" {
		t.Fatal("negative: not an admin")
	}
	if out := call(ann, "rules.deleteRule", `["rpage"]`); out["result"] != true {
		t.Fatalf("deleted: %v", out)
	}
	if n, _ := w.db.Collection("actions").CountDocuments(ctx, bson.M{"_id": "rpagea"}); n != 0 {
		t.Fatal("with its action")
	}
}

// WeKan's rules export: JSON in WeKan's format, or CSV with its columns - the
// ticked rules when some are - for a board admin only.
func TestRulesExport(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Rules")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$push": bson.M{"members": bson.M{"userId": bob.ID, "isActive": true}}})
	addRule(t, w, b.ID, "rone", bson.M{"activityType": "createCard", "listName": "Todo"}, bson.M{"actionType": "archive"}, true)
	addRule(t, w, b.ID, "rtwo", bson.M{"activityType": "moveCard"}, bson.M{"actionType": "unarchive"}, true)
	_, _ = w.db.Collection("rules").UpdateMany(ctx, bson.M{"boardId": b.ID}, bson.M{"$set": bson.M{"title": "R"}})
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	base := w.srv.URL + "/b/" + b.ID + "/" + b.Slug + "/rules/export"
	_, js := get(t, ann, base+"?format=json")
	var out struct {
		Format string   `json:"_format"`
		Rules  []bson.M `json:"rules"`
	}
	if json.Unmarshal([]byte(js), &out) != nil || out.Format != "wekan-rules-1.0.0" || len(out.Rules) != 2 {
		t.Fatalf("the JSON export: %s", js)
	}
	if strings.Contains(js, `"boardId"`+": \""+b.ID+"\",\n      \"") || strings.Contains(js, "rone") {
		t.Fatal("negative: no ids of this board in a rule")
	}
	_, one := get(t, ann, base+"?format=json&ids=rtwo")
	if strings.Count(one, `"title"`) != 1 || !strings.Contains(one, "unarchive") {
		t.Fatalf("only the ticked rule: %s", one)
	}
	_, csvText := get(t, ann, base+"?format=csv")
	if !strings.HasPrefix(csvText, "title,triggerType,triggerFields,actionType,actionFields\r\n") || !strings.Contains(csvText, "createCard") {
		t.Fatalf("the CSV export: %s", csvText)
	}
	bc := w.client(t)
	body(t, w.signIn(t, bc, "bob", "pw"))
	if res, _ := get(t, bc, base+"?format=json"); res.StatusCode != http.StatusNotFound {
		t.Fatalf("negative: not an admin: %d", res.StatusCode)
	}
}

// WeKan's ruleTriggerCopula and ruleNameBeforeSubject: English links with
// "is"; Basque has its verb in the phrase, and the name before its subject.
func TestRuleTriggerGrammar(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Grammar")
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	rulesURL := w.srv.URL + "/b/" + b.ID + "/" + b.Slug + "/rules"
	_, en := get(t, ann, rulesURL)
	i := strings.Index(en, `id="spec-member"`)
	if i < 0 || !strings.Contains(en[i:i+400], `<div class="trigger-text">is</div>`) || !strings.Contains(en[i-200:i], "When the member") {
		t.Fatal("English: the subject, the name, then is")
	}
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": u.ID}, bson.M{"$set": bson.M{"profile.language": "eu"}})
	_, eu := get(t, ann, rulesURL)
	i = strings.Index(eu, `id="spec-member"`)
	if i < 0 || strings.Contains(eu[i:i+400], `<div class="trigger-text">da</div>`) || !strings.Contains(eu[i:i+400], "Kide hau") {
		t.Fatal("negative: Basque has no separate da, and its name comes before Kide hau")
	}
}

// #2076: a "moved forward" rule fires on a move to a later list in the board's
// list order, never on a move back - and "moved back" the other way round.
func TestRuleMoveDirection(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Direction")
	first, _ := w.s.AddList(ctx, b, "First")
	second, _ := w.s.AddList(ctx, b, "Second")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	addRule(t, w, b.ID, "rfwd", bson.M{"activityType": "moveCardDirection", "direction": "forward", "cardTitle": "*"},
		bson.M{"actionType": "setColor", "selectedColor": "red"}, true)
	addRule(t, w, b.ID, "rback", bson.M{"activityType": "moveCardDirection", "direction": "back", "cardTitle": "Beta"},
		bson.M{"actionType": "setColor", "selectedColor": "blue"}, true)
	alpha, _ := w.s.AddCard(ctx, b, u, first.ID, sw.ID, "Alpha")
	beta, _ := w.s.AddCard(ctx, b, u, second.ID, sw.ID, "Beta")
	gamma, _ := w.s.AddCard(ctx, b, u, second.ID, sw.ID, "Gamma")
	color := func(id string) any {
		var c bson.M
		_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": id}).Decode(&c)
		return c["color"]
	}
	move := func(id, list string) {
		if err := w.s.MoveCards(ctx, b, u, []Move{{CardID: id, ListID: list, SwimlaneID: sw.ID, Sort: 0}}); err != nil {
			t.Fatal(err)
		}
	}
	move(alpha.ID, second.ID)
	move(beta.ID, first.ID)
	move(gamma.ID, first.ID)
	if color(alpha.ID) != "red" || color(beta.ID) != "blue" {
		t.Errorf("forward and back: %v %v", color(alpha.ID), color(beta.ID))
	}
	if color(gamma.ID) != nil {
		t.Errorf("negative: moved back, but its title has no word Beta: %v", color(gamma.ID))
	}
	// Negative: within the same list, and between lists of equal sort, there is no direction.
	_, _ = w.db.Collection("lists").UpdateOne(ctx, bson.M{"_id": second.ID}, bson.M{"$set": bson.M{"sort": first.Sort}})
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": gamma.ID}, bson.M{"$unset": bson.M{"color": ""}})
	move(gamma.ID, second.ID)
	if color(gamma.ID) != nil {
		t.Errorf("negative: equal sort is no direction: %v", color(gamma.ID))
	}
	if w.s.moveDirection(ctx, first.ID, first.ID) != "" {
		t.Error("negative: the same list is no direction")
	}
}

// #2194: "a card's title or description contains" - case-insensitive, on a new
// card, a changed title and a changed description; WeKan's activities for the
// last two carry what was and what is.
func TestRuleTextContains(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	a := login(t, w, "ann")
	b := newRESTBoard(t, w, a, "Text")
	ctx := context.Background()
	addRule(t, w, b.ID, "rtext", bson.M{"activityType": "textContainsTrigger", "textContains": "URGENT"},
		bson.M{"actionType": "setColor", "selectedColor": "red"}, true)
	addRule(t, w, b.ID, "rempty", bson.M{"activityType": "textContainsTrigger", "textContains": "  "},
		bson.M{"actionType": "setColor", "selectedColor": "blue"}, true)
	color := func(id string) any {
		var c bson.M
		_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": id}).Decode(&c)
		return c["color"]
	}
	reset := func(id string) {
		_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$unset": bson.M{"color": ""}})
	}
	cards := w.srv.URL + "/api/boards/" + b.ID + "/lists/" + b.List + "/cards"
	var made struct {
		ID string `json:"_id"`
	}
	call(t, "POST", cards, a, `{"title":"An urgent fix","swimlaneId":"`+b.Swimlane+`"}`, &made)
	if color(made.ID) != "red" {
		t.Errorf("a new card whose title contains it, in any case: %v", color(made.ID))
	}
	if color(b.Card) != nil {
		t.Errorf("negative: a card without the text: %v", color(b.Card))
	}
	edit := func(body string) {
		if code := call(t, "PUT", cards+"/"+b.Card, a, body, nil); code != 200 {
			t.Fatalf("edit: %d", code)
		}
	}
	edit(`{"description":"this is Urgent now"}`)
	if color(b.Card) != "red" {
		t.Errorf("a description changed to contain it: %v", color(b.Card))
	}
	var act bson.M
	_ = w.db.Collection("activities").FindOne(ctx, bson.M{"activityType": "a-changedDescription", "cardId": b.Card}).Decode(&act)
	if act["oldValue"] != "" || act["value"] != "this is Urgent now" || act["cardTitle"] != "C" || act["username"] != "ann" {
		t.Errorf("WeKan's a-changedDescription: %v", act)
	}
	reset(b.Card)
	edit(`{"description":"calm","title":"URGENT title"}`)
	_ = w.db.Collection("activities").FindOne(ctx, bson.M{"activityType": "a-changedTitle", "cardId": b.Card}).Decode(&act)
	if color(b.Card) != "red" || act["oldValue"] != "C" || act["value"] != "URGENT title" {
		t.Errorf("a title changed to contain it: %v %v", color(b.Card), act)
	}
	reset(b.Card)
	edit(`{"title":"URGENT title"}`)
	if color(b.Card) != nil {
		t.Error("negative: an unchanged title is no activity, and starts no rule")
	}
	if n, _ := w.db.Collection("activities").CountDocuments(ctx, bson.M{"activityType": "a-changedTitle", "cardId": b.Card}); n != 1 {
		t.Errorf("negative: one title change, one activity: %d", n)
	}
	if textContains("anything", " ") || textContains("", "x") || !textContains("Ab", "aB") {
		t.Error("WeKan's textContainsMatch")
	}
}

// #4294 / #3195: the action {tokens} - the card's people and its custom
// fields - in actions and in trigger values, read for the activity's card.
func TestRuleTokens(t *testing.T) {
	w := newTestWeKan(t)
	ann := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, ann, "Tokens")
	setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: true})
	todo, _ := w.s.AddList(ctx, b, "Todo")
	target, _ := w.s.AddList(ctx, b, "Review")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	_, _ = w.db.Collection("customFields").InsertOne(ctx, bson.M{"_id": "cftarget0000001", "boardIds": bson.A{b.ID}, "name": "Target", "type": "text"})
	_, _ = w.db.Collection("customFields").InsertOne(ctx, bson.M{"_id": "cfsecret0000001", "boardIds": bson.A{b.ID}, "name": "Secret", "type": "text", "adminOnly": true})
	_, _ = w.db.Collection("customFields").InsertOne(ctx, bson.M{"_id": "cfstage00000001", "boardIds": bson.A{b.ID}, "name": "Stage", "type": "dropdown",
		"settings": bson.M{"dropdownItems": bson.A{bson.M{"_id": "opt1", "name": "Done"}}}})
	alpha, _ := w.s.AddCard(ctx, b, ann, todo.ID, sw.ID, "Alpha")
	beta, _ := w.s.AddCard(ctx, b, ann, todo.ID, sw.ID, "Beta")
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": alpha.ID}, bson.M{"$set": bson.M{"assignees": bson.A{bob.ID}, "customFields": bson.A{
		bson.M{"_id": "cftarget0000001", "value": "Review"}, bson.M{"_id": "cfsecret0000001", "value": "hush"}, bson.M{"_id": "cfstage00000001", "value": "opt1"}}}})
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": beta.ID}, bson.M{"$set": bson.M{"customFields": bson.A{bson.M{"_id": "cftarget0000001", "value": "Elsewhere"}}}})
	var card Card
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": alpha.ID}).Decode(&card)
	v := w.s.ruleVars(ctx, bson.M{"boardId": b.ID, "userId": ann.ID}, &card, ann)
	got := substituteVars("{creator}|{assignees}|{customField:target}|{customField:Stage}|{customField:Secret}|{card}|{list}|{board}|{nobody}", v)
	if got != "ann|bob|Review|Done|{customField:Secret}|Alpha|Todo|Tokens|{nobody}" {
		t.Errorf("tokens: %s", got)
	}
	// A trigger list name read from the card's own field: only the card whose
	// Target names the list it moved to.
	addRule(t, w, b.ID, "rtarget", bson.M{"activityType": "moveCard", "listName": "{customField:Target}", "oldListName": "*", "swimlaneName": "*", "userId": "*", "cardTitle": "*"},
		bson.M{"actionType": "setColor", "selectedColor": "red"}, true)
	addRule(t, w, b.ID, "rby", bson.M{"activityType": "moveCard", "listName": "*", "oldListName": "*", "swimlaneName": "*", "userId": "{assignees}", "cardTitle": "*"},
		bson.M{"actionType": "setColor", "selectedColor": "blue"}, true)
	color := func(id string) any {
		var c bson.M
		_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": id}).Decode(&c)
		return c["color"]
	}
	for _, id := range []string{alpha.ID, beta.ID} {
		if err := w.s.MoveCards(ctx, b, ann, []Move{{CardID: id, ListID: target.ID, SwimlaneID: sw.ID}}); err != nil {
			t.Fatal(err)
		}
	}
	if color(alpha.ID) != "red" || color(beta.ID) != nil {
		t.Errorf("negative too: only the card whose field names the list: %v %v", color(alpha.ID), color(beta.ID))
	}
	// "by one of the card's {assignees}": bob is alpha's, ann is not.
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": alpha.ID}, bson.M{"$unset": bson.M{"color": ""}})
	if err := w.s.MoveCards(ctx, b, bob, []Move{{CardID: alpha.ID, ListID: todo.ID, SwimlaneID: sw.ID}}); err != nil {
		t.Fatal(err)
	}
	if color(alpha.ID) != "blue" {
		t.Errorf("by an assignee: %v", color(alpha.ID))
	}
	if toks := w.s.ruleVariableTokens(ctx, b.ID); !slices.Contains(toks, "{customField:Target}") || slices.Contains(toks, "{customField:Secret}") || toks[0] != "{creator}" {
		t.Errorf("the picker: %v", toks)
	}
}

// WeKan's setDateRelative (#5621) and sortList actions.
func TestRuleRelativeDateAndSortList(t *testing.T) {
	base := time.Date(2026, 1, 31, 10, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		value any
		unit  string
		want  time.Time
	}{
		{float64(2), "days", base.AddDate(0, 0, 2)}, {"3 days", "", base.AddDate(0, 0, 3)}, {float64(-1), "weeks", base.AddDate(0, 0, -7)},
		{float64(90), "minutes", base.Add(90 * time.Minute)}, {float64(5), "hours", base.Add(5 * time.Hour)},
		{float64(1), "months", time.Date(2026, 3, 3, 10, 0, 0, 0, time.UTC)}, {"nonsense", "days", base}, {float64(2), "fortnights", base.AddDate(0, 0, 2)},
	} {
		if got := relativeDateOffset(base, c.value, c.unit); !got.Equal(c.want) {
			t.Errorf("%v %s: %v, want %v", c.value, c.unit, got, c.want)
		}
	}
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Sorting")
	todo, _ := w.s.AddList(ctx, b, "Todo")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	addRule(t, w, b.ID, "rsort", bson.M{"activityType": "createCard", "cardTitle": "*"}, bson.M{"actionType": "sortList", "listName": "*", "sortField": "name"}, true)
	addRule(t, w, b.ID, "rdue", bson.M{"activityType": "createCard", "cardTitle": "*"}, bson.M{"actionType": "setDateRelative", "dateField": "dueAt", "days": float64(2), "unit": "days"}, true)
	var ids []string
	for _, title := range []string{"cherry", "Apple", "banana"} {
		c, _ := w.s.AddCard(ctx, b, u, todo.ID, sw.ID, title)
		ids = append(ids, c.ID)
	}
	order := func() []string {
		cur, _ := w.db.Collection("cards").Find(ctx, bson.M{"listId": todo.ID}, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}}))
		var cs []Card
		_ = cur.All(ctx, &cs)
		var out []string
		for _, c := range cs {
			out = append(out, c.Title)
		}
		return out
	}
	if got := order(); !slices.Equal(got, []string{"Apple", "banana", "cherry"}) {
		t.Errorf("sorted by name, any case: %v", got)
	}
	var raw bson.M
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": ids[0]}).Decode(&raw)
	due, _ := plain(raw["dueAt"]).(time.Time)
	if d := time.Until(due); d < 47*time.Hour || d > 49*time.Hour {
		t.Errorf("due in two days: %v", due)
	}
	if n, _ := w.db.Collection("activities").CountDocuments(ctx, bson.M{"activityType": "a-dueAt", "cardId": ids[0]}); n != 1 {
		t.Errorf("WeKan's a-dueAt: %d", n)
	}
	// Negative: a "date field" that is not one of the card's dates is never written.
	if err := w.s.performAction(ctx, bson.M{"boardId": b.ID, "cardId": ids[0], "userId": u.ID},
		bson.M{"actionType": "setDateRelative", "dateField": "title", "days": float64(1)}); err != nil {
		t.Fatal(err)
	}
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": ids[0]}).Decode(&raw)
	if raw["title"] != "cherry" {
		t.Errorf("negative: the title stays: %v", raw["title"])
	}
	c := w.client(t)
	body(t, w.signIn(t, c, "ann", "pw"))
	_, html := get(t, c, w.srv.URL+"/b/"+b.ID+"/"+b.Slug+"/rules")
	for _, want := range []string{"js-sort-list-action", `id="sort-field"`, "js-set-reldate-action", `id="reldate-unit"`} {
		if !strings.Contains(html, want) {
			t.Errorf("the Rules page has %s", want)
		}
	}
}

// WeKan's linkCard action: a linked card of the original, in the named list
// and swimlane of a board its user may change, with only that board's fields.
func TestRuleLinkCard(t *testing.T) {
	w := newTestWeKan(t)
	ann := w.seedUser(t, "ann", "pw")
	eve := w.seedUser(t, "eve", "pw")
	ctx := context.Background()
	src, _ := w.s.CreateBoard(ctx, ann, "Source")
	dst, _ := w.s.CreateBoard(ctx, ann, "Destination")
	hidden, _ := w.s.CreateBoard(ctx, eve, "Hidden")
	in, _ := w.s.AddList(ctx, src, "In")
	w.s.AddList(ctx, dst, "Linked")
	w.s.AddList(ctx, hidden, "Linked")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": src.ID}).Decode(&sw)
	_, _ = w.db.Collection("customFields").InsertOne(ctx, bson.M{"_id": "srcfield0000001", "name": "Here", "type": "text", "boardIds": bson.A{src.ID}})
	addRule(t, w, src.ID, "rlink", bson.M{"activityType": "createCard", "cardTitle": "*"},
		bson.M{"actionType": "linkCard", "boardId": dst.ID, "listName": "Linked", "swimlaneName": "*"}, true)
	c, _ := w.s.AddCard(ctx, src, ann, in.ID, sw.ID, "Original")
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": c.ID}, bson.M{"$set": bson.M{"labelIds": bson.A{"l1"}, "customFields": bson.A{bson.M{"_id": "srcfield0000001", "value": "x"}}}})
	var link bson.M
	if w.db.Collection("cards").FindOne(ctx, bson.M{"boardId": dst.ID, "linkedId": c.ID}).Decode(&link) != nil {
		t.Fatal("linked into the destination")
	}
	if link["type"] != "cardType-linkedCard" || link["_id"] == c.ID || link["swimlaneId"] == "" {
		t.Errorf("a linked card in the board's first swimlane: %v", link)
	}
	// A link of the link names the original; no labels, no other board's fields.
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": link["_id"]}, bson.M{"$set": bson.M{"labelIds": bson.A{"l1"}, "customFields": bson.A{bson.M{"_id": "srcfield0000001", "value": "x"}}}})
	addRule(t, w, dst.ID, "rlink2", bson.M{"activityType": "archivedCard"}, bson.M{"actionType": "linkCard", "boardId": dst.ID, "listName": "Linked", "swimlaneName": "*"}, true)
	w.s.activity(ctx, ann.ID, "archivedCard", bson.M{"boardId": dst.ID, "cardId": link["_id"]})
	cur, _ := w.db.Collection("cards").Find(ctx, bson.M{"boardId": dst.ID, "linkedId": c.ID})
	var links []bson.M
	_ = cur.All(ctx, &links)
	if len(links) != 2 {
		t.Fatalf("a link of a link: %d", len(links))
	}
	for _, l := range links {
		if l["_id"] != link["_id"] && (l["labelIds"] != nil || len(l["customFields"].(bson.A)) != 0) {
			t.Errorf("negative: no labels, no other board's fields: %v", l)
		}
	}
	// Negative: a board its user may not change, and a list that is not there.
	for _, act := range []bson.M{{"actionType": "linkCard", "boardId": hidden.ID, "listName": "Linked"}, {"actionType": "linkCard", "boardId": dst.ID, "listName": "Nowhere"}} {
		before, _ := w.db.Collection("cards").CountDocuments(ctx, bson.M{"linkedId": c.ID})
		if err := w.s.performAction(ctx, bson.M{"boardId": src.ID, "cardId": c.ID, "userId": ann.ID}, act); err != nil {
			t.Fatal(err)
		}
		if after, _ := w.db.Collection("cards").CountDocuments(ctx, bson.M{"linkedId": c.ID}); after != before {
			t.Errorf("negative: nothing linked by %v", act)
		}
	}
	if n, _ := w.db.Collection("cards").CountDocuments(ctx, bson.M{"boardId": hidden.ID}); n != 0 {
		t.Error("negative: nothing on the hidden board")
	}
	var ev struct {
		Count int64 `bson:"count"`
	}
	_ = w.db.Collection("eventlog").FindOne(ctx, bson.M{"source": "canary:rule.cross-board-write"}).Decode(&ev)
	if ev.Count < 1 {
		t.Error("the cross-board write is recorded")
	}
}

// The Rules page offers "Edit trigger/action" on each rule, to its admin.
func TestRuleEditButton(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Editing")
	addRule(t, w, b.ID, "redit", bson.M{"activityType": "createCard"}, bson.M{"actionType": "archive"}, true)
	c := w.client(t)
	body(t, w.signIn(t, c, "ann", "pw"))
	if _, html := get(t, c, w.srv.URL+"/b/"+b.ID+"/"+b.Slug+"/rules"); !strings.Contains(html, `js-edit-rule-full" type="button" data-rule-id="redit"`) &&
		!strings.Contains(html, `data-rule-id="redit" data-title="redit"`) {
		t.Errorf("the edit button: %s", html[strings.Index(html, "rules-lists-item"):][:600])
	}
}
