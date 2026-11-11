package web

// A list's WIP limit, as WeKan keeps it (models/lists.js): wipLimit
// { value, enabled, soft }. Its methods are a board admin's
// (server/models/lists.js): applyWipLimit sets the value - one when what was
// typed is no number of at least one - and enableWipLimit and enableSoftLimit
// toggle it on and off, and between hard and soft.
//
// A HARD limit holds the list at its value: a card restored from the Archive
// into a list that has as many cards - or more, a limit lowered below the count
// - goes to a list with room instead (#6772, models/lib/wipRestore.js). A soft
// limit only warns. The count is every open card of the list, in every
// swimlane, whatever a filter shows (#2095).

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// wipLimit is a list's.
type wipLimit struct {
	Value   float64 `bson:"value"`
	Enabled bool    `bson:"enabled"`
	Soft    bool    `bson:"soft"`
}

// blocksRestore is wipLimitBlocksRestore.
func (w wipLimit) blocksRestore(count int64) bool {
	return w.Enabled && !w.Soft && float64(count) >= w.Value
}

// wipList is a list as the WIP rules read it.
type wipList struct {
	ID       string   `bson:"_id"`
	Title    string   `bson:"title"`
	Sort     float64  `bson:"sort"`
	BoardID  string   `bson:"boardId"`
	Swimlane string   `bson:"swimlaneId"`
	Archived bool     `bson:"archived"`
	WipLimit wipLimit `bson:"wipLimit"`
}

func (s *Server) openCardCount(ctx context.Context, listID string) int64 {
	n, _ := s.db.Collection("cards").CountDocuments(ctx, bson.M{"listId": eq(listID), "archived": false})
	return n
}

// listHasRoom: is listID an open list of the board that a hard limit has not
// filled?
func (s *Server) listHasRoom(ctx context.Context, boardID, listID string) (*wipList, bool) {
	var l wipList
	if !validID(listID) || s.db.Collection("lists").FindOne(ctx, bson.M{"_id": eq(listID), "boardId": eq(boardID), "archived": false}).Decode(&l) != nil {
		return nil, false
	}
	return &l, !l.WipLimit.blocksRestore(s.openCardCount(ctx, l.ID))
}

// listsWithRoom: the board's open lists a card can be restored into.
func (s *Server) listsWithRoom(ctx context.Context, boardID string) []wipList {
	cur, err := s.db.Collection("lists").Find(ctx, bson.M{"boardId": eq(boardID), "archived": false},
		options.Find().SetSort(bson.D{{Key: "sort", Value: 1}, {Key: "title", Value: 1}}))
	if err != nil {
		return nil
	}
	var lists []wipList
	_ = cur.All(ctx, &lists)
	out := lists[:0]
	for _, l := range lists {
		if !l.WipLimit.blocksRestore(s.openCardCount(ctx, l.ID)) {
			out = append(out, l)
		}
	}
	return out
}

// wipListFor is the list of the method's first argument, for a board admin.
func (s *Server) wipListFor(r *http.Request, u *User, args []json.RawMessage) (*wipList, error) {
	listID, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	var l wipList
	if !validID(listID) || s.db.Collection("lists").FindOne(r.Context(), bson.M{"_id": eq(listID)}).Decode(&l) != nil {
		return nil, &methodError{"list-not-found", "List not found"}
	}
	var b Board
	if s.db.Collection("boards").FindOne(r.Context(), bson.M{"_id": eq(l.BoardID)}).Decode(&b) != nil || !b.AdminOf(u.ID) {
		return nil, &methodError{"not-authorized", "You must be a board admin to modify WIP limits."}
	}
	return &l, nil
}

func (s *Server) setWip(r *http.Request, l *wipList, set bson.M) error {
	set["modifiedAt"] = time.Now().UTC()
	if _, err := s.db.Collection("lists").UpdateOne(r.Context(), bson.M{"_id": eq(l.ID)}, bson.M{"$set": set}); err != nil {
		return err
	}
	// Open pages of the board draw the list's limit again.
	s.hub.publishFrom(l.BoardID, "board-changed", r.Header.Get("X-Weddy-Page"))
	return nil
}

func (s *Server) wipMethods() map[string]methodFunc {
	return map[string]methodFunc{
		"applyWipLimit": func(r *http.Request, u *User, args []json.RawMessage) (any, error) {
			var limit float64
			if len(args) < 2 || json.Unmarshal(args[1], &limit) != nil {
				return nil, &methodError{"Match.Error", "Expected number"}
			}
			l, err := s.wipListFor(r, u, args)
			if err != nil {
				return nil, err
			}
			if math.IsNaN(limit) || math.IsInf(limit, 0) || limit < 1 {
				limit = 1
			}
			return nil, s.setWip(r, l, bson.M{"wipLimit.value": limit})
		},
		"enableWipLimit": func(r *http.Request, u *User, args []json.RawMessage) (any, error) {
			l, err := s.wipListFor(r, u, args)
			if err != nil {
				return nil, err
			}
			set := bson.M{"wipLimit.enabled": !l.WipLimit.Enabled}
			if l.WipLimit.Value == 0 {
				set["wipLimit.value"] = 1.0
			}
			return nil, s.setWip(r, l, set)
		},
		"enableSoftLimit": func(r *http.Request, u *User, args []json.RawMessage) (any, error) {
			l, err := s.wipListFor(r, u, args)
			if err != nil {
				return nil, err
			}
			return nil, s.setWip(r, l, bson.M{"wipLimit.soft": !l.WipLimit.Soft})
		},
	}
}

// restoreBlocked is what the Archive shows for an archived card that cannot
// go back to its own list: the text, and the lists it can go to instead.
type restoreBlocked struct {
	Reason  string
	Targets []wipList
	NoRoom  string
}

// restoreBlockFor is nil when the card goes back to its own list.
func (s *Server) restoreBlockFor(ctx context.Context, boardID, listID, lang string, targets func() []wipList) *restoreBlocked {
	l, room := s.listHasRoom(ctx, boardID, listID)
	if room {
		return nil
	}
	rb := &restoreBlocked{Targets: targets(), NoRoom: tr(lang, "restore-card-no-list-with-room")}
	if l != nil {
		rb.Reason = strings.ReplaceAll(tr(lang, "restore-card-wip-limit-reached"), "__list__", l.Title)
	} else {
		rb.Reason = tr(lang, "error-list-doesNotExist")
	}
	return rb
}
