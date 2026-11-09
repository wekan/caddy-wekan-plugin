package web

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// ruleLinkCard is Node.js WeKan's linkCard action (server/rulesHelper.js
// linkCardTarget, models/cards.js link): a linked card of the activity's card
// - of its original, when it is itself a link - in the list and swimlane the
// action names on its board, the swimlane falling back to the board's first;
// with only that board's custom fields (#6800) and no labels. A board its
// user may not change is RuleBleed's cross-board write, recorded, and no list
// of that name is nothing linked.
func (s *Server) ruleLinkCard(ctx context.Context, a, act bson.M, c *Card) error {
	destID := docString(act, "boardId")
	if destID == "" {
		destID = c.BoardID
	}
	userID := docString(a, "userId")
	var dest Board
	if !validID(destID) || s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(destID)}).Decode(&dest) != nil {
		return nil
	}
	if !dest.Can(userID, "write") {
		s.recordSecurityCtx(ctx, securityEvent{Key: "authz.canary", Action: "detected", Source: "canary:rule.cross-board-write", UserID: userID,
			Detail: "A rule's link action named a board its user may not change."})
		return nil
	}
	var list, lane struct {
		ID string `bson:"_id"`
	}
	if s.db.Collection("lists").FindOne(ctx, bson.M{"title": eq(docString(act, "listName")), "boardId": eq(destID)}).Decode(&list) != nil {
		return nil
	}
	if s.db.Collection("swimlanes").FindOne(ctx, bson.M{"title": eq(docString(act, "swimlaneName")), "boardId": eq(destID)}).Decode(&lane) != nil {
		var first Swimlane
		if s.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": eq(destID), "archived": bson.M{"$ne": true}}).Decode(&first) != nil {
			return nil
		}
		lane.ID = first.ID
	}
	var raw bson.M
	if s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(c.ID)}).Decode(&raw) != nil {
		return nil
	}
	link := bson.M{}
	for k, v := range raw {
		link[k] = v
	}
	source := docString(raw, "linkedId")
	if source == "" {
		source = c.ID
	}
	now := time.Now().UTC()
	delete(link, "labelIds")
	for _, k := range []string{"scrum", "scrumRevision", "customFieldLinks"} {
		delete(link, k)
	}
	link["_id"], link["linkedId"], link["type"] = randomID(), source, "cardType-linkedCard" // inserted, not a filter
	link["boardId"], link["swimlaneId"], link["listId"] = destID, lane.ID, list.ID
	link["customFields"] = s.boardFields(ctx, destID, raw["customFields"])
	link["createdAt"], link["modifiedAt"] = now, now
	link["sort"] = s.nextSort(ctx, "cards", bson.M{"boardId": eq(destID), "listId": eq(list.ID), "swimlaneId": eq(lane.ID)})
	if _, err := s.db.Collection("cards").InsertOne(ctx, link); err != nil {
		return err
	}
	s.activity(ctx, userID, "createCard", bson.M{"boardId": destID, "listId": list.ID, "swimlaneId": lane.ID, "cardId": link["_id"], // inserted, not a filter
		"cardTitle": c.Title, "listName": s.titleOfID(ctx, "lists", list.ID), "swimlaneName": s.titleOfID(ctx, "swimlanes", lane.ID)})
	s.hub.publish(destID, "board-changed")
	return nil
}
