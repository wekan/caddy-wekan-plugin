package web

import (
	"cmp"
	"math"
	"slices"
	"time"
)

type weeklyFlowView struct {
	slug, key, title, icon string
	column                 int
}

var weeklyFlowViews = []weeklyFlowView{
	{"release-burndown", "releaseBurndown", "board-view-release-burndown", "fa-flag", 3},
	{"epic-progress", "epicProgress", "board-view-epic-progress", "fa-sitemap", 4},
	{"created-resolved", "createdResolved", "board-view-created-resolved", "fa-exchange", 3},
	{"net-flow", "netFlow", "board-view-net-flow", "fa-balance-scale", 3},
	{"average-age", "averageAge", "board-view-average-age", "fa-birthday-cake", 2},
	{"throughput-run", "throughputRun", "board-view-throughput-run", "fa-line-chart", 1},
	{"due-date-performance", "dueDatePerformance", "board-view-due-date-performance", "fa-calendar-check-o", 1},
}

func isWeeklyFlow(key string) bool {
	return slices.ContainsFunc(weeklyFlowViews, func(report weeklyFlowView) bool { return report.key == key })
}

func mondayUTC(at time.Time) time.Time {
	at = at.UTC()
	day := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
	return day.AddDate(0, 0, -(int(day.Weekday())+6)%7)
}

func weeklyFlowRows(cards []chartCard, now time.Time, key, lang string) ([]any, [][]any) {
	first := now
	for _, card := range cards {
		if !card.CreatedAt.IsZero() && card.CreatedAt.Before(first) {
			first = card.CreatedAt
		}
	}
	start, end := mondayUTC(first), mondayUTC(now)
	if limit := end.AddDate(0, 0, -52*7); start.Before(limit) {
		start = limit
	}
	if key == "averageAge" {
		var rows [][]any
		for week := start; !week.After(end); week = week.AddDate(0, 0, 7) {
			at := week.AddDate(0, 0, 7)
			if at.After(now) {
				at = now
			}
			open, age := 0, 0.0
			for _, card := range cards {
				done, finished := card.done()
				if card.CreatedAt.IsZero() || card.CreatedAt.After(at) || (finished && !done.After(at)) {
					continue
				}
				open++
				age += float64(at.UnixMilli()-card.CreatedAt.UnixMilli()) / 86400000
			}
			if open > 0 {
				age = math.Round(age/float64(open)*10) / 10
			}
			rows = append(rows, []any{dayOf(week), open, age})
		}
		return weeklyFlowHeaders(lang, msg("week"), msg("open"), msg("report-average-age")), rows
	}
	type totals struct {
		created, resolved, onTime, late, noDue int
		daysLate                               float64
	}
	counts := map[time.Time]totals{}
	open := 0
	for _, card := range cards {
		done, finished := card.done()
		if !card.CreatedAt.IsZero() {
			week := mondayUTC(card.CreatedAt)
			if week.Before(start) && (!finished || !done.Before(start)) {
				open++
			}
			if !week.Before(start) && !week.After(end) {
				total := counts[week]
				total.created++
				counts[week] = total
			}
		}
		if finished {
			week := mondayUTC(done)
			if !week.Before(start) && !week.After(end) {
				total := counts[week]
				total.resolved++
				if key == "dueDatePerformance" {
					switch {
					case card.DueAt.IsZero():
						total.noDue++
					case done.After(card.DueAt):
						total.late++
						total.daysLate += float64(done.UnixMilli()-card.DueAt.UnixMilli()) / 86400000
					default:
						total.onTime++
					}
				}
				counts[week] = total
			}
		}
	}
	if key == "dueDatePerformance" {
		var rows [][]any
		for week := start; !week.After(end); week = week.AddDate(0, 0, 7) {
			total := counts[week]
			average := 0.0
			if total.late > 0 {
				average = math.Round(total.daysLate/float64(total.late)*10) / 10
			}
			rows = append(rows, []any{dayOf(week), total.onTime, total.late, total.noDue, average})
		}
		return weeklyFlowHeaders(lang, msg("week"), msg("report-on-time"), msg("report-late"), msg("report-no-due-date"), msg("report-average-days-late")), rows
	}
	if key == "throughputRun" {
		var samples []int
		for week := start; !week.After(end); week = week.AddDate(0, 0, 7) {
			samples = append(samples, counts[week].resolved)
		}
		sorted := slices.Clone(samples)
		slices.Sort(sorted)
		median := float64(sorted[(len(sorted)-1)/2]+sorted[len(sorted)/2]) / 2
		rows := make([][]any, 0, len(samples))
		for i, count := range samples {
			rows = append(rows, []any{dayOf(start.AddDate(0, 0, i*7)), count, median})
		}
		return weeklyFlowHeaders(lang, msg("week"), msg("done"), msg("report-median")), rows
	}
	columns := []string{msg("week"), msg("report-created"), msg("resolved"), msg("open")}
	if key == "netFlow" {
		columns = []string{msg("week"), msg("report-arrivals"), msg("report-departures"), msg("report-net")}
	}
	var rows [][]any
	for week := start; !week.After(end); week = week.AddDate(0, 0, 7) {
		total := counts[week]
		open += total.created - total.resolved
		value := open
		if key == "netFlow" {
			value = total.resolved - total.created
		}
		rows = append(rows, []any{dayOf(week), total.created, total.resolved, value})
	}
	return weeklyFlowHeaders(lang, columns...), rows
}

func weeklyFlowHeaders(lang string, columns ...string) []any {
	headers := make([]any, len(columns))
	for i, column := range columns {
		headers[i] = tr(lang, column)
	}
	return headers
}

type weeklyFlowBar struct {
	Label                 string
	Value, Magnitude, Max float64
}

func weeklyFlowBars(rows [][]any, column int, key string) []weeklyFlowBar {
	bars := make([]weeklyFlowBar, 0, min(len(rows), 24))
	scale := 1.0
	if key == "dueDatePerformance" || key == "epicProgress" {
		scale = 100
	}
	if key == "epicProgress" {
		rows = rows[:min(len(rows), 24)]
	} else if key != "releaseBurndown" {
		rows = rows[max(0, len(rows)-24):]
	}
	for _, row := range rows {
		value, _ := toFloat(row[column])
		if key == "dueDatePerformance" {
			late, _ := toFloat(row[2])
			if total := value + late; total > 0 {
				value = math.Round(value/total*1000) / 10
			}
		}
		magnitude := max(value, -value)
		scale = max(scale, magnitude)
		bars = append(bars, weeklyFlowBar{Label: row[0].(string), Value: value, Magnitude: magnitude})
	}
	for i := range bars {
		bars[i].Max = scale
	}
	return bars
}

func epicProgressRows(cards []chartCard, lang string) ([]any, [][]any) {
	titles := make(map[string]string, len(cards))
	for _, card := range cards {
		titles[card.ID] = card.Title
	}
	type counts struct{ total, done int }
	byParent := map[string]counts{}
	var parents []string
	for _, card := range cards {
		if _, exists := titles[card.ParentID]; !exists || card.ParentID == "" {
			continue
		}
		total, seen := byParent[card.ParentID]
		if !seen {
			parents = append(parents, card.ParentID)
		}
		total.total++
		if _, done := card.done(); done {
			total.done++
		}
		byParent[card.ParentID] = total
	}
	rows := make([][]any, 0, len(parents))
	for _, parent := range parents {
		total := byParent[parent]
		ratio := math.Round(float64(total.done)/float64(total.total)*1000) / 10
		rows = append(rows, []any{titles[parent], total.total, total.done, total.total - total.done, ratio})
	}
	slices.SortStableFunc(rows, func(a, b []any) int {
		return cmp.Compare(a[4].(float64), b[4].(float64))
	})
	return weeklyFlowHeaders(lang, msg("card"), msg("subtasks"), msg("done"), msg("open"), msg("progress")), rows
}

type chartRelease struct {
	ID   string `bson:"_id"`
	Name string `bson:"name"`
}

func releaseBurndownRows(cards []chartCard, releases []chartRelease, now time.Time, lang string) ([]any, [][]any) {
	type counts struct{ total, done int }
	byRelease := make(map[string]counts, len(releases))
	recent := 0
	since := now.AddDate(0, 0, -28)
	for _, card := range cards {
		at, done := card.done()
		if done && !at.Before(since) {
			recent++
		}
		for _, id := range cardReleaseIds(card.Scrum) {
			total := byRelease[id]
			total.total++
			if done {
				total.done++
			}
			byRelease[id] = total
		}
	}
	rows := make([][]any, 0, len(releases))
	for _, release := range releases {
		name := release.Name
		if name == "" {
			name = release.ID
		}
		total := byRelease[release.ID]
		left := total.total - total.done
		finish := ""
		if left > 0 {
			finish = "-"
			if recent > 0 {
				weeks := int(math.Ceil(float64(left) * 4 / float64(recent)))
				finish = dayOf(now.AddDate(0, 0, weeks*7))
			}
		}
		rows = append(rows, []any{name, total.total, total.done, left, finish})
	}
	return weeklyFlowHeaders(lang, msg("scrum-release"), msg("cards"), msg("done"), msg("report-remaining"), msg("report-projected-finish")), rows
}
