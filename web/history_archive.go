package web

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func (s *Server) replayTemplateListArchive(ctx context.Context, row, target map[string]any, direction string) bool {
	boardID, listID := docString(row, "boardId"), docString(row, "entityId")
	archive, valid := target["value"].(bool)
	if !valid || !validID(boardID) || !validID(listID) || docString(row, "group") != "lifecycle" {
		return false
	}
	snapshot, _ := row["newContent"].(map[string]any)
	values, valid := snapshot["archiveCards"].([]any)
	if !valid {
		return false
	}
	ids := make([]string, 0, len(values))
	for _, value := range values {
		id, ok := value.(string)
		if !ok || !validID(id) {
			return false
		}
		ids = append(ids, id)
	}
	s.listArchiveMu.Lock()
	defer s.listArchiveMu.Unlock()
	lists := s.db.Collection("lists")
	scope := bson.M{"_id": listID, "boardId": boardID, "deletedAt": nil, "type": "template-list"}
	operation := docString(row, "_id") + ":" + direction
	var current struct {
		Archived   bool      `bson:"archived"`
		ArchivedAt time.Time `bson:"archivedAt"`
		Pending    *bool     `bson:"archiveCascadePending"`
		Replay     string    `bson:"archiveCascadeReplay"`
	}
	if err := lists.FindOne(ctx, scope).Decode(&current); err != nil {
		return false
	}
	now := time.Now().UTC()
	if current.Pending != nil {
		if current.Replay != operation || *current.Pending != archive || current.Archived != archive {
			return false
		}
		if archive {
			now = current.ArchivedAt
		}
	} else {
		if current.Archived == archive {
			return false
		}
		if len(ids) != 0 {
			matching, err := s.db.Collection("cards").CountDocuments(ctx, bson.M{"_id": bson.M{"$in": ids},
				"boardId": boardID, "listId": listID, "archived": !archive, "deletedAt": nil})
			if err != nil || matching != int64(len(ids)) {
				return false
			}
		}
		scope["archived"] = !archive
		scope["archiveCascadePending"] = nil
		update := archiveStateUpdate(archive, now)
		set := update["$set"].(bson.M)
		set["archiveCascadePending"], set["archiveCascadeCards"], set["archiveCascadeReplay"] = archive, ids, operation
		if changed, err := lists.UpdateOne(ctx, scope, update); err != nil || changed.MatchedCount != 1 {
			return false
		}
	}
	if err := s.archiveTemplateListCards(ctx, "", boardID, listID, archive, now, ids, operation); err != nil {
		return false
	}
	s.hub.publish(boardID, "board-changed")
	return true
}
