package web

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Swimlane and list resizing, as WeKan's (client/components/lists/list.js,
// client/components/swimlanes/swimlanes.js, models/lib/listWidth.js): a list's
// width is the board's, the same for everybody - lists.width, or the board's
// one sameWidthForAllListsValue while Board Settings / List's "same width for
// all lists" is on - and changed by dragging the handle on a list's inline
// end, by whoever may change the board. A swimlane's height is each user's
// own (profile.swimlaneHeights), dragged by the handle at its bottom. Board
// Settings' two locks take the handles away, for everybody.
const (
	minListWidth      = 200  // models/lib/listWidth.js MIN_LIST_WIDTH
	maxListWidth      = 1000 // models/users.js: isValidNumber(width, MIN_LIST_WIDTH, 1000)
	defaultListWidth  = 220  // models/lib/listWidth.js DEFAULT_LIST_WIDTH, the same-width default
	minSwimlaneHeight = 100  // swimlanes.js minHeight
	maxSwimlaneHeight = 2000 // swimlanes.js maxHeight
)

// boardResize is how the board's lists and swimlanes are sized for one user.
type boardResize struct {
	SameWidth      bool    // sameWidthForAllLists
	SameWidthValue float64 // sameWidthForAllListsValue
	ListLocked     bool    // listWidthResizeLocked
	LaneLocked     bool    // swimlaneHeightResizeLocked
	Heights        map[string]float64
	// The viewer's own modes: the Set Width pop-over's same width for all
	// lists (profile.fixedListWidthBoards, fixedListWidths), and personal
	// widths while the board allows them (allowsPersonalListWidth,
	// profile.listWidths).
	Fixed      bool
	FixedWidth float64
	Personal   bool
	Personals  map[string]float64
}

func validListWidth(w float64) bool {
	return !math.IsNaN(w) && !math.IsInf(w, 0) && w >= minListWidth && w <= maxListWidth
}

// listWidth is the width a list is drawn at, in whole pixels, or 0 for the
// stylesheet's own, in WeKan's order (effectiveListWidth, resolveListWidth):
// the board's one width while its same width is on, the viewer's own same
// width, the viewer's personal width while the board allows them, the list's
// own width.
func (z boardResize) listWidth(listID string, own float64) int {
	px := func(w float64) int { return int(math.Round(w)) }
	switch {
	case z.SameWidth && validListWidth(z.SameWidthValue):
		return px(z.SameWidthValue)
	case z.SameWidth:
		return defaultListWidth
	case z.Fixed && validListWidth(z.FixedWidth):
		return px(z.FixedWidth)
	case z.Fixed:
		return defaultListWidth
	case z.Personal && validListWidth(z.Personals[listID]):
		return px(z.Personals[listID])
	case validListWidth(own):
		return px(own)
	}
	return 0
}

// canResize: who drags a list's handle (canResizeList) - nobody while it is
// locked; while the board's same width is on, who may change the board; in a
// viewer's own mode, the viewer; else who may change the board.
func (z boardResize) canResize(writer bool) bool {
	switch {
	case z.ListLocked:
		return false
	case z.SameWidth:
		return writer
	case z.Fixed, z.Personal:
		return true
	}
	return writer
}

// laneHeight is the user's height of a swimlane, or 0 for its own height
// (swimlaneHeight: -1 is "auto"); WeKan draws it 5px taller than stored.
func (z boardResize) laneHeight(id string) int {
	h, ok := z.Heights[id]
	if !ok || math.IsNaN(h) || h < minSwimlaneHeight || h > maxSwimlaneHeight {
		return 0
	}
	return int(math.Round(h)) + 5
}

func (s *Server) resizeOf(ctx context.Context, userID, boardID string, rawB bson.M) boardResize {
	z := boardResize{SameWidth: rawB["sameWidthForAllLists"] == true, ListLocked: rawB["listWidthResizeLocked"] == true,
		LaneLocked: rawB["swimlaneHeightResizeLocked"] == true}
	z.SameWidthValue = numberOf(rawB["sameWidthForAllListsValue"])
	if userID == "" {
		return z
	}
	var u struct {
		Profile struct {
			SwimlaneHeights      map[string]map[string]float64 `bson:"swimlaneHeights"`
			FixedListWidthBoards map[string]bool               `bson:"fixedListWidthBoards"`
			FixedListWidths      map[string]float64            `bson:"fixedListWidths"`
			ListWidths           map[string]map[string]float64 `bson:"listWidths"`
		} `bson:"profile"`
	}
	_ = s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(userID)}, options.FindOne().SetProjection(bson.M{
		"profile.swimlaneHeights": 1, "profile.fixedListWidthBoards": 1, "profile.fixedListWidths": 1, "profile.listWidths": 1})).Decode(&u)
	z.Heights = u.Profile.SwimlaneHeights[boardID]
	z.Fixed, z.FixedWidth = u.Profile.FixedListWidthBoards[boardID], u.Profile.FixedListWidths[boardID]
	z.Personal = rawB["allowsPersonalListWidth"] == true
	z.Personals = u.Profile.ListWidths[boardID]
	return z
}

func numberArg(args []json.RawMessage, i int) (float64, error) {
	var n float64
	if len(args) <= i || json.Unmarshal(args[i], &n) != nil {
		return 0, &methodError{"Match.Error", "Expected number"}
	}
	return n, nil
}

// methodApplyListWidth is WeKan's applyListWidth(boardId, listId, width,
// constraint): the list's own width, shared with everybody, by a member
// who may change the board (MutationBleed's sibling: write access, not
// membership), for a list of that board. The constraint is ignored, as
// WeKan's #6409 does.
func (s *Server) methodApplyListWidth(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	boardID, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	listID, err := stringArg(args, 1)
	if err != nil {
		return nil, err
	}
	width, err := numberArg(args, 2)
	if err != nil {
		return nil, err
	}
	if _, err := numberArg(args, 3); err != nil {
		return nil, err
	}
	denied := &methodError{"error-notAuthorized", "error-notAuthorized"}
	var b Board
	if !validID(boardID) || !validID(listID) || s.db.Collection("boards").FindOne(r.Context(), bson.M{"_id": eq(boardID)}).Decode(&b) != nil ||
		!b.Writable(u, "write", nil) {
		return nil, denied
	}
	if n, _ := s.db.Collection("lists").CountDocuments(r.Context(), bson.M{"_id": eq(listID), "boardId": b.ID}); n == 0 {
		return nil, denied
	}
	if !validListWidth(width) {
		return nil, &methodError{"invalid-width", "Width must be between 200 and 1000"}
	}
	if _, err := s.entityUpdateOne(r.Context(), u.ID, "list", bson.M{"_id": eq(listID)}, bson.M{"$set": bson.M{"width": width}}); err != nil {
		return nil, err
	}
	s.hub.publishFrom(b.ID, "board-changed", pageOfRequest(r))
	return true, nil
}

// methodApplySwimlaneHeight is WeKan's applySwimlaneHeightToStorage (and
// applySwimlaneHeight, the same for a signed-in user): the user's own height
// of a swimlane of a board they can see. The ids become keys of the user's
// heights map, so never a "$" or a "." (assertSafeMapKey, PrototypeBleed).
func (s *Server) methodApplySwimlaneHeight(source string) methodFunc {
	return func(r *http.Request, u *User, args []json.RawMessage) (any, error) {
		boardID, err := stringArg(args, 0)
		if err != nil {
			return nil, err
		}
		laneID, err := stringArg(args, 1)
		if err != nil {
			return nil, err
		}
		height, err := numberArg(args, 2)
		if err != nil {
			return nil, err
		}
		if math.IsNaN(height) || height < minSwimlaneHeight || height > maxSwimlaneHeight {
			return nil, &methodError{"invalid-height", "Height must be between 100 and 2000"}
		}
		if err := s.setProfileMap(r, u, source, "swimlaneHeights", height, boardID, laneID); err != nil {
			return nil, err
		}
		return nil, nil
	}
}

// sameWidthValueWrite is a /boards/update that sets only the board's one
// list width while same width is on - dragging a list's handle then - which
// WeKan lets any member with write access make, not only a board admin
// (server/permissions/boards.js canUpdateBoardSameWidthValue); the schema's
// minimum is 200.
func sameWidthValueWrite(set map[string]any) (float64, bool) {
	if len(set) != 1 {
		return 0, false
	}
	v, ok := set["sameWidthForAllListsValue"].(float64)
	return v, ok
}

// pageOfRequest is the page that made the change, so it is not told of its
// own: a form's X-Weddy-Page, or a script's call's X-Weddy-Live.
func pageOfRequest(r *http.Request) string {
	if p := r.Header.Get("X-Weddy-Page"); p != "" {
		return p
	}
	return r.Header.Get("X-Weddy-Live")
}

// setProfileMap writes profile.<mapName>.<board>[.<id>] = value in the
// user's own profile, as WeKan's user setters do, on a board the user can
// see. The keys become the path, so they are refused first when they would
// make it something else - a "$" or a "." (assertSafeMapKey, PrototypeBleed) -
// and must be ids.
func (s *Server) setProfileMap(r *http.Request, u *User, source, mapName string, value any, boardID string, ids ...string) error {
	keys := append([]string{boardID}, ids...)
	if s.refuseMapKeys(r, u, source, keys...) {
		return &methodError{"invalid-map-key", "invalid-map-key"}
	}
	for _, k := range keys {
		if !validID(k) {
			return &methodError{"Match.Error", "Expected ids"}
		}
	}
	if _, err := s.boardFor(r.Context(), boardID, u); err != nil {
		return &methodError{"not-found", "Board not found"}
	}
	path := "profile." + mapName + "." + strings.Join(keys, ".")
	_, err := s.db.Collection("users").UpdateOne(r.Context(), bson.M{"_id": eq(u.ID)}, bson.M{"$set": bson.M{path: value}})
	return err
}

// methodSetFixedListWidthEnabled is WeKan's setFixedListWidthEnabled
// (#5729): the viewer's own same width for all lists, on one board.
func (s *Server) methodSetFixedListWidthEnabled(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	boardID, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	var enabled bool
	if len(args) < 2 || json.Unmarshal(args[1], &enabled) != nil {
		return nil, &methodError{"Match.Error", "Expected boolean"}
	}
	if err := s.setProfileMap(r, u, "setFixedListWidthEnabled", "fixedListWidthBoards", enabled, boardID); err != nil {
		return nil, err
	}
	return true, nil
}

// methodSetFixedListWidth is WeKan's setFixedListWidth: the one width of
// every list while the viewer's own same width is on - under 200 refused.
func (s *Server) methodSetFixedListWidth(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	boardID, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	width, err := numberArg(args, 1)
	if err != nil {
		return nil, err
	}
	if !validListWidth(width) {
		return nil, &methodError{"invalid-width", "Width must be >= 200"}
	}
	if err := s.setProfileMap(r, u, "setFixedListWidth", "fixedListWidths", width, boardID); err != nil {
		return nil, err
	}
	return true, nil
}

// methodApplyListWidthToStorage is WeKan's applyListWidthToStorage: the
// viewer's personal width of one list of the board (and its constraint, kept
// as WeKan keeps it), which the board draws while it allows personal widths.
func (s *Server) methodApplyListWidthToStorage(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	boardID, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	listID, err := stringArg(args, 1)
	if err != nil {
		return nil, err
	}
	width, err := numberArg(args, 2)
	if err != nil {
		return nil, err
	}
	constraint, err := numberArg(args, 3)
	if err != nil {
		return nil, err
	}
	if !validListWidth(width) {
		return nil, &methodError{"invalid-width", "Width must be between 200 and 1000"}
	}
	if n, _ := s.db.Collection("lists").CountDocuments(r.Context(), bson.M{"_id": eq(listID), "boardId": eq(boardID)}); n == 0 {
		return nil, &methodError{"error-notAuthorized", "error-notAuthorized"}
	}
	if err := s.setProfileMap(r, u, "applyListWidthToStorage", "listWidths", width, boardID, listID); err != nil {
		return nil, err
	}
	if err := s.setProfileMap(r, u, "applyListWidthToStorage", "listConstraints", constraint, boardID, listID); err != nil {
		return nil, err
	}
	return nil, nil
}
