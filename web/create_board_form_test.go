package web

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestCreationTitles(t *testing.T) {
	for _, c := range []struct {
		in       string
		separate bool
		want     []string
	}{
		{" One\r\nTwo ", false, []string{"One\nTwo"}},
		{" One\r\n\n Two \rThree", true, []string{"One", "Two", "Three"}},
		{" \n ", true, nil},
		{"", false, nil},
	} {
		if got := creationTitles(c.in, c.separate); !slices.Equal(got, c.want) {
			t.Errorf("%q %v: %q", c.in, c.separate, got)
		}
	}
}

// The header's Create Board pop-over, WeKan's createBoardForm: one board, or
// one a line, the page going to the last made; offered to whoever may make a
// board, and the policy still holds for each.
func TestHeaderCreateBoard(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	ac := w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	if _, html := get(t, ac, w.srv.URL+"/"); !strings.Contains(html, `id="header-new-board-icon"`) || !strings.Contains(html, "js-new-board-title") {
		t.Fatal("the header offers Create Board")
	}
	post := func(v url.Values) *http.Response {
		res, err := noRedirect(ac).PostForm(w.srv.URL+"/boards", v)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res
	}
	count := func(title string) int64 {
		n, _ := w.db.Collection("boards").CountDocuments(ctx, bson.M{"title": title})
		return n
	}
	if res := post(url.Values{"title": {"Alpha\nBeta\n\nGamma"}, "titleMode": {"separate"}}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("many: %d", res.StatusCode)
	} else {
		var last Board
		_ = w.db.Collection("boards").FindOne(ctx, bson.M{"title": "Gamma"}).Decode(&last)
		if loc := res.Header.Get("Location"); !strings.Contains(loc, last.ID) {
			t.Errorf("the page goes to the last board made: %s", loc)
		}
	}
	for _, title := range []string{"Alpha", "Beta", "Gamma"} {
		if count(title) != 1 {
			t.Errorf("one board a line: %s", title)
		}
	}
	if post(url.Values{"title": {"One\nboard"}, "titleMode": {"one"}}); count("One\nboard") != 1 {
		t.Error("one board, its title with the newline")
	}
	if res := post(url.Values{"title": {" \n "}, "titleMode": {"separate"}}); res.StatusCode != http.StatusBadRequest {
		t.Errorf("negative: no title: %d", res.StatusCode)
	}
	// While only admins make boards, a member is not offered it, and is refused.
	_, _ = w.db.Collection("tableVisibilityModeSettings").UpdateOne(ctx, bson.M{"_id": "tableVisibilityMode-boardCreationAdminOnly"},
		bson.M{"$set": bson.M{"booleanValue": true}}, optionsUpsert())
	if _, html := get(t, ac, w.srv.URL+"/"); strings.Contains(html, `id="header-new-board-icon"`) {
		t.Error("negative: not offered while only admins make boards")
	}
	if n, _ := w.db.Collection("eventlog").CountDocuments(ctx, bson.M{"bleed": "CreateBleed"}); n != 0 {
		t.Errorf("negative: showing a page is no attempt: %d", n)
	}
	if res := post(url.Values{"title": {"X\nY"}, "titleMode": {"separate"}}); res.StatusCode != http.StatusForbidden || count("X") != 0 {
		t.Errorf("negative: refused, none made: %d", res.StatusCode)
	}
	if n, _ := w.db.Collection("eventlog").CountDocuments(ctx, bson.M{"bleed": "CreateBleed"}); n == 0 {
		t.Error("the refusal is recorded")
	}
}
