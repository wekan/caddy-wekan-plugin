package web

import (
	"net/http"
	"slices"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// The card's Move and Copy, as Node.js WeKan's moveCardPopup and
// copyCardPopup: a board the user may change, one of its swimlanes and one of
// its lists.

// placeOption is a list or swimlane as a Move or Copy select shows it.
type placeOption struct {
	ID, Title string
	N         int
}

// boardPlaces are a board's open lists and swimlanes, in order.
func (s *Server) boardPlaces(r *http.Request, boardID string) (lists, swimlanes []placeOption) {
	for _, p := range []struct {
		coll string
		out  *[]placeOption
	}{{"lists", &lists}, {"swimlanes", &swimlanes}} {
		cur, err := s.db.Collection(p.coll).Find(r.Context(), bson.M{"boardId": eq(boardID), "archived": false},
			options.Find().SetSort(bson.D{{Key: "sort", Value: 1}}).SetProjection(bson.M{"title": 1}))
		if err != nil {
			continue
		}
		var docs []bson.M
		_ = cur.All(r.Context(), &docs)
		for i, d := range docs {
			*p.out = append(*p.out, placeOption{ID: docString(d, "_id"), Title: docString(d, "title"), N: i + 1})
		}
	}
	return lists, swimlanes
}

// targetBoards are the boards a card can go to: the user's own that they may
// change, the current one first (WeKan's select starts there).
func (s *Server) targetBoards(r *http.Request, u *User, current *Board) []Board {
	boards, _ := s.boardsOf(r.Context(), u.ID)
	out := []Board{}
	if current.Writable(u, "write", nil) {
		out = append(out, *current)
	}
	for _, b := range boards {
		if b.ID != current.ID && b.Writable(u, "write", nil) {
			out = append(out, b)
		}
	}
	return out
}

func (s *Server) cardMoveRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /b/{board}/places", s.page(s.placesPage))
	m.HandleFunc("POST /b/{board}/cards/{card}/move-to", s.page(s.moveCardToPage))
	m.HandleFunc("POST /b/{board}/cards/{card}/copy-to", s.page(s.copyCardToPage))
}

// placesPage answers a board's lists and swimlanes for Move and Copy - only of
// a board the user may change.
func (s *Server) placesPage(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.writableBoard(r.Context(), r.PathValue("board"), u)
	if b == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	lists, swimlanes := s.boardPlaces(r, b.ID)
	out := map[string][]map[string]any{"lists": {}, "swimlanes": {}}
	for _, l := range lists {
		out["lists"] = append(out["lists"], map[string]any{"_id": l.ID, "title": l.Title, "n": l.N})
	}
	for _, sw := range swimlanes {
		out["swimlanes"] = append(out["swimlanes"], map[string]any{"_id": sw.ID, "title": sw.Title, "n": sw.N})
	}
	writeJSON(w, http.StatusOK, out)
}

// cardTarget reads the request's card, and the destination - a board the user
// may change, a list and a swimlane of it - or answers the error.
func (s *Server) cardTarget(w http.ResponseWriter, r *http.Request, u *User) (*Board, bson.M, *Card, *Board, string, string) {
	b, err := s.boardFor(r.Context(), r.PathValue("board"), u)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return nil, nil, nil, nil, "", ""
	}
	id := r.PathValue("card")
	var raw bson.M
	var c Card
	if !validID(id) || s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": eq(id), "boardId": b.ID}).Decode(&raw) != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return nil, nil, nil, nil, "", ""
	}
	_ = s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": eq(id)}).Decode(&c)
	to := s.writableBoard(r.Context(), r.FormValue("boardId"), u)
	if to == nil {
		http.Error(w, "the destination board is not yours to change", http.StatusForbidden)
		return nil, nil, nil, nil, "", ""
	}
	listID, swimlaneID := r.FormValue("listId"), r.FormValue("swimlaneId")
	if swimlaneID == "" {
		swimlaneID = s.firstSwimlane(r.Context(), to.ID)
	}
	lists, swimlanes := s.boardPlaces(r, to.ID)
	if !slices.ContainsFunc(lists, func(p placeOption) bool { return p.ID == listID }) ||
		!slices.ContainsFunc(swimlanes, func(p placeOption) bool { return p.ID == swimlaneID }) {
		http.Error(w, "a list and a swimlane of the destination board", http.StatusBadRequest)
		return nil, nil, nil, nil, "", ""
	}
	return b, raw, &c, to, swimlaneID, listID
}

// moveCardToPage is moveCardPopup's Done: the card, by whoever may change it,
// to the top of a list of a board they may change.
func (s *Server) moveCardToPage(w http.ResponseWriter, r *http.Request, u *User) {
	b, raw, c, to, swimlaneID, listID := s.cardTarget(w, r, u)
	if b == nil {
		return
	}
	if !b.WritableIn(r.Context(), s, u, "write", c) {
		http.Error(w, "your role on this board cannot change it", http.StatusForbidden)
		return
	}
	ctx := r.Context()
	if to.ID == b.ID {
		if err := s.MoveCards(ctx, b, u, []Move{{CardID: c.ID, ListID: listID, SwimlaneID: swimlaneID, Sort: s.topSort(ctx, b.ID, listID)}}); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	} else if s.refuseInList(ctx, to, listID, u, "moveCard", "list-locked:move-into") {
		http.Error(w, "your role cannot add cards to that list", http.StatusForbidden)
		return
	} else if err := s.moveCardTo(ctx, u, raw, b, to, swimlaneID, listID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.hub.publish(b.ID, "board-changed")
	s.hub.publish(to.ID, "board-changed")
	http.Redirect(w, r, "/b/"+to.ID+"/"+to.Slug+"/"+c.ID, http.StatusSeeOther)
}

// copyCardToPage is copyCardPopup's Done: a copy - its title the one given -
// by whoever may copy the card (an assigned-only member only theirs, WeKan's
// mayCopyFromBoard), onto a board they may change.
func (s *Server) copyCardToPage(w http.ResponseWriter, r *http.Request, u *User) {
	b, raw, c, to, swimlaneID, listID := s.cardTarget(w, r, u)
	if b == nil {
		return
	}
	if m := b.member(u.ID); !u.IsAdmin && (m == nil || (m.AssignedOnly() && !slices.Contains(c.Assignees, u.ID))) {
		http.Error(w, "this card is not yours to copy", http.StatusForbidden)
		return
	}
	if s.refuseInList(r.Context(), to, listID, u, "write", "page:card-copy-into") {
		http.Error(w, "your role cannot add cards to that list", http.StatusForbidden)
		return
	}
	id, err := s.copyCardTo(r, u, raw, b, to, swimlaneID, listID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if t := strings.TrimSpace(r.FormValue("title")); t != "" && len(t) <= 10000 {
		_, _ = s.db.Collection("cards").UpdateOne(r.Context(), bson.M{"_id": eq(id)}, bson.M{"$set": bson.M{"title": t}})
	}
	s.hub.publish(to.ID, "board-changed")
	http.Redirect(w, r, "/b/"+b.ID+"/"+b.Slug+"/"+c.ID, http.StatusSeeOther)
}
