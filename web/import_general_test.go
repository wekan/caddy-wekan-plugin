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
	for n, want := range []struct{ fullname, username string }{{"ann", "ann-2"}, {"Bob", "Bob"}} {
		var author bson.M
		if err := w.db.Collection("users").FindOne(ctx, bson.M{"_id": docString(comments[n], "userId")}).Decode(&author); err != nil {
			t.Fatalf("comment %d's author: %v", n, err)
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
