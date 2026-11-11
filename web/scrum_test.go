package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// scrumCaller signs name in and calls methods as them.
func scrumCaller(t *testing.T, w *testWeKan, name string) func(method string, args ...any) map[string]any {
	c := w.client(t)
	body(t, w.signIn(t, c, name, "pw"))
	return func(method string, args ...any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(args)
		req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/"+url.PathEscape(method), strings.NewReader(string(raw)))
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
}

func resultOf(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	if out["error"] != nil {
		t.Fatalf("method failed: %v", out)
	}
	m, _ := out["result"].(map[string]any)
	return m
}

// Scrum's methods as WeKan's server/scrum.js: settings, sprints, releases,
// events and card metadata by those allowed, every write compare-and-set,
// a sprint started and closed with its snapshots stored as WeKan stores them
// and its unfinished cards rolled over, the board's data without the
// snapshots' rows, and one History row per call undone and redone.
func TestScrumMethods(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	cy := w.seedUser(t, "cy", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Scrum")
	setMember(t, w, rb.ID, Member{UserID: bob.ID, IsActive: true, IsReadOnly: true})
	setMember(t, w, rb.ID, Member{UserID: cy.ID, IsActive: true, IsNormalAssignedOnly: true})
	done := "card" + randomID()[:12]
	_, _ = w.db.Collection("cards").InsertOne(ctx, bson.M{"_id": done, "boardId": rb.ID, "listId": rb.List, "swimlaneId": rb.Swimlane,
		"title": "Done", "archived": false, "sort": 2, "dueComplete": true, "poker": bson.M{"estimation": 3}})
	ann, bobCall, cyCall := scrumCaller(t, w, "ann"), scrumCaller(t, w, "bob"), scrumCaller(t, w, "cy")

	// Settings: a board admin's, at the expected revision.
	if out := bobCall("scrum.configure", rb.ID, map[string]any{"enabled": true}); out["error"] != "not-authorized" {
		t.Errorf("negative: a read-only member configures: %v", out)
	}
	if out := ann("scrum.configure", rb.ID, map[string]any{"nope": 1}); out["error"] != "invalid-scrum" || !strings.Contains(out["reason"].(string), "Unsupported field: nope") {
		t.Errorf("negative: an unknown setting: %v", out)
	}
	if out := ann("scrum.configure", rb.ID, map[string]any{"productOwnerId": "nobodyAtAll12345"}); out["error"] != "invalid-scrum" {
		t.Errorf("negative: a product owner who is not a member: %v", out)
	}
	set := resultOf(t, ann("scrum.configure", rb.ID, map[string]any{"workingDays": []int{7}}, 0))
	if set["settingsRevision"] != 1.0 || set["settings"].(map[string]any)["estimateUnit"] != "points" {
		t.Fatalf("configured: %v", set)
	}
	if out := ann("scrum.configure", rb.ID, map[string]any{"workingDays": []int{1}}, 0); out["error"] != "scrum-conflict" {
		t.Errorf("negative: a stale settings revision: %v", out)
	}

	// Records.
	if out := bobCall("scrum.saveSprint", rb.ID, nil, map[string]any{"name": "Forbidden"}, nil); out["error"] != "not-authorized" {
		t.Errorf("negative: a read-only member's sprint: %v", out)
	}
	if out := ann("scrum.saveSprint", rb.ID, nil, map[string]any{"name": "S", "plannedStart": "2026-09-30", "plannedEnd": "2026-09-01"}, nil); out["error"] != "invalid-scrum" {
		t.Errorf("negative: a sprint that ends before it starts: %v", out)
	}
	sprint := resultOf(t, ann("scrum.saveSprint", rb.ID, nil, map[string]any{"name": " Sprint one ", "goal": "g", "plannedStart": "2026-09-01", "plannedEnd": "2026-09-30"}, nil))
	sprintID := sprint["_id"].(string)
	if sprint["name"] != "Sprint one" || sprint["state"] != "planned" || sprint["revision"] != 1.0 || sprint["incarnation"] == nil {
		t.Fatalf("a new sprint: %v", sprint)
	}
	if out := ann("scrum.saveSprint", rb.ID, sprintID, map[string]any{"goal": "x"}, nil); out["error"] != "scrum-conflict" {
		t.Errorf("negative: an edit with no revision: %v", out)
	}
	next := resultOf(t, ann("scrum.saveSprint", rb.ID, nil, map[string]any{"name": "Sprint two"}, nil))
	release := resultOf(t, ann("scrum.saveRelease", rb.ID, nil, map[string]any{"name": "R1"}, nil))
	if out := ann("scrum.saveEvent", rb.ID, nil, map[string]any{"name": "Review"}, nil); out["error"] != "invalid-scrum" {
		t.Errorf("negative: an event with no sprint, kind or date: %v", out)
	}
	event := resultOf(t, ann("scrum.saveEvent", rb.ID, nil, map[string]any{"name": "Review", "kind": "review", "sprintId": sprintID,
		"startsAt": "2026-09-28T09:30:12.123Z", "followUpCardIds": []string{rb.Card}}, nil))
	var stored bson.M
	_ = w.db.Collection("scrumEvents").FindOne(ctx, bson.M{"_id": event["_id"]}).Decode(&stored)
	if at, ok := stored["startsAt"].(bson.DateTime); !ok || at.Time().UTC().Format(time.RFC3339Nano) != "2026-09-28T09:30:12.123Z" {
		t.Errorf("the event's time is a stored date to the millisecond: %v", stored["startsAt"])
	}

	// Card metadata: whoever may change the card.
	if out := bobCall("scrum.updateCard", rb.ID, rb.Card, map[string]any{"sprintId": sprintID}); out["error"] != "not-authorized" {
		t.Errorf("negative: a read-only member's card metadata: %v", out)
	}
	if out := cyCall("scrum.updateCard", rb.ID, rb.Card, map[string]any{"sprintId": sprintID}); out["error"] != "not-authorized" {
		t.Errorf("negative: an assigned-only member on a card not assigned to them: %v", out)
	}
	if out := ann("scrum.updateCard", rb.ID, rb.Card, map[string]any{"pastSprintIds": []string{sprintID}}); out["error"] != "invalid-scrum" {
		t.Errorf("negative: past sprints written by a form: %v", out)
	}
	if out := ann("scrum.updateCard", rb.ID, rb.Card, map[string]any{"sprintId": "elsewhere1234567"}); out["error"] != "invalid-scrum" {
		t.Errorf("negative: another board's sprint: %v", out)
	}
	card := resultOf(t, ann("scrum.updateCard", rb.ID, rb.Card, map[string]any{"sprintId": sprintID, "backlogRank": 2, "releaseId": release["_id"], "acceptanceCriteria": "A\nB"}, nil))
	if card["scrumRevision"] != 1.0 || card["scrum"].(map[string]any)["releaseIds"].([]any)[0] != release["_id"] {
		t.Fatalf("card metadata, the releases in both forms: %v", card)
	}
	resultOf(t, ann("scrum.updateCard", rb.ID, done, map[string]any{"sprintId": sprintID}, nil))
	if out := ann("scrum.updateList", rb.ID, rb.List, map[string]any{"category": "later"}); out["error"] != "invalid-scrum" {
		t.Errorf("negative: a list category WeKan has not got: %v", out)
	}

	// The assigned-only member's board data is theirs alone.
	if data := resultOf(t, cyCall("scrum.getBoardData", rb.ID)); data["partial"] != true || len(data["cards"].([]any)) != 0 || data["canAdmin"] != false {
		t.Errorf("an assigned-only member sees only their cards: %v", data["cards"])
	}

	// The lifecycle.
	if out := ann("scrum.startSprint", rb.ID, next["_id"], 1); out["error"] != "invalid-scrum" {
		t.Errorf("negative: a sprint with no dates starts: %v", out)
	}
	started := resultOf(t, ann("scrum.startSprint", rb.ID, sprintID, 1))
	head := started["startSnapshot"].(map[string]any)
	if started["state"] != "active" || head["stored"] != "rows" || head["rowCount"] != 2.0 || head["cards"] != nil {
		t.Fatalf("started: %v", started)
	}
	if n, _ := w.db.Collection("scrumSnapshotRows").CountDocuments(ctx, bson.M{"sprintId": sprintID, "kind": "start"}); n != 1 {
		t.Errorf("the start's rows in one chunk: %d", n)
	}
	if out := ann("scrum.configure", rb.ID, map[string]any{"estimateUnit": "hours"}); out["error"] != "invalid-scrum" {
		t.Errorf("negative: the estimate unit changed during an active sprint: %v", out)
	}
	if out := ann("scrum.saveSprint", rb.ID, sprintID, map[string]any{"goal": "x"}, 2); out["error"] != "invalid-scrum" {
		t.Errorf("negative: an active sprint edited: %v", out)
	}
	closed := resultOf(t, ann("scrum.closeSprint", rb.ID, sprintID, 2, next["_id"]))
	if closed["state"] != "closed" || closed["rolloverPending"] != nil || closed["closedFromRevision"] != 2.0 {
		t.Fatalf("closed: %v", closed)
	}
	scrumOf := func(id string) map[string]any {
		var c bson.M
		_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": id}).Decode(&c)
		return plain(c["scrum"]).(map[string]any)
	}
	if sc := scrumOf(rb.Card); sc["sprintId"] != next["_id"] || !containsString(stringsOf(sc["pastSprintIds"]), sprintID) {
		t.Errorf("the unfinished card rolled over: %v", sc)
	}
	if sc := scrumOf(done); sc["sprintId"] != nil {
		t.Errorf("the finished card stays out of the next sprint: %v", sc)
	}
	if n, _ := w.db.Collection("scrumRolloverRows").CountDocuments(ctx, bson.M{"sprintId": sprintID}); n != 0 {
		t.Errorf("the rollover plan is gone once applied: %d", n)
	}
	// A retry of the same close is the close.
	if again := resultOf(t, ann("scrum.closeSprint", rb.ID, sprintID, 2, next["_id"])); again["revision"] != closed["revision"] {
		t.Errorf("a retried close: %v", again)
	}
	data := resultOf(t, ann("scrum.getBoardData", rb.ID))
	var report map[string]any
	for _, x := range data["sprints"].([]any) {
		if sp := x.(map[string]any); sp["_id"] == sprintID {
			report = sp["report"].(map[string]any)
			if sp["closeSnapshot"].(map[string]any)["cards"] != nil || sp["reportTotals"] != nil {
				t.Error("negative: the board data carries the snapshots' rows")
			}
		}
	}
	if report == nil || report["completed"].(map[string]any)["count"] != 1.0 || report["completed"].(map[string]any)["estimate"] != 3.0 ||
		report["committed"].(map[string]any)["count"] != 2.0 || report["plannedWorkingDays"] != 4.0 {
		t.Errorf("the closed sprint's report: %v", report)
	}

	// History: the event's edit undone, a newer edit never overwritten.
	ev := resultOf(t, ann("scrum.saveEvent", rb.ID, event["_id"], map[string]any{"notes": "Outcome"}, 1))
	if out := resultOf(t, ann("changeHistory.undoLast", rb.ID)); out["undone"] != true {
		t.Fatalf("undo: %v", out)
	}
	var e bson.M
	_ = w.db.Collection("scrumEvents").FindOne(ctx, bson.M{"_id": ev["_id"]}).Decode(&e)
	if e["notes"] != nil || e["revision"] != int32(3) {
		t.Errorf("the edit undone, the revision one up: %v", e)
	}
	if out := resultOf(t, ann("changeHistory.redoLast", rb.ID)); out["redone"] != true {
		t.Fatalf("redo: %v", out)
	}
	_ = w.db.Collection("scrumEvents").FindOne(ctx, bson.M{"_id": ev["_id"]}).Decode(&e)
	if e["notes"] != "Outcome" {
		t.Errorf("redone: %v", e)
	}
	_, _ = w.db.Collection("scrumEvents").UpdateOne(ctx, bson.M{"_id": ev["_id"]}, bson.M{"$set": bson.M{"notes": "Somebody else"}})
	if out := ann("changeHistory.undoLast", rb.ID); out["error"] != "scrum-conflict" {
		t.Errorf("negative: an undo over a newer edit: %v", out)
	}

	// The views: the Board View menu offers them, a board admin gets the
	// forms, a read-only member the sprint picker alone (negative), and the
	// Velocity view charts and tables the closed sprint.
	var bd Board
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": rb.ID}).Decode(&bd)
	annC, bobC := w.client(t), w.client(t)
	body(t, w.signIn(t, annC, "ann", "pw"))
	body(t, w.signIn(t, bobC, "bob", "pw"))
	base := w.srv.URL + "/b/" + rb.ID + "/" + bd.Slug
	if _, html := get(t, annC, base); !strings.Contains(html, "js-toggle-board-view") || !strings.Contains(html, "js-open-sprints-view") ||
		!strings.Contains(html, `href="/b/`+rb.ID+`/`+bd.Slug+`/scrum?view=velocity"`) {
		t.Error("the board offers the Scrum views")
	}
	_, html := get(t, annC, base+"/scrum?view=sprints&sprint="+next["_id"].(string))
	for _, want := range []string{`class="scrum-view`, "js-scrum-settings", "js-scrum-sprint-form", "js-scrum-release", "js-scrum-event", "js-scrum-start",
		`<option value="` + next["_id"].(string) + `" selected="selected">Sprint two (`} {
		if !strings.Contains(html, want) {
			t.Errorf("an admin's Sprints view has %s", want)
		}
	}
	_, html = get(t, bobC, base+"/scrum?view=sprints")
	if !strings.Contains(html, "js-scrum-sprint") || strings.Contains(html, "js-scrum-sprint-form") || strings.Contains(html, "js-scrum-settings") ||
		strings.Contains(html, "js-scrum-card") || strings.Contains(html, "js-scrum-release\"") {
		t.Error("negative: a read-only member's Sprints view has a form")
	}
	_, html = get(t, annC, base+"/scrum?view=velocity")
	if strings.Count(html, `<figure class="scrum-chart-row"`) != 1 || !strings.Contains(html, `<td class="scrum-working-days">4</td>`) ||
		!strings.Contains(html, `data-count="1" data-estimate="3" data-width-count="50" data-width-estimate="100"`) {
		t.Errorf("the Velocity view charts the closed sprint: %s", html[strings.Index(html, "scrum-view"):])
	}
	// The Velocity export: the closed sprint's row, as WeKan's chartExportRows.
	res, xlsx := get(t, annC, w.srv.URL+"/api/boards/"+rb.ID+"/charts/scrumVelocity/exportExcel")
	sheets, err := readXlsx([]byte(xlsx))
	if res.StatusCode != http.StatusOK || err != nil || len(sheets) != 1 {
		t.Fatalf("velocity export: %d %v", res.StatusCode, err)
	}
	// WeKan's layout: the title in row 1, the headers in row 2, the sprint in
	// row 3 - A3 its name, R3 its working days, S3 and U3 the estimate source
	// and the completion policy, V3-X3 the completed commitment.
	if rows := sheets[0].Rows; len(rows) != 3 || rows[0][0] != "Scrum - Velocity" || rows[2][0] != "Sprint one" || rows[2][17] != "4" ||
		rows[2][18] != "poker" || rows[2][20] != "dueComplete" || rows[2][21] != "1" || rows[2][22] != "3" || rows[2][23] != "0" {
		t.Errorf("velocity rows: %q", rows)
	}
	if typed, err := readXlsxValues([]byte(xlsx)); err != nil || len(typed[0].Rows) < 3 || len(typed[0].Rows[2]) < 18 || typed[0].Rows[2][17] != 4.0 {
		t.Error("the working days are a number cell, not text")
	}
	if res, _ := get(t, annC, w.srv.URL+"/api/boards/"+rb.ID+"/charts/scrumSprint/exportPDF?sprintId=nope"); res.StatusCode != http.StatusBadRequest {
		t.Errorf("negative: a sprint report of no sprint: %d", res.StatusCode)
	}
	var row bson.M
	_ = w.db.Collection("changeHistory").FindOne(ctx, bson.M{"boardId": rb.ID, "entityType": "scrum", "entityId": rb.Card}).Decode(&row)
	if row == nil || row["group"] != "scrum" || row["cardId"] != rb.Card {
		t.Errorf("a card's Scrum metadata is a scrum row of that card: %v", row)
	}
}

// Every write to Scrum's own collections is scrum.go's, scrum_history.go's or
// scrum_daily.go's,
// so each one is compare-and-set and in a History row (negative: the scan
// sees the shape it guards).
func TestScrumCollectionsWrittenOnlyByScrum(t *testing.T) {
	write := regexp.MustCompile(`Collection\("scrum(Sprints|Releases|Events|SnapshotRows|RolloverRows)"\)\.(InsertOne|InsertMany|UpdateOne|UpdateMany|ReplaceOne|DeleteOne|DeleteMany|FindOneAndUpdate)`)
	if !write.MatchString(`s.db.Collection("scrumSprints").UpdateOne(ctx`) || write.MatchString(`s.db.Collection("scrumSprints").FindOne(ctx`) {
		t.Fatal("the scan misses a write, or takes a read")
	}
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "scrum.go" || f == "scrum_history.go" || f == "scrum_daily.go" {
			continue
		}
		src, _ := os.ReadFile(f)
		if write.Match(src) {
			t.Errorf("%s writes a Scrum collection outside scrum.go", f)
		}
	}
}

// Daily observations, as WeKan's: taken on read once a UTC day and never
// replaced, the reader's cards only (negative: an assigned-only member sees
// no other card's estimate), the newest 366, refused while a Scrum import is
// unfinished; their export with WeKan's notices, through a download token
// that only the export routes take and that runs out (negatives).
func TestScrumDailyObservations(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	cy := w.seedUser(t, "cy", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Daily")
	setMember(t, w, rb.ID, Member{UserID: cy.ID, IsActive: true, IsReadAssignedOnly: true})
	other := "card" + randomID()[:12]
	_, _ = w.db.Collection("cards").InsertOne(ctx, bson.M{"_id": other, "boardId": rb.ID, "listId": rb.List, "swimlaneId": rb.Swimlane,
		"title": "Hidden", "archived": false, "sort": 2, "poker": bson.M{"estimation": 999}})
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": rb.Card}, bson.M{"$set": bson.M{"poker.estimation": 3, "assignees": bson.A{cy.ID}}})
	ann, cyCall := scrumCaller(t, w, "ann"), scrumCaller(t, w, "cy")
	sp := resultOf(t, ann("scrum.saveSprint", rb.ID, nil, map[string]any{"name": "D", "plannedStart": "2026-09-01", "plannedEnd": "2026-09-30"}, nil))
	id := sp["_id"].(string)
	for _, c := range []string{rb.Card, other} {
		resultOf(t, ann("scrum.updateCard", rb.ID, c, map[string]any{"sprintId": id}, 0))
	}
	resultOf(t, ann("scrum.startSprint", rb.ID, id, 1))
	_, _ = w.db.Collection("scrumSprints").UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"scrumImportPending": true}})
	if out := ann("scrum.getDailyHistory", rb.ID, id); out["error"] != "scrum-import-pending" {
		t.Errorf("negative: a daily history while the import is unfinished: %v", out)
	}
	if n, _ := w.db.Collection("scrumDailySnapshots").CountDocuments(ctx, bson.M{"boardId": rb.ID}); n != 0 {
		t.Errorf("negative: an observation taken while the import is unfinished: %d", n)
	}
	_, _ = w.db.Collection("scrumSprints").UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$unset": bson.M{"scrumImportPending": ""}})
	first := resultOf(t, ann("scrum.getDailyHistory", rb.ID, id))
	rows := first["rows"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["scope"].(map[string]any)["estimate"] != 1002.0 || rows[0].(map[string]any)["consistency"] != "observed" {
		t.Fatalf("today's observation: %v", first)
	}
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": rb.Card}, bson.M{"$set": bson.M{"poker.estimation": 8}})
	if again := resultOf(t, ann("scrum.getDailyHistory", rb.ID, id)); canonical(again["rows"]) != canonical(first["rows"]) {
		t.Errorf("an observation is never replaced: %v", again)
	}
	mine := resultOf(t, cyCall("scrum.getDailyHistory", rb.ID, id))
	if mine["partial"] != true || mine["rows"].([]any)[0].(map[string]any)["scope"].(map[string]any)["estimate"] != 3.0 {
		t.Errorf("an assigned-only member's observations: %v", mine)
	}
	if raw, _ := json.Marshal(mine); strings.Contains(string(raw), "999") {
		t.Error("negative: another card's estimate reaches an assigned-only member")
	}

	// The export, through a download token.
	tok := ann("exportDownloadToken")["result"].(string)
	plainC := testClient
	res, err := plainC.Get(w.srv.URL + "/api/boards/" + rb.ID + "/charts/scrumDaily/exportExcel?sprintId=" + id + "&authToken=" + tok)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("the daily export with a download token: %v %v", err, res)
	}
	data, _ := io.ReadAll(res.Body)
	res.Body.Close()
	sheets, _ := readXlsx(data)
	if len(sheets) != 2 || sheets[1].Name != "Notes" || len(sheets[0].Rows) != 3 || sheets[0].Rows[2][4] != "2" || !strings.Contains(sheets[1].Rows[0][0], "missing days are omitted") {
		t.Errorf("the daily export: %+v", sheets)
	}
	if res, _ := plainC.Get(w.srv.URL + "/api/boards/" + rb.ID + "/charts/scrumDaily/exportExcel?sprintId=foreign&authToken=" + tok); res.StatusCode == 200 {
		t.Error("negative: another sprint's daily export")
	}
	req, _ := http.NewRequest("GET", w.srv.URL+"/api/boards/"+rb.ID, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	if res, _ := plainC.Do(req); res.StatusCode == 200 {
		t.Error("negative: a download token opens the REST API")
	}
	_, _ = w.db.Collection("exportDownloadTokens").UpdateMany(ctx, bson.M{}, bson.M{"$set": bson.M{"expiresAt": time.Now().Add(-time.Minute)}})
	if res, _ := plainC.Get(w.srv.URL + "/api/boards/" + rb.ID + "/charts/scrumDaily/exportExcel?sprintId=" + id + "&authToken=" + tok); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("negative: an expired download token: %d", res.StatusCode)
	}

	// The newest 366 of 367.
	trunc := "trunc" + randomID()[:12]
	start := time.Now().UTC().Add(-367 * 24 * time.Hour).Truncate(time.Millisecond)
	_, _ = w.db.Collection("scrumSprints").InsertOne(ctx, bson.M{"_id": trunc, "boardId": rb.ID, "name": "W", "state": "closed",
		"startSnapshot": bson.M{"at": start, "estimateSource": "poker", "completionPolicy": "dueComplete", "unit": "points", "cards": bson.A{}}})
	for i := 0; i < 367; i++ {
		at := start.Add(time.Duration(i+1) * 24 * time.Hour)
		_, _ = w.db.Collection("scrumDailySnapshots").InsertOne(ctx, bson.M{"_id": trunc + strconv.Itoa(i), "boardId": rb.ID, "sprintId": trunc,
			"startedAt": start, "capturedAt": at, "day": at.Format("2006-01-02"), "snapshot": bson.M{"unit": "points", "cards": bson.A{}}})
	}
	win := resultOf(t, ann("scrum.getDailyHistory", rb.ID, trunc))
	if wr := win["rows"].([]any); win["truncated"] != true || len(wr) != 366 || wr[0].(map[string]any)["day"] != start.Add(48*time.Hour).Format("2006-01-02") {
		t.Errorf("the newest 366: %v %d", win["truncated"], len(wr))
	}
}
