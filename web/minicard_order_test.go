package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func partKeys(parts []minicardPart) string {
	var out []string
	for _, p := range parts {
		out = append(out, p.Key)
	}
	return strings.Join(out, " ")
}

// The minicard's parts in the Minicard pane's order: its sections', its
// rows', none of a hidden section; the opened card's order is not it.
func TestMinicardParts(t *testing.T) {
	if got := partKeys(minicardParts(bson.M{})); !strings.HasPrefix(got, "scrum issueType dates location assignee members requestedBy checklists") {
		t.Fatalf("WeKan's default: %s", got)
	}
	got := partKeys(minicardParts(bson.M{"minicardSettingsSectionOrder": bson.A{"members"}, "minicardFieldOrder": bson.A{"members", "assignee"},
		"cardSettingsSectionOrder": bson.A{"date"}, "cardSettingsHiddenOnMinicard": bson.A{"scrum"}}))
	if !strings.HasPrefix(got, "members assignee requestedBy dates") || strings.Contains(got, "scrum") {
		t.Fatalf("the Minicard pane's order, Scrum hidden: %s", got)
	}
}

func TestMinicardFollowsTheMinicardPane(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	ann := w.seedUser(t, "ann", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Mini")
	due := time.Now().Add(24 * time.Hour)
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": rb.Card}, bson.M{"$set": bson.M{"dueAt": due, "members": bson.A{ann.ID}, "assignees": bson.A{ann.ID}}})
	ac := w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	order := func() string {
		_, html := get(t, ac, w.srv.URL+"/b/"+rb.ID+"/mini")
		at := map[string]int{"dates": strings.Index(html, `<div class="dates">`), "assignees": strings.Index(html, `minicard-assignees`), "members": strings.Index(html, `minicard-members`)}
		for k, v := range at {
			if v < 0 {
				t.Fatalf("the minicard draws its %s", k)
			}
		}
		keys := []string{"dates", "assignees", "members"}
		for i := 0; i < len(keys); i++ {
			for j := i + 1; j < len(keys); j++ {
				if at[keys[j]] < at[keys[i]] {
					keys[i], keys[j] = keys[j], keys[i]
				}
			}
		}
		return strings.Join(keys, " ")
	}
	if got := order(); got != "dates assignees members" {
		t.Fatalf("the default: %s", got)
	}
	if _, out := methodCall(t, w, ac, "/boards/update", `[{"_id":"`+rb.ID+`"},{"$set":{"minicardSettingsSectionOrder":["members"],"minicardFieldOrder":["members","assignee"]}}]`); out["error"] != nil {
		t.Fatalf("the board admin orders the minicard: %v", out)
	}
	if got := order(); got != "members assignees dates" {
		t.Fatalf("the Minicard pane's order: %s", got)
	}
}
