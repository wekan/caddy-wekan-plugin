package web

// Activities, as Node.js WeKan records them in the activities collection - what
// its activity feed, notifications and rules read. Recording never fails the
// change it records.

import (
	"context"
	"errors"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"slices"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func (s *Server) activity(ctx context.Context, userID, activityType string, fields bson.M) {
	now := time.Now().UTC()
	doc := bson.M{"_id": randomID(), "userId": userID, "activityType": activityType, "createdAt": now, "modifiedAt": now} // inserted, not a filter
	for k, v := range fields {
		doc[k] = v
	}
	if _, err := s.db.Collection("activities").InsertOne(ctx, doc); err != nil {
		s.log.Warn("recording an activity", "type", activityType, "error", err)
		return
	}
	s.notify(ctx, userID, doc)
	s.executeRules(ctx, doc)
}

// titleOfID is the title of a document, "" when there is none.
func (s *Server) titleOfID(ctx context.Context, coll, id string) string {
	var d struct {
		Title string `bson:"title"`
	}
	if !validID(id) {
		return ""
	}
	_ = s.db.Collection(coll).FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&d)
	return d.Title
}

// activityRow is an activity as the pages show it: WeKan's
// client/components/activities/activities.jade, its words in the reader's language.
type activityRow struct {
	ID, User, Text string
	At             time.Time
}

// ISO is the time for a <time datetime>; Display what a reader sees.
func (a activityRow) ISO() string     { return a.At.UTC().Format(time.RFC3339) }
func (a activityRow) Display() string { return a.At.Local().Format("2006-01-02 15:04") }

// fillMessage puts args into a translation's %s, in order.
func fillMessage(text string, args ...string) string {
	for _, a := range args {
		text = strings.Replace(text, "%s", a, 1)
	}
	return text
}

// activityRows are the activities of filter, newest first, in lang.
func (s *Server) activityRows(ctx context.Context, b *Board, filter bson.M, lang string, limit int64) []activityRow {
	cur, err := s.db.Collection("activities").Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}}).SetLimit(limit))
	if err != nil {
		return nil
	}
	var docs []bson.M
	_ = cur.All(ctx, &docs)
	cache := map[string]string{}
	title := func(coll string, id any) string {
		sid, _ := id.(string)
		if sid == "" {
			return ""
		}
		k := coll + "/" + sid
		if v, ok := cache[k]; ok {
			return v
		}
		v := ""
		if coll == "users" {
			var u User
			if s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(sid)}).Decode(&u) == nil {
				v = u.Name()
			}
		} else {
			v = s.titleOfID(ctx, coll, sid)
		}
		cache[k] = v
		return v
	}
	out := make([]activityRow, 0, len(docs))
	for _, a := range docs {
		str := func(k string) string { v, _ := a[k].(string); return v }
		card, list, board := title("cards", a["cardId"]), title("lists", a["listId"]), b.Title
		if card == "" {
			card = str("cardTitle")
		}
		if list == "" {
			list = str("listName")
		}
		checklist := title("checklists", a["checklistId"])
		var key string
		var args []string
		switch str("activityType") {
		case "createBoard":
			key, args = msg("activity-created"), []string{board}
		case "importBoard":
			key, args = msg("activity-imported-board"), []string{board, ""}
		case "addBoardMember":
			key, args = msg("activity-added"), []string{title("users", a["memberId"]), board}
		case "removeBoardMember":
			key, args = msg("activity-excluded"), []string{title("users", a["memberId"]), board}
		case "createCard":
			key, args = msg("activity-added"), []string{card, list}
		case "moveCard":
			key, args = msg("activity-moved"), []string{card, title("lists", a["oldListId"]), list}
		case "archivedCard":
			key, args = msg("activity-archived"), []string{card}
		case "restoredCard":
			key, args = msg("activity-sent"), []string{card, board}
		case "addChecklist":
			key, args = msg("activity-checklist-added"), []string{card}
		case "removedChecklist":
			key, args = msg("activity-checklist-removed"), []string{card}
		case "addChecklistItem", "addedChecklistItem":
			key, args = msg("activity-checklist-item-added"), []string{checklist, card}
		case "addComment":
			key, args = msg("activity-on"), []string{card}
		case "editComment":
			key, args = msg("activity-editComment"), []string{str("commentText")}
		case "deleteComment":
			key, args = msg("activity-deleteComment"), []string{str("commentText")}
		case "addAttachment":
			key, args = msg("activity-attached"), []string{str("attachmentName"), card}
		case "deleteAttachment":
			key, args = msg("activity-delete-attach"), []string{card}
		case "createList":
			key, args = msg("activity-added"), []string{list, board}
		case "archivedList":
			key, args = msg("activity-archived"), []string{list}
		case "removeList":
			key, args = msg("activity-removed"), []string{str("title"), board}
		case "joinMember":
			key, args = msg("activity-added"), []string{title("users", a["memberId"]), card}
		case "unjoinMember":
			key, args = msg("activity-removed"), []string{title("users", a["memberId"]), card}
		case "createSwimlane":
			key, args = msg("activity-added"), []string{title("swimlanes", a["swimlaneId"]), board}
		case "createCustomField":
			key, args = msg("activity-customfield-created"), []string{title("customFields", a["customFieldId"])}
		default:
			key = str("activityType") // WeKan: {{_ activity.activityType}}
		}
		at, _ := a["createdAt"].(bson.DateTime)
		id, _ := a["_id"].(string)
		out = append(out, activityRow{ID: id, User: title("users", a["userId"]), Text: fillMessage(tr(lang, key), args...), At: at.Time()})
	}
	return out
}

// commentRow is a card's comment as the card details show it.
type commentRow struct {
	ID, User, Text string
	At             time.Time
}

func (c commentRow) ISO() string     { return c.At.UTC().Format(time.RFC3339) }
func (c commentRow) Display() string { return c.At.Local().Format("2006-01-02 15:04") }

// cardComments are a card's comments, newest first, as WeKan lists them.
func (s *Server) cardComments(ctx context.Context, b *Board, cardID string) []commentRow {
	cur, err := s.db.Collection("card_comments").Find(ctx, bson.M{"boardId": b.ID, "cardId": eq(cardID)},
		options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}}))
	if err != nil {
		return nil
	}
	var docs []struct {
		ID        string    `bson:"_id"`
		UserID    string    `bson:"userId"`
		Text      string    `bson:"text"`
		CreatedAt time.Time `bson:"createdAt"`
	}
	_ = cur.All(ctx, &docs)
	out := make([]commentRow, 0, len(docs))
	for _, d := range docs {
		var u User
		name := ""
		if s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(d.UserID)}).Decode(&u) == nil {
			name = u.Name()
		}
		out = append(out, commentRow{ID: d.ID, User: name, Text: d.Text, At: d.CreatedAt})
	}
	return out
}

// AddComment adds u's comment to c: WeKan's comment capability, and an
// assigned-only member only on a card assigned to them.
func (s *Server) AddComment(ctx context.Context, b *Board, u *User, c *Card, text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" || len(text) > 100000 {
		return "", errors.New("a comment is needed")
	}
	if !u.IsAdmin && (!b.Can(u.ID, "comment") || (b.member(u.ID).AssignedOnly() && !slices.Contains(c.Assignees, u.ID))) {
		return "", errForbidden
	}
	now := time.Now().UTC()
	id := randomID()
	if _, err := s.db.Collection("card_comments").InsertOne(ctx, bson.M{"_id": id, "boardId": b.ID, "cardId": c.ID, // inserted, not a filter
		"text": text, "userId": u.ID, "createdAt": now, "modifiedAt": now}); err != nil {
		return "", err
	}
	s.activity(ctx, u.ID, "addComment", bson.M{"boardId": b.ID, "cardId": c.ID, "commentId": id, // inserted, not a filter
		"listId": c.ListID, "swimlaneId": c.SwimlaneID})
	s.hub.publish(b.ID, "board-changed")
	return id, nil
}

var errForbidden = errors.New("forbidden")
