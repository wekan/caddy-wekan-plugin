package web

import (
	"net/http"
	"regexp"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Boards for a site admin, and a board's title, as Node.js WeKan
// (server/models/boards.js).
func (s *Server) apiBoardRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/boards", s.api(s.apiAllBoards))
	m.HandleFunc("GET /api/boards_count", s.api(s.apiBoardsCount))
	m.HandleFunc("PUT /api/boards/{board}/title", s.api(s.apiBoardTitle))
}

// A board whose title is wrapped in carets is one of WeKan's own (templates).
var caretWrapped = regexp.MustCompile(`^\^.*\^$`)

// apiAllBoards: for a site admin, the boards anybody signed in may open -
// public and instance-wide - that are real boards (WeKan's filterUserBoards).
func (s *Server) apiAllBoards(w http.ResponseWriter, r *http.Request, u *User) {
	if !u.IsAdmin {
		apiError(w, http.StatusForbidden, "Forbidden")
		return
	}
	cur, err := s.db.Collection("boards").Find(r.Context(), bson.M{"permission": bson.M{"$in": bson.A{"public", "instance"}}, "type": "board"},
		options.Find().SetSort(bson.D{{Key: "sort", Value: 1}}).SetProjection(bson.M{"title": 1}))
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var boards []Board
	if err := cur.All(r.Context(), &boards); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []map[string]string{}
	for _, b := range boards {
		if !caretWrapped.MatchString(strings.TrimSpace(b.Title)) {
			out = append(out, map[string]string{"_id": b.ID, "title": b.Title})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// apiBoardsCount: for a site admin, how many boards of each permission.
func (s *Server) apiBoardsCount(w http.ResponseWriter, r *http.Request, u *User) {
	if !u.IsAdmin {
		apiError(w, http.StatusForbidden, "Forbidden")
		return
	}
	out := map[string]int64{}
	for _, p := range []string{"private", "public", "instance"} {
		n, err := s.db.Collection("boards").CountDocuments(r.Context(), bson.M{"permission": p})
		if err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
		out[p] = n
	}
	writeJSON(w, http.StatusOK, out)
}

// apiBoardTitle: a board admin renames the board.
func (s *Server) apiBoardTitle(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardAdmin(w, r, u)
	if b == nil {
		return
	}
	var req struct {
		Title string `json:"title"`
	}
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	title := strings.TrimSpace(req.Title)
	if title == "" || len(title) > 1000 {
		apiError(w, http.StatusBadRequest, "a board needs a title")
		return
	}
	if _, err := s.db.Collection("boards").UpdateOne(r.Context(), bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"title": title, "slug": slugify(title)}}); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.hub.publish(b.ID, "board-changed")
	writeJSON(w, http.StatusOK, map[string]string{"_id": b.ID, "title": title})
}
