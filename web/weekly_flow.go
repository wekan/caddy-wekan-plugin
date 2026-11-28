package web

import "time"

var weeklyFlowViews = []struct{ slug, key, title, icon string }{
	{"created-resolved", "createdResolved", "board-view-created-resolved", "fa-exchange"},
	{"net-flow", "netFlow", "board-view-net-flow", "fa-balance-scale"},
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
	type totals struct{ created, resolved int }
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
				counts[week] = total
			}
		}
	}
	columns := []string{"week", "report-created", "resolved", "open"}
	if key == "netFlow" {
		columns = []string{"week", "report-arrivals", "report-departures", "report-net"}
	}
	headers := make([]any, len(columns))
	for i, column := range columns {
		headers[i] = tr(lang, column)
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
	return headers, rows
}

type weeklyFlowBar struct {
	Week                  string
	Value, Magnitude, Max int
}

func weeklyFlowBars(rows [][]any) []weeklyFlowBar {
	bars := make([]weeklyFlowBar, 0, min(len(rows), 24))
	scale := 1
	for _, row := range rows[max(0, len(rows)-24):] {
		value := row[3].(int)
		magnitude := max(value, -value)
		scale = max(scale, magnitude)
		bars = append(bars, weeklyFlowBar{Week: row[0].(string), Value: value, Magnitude: magnitude})
	}
	for i := range bars {
		bars[i].Max = scale
	}
	return bars
}
