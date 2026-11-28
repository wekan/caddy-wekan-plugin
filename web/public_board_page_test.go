package web

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

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
			for _, report := range weeklyFlowViews {
				response, page := get(t, guest, url+"?view="+report.slug)
				if response.StatusCode != http.StatusOK || !strings.Contains(page, "weekly-flow-view") ||
					!strings.Contains(page, "js-open-"+report.slug+"-view") || ((report.key != "epicProgress" && report.key != "releaseBurndown" && report.key != "timeInList" && report.key != "workload") && !strings.Contains(page, "<meter")) {
					t.Fatalf("weekly report %s: %d", report.slug, response.StatusCode)
				}
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

func TestGuestBoardLiveEndsWhenPrivate(t *testing.T) {
	w := newTestWeKan(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	w.seedUser(t, "ann", "pw")
	board := newRESTBoard(t, w, login(t, w, "ann"), "Guest live fixture")
	set := func(permission string) {
		t.Helper()
		if _, err := w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": board.ID},
			bson.M{"$set": bson.M{"permission": permission}}); err != nil {
			t.Fatal(err)
		}
	}
	set("public")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.srv.URL+"/b/"+board.ID+"/live", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := w.client(t).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("live request status: %d", res.StatusCode)
	}
	set("private")
	w.s.hub.publishFrom(board.ID, "board-changed", "")
	var events []string
	scanner := bufio.NewScanner(res.Body)
	for scanner.Scan() {
		if line := scanner.Text(); strings.HasPrefix(line, "event: ") {
			events = append(events, line)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0] != "event: board-unavailable" {
		t.Fatalf("anonymous stream returned %v, want only board-unavailable", events)
	}
}
