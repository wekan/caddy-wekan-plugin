package web

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type boardSearchMatch struct {
	ID    string `bson:"_id" json:"id"`
	Title string `bson:"title" json:"title"`
	URL   string `bson:"-" json:"url"`
}

var boardSearchDecimal = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:[.,][0-9]+)?|[.,][0-9]+)$`)

func (fr *fieldReader) searchMatches(text string) bson.A {
	var textIDs, numericIDs bson.A
	for id, def := range fr.defs {
		if !fr.mayRead(id) {
			continue
		}
		switch docString(def, "type") {
		case "text", "stringtemplate":
			textIDs = append(textIDs, id)
		case "number", "currency":
			numericIDs = append(numericIDs, id)
		}
	}
	fieldMatch := func(ids bson.A, value any) bson.M {
		return bson.M{"customFields": bson.M{"$elemMatch": bson.M{"_id": dbIn(ids), "value": value}}}
	}
	var matches bson.A
	if len(textIDs) > 0 {
		matches = append(matches, fieldMatch(textIDs, bson.Regex{Pattern: queryText(regexp.QuoteMeta(text)), Options: "i"}))
	}
	if len(numericIDs) > 0 && boardSearchDecimal.MatchString(text) {
		if value, err := strconv.ParseFloat(strings.Replace(text, ",", ".", 1), 64); err == nil {
			matches = append(matches, fieldMatch(numericIDs, value))
		}
	}
	return matches
}

func (s *Server) boardSearchMatches(w http.ResponseWriter, r *http.Request, u *User) {
	ctx := r.Context()
	b, err := s.boardFor(ctx, r.PathValue("board"), u)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	matches := make([]boardSearchMatch, 0)
	filter := parseBoardFilter(url.Values{"text": {r.URL.Query().Get("q")}}, b)
	if filter.Text != "" {
		alternatives := s.boardTextMatches(ctx, b.ID, filter.Text)
		if u != nil {
			reader := s.fieldReaderFor(ctx, b, u)
			alternatives = append(alternatives, reader.searchMatches(filter.Text)...)
		}
		selector := bson.M{
			"boardId": eq(b.ID), "archived": false, "deletedAt": nil, "$or": alternatives,
		}
		if u != nil && !u.IsAdmin && assignedOnlyOn(b, u) {
			selector["assignees"] = eq(u.ID)
		}
		cursor, err := s.db.Collection("cards").Find(ctx, selector, options.Find().
			SetProjection(bson.M{"title": 1}).
			SetSort(bson.D{{Key: "sort", Value: 1}, {Key: "_id", Value: 1}}))
		if err != nil || cursor.All(ctx, &matches) != nil {
			http.Error(w, "Search unavailable", http.StatusInternalServerError)
			return
		}
		for i := range matches {
			matches[i].URL = "/b/" + url.PathEscape(b.ID) + "/" + url.PathEscape(b.Slug) + "/" + url.PathEscape(matches[i].ID)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	_ = json.NewEncoder(w).Encode(matches)
}
