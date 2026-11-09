package web

import (
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"regexp"
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

// collectionWrite is a Meteor collection's own client write method.
var collectionWrite = regexp.MustCompile(`^/[A-Za-z0-9_.-]+/(insert|update|remove|upsert)$`)

func (e *methodError) Error() string { return e.Code }

func (s *Server) methods() map[string]methodFunc {
	m := map[string]methodFunc{
		"setLanguage":                     s.methodSetLanguage,
		"cards.pokerVote":                 s.methodPokerVote,
		"setListCollapsedState":           s.methodSetListCollapsedState,
		"toggleBoardStar":                 s.methodToggleBoardStar,
		"/checklists/remove":              s.methodRemoveChecklist,
		"updateListSort":                  s.methodUpdateListSort,
		"/lists/update":                   s.methodUpdateList,
		"moveList":                        s.methodMoveList,
		"/card_comments/insert":           s.methodInsertComment,
		"sendInvitation":                  s.methodSendInvitation,
		"acceptInvite":                    s.methodAcceptInvite,
		"quitBoard":                       s.methodQuitBoard,
		"/users/update":                   s.methodUpdateUser,
		"setEmail":                        s.methodSetEmail,
		"rules.runButton":                 s.methodRunButton,
		"rules.createRule":                s.methodCreateRule,
		"rules.deleteRule":                s.methodDeleteRule,
		"rules.setEnabled":                s.methodSetRuleEnabled,
		"toggleCardMaximized":             s.methodToggleCardMaximized,
		"archiveSelectedCards":            s.methodArchiveSelectedCards,
		"/cards/update":                   s.methodCardsUpdate,
		"watch":                           s.methodWatch,
		"createBoardWithInitialSwimlanes": s.methodCreateBoardWithInitialSwimlanes,
	}
	for k, v := range s.customFieldMethods() {
		m[k] = v
	}
	for k, v := range s.orgTeamMethods() {
		m[k] = v
	}
	for k, v := range s.orgTeamAdminMethods() {
		m[k] = v
	}
	for k, v := range s.activityRecoveryMethods() {
		m[k] = v
	}
	for k, v := range s.emailRecoveryMethods() {
		m[k] = v
	}
	return m
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
		if fn == nil && collectionWrite.MatchString(r.PathValue("name")) {
			// Meteor's answer to a client write of a restricted collection
			// with no allow rule: Weddy writes nothing through these, so every
			// one it has no method for is denied - never a 404 that says
			// whether the collection exists.
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": 403, "reason": "Access denied"})
			return
		}
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

// optionalString is argument i as text, "" when it is null or missing, or a
// Match.Error for anything else.
func optionalString(args []json.RawMessage, i int) (string, error) {
	if i >= len(args) || string(args[i]) == "null" {
		return "", nil
	}
	return stringArg(args, i)
}

// methodMoveList is WeKan's moveList(listId, boardId, swimlaneId,
// neighborListId, position, title): the list to a board - its own or
// another - by whoever may change both (requireBoardMutation): anybody else,
// a comment-only member too, gets not-authorized. A swimlane is one of the
// destination board's, or "" for every swimlane.
func (s *Server) methodMoveList(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	var str [3]string
	for i := range str {
		v, err := stringArg(args, i)
		if err != nil {
			return nil, err
		}
		str[i] = v
	}
	listID, boardID, swimlaneID := str[0], str[1], str[2]
	var opt [3]string
	for i := range opt {
		v, err := optionalString(args, 3+i)
		if err != nil {
			return nil, err
		}
		opt[i] = v
	}
	ctx := r.Context()
	var raw bson.M
	if !validID(listID) || s.db.Collection("lists").FindOne(ctx, bson.M{"_id": eq(listID)}).Decode(&raw) != nil {
		return nil, &methodError{"list-not-found", "List not found"}
	}
	source, _ := raw["boardId"].(string)
	var b, to Board
	if s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(source)}).Decode(&b) != nil || !b.Writable(u, "write", nil) {
		return nil, &methodError{"not-authorized", "You cannot move this list."}
	}
	if !validID(boardID) || s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(boardID)}).Decode(&to) != nil {
		return nil, &methodError{"board-not-found", "Target board not found"}
	}
	if !to.Writable(u, "write", nil) {
		return nil, &methodError{"not-authorized", "You cannot move a list to this board."}
	}
	if swimlaneID != "" && (!validID(swimlaneID) || s.db.Collection("swimlanes").FindOne(ctx, bson.M{"_id": eq(swimlaneID), "boardId": to.ID}).Err() != nil) {
		return nil, &methodError{"swimlane-not-found", "That swimlane is not on the board the list is being moved to."}
	}
	req := &laneRequest{ToBoardID: to.ID, ToSwimlaneID: swimlaneID, Title: opt[2]}
	// Next to a neighbour list of the destination board, before or after it:
	// its 0-based place among the other lists there.
	if validID(opt[0]) {
		cur, err := s.db.Collection("lists").Find(ctx, bson.M{"boardId": to.ID, "archived": false}, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}}))
		if err == nil {
			var lists []List
			_ = cur.All(ctx, &lists)
			lists = slices.DeleteFunc(lists, func(x List) bool { return x.ID == listID })
			if i := slices.IndexFunc(lists, func(x List) bool { return x.ID == opt[0] }); i >= 0 {
				pos := float64(i + 1)
				if opt[1] == "before" {
					pos = float64(i)
				}
				req.Position = &pos
			}
		}
	}
	if err := s.moveListTo(ctx, u, &b, &to, raw, req); err != nil {
		return nil, &methodError{"move-failed", err.msg}
	}
	return nil, nil
}

// methodInsertComment is the card_comments collection's own insert, as
// WeKan's deny and allow rules let it through (server/permissions/
// cardComments.js): a comment's text, on a card of the board it names - else
// CommentBoundaryBleed - by whoever may comment there, an assigned-only member
// only on a card assigned to them (AssignedBleed). Its author is always the
// caller. Anything else is the numeric 403 "Access denied".
func (s *Server) methodInsertComment(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	denied := &methodError{"403", "Access denied"}
	var doc map[string]json.RawMessage
	if len(args) < 1 || json.Unmarshal(args[0], &doc) != nil {
		return nil, &methodError{"Match.Error", "Expected object"}
	}
	for k := range doc {
		if k != "boardId" && k != "cardId" && k != "userId" && k != "text" {
			return nil, denied
		}
	}
	var boardID, cardID, text string
	_ = json.Unmarshal(doc["boardId"], &boardID)
	_ = json.Unmarshal(doc["cardId"], &cardID)
	_ = json.Unmarshal(doc["text"], &text)
	ctx := r.Context()
	var c Card
	if !validID(cardID) || !validID(boardID) || s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(cardID)}).Decode(&c) != nil || c.BoardID != boardID {
		// As WeKan records it: no user, so the attempt never disables an account.
		s.recordSecurity(r, securityEvent{Key: "authz.comment-card", Action: "blocked", Source: "ddp:comment-insert",
			Detail: "Comment refused because its card does not belong to the requested board."})
		return nil, denied
	}
	var b Board
	if s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(boardID)}).Decode(&b) != nil {
		return nil, denied
	}
	if m := b.member(u.ID); m != nil && m.AssignedOnly() && !u.IsAdmin && !slices.Contains(c.Assignees, u.ID) {
		s.recordSecurity(r, securityEvent{Key: "authz.assigned", Action: "blocked", Source: "ddp:comment-insert", UserID: u.ID,
			Detail: "Assigned-only member commented on a card not assigned to them."})
		return nil, denied
	}
	id, err := s.AddComment(ctx, &b, u, &c, text)
	if err != nil {
		if errors.Is(err, errForbidden) {
			return nil, denied
		}
		return nil, &methodError{"Match.Error", err.Error()}
	}
	return id, nil
}

// methodSetEmail is WeKan's setEmail(email, userId).
func (s *Server) methodSetEmail(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	email, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	userID, err := stringArg(args, 1)
	if err != nil {
		return nil, err
	}
	return nil, s.setEmail(r.Context(), u, userID, email)
}

// methodToggleCardMaximized is WeKan's toggleCardMaximized: the user's own
// choice of opening a card full-screen, turned over.
func (s *Server) methodToggleCardMaximized(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	_, err := s.db.Collection("users").UpdateOne(r.Context(), bson.M{"_id": eq(u.ID)},
		bson.M{"$set": bson.M{"profile.cardMaximized": !u.Profile.CardMaximized}})
	return nil, err
}

// methodArchiveSelectedCards is WeKan's archiveSelectedCards(boardId, cardIds):
// a Multi-Selection to the Archive, by whoever may change the board - every
// card of the board and open, and, for an assigned-only member, every card
// assigned to them (GHSA-6xqf-jh8x-4wxc, UnassignedArchiveBleed). The whole
// selection is checked before the first card is archived.
func (s *Server) methodArchiveSelectedCards(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	boardID, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	var ids []string
	if len(args) < 2 || json.Unmarshal(args[1], &ids) != nil {
		return nil, &methodError{"Match.Error", "Expected [String]"}
	}
	ids = slices.Compact(slices.Sorted(slices.Values(ids)))
	if len(ids) == 0 || len(ids) > 5000 {
		return nil, &methodError{"invalid-card-selection", "invalid-card-selection"}
	}
	ctx := r.Context()
	var b Board
	if !validID(boardID) || s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(boardID)}).Decode(&b) != nil || !b.Can(u.ID, "write") {
		return nil, &methodError{"not-authorized", "not-authorized"}
	}
	in, ok := inIDs(ids)
	if !ok {
		return nil, &methodError{"invalid-card-selection", "invalid-card-selection"}
	}
	cur, err := s.db.Collection("cards").Find(ctx, bson.M{"_id": in, "boardId": b.ID, "archived": false})
	if err != nil {
		return nil, err
	}
	var cards []Card
	_ = cur.All(ctx, &cards)
	if len(cards) != len(ids) {
		return nil, &methodError{"invalid-card-selection", "invalid-card-selection"}
	}
	for i := range cards {
		if !b.Writable(u, "write", &cards[i]) {
			s.recordSecurity(r, securityEvent{Key: "authz.assigned-card-write", Action: "blocked", Source: "method:archiveSelectedCards", UserID: u.ID,
				Detail: "Assigned-only member changed a card not assigned to them."})
			return nil, &methodError{"not-authorized", "not-authorized"}
		}
	}
	now := time.Now().UTC()
	for _, c := range cards {
		if _, err := s.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": c.ID}, bson.M{"$set": bson.M{"archived": true, "archivedAt": now, "modifiedAt": now}}); err != nil {
			return nil, err
		}
		s.activity(ctx, u.ID, "archivedCard", bson.M{"boardId": b.ID, "cardId": c.ID, "listId": c.ListID, "swimlaneId": c.SwimlaneID, "cardTitle": c.Title})
	}
	s.hub.publish(b.ID, "board-changed")
	return map[string]int{"archived": len(cards)}, nil
}

// methodWatch is WeKan's watch(watchableType, id, level) for a list or a
// card: the user, who may read its board, watches it ('watching') or stops
// (null). Only their own watching changes.
func (s *Server) methodWatch(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	kind, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	id, err := stringArg(args, 1)
	if err != nil {
		return nil, err
	}
	var level *string
	if len(args) < 3 || json.Unmarshal(args[2], &level) != nil || (level != nil && *level != "watching") {
		return nil, &methodError{"Match.Error", "watching or null"}
	}
	coll := map[string]string{"list": "lists", "card": "cards"}[kind]
	if coll == "" {
		return nil, &methodError{"error-json-schema", "a list or a card"}
	}
	ctx := r.Context()
	var doc struct {
		BoardID string `bson:"boardId"`
	}
	if !validID(id) || s.db.Collection(coll).FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&doc) != nil {
		return nil, &methodError{"error-" + kind + "-doesNotExist", "not found"}
	}
	if _, err := s.boardFor(ctx, doc.BoardID, u); err != nil {
		return nil, &methodError{"error-board-notAMember", "error-board-notAMember"}
	}
	update := bson.M{"$pull": bson.M{"watchers": u.ID}}
	if level != nil {
		update = bson.M{"$addToSet": bson.M{"watchers": u.ID}}
	}
	if _, err := s.db.Collection(coll).UpdateOne(ctx, bson.M{"_id": eq(id)}, update); err != nil {
		return nil, err
	}
	s.hub.publish(doc.BoardID, "board-changed")
	return true, nil
}
