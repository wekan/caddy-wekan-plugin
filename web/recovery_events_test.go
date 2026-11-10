package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// TestRecoveryEventsReport: Admin Panel / Problems / Recovery lists the
// recoveryEvents rows as WeKan's report does - an import's losses among
// them - newest first, ten a page, searched and shown by status; its
// detail is text, never markup; and it is the admins' only.
func TestRecoveryEventsReport(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	admin := w.seedUser(t, "boss", "pw")
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": admin.ID}, bson.M{"$set": bson.M{"isAdmin": true}})
	w.seedUser(t, "ann", "pw")
	c := w.client(t)
	body(t, w.signIn(t, c, "boss", "pw"))
	if res, err := c.PostForm(w.srv.URL+"/import/markdown", url.Values{"text": {"# Lossy\n## To Do\n- [ ] One\n<b>stray</b> line\n"}}); err != nil || res.StatusCode != http.StatusSeeOther {
		t.Fatalf("the import: %v %d", err, res.StatusCode)
	}
	base := time.Now().Add(-time.Hour)
	for n := 0; n < 12; n++ {
		_, _ = w.db.Collection("recoveryEvents").InsertOne(ctx, bson.M{"_id": "ev" + strconv.Itoa(n), "type": "text-backup", // inserted, not a filter
			"detail": "backup " + strconv.Itoa(n), "done": n != 3, "deletedData": n == 5, "createdAt": base.Add(time.Duration(n) * time.Minute),
			"ip": "203.0.113.7", "location": bson.M{"city": "Oulu", "country": "FI"}})
	}
	page := getBody(t, c, w.srv.URL+"/admin/problems/recovery")
	if !strings.Contains(page, "import-completed-with-warnings") || !strings.Contains(page, "markdown: 1 not imported") {
		t.Error("the import's row is listed, newest first")
	}
	if strings.Contains(page, "<b>stray</b>") {
		t.Error("negative: a detail is text, never markup")
	}
	if !strings.Contains(page, "1 / 2") || !strings.Contains(page, "203.0.113.7") || !strings.Contains(page, "Oulu, FI") {
		t.Error("ten a page of thirteen, the address and its location")
	}
	if p2 := getBody(t, c, w.srv.URL+"/admin/problems/recovery?page=2"); !strings.Contains(p2, "backup 0") || strings.Contains(p2, "backup 9") {
		t.Error("the second page has the oldest")
	}
	if f := getBody(t, c, w.srv.URL+"/admin/problems/recovery?status=failed"); !strings.Contains(f, "backup 3") || strings.Contains(f, "backup 4") {
		t.Error("Failed shows the rows that are not done")
	}
	if d := getBody(t, c, w.srv.URL+"/admin/problems/recovery?status=deleted"); !strings.Contains(d, "backup 5") || strings.Contains(d, "backup 4") {
		t.Error("Deleted shows the rows that deleted data")
	}
	if s := getBody(t, c, w.srv.URL+"/admin/problems/recovery?q=MARKDOWN"); !strings.Contains(s, "markdown: 1 not imported") || strings.Contains(s, "backup 1") {
		t.Error("the search is the event, detail and database, any case")
	}
	if s := getBody(t, c, w.srv.URL+"/admin/problems/recovery?q=.*"); strings.Contains(s, "backup 1") {
		t.Error("negative: the search is text, not a regular expression")
	}
	// Negative: a member who is not an admin does not see it.
	other := w.client(t)
	body(t, w.signIn(t, other, "ann", "pw"))
	res, _ := other.Get(w.srv.URL + "/admin/problems/recovery")
	b := body(t, res)
	if res.StatusCode == http.StatusOK || strings.Contains(b, "backup 1") {
		t.Errorf("negative: a non-admin sees the report: %d", res.StatusCode)
	}
}
