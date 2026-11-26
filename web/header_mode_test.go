package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// WeKan's header: Home, the toggle that collapses the header's icons and the
// board's title in that order, then the mobile/desktop toggle, the first of
// the icons it collapses; setMobileMode keeps the user's choice, a boolean.
func TestHeaderCollapseAndMobileMode(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Header")
	c := w.client(t)
	body(t, w.signIn(t, c, "ann", "pw"))
	_, html := get(t, c, w.srv.URL+"/b/"+b.ID+"/"+b.Slug)
	home := strings.Index(html, `class="header-home-link"`)
	toggle := strings.Index(html, `js-toggle-header-icons-collapsed`)
	title := strings.Index(html, `class="header-page-title`)
	mode := strings.Index(html, `class="mobile-mode-toggle js-header-collapsible-icon"`)
	if home < 0 || !(home < toggle && toggle < title && title < mode) {
		t.Fatalf("Home, the collapse toggle, the title, then the mobile/desktop toggle: %d %d %d %d", home, toggle, title, mode)
	}
	if !strings.Contains(html, `data-profile=""`) {
		t.Error("no choice yet")
	}
	post := func(args string) int {
		req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/setMobileMode", strings.NewReader(args))
		req.Header.Set("Content-Type", "application/json")
		res, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	profile := func() any {
		var d bson.M
		_ = w.db.Collection("users").FindOne(ctx, bson.M{"_id": u.ID}).Decode(&d)
		p, _ := d["profile"].(bson.M)
		if p == nil {
			if pd, ok := d["profile"].(bson.D); ok {
				for _, e := range pd {
					if e.Key == "mobileMode" {
						return e.Value
					}
				}
			}
			return nil
		}
		return p["mobileMode"]
	}
	if code := post(`[true]`); code != 200 || profile() != true {
		t.Fatalf("setMobileMode true: %d %v", code, profile())
	}
	if _, html := get(t, c, w.srv.URL+"/b/"+b.ID+"/"+b.Slug); !strings.Contains(html, `data-profile="true"`) {
		t.Error("the choice is on the toggle")
	}
	if code := post(`[false]`); code != 200 || profile() != false {
		t.Fatalf("setMobileMode false: %d %v", code, profile())
	}
	// Negative: only a boolean, and only one's own, signed in.
	for _, args := range []string{`["true"]`, `[1]`, `[null]`, `[]`, `[true,true]`, `[{"$set":1}]`} {
		if code := post(args); code == 200 {
			t.Errorf("setMobileMode %s: %d", args, code)
		}
	}
	if profile() != false {
		t.Errorf("unchanged by what was refused: %v", profile())
	}
	anon := w.client(t)
	req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/setMobileMode", strings.NewReader(`[true]`))
	req.Header.Set("Content-Type", "application/json")
	if res, err := anon.Do(req); err == nil {
		res.Body.Close()
		if res.StatusCode == 200 {
			t.Error("signed out")
		}
	}
	if profile() != false {
		t.Errorf("signed out changed it: %v", profile())
	}
}
