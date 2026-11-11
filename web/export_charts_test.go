package web

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestEachDayAndDone(t *testing.T) {
	from := time.Date(2026, 1, 30, 23, 0, 0, 0, time.UTC)
	if got := eachDay(from, from.Add(50*time.Hour)); strings.Join(got, ",") != "2026-01-30,2026-01-31,2026-02-01,2026-02-02" {
		t.Fatalf("%v", got)
	}
	at := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	if d, ok := (chartCard{ArchivedAt: at}).done(); !ok || !d.Equal(at) {
		t.Error("an archived card is done when it was archived")
	}
	if _, ok := (chartCard{}).done(); ok {
		t.Error("negative: an open card is not done")
	}
}

func TestExportCharts(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	a, bt := login(t, w, "ann"), login(t, w, "bob")
	b := newRESTBoard(t, w, a, "Charts")
	ctx := context.Background()
	created := time.Now().UTC().AddDate(0, 0, -3)
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": b.Card}, bson.M{"$set": bson.M{
		"createdAt": created, "endAt": created.Add(48 * time.Hour), "assignees": bson.A{bob.ID}}})
	get := func(url, token string) (int, string, []byte) {
		req, _ := http.NewRequest("GET", w.srv.URL+url, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := testClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, _ := io.ReadAll(res.Body)
		return res.StatusCode, res.Header.Get("Content-Type"), data
	}
	sheet := func(data []byte) string {
		z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range z.File {
			if f.Name == "xl/worksheets/sheet1.xml" {
				r, _ := f.Open()
				x, _ := io.ReadAll(r)
				return string(x)
			}
		}
		return ""
	}
	base := "/api/boards/" + b.ID + "/charts/"
	code, typ, data := get(base+"burndown/exportExcel", a)
	if code != 200 || typ != xlsxType {
		t.Fatalf("burndown: %d %s", code, typ)
	}
	// Four days: one card remaining for two, then done; ideal 1 -> 0.
	s := sheet(data)
	// ExporterChartExcel's layout (deliberately WeKan's since 2026-11-11):
	// "<board> - <chart>" in row 1, the headers in row 2, a row a day from 3.
	if strings.Count(s, "<row ") != 6 || !strings.Contains(s, ">Charts - Burndown<") || !strings.Contains(s, `<mergeCell ref="A1:C1"/>`) ||
		!strings.Contains(s, ">Ideal<") || !strings.Contains(s, ">"+dayOf(created)+"<") {
		t.Fatalf("burndown rows: %s", s)
	}
	for chart, want := range map[string]string{"burnup": ">Total<", "leadTime": ">C<", "cycleTime": ">C<", "controlChart": ">StdDev<",
		"throughputHistogram": ">1<", "gantt": ">C<", "pulse": ">" + dayOf(time.Now()) + "<", "dashboard": ">BOB<", "scrumVelocity": "<row r=\"2\">"} {
		code, _, data := get(base+chart+"/exportExcel", a)
		if code != 200 || !strings.Contains(sheet(data), want) {
			t.Errorf("%s: %d, wants %s: %s", chart, code, want, sheet(data))
		}
	}
	if code, typ, data = get(base+"dashboard/exportPDF", a); code != 200 || typ != "application/pdf" || !bytes.HasPrefix(data, []byte("%PDF")) {
		t.Fatalf("pdf: %d %s", code, typ)
	}
	if code, _, _ = get(base+"nosuch/exportPDF", a); code != 400 {
		t.Errorf("negative: an unknown chart: %d", code)
	}
	// Every chart of WeKan's is in Weddy: none answers 501.
	for chart := range chartTitles {
		if chart == "scrumDaily" || chart == "scrumSprint" {
			continue
		}
		if code, _, _ = get(base+chart+"/exportPDF", a); code != 200 {
			t.Errorf("%s: %d", chart, code)
		}
	}
	if code, _, _ = get(base+"burndown/exportPDF", ""); code != 401 {
		t.Errorf("negative: no token: %d", code)
	}
	if code, _, _ = get(base+"burndown/exportPDF", bt); code != 403 {
		t.Errorf("negative: a non-member: %d", code)
	}
	// export-access.e2e.js: an assigned-only member gets no board-wide chart,
	// only the Scrum ones, which are their own.
	setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: true, IsNormalAssignedOnly: true})
	for _, chart := range []string{"burndown", "dashboard", "gantt"} {
		if code, _, _ = get(base+chart+"/exportExcel", bt); code != 403 {
			t.Errorf("negative: assigned-only %s: %d", chart, code)
		}
	}
	if code, _, _ = get(base+"scrumVelocity/exportExcel", bt); code != 200 {
		t.Errorf("assigned-only Scrum velocity: %d", code)
	}
	// The Admin Panel switches hold here too.
	_, _ = w.db.Collection("settings").UpdateOne(ctx, bson.M{}, bson.M{"$set": bson.M{"anonymizeExportUsers": true}})
	if _, _, data = get(base+"dashboard/exportExcel", a); strings.Contains(sheet(data), "BOB") {
		t.Error("negative: an anonymized dashboard names nobody")
	}
	_, _ = w.db.Collection("settings").UpdateOne(ctx, bson.M{}, bson.M{"$set": bson.M{"disableAllExport": true}})
	if code, _, _ = get(base+"dashboard/exportExcel", a); code != 403 {
		t.Errorf("negative: export off: %d", code)
	}
}

// The CFD and the WIP Run replay the card activities: a card created in the
// first list, moved to the second, then archived.
func TestExportFlowCharts(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	a := login(t, w, "ann")
	b := newRESTBoard(t, w, a, "Flow")
	ctx := context.Background()
	var l2, l3 struct {
		ID string `json:"_id"`
	}
	call(t, "POST", w.srv.URL+"/api/boards/"+b.ID+"/lists", a, `{"title":"Doing"}`, &l2)
	call(t, "POST", w.srv.URL+"/api/boards/"+b.ID+"/lists", a, `{"title":"Done"}`, &l3)
	_, _ = w.db.Collection("lists").UpdateOne(ctx, bson.M{"_id": l2.ID}, bson.M{"$set": bson.M{"wipLimit": bson.M{"enabled": true, "value": 3}}})
	day := func(n int) time.Time { return time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -n).Add(time.Hour) }
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": b.Card}, bson.M{"$set": bson.M{"createdAt": day(3)}})
	_, _ = w.db.Collection("activities").DeleteMany(ctx, bson.M{"boardId": b.ID})
	for i, e := range []bson.M{{"activityType": "createCard", "listId": b.List, "createdAt": day(3)},
		{"activityType": "moveCard", "listId": l2.ID, "createdAt": day(2)}, {"activityType": "archivedCard", "createdAt": day(1)}} {
		e["_id"], e["boardId"], e["cardId"] = "flowactivity000"+string(rune('a'+i)), b.ID, b.Card // inserted, not a filter
		_, _ = w.db.Collection("activities").InsertOne(ctx, e)
	}
	rows := func(chart string) [][]string {
		req, _ := http.NewRequest("GET", w.srv.URL+"/api/boards/"+b.ID+"/charts/"+chart+"/exportExcel", nil)
		req.Header.Set("Authorization", "Bearer "+a)
		res, err := testClient.Do(req)
		if err != nil || res.StatusCode != 200 {
			t.Fatalf("%s: %v %v", chart, err, res)
		}
		data, _ := io.ReadAll(res.Body)
		res.Body.Close()
		sheets, err := readXlsx(data)
		if err != nil || len(sheets[0].Rows) == 0 || !strings.HasPrefix(sheets[0].Rows[0][0], "Flow - ") {
			t.Fatalf("%s: the title row: %v", chart, err)
		}
		// Row 1 is the title (WeKan's layout); the table starts at row 2.
		return sheets[0].Rows[1:]
	}
	cfd := rows("cumulativeFlow")
	if strings.Join(cfd[0], ",") != "Date,L,Doing,Done" || len(cfd) != 5 {
		t.Fatalf("cfd: %q", cfd)
	}
	// Created: in L only; moved: in L and Doing; archived: nowhere.
	for i, want := range []string{"1,0,0", "1,1,0", "0,0,0", "0,0,0"} {
		if got := strings.Join(cfd[i+1][1:], ","); got != want {
			t.Errorf("cfd day %d: %s, want %s", i, got, want)
		}
	}
	wip := rows("wipRun")
	if len(wip) != 5 || wip[1][1] != "0" || wip[2][1] != "1" || wip[3][1] != "0" || wip[2][2] != "3" {
		t.Errorf("wip run, limit 3 of the lists between: %q", wip)
	}
}

func TestExportFlowEfficiencyAndTime(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	a := login(t, w, "ann")
	b := newRESTBoard(t, w, a, "Time")
	ctx := context.Background()
	start := time.Now().UTC().Add(-10 * time.Hour)
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": b.Card}, bson.M{"$set": bson.M{"startAt": start, "endAt": start.Add(8 * time.Hour),
		"spentTime": 2.0, "isOvertime": true, "assignees": bson.A{bob.ID}}})
	_, _ = w.db.Collection("changeHistory").InsertOne(ctx, bson.M{"_id": "timehistory00001", "boardId": b.ID, "entityType": "card", // inserted, not a filter
		"entityId": b.Card, "userId": bob.ID, "previousContent": bson.M{"field": "spentTime", "value": 0.5}, "newContent": bson.M{"field": "spentTime", "value": 2.0}})
	rows := func(chart string) string {
		req, _ := http.NewRequest("GET", w.srv.URL+"/api/boards/"+b.ID+"/charts/"+chart+"/exportExcel", nil)
		req.Header.Set("Authorization", "Bearer "+a)
		res, err := testClient.Do(req)
		if err != nil || res.StatusCode != 200 {
			t.Fatalf("%s: %v %v", chart, err, res)
		}
		data, _ := io.ReadAll(res.Body)
		res.Body.Close()
		sheets, _ := readXlsx(data)
		out := []string{}
		for _, r := range sheets[0].Rows {
			out = append(out, strings.Join(r, "|"))
		}
		return strings.Join(out, "\n")
	}
	// 2 of 8 hours active: 25%.
	if got := rows("flowEfficiency"); !strings.Contains(got, "C|25|2|8") {
		t.Errorf("flow efficiency: %s", got)
	}
	got := rows("time")
	for _, want := range []string{"BOB|1.5", "BOB|2", "C (Overtime)|2", "Remaining time until due"} {
		if !strings.Contains(got, want) {
			t.Errorf("time has %q: %s", want, got)
		}
	}
	_, _ = w.db.Collection("settings").UpdateOne(ctx, bson.M{}, bson.M{"$set": bson.M{"anonymizeExportUsers": true}})
	if got := rows("time"); strings.Contains(got, "BOB") {
		t.Errorf("negative: anonymized: %s", got)
	}
}
