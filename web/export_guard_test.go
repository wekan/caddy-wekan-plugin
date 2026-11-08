package web

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// CrossBoardExportBleed: with export off in Admin Panel, every export route
// answers 403 - whoever asks, a board admin of a public board included - and
// the attempt is in Admin Panel -> Problems without costing the account.
func TestExportDisabledRefusesEveryExport(t *testing.T) {
	w := newTestWeKan(t)
	ann := w.seedUser(t, "ann", "pw")
	a := login(t, w, "ann")
	b := newRESTBoard(t, w, a, "Off")
	ctx := context.Background()
	routes := []string{"/api/boards/" + b.ID + "/export", "/api/boards/" + b.ID + "/exportZip", "/api/boards/" + b.ID + "/export/csv",
		"/api/boards/" + b.ID + "/calendar.ics", "/api/boards/" + b.ID + "/exportExcel", "/api/boards/" + b.ID + "/exportPDF",
		"/api/boards/" + b.ID + "/lists/" + b.List + "/cards/" + b.Card + "/exportPDF",
		"/api/boards/" + b.ID + "/lists/" + b.List + "/cards/" + b.Card + "/exportExcel",
		"/api/export-all-boards/wekan", "/api/boards/" + b.ID + "/attachments/noattachment0001/export"}
	get := func(url string) int {
		req, _ := http.NewRequest("GET", w.srv.URL+url, nil)
		req.Header.Set("Authorization", "Bearer "+a)
		res, err := testClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if code := get(routes[0]); code != 200 {
		t.Fatalf("export is on by default: %d", code)
	}
	_, _ = w.db.Collection("settings").UpdateOne(ctx, bson.M{}, bson.M{"$set": bson.M{"disableAllExport": true}})
	var set bson.M
	if w.db.Collection("settings").FindOne(ctx, bson.M{}).Decode(&set) != nil || set["disableAllExport"] != true {
		t.Fatal("the switch is set")
	}
	for _, r := range routes {
		if code := get(r); code != 403 {
			t.Errorf("negative: %s with export off: %d", r, code)
		}
	}
	var row bson.M
	if err := w.db.Collection("eventlog").FindOne(ctx, bson.M{"bleed": "CrossBoardExportBleed"}).Decode(&row); err != nil {
		t.Fatalf("the refusal is in Problems: %v", err)
	}
	if n, _ := row["count"].(int32); int(n) < len(routes) {
		t.Errorf("one row, counting every attempt: %v", row["count"])
	}
	var u User
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"_id": ann.ID}).Decode(&u)
	if u.LoginDisabled {
		t.Error("negative: an export while it is off never disables the account")
	}
	_, _ = w.db.Collection("settings").UpdateOne(ctx, bson.M{}, bson.M{"$set": bson.M{"disableAllExport": false}})
	if code := get(routes[0]); code != 200 {
		t.Fatalf("on again: %d", code)
	}
}

// The shape, anywhere in the package: an API route whose path exports - a
// path segment starting "export", or a calendar feed - registered without
// the switch. The rules page's export is not one: WeKan's is in the browser
// and not behind the switch either.
func TestEveryExportRouteIsExportable(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	route := regexp.MustCompile(`HandleFunc\("GET ([^"]*)"(?:\+base\+"([^"]*)")?, (.+)\)$`)
	exporting := regexp.MustCompile(`/export|\.ics`)
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, _ := os.ReadFile(f)
		for _, line := range strings.Split(string(src), "\n") {
			m := route.FindStringSubmatch(strings.TrimSpace(line))
			if m == nil || !exporting.MatchString(m[1]+m[2]) || strings.HasPrefix(m[1], "/b/") {
				continue
			}
			checked++
			if !strings.HasPrefix(m[3], "s.exportable(") {
				t.Errorf("%s: %s is not behind the Admin Panel export switch", f, m[1]+m[2])
			}
		}
	}
	if checked < 10 {
		t.Fatalf("the guard sees the export routes: %d", checked)
	}
}
