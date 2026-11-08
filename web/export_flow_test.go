package web

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The same cards as WeKan's flowAnalytics.js was given (.tools/tmp/mc):
// its forecasts and XmR limits, number for number.
func flowCards() []map[string]any {
	cards := []map[string]any{}
	for i := 0; i < 12; i++ {
		created := time.Date(2026, 5, 1+i, 0, 0, 0, 0, time.UTC)
		c := map[string]any{"_id": "c" + string(rune('0'+i/10)) + string(rune('0'+i%10)), "title": "T", "createdAt": created}
		if i%3 != 0 {
			c["endAt"] = created.Add(time.Duration(float64(i+1) * 1.5 * float64(24*time.Hour)))
		}
		cards = append(cards, c)
	}
	return cards
}

func TestMonteCarloIsWeKans(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	o, err := flowOptionsOf(url.Values{"targetCount": {"5"}, "targetDate": {"2026-07-15"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := monteCarlo(flowCards(), o, float64(now.UnixMilli()))
	want := []forecast{{50, 5, 26, true, "2026-07-11"}, {70, 4, 33, true, "2026-07-18"}, {85, 3, 39, true, "2026-07-24"}, {95, 2, 49, true, "2026-08-03"}}
	if len(f) != 4 {
		t.Fatalf("%+v", f)
	}
	for i := range want {
		if f[i] != want[i] {
			t.Errorf("forecast %d: %+v, WeKan's %+v", i, f[i], want[i])
		}
	}
	pts := cyclePoints(flowCards(), float64(now.UnixMilli()))
	sum, rs := 0.0, 0.0
	for i, p := range pts {
		sum += p.cycle
		if i > 0 {
			rs += math.Abs(p.cycle - pts[i-1].cycle)
		}
	}
	if avg, mr := sum/float64(len(pts)), rs/float64(len(pts)-1); math.Abs(avg-10.5) > 1e-9 || math.Abs(mr-2.142857142857143) > 1e-9 {
		t.Errorf("XmR: %v %v", avg, mr)
	}
	for _, bad := range []url.Values{{"targetCount": {"0"}}, {"historyDays": {"6"}}, {"targetDate": {"2020-01-01"}}, {"targetDate": {"2026-02-30"}}} {
		if _, err := flowOptionsOf(bad, now); err == nil {
			t.Errorf("negative: %v is accepted", bad)
		}
	}
}

func TestBlockerEpisodes(t *testing.T) {
	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	cards := []map[string]any{
		{"_id": "a", "title": "Blocker", "boardId": "B", "listId": "L", "createdAt": t0},
		{"_id": "b", "title": "Blocked", "boardId": "B", "listId": "L", "createdAt": t0,
			"cardDependencies": []any{map[string]any{"type": "is-blocked-by", "cardId": "a"}}},
	}
	// The dependency was added on day 2, and is still there.
	history := []map[string]any{{"entityType": "card", "entityId": "b", "createdAt": t0.Add(48 * time.Hour), "group": "dependencies",
		"previousContent": map[string]any{"field": "cardDependencies", "value": []any{}},
		"newContent":      map[string]any{"field": "cardDependencies", "value": []any{map[string]any{"type": "is-blocked-by", "cardId": "a"}}}}}
	e := blockerEpisodes(cards, history, float64(t0.Add(5*24*time.Hour).UnixMilli()))
	if len(e) != 1 || e[0].blocker != "Blocker" || e[0].title != "Blocked" || math.Abs(e[0].days-3) > 1e-9 || finite(e[0].end) {
		t.Fatalf("one open episode of 3 days: %+v", e)
	}
	// Without history the start is unknown, never the card's age.
	if e := blockerEpisodes(cards, nil, float64(t0.Add(5*24*time.Hour).UnixMilli())); len(e) != 1 || finite(e[0].days) {
		t.Errorf("negative: unknown start: %+v", e)
	}
}

func TestExportFlowAnalyticsCharts(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	a := login(t, w, "ann")
	b := newRESTBoard(t, w, a, "Analytics")
	ctx := context.Background()
	done := time.Now().UTC().Add(-48 * time.Hour)
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": b.Card}, bson.M{"$set": bson.M{"createdAt": done.Add(-72 * time.Hour),
		"endAt": done, "poker": bson.M{"estimation": 5}}})
	get := func(chart, query string) (int, string) {
		req, _ := http.NewRequest("GET", w.srv.URL+"/api/boards/"+b.ID+"/charts/"+chart+"/exportExcel"+query, nil)
		req.Header.Set("Authorization", "Bearer "+a)
		res, err := testClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 {
			return res.StatusCode, string(data)
		}
		sheets, _ := readXlsx(data)
		out := []string{}
		for _, r := range sheets[0].Rows {
			out = append(out, strings.Join(r, "|"))
		}
		return 200, strings.Join(out, "\n")
	}
	for chart, want := range map[string]string{"sizeCycleTime": "C|Planning Poker|5|3|", "processBehavior": "Mean moving range",
		"agingWip": "Age in stage", "blockerAnalysis": "Blocked card-days", "monteCarlo": "50%|10|"} {
		if code, got := get(chart, ""); code != 200 || !strings.Contains(got, want) {
			t.Errorf("%s: %d, wants %q: %s", chart, code, want, got)
		}
	}
	if code, _ := get("monteCarlo", "?targetCount=0"); code != 400 {
		t.Errorf("negative: a bad option: %d", code)
	}
	if code, _ := get("sizeCycleTime", "?sizeField=nosuchfield00001"); code != 400 {
		t.Errorf("negative: a size field not on the board: %d", code)
	}
}
