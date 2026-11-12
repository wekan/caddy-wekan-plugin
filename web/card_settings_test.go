package web

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The table is WeKan's: generated again from WeKan's own files when they and
// a node are beside the plugin, it is byte for byte card_settings.json.
func TestCardSettingsTableIsWeKans(t *testing.T) {
	wekan, _ := filepath.Abs("../../../..")
	if _, err := os.Stat(filepath.Join(wekan, "models/lib/cardSettingsRows.js")); err != nil {
		t.Skip("WeKan is not beside the plugin")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		matches, _ := filepath.Glob(filepath.Join(wekan, ".tools/node-v*/bin/node"))
		if len(matches) == 0 {
			t.Skip("no node")
		}
		node = matches[0]
	}
	out, err := exec.Command(node, "../cmd/card-settings/generate.cjs", wekan).Output()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, cardSettingsJSON) {
		t.Fatal("card_settings.json is not what cmd/card-settings/generate.cjs makes of WeKan's table: run it again")
	}
}

// Every row of WeKan's table has its side(s), every section's key is a row,
// and the one row whose checkbox writes another field than its own writes it.
func TestCardSettingsTable(t *testing.T) {
	if len(cardSettings.Rows) < 60 || len(cardSettings.Sections) != 14 {
		t.Fatalf("rows %d, sections %d", len(cardSettings.Rows), len(cardSettings.Sections))
	}
	for _, sec := range cardSettings.Sections {
		for _, k := range sec.Keys {
			if cardSettingsRowOf(k) == nil {
				t.Errorf("section %s: no row %s", sec.ID, k)
			}
		}
	}
	if r := cardSettingsRowOf("labelText"); r == nil || r.Minicard.Field != "showLabelText" {
		t.Fatalf("labelText's minicard checkbox writes showLabelText: %+v", r)
	}
	if r := cardSettingsRowOf("scrumBacklogRank"); r == nil || r.Minicard.Kind != "scrum" || r.Minicard.Scrum != "minicardBacklogRank" {
		t.Fatalf("Backlog rank is a Scrum visibility flag: %+v", r)
	}
	if r := cardSettingsRowOf("scrumIssueType"); r == nil || r.Minicard.Field != "allowsIssueTypeOnMinicard" || !r.Minicard.Default {
		t.Fatalf("the work item type's minicard row is its badge's own flag, on by default: %+v", r)
	}
	if !cardSettingsBoardFields["allowsCardNumberOnMinicard"] || cardSettingsBoardFields["members"] || cardSettingsBoardFields["permission"] {
		t.Fatal("the writable fields are the rows' and no others")
	}
}

// Board Settings / Card: both panes, the Scrum rows named without the
// section's title, the work item type badge on the minicard until a board
// admin turns it off; a member who is no admin may not, nor may anybody write
// another field, a wrong type or an unknown section through it.
func TestCardSettingsPopover(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Settings")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": rb.ID}, bson.M{"$push": bson.M{"members": bson.M{"userId": bob.ID, "isAdmin": false, "isActive": true}}})
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": rb.Card}, bson.M{"$set": bson.M{"scrum": bson.M{"issueType": "Story", "backlogRank": 4}, "scrumRevision": 1}})
	ac := w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	page := "/b/" + rb.ID + "/settings"
	_, html := get(t, ac, w.srv.URL+page)
	for _, want := range []string{`data-popup="boardCardSettingsPopup"`, `js-card-settings-pane card-settings-pane-minicard" data-side="minicard"`,
		`data-section="scrum"`, `data-key="scrumBacklogRank" data-side="minicard" data-kind="scrum"`, "Backlog rank",
		`data-key="scrumIssueType" data-side="minicard" data-kind="board" data-field="allowsIssueTypeOnMinicard"`,
		`class="badge minicard-issue-type issue-type-green"`, "js-open-board-card-settings"} {
		if !strings.Contains(html, want) {
			t.Fatalf("the board page has %s", want)
		}
	}
	if strings.Contains(html, `data-key="listTitle"`) {
		t.Fatal("List title is an opened card's own row, not the board's")
	}
	write := func(cl *http.Client, set string) map[string]any {
		_, out := methodCall(t, w, cl, "/boards/update", `[{"_id":"`+rb.ID+`"},{"$set":`+set+`}]`)
		return out
	}
	bc := w.client(t)
	body(t, w.signIn(t, bc, "bob", "pw"))
	if out := write(bc, `{"allowsIssueTypeOnMinicard":false}`); out["reason"] != "Access denied" {
		t.Fatalf("a member who is no admin may not: %v", out)
	}
	for _, bad := range []string{`{"allowsIssueTypeOnMinicard":"no"}`, `{"allowsIssueTypeOnMinicard":false,"permission":"public"}`,
		`{"cardSettingsHiddenOnCard":["scrum","nope"]}`, `{"cardSettingsHiddenOnCard":"scrum"}`} {
		if out := write(ac, bad); out["reason"] != "Access denied" {
			t.Fatalf("%s is refused: %v", bad, out)
		}
	}
	var b bson.M
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": rb.ID}).Decode(&b)
	if b["allowsIssueTypeOnMinicard"] == false || b["permission"] == "public" {
		t.Fatal("nothing refused was written")
	}
	if out := write(ac, `{"allowsIssueTypeOnMinicard":false,"cardSettingsHiddenOnCard":["scrum"]}`); out["error"] != nil {
		t.Fatalf("the board admin may: %v", out)
	}
	_, html = get(t, ac, w.srv.URL+page)
	if strings.Contains(html, "minicard-issue-type") {
		t.Fatal("the badge is gone from the minicard")
	}
	if cardSettingsTicked(t, html, "scrumIssueType", "minicard") {
		t.Fatal("its checkbox is unticked")
	}
	if !strings.Contains(html, `js-card-settings-section is-section-hidden" data-section="scrum"`) {
		t.Fatal("the hidden section is drawn hidden")
	}
	// The Scrum row: scrum.configure's visibility flag, one side only.
	if _, out := methodCall(t, w, ac, "scrum.configure", `["`+rb.ID+`",{"visibility":{"minicardBacklogRank":true}},0]`); out["error"] != nil {
		t.Fatalf("scrum.configure: %v", out)
	}
	_, html = get(t, ac, w.srv.URL+page)
	if !cardSettingsTicked(t, html, "scrumBacklogRank", "minicard") {
		t.Fatal("Backlog rank is ticked on the minicard")
	}
	if cardSettingsTicked(t, html, "scrumBacklogRank", "card") {
		t.Fatal("and not on the card")
	}
	// The user's own labels text: theirs to set, and anybody's.
	if _, out := methodCall(t, w, bc, "setShowLabelTextOverride", `[false]`); out["error"] != nil {
		t.Fatalf("setShowLabelTextOverride: %v", out)
	}
	var u struct {
		Profile struct {
			Override *bool `bson:"showLabelTextOverride"`
		} `bson:"profile"`
	}
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"_id": bob.ID}).Decode(&u)
	if u.Profile.Override == nil || *u.Profile.Override {
		t.Fatal("the override is set")
	}
	if _, out := methodCall(t, w, bc, "setShowLabelTextOverride", `["x"]`); out["error"] != "Match.Error" {
		t.Fatalf("only a boolean or null: %v", out)
	}
}

// cardSettingsTicked says whether the row's checkbox on that side is ticked.
func cardSettingsTicked(t *testing.T, html, key, side string) bool {
	t.Helper()
	at := strings.Index(html, `data-key="`+key+`" data-side="`+side+`"`)
	if at < 0 {
		t.Fatalf("no row %s on %s", key, side)
	}
	row := html[at:]
	row = row[:strings.Index(row, "</a>")]
	return strings.Contains(row, `aria-checked="true"`)
}

func TestIssueTypeBadge(t *testing.T) {
	for in, want := range map[string]string{"Bug": "fa-bug red", "User Story": "fa-bookmark green", "new_feature": "fa-plus-square green", "Chore": "fa-circle-o grey"} {
		b := issueTypeBadge(in)
		if b == nil || b.Icon+" "+b.Tone != want || b.Name != in {
			t.Errorf("%s: %+v", in, b)
		}
	}
	if issueTypeBadge("  ") != nil || issueTypeBadge(nil) != nil || issueTypeBadge(3) != nil {
		t.Fatal("no badge without a name")
	}
}
