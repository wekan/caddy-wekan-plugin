package web

import (
	"net/http"
	"regexp"
	"slices"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Card dependencies ("Red Strings", #3392) at WeKan's addresses
// (server/models/dependencies.js), stored as WeKan stores them: a card's
// cardDependencies, each {cardId, type, color, icon}.
func (s *Server) apiDependencyRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/boards/{board}/dependencies", s.api(s.apiBoardDependencies))
	m.HandleFunc("GET /api/boards/{board}/cards/{card}/dependencies", s.api(s.apiCardDependencies))
	m.HandleFunc("POST /api/boards/{board}/cards/{card}/dependencies", s.api(s.apiAddDependency))
	m.HandleFunc("PUT /api/boards/{board}/cards/{card}/dependencies/{target}", s.api(s.apiEditDependency))
	m.HandleFunc("DELETE /api/boards/{board}/cards/{card}/dependencies/{target}", s.api(s.apiDeleteDependency))
}

// WeKan's models/metadata/dependencies.js.
var dependencyTypes = []string{"related-to", "blocks", "is-blocked-by", "fixes", "is-fixed-by", "duplicates", "is-duplicated-by"}

const (
	defaultDependencyType  = "related-to"
	defaultDependencyColor = "#eb144c"
	defaultDependencyIcon  = "link"
)

// A color is drawn on the board and an icon names a Font Awesome class: only
// what can be either, never markup or CSS.
var (
	dependencyColor = regexp.MustCompile(`^(#[0-9a-fA-F]{3,8}|[a-z]{1,20})$`)
	dependencyIcon  = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)
)

type dependency struct {
	CardID string `bson:"cardId" json:"cardId"`
	Type   string `bson:"type" json:"type"`
	Color  string `bson:"color" json:"color"`
	Icon   string `bson:"icon" json:"icon"`
}

// normalizeDependencies is WeKan's normalizeDependencies: a bare card id (the
// old form) gets the defaults, an unknown type becomes related-to.
func normalizeDependencies(raw any) []dependency {
	arr, _ := raw.(bson.A)
	out := []dependency{}
	for _, e := range arr {
		switch d := e.(type) {
		case string:
			out = append(out, dependency{d, defaultDependencyType, defaultDependencyColor, defaultDependencyIcon})
		case bson.D:
			m := docMap(d)
			id, _ := m["cardId"].(string)
			if id == "" {
				continue
			}
			dep := dependency{CardID: id}
			dep.Type, _ = m["type"].(string)
			dep.Color, _ = m["color"].(string)
			dep.Icon, _ = m["icon"].(string)
			out = append(out, dep.normalized())
		}
	}
	return out
}

// docMap is a document's fields by name.
func docMap(d bson.D) map[string]any {
	m := make(map[string]any, len(d))
	for _, e := range d {
		m[e.Key] = e.Value
	}
	return m
}

func (d dependency) normalized() dependency {
	if !slices.Contains(dependencyTypes, d.Type) {
		d.Type = defaultDependencyType
	}
	if d.Color == "" {
		d.Color = defaultDependencyColor
	}
	if d.Icon == "" {
		d.Icon = defaultDependencyIcon
	}
	return d
}

// depCard is a card of the board with its raw cardDependencies.
func (s *Server) depCard(r *http.Request, b *Board, id string) (*Card, []dependency) {
	if !validID(id) {
		return nil, nil
	}
	var raw bson.D
	if s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": eq(id), "boardId": b.ID}).Decode(&raw) != nil {
		return nil, nil
	}
	var c Card
	doc, _ := bson.Marshal(raw)
	_ = bson.Unmarshal(doc, &c)
	return &c, normalizeDependencies(docMap(raw)["cardDependencies"])
}

// mayEditDependency is WeKan's canEditCardDependency: write or moveCard, and an
// assigned-only member only between cards assigned to them.
func mayEditDependency(b *Board, u *User, cards ...*Card) bool {
	for _, c := range cards {
		if c != nil && !b.Writable(u, "write", c) && !b.Writable(u, "moveCard", c) {
			return false
		}
	}
	return true
}

func (s *Server) apiBoardDependencies(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, false)
	if b == nil {
		return
	}
	cur, err := s.db.Collection("cards").Find(r.Context(), bson.M{"boardId": b.ID, "archived": false},
		options.Find().SetProjection(bson.M{"title": 1, "cardNumber": 1, "cardDependencies": 1}))
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var cards []bson.M
	if err := cur.All(r.Context(), &cards); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	byID := map[string]bson.M{}
	for _, c := range cards {
		byID[c["_id"].(string)] = c
	}
	lines := []map[string]any{}
	for _, c := range cards {
		for _, d := range normalizeDependencies(c["cardDependencies"]) {
			to := byID[d.CardID]
			lines = append(lines, map[string]any{"from": c["_id"], "fromTitle": c["title"], "fromCardNumber": c["cardNumber"],
				"to": d.CardID, "toTitle": field(to, "title"), "toCardNumber": field(to, "cardNumber"),
				"type": d.Type, "color": d.Color, "icon": d.Icon})
		}
	}
	writeJSON(w, http.StatusOK, lines)
}

func field(m bson.M, k string) any {
	if m == nil {
		return nil
	}
	return m[k]
}

func (s *Server) apiCardDependencies(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, false)
	if b == nil {
		return
	}
	c, deps := s.depCard(r, b, r.PathValue("card"))
	if c == nil {
		apiError(w, http.StatusNotFound, "Card not found")
		return
	}
	writeJSON(w, http.StatusOK, deps)
}

// depBody is a dependency's type, color and icon from a request, checked.
func depBody(w http.ResponseWriter, req map[string]any) (dependency, bool) {
	var d dependency
	d.Type, _ = req["type"].(string)
	d.Color, _ = req["color"].(string)
	d.Icon, _ = req["icon"].(string)
	switch {
	case d.Type != "" && !slices.Contains(dependencyTypes, d.Type):
		apiError(w, http.StatusBadRequest, "type must be one of: related-to, blocks, is-blocked-by, fixes, is-fixed-by, duplicates, is-duplicated-by")
	case d.Color != "" && !dependencyColor.MatchString(d.Color):
		apiError(w, http.StatusBadRequest, "color is #rgb, #rrggbb or a color name")
	case d.Icon != "" && !dependencyIcon.MatchString(d.Icon):
		apiError(w, http.StatusBadRequest, "icon is a Font Awesome icon name")
	default:
		return d, true
	}
	return d, false
}

// depBoard is the board for a dependency write, WeKan's canEditBoardDependencies:
// the caller's role may write or move cards there, or 403. Which cards an
// assigned-only member may link is mayEditDependency's.
func (s *Server) depBoard(w http.ResponseWriter, r *http.Request, u *User) *Board {
	b, err := s.boardFor(r.Context(), r.PathValue("board"), u)
	if err != nil || !(u.IsAdmin || b.Can(u.ID, "write") || b.Can(u.ID, "moveCard")) {
		apiError(w, http.StatusForbidden, "Forbidden")
		return nil
	}
	return b
}

func (s *Server) apiAddDependency(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.depBoard(w, r, u)
	if b == nil {
		return
	}
	var req map[string]any
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	c, deps := s.depCard(r, b, r.PathValue("card"))
	if c == nil {
		apiError(w, http.StatusNotFound, "Card not found")
		return
	}
	target, _ := req["cardId"].(string)
	if target == "" {
		target, _ = req["dependsOn"].(string)
	}
	if target == "" || target == c.ID {
		apiError(w, http.StatusBadRequest, "A valid target cardId is required")
		return
	}
	d, ok := depBody(w, req)
	if !ok {
		return
	}
	t, _ := s.depCard(r, b, target)
	if t == nil {
		apiError(w, http.StatusBadRequest, "Target card is not on this board")
		return
	}
	if !mayEditDependency(b, u, c, t) {
		apiError(w, http.StatusForbidden, "Forbidden")
		return
	}
	d.CardID = t.ID
	at := slices.IndexFunc(deps, func(x dependency) bool { return x.CardID == t.ID })
	if at >= 0 { // the same target again: updated in place, never a duplicate
		old := deps[at]
		if d.Type == "" {
			d.Type = old.Type
		}
		if d.Color == "" {
			d.Color = old.Color
		}
		if d.Icon == "" {
			d.Icon = old.Icon
		}
		deps[at] = d.normalized()
	} else {
		deps = append(deps, d.normalized())
	}
	if _, err := s.cardsUpdateOne(r.Context(), u.ID, bson.M{"_id": c.ID}, bson.M{"$set": bson.M{"cardDependencies": deps}}); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	e := d.normalized()
	writeJSON(w, http.StatusOK, map[string]any{"_id": c.ID, "cardId": e.CardID, "type": e.Type, "color": e.Color, "icon": e.Icon})
}

func (s *Server) apiEditDependency(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.depBoard(w, r, u)
	if b == nil {
		return
	}
	var req map[string]any
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	c, deps := s.depCard(r, b, r.PathValue("card"))
	if c == nil {
		apiError(w, http.StatusNotFound, "Card not found")
		return
	}
	if !mayEditDependency(b, u, c) {
		apiError(w, http.StatusForbidden, "Forbidden")
		return
	}
	d, ok := depBody(w, req)
	if !ok {
		return
	}
	if d.Type == "" && d.Color == "" && d.Icon == "" {
		apiError(w, http.StatusBadRequest, "Provide at least one of type, color, icon")
		return
	}
	for i := range deps {
		if deps[i].CardID == r.PathValue("target") {
			if d.Type != "" {
				deps[i].Type = d.Type
			}
			if d.Color != "" {
				deps[i].Color = d.Color
			}
			if d.Icon != "" {
				deps[i].Icon = d.Icon
			}
		}
	}
	if _, err := s.cardsUpdateOne(r.Context(), u.ID, bson.M{"_id": c.ID}, bson.M{"$set": bson.M{"cardDependencies": deps}}); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	writeJSON(w, http.StatusOK, map[string]string{"_id": c.ID})
}

func (s *Server) apiDeleteDependency(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.depBoard(w, r, u)
	if b == nil {
		return
	}
	c, deps := s.depCard(r, b, r.PathValue("card"))
	if c == nil {
		apiError(w, http.StatusNotFound, "Card not found")
		return
	}
	if !mayEditDependency(b, u, c) {
		apiError(w, http.StatusForbidden, "Forbidden")
		return
	}
	target := r.PathValue("target")
	deps = slices.DeleteFunc(deps, func(x dependency) bool { return x.CardID == target })
	if _, err := s.cardsUpdateOne(r.Context(), u.ID, bson.M{"_id": c.ID}, bson.M{"$set": bson.M{"cardDependencies": deps}}); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	writeJSON(w, http.StatusOK, map[string]string{"_id": c.ID})
}
