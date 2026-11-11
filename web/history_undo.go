package web

// Change history, step 3: a list's soft delete, recorded, and undo and redo
// of one's own last change on a board (WeKan's server/models/lists.js
// softRemoveList and lists.softRemove, server/models/changeHistory.js
// changeHistory.undoLast and changeHistory.redoLast, models/lib/
// listLifecycleRestore.js, models/lib/undoRedoSelection.js).
//
// A deleted list and the cards deleted with it share a batch id: undo
// clears the marks of that batch only, so a card deleted on its own before
// stays deleted; redo marks the list and its live cards again. Each undo
// and redo is recorded too, as a checkpoint ("restored") row - in the
// timeline, out of the undo stack. A row is claimed before it is applied,
// so two tabs pressing undo at once apply it once. A row whose integrity
// fails in its chain is refused and recorded (HistoryIntegrity).
//
// The other kinds of change - titles, positions, cards' fields - are
// recorded and undone in later steps; until then undo answers not-applicable
// for such a row and leaves it on the stack.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// softDeleteSet is WeKan's: when, who, and the batch.
func softDeleteSet(userID, batchID string, at time.Time) bson.M {
	set := bson.M{"deletedAt": at}
	if userID != "" {
		set["deletedBy"] = userID
	}
	if batchID != "" {
		set["deleteBatchId"] = batchID
	}
	return set
}

var softRestore = bson.M{"$unset": bson.M{"deletedAt": "", "deletedBy": "", "deleteBatchId": ""}}

// softRemoveList is WeKan's: the list and its live cards marked deleted as
// one batch, and the lifecycle row recorded.
func (s *Server) softRemoveList(ctx context.Context, u *User, list bson.M) string {
	at := time.Now().UTC().Truncate(time.Millisecond)
	batch := randomID()
	listID, boardID := docString(list, "_id"), docString(list, "boardId")
	_, _ = s.db.Collection("cards").UpdateMany(ctx, bson.M{"listId": eq(listID), "boardId": eq(boardID), "deletedAt": nil},
		bson.M{"$set": softDeleteSet(u.ID, batch, at)})
	_, _ = s.db.Collection("lists").UpdateOne(ctx, bson.M{"_id": eq(listID)}, bson.M{"$set": softDeleteSet(u.ID, batch, at)})
	s.recordHistory(ctx, historyChange{BoardID: boardID, SwimlaneID: list["swimlaneId"], ListID: listID, EntityType: "list", EntityID: listID,
		Group: "lifecycle", ChangeType: "removed", PreviousContent: map[string]any{"deleted": false, "deleteBatchId": batch},
		NewContent: map[string]any{"deleted": true, "deletedAt": at, "deleteBatchId": batch}, UserID: u.ID, BatchID: batch})
	s.hub.publish(boardID, "board-changed")
	return batch
}

// historyBoardWrite is requireBoardWrite: a board whose role table lets the
// caller write.
func (s *Server) historyBoardWrite(ctx context.Context, u *User, boardID string) (*Board, error) {
	var b Board
	if !validID(boardID) || s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(boardID)}).Decode(&b) != nil || !(u.IsAdmin || b.Can(u.ID, "write")) {
		return nil, &methodError{"not-authorized", "You cannot change this board."}
	}
	return &b, nil
}

func (s *Server) methodListSoftRemove(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	listID, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	ctx := r.Context()
	var list bson.M
	if !validID(listID) || s.db.Collection("lists").FindOne(ctx, bson.M{"_id": eq(listID)}).Decode(&list) != nil {
		return nil, &methodError{"not-found", "List not found"}
	}
	if _, err := s.historyBoardWrite(ctx, u, docString(list, "boardId")); err != nil {
		return nil, &methodError{"not-authorized", "You cannot modify this board."}
	}
	if list["deletedAt"] != nil {
		return map[string]any{"deleted": true, "batchId": list["deleteBatchId"]}, nil
	}
	return map[string]any{"deleted": true, "batchId": s.softRemoveList(ctx, u, list)}, nil
}

// requireHistoryIntegrity is WeKan's: the row's own hash, its predecessor
// on the board, and it the only one after that predecessor (or the only
// root). A row from before the hashes existed passes.
func (s *Server) requireHistoryIntegrity(r *http.Request, u *User, row map[string]any) error {
	if docString(row, "integrityHash") == "" {
		return nil
	}
	ctx := r.Context()
	coll := s.db.Collection("changeHistory")
	boardID := docString(row, "boardId")
	valid := rowHashIsValid(row)
	if prev := docString(row, "previousHash"); valid && prev != "" {
		valid = coll.FindOne(ctx, bson.M{"boardId": eq(boardID), "integrityHash": eq(prev)}).Err() == nil
		if valid {
			n, _ := coll.CountDocuments(ctx, bson.M{"boardId": eq(boardID), "previousHash": eq(prev)})
			valid = n == 1
		}
	} else if valid {
		n, _ := coll.CountDocuments(ctx, bson.M{"boardId": eq(boardID), "previousHash": nil, "integrityHash": bson.M{"$nin": bson.A{nil, ""}}})
		valid = n == 1
	}
	if valid {
		return nil
	}
	observed := "row-changed"
	if rowHashIsValid(row) {
		observed = "row-valid"
	}
	// Detected, never the caller's attempt: their account is not disabled.
	s.recordSecurity(r, securityEvent{Key: "integrity.history", Action: "detected", Source: "changeHistory", UserID: u.ID,
		Detail: "history row=" + docString(row, "_id") + " board=" + boardID + " observed=" + observed + "; restore/undo/redo refused"})
	return &methodError{"history-integrity-failed", "Change history verification failed; the operation was refused."}
}

// currentHistoryContent is currentContentOf for what Weddy applies: a
// list's lifecycle.
func (s *Server) currentHistoryContent(ctx context.Context, row map[string]any) any {
	if docString(row, "entityType") == "rule" {
		return s.ruleSnapshot(ctx, docString(row, "entityId"))
	}
	if docString(row, "entityType") == "scrum" {
		return s.scrumHistorySnapshot(ctx, row)
	}
	for _, side := range []string{"newContent", "previousContent"} {
		if c, ok := row[side].(map[string]any); ok {
			if field, ok := c["field"].(string); ok {
				var d bson.M
				if s.db.Collection(historyCollections[docString(row, "entityType")]).FindOne(ctx, bson.M{"_id": eq(docString(row, "entityId"))}).Decode(&d) != nil {
					return nil
				}
				v, present := plain(d).(map[string]any)[field]
				return historyContent(field, v, present)
			}
		}
	}
	if docString(row, "entityType") != "list" || docString(row, "group") != "lifecycle" {
		return nil
	}
	var l bson.M
	if s.db.Collection("lists").FindOne(ctx, bson.M{"_id": eq(docString(row, "entityId"))}).Decode(&l) != nil {
		return nil
	}
	batch := docString(l, "deleteBatchId")
	for _, side := range []string{"newContent", "previousContent"} {
		if batch == "" {
			if m, ok := row[side].(map[string]any); ok {
				batch, _ = m["deleteBatchId"].(string)
			}
		}
	}
	if batch == "" {
		batch = docString(row, "batchId")
	}
	var at any
	if v, ok := plain(l["deletedAt"]).(time.Time); ok {
		at = v
	}
	var b any
	if batch != "" {
		b = batch
	}
	return map[string]any{"deleted": l["deletedAt"] != nil, "deletedAt": at, "deleteBatchId": b}
}

// applyHistoryRow is applyRow for a list's lifecycle (listLifecyclePlan):
// false when it cannot be applied.
func (s *Server) applyHistoryRow(ctx context.Context, row map[string]any, direction string) bool {
	content, _ := row["newContent"].(map[string]any)
	switch {
	case direction == "undo":
		content, _ = row["previousContent"].(map[string]any)
	case direction == "restore" && content == nil:
		// A removal: restoring it puts back what it removed.
		content, _ = row["previousContent"].(map[string]any)
	}
	// An attachment's lifecycle: deleted or restored again (applyAttachmentContent).
	if docString(row, "entityType") == "attachment" && docString(row, "group") == "lifecycle" && content != nil {
		deleted, ok := content["deleted"].(bool)
		if !ok {
			return false
		}
		var a Attachment
		if s.db.Collection("attachments").FindOne(ctx, bson.M{"_id": eq(docString(row, "entityId"))}).Decode(&a) != nil {
			return false
		}
		r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/", nil)
		by := &User{ID: docString(row, "userId")}
		if id := docString(row, "restoredByUserId"); id != "" && !deleted {
			by.ID = id
		}
		var err error
		if deleted {
			_, err = s.softDeleteAttachmentAs(r, &a, by, false)
		} else {
			err = s.restoreAttachmentAs(r, &a, by, false)
		}
		s.hub.publish(a.Meta.BoardID, "board-changed")
		return err == nil
	}
	// A card's position: moved back, across boards too (Card.move).
	if docString(row, "entityType") == "card" && docString(row, "group") == "position" && content != nil {
		return s.applyCardPosition(ctx, row, content)
	}
	// A field's content: the field set back (applyFieldContent), directly -
	// the reversal's own row is written by the caller, not by this write.
	if field, ok := content["field"].(string); ok && historyGroupFor(docString(row, "entityType"), field) != "" {
		value, ok := historyValue(content)
		coll := historyCollections[docString(row, "entityType")]
		if !ok || coll == "" {
			return false
		}
		res, err := s.db.Collection(coll).UpdateOne(ctx, bson.M{"_id": eq(docString(row, "entityId"))}, bson.M{"$set": bson.M{field: value}})
		if err != nil || res.MatchedCount == 0 {
			return false
		}
		s.hub.publish(docString(row, "boardId"), "board-changed")
		return true
	}
	if docString(row, "entityType") != "list" || docString(row, "group") != "lifecycle" || content == nil {
		return false
	}
	deleted, ok := content["deleted"].(bool)
	if !ok {
		return false
	}
	var l bson.M
	listID := docString(row, "entityId")
	if s.db.Collection("lists").FindOne(ctx, bson.M{"_id": eq(listID)}).Decode(&l) != nil || docString(l, "boardId") != docString(row, "boardId") {
		return false
	}
	batch, _ := content["deleteBatchId"].(string)
	for _, b := range []string{docString(l, "deleteBatchId"), docString(row, "batchId"), docString(row, "_id")} {
		if batch == "" {
			batch = b
		}
	}
	scope := bson.M{"boardId": eq(docString(l, "boardId")), "listId": eq(listID)}
	if deleted {
		at := time.Now().UTC().Truncate(time.Millisecond)
		if t, ok := plain(content["deletedAt"]).(time.Time); ok {
			at = t
		}
		set := bson.M{"$set": softDeleteSet(docString(row, "userId"), batch, at)}
		scope["deletedAt"] = nil
		_, _ = s.db.Collection("cards").UpdateMany(ctx, scope, set)
		_, _ = s.db.Collection("lists").UpdateOne(ctx, bson.M{"_id": eq(listID)}, set)
	} else {
		// Only this batch: a card deleted on its own keeps its own state.
		scope["deleteBatchId"] = eq(batch)
		_, _ = s.db.Collection("cards").UpdateMany(ctx, scope, softRestore)
		_, _ = s.db.Collection("lists").UpdateOne(ctx, bson.M{"_id": eq(listID)}, softRestore)
	}
	s.hub.publish(docString(l, "boardId"), "board-changed")
	return true
}

// reverseLast is undoLast (direction "undo") and redoLast ("redo").
// historyStackTop is the row an undo or redo takes: the user's newest
// change on the board, or most recently undone one.
func (s *Server) historyStackTop(ctx context.Context, u *User, boardID, direction string) map[string]any {
	filter := bson.M{"userId": eq(u.ID), "boardId": eq(boardID), "undone": direction == "redo", "isCheckpoint": bson.M{"$ne": true}, "superseded": bson.M{"$ne": true}}
	sortBy := "createdAt"
	if direction == "redo" {
		sortBy = "undoneAt"
	}
	var raw bson.M
	if s.db.Collection("changeHistory").FindOne(ctx, filter, options.FindOne().SetSort(bson.D{{Key: sortBy, Value: -1}})).Decode(&raw) != nil {
		return nil
	}
	return plain(raw).(map[string]any)
}

func (s *Server) reverseLast(r *http.Request, u *User, boardID, direction string, requestID *string) (map[string]any, error) {
	ctx := r.Context()
	if _, err := s.historyBoardWrite(ctx, u, boardID); err != nil {
		return nil, err
	}
	key := map[string]string{"undo": "undone", "redo": "redone"}[direction]
	if requestID != nil {
		return s.requestedScrumReversal(r, u, boardID, direction, *requestID)
	}
	// An interrupted Scrum operation of this user's, in this direction, is
	// the one to finish first (pendingScrumHistoryRow).
	row, err := s.pendingScrumHistoryRow(ctx, boardID, u.ID, direction)
	if err != nil {
		return nil, err
	}
	if row == nil {
		row = s.historyStackTop(ctx, u, boardID, direction)
	}
	if row == nil {
		return map[string]any{key: false}, nil
	}
	if err := s.requireHistoryIntegrity(r, u, row); err != nil {
		return nil, err
	}
	if err := s.requireHistoryRowAccess(ctx, u, row); err != nil {
		return nil, err
	}
	// A Scrum row is its own durable operation (scrum_journal.go), which
	// flags the row itself.
	if docString(row, "entityType") == "scrum" {
		if err := s.applyScrumHistoryJournaled(ctx, u, row, direction, nil); err != nil {
			return nil, err
		}
		return map[string]any{key: true, "entityType": row["entityType"], "entityId": row["entityId"], "group": row["group"]}, nil
	}
	before := s.currentHistoryContent(ctx, row)
	// Claimed first: of two that picked the same row, one applies it.
	at := time.Now().UTC().Truncate(time.Millisecond)
	sel, taken, released := bson.M{"_id": eq(docString(row, "_id")), "undone": false, "superseded": bson.M{"$ne": true}},
		bson.M{"undone": true, "undoneAt": at}, bson.M{"undone": false, "undoneAt": nil}
	if direction == "redo" {
		sel["undone"] = true
		taken, released = bson.M{"undone": false, "undoneAt": nil}, bson.M{"undone": true, "undoneAt": row["undoneAt"]}
	}
	res, err := s.db.Collection("changeHistory").UpdateOne(ctx, sel, bson.M{"$set": taken})
	if err != nil || res.ModifiedCount == 0 {
		return map[string]any{key: false, "reason": "conflict"}, nil
	}
	applied := false
	if docString(row, "entityType") == "rule" {
		ok, rerr := s.applyRuleHistory(ctx, u, row, direction)
		if rerr != nil {
			back := bson.M{"_id": eq(docString(row, "_id"))}
			for k, v := range taken {
				back[k] = v
			}
			_, _ = s.db.Collection("changeHistory").UpdateOne(ctx, back, bson.M{"$set": released})
			return nil, rerr
		}
		applied = ok
	} else {
		applied = s.applyHistoryRow(ctx, row, direction)
	}
	if !applied {
		back := bson.M{"_id": eq(docString(row, "_id"))}
		for k, v := range taken {
			back[k] = v
		}
		_, _ = s.db.Collection("changeHistory").UpdateOne(ctx, back, bson.M{"$set": released})
		return map[string]any{key: false, "reason": "not-applicable"}, nil
	}
	// The reversal itself, in the timeline and out of the stack.
	s.recordHistory(ctx, historyChange{BoardID: boardID, SwimlaneID: row["swimlaneId"], ListID: row["listId"], CardID: row["cardId"],
		EntityType: docString(row, "entityType"), EntityID: docString(row, "entityId"), Group: docString(row, "group"), ChangeType: "restored",
		PreviousContent: before, NewContent: s.currentHistoryContent(ctx, row), UserID: u.ID, RestoredFromID: docString(row, "_id"),
		RestoredByUserID: u.ID, IsCheckpoint: true})
	return map[string]any{key: true, "entityType": row["entityType"], "entityId": row["entityId"], "group": row["group"]}, nil
}

func (s *Server) historyMethods() map[string]methodFunc {
	reverse := func(direction string) methodFunc {
		return func(r *http.Request, u *User, args []json.RawMessage) (any, error) {
			boardID, err := stringArg(args, 0)
			if err != nil {
				return nil, err
			}
			// check(requestId, Match.Maybe(String)).
			var requestID *string
			if v, given := scrumArg(args, 1); given && v != nil {
				id, ok := v.(string)
				if !ok {
					return nil, &methodError{"Match.Error", "Expected string"}
				}
				requestID = &id
			}
			return s.reverseLast(r, u, boardID, direction, requestID)
		}
	}
	return map[string]methodFunc{
		"lists.softRemove":       s.methodListSoftRemove,
		"changeHistory.undoLast": reverse("undo"),
		"changeHistory.redoLast": reverse("redo"),
		"changeHistory.page":     s.methodHistoryPage,
		"changeHistory.restore":  s.methodHistoryRestore,
	}
}

// applyCardPosition is applyCardContent's position: the card back on its
// board, swimlane and list, at its sort; to another board through the move
// that takes its comments, checklists and attachments along.
func (s *Server) applyCardPosition(ctx context.Context, row, content map[string]any) bool {
	var card bson.M
	if s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(docString(row, "entityId"))}).Decode(&card) != nil {
		return false
	}
	boardID, _ := content["boardId"].(string)
	if boardID == "" {
		boardID = docString(card, "boardId")
	}
	lane, _ := content["swimlaneId"].(string)
	list, _ := content["listId"].(string)
	if !validID(boardID) || !validID(list) {
		return false
	}
	set := bson.M{} // the $set below
	set["swimlaneId"], set["listId"] = lane, list
	if v, ok := content["sort"].(float64); ok {
		set["sort"] = v
	}
	if r, ok := content["lastMoveReason"].(string); ok {
		set["lastMoveReason"] = r
	}
	if boardID != docString(card, "boardId") {
		var from, to Board
		if s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(docString(card, "boardId"))}).Decode(&from) != nil ||
			s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(boardID)}).Decode(&to) != nil {
			return false
		}
		if s.moveCardTo(ctx, &User{ID: docString(row, "userId")}, card, &from, &to, lane, list) != nil {
			return false
		}
		s.hub.publish(from.ID, "board-changed")
	}
	if _, err := s.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": eq(docString(card, "_id"))}, bson.M{"$set": set}); err != nil {
		return false
	}
	s.hub.publish(boardID, "board-changed")
	return true
}

// requireHistoryRowAccess is WeKan's: a row the caller may read
// (historyReadScope), its card one they may edit where it is NOW, and the
// board on the row one they may write. A row's board is the one it was
// recorded on; a card moved since to a board the caller cannot reach is not
// written through History.
func (s *Server) requireHistoryRowAccess(ctx context.Context, u *User, row map[string]any) error {
	denied := &methodError{"not-authorized", "This history entry is outside your current access."}
	hr := &historyReader{s: s, ctx: ctx, u: u, boards: map[string]*historyBoardScope{}}
	if !hr.readable(row) {
		return denied
	}
	cardID := docString(row, "cardId")
	if cardID == "" && docString(row, "entityType") == "card" {
		cardID = docString(row, "entityId")
	}
	if cardID != "" {
		var c Card
		if s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(cardID)}).Decode(&c) != nil {
			return denied
		}
		b, err := s.boardFor(ctx, c.BoardID, u)
		if err != nil || !b.Writable(u, "write", &c) {
			return &methodError{"not-authorized", "You cannot edit this card through History."}
		}
	}
	return nil
}

// methodHistoryRestore is changeHistory.restore(selected): the rows applied
// oldest first, each as the value its row shows, and each recorded twice -
// for whoever made the change and whoever restored it - in one batch.
func (s *Server) methodHistoryRestore(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	if len(args) < 1 {
		return nil, &methodError{"Match.Error", "Match failed"}
	}
	var ids []string
	var one string
	var set map[string]bool
	switch {
	case json.Unmarshal(args[0], &ids) == nil:
	case json.Unmarshal(args[0], &one) == nil:
		ids = []string{one}
	case json.Unmarshal(args[0], &set) == nil:
		for id, on := range set {
			if on {
				ids = append(ids, id)
			}
		}
	default:
		return nil, &methodError{"Match.Error", "Match failed"}
	}
	seen := map[string]bool{}
	clean := ids[:0]
	for _, id := range ids {
		if validID(id) && !seen[id] {
			seen[id] = true
			clean = append(clean, id)
		}
	}
	if len(clean) == 0 {
		return map[string]any{"restored": 0, "skipped": 0}, nil
	}
	ctx := r.Context()
	ids2 := bson.A{}
	for _, id := range clean {
		ids2 = append(ids2, id)
	}
	cur, err := s.db.Collection("changeHistory").Find(ctx, bson.M{"_id": dbIn(ids2)}, options.Find().SetSort(bson.D{{Key: "createdAt", Value: 1}}))
	if err != nil {
		return nil, err
	}
	var raws []bson.M
	_ = cur.All(ctx, &raws)
	batch := fmt.Sprintf("restore-%d-%s", time.Now().UnixMilli(), u.ID)
	restored, skipped := 0, 0
	for _, raw := range raws {
		row := plain(raw).(map[string]any)
		if _, err := s.historyBoardWrite(ctx, u, docString(row, "boardId")); err != nil {
			return nil, err
		}
		if err := s.requireHistoryIntegrity(r, u, row); err != nil {
			return nil, err
		}
		if err := s.requireHistoryRowAccess(ctx, u, row); err != nil {
			return nil, err
		}
		// A Scrum row's Restore writes its own restore rows (scrum_journal.go).
		if docString(row, "entityType") == "scrum" {
			if err := s.applyScrumHistoryJournaled(ctx, u, row, "restore", nil); err != nil {
				return nil, err
			}
			restored++
			continue
		}
		displaced := s.currentHistoryContent(ctx, row)
		applied := false
		if docString(row, "entityType") == "rule" {
			ok, rerr := s.applyRuleHistory(ctx, u, row, "restore")
			if rerr != nil {
				return nil, rerr
			}
			applied = ok
		} else {
			applied = s.applyHistoryRow(ctx, row, "restore")
		}
		if !applied {
			skipped++
			continue
		}
		restored++
		content := row["newContent"]
		if content == nil {
			content = row["previousContent"]
		}
		common := historyChange{BoardID: docString(row, "boardId"), SwimlaneID: row["swimlaneId"], ListID: row["listId"], CardID: row["cardId"],
			EntityType: docString(row, "entityType"), EntityID: docString(row, "entityId"), Group: docString(row, "group"), ChangeType: "restored",
			PreviousContent: displaced, NewContent: content, BatchID: batch, RestoredFromID: docString(row, "_id"), RestoredByUserID: u.ID}
		author := common
		author.UserID = docString(row, "userId")
		s.recordHistory(ctx, author)
		if author.UserID != u.ID {
			common.UserID = u.ID
			s.recordHistory(ctx, common)
		}
	}
	return map[string]any{"restored": restored, "skipped": skipped}, nil
}
