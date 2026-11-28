package web

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const maxAutoArchiveDays = 3650
const autoArchiveBatch = 200

func autoArchiveDays(value any) int {
	days := numberOf(value)
	if days >= 1 && days <= maxAutoArchiveDays && math.Trunc(days) == days {
		return int(days)
	}
	return 0
}

func (s *Server) saveAutoArchiveDays(w http.ResponseWriter, r *http.Request, user *User) {
	ctx := r.Context()
	board, err := s.boardFor(ctx, r.PathValue("board"), user)
	if err != nil || !board.AdminOf(user.ID) {
		http.Error(w, "only a board admin may change inactivity archiving", http.StatusForbidden)
		return
	}
	raw := strings.TrimSpace(r.FormValue("days"))
	days, err := strconv.Atoi(raw)
	if raw != "" && (err != nil || days < 1 || days > maxAutoArchiveDays) {
		http.Error(w, "expected 1 to 3650 days or an empty value", http.StatusBadRequest)
		return
	}
	update := bson.M{"$unset": bson.M{"autoArchiveInactiveDays": ""}}
	if days > 0 {
		update = bson.M{"$set": bson.M{"autoArchiveInactiveDays": days}}
	}
	filter := bson.M{"_id": eq(board.ID), "archived": false, "members": bson.M{"$elemMatch": bson.M{
		"userId": user.ID, "isActive": true, "isAdmin": true,
	}}}
	result, err := s.db.Collection("boards").UpdateOne(ctx, filter, update)
	if err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	if result.MatchedCount != 1 {
		http.Error(w, "board administration required", http.StatusForbidden)
		return
	}
	http.Redirect(w, r, "/b/"+board.ID+"/"+board.Slug, http.StatusSeeOther)
}

func (s *Server) scanInactiveCards(ctx context.Context, now time.Time) {
	cursor, err := s.db.Collection("boards").Find(ctx, bson.M{
		"archived": false, "type": "board", "autoArchiveInactiveDays": bson.M{"$gte": 1},
	})
	if err != nil {
		s.log.Warn("find boards for inactivity archiving", "error", err)
		return
	}
	defer cursor.Close(ctx)
	for cursor.Next(ctx) {
		var board bson.M
		if cursor.Decode(&board) != nil {
			continue
		}
		days, id := autoArchiveDays(board["autoArchiveInactiveDays"]), docString(board, "_id")
		if days == 0 || id == "" {
			continue
		}
		s.archiveInactiveBoard(ctx, id, days, now.UTC())
	}
}

func (s *Server) archiveInactiveBoard(ctx context.Context, boardID string, days int, now time.Time) {
	var board Board
	if s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(boardID), "archived": false,
		"type": "board", "autoArchiveInactiveDays": days}).Decode(&board) != nil {
		return
	}
	var actor User
	for _, member := range board.Members {
		if !member.IsActive || !member.IsAdmin {
			continue
		}
		if s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(member.UserID),
			"loginDisabled": bson.M{"$ne": true}}).Decode(&actor) == nil {
			break
		}
	}
	if actor.ID == "" || !board.WritableIn(ctx, s, &actor, "write", nil) {
		return
	}
	filter := bson.M{"boardId": boardID, "archived": false, "deletedAt": nil,
		"type": bson.M{"$ne": "template-card"}, "dateLastActivity": bson.M{"$lt": now.AddDate(0, 0, -days)}}
	cursor, err := s.db.Collection("cards").Find(ctx, filter,
		options.Find().SetSort(bson.D{{Key: "dateLastActivity", Value: 1}}).SetLimit(autoArchiveBatch))
	if err != nil {
		return
	}
	var cards []Card
	if cursor.All(ctx, &cards) != nil {
		return
	}
	for _, card := range cards {
		filter["_id"] = eq(card.ID)
		if s.archiveInactiveCard(ctx, &actor, card, filter, now) {
			s.archiveInactiveChildren(ctx, &actor, card.ID, now)
		}
	}
}

func (s *Server) archiveInactiveCard(ctx context.Context, user *User, card Card, filter bson.M, now time.Time) bool {
	result, err := s.cardsUpdateOne(ctx, user.ID, filter, bson.M{"$set": bson.M{
		"archived": true, "archivedAt": now, "modifiedAt": now, "dateLastActivity": now,
	}})
	if err != nil || result.ModifiedCount != 1 {
		return false
	}
	s.activity(ctx, user.ID, "archivedCard", bson.M{"boardId": card.BoardID, "cardId": card.ID,
		"listId": card.ListID, "swimlaneId": card.SwimlaneID, "cardTitle": card.Title})
	s.hub.publishFrom(card.BoardID, "board-changed", "")
	return true
}

func (s *Server) archiveInactiveChildren(ctx context.Context, user *User, parentID string, now time.Time) {
	pending := []string{parentID}
	for len(pending) > 0 && ctx.Err() == nil {
		parent := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		filter := bson.M{"parentId": parent, "archived": false, "deletedAt": nil,
			"type": bson.M{"$ne": "template-card"},
			"$or":  bson.A{bson.M{"parentIds.1": bson.M{"$exists": false}}, bson.M{"parentIds.1": nil}}}
		cursor, err := s.db.Collection("cards").Find(ctx, filter)
		if err != nil {
			continue
		}
		var children []Card
		if cursor.All(ctx, &children) != nil {
			continue
		}
		for _, child := range children {
			board, err := s.boardFor(ctx, child.BoardID, user)
			if err != nil || board.Archived || !board.WritableIn(ctx, s, user, "write", &child) {
				continue
			}
			filter["_id"], filter["boardId"] = eq(child.ID), child.BoardID
			if s.archiveInactiveCard(ctx, user, child, filter, now) {
				pending = append(pending, child.ID)
			}
		}
	}
}
