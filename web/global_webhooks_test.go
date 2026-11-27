package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func TestGlobalWebhookManagement(t *testing.T) {
	realLookup := lookupHost
	t.Cleanup(func() { lookupHost = realLookup })
	lookupHost = func(context.Context, string) ([]string, error) { return []string{"93.184.216.34"}, nil }

	w := newTestWeKan(t)
	ctx := context.Background()
	admin := w.seedUser(t, "site", "pw")
	w.seedUser(t, "member", "pw")
	if _, err := w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": admin.ID}, bson.M{"$set": bson.M{"isAdmin": true}}); err != nil {
		t.Fatal(err)
	}
	siteToken, memberToken := login(t, w, "site"), login(t, w, "member")
	site, member := w.client(t), w.client(t)
	body(t, w.signIn(t, site, "site", "pw"))
	body(t, w.signIn(t, member, "member", "pw"))
	page := w.srv.URL + "/admin/settings/global-webhooks"
	base := w.srv.URL + "/api/admin/webhooks"
	payload := `{"url":"https://hooks.example/global","title":"Site activity","token":"example-private-token"}`
	if status := call(t, "POST", base, memberToken, payload, nil); status != http.StatusForbidden {
		t.Fatalf("member write status: %d", status)
	}
	if res, _ := get(t, member, page); res.StatusCode != http.StatusNotFound {
		t.Fatalf("member page status: %d", res.StatusCode)
	}
	var created struct {
		ID string `json:"_id"`
	}
	if status := call(t, "POST", base, siteToken, payload, &created); status != http.StatusOK || created.ID == "" {
		t.Fatalf("global creation status: %d", status)
	}
	if status := call(t, "PUT", base+"/"+created.ID, siteToken, `{"title":"Updated global activity"}`, nil); status != http.StatusOK {
		t.Fatalf("global edit status: %d", status)
	}
	if res, html := get(t, site, page); res.StatusCode != http.StatusOK ||
		!strings.Contains(html, "Updated global activity") || strings.Contains(html, "example-private-token") {
		t.Fatal("expected the global webhook form without its stored token")
	}
	adminNotifications := w.srv.URL + "/admin/notifications"
	if html := getBody(t, site, adminNotifications); !strings.Contains(html, "js-global-webhooks") {
		t.Fatal("expected navigation to Global Webhooks")
	}
	if _, err := w.db.Collection("settings").UpdateOne(ctx, bson.M{}, bson.M{"$set": bson.M{"webhooksDisabled": true}}, options.UpdateOne().SetUpsert(true)); err != nil {
		t.Fatal(err)
	}
	if res, _ := get(t, site, page); res.StatusCode != http.StatusNotFound {
		t.Fatalf("disabled page status: %d", res.StatusCode)
	}
	if html := getBody(t, site, adminNotifications); strings.Contains(html, "js-global-webhooks") {
		t.Fatal("Global Webhooks navigation should be hidden")
	}
	if status := call(t, "PUT", base+"/"+created.ID, siteToken, `{"enabled":false}`, nil); status != http.StatusForbidden {
		t.Fatalf("disabled edit status: %d", status)
	}
	if status := call(t, "DELETE", base+"/"+created.ID, siteToken, "", nil); status != http.StatusOK {
		t.Fatalf("cleanup status: %d", status)
	}
}
