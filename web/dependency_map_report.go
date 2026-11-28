package web

import (
	"cmp"
	"slices"
)

func dependencyMapRows(cards []chartCard, lang string) ([]any, [][]any) {
	byID := make(map[string]chartCard, len(cards))
	for _, card := range cards {
		byID[card.ID] = card
	}
	links := make(map[string][]dependency, len(cards))
	for _, card := range cards {
		for _, link := range normalizeDependencies(card.Dependencies) {
			if _, exists := byID[link.CardID]; exists {
				links[card.ID] = append(links[card.ID], link)
			}
		}
	}
	memo, visiting := map[string]int{}, map[string]bool{}
	var chain func(string) int
	chain = func(id string) int {
		if length, ok := memo[id]; ok {
			return length
		}
		if visiting[id] {
			return 0
		}
		visiting[id] = true
		length := 0
		for _, link := range links[id] {
			if _, finished := byID[link.CardID].done(); !finished {
				length = max(length, 1+chain(link.CardID))
			}
		}
		delete(visiting, id)
		memo[id] = length
		return length
	}
	var rows [][]any
	for _, card := range cards {
		if _, finished := card.done(); finished {
			continue
		}
		for _, link := range links[card.ID] {
			target := byID[link.CardID]
			status := msg("open")
			if _, finished := target.done(); finished {
				status = msg("done")
			}
			rows = append(rows, []any{card.Title, link.Type, target.Title, tr(lang, status), chain(card.ID)})
		}
	}
	slices.SortStableFunc(rows, func(a, b []any) int {
		return cmp.Compare(b[4].(int), a[4].(int))
	})
	return weeklyFlowHeaders(lang, msg("card"), msg("dependency-type"), msg("report-depends-on"), msg("status"), msg("report-blocked-chain")), rows
}
