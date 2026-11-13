package web

import (
	"encoding/json"
	"sort"
	"time"
)

// WeKan's Calendar view (boardBody.js calendarView, models/lib/
// calendarBaseOptions.js): the board's cards on a FullCalendar, each the span
// of its start and end, and its received, due and end dates as their own
// events - the cards the board shows, so the Filter applies here as on the
// board. A day selected makes a card due then (createCardWithDueDate); an
// event dragged moves its due date, resized its end.

type calendarEvent struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Start     string `json:"start"`
	End       string `json:"end,omitempty"`
	AllDay    bool   `json:"allDay"`
	URL       string `json:"url"`
	ClassName string `json:"className,omitempty"`
}

// calendarEventsOf are the events of the lanes' cards, by id as WeKan sorts
// them.
func calendarEventsOf(b *Board, lanes []lane, lang string) []calendarEvent {
	var out []calendarEvent
	seen := map[string]bool{}
	iso := func(t time.Time) string { return jsISO(t) }
	for _, l := range lanes {
		for _, ll := range l.Lists {
			for _, c := range ll.Cards {
				if seen[c.ID] {
					continue // a list shown in every swimlane holds the card once
				}
				seen[c.ID] = true
				url := "/b/" + b.ID + "/" + b.Slug + "/" + c.ID
				color := ""
				if c.Color != "" {
					color = "calendar-event-" + c.Color
				}
				push := func(title string, start, end time.Time, cls string) {
					class := cls
					if color != "" {
						if class != "" {
							class += " "
						}
						class += color
					}
					out = append(out, calendarEvent{ID: c.ID, Title: title, Start: iso(start), End: iso(end),
						AllDay: end.Sub(start) == 24*time.Hour, URL: url, ClassName: class})
				}
				// The card's span, start to end.
				if c.StartAt != nil && c.EndAt != nil {
					push(c.Title, *c.StartAt, *c.EndAt, "")
				}
				for _, d := range []struct {
					at  *time.Time
					key string
					cls string
				}{{c.ReceivedAt, "card-received", "calendar-event-received"}, {c.DueAt, "card-due", "calendar-event-due"}, {c.EndAt, "card-end", "calendar-event-end"}} {
					if d.at != nil && !d.at.IsZero() {
						push(c.Title+" "+tr(lang, d.key), *d.at, d.at.Add(time.Hour), d.cls)
					}
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// calendarData is what the Calendar view's page carries for its script.
func calendarData(b *Board, lanes []lane, lang string) map[string]any {
	events, _ := json.Marshal(calendarEventsOf(b, lanes, lang))
	firstList, firstLane := "", ""
	for _, l := range lanes {
		if firstLane == "" {
			firstLane = l.ID
		}
		for _, ll := range l.Lists {
			if firstList == "" {
				firstList = ll.ID
			}
		}
	}
	labels := map[string]string{}
	for _, k := range []string{msg("previous"), msg("next"), msg("today"), msg("day"), msg("week"), msg("month"), msg("list"),
		msg("r-create-card"), msg("add-card"), msg("close")} {
		labels[k] = tr(lang, k)
	}
	l, _ := json.Marshal(labels)
	return map[string]any{"Events": string(events), "List": firstList, "Swimlane": firstLane, "Labels": string(l)}
}
