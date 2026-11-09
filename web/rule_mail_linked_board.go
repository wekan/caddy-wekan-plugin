package web

import (
	"context"
	"errors"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// A rule mail about a linked-board card, as Node.js WeKan's
// resolveRuleEmailSource, ruleCardDetails.js (its linked-board branch),
// ruleCardWrapper.js and ruleBoardDiscussion.js: the card shows the linked
// board, so the mail's title and description are that board's; "Details"
// gives the board's own fields and its active members by name - never their
// roles or accounts - and then the card's local details; "Checklists /
// Comments" gives the discussion of the linked board's cards, each card only
// when the rule's user may read it. The linked board must be readable by the
// rule's user, or no mail is sent.

// ruleMailBoardReadable: the board exists and its user may read it.
func (s *Server) ruleMailBoardReadable(ctx context.Context, userID, boardID string) bool {
	var b Board
	return validID(boardID) && s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(boardID)}).Decode(&b) == nil && canReadBoard(userID, &b)
}

// ruleMailLinkedBoardView is the linked-board card as WeKan's getters show
// it: the linked board's title and description over the card's own.
func (s *Server) ruleMailLinkedBoardView(ctx context.Context, userID string, c *Card) (*Card, string, error) {
	var raw struct {
		LinkedID string `bson:"linkedId"`
	}
	if s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(c.ID)}).Decode(&raw) != nil || !validID(raw.LinkedID) {
		return nil, "", errors.New("rule-email-source-invalid")
	}
	var target Board
	if s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(raw.LinkedID)}).Decode(&target) != nil || !canReadBoard(userID, &target) {
		return nil, "", errRuleMailSource
	}
	view := *c
	view.Title, view.Description = target.Title, target.Description
	return &view, raw.LinkedID, nil
}

// ruleMailLinkedBoardDetails is WeKan's linked-board branch of the details.
func (s *Server) ruleMailLinkedBoardDetails(ctx context.Context, userID string, here *Board, card map[string]any) (string, error) {
	linkedID, _ := card["linkedId"].(string)
	target := s.ruleMailRawBoard(ctx, linkedID)
	if target == nil || !s.ruleMailBoardReadable(ctx, userID, linkedID) {
		return "", nil // WeKan shows nothing of a board its user may not read
	}
	var lines []string
	add := func(label string, v any) {
		if text := ruleMailValues(v); text != "" {
			lines = append(lines, label+": "+text)
		}
	}
	add("Linked board", target["title"])
	add("Description", target["description"])
	for _, f := range [][2]string{{"receivedAt", "Received"}, {"startAt", "Start"}, {"dueAt", "Due"}, {"endAt", "End"},
		{"spentTime", "Spent time (hours)"}, {"archived", "Archived"}, {"archivedAt", "Archived at"},
		{"dueComplete", "Due complete"}, {"isOvertime", "Overtime"}} {
		add(f[1], target[f[0]])
	}
	// Both people fields are the linked board's active members, by name.
	var names []any
	seen := map[string]bool{}
	members, _ := target["members"].([]any)
	for _, m := range members {
		mm, _ := m.(map[string]any)
		id, _ := mm["userId"].(string)
		if mm["isActive"] != true || id == "" || seen[id] {
			continue
		}
		seen[id] = true
		var u User
		if s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&u) == nil {
			names = append(names, s.ruleMailDisplayName(ctx, id))
		}
	}
	add("Members", names)
	add("Assignees", names)
	hereRaw := s.ruleMailRawBoard(ctx, here.ID)
	presentation := map[string]any{"allowsVote": hereRaw["allowsVote"] != false && target["allowsVote"] != false,
		"allowsPoker": hereRaw["allowsPoker"] != false && target["allowsPoker"] != false}
	policy := ruleMailVotingPolicy(target, target)
	s.ruleMailVoting(ctx, target, presentation, add)
	now := s.ruleMailRawBoard(ctx, linkedID)
	if now == nil || !s.ruleMailBoardReadable(ctx, userID, linkedID) {
		return "", errRuleMailDetails
	}
	if ruleMailVotingPolicy(now, now) != policy {
		return "", errRuleMailVotingChanged
	}
	return strings.Join(lines, "\n"), nil
}

// ruleMailLinkedBoardDiscussion is WeKan's prepareRuleBoardDiscussion: the
// comments of the linked board's other cards, each card as its user may read
// it; more than 1000 comments, or 768 KiB, is refused rather than cut.
func (s *Server) ruleMailLinkedBoardDiscussion(ctx context.Context, userID, wrapperID, linkedID string) (string, error) {
	cur, err := s.db.Collection("card_comments").Find(ctx, bson.M{"boardId": eq(linkedID), "deletedAt": nil},
		options.Find().SetProjection(bson.M{"cardId": 1}).SetSort(bson.D{{Key: "createdAt", Value: 1}, {Key: "_id", Value: 1}}).SetLimit(1001))
	if err != nil {
		return "", err
	}
	var rows []struct {
		CardID string `bson:"cardId"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return "", err
	}
	if len(rows) > 1000 {
		return "", errors.New("rule-email-board-discussion-too-large")
	}
	var ids []string
	seen := map[string]bool{}
	for _, r := range rows {
		if validID(r.CardID) && r.CardID != wrapperID && !seen[r.CardID] {
			seen[r.CardID] = true
			ids = append(ids, r.CardID)
		}
	}
	var sections []string
	bytes := 0
	for _, id := range ids {
		var owner Card
		if s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&owner) != nil || owner.BoardID != linkedID ||
			!s.ruleMailCardReadable(ctx, userID, linkedID, id) {
			continue
		}
		text, err := s.ruleMailComments(ctx, userID, linkedID, id)
		if err != nil {
			return "", err
		}
		if text == "" {
			continue
		}
		section := "Card: " + owner.Title + "\n" + text
		bytes += len(section) + 2
		if bytes > ruleMailDiscussionMax {
			return "", errors.New("rule-email-board-discussion-too-large")
		}
		sections = append(sections, section)
	}
	if !s.ruleMailBoardReadable(ctx, userID, linkedID) {
		return "", errRuleMailSource
	}
	if len(sections) == 0 {
		return "", nil
	}
	return "Linked board discussion:\n" + strings.Join(sections, "\n\n"), nil
}
