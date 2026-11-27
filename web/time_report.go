package web

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

type timeRemaining struct {
	TotalHours float64 `json:"totalHours"`
	Days       int     `json:"days"`
	Hours      int     `json:"hours"`
	CardCount  int     `json:"cardCount"`
}

func remainingTime(cards []chartCard, now time.Time) timeRemaining {
	var result timeRemaining
	var hours float64
	for _, card := range cards {
		if _, done := card.done(); !card.Archived && !done && !card.DueAt.IsZero() {
			hours += card.DueAt.Sub(now).Hours()
			result.CardCount++
		}
	}
	whole := int(math.Round(hours))
	result.TotalHours, result.Days, result.Hours = round2(hours), whole/24, whole%24
	return result
}

func absTimePart(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func (s *Server) methodBoardChartData(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	var boardID, key *string
	var options map[string]json.RawMessage
	if len(args) < 2 || len(args) > 3 || json.Unmarshal(args[0], &boardID) != nil || boardID == nil ||
		json.Unmarshal(args[1], &key) != nil || key == nil ||
		(len(args) == 3 && (json.Unmarshal(args[2], &options) != nil || options == nil)) {
		return nil, &methodError{"Match.Error", "Match failed"}
	}
	ctx := r.Context()
	board, err := s.boardFor(ctx, *boardID, u)
	if err != nil || u == nil {
		return nil, &methodError{"not-authorized", "not-authorized"}
	}
	if member := board.member(u.ID); member != nil && member.AssignedOnly() {
		return nil, &methodError{"not-authorized", "not-authorized"}
	}
	if *key != "time" {
		return nil, &methodError{"bad-request", "unknown chartKey"}
	}
	cards, err := s.chartCards(ctx, board)
	if err != nil {
		return nil, err
	}
	return s.boardTimeReport(ctx, board, cards, cards), nil
}

func (s *Server) boardTimeReport(ctx context.Context, board *Board, cards, titleCards []chartCard) map[string]any {
	changes := s.timeAdjustments(ctx, board, titleCards)
	ids := bson.A{}
	for _, change := range changes {
		ids = append(ids, change.user)
	}
	for _, card := range cards {
		for _, id := range card.Assignees {
			ids = append(ids, id)
		}
	}
	nameOf := timeReportNames(s.timeReportUsers(ctx, ids))
	type author struct {
		UserID string  `json:"userId"`
		Label  string  `json:"label"`
		Hours  float64 `json:"hours"`
		Count  int     `json:"count"`
	}
	entries := []map[string]any{}
	groups := []author{}
	authors := map[string]int{}
	for _, change := range changes {
		index, seen := authors[change.user]
		if !seen {
			index = len(groups)
			authors[change.user] = index
			groups = append(groups, author{UserID: change.user, Label: nameOf(change.user)})
		}
		groups[index].Hours += change.delta
		groups[index].Count++
		entries = append(entries, map[string]any{"cardId": change.card, "title": change.title, "author": groups[index].Label,
			"at": change.at, "hours": change.delta, "total": change.sum})
	}
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].Hours > groups[j].Hours })
	type timeCard struct {
		Key        string  `json:"key"`
		Title      string  `json:"title"`
		Hours      float64 `json:"hours"`
		IsOvertime bool    `json:"isOvertime"`
	}
	type assignee struct {
		Key     string  `json:"key"`
		Label   string  `json:"label"`
		Hours   float64 `json:"hours"`
		Cards   int     `json:"cards"`
		Percent float64 `json:"percent"`
	}
	byCard := []timeCard{}
	byAssignee := []assignee{}
	assignees := map[string]int{}
	var total float64
	for _, card := range cards {
		if card.Archived || card.SpentTime <= 0 || math.IsNaN(card.SpentTime) || math.IsInf(card.SpentTime, 0) {
			continue
		}
		title := card.Title
		if title == "" {
			title = card.ID
		}
		byCard = append(byCard, timeCard{card.ID, title, round2(card.SpentTime), card.IsOvertime})
		total += card.SpentTime
		keys := card.Assignees
		if len(keys) == 0 {
			keys = []string{"__no_assignee__"}
		}
		for _, id := range keys {
			index, seen := assignees[id]
			if !seen {
				index = len(byAssignee)
				assignees[id] = index
				byAssignee = append(byAssignee, assignee{Key: id, Label: nameOf(id)})
			}
			byAssignee[index].Hours += card.SpentTime
			byAssignee[index].Cards++
		}
	}
	sort.SliceStable(byCard, func(i, j int) bool { return byCard[i].Hours > byCard[j].Hours })
	sort.SliceStable(byAssignee, func(i, j int) bool { return byAssignee[i].Hours > byAssignee[j].Hours })
	for i := range byAssignee {
		byAssignee[i].Percent = math.Round(1000*byAssignee[i].Hours/total) / 10
		byAssignee[i].Hours = round2(byAssignee[i].Hours)
	}
	return map[string]any{"adjustments": map[string]any{"entries": entries, "groups": groups},
		"byCard": byCard, "byAssignee": byAssignee, "remaining": remainingTime(cards, time.Now())}
}
