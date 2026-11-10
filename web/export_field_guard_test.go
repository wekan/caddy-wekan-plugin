package web

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// adminOnlyCustomFields.js assertFieldExport, at every export: a card that
// holds a value of a field the reader may not read - another board's - and a
// board with an admin-only field refuse the JSON, the .zip, CSV, Excel, PDF,
// the other tools' formats and the charts, and Export all boards leaves the
// board out and names it. A field's null value is no value.
func TestEveryExportAssertsTheFields(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	a, bt := login(t, w, "ann"), login(t, w, "bob")
	b := newRESTBoard(t, w, a, "Guarded")
	other := newRESTBoard(t, w, a, "Elsewhere")
	setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: true})
	routes := []string{"/export", "/exportZip", "/export/csv", "/exportExcel", "/exportPDF", "/export/zenkit", "/export/markdown",
		"/lists/" + b.List + "/cards/" + b.Card + "/exportPDF", "/charts/dashboard/exportExcel"}
	get := func(path, token string) int {
		req, _ := http.NewRequest("GET", w.srv.URL+"/api/boards/"+b.ID+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := testClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	for _, p := range routes {
		if code := get(p, bt); code != 200 {
			t.Fatalf("before: %s answers %d", p, code)
		}
	}
	// A null value is no value.
	_, _ = w.db.Collection("customFields").InsertOne(ctx, bson.M{"_id": "fieldElsewhere1", "boardIds": bson.A{other.ID}, "name": "Theirs", "type": "text"}) // inserted, not a filter
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": b.Card}, bson.M{"$set": bson.M{"customFields": bson.A{bson.M{"_id": "fieldElsewhere1", "value": nil}}}})
	if code := get("/export", bt); code != 200 {
		t.Errorf("a null value refuses nothing: %d", code)
	}
	// Another board's field's value: refused, admin or not.
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": b.Card}, bson.M{"$set": bson.M{"customFields": bson.A{bson.M{"_id": "fieldElsewhere1", "value": "secret"}}}})
	for _, p := range routes {
		if code := get(p, bt); code != 403 {
			t.Errorf("negative: %s with another board's field value: %d", p, code)
		}
		if code := get(p, a); code != 403 {
			t.Errorf("negative: %s, for the admin too: %d", p, code)
		}
	}
	// An admin-only field: its admins only.
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": b.Card}, bson.M{"$unset": bson.M{"customFields": ""}})
	_, _ = w.db.Collection("customFields").InsertOne(ctx, bson.M{"_id": "fieldAdminOnly1", "boardIds": bson.A{b.ID}, "name": "Pay", "type": "text", "adminOnly": true}) // inserted, not a filter
	for _, p := range routes {
		if code := get(p, bt); code != 403 {
			t.Errorf("negative: %s, an admin-only field, a member: %d", p, code)
		}
		if code := get(p, a); code != 200 {
			t.Errorf("%s, an admin-only field, the admin: %d", p, code)
		}
	}
	// Export all boards: the board left out, named in skipped.txt.
	req, _ := http.NewRequest("GET", w.srv.URL+"/api/export-all-boards/wekan", nil)
	req.Header.Set("Authorization", "Bearer "+bt)
	res, err := testClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode == 200 {
		z, _ := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		found := false
		for _, f := range z.File {
			if f.Name == "skipped.txt" {
				rc, _ := f.Open()
				txt, _ := io.ReadAll(rc)
				found = strings.Contains(string(txt), "Guarded") && strings.Contains(string(txt), errFieldExport)
			}
		}
		if !found {
			t.Error("Export all boards names the board it left out")
		}
	} else if !strings.Contains(string(data), errFieldExport) {
		t.Errorf("Export all boards: %d %s", res.StatusCode, data)
	}
}
