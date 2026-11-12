package web

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestSectionOrderOf(t *testing.T) {
	got := sectionOrderOf(bson.A{"description", "nope", "labels", "description", 3}, "card")
	if got[0] != "description" || got[1] != "labels" || len(got) != len(cardSettings.Order["card"]) {
		t.Fatalf("known ids once, then the rest: %v", got)
	}
	if got := sectionOrderOf(nil, "minicard"); got[0] != "scrum" {
		t.Fatalf("the minicard's default starts with Scrum: %v", got)
	}
}

// A row that modifies another follows it, wherever that one is moved.
func TestRowsForSide(t *testing.T) {
	keys := func(rows []*cardSettingsRow) string {
		var out []string
		for _, r := range rows {
			out = append(out, r.Key)
		}
		return " " + strings.Join(out, " ") + " "
	}
	got := keys(rowsForSide("card", applyCardLayout([]string{"dueDate", "receivedDate"})))
	if !strings.Contains(got, " dueDate dueFromTitle atRisk ") && !strings.Contains(got, " dueDate atRisk dueFromTitle ") {
		t.Fatalf("the due date's helpers under it: %s", got)
	}
	if strings.Index(got, " dueDate ") > strings.Index(got, " receivedDate ") {
		t.Fatalf("the stored order: %s", got)
	}
	if strings.Contains(got, " listTitle ") == false {
		t.Fatal("every row of the side")
	}
}

func TestCardSettingsOrderWrites(t *testing.T) {
	// An unknown id is normalised away, as WeKan's sectionOrder does.
	clean, ok := cardSettingsBoardWrite(map[string]any{"cardSettingsSectionOrder": []any{"description", "labels", "nope"}})
	if !ok || clean["cardSettingsSectionOrder"].([]string)[0] != "description" || len(clean["cardSettingsSectionOrder"].([]string)) != len(cardSettings.Order["card"]) {
		t.Fatalf("normalised: %v", clean)
	}
	clean, ok = cardSettingsBoardWrite(map[string]any{"cardSettingsSectionOrder": []any{"description", "labels"}, "cardFieldOrder": []any{"dueDate", "receivedDate", "bogus"}})
	if !ok {
		t.Fatal("both orders are Card Settings'")
	}
	if o := clean["cardSettingsSectionOrder"].([]string); o[0] != "description" || len(o) != len(cardSettings.Order["card"]) {
		t.Fatalf("the section order, whole: %v", o)
	}
	if o := strings.Join(clean["cardFieldOrder"].([]string), " "); strings.Contains(o, "bogus") || strings.Index(o, "dueDate") > strings.Index(o, "receivedDate") {
		t.Fatalf("the field order, canonical: %s", o)
	}
	for _, bad := range []map[string]any{{"cardFieldOrder": "dueDate"}, {"cardSettingsSectionOrder": []any{1}}, {"minicardFieldOrders": []any{"labels"}}, {"minicardFieldOrder": []any{"labels", 2}}} {
		if _, ok := cardSettingsBoardWrite(bad); ok {
			t.Fatalf("refused: %v", bad)
		}
	}
	// The minicard's field order is its own, in the minicard's layout
	// (MINICARD_LAYOUT): writable since the Minicard pane's rows are dragged.
	clean, ok = cardSettingsBoardWrite(map[string]any{"minicardFieldOrder": []any{"endDate", "receivedDate"}})
	if o := strings.Join(clean["minicardFieldOrder"].([]string), " "); !ok || !strings.HasPrefix(o, "dueComplete cardNumber cover endDate receivedDate startDate dueDate scrumSprint") ||
		strings.Index(o, "endDate") > strings.Index(o, "receivedDate") || !strings.Contains(o, "swimlaneName") {
		t.Fatalf("the minicard's order, canonical in its layout: %s", o)
	}
}

// The opened card draws its sections in the Card pane's order, its dates in
// the row order; the minicard's order is the minicard's own.
func TestOpenedCardFollowsTheCardPane(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Order")
	ac := w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	cardURL := w.srv.URL + "/b/" + rb.ID + "/order/" + rb.Card
	sections := func(html string) []string {
		var out []string
		for _, m := range regexp.MustCompile(`class="card-details-section card-details-section-[A-Za-z]+" data-card-section="([A-Za-z]+)"`).FindAllStringSubmatch(html, -1) {
			out = append(out, m[1])
		}
		return out
	}
	_, html := get(t, ac, cardURL)
	got := sections(html)
	if len(got) < 5 || got[0] != "labels" {
		t.Fatalf("a default board: Labels first: %v", got)
	}
	if !strings.Contains(html, `js-toggle-card-section" data-section="date" role="button" tabindex="0"><i class="fa fa-calendar"`) {
		t.Fatal("each section headed with its icon")
	}
	if _, out := methodCall(t, w, ac, "/boards/update", `[{"_id":"`+rb.ID+`"},{"$set":{"cardSettingsSectionOrder":["description","labels"],"cardFieldOrder":["dueDate","receivedDate"]}}]`); out["error"] != nil {
		t.Fatalf("the board admin orders: %v", out)
	}
	_, html = get(t, ac, cardURL)
	got = sections(html)
	if got[0] != "description" || got[1] != "labels" {
		t.Fatalf("the card follows: %v", got)
	}
	if strings.Index(html, `card-details-item-due`) > strings.Index(html, `card-details-item-received`) {
		t.Fatal("Due above Received on the card")
	}
	var b bson.M
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": rb.ID}).Decode(&b)
	if b["minicardSettingsSectionOrder"] != nil {
		t.Fatal("the minicard's order is its own")
	}
	// The pane shows the stored order too.
	if strings.Index(html, `js-card-settings-pane card-settings-pane-card" data-side="card"`) < 0 {
		t.Fatal("the Card pane")
	}
	pane := html[strings.Index(html, `data-side="card" data-board=`):]
	if strings.Index(pane, `data-section="description"`) > strings.Index(pane, `data-section="labels"`) {
		t.Fatal("the Card pane in the card's order")
	}
}
