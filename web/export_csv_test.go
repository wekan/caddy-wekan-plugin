package web

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestCSVCell(t *testing.T) {
	for in, want := range map[any]string{nil: `""`, "a\"b": `"a""b"`, "=1+1": `"'=1+1"`, "-x": `"'-x"`, "@SUM": `"'@SUM"`,
		true: `"true"`, 1.5: `"1.5"`, float64(-5): `"-5"`, "plain": `"plain"`} {
		if got := csvCell(in); got != want {
			t.Errorf("csvCell(%#v) = %s, want %s", in, got, want)
		}
	}
}

func TestExportCSV(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	a, bt := login(t, w, "ann"), login(t, w, "bob")
	b := newRESTBoard(t, w, a, "CSV")
	ctx := context.Background()
	var f struct {
		ID string `json:"_id"`
	}
	call(t, "POST", w.srv.URL+"/api/boards/"+b.ID+"/custom-fields", a, `{"name":"Size","type":"dropdown","settings":{}}`, &f)
	call(t, "POST", w.srv.URL+"/api/boards/"+b.ID+"/custom-fields/"+f.ID+"/dropdown-items", a, `{"items":["S","M"]}`, nil)
	var fd bson.M
	_ = w.db.Collection("customFields").FindOne(ctx, bson.M{"_id": f.ID}).Decode(&fd)
	first := ""
	for id := range dropdownItems(fd) {
		if dropdownItems(fd)[id] == "M" {
			first = id
		}
	}
	var brd Board
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": b.ID}).Decode(&brd)
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": b.Card}, bson.M{"$set": bson.M{
		"description": `=HYPERLINK("http://x")`, "labelIds": bson.A{brd.Labels[0].ID}, "members": bson.A{bob.ID, "ghostuser000001"},
		"customFields": bson.A{bson.M{"_id": f.ID, "value": first}}}})

	get := func(url, token string) (int, string, string) {
		req, _ := http.NewRequest("GET", url, nil)
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
	url := w.srv.URL + "/api/boards/" + b.ID + "/export/csv"
	code, typ, body := get(url, a)
	if code != 200 || typ != "text/csv; charset=utf-8" || !strings.HasPrefix(body, "\uFEFF") {
		t.Fatalf("csv: %d %s %q", code, typ, body[:min(len(body), 40)])
	}
	lines := strings.Split(strings.TrimSuffix(strings.TrimPrefix(body, "\uFEFF"), "\r\n"), "\r\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], `"Title","Description","List","Swimlane","Owner"`) || !strings.HasSuffix(lines[0], `"CustomField-Size-dropdown-S/M"`) {
		t.Fatalf("WeKan's header, in English, and the custom field: %q", lines[0])
	}
	row := lines[1]
	// A label without a name is "-green": formula-escaped, as WeKan's Papa Parse does.
	for _, want := range []string{`"C"`, `"'=HYPERLINK(""http://x"")"`, `"L"`, `"ann"`, `"bob"`, `"'-green"`, `"M"`} {
		if !strings.Contains(row, want) {
			t.Errorf("the row has %s: %s", want, row)
		}
	}
	if strings.Contains(row, "ghostuser") {
		t.Error("negative: a deleted member is a blank, not an id")
	}
	code, typ, body = get(url+"?delimiter=%09", a)
	if code != 200 || typ != "text/tsv" || strings.HasPrefix(body, "\uFEFF") || !strings.Contains(body, "\"Title\"\t\"Description\"") {
		t.Fatalf("tsv: %d %s %q", code, typ, body[:min(len(body), 40)])
	}
	if code, _, _ = get(url+"?delimiter=%22", a); code != 200 {
		t.Fatal("a quote as the delimiter falls back to a comma")
	}
	if code, _, _ = get(url, ""); code != 401 {
		t.Fatalf("negative: no token: %d", code)
	}
	if code, _, _ = get(url, bt); code != 403 {
		t.Fatalf("negative: a non-member: %d", code)
	}
	setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: true, IsNormalAssignedOnly: true})
	if code, _, _ = get(url, bt); code != 403 {
		t.Fatalf("negative: an assigned-only member, who may not see every card: %d", code)
	}
	if code, _, _ = get(url+"?authToken="+a, ""); code != 200 {
		t.Fatalf("a download link: %d", code)
	}
}
