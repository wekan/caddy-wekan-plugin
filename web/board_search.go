package web

import (
	"encoding/json"
	"net/http"
	"net/url"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type boardSearchMatch struct {
	ID    string `bson:"_id" json:"id"`
	Title string `bson:"title" json:"title"`
	URL   string `bson:"-" json:"url"`
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
		selector := s.boardFilterSelector(ctx, b.ID, filter)
		selector["archived"] = false
		selector["deletedAt"] = nil
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
