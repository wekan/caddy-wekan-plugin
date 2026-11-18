package web

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"strings"
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

// WeKan's Calendar Mode (#3194, calendarModeView.jade, models/lib/
// calendarMode.js): Trello's calendar - each card on the day it is due, the
// date only, with its labels and whole title, a day's cards in the board's
// order. The page carries the due cards the board shows (the Filter's too);
// weddy.js lays them on the viewer's own local days, a month or a week.
type calendarModeCard struct {
	ID       string             `json:"_id"`
	Title    string             `json:"title"`
	DueAt    string             `json:"dueAt"`
	ListSort float64            `json:"listSort"`
	Sort     float64            `json:"sort"`
	Color    string             `json:"color,omitempty"`
	URL      string             `json:"url"`
	Labels   []calendarModeMark `json:"labels"`
}

type calendarModeMark struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

func calendarModeCardsOf(b *Board, lanes []lane) []calendarModeCard {
	labels := map[string]Label{}
	for _, l := range b.Labels {
		labels[l.ID] = l
	}
	out := []calendarModeCard{}
	seen := map[string]bool{}
	for _, l := range lanes {
		for _, ll := range l.Lists {
			for _, c := range ll.Cards {
				if seen[c.ID] || c.DueAt == nil || c.DueAt.IsZero() {
					continue
				}
				seen[c.ID] = true
				marks := []calendarModeMark{}
				for _, id := range c.LabelIDs {
					if lb, ok := labels[id]; ok {
						marks = append(marks, calendarModeMark{Name: lb.Name, Color: lb.Color})
					}
				}
				out = append(out, calendarModeCard{ID: c.ID, Title: c.Title, DueAt: jsISO(*c.DueAt), ListSort: ll.Sort, Sort: c.Sort,
					Color: c.Color, URL: "/b/" + b.ID + "/" + b.Slug + "/" + c.ID, Labels: marks})
			}
		}
	}
	return out
}

// calendarModeData is what the Calendar Mode page carries for its script.
func calendarModeData(b *Board, lanes []lane, lang string) map[string]any {
	cards, _ := json.Marshal(calendarModeCardsOf(b, lanes))
	labels := map[string]string{}
	for _, k := range []string{msg("today"), msg("previous"), msg("next"), msg("week"), msg("month")} {
		labels[k] = tr(lang, k)
	}
	l, _ := json.Marshal(labels)
	return map[string]any{"Cards": string(cards), "Labels": string(l)}
}

// isHelperBoardTitle is WeKan's models/lib/helperBoards.js: an internal board
// of WeKan's own - ^Subtasks^ and the like - that no list of boards shows.
func isHelperBoardTitle(title string) bool {
	return len(title) >= 2 && strings.HasPrefix(title, "^") && strings.HasSuffix(title, "^")
}

// multiboardCalendarData is WeKan's Multi Board Calendar (#2469,
// multiboardCalendarView.js): the Calendar view of every board the viewer is
// a member of - not archived, not a template, not a helper board - each event
// prefixed with its board's title, and read-only: a card's board is not
// necessarily the one open. The board's Filter applies to each, as WeKan's
// one filter selector does.
func (s *Server) multiboardCalendarData(ctx context.Context, u *User, f boardFilter, lang string) map[string]any {
	events := []calendarEvent{}
	boards, _ := s.boardsOf(ctx, u.ID)
	for i := range boards {
		b := &boards[i]
		if isHelperBoardTitle(b.Title) {
			continue
		}
		lanes, _, err := s.boardData(ctx, b)
		if err != nil {
			continue
		}
		if f.active() {
			shown := s.boardFilterCardIDs(ctx, b.ID, f)
			for i := range lanes {
				for j := range lanes[i].Lists {
					ll := &lanes[i].Lists[j]
					kept := ll.Cards[:0]
					for _, c := range ll.Cards {
						if shown[c.ID] {
							kept = append(kept, c)
						}
					}
					ll.Cards = kept
				}
			}
		}
		s.annotateMinicards(ctx, b, lanes, u.Profile.DateFormat) // the cards' dates
		for _, e := range calendarEventsOf(b, lanes, lang) {
			e.Title = "[" + b.Title + "] " + e.Title
			events = append(events, e)
		}
	}
	data := calendarData(&Board{}, nil, lang)
	out, _ := json.Marshal(events)
	data["Events"], data["ReadOnly"] = string(out), true
	return data
}

// storedCalendarView is the calendar view the member chose in WeKan for this
// board - the board's own choice, else the one every board shared (WeKan's
// storedBoardView) - for the views Weddy draws from it; "" for the board.
func storedCalendarView(u *User, boardID string) string {
	if u == nil {
		return ""
	}
	views := map[string]string{"board-view-cal": "cal", "board-view-calendar-mode": "calendar-mode", "board-view-multiboard-cal": "multiboard-cal"}
	if v, ok := u.Profile.BoardViews[boardID]; ok && slices.Contains(boardViewKeys, v) {
		return views[v]
	}
	if slices.Contains(boardViewKeys, u.Profile.BoardView) {
		return views[u.Profile.BoardView]
	}
	return ""
}
