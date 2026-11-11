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
		// ExporterChartExcel/PDF: assertFieldExport, as every export.
		if !s.fieldExportAllowed(ctx, &b, u) {
			http.Error(w, errFieldExport, http.StatusForbidden)
			return
		}
		lang := exportLang(ctx)
		headers, rows, ok := s.chartRows(ctx, &b, key, lang)
		title := tr(lang, chartTitles[key])
		var notices []string
		if key == "scrumDaily" {
			var err error
			if headers, rows, notices, err = s.scrumDailyChartRows(ctx, u, &b, lang, r.URL.Query().Get("sprintId")); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		if key == "scrumVelocity" || key == "scrumSprint" {
			var err error
			if title, headers, rows, err = s.scrumChartRows(ctx, u, &b, key, lang, r.URL.Query().Get("sprintId"), title); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		if slices.Contains(flowCharts, key) {
			var err error
			if headers, rows, err = s.flowAnalyticsRows(ctx, &b, key, r.URL.Query(), time.Now().UTC(), lang); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			ok = true
		}
		if !ok {
			http.Error(w, "This chart's export is not in Weddy yet.", http.StatusNotImplemented)
			return
		}
		name := "export-chart-" + key + "-" + b.ID
		w.Header().Set("Cache-Control", "no-store")
		if format == "xlsx" {
			out, err := chartXlsx(b.Title, title, headers, rows, notices)
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
		if len(notices) > 0 {
			lines = append(append(lines, ""), notices...)
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
	SpentTime  float64   `bson:"spentTime"`
	IsOvertime bool      `bson:"isOvertime"`
	Archived   bool      `bson:"archived"`
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
		// Their rows are the reader's (scrumChartRows, scrumDailyChartRows).
		return nil, nil, true
	}
	cur, err := s.db.Collection("cards").Find(ctx, bson.M{"boardId": b.ID}, options.Find().SetProjection(bson.M{
		"title": 1, "listId": 1, "createdAt": 1, "startAt": 1, "dueAt": 1, "endAt": 1, "archivedAt": 1, "assignees": 1, "labelIds": 1,
		"spentTime": 1, "isOvertime": 1, "archived": 1}))
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
	case "flowEfficiency":
		rows := [][]any{}
		for _, c := range cards {
			at, ok := c.done()
			if !ok || c.SpentTime == 0 {
				continue
			}
			start := c.StartAt
			if start.IsZero() {
				start = c.CreatedAt
			}
			total := math.Max(at.Sub(start).Hours(), c.SpentTime)
			eff := 0.0
			if total > 0 {
				eff = math.Min(100, c.SpentTime/total*100)
			}
			rows = append(rows, []any{titleOf(c), round2(eff), round2(c.SpentTime), round2(total)})
		}
		return []any{tr(lang, "card"), tr(lang, "board-view-flow-efficiency"), "Active (h)", "Total (h)"}, rows, true
	case "time":
		return []any{tr(lang, "name"), tr(lang, "hours")}, s.timeRows(ctx, b, cards, now, lang), true
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

// timeRows are WeKan's Time export (#812, #1121): the remaining time until
// due of the open cards, the time adjustments by author (from WeKan's change
// history), then the hours by assignee and by card - archived cards left out.
func (s *Server) timeRows(ctx context.Context, b *Board, cards []chartCard, now time.Time, lang string) [][]any {
	active := []chartCard{}
	for _, c := range cards {
		if !c.Archived {
			active = append(active, c)
		}
	}
	section := func(name string, rows [][]any) [][]any {
		return append(append([][]any{{name, ""}}, rows...), []any{"", ""})
	}
	// Remaining: due minus now over the open cards with a due date.
	ms := 0.0
	for _, c := range active {
		if _, done := c.done(); !done && !c.DueAt.IsZero() {
			ms += float64(c.DueAt.Sub(now).Milliseconds())
		}
	}
	hours := ms / 3600000
	abs := math.Round(math.Abs(hours))
	days, rest := math.Floor(abs/24), abs-math.Floor(abs/24)*24
	sign := ""
	if hours < 0 && abs > 0 {
		sign = "-"
	}
	remaining := sign + strconv.FormatFloat(days, 'f', -1, 64) + " " + tr(lang, "days") + ", " + strconv.FormatFloat(rest, 'f', -1, 64) + " " + tr(lang, "hours")
	label := tr(lang, "board-status-remaining-time-total")
	rows := section(label, [][]any{{label, remaining}})
	// Adjustments by author.
	var history []struct {
		UserID   string    `bson:"userId"`
		Restored string    `bson:"restoredByUserId"`
		EntityID string    `bson:"entityId"`
		At       time.Time `bson:"createdAt"`
		Prev     bson.M    `bson:"previousContent"`
		New      bson.M    `bson:"newContent"`
	}
	if cur, err := s.db.Collection("changeHistory").Find(ctx, bson.M{"boardId": b.ID, "entityType": "card",
		"$or": bson.A{bson.M{"newContent.field": "spentTime"}, bson.M{"previousContent.field": "spentTime"}}}); err == nil {
		_ = cur.All(ctx, &history)
	}
	num := func(m bson.M) (float64, bool) {
		switch v := m["value"].(type) {
		case nil:
			return 0, true
		case float64:
			return v, true
		case int32:
			return float64(v), true
		case int64:
			return float64(v), true
		}
		return 0, false
	}
	type adjustment struct {
		card, user string
		at         time.Time
		delta, sum float64
	}
	entries := []adjustment{}
	byAuthor, authors := map[string]float64{}, []string{}
	ids := bson.A{}
	for _, h := range history {
		if h.Restored != "" && h.UserID != h.Restored {
			continue
		}
		f, _ := h.New["field"].(string)
		if f == "" {
			f, _ = h.Prev["field"].(string)
		}
		before, ok1 := num(h.Prev)
		after, ok2 := num(h.New)
		if f != "spentTime" || !ok1 || !ok2 || after == before {
			continue
		}
		if _, seen := byAuthor[h.UserID]; !seen {
			authors = append(authors, h.UserID)
			ids = append(ids, h.UserID)
		}
		byAuthor[h.UserID] += after - before
		entries = append(entries, adjustment{h.EntityID, h.UserID, h.At, after - before, after})
	}
	for _, c := range active {
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
		names[id], _ = u["username"].(string)
		var full any
		switch p := u["profile"].(type) {
		case bson.M:
			full = p["fullname"]
		case bson.D:
			full = docMap(p)["fullname"]
		}
		if f, _ := full.(string); f != "" {
			names[id] = f
		}
	}
	nameOf := func(id string) string {
		if n := names[id]; n != "" {
			return n
		}
		return id
	}
	sort.SliceStable(authors, func(i, j int) bool { return byAuthor[authors[i]] > byAuthor[authors[j]] })
	adj := [][]any{}
	for _, id := range authors {
		adj = append(adj, []any{nameOf(id), byAuthor[id]})
	}
	rows = append(rows, section(tr(lang, "time-adjustments"), adj)...)
	// Hours by assignee: a card's hours under each of its assignees.
	byKey, order := map[string]float64{}, []string{}
	for _, c := range active {
		if c.SpentTime <= 0 {
			continue
		}
		keys := c.Assignees
		if len(keys) == 0 {
			keys = []string{""}
		}
		for _, k := range keys {
			if _, ok := byKey[k]; !ok {
				order = append(order, k)
			}
			byKey[k] += c.SpentTime
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return byKey[order[i]] > byKey[order[j]] })
	byAssignee := [][]any{}
	for _, k := range order {
		label := tr(lang, "no-assignee")
		if k != "" {
			label = nameOf(k)
		}
		byAssignee = append(byAssignee, []any{label, round2(byKey[k])})
	}
	rows = append(rows, section(tr(lang, "assignees"), byAssignee)...)
	withTime := []chartCard{}
	for _, c := range active {
		if c.SpentTime > 0 {
			withTime = append(withTime, c)
		}
	}
	sort.SliceStable(withTime, func(i, j int) bool { return withTime[i].SpentTime > withTime[j].SpentTime })
	byCard := [][]any{}
	for _, c := range withTime {
		title := c.Title
		if title == "" {
			title = c.ID
		}
		if c.IsOvertime {
			title += " (" + tr(lang, "overtime") + ")"
		}
		byCard = append(byCard, []any{a.text(title), round2(c.SpentTime)})
	}
	rows = append(rows, section(tr(lang, "card"), byCard)...)
	// flowDetailRows: every adjustment, the newest first.
	if len(entries) > 0 {
		sort.SliceStable(entries, func(i, j int) bool { return entries[i].at.After(entries[j].at) })
		rows = append(rows, []any{}, []any{tr(lang, "date"), tr(lang, "card"), tr(lang, "username"), "Δ " + tr(lang, "hours"), tr(lang, "hours")})
		for _, e := range entries {
			title := s.titleOfID(ctx, "cards", e.card)
			if title == "" {
				title = e.card
			}
			rows = append(rows, []any{e.at, a.text(title), nameOf(e.user), e.delta, e.sum})
		}
	}
	return rows
}

// scrumChartRows is chartExportRows' scrumVelocity and scrumSprint: each
// closed sprint's report (scrum.getBoardData's, the reader's cards only) as
// one row - its totals as cards, estimate and unknown, its working days, its
// estimate settings and the completed commitment.
func (s *Server) scrumChartRows(ctx context.Context, u *User, b *Board, key, lang, sprintID, title string) (string, []any, [][]any, error) {
	if u == nil {
		u = &User{}
	}
	res, err := s.scrumBoardData(ctx, u, b.ID)
	if err != nil {
		return "", nil, nil, err
	}
	data := res.(map[string]any)
	if data["importPending"] == true {
		return "", nil, nil, &methodError{"scrum-import-pending", "The Scrum import is incomplete. Report exports are unavailable."}
	}
	var reports []map[string]any
	sprints := asDocs(data["sprints"])
	if key == "scrumVelocity" {
		var closed []map[string]any
		for _, sp := range sprints {
			if docString(sp, "state") == "closed" && sp["closeSnapshot"] != nil && sp["report"] != nil {
				closed = append(closed, sp)
			}
		}
		sort.SliceStable(closed, func(i, j int) bool {
			a, _ := plain(closed[i]["closeSnapshot"].(map[string]any)["at"]).(time.Time)
			c, _ := plain(closed[j]["closeSnapshot"].(map[string]any)["at"]).(time.Time)
			return a.Before(c)
		})
		for _, sp := range closed {
			reports = append(reports, sp["report"].(map[string]any))
		}
	} else {
		if len(sprintID) > 200 {
			return "", nil, nil, &methodError{"invalid-sprint", "invalid-sprint"}
		}
		var sprint map[string]any
		for _, sp := range sprints {
			if docString(sp, "_id") == sprintID {
				sprint = sp
			}
		}
		if sprint == nil {
			return "", nil, nil, &methodError{"not-found", "Sprint not found on this board"}
		}
		if sprint["closeSnapshot"] != nil && sprint["report"] != nil {
			reports = append(reports, sprint["report"].(map[string]any))
		}
	}
	totals := []string{"committed", "completed", "added", "removed", "incomplete"}
	headers := []any{tr(lang, "scrum-sprint"), tr(lang, "scrum-estimate-unit")}
	for _, k := range totals {
		label := scrumLabel(lang, "scrum-"+k)
		headers = append(headers, label+": "+tr(lang, "cards"), label+": "+tr(lang, "scrum-estimate"), label+": "+tr(lang, "scrum-unknown-estimate"))
	}
	// "custom-field" is not one of WeKan's keys: its translate() falls back to
	// "Custom field" in every language, and so does this.
	headers = append(headers, tr(lang, "scrum-working-days"), tr(lang, "scrum-estimate-source"), "Custom field", tr(lang, "scrum-completion-policy"))
	commitment := tr(lang, "scrum-completed") + " / " + tr(lang, "scrum-committed")
	headers = append(headers, commitment+": "+tr(lang, "cards"), commitment+": "+tr(lang, "scrum-estimate"), commitment+": "+tr(lang, "scrum-unknown-estimate"))
	num := func(v any) any {
		if f := numberOf(v); f == f {
			return f
		}
		return ""
	}
	rows := [][]any{}
	for _, rep := range reports {
		name := docString(rep, "name")
		if rep["partial"] == true {
			name += " — " + tr(lang, "scrum-partial-snapshot")
		}
		row := []any{name, docString(rep, "unit")}
		for _, k := range totals {
			t, _ := rep[k].(map[string]any)
			row = append(row, num(t["count"]), num(t["estimate"]), num(t["unknown"]))
		}
		cc, _ := rep["completedCommitment"].(map[string]any)
		row = append(row, num(rep["plannedWorkingDays"]), docString(rep, "estimateSource"), docString(rep, "estimateCustomFieldId"), docString(rep, "completionPolicy"),
			num(cc["count"]), num(cc["estimate"]), num(cc["unknown"]))
		rows = append(rows, row)
	}
	if data["partial"] == true {
		title += " — " + tr(lang, "scrum-partial-report")
	}
	return title, headers, rows, nil
}

// xColumn is a column's letters: 0 is A, 26 is AA.
func xColumn(i int) string {
	name := ""
	for i++; i > 0; i = (i - 1) / 26 {
		name = string(rune('A'+(i-1)%26)) + name
	}
	return name
}

// chartXlsx is ExporterChartExcel's sheet: "<board> - <chart>" merged
// across row 1 in 16pt bold, the headers in row 2 bold on gray with thin
// borders, the rows from row 3 - numbers as numbers and times as dates -
// and every column 20 wide.
func chartXlsx(boardTitle, title string, headers []any, rows [][]any, notices []string) ([]byte, error) {
	name := strings.Map(func(r rune) rune {
		if strings.ContainsRune(`[]:*?/\`, r) {
			return ' '
		}
		return r
	}, title)
	if len([]rune(name)) > 31 {
		name = string([]rune(name)[:31])
	}
	ws := newXSheet(name)
	n := max(len(headers), 1)
	for i := 0; i < n; i++ {
		ws.Widths = append(ws.Widths, 20)
	}
	ws.merge("A1:" + xColumn(n-1) + "1")
	head := ws.cell("A1")
	head.Value, head.Font = boardTitle+" - "+title, xObj("size", 16, "bold", true)
	ws.Heights[1] = 30
	for i, h := range headers {
		c := ws.cell(xColumn(i) + "2")
		c.Value, c.Font, c.Fill, c.Border = h, xObj("bold", true), xGray(), xBorder("thin")
	}
	for r, row := range rows {
		for i, v := range row {
			c := ws.cell(xColumn(i) + strconv.Itoa(3+r))
			switch x := v.(type) {
			case time.Time:
				c.Value, c.NumFmt = jsDate(x), "yyyy-mm-dd hh:mm"
			case string, float64, int, int32, int64, bool, nil:
				c.Value = x
			default:
				c.Value = cellText(x)
			}
		}
	}
	sheets := []*xSheet{ws}
	// The notices on a sheet of their own, wrapped, a column 100 wide.
	if len(notices) > 0 {
		notes := newXSheet("Notes")
		notes.Widths = []float64{100}
		for i, n := range notices {
			c := notes.cell("A" + strconv.Itoa(i+1))
			c.Value, c.Alignment = n, xObj("wrapText", true)
		}
		sheets = append(sheets, notes)
	}
	return xlsxStyledBook(sheets)
}
