package web

import (
	"context"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// One board report chart as Excel or PDF - Node.js WeKan's
// models/exportCharts.js, with the numbers of models/lib/chartCalculations.js
// and the tables of models/lib/chartExportRows.js. The rule is
// models/lib/exportAccess.js canExportBoardData: a board its reader may see,
// and an assigned-only member only the Scrum charts, which WeKan scopes to
// the cards assigned to them.

// chartTitles are WeKan's chart keys and their titles' translation keys.
var chartTitles = map[string]string{
	"dashboard": msg("board-view-dashboard"), "burndown": msg("board-view-burndown"), "burnup": msg("board-view-burnup"),
	"cumulativeFlow": msg("board-view-cumulative-flow"), "controlChart": msg("board-view-control-chart"),
	"cycleTime": msg("board-view-cycle-time"), "flowEfficiency": msg("board-view-flow-efficiency"),
	"leadTime": msg("board-view-lead-time"), "throughputHistogram": msg("board-view-throughput-histogram"),
	"wipRun": msg("board-view-wip-run"), "gantt": msg("board-view-gantt"), "time": msg("board-view-time"),
	"pulse": msg("board-view-pulse"), "scrumVelocity": msg("board-view-velocity"), "scrumSprint": msg("board-view-sprint-report"),
	"scrumDaily": msg("scrum-daily-observations"), "agingWip": msg("board-view-aging-wip"),
	"blockerAnalysis": msg("board-view-blocker-analysis"), "monteCarlo": msg("board-view-monte-carlo"),
	"processBehavior": msg("board-view-process-behavior"), "sizeCycleTime": msg("board-view-size-cycle-time"),
}

// scrumCharts are scoped to their reader's cards, so assigned-only may export them.
var scrumCharts = []string{"scrumVelocity", "scrumSprint", "scrumDaily"}

func (s *Server) exportChartRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/boards/{board}/charts/{chart}/exportExcel", s.exportable(s.apiExportChart("xlsx")))
	m.HandleFunc("GET /api/boards/{board}/charts/{chart}/exportPDF", s.exportable(s.apiExportChart("pdf")))
}

func (s *Server) apiExportChart(format string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("chart")
		if _, ok := chartTitles[key]; !ok {
			http.Error(w, "Unknown chart", http.StatusBadRequest)
			return
		}
		var b Board
		if id := r.PathValue("board"); !validID(id) || s.db.Collection("boards").FindOne(r.Context(), bson.M{"_id": eq(id)}).Decode(&b) != nil {
			http.Error(w, "Board not found", http.StatusNotFound)
			return
		}
		u, ok := s.exportUser(r)
		if !ok {
			http.Error(w, "Invalid token", http.StatusUnauthorized)
			return
		}
		if b.Permission != "public" {
			if u == nil {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			if m := b.member(u.ID); m == nil || !m.IsActive || (m.AssignedOnly() && !slices.Contains(scrumCharts, key)) {
				http.Error(w, "Unauthorized", http.StatusForbidden)
				return
			}
		}
		ctx := r.Context()
		lang := exportLang(ctx)
		headers, rows, ok := s.chartRows(ctx, &b, key, lang)
		if !ok {
			http.Error(w, "This chart's export is not in Weddy yet.", http.StatusNotImplemented)
			return
		}
		title := tr(lang, chartTitles[key])
		name := "export-chart-" + key + "-" + b.ID
		w.Header().Set("Cache-Control", "no-store")
		if format == "xlsx" {
			out, err := xlsxOf(title, append([][]any{headers}, rows...))
			if err != nil {
				http.Error(w, "Export failed", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", xlsxType)
			w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.xlsx"`)
			_, _ = w.Write(out)
			return
		}
		lines := []string{b.Title + " - " + title, ""}
		for _, row := range append([][]any{headers}, rows...) {
			cells := make([]string, len(row))
			for i, c := range row {
				cells[i] = cellText(c)
			}
			lines = append(lines, strings.Join(cells, "    "))
		}
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.pdf"`)
		_, _ = w.Write(pdfOf(lines))
	}
}

// chartCard is what the charts read of a card.
type chartCard struct {
	ID         string    `bson:"_id"`
	Title      string    `bson:"title"`
	ListID     string    `bson:"listId"`
	CreatedAt  time.Time `bson:"createdAt"`
	StartAt    time.Time `bson:"startAt"`
	DueAt      time.Time `bson:"dueAt"`
	EndAt      time.Time `bson:"endAt"`
	ArchivedAt time.Time `bson:"archivedAt"`
	Assignees  []string  `bson:"assignees"`
	LabelIDs   []string  `bson:"labelIds"`
}

// done is WeKan's completionDate: the end date, else when it was archived.
func (c chartCard) done() (time.Time, bool) {
	if !c.EndAt.IsZero() {
		return c.EndAt, true
	}
	return c.ArchivedAt, !c.ArchivedAt.IsZero()
}

func dayOf(t time.Time) string { return t.UTC().Format("2006-01-02") }

func eachDay(from, to time.Time) []string {
	days := []string{}
	end, _ := time.Parse("2006-01-02", dayOf(to))
	for d, _ := time.Parse("2006-01-02", dayOf(from)); !d.After(end); d = d.AddDate(0, 0, 1) {
		days = append(days, dayOf(d))
	}
	return days
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func sinceDays(a, b time.Time) float64 { return math.Max(0, b.Sub(a).Hours()/24) }

// chartRows are a chart's table; ok false for one Weddy cannot compute yet.
func (s *Server) chartRows(ctx context.Context, b *Board, key, lang string) ([]any, [][]any, bool) {
	if slices.Contains(scrumCharts, key) {
		// Weddy keeps no Scrum sprints yet, so there is no closed sprint
		// to report: WeKan's table, without rows.
		if key == "scrumDaily" {
			return []any{tr(lang, "scrum-sprint"), tr(lang, "date"), "UTC", tr(lang, "scrum-estimate-unit")}, [][]any{}, true
		}
		return []any{tr(lang, "scrum-sprint"), tr(lang, "scrum-estimate-unit")}, [][]any{}, true
	}
	cur, err := s.db.Collection("cards").Find(ctx, bson.M{"boardId": b.ID}, options.Find().SetProjection(bson.M{
		"title": 1, "listId": 1, "createdAt": 1, "startAt": 1, "dueAt": 1, "endAt": 1, "archivedAt": 1, "assignees": 1, "labelIds": 1}))
	if err != nil {
		return nil, nil, false
	}
	var cards []chartCard
	_ = cur.All(ctx, &cards)
	now := time.Now().UTC()
	from := now
	for _, c := range cards {
		if !c.CreatedAt.IsZero() && c.CreatedAt.Before(from) {
			from = c.CreatedAt
		}
	}
	endOf := func(day string) time.Time {
		d, _ := time.Parse("2006-01-02", day)
		return d.Add(24*time.Hour - time.Millisecond)
	}
	type point struct {
		c         chartCard
		at        time.Time
		lead, cyc float64
	}
	points := func() []point {
		out := []point{}
		for _, c := range cards {
			at, ok := c.done()
			if !ok {
				continue
			}
			start := c.StartAt
			if start.IsZero() {
				start = c.CreatedAt
			}
			out = append(out, point{c: c, at: at, lead: sinceDays(c.CreatedAt, at), cyc: sinceDays(start, at)})
		}
		sort.SliceStable(out, func(i, j int) bool { return out[i].at.Before(out[j].at) })
		return out
	}
	titleOf := func(c chartCard) string {
		if c.Title != "" {
			return c.Title
		}
		return c.ID
	}
	iso := func(t time.Time) any {
		if t.IsZero() {
			return "-"
		}
		return t
	}
	switch key {
	case "burndown", "burnup":
		days := eachDay(from, now)
		rows := [][]any{}
		first := endOf(days[0])
		initial := 0
		for _, c := range cards {
			if !c.CreatedAt.After(first) {
				initial++
			}
		}
		for i, day := range days {
			end := endOf(day)
			total, completed, remaining := 0, 0, 0
			for _, c := range cards {
				at, isDone := c.done()
				if !c.CreatedAt.After(end) {
					total++
					if !isDone || at.After(end) {
						remaining++
					}
				}
				if isDone && !at.After(end) {
					completed++
				}
			}
			if key == "burnup" {
				rows = append(rows, []any{day, float64(total), float64(completed)})
				continue
			}
			ideal := 0.0
			if len(days) > 1 {
				ideal = math.Max(0, float64(initial)-float64(initial)*float64(i)/float64(len(days)-1))
			}
			rows = append(rows, []any{day, float64(remaining), round2(ideal)})
		}
		if key == "burnup" {
			return []any{tr(lang, "date"), "Total", tr(lang, "completed")}, rows, true
		}
		return []any{tr(lang, "date"), tr(lang, "board-view-burndown"), "Ideal"}, rows, true
	case "controlChart":
		ps := points()
		rows := [][]any{}
		for i := range ps {
			window := ps[max(0, i-4) : i+1]
			sum := 0.0
			for _, p := range window {
				sum += p.cyc
			}
			avg := sum / float64(len(window))
			variance := 0.0
			for _, p := range window {
				variance += (p.cyc - avg) * (p.cyc - avg)
			}
			rows = append(rows, []any{titleOf(ps[i].c), ps[i].at, round2(ps[i].cyc), round2(avg), round2(math.Sqrt(variance / float64(len(window))))})
		}
		return []any{tr(lang, "card"), tr(lang, "completed"), tr(lang, "board-view-cycle-time"), "Average", "StdDev"}, rows, true
	case "leadTime", "cycleTime":
		rows := [][]any{}
		for _, p := range points() {
			rows = append(rows, []any{titleOf(p.c), p.at, round2(p.lead), round2(p.cyc)})
		}
		return []any{tr(lang, "card"), tr(lang, "completed"), tr(lang, "board-view-lead-time"), tr(lang, "board-view-cycle-time")}, rows, true
	case "throughputHistogram":
		weeks := map[string]int{}
		for _, c := range cards {
			at, ok := c.done()
			if !ok {
				continue
			}
			at = at.UTC()
			weekday := (int(at.Weekday()) + 6) % 7 // Monday = 0
			weeks[dayOf(at.AddDate(0, 0, -weekday))]++
		}
		keys := make([]string, 0, len(weeks))
		for k := range weeks {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		rows := [][]any{}
		for _, k := range keys {
			rows = append(rows, []any{k, float64(weeks[k])})
		}
		return []any{tr(lang, "board-view-throughput-histogram"), tr(lang, "cards")}, rows, true
	case "gantt":
		dated := []chartCard{}
		for _, c := range cards {
			if !c.StartAt.IsZero() || !c.DueAt.IsZero() || !c.EndAt.IsZero() || !c.CreatedAt.IsZero() {
				dated = append(dated, c)
			}
		}
		startOf := func(c chartCard) time.Time {
			if !c.StartAt.IsZero() {
				return c.StartAt
			}
			return c.CreatedAt
		}
		sort.SliceStable(dated, func(i, j int) bool { return startOf(dated[i]).Before(startOf(dated[j])) })
		rows := [][]any{}
		for _, c := range dated {
			rows = append(rows, []any{c.Title, iso(c.StartAt), iso(c.DueAt), iso(c.EndAt)})
		}
		return []any{tr(lang, "card"), tr(lang, "card-start"), tr(lang, "card-due"), tr(lang, "card-end")}, rows, true
	case "pulse":
		to := now
		since := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -29)
		counts := map[string]int{}
		if cur, err := s.db.Collection("activities").Find(ctx, bson.M{"boardId": b.ID, "createdAt": bson.M{"$gte": since, "$lte": to}},
			options.Find().SetProjection(bson.M{"createdAt": 1})); err == nil {
			var acts []struct {
				CreatedAt time.Time `bson:"createdAt"`
			}
			_ = cur.All(ctx, &acts)
			for _, a := range acts {
				counts[dayOf(a.CreatedAt)]++
			}
		}
		rows := [][]any{}
		for _, day := range eachDay(since, to) {
			rows = append(rows, []any{day, float64(counts[day])})
		}
		return []any{tr(lang, "date"), tr(lang, "board-view-pulse")}, rows, true
	case "dashboard":
		return []any{tr(lang, "name"), tr(lang, "cards")}, s.dashboardRows(ctx, b, cards, lang), true
	case "cumulativeFlow", "wipRun":
		return s.flowRows(ctx, b, key, from, now, lang)
	}
	return nil, nil, false
}

// dashboardRows are WeKan's Dashboard export: cards by assignee, label and
// list, each group's count and share.
func (s *Server) dashboardRows(ctx context.Context, b *Board, cards []chartCard, lang string) [][]any {
	ids := bson.A{}
	for _, c := range cards {
		for _, id := range c.Assignees {
			ids = append(ids, id)
		}
	}
	users := []bson.M{}
	if len(ids) > 0 {
		if cur, err := s.db.Collection("users").Find(ctx, bson.M{"_id": dbIn(ids)}, options.Find().SetProjection(bson.M{"username": 1, "profile.fullname": 1})); err == nil {
			_ = cur.All(ctx, &users)
		}
	}
	// A chart export follows Admin Panel -> Anonymize export users too.
	a := s.exportAnonymizer(ctx, b, users)
	names := map[string]string{}
	for _, u := range users {
		a.user(u)
		id, _ := u["_id"].(string)
		name, _ := u["username"].(string)
		if p, ok := u["profile"].(bson.M); ok {
			if f, _ := p["fullname"].(string); f != "" {
				name = f
			}
		} else if p, ok := u["profile"].(bson.D); ok {
			if f, _ := docMap(p)["fullname"].(string); f != "" {
				name = f
			}
		}
		names[id] = name
	}
	labels := map[string]string{}
	for _, l := range b.Labels {
		labels[l.ID] = l.Name
		if l.Name == "" {
			labels[l.ID] = l.Color
		}
	}
	lists := map[string]string{}
	if cur, err := s.db.Collection("lists").Find(ctx, bson.M{"boardId": b.ID, "archived": false}); err == nil {
		var ls []List
		_ = cur.All(ctx, &ls)
		for _, l := range ls {
			lists[l.ID] = l.Title
		}
	}
	type group struct {
		label string
		count int
	}
	section := func(title string, groupsOf func(chartCard) []string, labelOf func(string) string, empty string) [][]any {
		byKey, order := map[string]*group{}, []string{}
		for _, c := range cards {
			keys := groupsOf(c)
			if len(keys) == 0 {
				keys = []string{"\x00"}
			}
			for _, k := range keys {
				if byKey[k] == nil {
					l := empty
					if k != "\x00" {
						l = labelOf(k)
					}
					byKey[k] = &group{label: l}
					order = append(order, k)
				}
				byKey[k].count++
			}
		}
		sort.SliceStable(order, func(i, j int) bool { return byKey[order[i]].count > byKey[order[j]].count })
		total := max(len(cards), 1)
		rows := [][]any{{title, ""}}
		for _, k := range order {
			g := byKey[k]
			pct := math.Round(float64(g.count)/float64(total)*1000) / 10
			rows = append(rows, []any{g.label, strconv.Itoa(g.count) + " (" + strconv.FormatFloat(pct, 'f', -1, 64) + "%)"})
		}
		return append(rows, []any{"", ""})
	}
	or := func(m map[string]string) func(string) string {
		return func(k string) string {
			if v := m[k]; v != "" {
				return v
			}
			return k
		}
	}
	rows := section(tr(lang, "assignees"), func(c chartCard) []string { return c.Assignees }, or(names), tr(lang, "no-assignee"))
	rows = append(rows, section(tr(lang, "labels"), func(c chartCard) []string { return c.LabelIDs }, or(labels), tr(lang, "no-label"))...)
	rows = append(rows, section(tr(lang, "lists"), func(c chartCard) []string { return []string{c.ListID} }, or(lists), "")...)
	return rows
}

// flowRows are the Cumulative Flow Diagram and the WIP Run: the board's
// card activities replayed a day at a time to know which list every card
// sat in (computeCumulativeFlow, computeWipRun). The CFD counts, per list,
// the cards in it or in a list after it; the WIP Run, the cards in the
// lists between the first and the last, against their WIP limits' sum.
func (s *Server) flowRows(ctx context.Context, b *Board, key string, from, now time.Time, lang string) ([]any, [][]any, bool) {
	var lists []bson.M
	if cur, err := s.db.Collection("lists").Find(ctx, bson.M{"boardId": b.ID, "archived": false}, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}})); err == nil {
		_ = cur.All(ctx, &lists)
	}
	var events []struct {
		Type   string    `bson:"activityType"`
		CardID string    `bson:"cardId"`
		ListID string    `bson:"listId"`
		At     time.Time `bson:"createdAt"`
	}
	if cur, err := s.db.Collection("activities").Find(ctx, bson.M{"boardId": b.ID,
		"activityType": bson.M{"$in": bson.A{"createCard", "moveCard", "archivedCard", "restoredCard"}}},
		options.Find().SetSort(bson.D{{Key: "createdAt", Value: 1}})); err == nil {
		_ = cur.All(ctx, &events)
	}
	ids := make([]string, len(lists))
	rank := map[string]int{}
	for i, l := range lists {
		ids[i], _ = l["_id"].(string)
		rank[ids[i]] = i
	}
	wip := map[string]bool{}
	limit := 0.0
	for i, id := range ids {
		if len(ids) <= 2 || (i > 0 && i < len(ids)-1) {
			wip[id] = true
		}
		if len(ids) > 2 && i > 0 && i < len(ids)-1 {
			var w bson.M
			switch d := lists[i]["wipLimit"].(type) {
			case bson.D:
				w = docMap(d)
			case bson.M:
				w = d
			}
			if on, _ := w["enabled"].(bool); on {
				switch v := w["value"].(type) {
				case float64:
					limit += v
				case int32:
					limit += float64(v)
				case int64:
					limit += float64(v)
				}
			}
		}
	}
	at, archived := map[string]string{}, map[string]bool{}
	next := 0
	rows := [][]any{}
	for _, day := range eachDay(from, now) {
		d, _ := time.Parse("2006-01-02", day)
		end := d.Add(24*time.Hour - time.Millisecond)
		for ; next < len(events) && !events[next].At.After(end); next++ {
			e := events[next]
			switch e.Type {
			case "createCard", "moveCard":
				at[e.CardID] = e.ListID
			case "archivedCard":
				archived[e.CardID] = true
			case "restoredCard":
				archived[e.CardID] = false
			}
		}
		if key == "wipRun" {
			n := 0
			for card, list := range at {
				if !archived[card] && wip[list] {
					n++
				}
			}
			var lim any = "-"
			if limit > 0 {
				lim = limit
			}
			rows = append(rows, []any{day, float64(n), lim})
			continue
		}
		counts := make([]float64, len(ids))
		for card, list := range at {
			r, ok := rank[list]
			if archived[card] || !ok {
				continue
			}
			for i := 0; i <= r; i++ {
				counts[i]++
			}
		}
		row := []any{day}
		for _, c := range counts {
			row = append(row, c)
		}
		rows = append(rows, row)
	}
	if key == "wipRun" {
		return []any{tr(lang, "date"), tr(lang, "board-view-wip-run"), "Limit"}, rows, true
	}
	header := []any{tr(lang, "date")}
	for _, l := range lists {
		header = append(header, l["title"])
	}
	return header, rows, true
}
