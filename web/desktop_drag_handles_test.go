package web

import (
	"net/http"
	"strings"
	"testing"
)

func TestDesktopDragHandlesPreference(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	w.seedUser(t, "rob", "pw")
	ann, rob := w.client(t), w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	body(t, w.signIn(t, rob, "rob", "pw"))

	assertSwitch := func(client *http.Client, want string) {
		t.Helper()
		_, html := get(t, client, w.srv.URL+"/")
		if !strings.Contains(html, `class="js-toggle-desktop-drag-handles" href="#" role="switch" aria-checked="`+want+`"`) {
			t.Fatalf("drag handles switch should read %s", want)
		}
	}
	toggle := func() {
		t.Helper()
		req, err := http.NewRequest("POST", w.srv.URL+"/_methods/toggleDesktopDragHandles", strings.NewReader("[]"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := ann.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("toggle returned %d", res.StatusCode)
		}
	}
	assertSwitch(ann, "false")
	toggle()
	assertSwitch(ann, "true")
	assertSwitch(rob, "false")
	toggle()
	assertSwitch(ann, "false")
}
