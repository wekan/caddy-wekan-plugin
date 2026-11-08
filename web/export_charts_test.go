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
	if strings.Count(s, "<row ") != 5 || !strings.Contains(s, ">Ideal<") || !strings.Contains(s, ">"+dayOf(created)+"<") {
		t.Fatalf("burndown rows: %s", s)
	}
	for chart, want := range map[string]string{"burnup": ">Total<", "leadTime": ">C<", "cycleTime": ">C<", "controlChart": ">StdDev<",
		"throughputHistogram": ">1<", "gantt": ">C<", "pulse": ">" + dayOf(time.Now()) + "<", "dashboard": ">BOB<", "scrumVelocity": "<row r=\"1\">"} {
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
	if code, _, _ = get(base+"cumulativeFlow/exportPDF", a); code != 501 {
		t.Errorf("a chart not in Weddy yet says so: %d", code)
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
