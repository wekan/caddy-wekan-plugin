package web

// WeKan's copyCard method (server/models/cards.js): (cardId, boardId,
// swimlaneId, listId, insertAtTop, mergeCardValues). The copy dialog edits
// the copy's title and description, nothing else - CopyIdentityBleed
// (models/lib/cardCopyOverrides.js): an _id, or any other field, used to make
// WeKan copy ANOTHER card's private children. Such a field is refused before
// anything is read or written, and recorded (authz.card-copy-overrides); a
// title or description that is not text is ordinary bad input, refused
// unrecorded. The destination's swimlane and list must be on its board
// (server/lib/cardCopyDestination.js).

import (
	"encoding/json"
	"net/http"
	"slices"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func (s *Server) methodCopyCard(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	ids := make([]string, 4)
	for i := range ids {
		v, err := stringArg(args, i)
		if err != nil {
			return nil, err
		}
		ids[i] = v
	}
	cardID, boardID, swimlaneID, listID := ids[0], ids[1], ids[2], ids[3]
	var insertAtTop bool
	if len(args) < 6 || json.Unmarshal(args[4], &insertAtTop) != nil {
		return nil, &methodError{"Match.Error", "Expected boolean"}
	}
	var values map[string]any
	if json.Unmarshal(args[5], &values) != nil || values == nil {
		return nil, &methodError{"Match.Error", "Expected object"}
	}
	ctx := r.Context()
	var card bson.M
	if !validID(cardID) || s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(cardID)}).Decode(&card) != nil {
		return nil, &methodError{"not-found", "not-found"}
	}
	from, err := s.boardFor(ctx, docString(card, "boardId"), u)
	if err != nil || !from.MemberOf(u.ID) { // not a write: the source is only read, WeKan's allowIsBoardMember
		return nil, &methodError{"not-authorized", "not-authorized"}
	}
	// AssignedBleed: an assigned-only member copies only a card assigned to them.
	if m := from.member(u.ID); m != nil && m.AssignedOnly() && !slices.Contains(stringsOf(card["assignees"]), u.ID) {
		return nil, &methodError{"not-authorized", "not-authorized"}
	}
	to, err := s.boardFor(ctx, boardID, u)
	if err != nil || !to.Writable(u, "write", nil) {
		return nil, &methodError{"not-authorized", "not-authorized"}
	}
	// A forged merge is refused before anything is copied (WeKan's
	// assertFieldWrite on { ...card, ...mergeCardValues }).
	merged := map[string]any{}
	for k, v := range card {
		merged[k] = v
	}
	for k, v := range values {
		merged[k] = v
	}
	if s.fieldWriteRefused(r, u, from, card, merged, "method:copyCard") {
		return nil, &methodError{"not-authorized", "Protected custom field"}
	}
	attempt, bad := false, false
	for k, v := range values {
		if k != "title" && k != "description" {
			attempt = true
		} else if _, isText := v.(string); !isText {
			bad = true
		}
	}
	if attempt {
		s.recordSecurity(r, securityEvent{Key: "authz.card-copy-overrides", Action: "blocked", Source: "method:copyCard", UserID: u.ID,
			Detail: "Unsupported card-copy overrides refused"})
	}
	if attempt || bad {
		return nil, &methodError{"bad-request", "Invalid card copy overrides"}
	}
	if !validID(swimlaneID) || !validID(listID) ||
		s.db.Collection("swimlanes").FindOne(ctx, bson.M{"_id": eq(swimlaneID), "boardId": eq(to.ID), "deletedAt": nil}).Err() != nil ||
		s.db.Collection("lists").FindOne(ctx, bson.M{"_id": eq(listID), "boardId": eq(to.ID), "deletedAt": nil}).Err() != nil {
		return nil, &methodError{"invalid-copy-destination", "Choose a list and swimlane on the destination board."}
	}
	if s.refuseInList(ctx, to, listID, u, "write", "method:copyCard") {
		return nil, &methodError{"not-authorized", "not-authorized"}
	}
	copied := bson.M{}
	for k, v := range card {
		copied[k] = v
	}
	for k, v := range values {
		copied[k] = v
	}
	// The copy goes to the top or the bottom of the destination list.
	order := 1
	if insertAtTop {
		order = -1
	}
	var edge struct {
		Sort float64 `bson:"sort"`
	}
	if s.db.Collection("cards").FindOne(ctx, bson.M{"boardId": eq(to.ID), "listId": eq(listID), "swimlaneId": eq(swimlaneID), "archived": false},
		options.FindOne().SetSort(bson.D{{Key: "sort", Value: -order}})).Decode(&edge) == nil {
		copied["sort"] = edge.Sort + float64(order)
	} else {
		copied["sort"] = 0.0
	}
	id, err := s.copyCardTo(r, u, copied, from, to, swimlaneID, listID)
	if err != nil {
		return nil, err
	}
	s.hub.publishFrom(to.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	return id, nil
}
