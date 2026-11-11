package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Rules' "copy the card" action, and the methods around it, as Node.js
// WeKan's server/lib/ruleCopyCard.js, server/rulesButton.js rules.updateRule
// and server/models/cards.js createCardWithDueDate.

// copyChainKey carries the copy actions already taken in this causal chain:
// a copy makes a card - an activity - which may start a copy rule again; each
// copy action runs once a chain, and independent events have chains of their own.
type copyChainKey struct{}

func copyChain(ctx context.Context) map[string]bool {
	m, _ := ctx.Value(copyChainKey{}).(map[string]bool)
	return m
}

// ruleCopyCard is WeKan's copyRuleCard: the activity's card - on its own
// board, not archived - into the action's list and swimlane of its board,
// when the acting user may change both boards; with its dates, description
// and color, its checklists and their items, and its comments.
func (s *Server) ruleCopyCard(ctx context.Context, a, act bson.M, c *Card) error {
	destID := docString(act, "boardId")
	if destID == "" {
		destID = docString(a, "boardId")
	}
	userID, listID, laneID := docString(a, "userId"), docString(act, "listId"), docString(act, "swimlaneId")
	for _, v := range []string{c.ID, docString(a, "boardId"), userID, destID, listID, laneID} {
		if !validID(v) {
			return nil
		}
	}
	key := docString(act, "_id")
	if key == "" {
		key = canonical([]any{docString(a, "boardId"), destID, listID, laneID})
	}
	chain := copyChain(ctx)
	if chain[key] {
		return nil
	}
	next := map[string]bool{key: true}
	for k := range chain {
		next[k] = true
	}
	ctx = context.WithValue(ctx, copyChainKey{}, next)
	var actor User
	if c.BoardID != docString(a, "boardId") || c.Archived || s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(userID)}).Decode(&actor) != nil {
		return nil
	}
	var source, dest Board
	if s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(c.BoardID)}).Decode(&source) != nil ||
		s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(destID)}).Decode(&dest) != nil ||
		source.Archived || dest.Archived || !source.Can(userID, "write") || !dest.Can(userID, "write") {
		// RuleBleed: a stored action naming a board its user may no longer
		// change is an attempt, as when such a rule is saved.
		if dest.ID != "" && dest.ID != c.BoardID && !dest.Can(userID, "write") {
			s.recordSecurityCtx(ctx, securityEvent{Key: "authz.canary", Action: "detected", Source: "canary:rule.cross-board-write", UserID: userID,
				Detail: "A rule's copy action named a board its user may not change."})
		}
		return nil
	}
	var list, lane struct {
		BoardID  string `bson:"boardId"`
		Archived bool   `bson:"archived"`
	}
	if s.db.Collection("lists").FindOne(ctx, bson.M{"_id": eq(listID)}).Decode(&list) != nil || list.Archived || list.BoardID != destID ||
		s.db.Collection("swimlanes").FindOne(ctx, bson.M{"_id": eq(laneID)}).Decode(&lane) != nil || lane.Archived || lane.BoardID != destID {
		return nil
	}
	var raw bson.M
	if s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(c.ID)}).Decode(&raw) != nil {
		return nil
	}
	number, err := s.nextCardNumber(ctx, destID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	copyDoc := bson.M{"_id": randomID(), "title": c.Title, "boardId": destID, "listId": listID, "swimlaneId": laneID, "userId": userID, // inserted, not a filter
		"members": []string{}, "assignees": []string{}, "labelIds": []string{}, "cardNumber": number, "type": "cardType-card",
		"archived": false, "createdAt": now, "modifiedAt": now,
		"sort": s.nextSort(ctx, "cards", bson.M{"boardId": eq(destID), "listId": eq(listID), "swimlaneId": eq(laneID)})}
	for _, k := range []string{"description", "color", "receivedAt", "startAt", "dueAt", "endAt"} {
		if v, ok := raw[k]; ok && v != nil {
			copyDoc[k] = v
		}
	}
	if destID == c.BoardID {
		copyDoc["labelIds"], copyDoc["members"], copyDoc["assignees"] = nonNil(c.LabelIDs), nonNil(c.Members), nonNil(c.Assignees)
	}
	if _, err := s.db.Collection("cards").InsertOne(ctx, copyDoc); err != nil {
		return err
	}
	copyID := copyDoc["_id"].(string)
	s.copyChecklists(ctx, c.ID, copyID)
	if cur, err := s.db.Collection("card_comments").Find(ctx, bson.M{"cardId": eq(c.ID)}); err == nil {
		var comments []bson.M
		_ = cur.All(ctx, &comments)
		for _, cm := range comments {
			cm["_id"], cm["cardId"], cm["boardId"] = randomID(), copyID, destID
			_, _ = s.db.Collection("card_comments").InsertOne(ctx, cm)
		}
	}
	s.activity(ctx, userID, "createCard", bson.M{"boardId": destID, "listId": listID, "swimlaneId": laneID, "cardId": copyID, // inserted, not a filter
		"cardTitle": c.Title, "listName": s.titleOfID(ctx, "lists", listID), "swimlaneName": s.titleOfID(ctx, "swimlanes", laneID)})
	s.hub.publish(destID, "board-changed")
	return nil
}

// methodCreateCardWithDueDate is WeKan's createCardWithDueDate(boardId,
// listId, title, dueDate, swimlaneId): a card with its due date, for a member
// with write access; no WeKan client calls it, so a refusal is recorded.
func (s *Server) methodCreateCardWithDueDate(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	bad := &methodError{"Match.Error", "Match failed"}
	if len(args) != 5 {
		return nil, bad
	}
	var boardID, listID, title, laneID string
	for i, p := range []*string{&boardID, &listID, &title, nil, &laneID} {
		if p != nil && json.Unmarshal(args[i], p) != nil {
			return nil, bad
		}
	}
	// A Date: EJSON {"$date": ms} as Meteor sends it, or the ISO text
	// JSON.stringify makes of one.
	var due time.Time
	var d struct {
		Date *float64 `json:"$date"`
	}
	var iso string
	switch {
	case json.Unmarshal(args[3], &d) == nil && d.Date != nil:
		due = time.UnixMilli(int64(*d.Date)).UTC()
	case json.Unmarshal(args[3], &iso) == nil:
		t, err := time.Parse(time.RFC3339Nano, iso)
		if err != nil {
			return nil, bad
		}
		due = t.UTC()
	default:
		return nil, bad
	}
	refuse := func(detail string) error {
		s.recordSecurity(r, securityEvent{Key: "authz.copy", Action: "blocked", Source: "method:createCardWithDueDate", UserID: u.ID, Detail: detail})
		return &methodError{"not-authorized", "not-authorized"}
	}
	var b Board
	if !validID(boardID) || s.db.Collection("boards").FindOne(r.Context(), bson.M{"_id": eq(boardID)}).Decode(&b) != nil {
		return nil, &methodError{"not-found", "not-found"}
	}
	if !b.Can(u.ID, "write") {
		return nil, refuse("Card insert refused: no write access to the board")
	}
	if s.refuseInList(r.Context(), &b, listID, u, "write", "method:createCardWithDueDate") {
		return nil, &methodError{"not-authorized", "not-authorized"}
	}
	c, err := s.AddCardWith(r.Context(), &b, u, listID, laneID, title, bson.M{"dueAt": due, "sort": 0.0})
	if err != nil {
		return nil, &methodError{"400", err.Error()}
	}
	s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	return c.ID, nil
}

// methodUpdateRule is WeKan's rules.updateRule(ruleId, title, trigger, action):
// a board admin replaces a rule's trigger and action in place - the rule and
// its own components keep their ids; one another rule shares is copied
// first, so editing this rule never changes that one.
func (s *Server) methodUpdateRule(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	ruleID, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	title, err := optionalString(args, 1)
	if err != nil {
		return nil, err
	}
	var trigIn, actIn map[string]any
	if len(args) < 4 || json.Unmarshal(args[2], &trigIn) != nil || json.Unmarshal(args[3], &actIn) != nil || trigIn == nil || actIn == nil {
		return nil, &methodError{"Match.Error", "Expected objects"}
	}
	ctx := r.Context()
	var rule bson.M
	if !validID(ruleID) || s.db.Collection("rules").FindOne(ctx, bson.M{"_id": eq(ruleID)}).Decode(&rule) != nil {
		return nil, &methodError{"not-found", "Rule not found"}
	}
	// One History row for the whole edit (ruleHistory.js withRuleHistory).
	beforeRule := s.ruleSnapshot(ctx, ruleID)
	defer func() { s.recordRuleChange(ctx, beforeRule, s.ruleSnapshot(ctx, ruleID), u.ID) }()
	var b Board
	if s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(docString(rule, "boardId"))}).Decode(&b) != nil {
		return nil, &methodError{"not-found", "Board not found"}
	}
	if !b.AdminOf(u.ID) {
		return nil, &methodError{"not-authorized", "Must be a board admin"}
	}
	trigger, err := ruleComponent(trigIn, "activityType")
	if err != nil {
		return nil, &methodError{"Match.Error", err.Error()}
	}
	action, err := ruleComponent(actIn, "actionType")
	if err != nil {
		return nil, &methodError{"Match.Error", err.Error()}
	}
	if dest, _ := actIn["boardId"].(string); dest != "" {
		if !validID(dest) {
			return nil, &methodError{"Match.Error", "boardId is an id"}
		}
		action["boardId"] = dest
	} else {
		action["boardId"] = b.ID
	}
	if !s.ruleDestinationOK(r, u, b.ID, action) {
		return nil, &methodError{"not-authorized", "Must have write access to the destination board"}
	}
	trigger = normalizeTrigger(trigger)
	trigger["boardId"] = b.ID
	now := time.Now().UTC()
	// writeComponent replaces this rule's own component whole - a trigger of
	// another kind keeps no field of the old one - or gives it its own copy.
	writeComponent := func(coll, field string, doc bson.M) (string, error) {
		id := docString(rule, field)
		others := bson.M{"$or": bson.A{bson.M{"triggerId": eq(id)}, bson.M{"actionId": eq(id)}, bson.M{"extraTriggerIds": eq(id)}, bson.M{"extraActionIds": eq(id)}}}
		others["_id"] = bson.M{"$ne": ruleID}
		shared, _ := s.db.Collection("rules").CountDocuments(ctx, others)
		var old bson.M
		exists := validID(id) && s.db.Collection(coll).FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&old) == nil
		doc["modifiedAt"] = now
		if !exists || shared > 0 {
			doc["_id"], doc["createdAt"] = randomID(), now
			_, err := s.db.Collection(coll).InsertOne(ctx, doc)
			return docString(doc, "_id"), err
		}
		doc["_id"] = id
		if ca, ok := old["createdAt"]; ok {
			doc["createdAt"] = ca
		}
		_, err := s.db.Collection(coll).ReplaceOne(ctx, bson.M{"_id": eq(id)}, doc)
		return id, err
	}
	triggerID, err := writeComponent("triggers", "triggerId", trigger)
	if err != nil {
		return nil, err
	}
	actionID, err := writeComponent("actions", "actionId", action)
	if err != nil {
		return nil, err
	}
	if title = strings.TrimSpace(title); title == "" {
		title = docString(rule, "title")
		if title == "" {
			title = "Rule"
		}
	}
	update := ruleButtonMetadata(trigger, title)
	set, _ := update["$set"].(bson.M)
	if set == nil {
		set = bson.M{}
	}
	set["title"], set["triggerId"], set["actionId"], set["modifiedAt"] = title, triggerID, actionID, now
	update["$set"] = set
	if _, err := s.db.Collection("rules").UpdateOne(ctx, bson.M{"_id": eq(ruleID)}, update); err != nil {
		return nil, err
	}
	s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	out := bson.M{"triggerId": triggerID, "actionId": actionID}
	out["_id"] = ruleID // the method's answer, not a filter
	return out, nil
}
