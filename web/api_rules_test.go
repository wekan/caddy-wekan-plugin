package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

const ruleBody = `{"title":"On create move to top","trigger":{"activityType":"createCard","listName":"","cardTitle":"*","userId":"*"},` +
	`"action":{"actionType":"moveCardToTop","listName":"*","swimlaneName":"*"}}`

func TestRESTRules(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	a, bt := login(t, w, "ann"), login(t, w, "bob")
	b := newRESTBoard(t, w, a, "Rules")
	base := w.srv.URL + "/api/boards/" + b.ID + "/rules"
	ctx := context.Background()

	var ids struct {
		ID        string `json:"_id"`
		TriggerID string `json:"triggerId"`
		ActionID  string `json:"actionId"`
	}
	if code := call(t, "POST", base, a, ruleBody, &ids); code != 200 || ids.ID == "" || ids.TriggerID == "" || ids.ActionID == "" {
		t.Fatalf("create: %d %+v", code, ids)
	}
	var trigger bson.M
	_ = w.db.Collection("triggers").FindOne(ctx, bson.M{"_id": ids.TriggerID}).Decode(&trigger)
	if trigger["activityType"] != "createCard" || trigger["listName"] != "*" || trigger["swimlaneName"] != "*" || trigger["boardId"] != b.ID {
		t.Fatalf("a trigger's empty matching fields match anything, as WeKan's triggersDef: %v", trigger)
	}
	var list []map[string]any
	if code := call(t, "GET", base, a, "", &list); code != 200 || len(list) != 1 {
		t.Fatalf("list: %d %v", code, list)
	}
	if tr, _ := list[0]["trigger"].(map[string]any); tr["activityType"] != "createCard" || tr["_id"] != nil || tr["boardId"] != nil {
		t.Fatalf("the trigger is embedded without _id and boardId: %v", tr)
	}
	call(t, "PUT", base+"/"+ids.ID, a, `{"title":"Renamed","action":{"actionType":"moveCardToBottom"}}`, nil)
	var action, rule bson.M
	_ = w.db.Collection("actions").FindOne(ctx, bson.M{"_id": ids.ActionID}).Decode(&action)
	_ = w.db.Collection("rules").FindOne(ctx, bson.M{"_id": ids.ID}).Decode(&rule)
	if action["actionType"] != "moveCardToBottom" || action["listName"] != "*" || rule["title"] != "Renamed" {
		t.Fatalf("an edit patches the rule's own action: %v %v", action, rule)
	}

	// A trigger another rule shares is never changed under it: the edit gets a copy.
	_, _ = w.db.Collection("rules").InsertOne(ctx, bson.M{"_id": "sharedrule12345", "boardId": b.ID, "title": "x",
		"triggerId": ids.TriggerID, "actionId": ids.ActionID})
	call(t, "PUT", base+"/"+ids.ID, a, `{"trigger":{"activityType":"archivedCard"}}`, nil)
	_ = w.db.Collection("triggers").FindOne(ctx, bson.M{"_id": ids.TriggerID}).Decode(&trigger)
	_ = w.db.Collection("rules").FindOne(ctx, bson.M{"_id": ids.ID}).Decode(&rule)
	if trigger["activityType"] != "createCard" || rule["triggerId"] == ids.TriggerID {
		t.Fatalf("negative: the shared trigger stays, the rule gets its own: %v %v", trigger["activityType"], rule["triggerId"])
	}

	// Negative: what may never be stored, and who may not write.
	for _, bad := range []string{`{"trigger":{"activityType":"x","$where":"1"},"action":{"actionType":"y"}}`,
		`{"trigger":{"activityType":"x","a.b":"1"},"action":{"actionType":"y"}}`,
		`{"trigger":{"activityType":"x","o":{"$ne":1}},"action":{"actionType":"y"}}`,
		`{"trigger":{"listName":"*"},"action":{"actionType":"y"}}`, `{"title":"no parts"}`} {
		if code := call(t, "POST", base, a, bad, nil); code != 400 {
			t.Fatalf("negative: %s: %d", bad, code)
		}
	}
	if code := call(t, "GET", base, "", "", nil); code != 401 {
		t.Fatalf("negative: no token: %d", code)
	}
	setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: true})
	if code := call(t, "GET", base+"/"+ids.ID, bt, "", nil); code != 200 {
		t.Fatalf("a member reads a rule: %d", code)
	}
	if code := call(t, "DELETE", base+"/"+ids.ID, bt, "", nil); code != 403 {
		t.Fatalf("negative: a member who is not a board admin: %d", code)
	}
	var row bson.M
	if err := w.db.Collection("eventlog").FindOne(ctx, bson.M{"bleed": "ManageBoardBleed", "source": "rest:board-admin"}).Decode(&row); err != nil {
		t.Fatalf("the refusal is in Admin Panel -> Problems: %v", err)
	}

	// Delete: the rule, and only the parts no other rule uses.
	if code := call(t, "DELETE", base+"/"+ids.ID, a, "", nil); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	if w.db.Collection("rules").FindOne(ctx, bson.M{"_id": ids.ID}).Err() == nil || w.db.Collection("triggers").FindOne(ctx, bson.M{"_id": rule["triggerId"]}).Err() == nil {
		t.Fatal("the rule and its own trigger are gone")
	}
	if w.db.Collection("actions").FindOne(ctx, bson.M{"_id": ids.ActionID}).Err() != nil || w.db.Collection("triggers").FindOne(ctx, bson.M{"_id": ids.TriggerID}).Err() != nil {
		t.Fatal("negative: the parts the other rule uses stay")
	}
}

func getAPI(t *testing.T, url, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := testClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func TestExportImportBoard(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	a, bt := login(t, w, "ann"), login(t, w, "bob")
	b := newRESTBoard(t, w, a, "Export")
	ctx := context.Background()
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": b.Card}, bson.M{"$set": bson.M{
		"description": "<b>Meteor 3 roadmap</b><script>alert(1)</script>", "secretNote": "x"}})
	call(t, "POST", w.srv.URL+"/api/boards/"+b.ID+"/rules", a, ruleBody, nil)
	var cl struct {
		ID string `json:"_id"`
	}
	call(t, "POST", w.srv.URL+"/api/boards/"+b.ID+"/cards/"+b.Card+"/checklists", a, `{"title":"CL","items":["one","two"]}`, &cl)
	call(t, "POST", w.srv.URL+"/api/boards/"+b.ID+"/cards/"+b.Card+"/comments", a, `{"comment":"hello"}`, nil)
	var c2 struct {
		ID string `json:"_id"`
	}
	call(t, "POST", w.srv.URL+"/api/boards/"+b.ID+"/lists/"+b.List+"/cards", a, `{"title":"Sub","swimlaneId":"`+b.Swimlane+`","parentId":"`+b.Card+`"}`, &c2)

	exportURL := w.srv.URL + "/api/boards/" + b.ID + "/export"
	code, body := getAPI(t, exportURL+"?attachments=false", a)
	var exp map[string]any
	if code != 200 || json.Unmarshal([]byte(body), &exp) != nil || exp["_format"] != "wekan-board-1.0.0" {
		t.Fatalf("export: %d %.200s", code, body)
	}
	for _, k := range []string{"lists", "cards", "swimlanes", "attachments", "rules", "triggers", "actions", "checklists", "checklistItems", "comments", "users", "subtaskItems"} {
		if _, ok := exp[k].([]any); !ok {
			t.Fatalf("the export has %s", k)
		}
	}
	for _, c := range exp["cards"].([]any) {
		m := c.(map[string]any)
		if m["_id"] == b.Card && (m["description"] != "Meteor 3 roadmap" || m["secretNote"] != nil || m["boardId"] != nil) {
			t.Fatalf("markup and secret-looking fields do not leave, nor the boardId: %v", m)
		}
	}
	if strings.Contains(body, "bcrypt") || strings.Contains(body, "services") || strings.Contains(body, "hashedToken") {
		t.Fatal("negative: no user secret in an export")
	}

	// Who may export: a member, a public board's reader, a download link's token.
	if code, _ := getAPI(t, exportURL, ""); code != 401 {
		t.Fatalf("negative: no token on a private board: %d", code)
	}
	if code, _ := getAPI(t, exportURL+"?authToken=nosuchtoken", ""); code != 401 {
		t.Fatalf("negative: an authToken that is no login token: %d", code)
	}
	if code, _ := getAPI(t, exportURL+"?authToken="+a, ""); code != 200 {
		t.Fatalf("a download link with the login token: %d", code)
	}
	if code, _ := getAPI(t, exportURL, bt); code != 403 {
		t.Fatalf("negative: a non-member: %d", code)
	}
	setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: true, IsReadAssignedOnly: true})
	if code, _ := getAPI(t, exportURL, bt); code != 403 {
		t.Fatalf("negative: an assigned-only member, who may not see every card: %d", code)
	}

	// Import: a new board of the caller, everything recreated with new ids.
	exp["cards"].([]any)[0].(map[string]any)["$set"] = map[string]any{"x": 1}
	exp["cards"].([]any)[0].(map[string]any)["a.b"] = 1
	payload, _ := json.Marshal(map[string]any{"board": exp})
	var imp struct {
		ID string `json:"_id"`
	}
	if code := call(t, "POST", w.srv.URL+"/api/boards/import", bt, string(payload), &imp); code != 200 || imp.ID == "" || imp.ID == b.ID {
		t.Fatalf("import: %d %v", code, imp)
	}
	var nb Board
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": imp.ID}).Decode(&nb)
	if !nb.AdminOf(bob.ID) || nb.MemberOf(a) || len(nb.Members) != 1 {
		t.Fatalf("the importer's board; unmapped members are not added: %+v", nb.Members)
	}
	count := func(coll string, f bson.M) int64 {
		n, _ := w.db.Collection(coll).CountDocuments(ctx, f)
		return n
	}
	if count("cards", bson.M{"boardId": imp.ID}) != 2 || count("lists", bson.M{"boardId": imp.ID}) != 1 ||
		count("rules", bson.M{"boardId": imp.ID}) != 1 || count("triggers", bson.M{"boardId": imp.ID}) != 1 ||
		count("card_comments", bson.M{"boardId": imp.ID}) != 1 {
		t.Fatal("the import recreates cards, lists, rules, triggers and comments")
	}
	var sub bson.M
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"boardId": imp.ID, "title": "Sub"}).Decode(&sub)
	var parent Card
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"boardId": imp.ID, "title": "C"}).Decode(&parent)
	if sub["parentId"] != parent.ID || sub["userId"] != bob.ID {
		t.Fatalf("references follow the new ids, and the author is the importer: %v", sub)
	}
	var raw bson.M
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": parent.ID}).Decode(&raw)
	if _, ok := raw["$set"]; ok || raw["a.b"] != nil {
		t.Fatal("negative: an operator or a path as a field name is never stored")
	}
	if n := count("checklistItems", bson.M{"cardId": parent.ID}); n != 2 {
		t.Fatalf("checklist items follow their card: %d", n)
	}
	var rule bson.M
	_ = w.db.Collection("rules").FindOne(ctx, bson.M{"boardId": imp.ID}).Decode(&rule)
	if count("triggers", bson.M{"_id": rule["triggerId"], "activityType": "createCard"}) != 1 {
		t.Fatal("the rule names its new trigger")
	}
	if code := call(t, "POST", w.srv.URL+"/api/boards/import", bt, `{"board":{"_format":"other"}}`, nil); code != 400 {
		t.Fatalf("negative: not a WeKan export: %d", code)
	}
	if code := call(t, "POST", w.srv.URL+"/api/boards/import", "", string(payload), nil); code != 401 {
		t.Fatalf("negative: no token: %d", code)
	}

	// Deleting a board: its admin only, and everything of it goes.
	if code := call(t, "DELETE", w.srv.URL+"/api/boards/"+imp.ID, a, "", nil); code != 403 {
		t.Fatalf("negative: not its admin: %d", code)
	}
	if code := call(t, "DELETE", w.srv.URL+"/api/boards/"+imp.ID, bt, "", nil); code != 200 ||
		count("boards", bson.M{"_id": imp.ID}) != 0 || count("cards", bson.M{"boardId": imp.ID}) != 0 || count("rules", bson.M{"boardId": imp.ID}) != 0 {
		t.Fatalf("delete: %d", code)
	}
}

func TestSanitizeInput(t *testing.T) {
	for in, want := range map[string]string{
		"<b>Meteor 3 roadmap</b><script>alert(1)</script>": "Meteor 3 roadmap",
		`<a href="javascript:alert(1)">x</a> & y`:          "x &amp; y",
		"<style>body{}</style>plain":                       "plain",
		"a url(http://x) b":                                "a  b",
	} {
		if got := sanitizeInput(in); got != want {
			t.Errorf("sanitizeInput(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRESTCopiesAndLinkedCards(t *testing.T) {
	w := newTestWeKan(t)
	ann := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	a, bt := login(t, w, "ann"), login(t, w, "bob")
	src, dest := newRESTBoard(t, w, a, "Source"), newRESTBoard(t, w, a, "Dest")
	ctx := context.Background()
	when := mustTime("2017-01-02T03:04:05Z")
	_, _ = w.db.Collection("cards").InsertOne(ctx, bson.M{"_id": "subtask00000001", "boardId": src.ID, "swimlaneId": src.Swimlane,
		"listId": src.List, "parentId": src.Card, "title": "subtask", "sort": 0, "archived": false, "userId": ann.ID})
	_, _ = w.db.Collection("card_comments").InsertOne(ctx, bson.M{"_id": "comment00000001", "boardId": src.ID, "cardId": src.Card,
		"userId": bob.ID, "text": "by bob", "createdAt": when})

	var cp struct {
		ID string `json:"_id"`
	}
	copyURL := w.srv.URL + "/api/boards/" + src.ID + "/lists/" + src.List + "/cards/" + src.Card + "/copy"
	if code := call(t, "POST", copyURL, a, `{"toBoardId":"`+dest.ID+`","toSwimlaneId":"`+dest.Swimlane+`","toListId":"`+dest.List+`"}`, &cp); code != 200 {
		t.Fatalf("copy: %d", code)
	}
	var sub, cm bson.M
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"parentId": cp.ID}).Decode(&sub)
	if sub["boardId"] != dest.ID || sub["listId"] != dest.List || sub["title"] != "subtask" {
		t.Fatalf("the subtask is re-homed onto the destination: %v", sub)
	}
	if n, _ := w.db.Collection("cards").CountDocuments(ctx, bson.M{"parentId": src.Card, "boardId": src.ID}); n != 1 {
		t.Fatal("negative: the original subtask stays where it was")
	}
	_ = w.db.Collection("card_comments").FindOne(ctx, bson.M{"cardId": cp.ID}).Decode(&cm)
	if cm["boardId"] != dest.ID || cm["userId"] != bob.ID || !cm["createdAt"].(bson.DateTime).Time().Equal(when) {
		t.Fatalf("#5166: the comment is on the destination board, with its author and time: %v", cm)
	}
	if code := call(t, "POST", copyURL, bt, `{"toBoardId":"`+bt+`"}`, nil); code != 403 {
		t.Fatalf("negative: a non-member copies nothing: %d", code)
	}

	// #5897: a linked card shows another card the caller may read.
	cards := w.srv.URL + "/api/boards/" + dest.ID + "/lists/" + dest.List + "/cards"
	var ln struct {
		ID string `json:"_id"`
	}
	if code := call(t, "POST", cards, a, `{"swimlaneId":"`+dest.Swimlane+`","linkedId":"`+src.Card+`"}`, &ln); code != 200 {
		t.Fatalf("linked card: %d", code)
	}
	var linked bson.M
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": ln.ID}).Decode(&linked)
	if linked["type"] != "cardType-linkedCard" || linked["linkedId"] != src.Card || linked["title"] != "C" {
		t.Fatalf("a linked card: %v", linked)
	}
	hidden := newRESTBoard(t, w, bt, "Hidden")
	if code := call(t, "POST", cards, a, `{"swimlaneId":"`+dest.Swimlane+`","linkedId":"`+hidden.Card+`"}`, nil); code != 403 {
		t.Fatalf("negative: a card on a board the caller cannot see: %d", code)
	}

	// #5592 / #5627: a board copy takes its webhooks and its rules along.
	_, _ = w.db.Collection("integrations").InsertOne(ctx, bson.M{"_id": "integration0001", "boardId": src.ID, "url": "https://example.com/hook",
		"type": "outgoing-webhooks", "enabled": true})
	call(t, "POST", w.srv.URL+"/api/boards/"+src.ID+"/rules", a, ruleBody, nil)
	var newID string
	if code := call(t, "POST", w.srv.URL+"/api/boards/"+src.ID+"/copy", a, `{"title":"Copied board"}`, &newID); code != 200 || newID == "" || newID == src.ID {
		t.Fatalf("board copy: %d %q", code, newID)
	}
	var rule, hook bson.M
	_ = w.db.Collection("rules").FindOne(ctx, bson.M{"boardId": newID}).Decode(&rule)
	_ = w.db.Collection("integrations").FindOne(ctx, bson.M{"boardId": newID}).Decode(&hook)
	if hook["url"] != "https://example.com/hook" || w.db.Collection("triggers").FindOne(ctx, bson.M{"_id": rule["triggerId"], "boardId": newID}).Err() != nil ||
		w.db.Collection("actions").FindOne(ctx, bson.M{"_id": rule["actionId"], "boardId": newID}).Err() != nil {
		t.Fatalf("the webhook and the rule with its own trigger and action: %v %v", hook, rule)
	}
	var nb Board
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": newID}).Decode(&nb)
	if nb.Title != "Copied board" || !nb.AdminOf(ann.ID) {
		t.Fatalf("the copy: %+v", nb)
	}
	var again string
	call(t, "POST", w.srv.URL+"/api/boards/"+src.ID+"/copy", a, `{}`, &again)
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": again}).Decode(&nb)
	if nb.Title != "Source [1]" {
		t.Fatalf("without a title, WeKan's unique title: %q", nb.Title)
	}
	setMember(t, w, src.ID, Member{UserID: bob.ID, IsActive: true})
	if code := call(t, "POST", w.srv.URL+"/api/boards/"+src.ID+"/copy", bt, `{}`, nil); code != 403 {
		t.Fatalf("negative: only a board admin copies a board: %d", code)
	}
}

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// A rule keeps what WeKan keeps beside its fields - a trigger's extraData -
// as a small plain object; never an operator, a path, or nesting without end.
func TestRuleComponentObjects(t *testing.T) {
	if _, err := ruleComponent(map[string]any{"activityType": "createCard", "extraData": map[string]any{"keep": "unchanged", "n": 1.0}}, "activityType"); err != nil {
		t.Errorf("a small object is kept: %v", err)
	}
	deep := map[string]any{"a": map[string]any{"b": map[string]any{"c": map[string]any{"d": "x"}}}}
	listy := []any{map[string]any{"a": []any{map[string]any{"b": []any{map[string]any{"c": "x"}}}}}}
	for name, v := range map[string]any{
		"an operator":       map[string]any{"$where": "1"},
		"a path":            map[string]any{"a.b": "x"},
		"four deep":         deep,
		"lists and objects": listy,
	} {
		if _, err := ruleComponent(map[string]any{"activityType": "createCard", "extraData": v}, "activityType"); err == nil {
			t.Errorf("negative: %s is refused", name)
		}
	}
}

// Negative: a custom field's value stays flat - no object anywhere.
func TestRuleValueStaysFlat(t *testing.T) {
	for _, v := range []any{map[string]any{"a": "x"}, []any{map[string]any{"a": "x"}}} {
		if ruleValue(v, true) {
			t.Errorf("negative: %v is not a flat value", v)
		}
	}
	if !ruleValue([]any{"a", 1.0, true}, true) {
		t.Error("a list of text, numbers and booleans")
	}
}
