package web

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// The same board WeKan's own formatters were given (testdata/external/
// collected.json; generate.mjs writes their output): Weddy's, byte for byte.
func TestExternalFormatsAreWeKans(t *testing.T) {
	raw, err := os.ReadFile("testdata/external/collected.json")
	if err != nil {
		t.Fatal(err)
	}
	var d extBoard
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	formats := map[string]string{"markdown": formatMarkdownKanban(d), "opml": formatOpml(d), "todotxt": formatTodoTxt(d),
		"taskwarrior": formatTaskwarrior(d), "zenkit": string(formatZenkit(d)), "nullboard": formatNullboard(d, time.Now()),
		"obsidian": formatObsidianKanban(d), "orgmode": formatOrgMode(d), "kanri": string(formatKanri(d, time.Now()))}
	formats["leo"], formats["meistertask"], formats["todoist"] = formatLeo(d), formatMeisterTaskCsv(d), formatTodoistCsv(d)
	formats["linear"], formats["clickup"], formats["pivotal"], formats["quire"] = formatLinearCsv(d), formatClickUpCsv(d), formatPivotalCsv(d), formatQuireCsv(d)
	formats["ticktick"] = formatTickTickCsv(d, time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC))
	formats["redmine"], formats["notion"] = formatRedmineCsv(d), formatNotionCsv(d)
	var withRules struct {
		WorkflowRules []struct {
			Trigger, Action map[string]any
		} `json:"workflowRules"`
	}
	_ = json.Unmarshal(raw, &withRules)
	var rules []bson.M
	for _, r := range withRules.WorkflowRules {
		rules = append(rules, bson.M{"trigger": bson.M(r.Trigger), "action": bson.M(r.Action)})
	}
	formats["wrikeworkflow"] = string(jsJSON(formatWrikeWorkflow(d, rules), ""))
	formats["wrike"] = string(jsJSON(formatWrikeRows(d, rules), ""))
	formats["teamwork"] = string(jsJSON(formatTeamworkSheet(d), ""))
	formats["monday"] = string(jsJSON(formatMondaySheets(d), ""))
	formats["businessmap"] = string(jsJSON(formatBusinessmapSheets(d), ""))
	formats["vikunja"] = formatVikunja(d, time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC))
	formats["planner"] = string(jsJSON(formatPlannerRows(d, time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)), ""))
	formats["superproductivity"] = formatSuperProductivity(d, time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC).UnixMilli())
	formats["tasksorg"] = string(jsJSON(formatTasksOrgBackup(d, time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)), ""))
	formats["focalboard"] = formatFocalboard(d, time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC).UnixMilli())
	for f, format := range apiFormatters {
		formats[f] = string(jsJSON(format(d), ""))
	}
	// The Jira export reads what collect() gives it alone: the time spent as
	// its time tracking (testdata/external/collected-jira.json).
	jiraRaw, err := os.ReadFile("testdata/external/collected-jira.json")
	if err != nil {
		t.Fatal(err)
	}
	var jd extBoard
	if err := json.Unmarshal(jiraRaw, &jd); err != nil {
		t.Fatal(err)
	}
	formats["jira"] = string(jsJSON(formatJira(jd), ""))
	formats["taiga"] = string(jsJSON(formatTaiga(d, time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)), ""))
	for format, got := range formats {
		want, err := os.ReadFile("testdata/external/" + format + ".golden")
		if err != nil {
			t.Fatal(err)
		}
		if got != string(want) {
			t.Errorf("%s:\n%s\nWeKan's:\n%s", format, got, want)
		}
	}
	// Every format Weddy offers is one of these.
	for f := range externalFormats {
		if _, err := os.Stat("testdata/external/" + f + ".golden"); err != nil {
			t.Errorf("negative: %s has no WeKan output to be checked against", f)
		}
	}
}

// taskwarriorUuid of WeKan's, for ids beyond the fixture's.
func TestTaskwarriorUUID(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	for _, id := range []string{"", "x", "QWERTYqwerty123", "äöå", "𝄞𝄞"} {
		if u := taskwarriorUUID(id); !re.MatchString(u) || u != taskwarriorUUID(id) {
			t.Errorf("%q: %s is not a stable RFC 4122 v4 uuid", id, u)
		}
	}
	if taskwarriorUUID("a") == taskwarriorUUID("b") {
		t.Error("negative: two cards, two uuids")
	}
}

func TestExportFormatRoutes(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	a, bt := login(t, w, "ann"), login(t, w, "bob")
	b := newRESTBoard(t, w, a, "Formats")
	get := func(path, token string) (int, string, string) {
		req, _ := http.NewRequest("GET", w.srv.URL+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := testClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, _ := io.ReadAll(res.Body)
		return res.StatusCode, res.Header.Get("Content-Type"), string(data)
	}
	if code, typ, body := get("/api/boards/"+b.ID+"/export/markdown", a); code != 200 || typ != "text/markdown; charset=utf-8" || !strings.Contains(body, "## L\n\n- [ ] C") {
		t.Errorf("markdown: %d %s %q", code, typ, body)
	}
	if code, _, body := get("/api/boards/"+b.ID+"/export/opml", a); code != 200 || !strings.Contains(body, `<outline text="C"/>`) {
		t.Errorf("opml: %d %q", code, body)
	}
	if code, _, body := get("/api/boards/"+b.ID+"/export/todotxt", a); code != 200 || !strings.Contains(body, "C list:L") {
		t.Errorf("todo.txt: %d %q", code, body)
	}
	// Every format Weddy offers answers as its file, and refuses a stranger.
	for f, ext := range externalFormats {
		code, typ, body := get("/api/boards/"+b.ID+"/export/"+f, a)
		// A .zip is compressed: its card is in its data.json, not in its
		// bytes - which held a "C" only now and then.
		text := body
		if ext[0] == "zip" && code == 200 {
			text = string(mustUnzip(t, []byte(body), "data.json"))
		}
		if code != 200 || typ != exportContentType(ext) || (ext[0] != "xlsx" && !strings.Contains(text, "C")) {
			t.Errorf("%s: %d %s %q", f, code, typ, body)
		}
		if code, _, _ := get("/api/boards/"+b.ID+"/export/"+f, bt); code != 403 {
			t.Errorf("negative: %s to a non-member: %d", f, code)
		}
	}
	if code, _, _ := get("/api/boards/"+b.ID+"/export/markdown", ""); code != 401 {
		t.Errorf("negative: no token: %d", code)
	}
	if code, _, _ := get("/api/boards/"+b.ID+"/export/markdown", bt); code != 403 {
		t.Errorf("negative: a non-member: %d", code)
	}
	setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: true, IsNormalAssignedOnly: true})
	if code, _, _ := get("/api/boards/"+b.ID+"/export/opml", bt); code != 403 {
		t.Errorf("negative: assigned-only: %d", code)
	}
	_, _ = w.db.Collection("settings").UpdateOne(context.Background(), bson.M{}, bson.M{"$set": bson.M{"disableAllExport": true}})
	if code, _, _ := get("/api/boards/"+b.ID+"/export/todotxt", a); code != 403 {
		t.Errorf("negative: export off: %d", code)
	}
}

// The JSON formats through the route: the card's people by username, its
// comments with their authors, and Admin Panel's Anonymize export users over
// all of it.
func TestExportFormatsCarryPeopleAndComments(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	ann := w.seedUser(t, "ann", "pw")
	a := login(t, w, "ann")
	b := newRESTBoard(t, w, a, "People")
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": b.Card}, bson.M{"$set": bson.M{"members": bson.A{ann.ID}}})
	call(t, "POST", w.srv.URL+"/api/boards/"+b.ID+"/cards/"+b.Card+"/comments", a, `{"comment":"Ready, @ann"}`, nil)
	fetch := func(format string) string {
		req, _ := http.NewRequest("GET", w.srv.URL+"/api/boards/"+b.ID+"/export/"+format, nil)
		req.Header.Set("Authorization", "Bearer "+a)
		res, err := testClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, _ := io.ReadAll(res.Body)
		if res.StatusCode != 200 {
			t.Fatalf("%s: %d", format, res.StatusCode)
		}
		return string(data)
	}
	var zk zkBoard
	if err := json.Unmarshal([]byte(fetch("zenkit")), &zk); err != nil || len(zk.Items) != 1 {
		t.Fatalf("zenkit is one JSON document: %v", err)
	}
	if it := zk.Items[0]; len(it.Assignees) != 1 || it.Assignees[0] != "ann" || len(it.Comments) != 1 || it.Comments[0].Author != "ann" || it.ID != b.Card {
		t.Errorf("zenkit carries the people and the comments: %+v", it)
	}
	var tasks []twTask
	if err := json.Unmarshal([]byte(fetch("taskwarrior")), &tasks); err != nil || len(tasks) != 1 || tasks[0].UUID != taskwarriorUUID(b.Card) ||
		len(tasks[0].Annotations) != 1 || tasks[0].Annotations[0].Description != "ann: Ready, @ann" {
		t.Errorf("taskwarrior carries the comment as an annotation: %v %+v", err, tasks)
	}
	// Admin Panel -> Anonymize export users.
	_, _ = w.db.Collection("settings").UpdateOne(ctx, bson.M{}, bson.M{"$set": bson.M{"anonymizeExportUsers": true}}, options.UpdateOne().SetUpsert(true))
	for _, format := range []string{"zenkit", "taskwarrior"} {
		if body := fetch(format); strings.Contains(body, `"ann"`) || strings.Contains(body, "@ann") || strings.Contains(body, "ann:") {
			t.Errorf("negative: %s names nobody once Admin Panel says so: %s", format, body)
		}
	}
}

// The workbooks read back as the rows WeKan's formatters write: text as
// text, the Keys as numbers, the dates as date cells, the header bold.
func TestExportWorkbooks(t *testing.T) {
	raw, _ := os.ReadFile("testdata/external/collected.json")
	var d extBoard
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		rows  workbookRows
		write func(workbookRows) ([]byte, error)
		sheet string
	}{
		"wrike":    {formatWrikeRows(d, nil), wrikeWorkbook, "Tasks"},
		"teamwork": {formatTeamworkSheet(d), teamworkWorkbook, xlsxSheetName(d.Board.Title, "Tasks")},
		"planner":  {formatPlannerRows(d, time.Now()), plannerWorkbook, "Tasks"},
	} {
		data, err := c.write(c.rows)
		if err != nil {
			t.Fatal(err)
		}
		sheets, err := readXlsx(data)
		if err != nil || len(sheets) != 1 || sheets[0].Name != c.sheet {
			t.Fatalf("%s: one sheet named %q: %v %+v", name, c.sheet, err, sheets)
		}
		got := sheets[0].Rows
		if len(got) != len(c.rows.Rows) {
			t.Fatalf("%s: %d rows, not %d", name, len(got), len(c.rows.Rows))
		}
		for r, row := range c.rows.Rows {
			for k, cell := range row {
				want := cellText(cell)
				if dt, ok := cell.(jsDate); ok {
					want = time.Time(dt).Format("2006-01-02")
				}
				have := ""
				if k < len(got[r]) {
					have = got[r][k]
				}
				if !strings.HasPrefix(have, want) {
					t.Errorf("%s row %d col %d: %q, not %q", name, r+1, k+1, have, want)
				}
			}
		}
		// The header is bold: row 1, or Planner's "Task ID" row below its plan details.
		headerRow := 1
		for r, row := range c.rows.Rows {
			if len(row) > 1 && (row[0] == "Task ID" || row[1] == "Task Name") {
				headerRow = r + 1
			}
		}
		if !strings.Contains(string(mustUnzip(t, data, "xl/worksheets/sheet1.xml")), `<c r="A`+strconv.Itoa(headerRow)+`" s="1" t="inlineStr">`) {
			t.Errorf("%s: the header, row %d, is bold", name, headerRow)
		}
	}
	wb, _ := wrikeWorkbook(formatWrikeRows(d, nil))
	sheet := string(mustUnzip(t, wb, "xl/worksheets/sheet1.xml"))
	if !strings.Contains(sheet, `<c r="A2"><v>1</v></c>`) {
		t.Error("wrike: a Key is a number")
	}
	if !strings.Contains(sheet, ` s="2"><v>`) {
		t.Error("wrike: a date is a date cell")
	}
}

func mustUnzip(t *testing.T, data []byte, name string) []byte {
	t.Helper()
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range z.File {
		if f.Name == name {
			rc, _ := f.Open()
			defer rc.Close()
			b, _ := io.ReadAll(rc)
			return b
		}
	}
	t.Fatalf("no %s", name)
	return nil
}

// Vikunja's export zip: data.json, filters.json and VERSION without a newline.
func TestVikunjaArchive(t *testing.T) {
	data := vikunjaArchive(`[{"id":1}]`)
	for name, want := range map[string]string{"data.json": `[{"id":1}]`, "filters.json": "[]", "VERSION": "v1.0.0"} {
		if got := string(mustUnzip(t, data, name)); got != want {
			t.Errorf("%s: %q, not %q", name, got, want)
		}
	}
}

// The board's Export pop-over offers every other-tool format Weddy writes,
// and nothing it does not.
func TestExportMenuOffersEveryFormat(t *testing.T) {
	offered := map[string]bool{}
	for _, x := range exportTools {
		if _, ok := externalFormats[x.Key]; !ok {
			t.Errorf("negative: the menu offers %s, which Weddy does not write", x.Key)
		}
		offered[x.Key] = true
	}
	for f := range externalFormats {
		if !offered[f] {
			t.Errorf("%s is written but not in the Export pop-over", f)
		}
	}
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	b := newRESTBoard(t, w, login(t, w, "ann"), "Menu")
	var bd Board
	_ = w.db.Collection("boards").FindOne(context.Background(), bson.M{"_id": b.ID}).Decode(&bd)
	c := w.client(t)
	body(t, w.signIn(t, c, "ann", "pw"))
	page := getBody(t, c, w.srv.URL+"/b/"+b.ID+"/"+bd.Slug)
	for f := range externalFormats {
		// With WeKan's link's fields query (exportScope.js exportUrl), its
		// commas escaped as html/template writes a query.
		if !strings.Contains(page, `href="/api/boards/`+b.ID+`/export/`+f+`?fields=`+strings.Join(exportFieldKeys, "%2c")+`"`) {
			t.Errorf("the page links %s with every section selected", f)
		}
	}
	// WeKan's "what to include": card details and a checkbox per section,
	// each on, labelled - and none that ?fields= would not take.
	if !strings.Contains(page, `js-export-card-details-toggle`) {
		t.Error("the pop-over has the card details checkbox")
	}
	for _, k := range exportFieldKeys[1:] {
		if !strings.Contains(page, `data-field="`+k+`" role="checkbox" aria-checked="true"`) {
			t.Errorf("the pop-over has the %s checkbox, on", k)
		}
	}
	// Three pop-overs carry the selection: the board's Export, and a
	// swimlane's, list's or card's Export and Import.
	if n := strings.Count(page, "js-export-field-toggle"); n != 3*(len(exportFieldKeys)-1) {
		t.Errorf("negative: %d section checkboxes, not %d", n, 3*(len(exportFieldKeys)-1))
	}
	for _, row := range exportFieldRows("en") {
		if row.Label == "" || !slices.Contains(exportFieldKeys, row.Field) {
			t.Errorf("negative: %+v is a section ?fields= takes, with a label", row)
		}
	}
}

// The Export pop-over's selection, as WeKan's collect() reads ?fields=: a
// section left out is not in the export, and a query naming no section WeKan
// knows is everything (parseExportFields).
func TestExportFieldsSelection(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	ann := w.seedUser(t, "ann", "pw")
	a := login(t, w, "ann")
	b := newRESTBoard(t, w, a, "Fields")
	due := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": b.Card}, bson.M{"$set": bson.M{"members": bson.A{ann.ID},
		"description": "Said once", "dueAt": due, "startAt": due, "parentId": "parentCard1"}})
	call(t, "POST", w.srv.URL+"/api/boards/"+b.ID+"/cards/"+b.Card+"/comments", a, `{"comment":"A remark"}`, nil)
	zenkit := func(fields string) zkItem {
		req, _ := http.NewRequest("GET", w.srv.URL+"/api/boards/"+b.ID+"/export/zenkit?fields="+fields, nil)
		req.Header.Set("Authorization", "Bearer "+a)
		res, err := testClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var zk zkBoard
		if err := json.NewDecoder(res.Body).Decode(&zk); err != nil || len(zk.Items) != 1 {
			t.Fatalf("fields=%s: one item: %v", fields, err)
		}
		return zk.Items[0]
	}
	all := "card-details,board-header,activities,labels,people,board-info,dates,description,custom-fields,checklists,subtasks,comments,attachments,voting,poker,stickers,locations,dependencies,sort,scrum"
	for _, q := range []string{all, "", "nothing-wekan-knows"} {
		if it := zenkit(q); it.Description != "Said once" || it.Due == "" || it.Start == "" || it.ParentID != "parentCard1" ||
			len(it.Assignees) != 1 || len(it.Comments) != 1 {
			t.Errorf("fields=%q is everything: %+v", q, it)
		}
	}
	if it := zenkit("labels"); it.Description != "" || it.Due != "" || it.Start != "" || it.CreatedAt != "" || it.ParentID != "" ||
		len(it.Assignees) != 0 || len(it.Comments) != 0 || it.Title == "" {
		t.Errorf("negative: fields=labels leaves out the description, dates, subtask parent, people and comments: %+v", it)
	}
	if it := zenkit("comments,people"); len(it.Comments) != 1 || it.Comments[0].Author != "ann" || len(it.Assignees) != 1 || it.Description != "" {
		t.Errorf("fields=comments,people: just those: %+v", it)
	}
}
