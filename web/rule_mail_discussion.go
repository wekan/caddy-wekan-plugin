package web

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// The mail action's "Checklists / Comments", as Node.js WeKan's
// server/lib/ruleCardDiscussion.js: the triggering card's checklists with
// their items, and its comments with their authors' display names and
// reaction counts - public prose only, never a whole document (a comment can
// hold a webhook's private response state) and never an author's address.
// The rule's user must still be able to read the card, before the reads and
// after them: an assigned-only member only a card assigned to them.

var errRuleMailContent = errors.New("rule-email-card-content-not-authorized")

const ruleMailDiscussionMax = 768 * 1024

var reactionCode = regexp.MustCompile(`^&#(\d{4,6});$`)

// canReadBoard is WeKan's models/lib/boardVisibility.js: a public board, an
// instance board to anybody signed in, otherwise an active member.
func canReadBoard(userID string, b *Board) bool {
	if b == nil {
		return false
	}
	switch b.Permission {
	case "public":
		return true
	case "instance":
		if userID != "" {
			return true
		}
	}
	return userID != "" && b.MemberOf(userID)
}

// ruleMailCardReadable is the check both sides of the reads make.
func (s *Server) ruleMailCardReadable(ctx context.Context, userID, boardID, cardID string) bool {
	var c Card
	var b Board
	var raw struct {
		DeletedAt any `bson:"deletedAt"`
	}
	if userID == "" || !validID(cardID) ||
		s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(cardID)}).Decode(&c) != nil ||
		s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(cardID)}).Decode(&raw) != nil || raw.DeletedAt != nil ||
		c.BoardID != boardID ||
		s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(c.BoardID)}).Decode(&b) != nil || !canReadBoard(userID, &b) {
		return false
	}
	if m := b.member(userID); m != nil && m.AssignedOnly() && !slices.Contains(c.Assignees, userID) {
		return false
	}
	return true
}

func (s *Server) ruleMailDisplayName(ctx context.Context, userID string) string {
	var u struct {
		Username string `bson:"username"`
		Profile  struct {
			Fullname string `bson:"fullname"`
		} `bson:"profile"`
	}
	if validID(userID) && s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(userID)}).Decode(&u) == nil {
		if u.Profile.Fullname != "" {
			return u.Profile.Fullname
		}
		if u.Username != "" {
			return u.Username
		}
	}
	return "Unknown user"
}

// ruleMailDiscussion is WeKan's prepareRuleCardDiscussion.
func (s *Server) ruleMailDiscussion(ctx context.Context, userID, boardID, cardID string) (string, error) {
	if !s.ruleMailCardReadable(ctx, userID, boardID, cardID) {
		return "", errRuleMailContent
	}
	find := func(coll string, filter bson.M, sortBy bson.D) []bson.M {
		cur, err := s.db.Collection(coll).Find(ctx, filter, options.Find().SetSort(sortBy))
		if err != nil {
			return nil
		}
		var out []bson.M
		_ = cur.All(ctx, &out)
		return out
	}
	bySort := bson.D{{Key: "sort", Value: 1}, {Key: "_id", Value: 1}}
	checklists := find("checklists", bson.M{"cardId": eq(cardID), "deletedAt": nil}, bySort)
	items := find("checklistItems", bson.M{"cardId": eq(cardID), "deletedAt": nil}, bySort)
	comments := find("card_comments", bson.M{"cardId": eq(cardID), "boardId": eq(boardID), "deletedAt": nil},
		bson.D{{Key: "createdAt", Value: 1}, {Key: "_id", Value: 1}})

	// Reactions: per comment, per code point, the distinct users.
	var commentIDs bson.A
	for _, cm := range comments {
		if id := docString(cm, "_id"); id != "" {
			commentIDs = append(commentIDs, id)
		}
	}
	reactions := map[string]map[int]map[string]bool{}
	if len(commentIDs) > 0 {
		for _, row := range find("card_comment_reactions", bson.M{"cardId": eq(cardID), "boardId": eq(boardID), "cardCommentId": bson.M{"$in": commentIDs}}, bson.D{{Key: "_id", Value: 1}}) {
			cid := docString(row, "cardCommentId")
			list, _ := plain(row["reactions"]).([]any)
			for _, r := range list {
				rmap, _ := r.(map[string]any)
				rm := bson.M(rmap)
				m := reactionCode.FindStringSubmatch(docString(rm, "reactionCodepoint"))
				if m == nil {
					continue
				}
				code, _ := strconv.Atoi(m[1])
				if code < 0x20 || (code >= 0x7f && code <= 0x9f) || code > 0x10ffff || (code >= 0xd800 && code <= 0xdfff) {
					continue
				}
				if reactions[cid] == nil {
					reactions[cid] = map[int]map[string]bool{}
				}
				if reactions[cid][code] == nil {
					reactions[cid][code] = map[string]bool{}
				}
				ids, _ := rm["userIds"].([]any)
				for _, id := range ids {
					if v, ok := id.(string); ok && v != "" {
						reactions[cid][code][v] = true
					}
				}
			}
		}
	}

	var lines []string
	bytes := 0
	var tooLarge bool
	add := func(line string) {
		bytes += len(line) + 1
		if bytes > ruleMailDiscussionMax {
			tooLarge = true
			return
		}
		lines = append(lines, line)
	}
	dateLine := func(label string, v any, indent string) {
		if t, ok := plain(v).(time.Time); ok && !t.IsZero() {
			add(indent + label + ": " + t.UTC().Format("2006-01-02T15:04:05.000Z"))
		}
	}
	itemsOf := map[string][]bson.M{}
	for _, it := range items {
		itemsOf[docString(it, "checklistId")] = append(itemsOf[docString(it, "checklistId")], it)
	}
	if len(checklists) > 0 {
		add("Checklists:")
		for _, cl := range checklists {
			add(docString(cl, "title"))
			dateLine("Due", cl["dueAt"], "  ")
			dateLine("Finished", cl["finishedAt"], "  ")
			if v := docString(cl, "resetInterval"); v != "" {
				add("  Reset interval: " + v)
			}
			dateLine("Last reset", cl["lastResetAt"], "  ")
			for _, it := range itemsOf[docString(cl, "_id")] {
				mark := " "
				if it["isFinished"] == true {
					mark = "x"
				}
				add("  [" + mark + "] " + docString(it, "title"))
				dateLine("Due", it["dueAt"], "    ")
				// A converted subtask is named only when its user may read it.
				if linked := docString(it, "linkedCardId"); validID(linked) {
					var sub Card
					if s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(linked)}).Decode(&sub) == nil &&
						s.ruleMailCardReadable(ctx, userID, sub.BoardID, sub.ID) && sub.Title != "" {
						add("    Converted subtask: " + sub.Title)
					}
				}
			}
		}
	}
	if len(comments) > 0 {
		if len(lines) > 0 {
			add("")
		}
		add("Comments:")
		for _, cm := range comments {
			if t, ok := plain(cm["createdAt"]).(time.Time); ok && !t.IsZero() {
				add("[" + t.UTC().Format("2006-01-02T15:04:05.000Z") + "]")
			} else {
				add("---")
			}
			if uid := docString(cm, "userId"); uid != "" {
				add("Author: " + s.ruleMailDisplayName(ctx, uid))
			}
			dateLine("Edited", cm["modifiedAt"], "")
			add(docString(cm, "text"))
			groups := reactions[docString(cm, "_id")]
			codes := make([]int, 0, len(groups))
			for code, users := range groups {
				if len(users) > 0 {
					codes = append(codes, code)
				}
			}
			sort.Ints(codes)
			var summary []string
			for _, code := range codes {
				summary = append(summary, fmt.Sprintf("%c %d", rune(code), len(groups[code])))
			}
			if len(summary) > 0 {
				add("Reactions: " + strings.Join(summary, ", "))
			}
		}
	}
	if tooLarge {
		return "", errors.New("rule-email-card-content-too-large")
	}
	if !s.ruleMailCardReadable(ctx, userID, boardID, cardID) {
		return "", errRuleMailContent
	}
	return strings.Join(lines, "\n"), nil
}
