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
	formats["superproductivity"] = formatSuperProductivity(d, time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC).UnixMilli())
	formats["tasksorg"] = string(jsJSON(formatTasksOrgBackup(d, time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)), ""))
	formats["focalboard"] = formatFocalboard(d, time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC).UnixMilli())
	for f, format := range apiFormatters {
		formats[f] = string(jsJSON(format(d), ""))
	}
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
		if code, typ, body := get("/api/boards/"+b.ID+"/export/"+f, a); code != 200 || typ != exportContentType(ext) || (ext[0] != "xlsx" && !strings.Contains(body, "C")) {
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
		if !strings.Contains(string(mustUnzip(t, data, "xl/worksheets/sheet1.xml")), `<c r="A1" s="1" t="inlineStr">`) {
			t.Errorf("%s: the header is bold", name)
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
