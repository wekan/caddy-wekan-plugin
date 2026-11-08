package web

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// The flow analytics charts - Aging WIP, Blocker Analysis, Monte Carlo,
// Process Behavior (XmR) and Size vs. Cycle Time - as Node.js WeKan's
// models/lib/flowAnalytics.js computes them and flowAnalyticsRows.js tables
// them. A measurement nobody can know stays "Unknown", never zero.

const dayMS = 86400000.0

var flowCharts = []string{"agingWip", "blockerAnalysis", "monteCarlo", "processBehavior", "sizeCycleTime"}

// plain is a decoded document as plain Go values: maps, slices and times.
func plain(v any) any {
	switch x := v.(type) {
	case bson.D:
		m := map[string]any{}
		for _, e := range x {
			m[e.Key] = plain(e.Value)
		}
		return m
	case bson.M:
		m := map[string]any{}
		for k, e := range x {
			m[k] = plain(e)
		}
		return m
	case map[string]any:
		m := map[string]any{}
		for k, e := range x {
			m[k] = plain(e)
		}
		return m
	case bson.A:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = plain(e)
		}
		return out
	case bson.DateTime:
		return x.Time().UTC()
	}
	return v
}

// msOf is a value as epoch milliseconds, NaN for none.
func msOf(v any) float64 {
	switch x := v.(type) {
	case time.Time:
		if x.IsZero() {
			return math.NaN()
		}
		return float64(x.UnixMilli())
	case string:
		if t, ok := cellDate(x); ok {
			return float64(t.UnixMilli())
		}
	}
	return math.NaN()
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

func strOf(v any) string { s, _ := v.(string); return s }

func dayKeyMS(ms float64) string { return time.UnixMilli(int64(ms)).UTC().Format("2006-01-02") }

// doneOf is completion: the end date, else when it was archived.
func doneOf(c map[string]any) any {
	if finite(msOf(c["endAt"])) {
		return c["endAt"]
	}
	return c["archivedAt"]
}

func openCard(c map[string]any) bool {
	return c["archived"] != true && !finite(msOf(c["deletedAt"])) && !finite(msOf(doneOf(c)))
}

func quantile(values []float64, p float64) (float64, bool) {
	if len(values) == 0 {
		return 0, false
	}
	s := slices.Clone(values)
	sort.Float64s(s)
	return s[max(0, int(math.Ceil(p*float64(len(s))))-1)], true
}

type flowOpts struct {
	TargetCount, HistoryDays int
	TargetDate, SizeField    string
}

var isoDay = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// flowOptions are the report's options, checked as WeKan's flowOptions.
func flowOptionsOf(q url.Values, now time.Time) (flowOpts, error) {
	integer := func(key string, fallback, lo, hi int) (int, error) {
		raw := q.Get(key)
		if raw == "" {
			return fallback, nil
		}
		v, err := strconv.Atoi(raw)
		if err != nil || v < lo || v > hi {
			return 0, errors.New("Invalid " + key)
		}
		return v, nil
	}
	today := float64(time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).UnixMilli())
	o := flowOpts{TargetDate: q.Get("targetDate"), SizeField: q.Get("sizeField")}
	if o.TargetDate == "" {
		o.TargetDate = dayKeyMS(today + 30*dayMS)
	}
	t, err := time.Parse("2006-01-02", o.TargetDate)
	if !isoDay.MatchString(o.TargetDate) || err != nil || float64(t.UnixMilli()) < today || float64(t.UnixMilli()) > today+3650*dayMS {
		return o, errors.New("Invalid targetDate")
	}
	if o.SizeField == "" {
		o.SizeField = "poker"
	}
	if len(o.SizeField) > 100 {
		return o, errors.New("Invalid sizeField")
	}
	if o.TargetCount, err = integer("targetCount", 10, 1, 10000); err != nil {
		return o, err
	}
	if o.HistoryDays, err = integer("historyDays", 90, 7, 365); err != nil {
		return o, err
	}
	return o, nil
}

type flowEvent struct {
	Type, CardID, ListID, OldListID string
	At                              float64
}

// flowData is what the flow charts read of a board.
func (s *Server) flowData(ctx context.Context, b *Board, key string) ([]map[string]any, []bson.M, []flowEvent, []map[string]any) {
	var raw []bson.M
	if cur, err := s.db.Collection("cards").Find(ctx, bson.M{"boardId": b.ID}); err == nil {
		_ = cur.All(ctx, &raw)
	}
	var lists []bson.M
	if cur, err := s.db.Collection("lists").Find(ctx, bson.M{"boardId": b.ID, "archived": false}, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}})); err == nil {
		_ = cur.All(ctx, &lists)
	}
	var hist []bson.M
	if cur, err := s.db.Collection("changeHistory").Find(ctx, bson.M{"boardId": b.ID, "entityType": "card",
		"group": bson.M{"$in": bson.A{"position", "dates", "lifecycle", "dependencies"}}}); err == nil {
		_ = cur.All(ctx, &hist)
	}
	history := make([]map[string]any, len(hist))
	for i, h := range hist {
		history[i] = plain(h).(map[string]any)
	}
	cards := []map[string]any{}
	seen := map[string]bool{}
	for _, c := range raw {
		m := plain(c).(map[string]any)
		cards = append(cards, m)
		seen[strOf(m["_id"])] = true
	}
	// withRemovedCards: the board's recorded deletions, the newest snapshot.
	sorted := slices.Clone(history)
	sort.SliceStable(sorted, func(i, j int) bool { return msOf(sorted[i]["createdAt"]) > msOf(sorted[j]["createdAt"]) })
	for _, row := range sorted {
		prev, _ := row["previousContent"].(map[string]any)
		doc, _ := prev["document"].(map[string]any)
		if row["group"] != "lifecycle" || row["newContent"] != nil || doc == nil || strOf(doc["boardId"]) != b.ID ||
			strOf(doc["_id"]) != strOf(row["entityId"]) || seen[strOf(doc["_id"])] {
			continue
		}
		d := map[string]any{}
		for k, v := range doc {
			d[k] = v
		}
		d["deletedAt"] = row["createdAt"]
		cards = append(cards, d)
		seen[strOf(doc["_id"])] = true
	}
	events := []flowEvent{}
	if key == "agingWip" || key == "blockerAnalysis" {
		var acts []bson.M
		if cur, err := s.db.Collection("activities").Find(ctx, bson.M{"boardId": b.ID,
			"activityType": bson.M{"$in": bson.A{"createCard", "moveCard", "moveCardBoard", "archivedCard", "restoredCard"}}}); err == nil {
			_ = cur.All(ctx, &acts)
		}
		for _, a := range acts {
			events = append(events, flowEvent{strOf(a["activityType"]), strOf(a["cardId"]), strOf(a["listId"]), strOf(a["oldListId"]), msOf(plain(a["createdAt"]))})
		}
		// The universal position history also counts, restores included.
		for _, row := range history {
			if row["group"] != "position" {
				continue
			}
			n, _ := row["newContent"].(map[string]any)
			p, _ := row["previousContent"].(map[string]any)
			events = append(events, flowEvent{"moveCard", strOf(row["entityId"]), strOf(n["listId"]), strOf(p["listId"]), msOf(row["createdAt"])})
		}
	}
	return cards, lists, events, history
}

// flowRows are one flow chart's table and its details.
func (s *Server) flowAnalyticsRows(ctx context.Context, b *Board, key string, q url.Values, now time.Time, lang string) ([]any, [][]any, error) {
	o, err := flowOptionsOf(q, now)
	if err != nil {
		return nil, nil, err
	}
	cards, lists, events, history := s.flowData(ctx, b, key)
	unknown := tr(lang, "flow-unknown")
	number := func(v float64, ok bool) any {
		if !ok || !finite(v) {
			return unknown
		}
		return round2(v)
	}
	listName := map[string]string{}
	for _, l := range lists {
		listName[strOf(l["_id"])] = strOf(l["title"])
	}
	nowMS := float64(now.UnixMilli())
	switch key {
	case "agingWip":
		head := []any{tr(lang, "card"), tr(lang, "list"), tr(lang, "flow-age-days"), tr(lang, "flow-p85"), tr(lang, "flow-samples"), tr(lang, "flow-signal")}
		return head, agingRows(cards, listName, events, nowMS, number, tr(lang, "flow-unusual")), nil
	case "blockerAnalysis":
		episodes := blockerEpisodes(cards, history, nowMS)
		head := []any{tr(lang, "flow-blocker"), tr(lang, "flow-episodes"), tr(lang, "flow-active"), tr(lang, "flow-blocked-days"),
			tr(lang, "flow-unknown-start"), tr(lang, "cards"), tr(lang, "lists")}
		rows := blockerGroupRows(episodes, listName, number)
		rows = append(rows, []any{}, []any{tr(lang, "flow-blocker"), tr(lang, "card"), tr(lang, "list"), tr(lang, "card-start"), tr(lang, "card-end"), tr(lang, "days")})
		for _, e := range episodes {
			l := listName[e.listID]
			if l == "" {
				l = e.listID
			}
			var start, end any = unknown, tr(lang, "flow-active")
			if finite(e.start) {
				start = time.UnixMilli(int64(e.start)).UTC()
			}
			if finite(e.end) {
				end = time.UnixMilli(int64(e.end)).UTC()
			}
			rows = append(rows, []any{e.blocker, e.title, l, start, end, number(e.days, finite(e.days))})
		}
		return head, rows, nil
	case "monteCarlo":
		forecasts, hist := monteCarlo(cards, o, nowMS)
		head := []any{tr(lang, "flow-confidence"), tr(lang, "flow-target-count"), tr(lang, "flow-finish-days"), tr(lang, "flow-finish-date"),
			tr(lang, "flow-target-date"), tr(lang, "flow-capacity"), tr(lang, "flow-history-days")}
		rows := [][]any{}
		for _, f := range forecasts {
			var date any = tr(lang, "flow-beyond-horizon")
			if f.date != "" {
				date = f.date
			}
			rows = append(rows, []any{strconv.Itoa(f.probability) + "%", float64(o.TargetCount), number(f.days, f.ok), date, o.TargetDate, float64(f.count), float64(len(hist))})
		}
		if len(hist) > 0 {
			rows = append(rows, []any{}, []any{tr(lang, "date"), tr(lang, "completed")})
			for _, h := range hist {
				rows = append(rows, []any{h.Day, float64(h.Count)})
			}
		}
		return head, rows, nil
	}
	points := cyclePoints(cards, nowMS)
	if key == "processBehavior" {
		head := []any{tr(lang, "card"), tr(lang, "completed"), tr(lang, "flow-cycle-days"), tr(lang, "flow-mean"), "LCL", "UCL",
			tr(lang, "flow-moving-range"), tr(lang, "flow-mr-mean"), "MR UCL", tr(lang, "flow-signal")}
		rows := [][]any{}
		if len(points) < 2 {
			return head, rows, nil
		}
		ranges := []float64{}
		sum, rsum := 0.0, 0.0
		for i, p := range points {
			sum += p.cycle
			if i > 0 {
				r := math.Abs(p.cycle - points[i-1].cycle)
				ranges = append(ranges, r)
				rsum += r
			}
		}
		avg, mr := sum/float64(len(points)), rsum/float64(len(ranges))
		// NIST e-Handbook 6.3.2.2: d2 = 1.128, D4 = 3.267.
		lower, upper, mrUpper := avg-3*mr/1.128, avg+3*mr/1.128, 3.267*mr
		for i, p := range points {
			var mrv float64
			if i > 0 {
				mrv = ranges[i-1]
			}
			signal := ""
			if p.cycle < lower || p.cycle > upper || (i > 0 && ranges[i-1] > mrUpper) {
				signal = tr(lang, "flow-unusual")
			}
			rows = append(rows, []any{p.title, time.UnixMilli(int64(p.done)).UTC(), round2(p.cycle), round2(avg), round2(lower), round2(upper),
				number(mrv, i > 0), round2(mr), round2(mrUpper), signal})
		}
		return head, rows, nil
	}
	// sizeCycleTime: Planning Poker's estimate, or a number custom field.
	head := []any{tr(lang, "card"), tr(lang, "flow-size-source"), tr(lang, "flow-size"), tr(lang, "flow-cycle-days"), tr(lang, "completed")}
	source := tr(lang, "poker-question")
	if o.SizeField != "poker" {
		var f bson.M
		if !validID(o.SizeField) || s.db.Collection("customFields").FindOne(ctx, bson.M{"_id": eq(o.SizeField), "boardIds": b.ID, "type": "number"}).Decode(&f) != nil {
			return nil, nil, errors.New("Invalid sizeField")
		}
		source = strOf(f["name"])
	}
	byID := map[string]map[string]any{}
	for _, c := range cards {
		byID[strOf(c["_id"])] = c
	}
	rows := [][]any{}
	for _, p := range points {
		c := byID[p.id]
		var v any
		if o.SizeField == "poker" {
			if pk, ok := c["poker"].(map[string]any); ok {
				v = pk["estimation"]
			}
		} else if fs, ok := c["customFields"].([]any); ok {
			for _, f := range fs {
				if fm, ok := f.(map[string]any); ok && strOf(fm["_id"]) == o.SizeField {
					v = fm["value"]
				}
			}
		}
		var size float64
		switch x := v.(type) {
		case float64:
			size = x
		case int32:
			size = float64(x)
		case int64:
			size = float64(x)
		case string:
			f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
			if err != nil || x == "" {
				continue
			}
			size = f
		default:
			continue
		}
		if !finite(size) || size < 0 {
			continue
		}
		rows = append(rows, []any{p.title, source, size, round2(p.cycle), time.UnixMilli(int64(p.done)).UTC()})
	}
	return head, rows, nil
}

type cyclePoint struct {
	id, title   string
	done, cycle float64
}

func cyclePoints(cards []map[string]any, now float64) []cyclePoint {
	out := []cyclePoint{}
	for _, c := range cards {
		end := msOf(doneOf(c))
		start := msOf(c["startAt"])
		if !finite(start) {
			start = msOf(c["createdAt"])
		}
		if !finite(end) || !finite(start) || end < start || end > now {
			continue
		}
		out = append(out, cyclePoint{strOf(c["_id"]), strOf(c["title"]), end, (end - start) / dayMS})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].done != out[j].done {
			return out[i].done < out[j].done
		}
		return out[i].id < out[j].id
	})
	return out
}

// agingRows is stageHistory: each open card's age in its list, against the
// 85th percentile of the time cards stayed in that list before.
func agingRows(cards []map[string]any, listName map[string]string, events []flowEvent, now float64, number func(float64, bool) any, unusual string) [][]any {
	type entry struct {
		list string
		at   float64
	}
	entries := map[string]entry{}
	samples := map[string][]float64{}
	sorted := []flowEvent{}
	for _, e := range events {
		if finite(e.At) && e.At <= now {
			sorted = append(sorted, e)
		}
	}
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].At < sorted[j].At })
	for _, e := range sorted {
		prev, had := entries[e.CardID]
		if e.Type == "archivedCard" || e.Type == "moveCardBoard" {
			delete(entries, e.CardID)
			continue
		}
		if !slices.Contains([]string{"createCard", "moveCard", "restoredCard"}, e.Type) {
			continue
		}
		if e.Type == "moveCard" && e.OldListID == e.ListID {
			continue
		}
		if had && prev.list == e.ListID {
			continue
		}
		if had && e.Type == "moveCard" && e.OldListID == prev.list {
			samples[prev.list] = append(samples[prev.list], (e.At-prev.at)/dayMS)
		}
		entries[e.CardID] = entry{e.ListID, e.At}
	}
	type point struct {
		row []any
		age float64
	}
	pts := []point{}
	for _, c := range cards {
		list := strOf(c["listId"])
		if _, ok := listName[list]; !ok || !openCard(c) {
			continue
		}
		age, hasAge := 0.0, false
		if e, ok := entries[strOf(c["_id"])]; ok && e.list == list && e.at >= msOf(c["createdAt"]) {
			age, hasAge = (now-e.at)/dayMS, true
		}
		vals := samples[list]
		threshold, hasT := 0.0, false
		if len(vals) >= 5 {
			threshold, hasT = quantile(vals, 0.85)
		}
		signal := ""
		if hasAge && hasT && age > threshold {
			signal = unusual
		}
		sortAge := -1.0
		if hasAge {
			sortAge = age
		}
		pts = append(pts, point{[]any{strOf(c["title"]), listName[list], number(age, hasAge), number(threshold, hasT), float64(len(vals)), signal}, sortAge})
	}
	sort.SliceStable(pts, func(i, j int) bool { return pts[i].age > pts[j].age })
	rows := make([][]any, len(pts))
	for i, p := range pts {
		rows[i] = p.row
	}
	return rows
}

type blockerEpisode struct {
	blockerID, blocker, cardID, title, listID string
	start, end, days                          float64
}

// blockerEpisodes replays WeKan's change history - current values rewound
// first, then every row applied in time order - to find when a card was
// blocked by another and for how long. A start that was never recorded
// stays unknown.
func blockerEpisodes(cards []map[string]any, history []map[string]any, now float64) []blockerEpisode {
	states := map[string]map[string]any{}
	for _, c := range cards {
		st := map[string]any{}
		for k, v := range c {
			st[k] = v
		}
		states[strOf(c["_id"])] = st
	}
	relevant := []string{"cardDependencies", "endAt", "archivedAt", "archived", "deletedAt"}
	fieldOf := func(row map[string]any) string {
		n, _ := row["newContent"].(map[string]any)
		p, _ := row["previousContent"].(map[string]any)
		if f := strOf(n["field"]); f != "" {
			return f
		}
		return strOf(p["field"])
	}
	rows := []map[string]any{}
	for _, row := range history {
		p, _ := row["previousContent"].(map[string]any)
		_, isDoc := p["document"].(map[string]any)
		if row["entityType"] != "card" || states[strOf(row["entityId"])] == nil || !finite(msOf(row["createdAt"])) || msOf(row["createdAt"]) > now {
			continue
		}
		if row["group"] == "position" || (row["group"] == "lifecycle" && isDoc) || slices.Contains(relevant, fieldOf(row)) {
			rows = append(rows, row)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return msOf(rows[i]["createdAt"]) < msOf(rows[j]["createdAt"]) })
	apply := func(row map[string]any, content any) {
		st := states[strOf(row["entityId"])]
		c, _ := content.(map[string]any)
		p, _ := row["previousContent"].(map[string]any)
		if _, isDoc := p["document"].(map[string]any); row["group"] == "lifecycle" && isDoc {
			if doc, ok := c["document"].(map[string]any); ok {
				for k, v := range doc {
					st[k] = v
				}
				st["deletedAt"] = doc["deletedAt"]
			} else {
				st["deletedAt"] = row["createdAt"]
			}
			return
		}
		if row["group"] == "position" {
			if v := strOf(c["listId"]); v != "" {
				st["listId"] = v
			}
			if v := strOf(c["boardId"]); v != "" {
				st["boardId"] = v
			}
			return
		}
		v := c["value"]
		if c["isDate"] == true {
			if t, ok := cellDate(strOf(v)); ok {
				v = t
			}
		}
		st[fieldOf(row)] = v
	}
	for i := len(rows) - 1; i >= 0; i-- {
		apply(rows[i], rows[i]["previousContent"])
	}
	edges := func() map[string]blockerEpisode {
		out := map[string]blockerEpisode{}
		for _, src := range states {
			deps, _ := src["cardDependencies"].([]any)
			for _, d := range deps {
				dep, _ := d.(map[string]any)
				typ := strOf(dep["type"])
				if typ != "blocks" && typ != "is-blocked-by" {
					continue
				}
				target := states[strOf(dep["cardId"])]
				if target == nil || strOf(target["_id"]) == strOf(src["_id"]) || strOf(target["boardId"]) != strOf(src["boardId"]) {
					continue
				}
				cause, card := src, target
				if typ == "is-blocked-by" {
					cause, card = target, src
				}
				if !openCard(cause) || !openCard(card) {
					continue
				}
				key, _ := json.Marshal([]string{strOf(cause["_id"]), strOf(card["_id"]), strOf(card["listId"])})
				out[string(key)] = blockerEpisode{blockerID: strOf(cause["_id"]), blocker: strOf(cause["title"]),
					cardID: strOf(card["_id"]), title: strOf(card["title"]), listID: strOf(card["listId"])}
			}
		}
		return out
	}
	active := map[string]blockerEpisode{}
	order := []string{}
	for k, e := range edges() {
		e.start = math.NaN()
		active[k] = e
		order = append(order, k)
	}
	sort.Strings(order)
	episodes := []blockerEpisode{}
	for _, row := range rows {
		apply(row, row["newContent"])
		next := edges()
		at := msOf(row["createdAt"])
		kept := []string{}
		for _, k := range order {
			if _, ok := next[k]; !ok {
				e := active[k]
				e.end = at
				episodes = append(episodes, e)
				delete(active, k)
			} else {
				kept = append(kept, k)
			}
		}
		order = kept
		keys := []string{}
		for k := range next {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if _, ok := active[k]; !ok {
				e := next[k]
				e.start = at
				active[k] = e
				order = append(order, k)
			}
		}
	}
	for _, k := range order {
		e := active[k]
		e.end = math.NaN()
		episodes = append(episodes, e)
	}
	for i := range episodes {
		e := &episodes[i]
		e.days = math.NaN()
		if finite(e.start) {
			end := e.end
			if !finite(end) {
				end = now
			}
			e.days = math.Max(0, (end-e.start)/dayMS)
		}
	}
	return episodes
}

// blockerGroupRows are historicalBlockerGroups: per blocking card, its
// episodes, the open ones, the blocked card-days, the unknown starts, and
// the cards and lists it blocked.
func blockerGroupRows(episodes []blockerEpisode, listName map[string]string, number func(float64, bool) any) [][]any {
	type group struct {
		title                 string
		cards, lists          []string
		count, active, unknwn int
		days                  float64
	}
	groups, order := map[string]*group{}, []string{}
	for _, e := range episodes {
		g := groups[e.blockerID]
		if g == nil {
			g = &group{title: e.blocker}
			groups[e.blockerID] = g
			order = append(order, e.blockerID)
		}
		if !slices.Contains(g.cards, e.title) {
			g.cards = append(g.cards, e.title)
		}
		l := listName[e.listID]
		if l == "" {
			l = e.listID
		}
		if !slices.Contains(g.lists, l) {
			g.lists = append(g.lists, l)
		}
		g.count++
		if !finite(e.end) {
			g.active++
		}
		if finite(e.days) {
			g.days += e.days
		} else {
			g.unknwn++
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := groups[order[i]], groups[order[j]]
		if a.days != b.days {
			return a.days > b.days
		}
		return a.count > b.count
	})
	rows := [][]any{}
	for _, id := range order {
		g := groups[id]
		rows = append(rows, []any{g.title, float64(g.count), float64(g.active), number(g.days, true), float64(g.unknwn),
			strings.Join(g.cards, ", "), strings.Join(g.lists, ", ")})
	}
	return rows
}

type forecast struct {
	probability, count int
	days               float64
	ok                 bool
	date               string
}

type historyDay struct {
	Day   string `json:"day"`
	Count int    `json:"count"`
}

// monteCarlo is WeKan's reproducible bootstrap: 2000 trials drawing days of
// the board's own completed-card history, seeded from the data and the
// options, so the same board on the same UTC day gives the same forecast.
func monteCarlo(cards []map[string]any, o flowOpts, now float64) ([]forecast, []historyDay) {
	today := float64(time.UnixMilli(int64(now)).UTC().Truncate(24 * time.Hour).UnixMilli())
	first := math.Inf(1)
	for _, c := range cards {
		if t := msOf(c["createdAt"]); finite(t) && t <= today {
			first = math.Min(first, t)
		}
	}
	if math.IsInf(first, 1) {
		return nil, nil
	}
	firstDay := float64(time.UnixMilli(int64(first)).UTC().Truncate(24 * time.Hour).UnixMilli())
	from := math.Max(firstDay, today-float64(o.HistoryDays)*dayMS)
	hist := []historyDay{}
	for at := from; at < today; at += dayMS {
		hist = append(hist, historyDay{dayKeyMS(at), 0})
	}
	completed := false
	for _, c := range cards {
		at := msOf(doneOf(c))
		if finite(at) && at >= from && at < today && at >= msOf(c["createdAt"]) {
			hist[int(math.Floor((at-from)/dayMS))].Count++
			completed = true
		}
	}
	if len(hist) == 0 || !completed {
		return nil, hist
	}
	sig, _ := json.Marshal([]any{hist, o.TargetCount, o.TargetDate})
	seed := uint32(2166136261)
	for _, r := range string(sig) {
		// JavaScript's charCodeAt: UTF-16 code units.
		for _, u := range utf16Units(r) {
			seed = (seed ^ uint32(u)) * 16777619
		}
	}
	random := func() float64 { seed = seed*1664525 + 1013904223; return float64(seed) / 4294967296 }
	target, _ := time.Parse("2006-01-02", o.TargetDate)
	horizon := int(math.Round((float64(target.UnixMilli()) - today) / dayMS))
	const trials = 2000
	finish := make([]float64, 0, trials)
	capacity := make([]float64, 0, trials)
	for range trials {
		total, byDate := 0, 0
		finished := math.Inf(1)
		for elapsed := 1; elapsed <= 3650; elapsed++ {
			total += hist[int(math.Floor(random()*float64(len(hist))))].Count
			if elapsed == horizon {
				byDate = total
			}
			if math.IsInf(finished, 1) && total >= o.TargetCount {
				finished = float64(elapsed)
			}
			if elapsed >= horizon && !math.IsInf(finished, 1) {
				break
			}
		}
		finish = append(finish, finished)
		capacity = append(capacity, float64(byDate))
	}
	sort.Float64s(capacity)
	out := []forecast{}
	for _, p := range []int{50, 70, 85, 95} {
		d, _ := quantile(finish, float64(p)/100)
		f := forecast{probability: p, count: int(capacity[int(math.Floor((1-float64(p)/100)*trials))])}
		if !math.IsInf(d, 1) {
			f.days, f.ok, f.date = d, true, dayKeyMS(today+d*dayMS)
		}
		out = append(out, f)
	}
	return out, hist
}

func utf16Units(r rune) []uint16 {
	if r < 0x10000 {
		return []uint16{uint16(r)}
	}
	r -= 0x10000
	return []uint16{uint16(0xD800 + (r >> 10)), uint16(0xDC00 + (r & 0x3FF))}
}
