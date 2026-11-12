package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Admin Panel / Problems / Security Report, WeKan's admin-only-custom-fields
// Problems test in Go: a refused field write is one row there - its Bleed,
// its source and who - with neither the forged value nor the secret one; it
// is a site admin's page, searched, and linked from the other panes.
func TestSecurityReportPage(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	f, bob := fieldSetup(t, w)
	root := w.seedUser(t, "root", "pw")
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": root.ID}, bson.M{"$set": bson.M{"isAdmin": true}})
	bc := w.client(t)
	body(t, w.signIn(t, bc, "bob", "pw"))
	if _, out := methodCall(t, w, bc, "/cards/update", `[{"_id":"`+f.rb.Card+`"},{"$set":{"customFields.0.value":"FORGED-PRIVATE-VALUE"}}]`); out["error"] == nil {
		t.Fatal("refused")
	}
	rc := w.client(t)
	body(t, w.signIn(t, rc, "root", "pw"))
	res, html := get(t, rc, w.srv.URL+"/admin/problems/security-report")
	if res.StatusCode != 200 {
		t.Fatalf("the page: %d", res.StatusCode)
	}
	main := html[strings.Index(html, `class="main-body"`):]
	for _, want := range []string{"AdminFieldBleed", "ddp:cards.update", bob.Username, "Security Report"} {
		if !strings.Contains(main, want) {
			t.Errorf("the row shows %q", want)
		}
	}
	for _, never := range []string{"FORGED-PRIVATE-VALUE", "CLASSIFIED-9999"} {
		if strings.Contains(html, never) {
			t.Errorf("negative: the page never shows %q", never)
		}
	}
	if _, found := get(t, rc, w.srv.URL+"/admin/problems/security-report?q=NoSuchBleed"); strings.Contains(found[strings.Index(found, `class="main-body"`):], "AdminFieldBleed") {
		t.Error("the search narrows the rows")
	}
	for _, pane := range []string{"/admin/problems/recovery", "/admin/problems/password-length"} {
		if _, p := get(t, rc, w.srv.URL+pane); !strings.Contains(p, `href="/admin/problems/security-report"`) {
			t.Errorf("%s links the Security Report", pane)
		}
	}
	// Negative: anybody else's answer is the not-found page.
	ac := w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	if res, p := get(t, ac, w.srv.URL+"/admin/problems/security-report"); res.StatusCode != http.StatusNotFound || strings.Contains(p, "AdminFieldBleed") {
		t.Errorf("negative: not a site admin: %d", res.StatusCode)
	}
}
