package web

// The Scrum views, as WeKan's client/components/boards/scrum/scrumView.jade:
// Sprints (the sprint's lifecycle, its edit form, the releases and the
// sprint's events), Product Backlog (the cards in no sprint, each with its
// Scrum form), Velocity and Sprint Report (the closed sprints' reports,
// charted and tabled) - and the Board View menu that opens them. WeKan draws
// them in the browser from scrum.getBoardData; Weddy draws the same markup
// on the server from the same data, and the page's script makes the same
// method calls and draws the view again. What the view has chosen - the
// sprint, the release, the event - is in its address.

import (
	"context"
	"math"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// scrumViews are the Board View menu's Scrum entries
// (models/lib/boardViewSettings.js): the address's view, its label, its icon.
var scrumViews = []struct{ Slug, Key, Icon string }{
	{"product-backlog", msg("board-view-product-backlog"), "fa-list-ol"},
	{"sprints", msg("board-view-sprints"), "fa-repeat"},
	{"sprint-report", msg("board-view-sprint-report"), "fa-bar-chart"},
	{"velocity", msg("board-view-velocity"), "fa-line-chart"},
}

// scrumLabels are the labels the views name at run time: a state, a policy,
// a source, a report's series and an event's kind.
var scrumLabels = map[string]bool{}

func init() {
	for _, k := range []string{msg("scrum-state-planned"), msg("scrum-state-active"), msg("scrum-state-closed"), msg("scrum-state-cancelled"),
		msg("scrum-state-released"), msg("scrum-policy-dueComplete"), msg("scrum-policy-doneLists"), msg("scrum-source-poker"),
		msg("scrum-source-customField"), msg("scrum-committed"), msg("scrum-completed"), msg("scrum-added"), msg("scrum-removed"),
		msg("scrum-incomplete"), msg("scrum-event-planning"), msg("scrum-event-daily"), msg("scrum-event-review"), msg("scrum-event-retrospective")} {
		scrumLabels[k] = true
	}
}

// scrumLabel is one of scrumLabels in the language, else the key itself.
func scrumLabel(lang, key string) string {
	if !scrumLabels[key] {
		return key
	}
	return tr(lang, key)
}

type scrumOption struct {
	Value, Label string
	Selected     bool
}

type scrumSeries struct {
	Key, Label                                        string
	Count, Estimate, WidthCount, WidthEstimate, Total string
}

type scrumChartRow struct {
	Name    string
	Partial bool
	Series  []scrumSeries
}

type scrumChartGroup struct {
	Unit, SourceLabel, PolicyLabel string
	Rows                           []scrumChartRow
}

type scrumReportRow struct {
	Name, Unit                                                    string
	Partial                                                       bool
	Committed, Completed, Added, Removed, Incomplete, WorkingDays string
}

// scrumReport is scrumReportTable's data, with the page's language.
type scrumReport struct {
	L      string
	Groups []scrumChartGroup
	Rows   []scrumReportRow
}

type scrumCardRow struct {
	ID, Title, URL, Estimate, Sprint, Rank, RankValue, Release, IssueType, Criteria, Revision string
	CanWrite                                                                                  bool
	Sprints, Releases                                                                         []scrumOption
}

type scrumAccountability struct {
	Field, Label string
	Multiple     bool
	Options      []scrumOption
}

type scrumReleaseView struct {
	ID, Name, State, Scope, Done, Goal, Start, End, Released, Notes string
}

type scrumEventView struct {
	Name, Kind, Date, Timebox, Notes string
	FollowUps                        []scrumOption // Value: the card's address, Label: its title
}

func scrumNum(v any) string {
	f := numberOf(v)
	if math.IsNaN(f) {
		return "0"
	}
	return jsNumber(f)
}

// scrumTotalLabel is formatTotal: "__count__ cards; __estimate__ estimated;
// __unknown__ unknown".
func scrumTotalLabel(lang string, v any) string {
	m, _ := v.(map[string]any)
	if m == nil {
		return ""
	}
	return strings.NewReplacer("__count__", scrumNum(m["count"]), "__estimate__", scrumNum(m["estimate"]), "__unknown__", scrumNum(m["unknown"])).
		Replace(tr(lang, msg("scrum-total")))
}

func scrumDateValue(v any) string {
	if t, ok := plain(v).(time.Time); ok {
		return t.UTC().Format("2006-01-02")
	}
	return ""
}

func scrumISO(v any) string {
	if t, ok := plain(v).(time.Time); ok {
		return t.UTC().Format("2006-01-02T15:04:05.000Z")
	}
	return ""
}

// scrumReportOf is scrumReportTable: the chart groups - one per estimate
// unit, source, policy and partiality, each with its own scale - and the
// table, of reports that have both snapshots.
func scrumReportOf(lang string, reports []map[string]any, velocity bool) scrumReport {
	keys := []string{"committed", "completed", "added", "removed", "incomplete"}
	if velocity {
		keys = keys[:2]
	}
	out := scrumReport{L: lang}
	type group struct {
		g                 scrumChartGroup
		maxCount, maxEst  float64
		counts, estimates [][]float64
	}
	var order []string
	groups := map[string]*group{}
	value := func(rep map[string]any, key, metric string) float64 {
		m, _ := rep[key].(map[string]any)
		f := numberOf(m[metric])
		if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
			return 0
		}
		return f
	}
	for _, rep := range reports {
		days := "—"
		if v := rep["plannedWorkingDays"]; v != nil {
			days = scrumNum(v)
		}
		out.Rows = append(out.Rows, scrumReportRow{Name: docString(rep, "name"), Unit: docString(rep, "unit"), Partial: rep["partial"] == true,
			Committed: scrumTotalLabel(lang, rep["committed"]), Completed: scrumTotalLabel(lang, rep["completed"]), Added: scrumTotalLabel(lang, rep["added"]),
			Removed: scrumTotalLabel(lang, rep["removed"]), Incomplete: scrumTotalLabel(lang, rep["incomplete"]), WorkingDays: days})
		if rep["hasClose"] != true || rep["hasStart"] != true {
			continue
		}
		sig := canonical([]any{rep["unit"], rep["estimateSource"], rep["estimateCustomFieldId"], rep["completionPolicy"], rep["partial"] == true})
		g := groups[sig]
		if g == nil {
			g = &group{g: scrumChartGroup{Unit: docString(rep, "unit")}}
			if p := docString(rep, "completionPolicy"); p != "" {
				g.g.PolicyLabel = scrumLabel(lang, "scrum-policy-"+p)
			}
			if src := docString(rep, "estimateSource"); src != "" {
				g.g.SourceLabel = scrumLabel(lang, "scrum-source-"+src)
			}
			groups[sig] = g
			order = append(order, sig)
		}
		row := scrumChartRow{Name: docString(rep, "name"), Partial: rep["partial"] == true}
		var cs, es []float64
		for _, k := range keys {
			c, e := value(rep, k, "count"), value(rep, k, "estimate")
			g.maxCount, g.maxEst = math.Max(g.maxCount, c), math.Max(g.maxEst, e)
			cs, es = append(cs, c), append(es, e)
			row.Series = append(row.Series, scrumSeries{Key: k, Label: scrumLabel(lang, "scrum-"+k), Count: jsNumber(c), Estimate: jsNumber(e), Total: scrumTotalLabel(lang, rep[k])})
		}
		g.counts, g.estimates = append(g.counts, cs), append(g.estimates, es)
		g.g.Rows = append(g.g.Rows, row)
	}
	width := func(v, max float64) string {
		if max == 0 {
			return "0"
		}
		return jsNumber(100 * (v / max))
	}
	for _, sig := range order {
		g := groups[sig]
		for i := range g.g.Rows {
			for j := range g.g.Rows[i].Series {
				g.g.Rows[i].Series[j].WidthCount = width(g.counts[i][j], g.maxCount)
				g.g.Rows[i].Series[j].WidthEstimate = width(g.estimates[i][j], g.maxEst)
			}
		}
		out.Groups = append(out.Groups, g.g)
	}
	return out
}

// scrumCardOrder is compareScrumCards (models/lib/scrumCardOrder.js): the
// backlog rank, else the card's sort, else 0; ties by id.
func scrumCardOrder(cards []map[string]any) {
	rank := func(c map[string]any) float64 {
		m, _ := c["scrum"].(map[string]any)
		for _, v := range []any{m["backlogRank"], c["sort"]} {
			if f := numberOf(v); !math.IsNaN(f) {
				return f
			}
		}
		return 0
	}
	sort.SliceStable(cards, func(i, j int) bool {
		if a, b := rank(cards[i]), rank(cards[j]); a != b {
			return a < b
		}
		return docString(cards[i], "_id") < docString(cards[j], "_id")
	})
}

func asDocs(v any) []map[string]any {
	switch x := v.(type) {
	case []map[string]any:
		return x
	case []any:
		out := []map[string]any{}
		for _, e := range x {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

const scrumCardPage = 100

// scrumPage is GET /b/{board}/{slug}/scrum?view=...: one of the Scrum views.
func (s *Server) scrumPage(w http.ResponseWriter, r *http.Request, u *User) {
	ctx := r.Context()
	lang := languageOf(r, u).Tag
	b, err := s.boardFor(ctx, r.PathValue("board"), u)
	if err != nil {
		s.render(w, r, "notfound", http.StatusNotFound, map[string]any{"Title": tr(lang, "board-not-found"), "User": u})
		return
	}
	q := r.URL.Query()
	view := q.Get("view")
	viewKey := ""
	for _, v := range scrumViews {
		if v.Slug == view {
			viewKey = v.Key
		}
	}
	if viewKey == "" {
		view, viewKey = "sprints", "board-view-sprints"
	}
	title := tr(lang, viewKey)
	page := map[string]any{"Title": title, "User": u, "Board": b, "View": view, "ViewKey": viewKey, "Views": s.boardViewEntries(b, lang, view)}
	res, err := s.scrumBoardData(ctx, u, b.ID)
	if err != nil {
		page["Error"] = err.Error()
		if me, ok := err.(*methodError); ok {
			page["Error"] = me.Reason
		}
		s.render(w, r, "scrum", http.StatusOK, page)
		return
	}
	data := res.(map[string]any)
	s.fillScrumPage(ctx, page, data, b, lang, view, q)
	s.render(w, r, "scrum", http.StatusOK, page)
}

// boardViewEntries is the Board View menu: the board's swimlanes and the
// Scrum views.
func (s *Server) boardViewEntries(b *Board, lang, current string) []map[string]any {
	out := []map[string]any{{"Class": "js-open-swimlanes-view", "Icon": "fa-th-large", "Label": tr(lang, "swimlanes"),
		"URL": "/b/" + b.ID + "/" + b.Slug, "Current": current == ""}}
	for _, v := range scrumViews {
		out = append(out, map[string]any{"Class": "js-open-" + v.Slug + "-view", "Icon": v.Icon, "Label": tr(lang, v.Key),
			"URL": "/b/" + b.ID + "/" + b.Slug + "/scrum?view=" + v.Slug, "Current": current == v.Slug})
	}
	return out
}

func (s *Server) fillScrumPage(ctx context.Context, page, data map[string]any, b *Board, lang, view string, q url.Values) {
	settings, _ := data["settings"].(map[string]any)
	sprints, releases, events, cards := asDocs(data["sprints"]), asDocs(data["releases"]), asDocs(data["events"]), asDocs(data["cards"])
	lists := asDocs(data["lists"])
	canAdmin := data["canAdmin"] == true
	page["Data"], page["CanAdmin"], page["Settings"] = data, canAdmin, settings
	page["SettingsRevision"] = scrumNum(data["settingsRevision"])
	state := func(st string) string { return scrumLabel(lang, "scrum-state-"+st) }
	find := func(list []map[string]any, id string) map[string]any {
		for _, d := range list {
			if id != "" && docString(d, "_id") == id {
				return d
			}
		}
		return nil
	}
	sprintID := q.Get("sprint")
	sprint := find(sprints, sprintID)
	if sprint == nil {
		sprintID = ""
	}
	page["SprintID"] = sprintID
	link := func(changes map[string]string) string {
		v := url.Values{"view": {view}}
		for _, k := range []string{"sprint", "release", "event"} {
			if x := q.Get(k); x != "" {
				v.Set(k, x)
			}
		}
		for k, x := range changes {
			if x == "" {
				v.Del(k)
			} else {
				v.Set(k, x)
			}
		}
		return "/b/" + b.ID + "/" + b.Slug + "/scrum?" + v.Encode()
	}
	page["Link"] = link(nil)

	// The sprint picker and the chosen sprint.
	opts := []scrumOption{}
	for _, sp := range sprints {
		label := docString(sp, "name")
		if view == "sprints" {
			label += " (" + state(docString(sp, "state")) + ")"
		}
		opts = append(opts, scrumOption{Value: docString(sp, "_id"), Label: label, Selected: docString(sp, "_id") == sprintID})
	}
	page["SprintOptions"] = opts
	if sprint != nil {
		st := docString(sprint, "state")
		page["Sprint"] = map[string]any{"ID": sprintID, "Name": docString(sprint, "name"), "Goal": docString(sprint, "goal"), "State": state(st),
			"Revision": scrumNum(sprint["revision"]), "Start": scrumDateValue(sprint["plannedStart"]), "End": scrumDateValue(sprint["plannedEnd"]),
			"Capacity": capacityValue(sprint["capacity"]), "CapacityUnit": docString(sprint, "capacityUnit"),
			"Planned": st == "planned", "Active": st == "active", "Open": st == "planned" || st == "active",
			"NeedsRecovery": st == "closed" && sprint["rolloverPending"] == true, "RolloverError": docString(sprint, "rolloverError"),
			"ClosedFromRevision": scrumNum(sprint["closedFromRevision"]), "RolloverSprintID": docString(sprint, "rolloverSprintId")}
		rollover := []scrumOption{}
		for _, sp := range sprints {
			if docString(sp, "state") == "planned" && docString(sp, "_id") != sprintID {
				rollover = append(rollover, scrumOption{Value: docString(sp, "_id"), Label: docString(sp, "name")})
			}
		}
		page["RolloverOptions"] = rollover
	} else {
		page["Sprint"] = map[string]any{"Revision": "", "Capacity": ""}
	}

	// Settings, a board admin's.
	if canAdmin {
		var accs []scrumAccountability
		for _, f := range [][2]string{{"productOwnerId", "scrum-product-owner"}, {"scrumMasterId", "scrum-master"}, {"developerIds", "scrum-developers"}} {
			a := scrumAccountability{Field: f[0], Label: tr(lang, f[1]), Multiple: f[0] == "developerIds"}
			for _, m := range b.Members {
				if !m.IsActive {
					continue
				}
				name := m.UserID
				var mu User
				if s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(m.UserID)}).Decode(&mu) == nil {
					name = mu.Name()
				}
				sel := docString(settings, f[0]) == m.UserID
				if a.Multiple {
					sel = slices.Contains(stringsOf(settings["developerIds"]), m.UserID)
				}
				a.Options = append(a.Options, scrumOption{Value: m.UserID, Label: name, Selected: sel})
			}
			accs = append(accs, a)
		}
		page["Accountabilities"] = accs
		days := []scrumOption{}
		chosen := []string{}
		for _, d := range asAnyList(settings["workingDays"]) {
			chosen = append(chosen, scrumNum(d))
		}
		for i, k := range []string{"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"} {
			v := strconv.Itoa(i + 1)
			days = append(days, scrumOption{Value: v, Label: tr(lang, k), Selected: slices.Contains(chosen, v)})
		}
		page["WorkingDays"] = days
		page["EstimateSources"] = []scrumOption{{"poker", tr(lang, "scrum-source-poker"), settings["estimateSource"] != "customField"},
			{"customField", tr(lang, "scrum-source-customField"), settings["estimateSource"] == "customField"}}
		page["CompletionPolicies"] = []scrumOption{{"dueComplete", tr(lang, "scrum-policy-dueComplete"), settings["completionPolicy"] != "doneLists"},
			{"doneLists", tr(lang, "scrum-policy-doneLists"), settings["completionPolicy"] == "doneLists"}}
		fields := []scrumOption{}
		for _, f := range asDocs(data["customFields"]) {
			fields = append(fields, scrumOption{Value: docString(f, "_id"), Label: docString(f, "name"), Selected: docString(f, "_id") == docString(settings, "estimateCustomFieldId")})
		}
		page["EstimateFields"] = fields
	}

	// Releases, with the cards in each.
	done := doneListIDs(lists)
	releaseID := q.Get("release")
	release := find(releases, releaseID)
	var rviews []scrumReleaseView
	ropts := []scrumOption{}
	for _, rel := range releases {
		id := docString(rel, "_id")
		var scope, doneRows []map[string]any
		for _, c := range cards {
			if c["archived"] == true || !slices.Contains(cardReleaseIds(c["scrum"]), id) {
				continue
			}
			row := map[string]any{"estimate": scrumCardEstimate(c, settings)}
			scope = append(scope, row)
			if scrumCardDone(c, settings, done) {
				doneRows = append(doneRows, row)
			}
		}
		released := ""
		if t, ok := plain(rel["releasedAt"]).(time.Time); ok {
			released = t.UTC().Format("2006-01-02 15:04")
		}
		rviews = append(rviews, scrumReleaseView{ID: id, Name: docString(rel, "name"), State: state(docString(rel, "state")),
			Scope: scrumTotalLabel(lang, scrumTotal(scope)), Done: scrumTotalLabel(lang, scrumTotal(doneRows)), Goal: docString(rel, "goal"),
			Start: scrumDateValue(rel["plannedStart"]), End: scrumDateValue(rel["plannedEnd"]), Released: released, Notes: docString(rel, "notes")})
		ropts = append(ropts, scrumOption{Value: id, Label: docString(rel, "name"), Selected: release != nil && id == releaseID})
	}
	page["Releases"], page["ReleaseOptions"] = rviews, ropts
	relState := docString(release, "state")
	if relState == "" {
		relState = "planned"
	}
	rstates := []scrumOption{}
	for _, st := range []string{"planned", "released", "cancelled"} {
		rstates = append(rstates, scrumOption{st, state(st), st == relState})
	}
	page["ReleaseStates"] = rstates
	page["Release"] = map[string]any{"ID": docString(release, "_id"), "Name": docString(release, "name"), "Goal": docString(release, "goal"),
		"Start": scrumDateValue(release["plannedStart"]), "End": scrumDateValue(release["plannedEnd"]), "ReleasedAt": scrumISO(release["releasedAt"]),
		"Notes": docString(release, "notes"), "Revision": revisionText(release)}

	// The cards of the view: the sprint's, or the Product Backlog's.
	shown := []map[string]any{}
	for _, c := range cards {
		sc, _ := c["scrum"].(map[string]any)
		in := docString(sc, "sprintId")
		if c["archived"] == true {
			continue
		}
		if view == "sprints" && sprintID != "" && in == sprintID || !(view == "sprints" && sprintID != "") && in == "" {
			shown = append(shown, c)
		}
	}
	scrumCardOrder(shown)
	limit := scrumCardPage
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > limit {
		limit = n
	}
	if hidden := len(shown) - limit; hidden > 0 {
		page["ShowMore"] = strings.ReplaceAll(tr(lang, "scrum-show-more"), "__count__", strconv.Itoa(min(scrumCardPage, hidden)))
		page["ShowMoreLink"] = link(map[string]string{"limit": strconv.Itoa(limit + scrumCardPage)}) + ""
		shown = shown[:limit]
	}
	sprintName := map[string]string{}
	for _, sp := range sprints {
		sprintName[docString(sp, "_id")] = docString(sp, "name")
	}
	releaseName := map[string]string{}
	for _, rel := range releases {
		releaseName[docString(rel, "_id")] = docString(rel, "name")
	}
	var rows []scrumCardRow
	for _, c := range shown {
		sc, _ := c["scrum"].(map[string]any)
		est := scrumCardEstimate(c, settings)
		row := scrumCardRow{ID: docString(c, "_id"), Title: docString(c, "title"), URL: "/b/" + b.ID + "/" + b.Slug + "/" + docString(c, "_id"),
			Estimate: tr(lang, "scrum-unknown-estimate"), Sprint: tr(lang, "scrum-product-backlog"), Rank: "—", Release: "—",
			IssueType: docString(sc, "issueType"), Criteria: docString(sc, "acceptanceCriteria"), CanWrite: c["canWrite"] == true,
			Revision: scrumNum(c["scrumRevision"])}
		if est != nil {
			row.Estimate = scrumNum(est)
		}
		if n := sprintName[docString(sc, "sprintId")]; n != "" {
			row.Sprint = n
		}
		if sc != nil && sc["backlogRank"] != nil {
			row.Rank, row.RankValue = scrumNum(sc["backlogRank"]), scrumNum(sc["backlogRank"])
		}
		ids := cardReleaseIds(sc)
		var names []string
		for _, id := range ids {
			if n := releaseName[id]; n != "" {
				names = append(names, n)
			}
		}
		if len(names) > 0 {
			row.Release = strings.Join(names, ", ")
		}
		for _, sp := range sprints {
			if st := docString(sp, "state"); st == "planned" || st == "active" {
				row.Sprints = append(row.Sprints, scrumOption{docString(sp, "_id"), docString(sp, "name"), docString(sp, "_id") == docString(sc, "sprintId")})
			}
		}
		for _, rel := range releases {
			row.Releases = append(row.Releases, scrumOption{docString(rel, "_id"), docString(rel, "name"), slices.Contains(ids, docString(rel, "_id"))})
		}
		rows = append(rows, row)
	}
	page["Cards"] = rows

	// The sprint's events.
	eventID := q.Get("event")
	var event map[string]any
	var evs []scrumEventView
	eopts := []scrumOption{}
	cardTitle := map[string]string{}
	for _, c := range cards {
		cardTitle[docString(c, "_id")] = docString(c, "title")
	}
	for _, e := range events {
		if docString(e, "sprintId") != sprintID || sprintID == "" {
			continue
		}
		id := docString(e, "_id")
		if id == eventID {
			event = e
		}
		label := docString(e, "name")
		if label == "" {
			label = scrumLabel(lang, "scrum-event-"+docString(e, "kind"))
		}
		eopts = append(eopts, scrumOption{id, label, id == eventID})
		ev := scrumEventView{Name: docString(e, "name"), Kind: scrumLabel(lang, "scrum-event-"+docString(e, "kind")), Timebox: scrumNum(e["timeboxMinutes"]), Notes: docString(e, "notes")}
		if t, ok := plain(e["startsAt"]).(time.Time); ok {
			ev.Date = t.UTC().Format("2006-01-02 15:04")
		}
		for _, cid := range stringsOf(e["followUpCardIds"]) {
			ev.FollowUps = append(ev.FollowUps, scrumOption{Value: "/b/" + b.ID + "/" + b.Slug + "/" + cid, Label: cardTitle[cid]})
		}
		evs = append(evs, ev)
	}
	page["Events"], page["EventOptions"] = evs, eopts
	kind := docString(event, "kind")
	if kind == "" {
		kind = "planning"
	}
	kinds := []scrumOption{}
	for _, k := range []string{"planning", "daily", "review", "retrospective"} {
		kinds = append(kinds, scrumOption{k, scrumLabel(lang, "scrum-event-"+k), k == kind})
	}
	page["EventKinds"] = kinds
	timebox := "15"
	if event != nil && event["timeboxMinutes"] != nil {
		timebox = scrumNum(event["timeboxMinutes"])
	}
	follow := []scrumOption{}
	for _, c := range cards {
		follow = append(follow, scrumOption{docString(c, "_id"), docString(c, "title"), slices.Contains(stringsOf(event["followUpCardIds"]), docString(c, "_id"))})
	}
	page["FollowUpCards"] = follow
	page["Event"] = map[string]any{"ID": docString(event, "_id"), "Name": docString(event, "name"), "StartsAt": scrumISO(event["startsAt"]),
		"Timebox": timebox, "Notes": docString(event, "notes"), "Revision": revisionText(event)}

	// The reports.
	switch view {
	case "velocity":
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
		var reports []map[string]any
		for _, sp := range closed {
			reports = append(reports, sp["report"].(map[string]any))
		}
		page["Report"] = scrumReportOf(lang, reports, true)
		page["ChartKey"] = "scrumVelocity"
	case "sprint-report":
		var reports []map[string]any
		if sprint != nil && sprint["closeSnapshot"] != nil && sprint["report"] != nil {
			reports = append(reports, sprint["report"].(map[string]any))
		}
		page["Report"] = scrumReportOf(lang, reports, false)
		page["ChartKey"] = "scrumSprint"
	}
	page["IsReport"] = view == "velocity" || view == "sprint-report"
}

func capacityValue(v any) string {
	if v == nil {
		return ""
	}
	return scrumNum(v)
}

// revisionText is a record's revision for its form, "" for a new record.
func revisionText(d map[string]any) string {
	if d == nil {
		return ""
	}
	return scrumNum(d["revision"])
}

func asAnyList(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	}
	return nil
}
