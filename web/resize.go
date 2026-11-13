package web

import (
	"context"
	"encoding/json"
	"math"
	"net/http"

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
}

func validListWidth(w float64) bool {
	return !math.IsNaN(w) && !math.IsInf(w, 0) && w >= minListWidth && w <= maxListWidth
}

// listWidth is the width a list is drawn at, in whole pixels, or 0 for the
// stylesheet's own (resolveListWidth): the board's one width while same width
// is on, else the list's own when it is one.
func (z boardResize) listWidth(own float64) int {
	if z.SameWidth {
		if validListWidth(z.SameWidthValue) {
			return int(math.Round(z.SameWidthValue))
		}
		return defaultListWidth
	}
	if validListWidth(own) {
		return int(math.Round(own))
	}
	return 0
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
	switch v := rawB["sameWidthForAllListsValue"].(type) {
	case float64:
		z.SameWidthValue = v
	case int32:
		z.SameWidthValue = float64(v)
	case int64:
		z.SameWidthValue = float64(v)
	}
	if userID == "" {
		return z
	}
	var u struct {
		Profile struct {
			SwimlaneHeights map[string]map[string]float64 `bson:"swimlaneHeights"`
		} `bson:"profile"`
	}
	_ = s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(userID)},
		options.FindOne().SetProjection(bson.M{"profile.swimlaneHeights": 1})).Decode(&u)
	z.Heights = u.Profile.SwimlaneHeights[boardID]
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
		if s.refuseMapKeys(r, u, source, boardID, laneID) {
			return nil, &methodError{"invalid-map-key", "invalid-map-key"}
		}
		if !validID(boardID) || !validID(laneID) {
			return nil, &methodError{"Match.Error", "Expected ids"}
		}
		if math.IsNaN(height) || height < minSwimlaneHeight || height > maxSwimlaneHeight {
			return nil, &methodError{"invalid-height", "Height must be between 100 and 2000"}
		}
		if _, err := s.boardFor(r.Context(), boardID, u); err != nil {
			return nil, &methodError{"not-found", "Board not found"}
		}
		path := "profile.swimlaneHeights." + boardID + "." + laneID
		if _, err := s.db.Collection("users").UpdateOne(r.Context(), bson.M{"_id": eq(u.ID)}, bson.M{"$set": bson.M{path: height}}); err != nil {
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
