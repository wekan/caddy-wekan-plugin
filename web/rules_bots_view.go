package web

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// The Rules page's Bots view, as Node.js WeKan's client/components/rules/
// rulesBots.js and rulesBots.jade (docs/Features/Rules/Bots.md): a rail of the
// board's bots, its live runs and their activity; the board's lists as
// columns, each card with its newest run - its state, the step it is on, the
// question it asked and the answers to click; and a card's drawer, its
// comments and its run's events in order, to hand it to a bot, stop the run
// or reply. For the board's admins only, as the rest of the Rules page.

// The run states' words, named so cmd/sync-i18n copies them: the view looks
// them up as "bots-status-" + the run's status.
var botStatusWords = []string{msg("bots-status-queued"), msg("bots-status-running"), msg("bots-status-awaiting_input"),
	msg("bots-status-review"), msg("bots-status-done"), msg("bots-status-failed"), msg("bots-status-stopped")}

type botView struct {
	ID, Title, Host string
	Enabled         bool
}

type botRunView struct {
	ID, Status, StatusKey, Ticker, Question, CostText string
	Active                                            bool
	Options                                           []map[string]string
}

type botCardView struct {
	ID, Title, Number, Age string
	SwimlaneID             string
	Sort                   float64
	TagName, TagColor      string
	Selected               bool
	Run                    *botRunView
}

type botColumnView struct {
	ID, Title string
	Cards     []botCardView
}

type botThreadItem struct {
	Kind, Who, When, Text string
	IsEvent               bool
	at                    time.Time
}

type botLiveView struct {
	CardID, CardTitle, Status, StatusKey, Ticker string
}

type botActivityView struct{ BotTitle, Text string }

type botDrawerView struct {
	ID, Title string
	Run       *botRunView
	Bots      []botView
	Thread    []botThreadItem
}

func botHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// botAge is WeKan's age(): minutes, hours to two days, then days.
func botAge(t time.Time, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	m := int(now.Sub(t).Minutes())
	if m < 0 {
		m = 0
	}
	switch {
	case m < 60:
		return fmt.Sprintf("%d min", m)
	case m < 60*48:
		return fmt.Sprintf("%d h", (m+30)/60)
	}
	return fmt.Sprintf("%d d", (m+720)/1440)
}

func eventText(e bson.M) string {
	var parts []string
	for _, k := range []string{"tool", "text"} {
		if v := docString(e, k); v != "" {
			parts = append(parts, v)
		}
	}
	return strings.Join(parts, ": ")
}

// botRunOf is WeKan's runOf: a run shaped for a card and the drawer.
func botRunOf(run bson.M) *botRunView {
	if run == nil {
		return nil
	}
	events, _ := plain(run["events"]).([]any)
	var last bson.M
	if len(events) > 0 {
		m, _ := events[len(events)-1].(map[string]any)
		last = bson.M(m)
	}
	status := docString(run, "status")
	v := &botRunView{ID: docString(run, "_id"), Status: status, StatusKey: "bots-status-" + status, Question: docString(run, "question"),
		Active: slices.Contains(botActive, status)}
	if status == "running" && last != nil {
		v.Ticker = eventText(last)
	}
	opts, _ := plain(run["options"]).([]any)
	for _, o := range opts {
		m, _ := o.(map[string]any)
		v.Options = append(v.Options, map[string]string{"value": strOf(m["value"]), "label": strOf(m["label"]), "runId": v.ID})
	}
	var cost []string
	if n, ok := plain(run["tokens"]).(float64); ok && n > 0 {
		cost = append(cost, fmt.Sprintf("%d tokens", int64(n)))
	}
	if n, ok := plain(run["costUsd"]).(float64); ok {
		cost = append(cost, fmt.Sprintf("$%.2f", n))
	}
	v.CostText = strings.Join(cost, " · ")
	return v
}

// botsViewData fills the Bots view of a board for one of its admins.
func (s *Server) botsViewData(ctx context.Context, b *Board, selected, lang string, data map[string]any) {
	now := time.Now()
	var hooks []bson.M
	if cur, err := s.db.Collection("integrations").Find(ctx, bson.M{"boardId": b.ID, "bot": true}); err == nil {
		_ = cur.All(ctx, &hooks)
	}
	bots := []botView{}
	titles := map[string]string{}
	for _, h := range hooks {
		title := docString(h, "title")
		if title == "" {
			title = botHost(docString(h, "url"))
		}
		titles[docString(h, "_id")] = title
		bots = append(bots, botView{ID: docString(h, "_id"), Title: title, Host: botHost(docString(h, "url")), Enabled: h["enabled"] != false})
	}
	var runs []bson.M
	if cur, err := s.db.Collection("botRuns").Find(ctx, bson.M{"boardId": b.ID}, options.Find().SetSort(bson.D{{Key: "updatedAt", Value: -1}}).SetLimit(300)); err == nil {
		_ = cur.All(ctx, &runs)
	}
	newest := map[string]bson.M{} // the newest run of each card
	running, waiting := 0, 0
	for _, r := range runs {
		if _, ok := newest[docString(r, "cardId")]; !ok {
			newest[docString(r, "cardId")] = r
		}
		switch docString(r, "status") {
		case "running":
			running++
		case "awaiting_input":
			waiting++
		}
	}
	// Columns: the board's lists and their cards.
	var lists []struct {
		ID    string `bson:"_id"`
		Title string `bson:"title"`
	}
	if cur, err := s.db.Collection("lists").Find(ctx, bson.M{"boardId": b.ID, "archived": bson.M{"$ne": true}}, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}})); err == nil {
		_ = cur.All(ctx, &lists)
	}
	var cards []Card
	if cur, err := s.db.Collection("cards").Find(ctx, bson.M{"boardId": b.ID, "archived": bson.M{"$ne": true}}, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}})); err == nil {
		_ = cur.All(ctx, &cards)
	}
	titleOf := map[string]string{}
	columns := []botColumnView{}
	for _, l := range lists {
		col := botColumnView{ID: l.ID, Title: l.Title, Cards: []botCardView{}}
		for _, c := range cards {
			if c.ListID != l.ID {
				continue
			}
			titleOf[c.ID] = c.Title
			v := botCardView{ID: c.ID, Title: c.Title, Age: botAge(c.CreatedAt, now), Selected: c.ID == selected, Run: botRunOf(newest[c.ID]),
				SwimlaneID: c.SwimlaneID, Sort: c.Sort}
			if c.CardNumber != 0 {
				v.Number = fmt.Sprintf("#%d", c.CardNumber)
			}
			for _, lab := range b.Labels {
				if slices.Contains(c.LabelIDs, lab.ID) {
					v.TagName, v.TagColor = lab.Name, lab.Color
					break
				}
			}
			col.Cards = append(col.Cards, v)
		}
		columns = append(columns, col)
	}
	// The rail's live runs and activity.
	live := []botLiveView{}
	activity := []botActivityView{}
	type stamped struct {
		botActivityView
		at time.Time
	}
	var acts []stamped
	for i, r := range runs {
		status := docString(r, "status")
		events, _ := plain(r["events"]).([]any)
		if slices.Contains(botActive, status) {
			lv := botLiveView{CardID: docString(r, "cardId"), CardTitle: titleOf[docString(r, "cardId")], Status: status, StatusKey: "bots-status-" + status}
			if len(events) > 0 {
				m, _ := events[len(events)-1].(map[string]any)
				lv.Ticker = eventText(bson.M(m))
			}
			live = append(live, lv)
		}
		if i < 20 {
			for _, e := range events {
				m, _ := e.(map[string]any)
				at, _ := m["at"].(time.Time)
				acts = append(acts, stamped{botActivityView{BotTitle: titles[docString(r, "integrationId")], Text: eventText(bson.M(m))}, at})
			}
		}
	}
	sort.SliceStable(acts, func(i, j int) bool { return acts[i].at.After(acts[j].at) })
	for i, a := range acts {
		if i == 30 {
			break
		}
		activity = append(activity, a.botActivityView)
	}
	data["Bots"], data["BotColumns"], data["BotLive"], data["BotActivity"] = bots, columns, live, activity
	data["BotSummary"] = strings.Replace(strings.Replace(tr(lang, "bots-summary"), "%s", fmt.Sprint(running), 1), "%s", fmt.Sprint(waiting), 1)
	// The drawer of the selected card of this board.
	var c Card
	if !validID(selected) || s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(selected), "boardId": b.ID}).Decode(&c) != nil {
		return
	}
	d := botDrawerView{ID: c.ID, Title: c.Title, Run: botRunOf(newest[c.ID])}
	for _, bv := range bots {
		if bv.Enabled {
			d.Bots = append(d.Bots, bv)
		}
	}
	var comments []bson.M
	if cur, err := s.db.Collection("card_comments").Find(ctx, bson.M{"cardId": c.ID}, options.Find().SetSort(bson.D{{Key: "createdAt", Value: 1}})); err == nil {
		_ = cur.All(ctx, &comments)
	}
	for _, cm := range comments {
		at, _ := plain(cm["createdAt"]).(time.Time)
		var u User
		who := ""
		if s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(docString(cm, "userId"))}).Decode(&u) == nil {
			who = u.Username
		}
		d.Thread = append(d.Thread, botThreadItem{Kind: "comment", Who: who, Text: docString(cm, "text"), at: at})
	}
	if run := newest[c.ID]; run != nil {
		events, _ := plain(run["events"]).([]any)
		for _, e := range events {
			m, _ := e.(map[string]any)
			at, _ := m["at"].(time.Time)
			text := strOf(m["text"])
			if tool := strOf(m["tool"]); tool != "" {
				text = strings.TrimSpace("`" + tool + "` " + text)
			}
			d.Thread = append(d.Thread, botThreadItem{Kind: "event", IsEvent: true, Who: tr(lang, "bots"), Text: text, at: at})
		}
	}
	sort.SliceStable(d.Thread, func(i, j int) bool { return d.Thread[i].at.Before(d.Thread[j].at) })
	for i := range d.Thread {
		if !d.Thread[i].at.IsZero() {
			d.Thread[i].When = d.Thread[i].at.Local().Format("2006-01-02 15:04")
		}
	}
	data["BotDrawer"] = d
}
