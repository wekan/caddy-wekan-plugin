package web

import (
	"context"
	"encoding/json"
	"net/http"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// WeKan's per-user folds beside the lists' (server/models/users.js): a
// swimlane folded on a board (setSwimlaneCollapsedState,
// profile.collapsedSwimlanes[board][swimlane]), a minicard folded
// (setCardCollapsedState, profile.collapsedCards[board][card]) and a card's
// section - a checklist's, "checklist-<id>", the same key on the minicard and
// in the opened card (#1591) - folded (setCollapsedCardSection,
// profile.collapsedCardSections[card][section]). Each key becomes a map key
// of the user's profile, so each is refused as PrototypeBleed unless safe
// (WeKan's assertSafeMapKey), and each is an id of something the user sees.

// boardFolds are the user's folds of one board, for its page.
type boardFolds struct {
	Swimlanes, Cards map[string]bool
	Sections         map[string]map[string]bool
}

func (s *Server) foldsOf(ctx context.Context, userID, boardID string) boardFolds {
	var u struct {
		Profile struct {
			CollapsedSwimlanes    map[string]map[string]bool `bson:"collapsedSwimlanes"`
			CollapsedCards        map[string]map[string]bool `bson:"collapsedCards"`
			CollapsedCardSections map[string]map[string]bool `bson:"collapsedCardSections"`
		} `bson:"profile"`
	}
	_ = s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(userID)}, options.FindOne().SetProjection(bson.M{
		"profile.collapsedSwimlanes": 1, "profile.collapsedCards": 1, "profile.collapsedCardSections": 1})).Decode(&u)
	return boardFolds{Swimlanes: u.Profile.CollapsedSwimlanes[boardID], Cards: u.Profile.CollapsedCards[boardID],
		Sections: u.Profile.CollapsedCardSections}
}

// setFold writes path.<a>.<b> = folded in the user's profile, after the
// checks every fold method makes.
func (s *Server) setFold(r *http.Request, u *User, source, mapName, a, b string, folded bool) (any, error) {
	if s.refuseMapKeys(r, u, source, a, b) {
		return nil, &methodError{"invalid-map-key", "invalid-map-key"}
	}
	if !validID(a) || b == "" || len(b) > 64 {
		return nil, &methodError{"Match.Error", "Expected ids"}
	}
	if _, err := s.db.Collection("users").UpdateOne(r.Context(), bson.M{"_id": eq(u.ID)},
		bson.M{"$set": bson.M{"profile." + mapName + "." + a + "." + b: folded}}); err != nil {
		return nil, err
	}
	return nil, nil
}

func foldArgs(args []json.RawMessage) (string, string, bool, error) {
	a, err := stringArg(args, 0)
	if err != nil {
		return "", "", false, err
	}
	b, err := stringArg(args, 1)
	if err != nil {
		return "", "", false, err
	}
	var folded bool
	if len(args) < 3 || json.Unmarshal(args[2], &folded) != nil {
		return "", "", false, &methodError{"Match.Error", "Expected boolean"}
	}
	return a, b, folded, nil
}

// methodSetSwimlaneCollapsedState is WeKan's setSwimlaneCollapsedState(boardId, swimlaneId, collapsed).
func (s *Server) methodSetSwimlaneCollapsedState(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	boardID, laneID, folded, err := foldArgs(args)
	if err != nil {
		return nil, err
	}
	if s.refuseMapKeys(r, u, "setSwimlaneCollapsedState", boardID, laneID) {
		return nil, &methodError{"invalid-map-key", "invalid-map-key"}
	}
	if _, err := s.boardFor(r.Context(), boardID, u); err != nil || !validID(laneID) {
		return nil, &methodError{"not-found", "Board not found"}
	}
	return s.setFold(r, u, "setSwimlaneCollapsedState", "collapsedSwimlanes", boardID, laneID, folded)
}

// methodSetCardCollapsedState is WeKan's setCardCollapsedState(boardId, cardId, collapsed).
func (s *Server) methodSetCardCollapsedState(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	boardID, cardID, folded, err := foldArgs(args)
	if err != nil {
		return nil, err
	}
	if s.refuseMapKeys(r, u, "setCardCollapsedState", boardID, cardID) {
		return nil, &methodError{"invalid-map-key", "invalid-map-key"}
	}
	if _, err := s.boardFor(r.Context(), boardID, u); err != nil || !validID(cardID) {
		return nil, &methodError{"not-found", "Board not found"}
	}
	return s.setFold(r, u, "setCardCollapsedState", "collapsedCards", boardID, cardID, folded)
}

// methodSetCollapsedCardSection is WeKan's user.setCollapsedCardSection(cardId,
// sectionKey, collapsed), for a card the user sees: a section of WeKan's
// card layout, or a checklist's "checklist-<id>".
func (s *Server) methodSetCollapsedCardSection(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	cardID, section, folded, err := foldArgs(args)
	if err != nil {
		return nil, err
	}
	if s.refuseMapKeys(r, u, "setCollapsedCardSection", cardID, section) {
		return nil, &methodError{"invalid-map-key", "invalid-map-key"}
	}
	var c Card
	if !validID(cardID) || s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": eq(cardID)}).Decode(&c) != nil {
		return nil, &methodError{"not-found", "not-found"}
	}
	if _, err := s.boardFor(r.Context(), c.BoardID, u); err != nil {
		return nil, &methodError{"not-found", "not-found"}
	}
	if len(section) > len("checklist-") && section[:len("checklist-")] == "checklist-" {
		if !validID(section[len("checklist-"):]) {
			return nil, &methodError{"Match.Error", "Expected a section"}
		}
	} else if cardSectionOf(section) == nil && !validSectionKey(section) {
		return nil, &methodError{"Match.Error", "Expected a section"}
	}
	return s.setFold(r, u, "setCollapsedCardSection", "collapsedCardSections", cardID, section, folded)
}

// validSectionKey is a key of WeKan's card layout's sections.
func validSectionKey(k string) bool {
	for _, sec := range cardLayout.Sections {
		if sec.Key == k {
			return true
		}
	}
	return false
}
