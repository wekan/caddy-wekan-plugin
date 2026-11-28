package web

import (
	"cmp"
	"context"
	"slices"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func workloadPeople(card chartCard) []string {
	if len(card.Assignees) > 0 {
		return card.Assignees
	}
	if len(card.Members) > 0 {
		return card.Members
	}
	return []string{""}
}

func (s *Server) workloadReport(ctx context.Context, board *Board, cards []chartCard, lang string, exporting bool) ([]any, [][]any, bool) {
	lists, err := s.chartLists(ctx, board.ID)
	if err != nil {
		return nil, nil, false
	}
	seen := map[string]bool{}
	var ids bson.A
	for _, card := range cards {
		if _, finished := card.done(); finished {
			continue
		}
		for _, id := range workloadPeople(card) {
			if id != "" && !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	users := s.timeReportUsers(ctx, ids)
	if exporting {
		// Include unresolved user IDs in anonymization as they also appear in the report.
		for _, user := range users {
			delete(seen, strOf(user["_id"]))
		}
		for id := range seen {
			users = append(users, bson.M{"_id": id})
		}
		aliases := s.exportAnonymizer(ctx, board, users)
		for _, user := range users {
			aliases.user(user)
		}
	}
	headers, rows := workloadRows(cards, lists, timeReportNames(users), lang)
	return headers, rows, true
}

func workloadRows(cards []chartCard, lists []List, nameOf func(string) string, lang string) ([]any, [][]any) {
	groups := map[string]map[string]int{}
	var order []string
	for _, card := range cards {
		if _, finished := card.done(); finished {
			continue
		}
		for _, id := range workloadPeople(card) {
			counts := groups[id]
			if counts == nil {
				counts = map[string]int{}
				groups[id] = counts
				order = append(order, id)
			}
			counts[card.ListID]++
		}
	}
	headers := weeklyFlowHeaders(lang, msg("person"), msg("open"))
	for _, list := range lists {
		headers = append(headers, list.Title)
	}
	rows := make([][]any, 0, len(order))
	for _, id := range order {
		name := "-"
		if id != "" {
			name = nameOf(id)
		}
		counts := groups[id]
		total := 0
		for _, count := range counts {
			total += count
		}
		row := []any{name, total}
		for _, list := range lists {
			row = append(row, counts[list.ID])
		}
		rows = append(rows, row)
	}
	slices.SortStableFunc(rows, func(a, b []any) int {
		return cmp.Compare(b[1].(int), a[1].(int))
	})
	return headers, rows
}
