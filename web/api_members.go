package web

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Labels, board members with roles, card members, dates and bulk cards - WeKan's
// REST API as tests/playwright/specs/17-rest-api.e2e.js runs it.
func (s *Server) apiMemberRoutes(m *http.ServeMux) {
	m.HandleFunc("PUT /api/boards/{board}/labels", s.api(s.apiAddLabel))
	m.HandleFunc("POST /api/boards/{board}/cards/labels", s.api(s.apiBulkLabels))
	m.HandleFunc("POST /api/boards/{board}/members/{user}/{action}", s.api(s.apiBoardMember))
	m.HandleFunc("POST /api/boards/{board}/lists/{list}/cards/{card}/members/{user}", s.api(s.apiCardMember(true)))
	m.HandleFunc("DELETE /api/boards/{board}/lists/{list}/cards/{card}/members/{user}", s.api(s.apiCardMember(false)))
}

// idList is a members or assignees value as WeKan takes it: an array of ids, one
// id, or "" / null for none - always stored as an array, never null (#3697, #2875).
func idList(v any) ([]string, error) {
	switch x := v.(type) {
	case nil:
		return []string{}, nil
	case string:
		if x == "" {
			return []string{}, nil
		}
		if !validID(x) {
			return nil, fmt.Errorf("%q is no id", x)
		}
		return []string{x}, nil
	case []any:
		out := make([]string, 0, len(x))
		seen := map[string]bool{}
		for _, e := range x {
			id, ok := e.(string)
			if !ok || !validID(id) {
				return nil, errors.New("ids are strings")
			}
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
		return out, nil
	}
	return nil, errors.New("ids are an array, one id, or empty")
}

var dateFields = []string{"dueAt", "startAt", "endAt", "receivedAt"}

// cardFields are the editable fields of a card in a request: what to $set and
// what to $unset. A date of "" or null clears it (#5846).
func cardFields(req map[string]any) (bson.M, bson.M, error) {
	set, unset := bson.M{}, bson.M{}
	if v, ok := req["title"]; ok {
		t, _ := v.(string)
		t = strings.TrimSpace(t)
		if t == "" || len(t) > 1000 {
			return nil, nil, errors.New("a card needs a title")
		}
		set["title"] = t
	}
	if v, ok := req["description"]; ok {
		d, isString := v.(string)
		if !isString && v != nil {
			return nil, nil, errors.New("a description is text")
		}
		set["description"] = d
	}
	for _, f := range []string{"members", "assignees"} {
		if v, ok := req[f]; ok {
			ids, err := idList(v)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %w", f, err)
			}
			set[f] = ids
		}
	}
	for _, f := range dateFields {
		v, ok := req[f]
		if !ok {
			continue
		}
		str, isString := v.(string)
		switch {
		case v == nil || (isString && str == ""):
			unset[f] = ""
		case isString:
			t, err := time.Parse(time.RFC3339Nano, str)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: not a date", f)
			}
			set[f] = t.UTC()
		default:
			return nil, nil, fmt.Errorf("%s: not a date", f)
		}
	}
	return set, unset, nil
}

// parentError is a refused parentId: WeKan's NotFound (404) or Forbidden (403).
type parentError struct {
	code   string
	status int
}

func (e *parentError) Error() string { return e.code }

// setParent puts a REST body's parentId into set, when its card is on a board
// the caller may see (WeKan's server/lib/visibleBoardIds.js
// assertParentCardIsVisible). Naming another is an attempt: the UI offers only
// visible parents, so it is recorded as ParentBleed.
func (s *Server) setParent(r *http.Request, u *User, req map[string]any, set bson.M) error {
	v, ok := req["parentId"]
	if !ok || v == nil || v == "" {
		return nil
	}
	id, isString := v.(string)
	if !isString || !validID(id) {
		return &parentError{"NotFound", http.StatusNotFound}
	}
	var parent Card
	if s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": eq(id)}).Decode(&parent) != nil {
		return &parentError{"NotFound", http.StatusNotFound}
	}
	if _, err := s.boardFor(r.Context(), parent.BoardID, u); err != nil {
		s.recordSecurity(r, securityEvent{Key: "authz.parent", Action: "blocked", Source: "parentId", UserID: u.ID,
			Detail: "A card on a board the user cannot see was named as a parent."})
		return &parentError{"Forbidden", http.StatusForbidden}
	}
	set["parentId"] = id
	return nil
}

// addCardFrom adds a card from a REST body: the author is the caller
// (GHSA-6jr3-42jf-vhm5), members and assignees are arrays, dates are dates.
func (s *Server) addCardFrom(r *http.Request, b *Board, u *User, listID, swimlaneID string, req map[string]any) (string, error) {
	title, _ := req["title"].(string)
	set, _, err := cardFields(req)
	if err != nil {
		return "", err
	}
	if err := s.setParent(r, u, req, set); err != nil {
		return "", err
	}
	c, err := s.AddCard(r.Context(), b, u, listID, swimlaneID, title)
	if err != nil {
		return "", err
	}
	delete(set, "title")
	if len(set) > 0 {
		if _, err := s.db.Collection("cards").UpdateOne(r.Context(), bson.M{"_id": c.ID}, bson.M{"$set": set}); err != nil {
			return "", err
		}
	}
	return c.ID, nil
}

func (s *Server) apiBulkAddCards(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, true)
	if b == nil {
		return
	}
	var req struct {
		SwimlaneID string           `json:"swimlaneId"`
		Cards      []map[string]any `json:"cards"`
	}
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(req.Cards) == 0 || len(req.Cards) > 500 {
		apiError(w, http.StatusBadRequest, "1 to 500 cards")
		return
	}
	ids := make([]map[string]any, 0, len(req.Cards))
	for i, c := range req.Cards {
		swimlane := req.SwimlaneID
		if v, ok := c["swimlaneId"].(string); ok && v != "" {
			swimlane = v
		}
		// ParentBleed, bulk sibling: an entry whose parent the caller cannot see is
		// refused alone, as WeKan's {index, error}, and the others are created.
		if perr := s.setParent(r, u, c, bson.M{}); perr != nil {
			var pe *parentError
			if errors.As(perr, &pe) {
				ids = append(ids, map[string]any{"index": i, "error": pe.code})
				continue
			}
		}
		id, err := s.addCardFrom(r, b, u, r.PathValue("list"), swimlane, c)
		if err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		ids = append(ids, map[string]any{"index": i, "_id": id})
	}
	s.hub.publish(b.ID, "board-changed")
	writeJSON(w, http.StatusOK, ids) // WeKan's shape: {index, _id} or {index, error} per card
}

var labelColors = map[string]bool{"white": true, "green": true, "yellow": true, "orange": true, "red": true, "purple": true,
	"blue": true, "sky": true, "lime": true, "pink": true, "black": true, "silver": true, "peachpuff": true, "crimson": true,
	"plum": true, "darkgreen": true, "slateblue": true, "magenta": true, "gold": true, "navy": true, "gray": true,
	"saddlebrown": true, "paleturquoise": true, "mistyrose": true, "indigo": true}

// apiAddLabel: only a board admin adds a label (#5819); the answer is its id.
func (s *Server) apiAddLabel(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, true)
	if b == nil {
		return
	}
	if !b.AdminOf(u.ID) {
		apiError(w, http.StatusForbidden, "only a board admin adds labels")
		return
	}
	var req struct {
		Label struct {
			Name  string `json:"name"`
			Color string `json:"color"`
		} `json:"label"`
	}
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.TrimSpace(req.Label.Name)
	if len(name) > 200 || !labelColors[req.Label.Color] {
		apiError(w, http.StatusBadRequest, "a label needs one of WeKan's colors")
		return
	}
	for _, l := range b.Labels {
		if l.Name == name && l.Color == req.Label.Color {
			writeJSON(w, http.StatusOK, l.ID)
			return
		}
	}
	l := Label{ID: randomString(idAlphabet, 6), Name: name, Color: req.Label.Color}
	if _, err := s.db.Collection("boards").UpdateOne(r.Context(), bson.M{"_id": b.ID}, bson.M{"$push": bson.M{"labels": l}}); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.hub.publish(b.ID, "board-changed")
	writeJSON(w, http.StatusOK, l.ID)
}

// apiBulkLabels adds and removes labels on many cards: a merge, never a
// replacement, and only labels of the board (#5819).
func (s *Server) apiBulkLabels(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, true)
	if b == nil {
		return
	}
	var req struct {
		CardIDs        []string `json:"cardIds"`
		AddLabelIDs    []string `json:"addLabelIds"`
		RemoveLabelIDs []string `json:"removeLabelIds"`
	}
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(req.CardIDs) == 0 || len(req.CardIDs) > 500 {
		apiError(w, http.StatusBadRequest, "1 to 500 cards")
		return
	}
	onBoard := map[string]bool{}
	for _, l := range b.Labels {
		onBoard[l.ID] = true
	}
	for _, id := range append(append([]string{}, req.AddLabelIDs...), req.RemoveLabelIDs...) {
		if !onBoard[id] {
			apiError(w, http.StatusBadRequest, fmt.Sprintf("label %q is not on this board", id))
			return
		}
	}
	in, ok := inIDs(req.CardIDs)
	if !ok {
		apiError(w, http.StatusBadRequest, "no such card on this board")
		return
	}
	filter := bson.M{"boardId": b.ID, "_id": in}
	if n, _ := s.db.Collection("cards").CountDocuments(r.Context(), filter); int(n) != len(req.CardIDs) {
		apiError(w, http.StatusBadRequest, "a card is not on this board")
		return
	}
	cards := s.db.Collection("cards")
	if len(req.AddLabelIDs) > 0 {
		if _, err := cards.UpdateMany(r.Context(), filter, bson.M{"$addToSet": bson.M{"labelIds": bson.M{"$each": req.AddLabelIDs}}}); err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if len(req.RemoveLabelIDs) > 0 {
		if _, err := cards.UpdateMany(r.Context(), filter, bson.M{"$pullAll": bson.M{"labelIds": req.RemoveLabelIDs}}); err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	s.hub.publish(b.ID, "board-changed")
	writeJSON(w, http.StatusOK, map[string]any{"cardIds": req.CardIDs})
}

// roles are WeKan's board roles and the member flags each sets.
var roles = map[string]Member{
	"normal":           {},
	"admin":            {IsAdmin: true},
	"noComments":       {IsNoComments: true},
	"commentOnly":      {IsCommentOnly: true},
	"worker":           {IsWorker: true},
	"readOnly":         {IsReadOnly: true},
	"readAssignedOnly": {IsReadAssignedOnly: true},
}

// apiBoardMember adds a user to the board with a role, or removes them - only a
// board admin (#5998). Removing keeps the entry with isActive false, as WeKan does.
func (s *Server) apiBoardMember(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, true)
	if b == nil {
		return
	}
	if !b.AdminOf(u.ID) {
		apiError(w, http.StatusForbidden, "only a board admin adds members")
		return
	}
	action := r.PathValue("action")
	var req struct {
		Role   string `json:"role"`
		Action string `json:"action"`
	}
	_ = decodeJSON(r, &req)
	// HashBleed sibling: WeKan answered any other body action with the whole user
	// document, password hash included. The action must be the route's own verb.
	if (action == "add" || action == "remove") && req.Action != action {
		apiError(w, http.StatusBadRequest, `action must be "`+action+`"`)
		return
	}
	if req.Role == "" {
		req.Role = "normal"
	}
	role, ok := roles[req.Role]
	if action != "add" && action != "remove" || !ok {
		apiError(w, http.StatusBadRequest, "action add or remove, and one of WeKan's roles")
		return
	}
	userID := r.PathValue("user")
	var target User
	if !validID(userID) || s.db.Collection("users").FindOne(r.Context(), bson.M{"_id": eq(userID)}).Decode(&target) != nil {
		apiError(w, http.StatusNotFound, "no such user")
		return
	}
	if action == "add" && target.LoginDisabled {
		apiError(w, http.StatusBadRequest, "a disabled user cannot be added (#1894)")
		return
	}
	role.UserID, role.IsActive = target.ID, action == "add"
	if action == "remove" {
		role = Member{UserID: target.ID}
	}
	members := make([]Member, 0, len(b.Members)+1)
	found := false
	for _, m := range b.Members {
		if m.UserID == target.ID {
			m, found = role, true
		}
		members = append(members, m)
	}
	if !found {
		if action == "remove" {
			writeJSON(w, http.StatusOK, map[string]string{"_id": b.ID})
			return
		}
		members = append(members, role)
	}
	if _, err := s.db.Collection("boards").UpdateOne(r.Context(), bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"members": members}}); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.hub.publish(b.ID, "board-changed")
	writeJSON(w, http.StatusOK, map[string]string{"_id": b.ID})
}

// apiCardMember adds or removes a card member - only an active member of the
// board (#5998).
func (s *Server) apiCardMember(add bool) apiHandler {
	return func(w http.ResponseWriter, r *http.Request, u *User) {
		b := s.apiBoardFor(w, r, u, true)
		if b == nil {
			return
		}
		c := s.cardOf(w, r, b)
		if c == nil {
			return
		}
		userID := r.PathValue("user")
		update := bson.M{"$pull": bson.M{"members": userID}}
		if add {
			if !validID(userID) || !b.MemberOf(userID) {
				apiError(w, http.StatusBadRequest, "only a member of the board can be a card member")
				return
			}
			update = bson.M{"$addToSet": bson.M{"members": userID}}
		}
		if _, err := s.db.Collection("cards").UpdateOne(r.Context(), bson.M{"_id": c.ID}, update); err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.hub.publish(b.ID, "board-changed")
		writeJSON(w, http.StatusOK, map[string]string{"_id": c.ID})
	}
}
