package web

import (
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// WeKan's Meteor methods that the browser - WeKan's scripts and tests - calls
// with Meteor.call / Meteor.callAsync: POST /_methods/{name} with the
// arguments as a JSON array, answering the result, or {error, reason} as a
// Meteor.Error does. A method Weddy has not got answers 404, never a guess.
type methodFunc func(r *http.Request, u *User, args []json.RawMessage) (any, error)

// methodError is a Meteor.Error: its error code and reason.
type methodError struct{ Code, Reason string }

func (e *methodError) Error() string { return e.Code }

func (s *Server) methods() map[string]methodFunc {
	return map[string]methodFunc{
		"setLanguage":           s.methodSetLanguage,
		"cards.pokerVote":       s.methodPokerVote,
		"setListCollapsedState": s.methodSetListCollapsedState,
		"toggleBoardStar":       s.methodToggleBoardStar,
		"/checklists/remove":    s.methodRemoveChecklist,
		"updateListSort":        s.methodUpdateListSort,
		"/lists/update":         s.methodUpdateList,
	}
}

func (s *Server) methodRoutes(m *http.ServeMux) {
	m.HandleFunc("POST /_methods/{name}", func(w http.ResponseWriter, r *http.Request) {
		u := s.currentUser(r)
		if u == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not-authorized", "reason": "You must be logged in."})
			return
		}
		// JSON only: a form on another site cannot send it without a CORS
		// preflight, on top of the session cookie's SameSite=Lax.
		if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "application/json" {
			writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "415", "reason": "application/json only"})
			return
		}
		fn := s.methods()[r.PathValue("name")]
		if fn == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "404", "reason": "Method '" + r.PathValue("name") + "' not found"})
			return
		}
		var args []json.RawMessage
		r.Body = http.MaxBytesReader(w, r.Body, 16<<20)
		if err := json.NewDecoder(r.Body).Decode(&args); err != nil && r.ContentLength > 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Match.Error", "reason": "the arguments are a JSON array"})
			return
		}
		out, err := fn(r, u, args)
		if err != nil {
			var me *methodError
			if errors.As(err, &me) {
				// A numeric code is a number, as a collection's own 403 is in Meteor.
				var code any = me.Code
				if n, err := strconv.Atoi(me.Code); err == nil {
					code = n
				}
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": code, "reason": me.Reason})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "500", "reason": "Internal server error"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"result": out})
	})
}

// stringArg is argument i as text, or a Match.Error.
func stringArg(args []json.RawMessage, i int) (string, error) {
	var s string
	if i >= len(args) || json.Unmarshal(args[i], &s) != nil {
		return "", &methodError{"Match.Error", "Expected string"}
	}
	return s, nil
}

// methodPokerVote is WeKan's cards.pokerVote: a member - or anybody when the
// poker allows non-members - chooses one card of the deck.
func (s *Server) methodPokerVote(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	cardID, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	state, _ := stringArg(args, 1)
	var c Card
	if !validID(cardID) || s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": eq(cardID)}).Decode(&c) != nil {
		return nil, &methodError{"not-found", "not-found"}
	}
	b, err := s.boardFor(r.Context(), c.BoardID, u)
	if err != nil {
		return nil, &methodError{"not-found", "not-found"}
	}
	if p := s.cardPoker(r.Context(), b, u, c.ID, "en"); p == nil || !p.CanVote {
		return nil, &methodError{"not-authorized", "not-authorized"}
	}
	return 1, s.setPoker(r.Context(), c.ID, u.ID, state)
}

// methodSetLanguage is WeKan's setLanguage: one of WeKan's languages, kept in
// profile.language.
func (s *Server) methodSetLanguage(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	code, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	l := translations.lookup(code)
	if l == nil || !strings.EqualFold(langKey(l.Tag), langKey(code)) && !strings.EqualFold(langKey(l.Code), langKey(code)) {
		return nil, &methodError{"invalid-language", "no such language"}
	}
	_, err = s.db.Collection("users").UpdateOne(r.Context(), bson.M{"_id": eq(u.ID)}, bson.M{"$set": bson.M{"profile.language": l.Code}})
	return nil, err
}

// collapsedLists is the user's folded lists of the board, as WeKan's
// profile.collapsedLists[boardId]: "listId:swimlaneId" (or "listId") -> folded.
func (s *Server) collapsedLists(ctx context.Context, userID, boardID string) map[string]bool {
	var u struct {
		Profile struct {
			CollapsedLists map[string]map[string]bool `bson:"collapsedLists"`
		} `bson:"profile"`
	}
	_ = s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(userID)},
		options.FindOne().SetProjection(bson.M{"profile.collapsedLists": 1})).Decode(&u)
	return u.Profile.CollapsedLists[boardID]
}

// methodSetListCollapsedState is WeKan's setListCollapsedState: the user folds
// or unfolds a list - in one swimlane, "listId:swimlaneId" - in their own
// profile, on a board they can see. The keys are ids, so never a "$" or a "."
// that would make the path something else (WeKan's assertSafeMapKey).
func (s *Server) methodSetListCollapsedState(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	boardID, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	key, err := stringArg(args, 1)
	if err != nil {
		return nil, err
	}
	var collapsed bool
	if len(args) < 3 || json.Unmarshal(args[2], &collapsed) != nil {
		return nil, &methodError{"Match.Error", "Expected boolean"}
	}
	list, lane, inLane := strings.Cut(key, ":")
	if !validID(boardID) || !validID(list) || (inLane && !validID(lane)) {
		return nil, &methodError{"Match.Error", "Expected ids"}
	}
	if _, err := s.boardFor(r.Context(), boardID, u); err != nil {
		return nil, &methodError{"not-found", "Board not found"}
	}
	path := "profile.collapsedLists." + boardID + "." + key
	if _, err := s.db.Collection("users").UpdateOne(r.Context(), bson.M{"_id": eq(u.ID)}, bson.M{"$set": bson.M{path: collapsed}}); err != nil {
		return nil, err
	}
	return nil, nil
}

// methodToggleBoardStar is WeKan's toggleBoardStar: the board becomes one of
// the user's favorites, or stops being one. A star is only given to a board
// the user can see; taking one away is always possible, a stale one too.
func (s *Server) methodToggleBoardStar(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	boardID, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	if !validID(boardID) {
		return nil, &methodError{"Match.Error", "Expected an id"}
	}
	update := bson.M{"$pull": bson.M{"profile.starredBoards": boardID}}
	if !slices.Contains(u.Profile.StarredBoards, boardID) {
		if _, err := s.boardFor(r.Context(), boardID, u); err != nil {
			return nil, &methodError{"not-found", "Board not found"}
		}
		update = bson.M{"$addToSet": bson.M{"profile.starredBoards": boardID}}
	}
	if _, err := s.db.Collection("users").UpdateOne(r.Context(), bson.M{"_id": eq(u.ID)}, update); err != nil {
		return nil, err
	}
	return nil, nil
}

// methodRemoveChecklist is WeKan's /checklists/remove, the collection's own
// method: a checklist, by its id only, removed by whoever may edit its card
// (server/permissions/checklists.js) - with its items, never anybody else's
// (#6816). It answers how many it removed, as a Meteor mutator does.
func (s *Server) methodRemoveChecklist(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	var sel struct {
		ID string `json:"_id"`
	}
	var keys map[string]json.RawMessage
	if len(args) < 1 || json.Unmarshal(args[0], &keys) != nil || len(keys) != 1 || json.Unmarshal(args[0], &sel) != nil || !validID(sel.ID) {
		return nil, &methodError{"403", "Not permitted. Untrusted code may only remove documents by ID."}
	}
	ctx := r.Context()
	var cl struct {
		ID     string `bson:"_id"`
		CardID string `bson:"cardId"`
	}
	if s.db.Collection("checklists").FindOne(ctx, bson.M{"_id": eq(sel.ID)}).Decode(&cl) != nil {
		return 0, nil
	}
	var c Card
	var b Board
	if s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(cl.CardID)}).Decode(&c) != nil ||
		s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(c.BoardID)}).Decode(&b) != nil || !b.Writable(u, "write", &c) {
		return nil, &methodError{"403", "Access denied"}
	}
	if _, err := s.db.Collection("checklistItems").DeleteMany(ctx, bson.M{"checklistId": cl.ID}); err != nil {
		return nil, err
	}
	res, err := s.db.Collection("checklists").DeleteOne(ctx, bson.M{"_id": cl.ID})
	if err != nil {
		return nil, err
	}
	s.hub.publish(b.ID, "board-changed")
	return res.DeletedCount, nil
}

// listOnWritableBoard is the list by id, on a board the user may change
// (WeKan's allowIsBoardMemberWithWriteAccess), or the error to answer.
func (s *Server) listOnWritableBoard(r *http.Request, u *User, listID string, denied error) (*List, *Board, error) {
	var l List
	var b Board
	if !validID(listID) || s.db.Collection("lists").FindOne(r.Context(), bson.M{"_id": eq(listID)}).Decode(&l) != nil {
		return nil, nil, &methodError{"list-not-found", "List not found"}
	}
	if s.db.Collection("boards").FindOne(r.Context(), bson.M{"_id": eq(l.BoardID)}).Decode(&b) != nil || !b.Writable(u, "write", nil) {
		return nil, nil, denied
	}
	return &l, &b, nil
}

// listSet checks a change of a list's sort or swimlane: a number, and a
// swimlane of the same board ("" for every swimlane).
func (s *Server) listSet(r *http.Request, b *Board, field string, raw json.RawMessage) (any, error) {
	switch field {
	case "sort":
		var n float64
		if json.Unmarshal(raw, &n) != nil {
			return nil, &methodError{"Match.Error", "sort is a number"}
		}
		return n, nil
	case "swimlaneId":
		var id string
		if json.Unmarshal(raw, &id) != nil || (id != "" && (!validID(id) ||
			s.db.Collection("swimlanes").FindOne(r.Context(), bson.M{"_id": eq(id), "boardId": b.ID}).Err() != nil)) {
			return nil, &methodError{"invalid-swimlane", "Invalid swimlane for this board"}
		}
		return id, nil
	case "title":
		var t string
		if json.Unmarshal(raw, &t) != nil || strings.TrimSpace(t) == "" || len(t) > 1000 {
			return nil, &methodError{"Match.Error", "title is text"}
		}
		return t, nil
	}
	return nil, &methodError{"invalid-field", "Field " + field + " is not allowed"}
}

// methodUpdateListSort is WeKan's updateListSort: a list's place - its sort,
// its swimlane - changed by whoever may change the board, for a list of that
// board only (#5462, #5997).
func (s *Server) methodUpdateListSort(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	listID, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	boardID, err := stringArg(args, 1)
	if err != nil {
		return nil, err
	}
	var data map[string]json.RawMessage
	if len(args) < 3 || json.Unmarshal(args[2], &data) != nil || data == nil {
		return nil, &methodError{"Match.Error", "Expected object"}
	}
	denied := &methodError{"permission-denied", "User does not have permission to modify this board"}
	l, b, err := s.listOnWritableBoard(r, u, listID, denied)
	if err != nil {
		return nil, err
	}
	if b.ID != boardID {
		return nil, &methodError{"permission-denied", "List does not belong to this board"}
	}
	set := bson.M{"modifiedAt": time.Now().UTC()}
	for field, raw := range data {
		if field == "updatedAt" || field == "modifiedAt" {
			continue // the server's own time
		}
		if field == "title" {
			return nil, &methodError{"invalid-field", "Field title is not allowed"}
		}
		v, err := s.listSet(r, b, field, raw)
		if err != nil {
			return nil, err
		}
		set[field] = v
	}
	if _, err := s.db.Collection("lists").UpdateOne(r.Context(), bson.M{"_id": l.ID}, bson.M{"$set": set}); err != nil {
		return nil, err
	}
	s.hub.publish(b.ID, "board-changed")
	return nil, nil
}

// methodUpdateList is the lists collection's own /lists/update, as WeKan's
// allow and deny rules let it through (server/permissions/lists.js): by id,
// a $set of its title, sort or swimlane, by whoever may change its board -
// anybody else gets the 403 "Access denied" of #5462.
func (s *Server) methodUpdateList(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	var sel struct {
		ID string `json:"_id"`
	}
	var keys map[string]json.RawMessage
	if len(args) < 2 || json.Unmarshal(args[0], &keys) != nil || len(keys) != 1 || json.Unmarshal(args[0], &sel) != nil || !validID(sel.ID) {
		return nil, &methodError{"403", "Not permitted. Untrusted code may only update documents by ID."}
	}
	var mod map[string]map[string]json.RawMessage
	if json.Unmarshal(args[1], &mod) != nil || len(mod) != 1 || mod["$set"] == nil {
		return nil, &methodError{"403", "Access denied"}
	}
	l, b, err := s.listOnWritableBoard(r, u, sel.ID, &methodError{"403", "Access denied"})
	if err != nil {
		var me *methodError
		if errors.As(err, &me) && me.Code == "list-not-found" {
			return 0, nil
		}
		return nil, err
	}
	set := bson.M{"modifiedAt": time.Now().UTC()}
	for field, raw := range mod["$set"] {
		v, err := s.listSet(r, b, field, raw)
		if err != nil {
			return nil, &methodError{"403", "Access denied"}
		}
		set[field] = v
	}
	if _, err := s.db.Collection("lists").UpdateOne(r.Context(), bson.M{"_id": l.ID}, bson.M{"$set": set}); err != nil {
		return nil, err
	}
	s.hub.publish(b.ID, "board-changed")
	return 1, nil
}
