package web

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func (s *Server) activityHeatmapRows(ctx context.Context, boardID string, now time.Time, lang string) ([]any, [][]any, bool) {
	cursor, err := s.db.Collection("activities").Find(ctx, bson.M{
		"boardId": eq(boardID), "createdAt": bson.M{"$gte": now.AddDate(0, 0, -365)},
	}, options.Find().SetProjection(bson.M{"createdAt": 1, "_id": 0}))
	if err != nil {
		return nil, nil, false
	}
	defer cursor.Close(ctx)
	var grid [7][24]int
	for cursor.Next(ctx) {
		if at, ok := cursor.Current.Lookup("createdAt").DateTimeOK(); ok {
			addHeatmapActivity(&grid, time.UnixMilli(at))
		}
	}
	if cursor.Err() != nil {
		return nil, nil, false
	}
	headers, rows := heatmapTable(grid, lang)
	return headers, rows, true
}

func addHeatmapActivity(grid *[7][24]int, at time.Time) {
	at = at.UTC()
	grid[(int(at.Weekday())+6)%7][at.Hour()]++
}

func heatmapTable(grid [7][24]int, lang string) ([]any, [][]any) {
	headers := []any{tr(lang, msg("day"))}
	for hour := 0; hour < 24; hour++ {
		headers = append(headers, fmt.Sprintf("%02d", hour))
	}
	days := []string{msg("monday"), msg("tuesday"), msg("wednesday"), msg("thursday"), msg("friday"), msg("saturday"), msg("sunday")}
	rows := make([][]any, 0, len(days))
	for i, day := range days {
		row := []any{tr(lang, day)}
		for _, count := range grid[i] {
			row = append(row, count)
		}
		rows = append(rows, row)
	}
	return headers, rows
}
