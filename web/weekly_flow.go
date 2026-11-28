package web

import (
	"math"
	"slices"
	"time"
)

type weeklyFlowView struct {
	slug, key, title, icon string
	column                 int
}

var weeklyFlowViews = []weeklyFlowView{
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
		return weeklyFlowHeaders(lang, "week", "open", "report-average-age"), rows
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
		return weeklyFlowHeaders(lang, "week", "report-on-time", "report-late", "report-no-due-date", "report-average-days-late"), rows
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
		return weeklyFlowHeaders(lang, "week", "done", "report-median"), rows
	}
	columns := []string{"week", "report-created", "resolved", "open"}
	if key == "netFlow" {
		columns = []string{"week", "report-arrivals", "report-departures", "report-net"}
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
	Week                  string
	Value, Magnitude, Max float64
}

func weeklyFlowBars(rows [][]any, column int, percentage bool) []weeklyFlowBar {
	bars := make([]weeklyFlowBar, 0, min(len(rows), 24))
	scale := 1.0
	if percentage {
		scale = 100
	}
	for _, row := range rows[max(0, len(rows)-24):] {
		value, _ := toFloat(row[column])
		if percentage {
			late, _ := toFloat(row[2])
			if total := value + late; total > 0 {
				value = math.Round(value/total*1000) / 10
			}
		}
		magnitude := max(value, -value)
		scale = max(scale, magnitude)
		bars = append(bars, weeklyFlowBar{Week: row[0].(string), Value: value, Magnitude: magnitude})
	}
	for i := range bars {
		bars[i].Max = scale
	}
	return bars
}
