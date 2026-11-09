package web

import (
	"context"
	"errors"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// A rule mail about a linked card, as Node.js WeKan's
// server/lib/ruleEmailSource.js resolveRuleEmailSource and
// ruleCardWrapper.js: the mail is about the card the link shows - followed
// through links of links, at most 32, never in a circle - and every card on
// the way must be readable by the rule's user, or no mail is sent. The
// subject, the message, the footer and what is included come from that
// source card; with "Details", the link's own local details follow.

var errRuleMailSource = errors.New("rule-email-source-not-authorized")

// ruleMailSource is the card the mail is about, and the chain to it (the
// triggering card first).
func (s *Server) ruleMailSource(ctx context.Context, userID string, c *Card) (*Card, []string, error) {
	var chain []string
	seen := map[string]bool{}
	cur := c
	for {
		if seen[cur.ID] || len(seen) >= 32 {
			return nil, nil, errors.New("rule-email-source-invalid")
		}
		seen[cur.ID] = true
		chain = append(chain, cur.ID)
		if !s.ruleMailCardReadable(ctx, userID, cur.BoardID, cur.ID) {
			return nil, nil, errRuleMailSource
		}
		if cur.Type != "cardType-linkedCard" {
			return cur, chain, nil
		}
		var raw struct {
			LinkedID string `bson:"linkedId"`
		}
		var next Card
		if s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(cur.ID)}).Decode(&raw) != nil || !validID(raw.LinkedID) ||
			s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(raw.LinkedID)}).Decode(&next) != nil {
			return nil, nil, errors.New("rule-email-source-invalid")
		}
		cur = &next
	}
}

// ruleMailSourceCurrent is assertCurrent: every card of the chain still
// readable and still linking where it did.
func (s *Server) ruleMailSourceCurrent(ctx context.Context, userID string, chain []string) error {
	for i, id := range chain {
		var raw struct {
			BoardID  string `bson:"boardId"`
			Type     string `bson:"type"`
			LinkedID string `bson:"linkedId"`
		}
		if s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&raw) != nil ||
			!s.ruleMailCardReadable(ctx, userID, raw.BoardID, id) {
			return errRuleMailSource
		}
		if i+1 < len(chain) && (raw.Type != "cardType-linkedCard" || raw.LinkedID != chain[i+1]) ||
			i+1 == len(chain) && raw.Type == "cardType-linkedCard" {
			return errors.New("rule-email-source-changed")
		}
	}
	return nil
}

// ruleMailWrapper is WeKan's prepareRuleCardWrapper for a linked card: the
// link's own board, list, swimlane and local fields - never the copied
// source content it carries.
func (s *Server) ruleMailWrapper(ctx context.Context, userID string, link *Card) (string, error) {
	if !s.ruleMailCardReadable(ctx, userID, link.BoardID, link.ID) {
		return "", errors.New("rule-email-wrapper-not-authorized")
	}
	var raw bson.M
	if s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(link.ID)}).Decode(&raw) != nil {
		return "", errors.New("rule-email-wrapper-not-authorized")
	}
	card, _ := plain(raw).(map[string]any)
	board := s.ruleMailRawBoard(ctx, link.BoardID)
	scrumPolicy := canonical(ruleMailScrumPolicy(board))
	lines := []string{"Linked card local details:"}
	add := func(label string, v any) {
		var text string
		switch x := v.(type) {
		case []any:
			var parts []string
			for _, e := range x {
				if s, ok := e.(string); ok {
					parts = append(parts, s)
				}
			}
			text = strings.Join(parts, ", ")
		case time.Time:
			if !x.IsZero() {
				text = x.UTC().Format("2006-01-02T15:04:05.000Z")
			}
		default:
			text = ruleMailScalar(v)
		}
		if text != "" {
			lines = append(lines, label+": "+text)
		}
	}
	title, _ := board["title"].(string)
	add("Board", title)
	str := func(v any) string { s, _ := v.(string); return s }
	for _, f := range [][2]string{{"lists", "listId"}, {"swimlanes", "swimlaneId"}} {
		var d struct {
			Title   string `bson:"title"`
			BoardID string `bson:"boardId"`
		}
		if id := str(card[f[1]]); validID(id) && s.db.Collection(f[0]).FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&d) == nil && d.BoardID == link.BoardID {
			add(map[string]string{"lists": "List", "swimlanes": "Swimlane"}[f[0]], d.Title)
		}
	}
	for _, f := range [][2]string{{"archived", "Archived"}, {"archivedAt", "Archived at"}, {"createdAt", "Created"},
		{"modifiedAt", "Modified"}, {"dateLastActivity", "Last activity"}, {"recurrenceInterval", "Recurrence"},
		{"lastRecurrenceAt", "Last recurrence"}, {"sort", "Sort"}, {"subtaskSort", "Subtask sort"}, {"listEnteredAt", "Entered list"},
		{"lastMoveReason", "Last move reason"}, {"flowStartAt", "Flowtime started"}, {"flowInterruptions", "Flowtime interruptions"},
		{"pomodoroStartAt", "Pomodoro started"}, {"pomodoroPhase", "Pomodoro phase"},
		{"pomodoroCount", "Pomodoro completed intervals"}, {"pomodoroWorkMinutes", "Pomodoro work interval (minutes)"}} {
		add(f[1], card[f[0]])
	}
	for _, f := range [][2]string{{"flowUserId", "Flowtime user"}, {"pomodoroUserId", "Pomodoro user"}} {
		if id := str(card[f[0]]); id != "" {
			add(f[1], s.ruleMailDisplayName(ctx, id))
		}
	}
	scrumCurrent := s.ruleMailScrum(ctx, card, board, link.BoardID, add)
	if err := scrumCurrent(); err != nil {
		return "", err
	}
	var now struct {
		Type     string `bson:"type"`
		LinkedID string `bson:"linkedId"`
	}
	if !s.ruleMailCardReadable(ctx, userID, link.BoardID, link.ID) ||
		s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(link.ID)}).Decode(&now) != nil ||
		now.Type != str(card["type"]) || now.LinkedID != str(card["linkedId"]) ||
		canonical(ruleMailScrumPolicy(s.ruleMailRawBoard(ctx, link.BoardID))) != scrumPolicy {
		return "", errors.New("rule-email-wrapper-changed")
	}
	return strings.Join(lines, "\n"), nil
}
