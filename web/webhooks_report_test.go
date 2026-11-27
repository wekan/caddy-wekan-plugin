package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func TestWebhooksReport(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	admin := w.seedUser(t, "root", "pw")
	owner := w.seedUser(t, "owner", "pw")
	if _, err := w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": admin.ID}, bson.M{"$set": bson.M{"isAdmin": true}}); err != nil {
		t.Fatal(err)
	}
	b, err := w.s.CreateBoard(ctx, owner, "Roadmap")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.db.Collection("settings").UpdateOne(ctx, bson.M{}, bson.M{"$set": bson.M{"webhooksDisabled": true}}, options.UpdateOne().SetUpsert(true)); err != nil {
		t.Fatal(err)
	}
	_, err = w.db.Collection("integrations").InsertMany(ctx, []bson.M{
		{"_id": "board-hook", "boardId": b.ID, "userId": owner.ID, "title": "<Roadmap hook>", "url": "https://user:url-password@example.org/path?key=query-secret", "token": "stored-token"},
		{"_id": "global-hook", "boardId": "_global", "title": "Global destination", "enabled": false},
	})
	if err != nil {
		t.Fatal(err)
	}
	siteClient, ownerClient := w.client(t), w.client(t)
	body(t, w.signIn(t, siteClient, "root", "pw"))
	body(t, w.signIn(t, ownerClient, "owner", "pw"))
	report := w.srv.URL + "/admin/problems/webhooks"
	res, html := get(t, siteClient, report+"?filter=on&q=roadmap")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("report status: %d", res.StatusCode)
	}
	for _, want := range []string{"js-webhooks-report-off", "js-webhooks-report-board", "board-hook", "&lt;Roadmap hook&gt;", "example.org/path?key=%2A%2A%2A", "owner"} {
		if !strings.Contains(html, want) {
			t.Errorf("report missing %q", want)
		}
	}
	for _, hidden := range []string{"url-password", "query-secret", "stored-token", "global-hook"} {
		if strings.Contains(html, hidden) {
			t.Errorf("report unexpectedly contains %q", hidden)
		}
	}
	if _, html := get(t, siteClient, report+"?filter=off"); !strings.Contains(html, "global-hook") || strings.Contains(html, "board-hook") {
		t.Fatal("the off filter should list only the disabled global webhook")
	}
	if res, _ := get(t, ownerClient, report); res.StatusCode != http.StatusNotFound {
		t.Fatalf("board owner report status: %d", res.StatusCode)
	}
	_, reply := methodCall(t, w, siteClient, "webhooksReportPage", `[{"limit":1}]`)
	result, ok := reply["result"].(map[string]any)
	if !ok || result["total"] != float64(2) || result["webhooksOff"] != true {
		t.Fatal("expected the total and switch state")
	}
	rows := result["rows"].([]any)
	if len(rows) != 1 {
		t.Fatal("expected one row at the requested limit")
	}
	row := rows[0].(map[string]any)
	if row["_id"] != "board-hook" || row["boardTitle"] != "Roadmap" || row["tokenSet"] != true || row["token"] != nil {
		t.Fatal("expected the redacted board hook")
	}
	if _, reply := methodCall(t, w, ownerClient, "webhooksReportPage", `[]`); reply["error"] != "not-authorized" {
		t.Fatal("the method is restricted to site administrators")
	}
}
