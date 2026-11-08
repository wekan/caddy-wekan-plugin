package web

import (
	"slices"
	"strings"
	"testing"
)

// Expected values are what WeKan's own models/lib/boardViewSettings.js answers
// for the same input (run with Node.js).
func TestBoardViewSettingsAsWeKan(t *testing.T) {
	older := []string{}
	for _, v := range defaultBoardViewOrder {
		if !slices.Contains(viewsAddedLater, v) {
			older = append(older, v)
		}
	}
	for name, c := range map[string]struct {
		in   []string
		head string
	}{
		"empty":        {nil, "board-view-swimlanes,board-view-lists,board-view-table,board-view-cal,board-view-calendar-mode,board-view-multiboard-cal"},
		"custom":       {[]string{"board-view-table", "board-view-swimlanes", "bogus", "board-view-table"}, "board-view-table,board-view-swimlanes,board-view-lists,board-view-cal,board-view-calendar-mode,board-view-multiboard-cal"},
		"olderDefault": {older, "board-view-swimlanes,board-view-lists,board-view-table,board-view-cal,board-view-calendar-mode,board-view-multiboard-cal"},
	} {
		got := normalizeBoardViewOrder(c.in)
		if len(got) != 40 || strings.Join(got[:6], ",") != c.head {
			t.Errorf("%s: %v", name, got[:6])
		}
	}
	state := func() *boardViewState {
		return &boardViewState{Settings: map[string]map[string]any{}, Defaults: map[string]string{"private": "board-view-lists"}}
	}
	for _, c := range []struct {
		in   map[string]any
		want string
	}{
		{map[string]any{"defaultPublicBoardView": "board-view-table", "boardViewSettings": map[string]any{"board-view-table": map[string]any{"showOnPublic": false}}},
			"boardViewSettings.board-view-table.showOnPublic: cannot hide the public default view; set defaultPublicBoardView to another view first"},
		{map[string]any{"boardViewSettings": map[string]any{"board-view-lists": map[string]any{"showOnPrivate": "false"}}},
			"boardViewSettings.board-view-lists.showOnPrivate: cannot hide the private default view; set defaultPrivateBoardView to another view first"},
		{map[string]any{"boardViewOrder": []any{"board-view-cal", "nope"}}, `boardViewOrder: unknown board view "nope"`},
		{map[string]any{"boardViewOrder": "x"}, "boardViewOrder must be an array of board view keys"},
	} {
		if _, problem := boardViewRequest(state(), c.in); problem != c.want {
			t.Errorf("%v:\n got  %s\n want %s", c.in, problem, c.want)
		}
	}
	set, problem := boardViewRequest(state(), map[string]any{"defaultPrivateBoardView": "board-view-table",
		"boardViewSettings": map[string]any{"board-view-lists": map[string]any{"showOnPrivate": false}}})
	if problem != "" || set["defaultPrivateBoardView"] != "board-view-table" || set["boardViewSettings.board-view-table.showOnPrivate"] != true ||
		set["boardViewSettings.board-view-lists.showOnPrivate"] != false || len(set) != 3 {
		t.Fatalf("a new default first, then the old one may be hidden: %v %s", set, problem)
	}
}

func TestRESTBoardViewSettings(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	a, bt := login(t, w, "ann"), login(t, w, "bob")
	b := newRESTBoard(t, w, a, "Views")
	url := w.srv.URL + "/api/boards/" + b.ID + "/boardViewSettings"
	var got map[string]any
	if code := call(t, "GET", url, a, "", &got); code != 200 || got["defaultPrivateBoardView"] != "board-view-swimlanes" {
		t.Fatalf("snapshot: %d %v", code, got["defaultPrivateBoardView"])
	}
	body := `{"defaultPublicBoardView":"board-view-table","boardViewSettings":{"board-view-map":{"showOnPublic":false}},"boardViewOrder":["board-view-cal"]}`
	if code := call(t, "PUT", url, a, body, &got); code != 200 || got["defaultPublicBoardView"] != "board-view-table" {
		t.Fatalf("put: %d %v", code, got)
	}
	settings := got["boardViewSettings"].(map[string]any)
	if settings["board-view-map"].(map[string]any)["showOnPublic"] != false || got["boardViewOrder"].([]any)[0] != "board-view-cal" {
		t.Fatalf("kept: %v", got)
	}
	if code := call(t, "PUT", url, a, `{"boardViewSettings":{"board-view-table":{"showOnPublic":false}}}`, nil); code != 400 {
		t.Fatalf("negative: hiding the public default: %d", code)
	}
	if code := call(t, "PUT", url, a, `{"something":1}`, nil); code != 400 {
		t.Fatalf("negative: nothing it knows: %d", code)
	}
	setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: true})
	if code := call(t, "PUT", url, bt, body, nil); code != 403 {
		t.Fatalf("negative: not a board admin: %d", code)
	}
}
