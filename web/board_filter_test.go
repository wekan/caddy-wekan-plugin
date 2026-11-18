package web

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The Filter sidebar's card text and label filters (#3361, #2044, #2886), as
// WeKan's client/lib/filter.js: the board is drawn with the cards they match.
func TestBoardTextAndLabelFilters(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	u := w.seedUser(t, "ann", "pw")
	b, _ := w.s.CreateBoard(ctx, u, "Filters")
	l, _ := w.s.AddList(ctx, b, "One")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"labels": bson.A{
		bson.M{"_id": "la", "name": "Audit A", "color": "red"}, bson.M{"_id": "lb", "name": "Audit B", "color": "blue"}}}})
	card := func(title string, labels ...string) string {
		c, err := w.s.AddCard(ctx, b, u, l.ID, sw.ID, title)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": c.ID}, bson.M{"$set": bson.M{"labelIds": labels}})
		return c.ID
	}
	card("Alpha Card", "la", "lb")
	card("Beta Card", "la")
	gamma := card("Gamma (x)")
	_, _ = w.db.Collection("checklists").InsertOne(ctx, bson.M{"_id": randomID(), "cardId": gamma, "boardId": b.ID, "title": "Needle list"})
	cl := w.client(t)
	body(t, w.signIn(t, cl, "ann", "pw"))
	shown := func(q url.Values) []string {
		res, err := cl.Get(w.srv.URL + "/b/" + b.ID + "/x?" + q.Encode())
		if err != nil {
			t.Fatal(err)
		}
		h := body(t, res)
		var out []string
		for _, title := range []string{"Alpha Card", "Beta Card", "Gamma (x)"} {
			if strings.Contains(h, ">"+title+"<") || strings.Contains(h, title) && strings.Contains(h, "minicard-title") && strings.Contains(h, "minicard-title\">"+title) {
				out = append(out, title)
			}
		}
		return out
	}
	eq := func(name string, q url.Values, want ...string) {
		t.Helper()
		if got := shown(q); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("%s: shown %v, want %v", name, got, want)
		}
	}
	eq("no filter", url.Values{}, "Alpha Card", "Beta Card", "Gamma (x)")
	eq("title text, any case", url.Values{"text": {"alpha"}}, "Alpha Card")
	eq("a checklist's title", url.Values{"text": {"needle"}}, "Gamma (x)")
	eq("regex characters are text", url.Values{"text": {"(x)"}}, "Gamma (x)")
	eq("negative: matches nothing", url.Values{"text": {"no-card-matches"}})
	eq("OR labels", url.Values{"labels": {"la,lb"}}, "Alpha Card", "Beta Card")
	eq("AND labels", url.Values{"labels": {"la,lb"}, "labelMode": {"and"}}, "Alpha Card")
	eq("no label", url.Values{"labels": {"__none__"}}, "Gamma (x)")
	eq("exclusion wins over a chosen label", url.Values{"labels": {"la"}, "xlabels": {"lb"}}, "Beta Card")
	eq("exclusion alone", url.Values{"xlabels": {"lb"}}, "Beta Card", "Gamma (x)")
	eq("text and labels together", url.Values{"text": {"card"}, "labels": {"la"}, "xlabels": {"lb"}}, "Beta Card")
	// Negative: only the board's own labels are a filter; an unknown id or an
	// operator in the URL changes nothing.
	eq("unknown label", url.Values{"labels": {"nope,$where"}}, "Alpha Card", "Beta Card", "Gamma (x)")
	// The sidebar shows what is applied, so reopening keeps it.
	res, _ := cl.Get(w.srv.URL + "/b/" + b.ID + "/x?text=Alpha&labels=la&xlabels=lb&labelMode=and")
	h := body(t, res)
	if strings.Contains(h, `value="or" selected`) {
		t.Error("negative: only the applied mode is selected")
	}
	for _, want := range []string{`class="js-field-card-filter" type="text" maxlength="512" value="Alpha"`, `data-filter-id="la" data-state="in"`, `data-filter-id="lb" data-state="out"`, `value="and" selected`} {
		if !strings.Contains(h, want) {
			t.Errorf("the sidebar keeps the filter: %s", want)
		}
	}
	f := parseBoardFilter(url.Values{"text": {strings.Repeat("é", 600)}}, b)
	if len([]rune(f.Text)) != maxFilterText {
		t.Fatalf("the text is cut at %d characters: %d", maxFilterText, len([]rune(f.Text)))
	}
}
