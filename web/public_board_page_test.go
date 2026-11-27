package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestGuestBoardPage(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	board := newRESTBoard(t, w, login(t, w, "ann"), "Public board fixture")
	guest := w.client(t)
	url := w.srv.URL + "/b/" + board.ID + "/public-board-fixture"
	change := func(permission string) {
		t.Helper()
		if _, err := w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": board.ID},
			bson.M{"$set": bson.M{"permission": permission}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, permission := range []string{"private", "instance", "public", "private"} {
		change(permission)
		res, html := get(t, guest, url)
		if permission == "public" {
			if res.StatusCode != http.StatusOK || !strings.Contains(html, "js-list") || strings.Contains(html, "js-create-board") {
				t.Fatalf("public board should render read-only for a guest: %d", res.StatusCode)
			}
			res, html = get(t, guest, url+"/"+board.Card)
			if res.StatusCode != http.StatusOK || !strings.Contains(html, "js-card-details") || strings.Contains(html, "js-toggle-watch-card") {
				t.Fatalf("public card should render read-only for a guest: %d", res.StatusCode)
			}
		} else if res.StatusCode != http.StatusNotFound || strings.Contains(html, "Public board fixture") {
			t.Fatalf("guest must not see a %s board: %d", permission, res.StatusCode)
		}
	}
}
