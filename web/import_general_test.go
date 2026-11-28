package web

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// A Kanboard project imported from the import page, as WeKan's
// KanboardCreator writes it: the lists in order with their WIP limit, the
// swimlanes, the tags and the category as labels, every card with its list,
// swimlane, colour, archived state, dates, checklist and comments, and the
// parent and dependency links between the import's own cards.
func TestImportKanboard(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	ann := w.seedUser(t, "ann", "pw")
	c := w.client(t)
	body(t, w.signIn(t, c, "ann", "pw"))
	if !strings.Contains(getBody(t, c, w.srv.URL+"/import/kanboard"), `name="text"`) {
		t.Fatal("the Kanboard import page asks for its JSON")
	}
	raw, _ := os.ReadFile("testdata/import/kanboard-api.json")
	res, err := c.PostForm(w.srv.URL+"/import/kanboard", url.Values{"text": {string(raw)}})
	// The fixture has what Kanboard's parser leaves out: the page says so
	// first, as WeKan's does, and links the new board.
	if err != nil || res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(res.Header.Get("Location"), "/import/kanboard/report?board=") {
		t.Fatalf("the import reports what it left out: %v %d %s", err, res.StatusCode, res.Header.Get("Location"))
	}
	var b Board
	if err := w.db.Collection("boards").FindOne(ctx, bson.M{"title": "Plant"}).Decode(&b); err != nil {
		t.Fatal(err)
	}
	report := getBody(t, c, w.srv.URL+res.Header.Get("Location"))
	if !strings.Contains(report, "js-import-report") || !strings.Contains(report, "import-report-row") ||
		!strings.Contains(report, `href="/b/`+b.ID+`/`+b.Slug+`"`) {
		t.Errorf("the report and the board's link: %s", report)
	}
	// Negative: another user's report is never shown; theirs opens the board.
	w.seedUser(t, "eve", "pw")
	eve := w.client(t)
	body(t, w.signIn(t, eve, "eve", "pw"))
	if page := getBody(t, eve, w.srv.URL+res.Header.Get("Location")); strings.Contains(page, "import-report-row") {
		t.Error("negative: a user who did not import it sees the report")
	}
	if !b.AdminOf(ann.ID) {
		t.Error("the person importing is the board's admin")
	}
	labels := map[string]string{}
	for _, l := range b.Labels {
		labels[l.Name] = l.ID
	}
	for _, want := range []string{"urgent", "shop", "Hardware", "priority:2", "x"} {
		if labels[want] == "" {
			t.Errorf("the label %q", want)
		}
	}
	if len(b.Labels) != 5 {
		t.Errorf("only the tags' labels, not WeKan's six defaults: %d", len(b.Labels))
	}
	find := func(coll string, filter bson.M) []bson.M {
		cur, _ := w.db.Collection(coll).Find(ctx, filter)
		var out []bson.M
		_ = cur.All(ctx, &out)
		return out
	}
	lists := find("lists", bson.M{"boardId": b.ID})
	listID := map[string]string{}
	for _, l := range lists {
		listID[docString(l, "title")] = docString(l, "_id")
	}
	if len(lists) != 2 || listID["Backlog"] == "" || listID["Done"] == "" {
		t.Errorf("the columns as lists: %v", listID)
	}
	for _, l := range lists {
		if docString(l, "title") == "Backlog" {
			if intOf(nestedField(l, "wipLimit", "value")) != 3 || nestedField(l, "wipLimit", "enabled") != true {
				t.Errorf("Backlog's WIP limit is 3: %v", l["wipLimit"])
			}
		}
	}
	lanes := find("swimlanes", bson.M{"boardId": b.ID})
	laneID := map[string]string{}
	for _, l := range lanes {
		laneID[docString(l, "title")] = docString(l, "_id")
	}
	if len(lanes) != 2 || laneID["Ops"] == "" {
		t.Errorf("the swimlanes: %v", laneID)
	}
	cards := map[string]bson.M{}
	for _, cd := range find("cards", bson.M{"boardId": b.ID}) {
		cards[docString(cd, "title")] = cd
	}
	valves := cards["Order valves"]
	if valves == nil || docString(valves, "listId") != listID["Backlog"] || docString(valves, "swimlaneId") != laneID["Ops"] ||
		docString(valves, "color") != "crimson" || valves["archived"] != false || docString(valves, "requestedBy") != "Carl" {
		t.Fatalf("Order valves as it was: %v", valves)
	}
	if due, _ := valves["dueAt"].(bson.DateTime); !due.Time().Equal(time.Unix(1793520000, 0)) {
		t.Errorf("the due date from unix seconds: %v", valves["dueAt"])
	}
	if n, _ := plain(valves["spentTime"]).(float64); n != 1.5 {
		t.Errorf("the time spent: %v", valves["spentTime"])
	}
	untitled := cards["Imported task"]
	if untitled == nil || untitled["archived"] != true || untitled["archivedAt"] == nil || docString(untitled, "color") != "gray" || docString(untitled, "listId") != listID["Done"] {
		t.Errorf("an inactive task is an archived card, its untitled name WeKan's: %v", untitled)
	}
	// The fixture makes Order valves and Child each other's parent: WeKan's
	// planner refuses both links as a cycle, so neither card has a parent.
	child := cards["Child"]
	if child == nil || child["parentId"] != nil || valves["parentId"] != nil {
		t.Errorf("negative: a parent cycle links nothing: valves %v, child %v", valves["parentId"], child["parentId"])
	}
	deps, _ := valves["cardDependencies"].(bson.A)
	if len(deps) != 1 || docString(docMap(deps[0].(bson.D)), "cardId") != docString(untitled, "_id") || docString(docMap(deps[0].(bson.D)), "type") != "blocks" {
		t.Errorf("Order valves blocks the untitled task, once: %v", deps)
	}
	checklists := find("checklists", bson.M{"cardId": docString(valves, "_id")})
	items := find("checklistItems", bson.M{"cardId": docString(valves, "_id")})
	if len(checklists) != 1 || docString(checklists[0], "title") != "Subtasks" || len(items) != 2 {
		t.Errorf("the subtasks as a checklist of two: %d %d", len(checklists), len(items))
	}
	// WeKan's default people choice: each comment author is a placeholder
	// user keeping the name, unable to log in, an inactive member with no
	// rights - and "ann", taken by the person importing, is never the real
	// account: the task's owner (Kanboard user 4, named ann) is a person of
	// its own, keyed "4", and is ann-1, so the comment author "ann" is ann-2,
	// as in WeKan.
	comments := find("card_comments", bson.M{"cardId": docString(valves, "_id")})
	if len(comments) != 2 {
		t.Fatalf("the two comments: %v", comments)
	}
	// Which comment is which by its author, not by the order a scan returns
	// them in: an index on the comments (WeKan's, created at startup) changes
	// that order, and the order is not what the import promises.
	authors := map[string]bson.M{}
	for n := range comments {
		var a bson.M
		if err := w.db.Collection("users").FindOne(ctx, bson.M{"_id": docString(comments[n], "userId")}).Decode(&a); err != nil {
			t.Fatalf("comment %d's author: %v", n, err)
		}
		fullname, _ := nestedField(a, "profile", "fullname").(string)
		authors[fullname] = a
	}
	for n, want := range []struct{ fullname, username string }{{"ann", "ann-2"}, {"Bob", "Bob"}} {
		author := authors[want.fullname]
		if author == nil {
			t.Fatalf("a comment is by %s: %v", want.fullname, authors)
		}
		if author["_id"] == ann.ID || author["loginDisabled"] != true || docString(author, "username") != want.username ||
			nestedField(author, "profile", "fullname") != want.fullname || docString(author, "authenticationMethod") != "imported" {
			t.Errorf("comment %d is by a placeholder %s: %v", n, want.username, author)
		}
		if _, stored := author["isActive"]; stored {
			t.Errorf("negative: isActive is not in WeKan's users schema and is not stored: %v", author)
		}
		var member *Member
		for i := range b.Members {
			if b.Members[i].UserID == docString(author, "_id") {
				member = &b.Members[i]
			}
		}
		if member == nil || member.IsActive || member.IsAdmin {
			t.Errorf("the placeholder is an inactive member with no rights: %+v", member)
		}
	}
	if n := len(find("activities", bson.M{"boardId": b.ID, "activityType": "addComment"})); n != 2 {
		t.Errorf("each comment's activity: %d", n)
	}
	// "me": every person of the file is the person importing, and no
	// placeholder is made.
	users, _ := w.db.Collection("users").CountDocuments(ctx, bson.M{})
	if res, err := c.PostForm(w.srv.URL+"/import/kanboard", url.Values{"text": {string(raw)}, "membersMode": {"me"}}); err != nil || res.StatusCode != http.StatusSeeOther {
		t.Fatalf("the import as me: %v %d", err, res.StatusCode)
	}
	if after, _ := w.db.Collection("users").CountDocuments(ctx, bson.M{}); after != users {
		t.Errorf("negative: me makes no placeholder users: %d -> %d", users, after)
	}
	var mine Board
	opts := options.FindOne().SetSort(bson.D{{Key: "createdAt", Value: -1}})
	if err := w.db.Collection("boards").FindOne(ctx, bson.M{"title": "Plant"}, opts).Decode(&mine); err != nil || mine.ID == b.ID {
		t.Fatalf("the second board: %v", err)
	}
	for _, cm := range find("card_comments", bson.M{"boardId": mine.ID}) {
		if docString(cm, "userId") != ann.ID {
			t.Errorf("with me, the comments are by the person importing: %v", cm)
		}
	}
	// Negative: what is not the source's JSON makes no board.
	before, _ := w.db.Collection("boards").CountDocuments(ctx, bson.M{})
	for name, text := range map[string]string{"malformed": `{"tasks": [`, "not an object": `"tasks"`} {
		res, _ := c.PostForm(w.srv.URL+"/import/kanboard", url.Values{"text": {text}})
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("negative: %s JSON is refused: %d", name, res.StatusCode)
		}
	}
	if after, _ := w.db.Collection("boards").CountDocuments(ctx, bson.M{}); after != before {
		t.Errorf("negative: and no board is made: %d -> %d", before, after)
	}
	// Negative: signed out, nothing is imported.
	res, _ = w.client(t).PostForm(w.srv.URL+"/import/kanboard", url.Values{"text": {string(raw)}})
	if after, _ := w.db.Collection("boards").CountDocuments(ctx, bson.M{}); after != before || res.StatusCode == http.StatusSeeOther && strings.HasPrefix(res.Header.Get("Location"), "/b/") {
		t.Errorf("negative: a signed-out post imports nothing: %d", res.StatusCode)
	}
}

// A text source - Markdown - through the import page: the text itself, as
// WeKan's check(board, String), not JSON.
func TestImportMarkdown(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	c := w.client(t)
	body(t, w.signIn(t, c, "ann", "pw"))
	if page := getBody(t, c, w.srv.URL+"/import/markdown"); !strings.Contains(page, `name="text"`) || !strings.Contains(page, "js-import-without-mapping") {
		t.Fatal("the Markdown import page asks for its text")
	}
	res, err := c.PostForm(w.srv.URL+"/import/markdown", url.Values{"text": {"# Garden\n## To Do\n- [ ] Plant\n    in spring\n## Done\n- [x] Dig\n"}})
	if err != nil || res.StatusCode != http.StatusSeeOther {
		t.Fatalf("the import opens the new board: %v %d", err, res.StatusCode)
	}
	var b Board
	if err := w.db.Collection("boards").FindOne(ctx, bson.M{"title": "Garden"}).Decode(&b); err != nil {
		t.Fatal(err)
	}
	var plant bson.M
	if err := w.db.Collection("cards").FindOne(ctx, bson.M{"boardId": b.ID, "title": "Plant"}).Decode(&plant); err != nil || docString(plant, "description") != "in spring" {
		t.Errorf("the card with its description: %v %v", err, plant)
	}
	if n, _ := w.db.Collection("lists").CountDocuments(ctx, bson.M{"boardId": b.ID}); n != 2 {
		t.Errorf("two lists: %d", n)
	}
	if len(b.Labels) != 1 || b.Labels[0].Name != "done" {
		t.Errorf("the done card's label: %v", b.Labels)
	}
}

func TestImportJira(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	c := w.client(t)
	body(t, w.signIn(t, c, "ann", "pw"))
	const payload = `{"board":{"name":"Jira migration"},"wekanScrumMapping":{"estimateFieldId":"customfield_10016","estimateUnit":"points"},"names":{"customfield_10020":"Rank","customfield_10022":"Sprint","customfield_10023":"Flag","customfield_10024":"Flag"},"issues":[{"key":"TEST-1","fields":{"summary":"Sample issue","customfield_10016":0,"customfield_10023":false,"customfield_10024":{"value":"Choice"},"customfield_10020":"0|abc:","customfield_10022":{"id":7,"name":"Future sprint","state":"future"},"status":{"name":"Doing","statusCategory":{"key":"indeterminate"}},"issuetype":{"name":"Bug"},"description":{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"Jira description"}]}]},"timetracking":{"timeSpentSeconds":0,"originalEstimateSeconds":7200},"timeestimate":0,"labels":["jira","UI"],"priority":{"name":"High"},"components":[{"name":"UI"},{"name":"Backend"}],"subtasks":[{"key":"SUB-1","fields":{"summary":"Done item","status":{"statusCategory":{"key":"done"}}}},{"fields":{"summary":"Open item"}}],"fixVersions":[{"id":"50","name":"Released","released":true,"releaseDate":"2026-10-10","description":"Notes"},{"id":"51","name":"Planned"},{"id":"50","name":"Duplicate"}],"comment":{"comments":[{"author":{"accountId":"jira-user","displayName":"Jira User"},"body":{"type":"paragraph","content":[{"type":"text","text":"First comment"}]},"created":"2026-10-10T12:30:00.000+0200"}]}}}]}`
	res, err := c.PostForm(w.srv.URL+"/import/jira", url.Values{"text": {payload}})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(res.Header.Get("Location"), "/b/") {
		t.Fatalf("Jira import: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	var board Board
	if err := w.db.Collection("boards").FindOne(ctx, bson.M{"title": "Jira migration"}).Decode(&board); err != nil {
		t.Fatal(err)
	}
	labels := map[string]bool{}
	for _, label := range board.Labels {
		labels[label.Name] = true
	}
	for _, name := range []string{"UI", "Backend", "priority:High", "jira"} {
		if !labels[name] {
			t.Fatalf("missing Jira label %q: %v", name, labels)
		}
	}

	var card bson.M
	if err := w.db.Collection("cards").FindOne(ctx, bson.M{"boardId": board.ID}).Decode(&card); err != nil {
		t.Fatal(err)
	}
	if card["title"] != "[TEST-1] Sample issue" || card["description"] != "Jira description" || card["spentTime"] != float64(0) {
		t.Fatalf("Jira card: %v", card)
	}
	if got := asM(card["scrum"])["issueType"]; got != "Bug" {
		t.Fatalf("Jira issue type: %v", got)
	}
	if got, ok := toFloat(asM(card["scrum"])["backlogRank"]); !ok || got != 1 {
		t.Fatalf("Jira backlog rank: %v", asM(card["scrum"]))
	}

	var sprint bson.M
	if err := w.db.Collection("scrumSprints").FindOne(ctx, bson.M{"_id": asM(card["scrum"])["sprintId"], "boardId": board.ID}).Decode(&sprint); err != nil {
		t.Fatal(err)
	}
	if sprint["name"] != "Future sprint" || sprint["state"] != "planned" || asM(sprint["provenance"])["recordId"] != "7" {
		t.Fatalf("Jira sprint: %v", sprint)
	}

	first := strOf(asM(card["scrum"])["releaseId"])
	ids, _ := asM(card["scrum"])["releaseIds"].(bson.A)
	if len(ids) != 2 || ids[0] != first {
		t.Fatalf("Jira release links: %v", asM(card["scrum"]))
	}
	var release bson.M
	if err := w.db.Collection("scrumReleases").FindOne(ctx, bson.M{"_id": first, "boardId": board.ID}).Decode(&release); err != nil {
		t.Fatal(err)
	}
	if release["name"] != "Released" || release["notes"] != "Notes" || release["state"] != "released" ||
		asM(release["provenance"])["recordId"] != "50" ||
		release["releasedAt"] != bson.NewDateTimeFromTime(time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("Jira release: %v", release)
	}

	var list bson.M
	if err := w.db.Collection("lists").FindOne(ctx, bson.M{"boardId": board.ID}).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if got := asM(list["scrum"])["category"]; got != "doing" {
		t.Fatalf("Jira status category: %v", got)
	}
	definitions := fieldReader{boardID: board.ID, admin: true, defs: map[string]bson.M{}}
	for _, estimate := range jiraImportTimeEstimates {
		var field bson.M
		if err := w.db.Collection("customFields").FindOne(ctx, bson.M{"boardIds": board.ID, "name": estimate.name}).Decode(&field); err != nil {
			t.Fatal(err)
		}
		definitions.defs[strOf(field["_id"])] = field
	}
	for name, expected := range map[string]any{"Flag": false, "Flag (2)": "Choice"} {
		var field bson.M
		if err := w.db.Collection("customFields").FindOne(ctx, bson.M{"boardIds": board.ID, "name": name}).Decode(&field); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, assignment := range fieldValues(card) {
			if assignment["_id"] == field["_id"] && assignment["value"] == expected {
				found = true
			}
		}
		if !found {
			t.Fatalf("Jira custom field %s: %v", name, card["customFields"])
		}
	}

	var mapped bson.M
	if err := w.db.Collection("customFields").FindOne(ctx, bson.M{"boardIds": board.ID,
		"settings.jiraEstimateFieldId": "customfield_10016"}).Decode(&mapped); err != nil {
		t.Fatal(err)
	}
	var savedBoard bson.M
	if err := w.db.Collection("boards").FindOne(ctx, bson.M{"_id": board.ID}).Decode(&savedBoard); err != nil {
		t.Fatal(err)
	}
	scrumSettings := asM(savedBoard["scrum"])
	if scrumSettings["estimateCustomFieldId"] != mapped["_id"] || scrumSettings["estimateUnit"] != "points" ||
		scrumSettings["enabled"] != true {
		t.Fatalf("Jira estimate settings: %v", scrumSettings)
	}
	definitions.defs[strOf(mapped["_id"])] = mapped
	mapping := definitions.jiraEstimateMapping()
	if mapping == nil || jsGet(mapping.value(card), "customfield_10016") != float64(0) {
		t.Fatalf("Jira mapped zero: %v", card)
	}

	tracking := jsObject{}
	jiraTimeEstimates(card, definitions.jiraTimeEstimateIds(), &tracking)
	if jsGet(tracking, "originalEstimateSeconds") != float64(7200) ||
		jsGet(tracking, "remainingEstimateSeconds") != float64(0) {
		t.Fatalf("re-exported Jira time estimates: %v", tracking)
	}

	var checklist bson.M
	if err := w.db.Collection("checklists").FindOne(ctx, bson.M{"cardId": card["_id"]}).Decode(&checklist); err != nil {
		t.Fatal(err)
	}
	if checklist["title"] != "Subtasks" {
		t.Fatalf("Jira checklist: %v", checklist)
	}
	cursor, err := w.db.Collection("checklistItems").Find(ctx, bson.M{"checklistId": checklist["_id"]},
		options.Find().SetSort(bson.D{{Key: "sort", Value: 1}}))
	if err != nil {
		t.Fatal(err)
	}
	var items []bson.M
	if err := cursor.All(ctx, &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0]["title"] != "[SUB-1] Done item" || items[0]["isFinished"] != true ||
		items[1]["title"] != "Open item" || items[1]["isFinished"] != false {
		t.Fatalf("Jira checklist items: %v", items)
	}

	var comment bson.M
	if err := w.db.Collection("card_comments").FindOne(ctx, bson.M{"cardId": card["_id"]}).Decode(&comment); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 10, 10, 30, 0, 0, time.UTC)
	if comment["text"] != "First comment" || comment["createdAt"] != bson.NewDateTimeFromTime(at) {
		t.Fatalf("Jira comment: %v", comment)
	}

	var author bson.M
	if err := w.db.Collection("users").FindOne(ctx, bson.M{"_id": comment["userId"]}).Decode(&author); err != nil {
		t.Fatal(err)
	}
	if author["authenticationMethod"] != "imported" || asM(author["profile"])["fullname"] != "Jira User" {
		t.Fatalf("Jira comment author: %v", author)
	}

	withChild := strings.TrimSuffix(strings.Replace(payload, `"key":"TEST-1"`, `"key":"TEST-1","id":10001`, 1), "]}") +
		`,{"key":"TEST-CHILD","fields":{"summary":"Child","parent":{"id":"10001"},"status":{"name":"Done","statusCategory":{"key":"done"}},"created":"2026-10-10T12:30:00.000+0200","updated":"2026-10-10T13:30:00.000+0200","resolutiondate":"2026-10-11T12:30:00.000+0200","duedate":"2026-10-12"}}]}`
	imported, err := c.PostForm(w.srv.URL+"/import/jira", url.Values{"text": {withChild}})
	if err != nil {
		t.Fatal(err)
	}
	defer imported.Body.Close()
	if imported.StatusCode != http.StatusSeeOther || !strings.HasPrefix(imported.Header.Get("Location"), "/b/") {
		t.Fatalf("Jira parent import: %d %s", imported.StatusCode, imported.Header.Get("Location"))
	}
	resultBoard := strings.Split(imported.Header.Get("Location"), "/")[2]
	var parent, child bson.M
	for title, row := range map[string]*bson.M{"[TEST-1] Sample issue": &parent, "[TEST-CHILD] Child": &child} {
		if err := w.db.Collection("cards").FindOne(ctx, bson.M{"boardId": resultBoard, "title": title}).Decode(row); err != nil {
			t.Fatal(err)
		}
	}
	if child["parentId"] != parent["_id"] {
		t.Fatalf("Jira parent link: %v", child)
	}

	if child["createdAt"] != bson.NewDateTimeFromTime(at) ||
		child["modifiedAt"] != bson.NewDateTimeFromTime(at.Add(time.Hour)) ||
		child["endAt"] != bson.NewDateTimeFromTime(at.AddDate(0, 0, 1)) ||
		child["dueAt"] != bson.NewDateTimeFromTime(time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)) ||
		child["dueComplete"] != true {
		t.Fatalf("Jira dates and completion: %v", child)
	}

	conflict := strings.TrimSuffix(payload, "]}") + `,{"key":"TEST-2","fields":{"summary":"Conflict","status":{"name":"Doing","statusCategory":{"key":"done"}}}}]}`
	rejected, err := c.PostForm(w.srv.URL+"/import/jira", url.Values{"text": {conflict}})
	if err != nil {
		t.Fatal(err)
	}
	defer rejected.Body.Close()
	if rejected.StatusCode != http.StatusBadRequest {
		t.Fatalf("conflicting Jira categories: %d", rejected.StatusCode)
	}

}

func TestJiraImportEpicAndInvalidParent(t *testing.T) {
	input, err := decodeJS([]byte(`{"names":{"customfield_10014":"Epic Link"},"issues":[
		{"key":"EPIC-1","fields":{"summary":"Epic"}},
		{"key":"TASK-1","fields":{"summary":"Child","customfield_10014":"EPIC-1"}},
		{"key":"TASK-2","fields":{"summary":"Missing parent","parent":{"key":"MISSING"},"customfield_10014":"EPIC-1"}},
		{"key":"TASK-3","fields":{"summary":"Self","parent":{"key":"TASK-3"}}}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := parseJira(input)
	if err != nil {
		t.Fatal(err)
	}
	links := planImportedLinks(importTasks(doc))
	if len(links.Parents) != 1 || links.Parents[0].Index != 1 || links.Parents[0].Parent != 0 || len(links.Unsupported) != 1 {
		t.Fatalf("Jira hierarchy: %+v", links)
	}
	losses, _ := jsGet(doc, "unsupported").([]any)
	if len(losses) != 1 || !strings.Contains(strOf(jsGet(losses[0], "reason")), "MISSING") {
		t.Fatalf("Jira missing parent: %v", losses)
	}
}

func TestJiraImportRankOrder(t *testing.T) {
	input, err := decodeJS([]byte(`{"names":{"customfield_10021":"Rank"},"schema":{
		"customfield_10020":{"custom":"com.pyxis.greenhopper.jira:gh-lexo-rank"}},"issues":[
		{"fields":{"customfield_10020":"0|b:","customfield_10021":"0|a:"}},
		{"fields":{"customfield_10020":"0|a:","customfield_10021":"0|b:"}},
		{"fields":{"customfield_10020":"0|a:"}},
		{"fields":{"customfield_10021":"0|a:"}}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := parseJira(input)
	if err != nil {
		t.Fatal(err)
	}
	tasks := importTasks(doc)
	for i, expected := range []any{3, 1, 2, nil} {
		if actual := jsGet(tasks[i], "jira_backlog_rank"); actual != expected {
			t.Errorf("issue %d: rank %v, want %v", i, actual, expected)
		}
	}
}

func TestJiraImportLegacySprint(t *testing.T) {
	input, err := decodeJS([]byte(`{"schema":{"customfield_10022":{"custom":"com.pyxis.greenhopper.jira:gh-sprint"}},
		"issues":[{"fields":{"customfield_10022":[
			"Sprint@1[id=8,state=ACTIVE,name=Active, sprint,startDate=2026-10-10]",
			{"id":9,"name":"Closed","state":"closed"}]}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := parseJira(input)
	if err != nil {
		t.Fatal(err)
	}
	sprints, _ := jsGet(doc, "jira_sprints").([]any)
	losses, _ := jsGet(doc, "unsupported").([]any)
	if len(sprints) != 1 || jsGet(sprints[0], "name") != "Active, sprint" || len(losses) != 2 ||
		jsGet(importTasks(doc)[0], "jira_sprint") != "8" {
		t.Fatalf("Jira sprint import: %v", doc)
	}
}

func TestJiraImportEstimateDiscovery(t *testing.T) {
	input, err := decodeJS([]byte(`{"schema":{"customfield_10016":{"type":"number","custom":"com.pyxis.greenhopper.jira:jsw-story-points"}},"issues":[{"fields":{"customfield_10016":null}},{"fields":{}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := parseJira(input)
	if err != nil {
		t.Fatal(err)
	}
	if mapping := jsGet(doc, "jira_estimate_mapping"); jsGet(mapping, "key") != "customfield_10016" ||
		jsGet(mapping, "unit") != "points" {
		t.Fatalf("discovered estimate: %v", mapping)
	}
	tasks := importTasks(doc)
	if value, present := jsLookup(tasks[0], "jira_estimate"); !present || value != nil {
		t.Fatalf("explicit null estimate: %v", tasks[0])
	}
	if _, present := jsLookup(tasks[1], "jira_estimate"); present {
		t.Fatalf("absent estimate: %v", tasks[1])
	}
}

func TestJiraImportAttachmentReport(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	c := w.client(t)
	body(t, w.signIn(t, c, "ann", "pw"))
	res, err := c.PostForm(w.srv.URL+"/import/jira", url.Values{"text": {
		`{"issues":[{"key":"J-1","fields":{"summary":"File","attachment":[{"filename":"spec.pdf","content":"https://example.test/file"}]}}]}`,
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	location := res.Header.Get("Location")
	if res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(location, "/import/jira/report?board=") {
		t.Fatalf("Jira attachment report: %d %s", res.StatusCode, location)
	}
	report := getBody(t, c, w.srv.URL+location)
	if !strings.Contains(report, "spec.pdf") || !strings.Contains(report, "give the Jira address") {
		t.Fatalf("missing Jira attachment explanation: %s", report)
	}
}
