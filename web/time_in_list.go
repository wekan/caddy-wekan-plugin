package web

import (
	"context"
	"math"
	"slices"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type chartListMove struct {
	EntityID string    `bson:"entityId"`
	At       time.Time `bson:"createdAt"`
	Previous struct {
		ListID string `bson:"listId"`
	} `bson:"previousContent"`
	Next struct {
		ListID *string `bson:"listId"`
	} `bson:"newContent"`
}

func (s *Server) chartLists(ctx context.Context, boardID string) ([]List, error) {
	listCursor, err := s.db.Collection("lists").Find(ctx, bson.M{"boardId": eq(boardID), "archived": bson.M{"$ne": true}, "deletedAt": nil},
		options.Find().SetProjection(bson.M{"title": 1}).SetSort(bson.D{{Key: "sort", Value: 1}, {Key: "_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	var lists []List
	err = listCursor.All(ctx, &lists)
	return lists, err
}

func (s *Server) timeInListReport(ctx context.Context, boardID string, cards []chartCard, now time.Time, lang string) ([]any, [][]any, bool) {
	lists, err := s.chartLists(ctx, boardID)
	if err != nil {
		return nil, nil, false
	}
	historyCursor, err := s.db.Collection("changeHistory").Find(ctx, bson.M{
		"boardId": eq(boardID), "entityType": "card", "group": "position", "newContent.listId": bson.M{"$exists": true},
	}, options.Find().SetProjection(bson.M{"entityId": 1, "createdAt": 1, "previousContent.listId": 1, "newContent.listId": 1}).
		SetSort(bson.D{{Key: "createdAt", Value: 1}, {Key: "_id", Value: 1}}))
	if err != nil {
		return nil, nil, false
	}
	var history []chartListMove
	if err := historyCursor.All(ctx, &history); err != nil {
		return nil, nil, false
	}
	headers, rows := timeInListRows(cards, lists, history, now, lang)
	return headers, rows, true
}

func timeInListRows(cards []chartCard, lists []List, history []chartListMove, now time.Time, lang string) ([]any, [][]any) {
	moves := map[string][]chartListMove{}
	for _, row := range history {
		if row.Next.ListID != nil && !row.At.IsZero() {
			moves[row.EntityID] = append(moves[row.EntityID], row)
		}
	}
	spans := make(map[string][]float64, len(lists))
	for _, list := range lists {
		spans[list.ID] = nil
	}
	addSpan := func(list string, since, until time.Time) {
		if _, exists := spans[list]; exists && !since.IsZero() && until.After(since) {
			spans[list] = append(spans[list], float64(until.UnixMilli()-since.UnixMilli())/86400000)
		}
	}
	for _, card := range cards {
		own := moves[card.ID]
		list, since := card.ListID, card.CreatedAt
		if len(own) > 0 {
			list = own[0].Previous.ListID
		}
		for _, move := range own {
			addSpan(list, since, move.At)
			list, since = *move.Next.ListID, move.At
		}
		end, done := card.done()
		if !done {
			end = now
		}
		addSpan(list, since, end)
	}
	percentile := func(sorted []float64, p float64) float64 {
		if len(sorted) == 0 {
			return 0
		}
		at := float64(len(sorted)-1) * p
		lo, hi := int(math.Floor(at)), int(math.Ceil(at))
		return math.Round((sorted[lo]+(sorted[hi]-sorted[lo])*(at-float64(lo)))*10) / 10
	}
	rows := make([][]any, 0, len(lists))
	for _, list := range lists {
		days := spans[list.ID]
		slices.Sort(days)
		rows = append(rows, []any{list.Title, len(days), percentile(days, .5), percentile(days, .85)})
	}
	return weeklyFlowHeaders(lang, msg("list"), msg("cards"), msg("report-median-days"), msg("flow-p85")), rows
}
