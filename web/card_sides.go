package web

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// What Card Settings' sides added on 2026-10-02 draw (WeKan's minicard.jade
// and cardDetails.jade), each by its board flag: on where the surface drew it
// before the row existed, off where it is new.

// cardLocation is one of the card's locations (WeKan's card.locations).
type cardLocation struct {
	Name    string `bson:"name,omitempty"`
	Address string `bson:"address,omitempty"`
}

// fillCardExtras reads what Card Settings' sides draw from the card's
// document, each of the type WeKan stores and anything else as none.
func fillCardExtras(c *Card, doc bson.M) {
	c.Color, _ = doc["color"].(string)
	c.RequestedBy, _ = doc["requestedBy"].(string)
	c.Locations = nil
	if list, ok := plain(doc["locations"]).([]any); ok {
		for _, v := range list {
			if m, ok := v.(map[string]any); ok {
				name, _ := m["name"].(string)
				addr, _ := m["address"].(string)
				c.Locations = append(c.Locations, cardLocation{Name: name, Address: addr})
			}
		}
	}
	at := func(k string) *time.Time { return dueDateOf(doc[k]).At }
	if t := at("dateLastActivity"); t != nil {
		c.DateLastActivity = *t
	}
	c.ReceivedAt, c.StartAt, c.DueAt, c.EndAt = at("receivedAt"), at("startAt"), at("dueAt"), at("endAt")
}

// fillCardsExtras is fillCardExtras for many cards: one query.
func (s *Server) fillCardsExtras(ctx context.Context, cards []*Card) {
	if len(cards) == 0 {
		return
	}
	ids := bson.A{}
	byID := map[string]*Card{}
	for _, c := range cards {
		ids = append(ids, c.ID)
		byID[c.ID] = c
	}
	cur, err := s.db.Collection("cards").Find(ctx, bson.M{"_id": dbIn(ids)}, options.Find().SetProjection(bson.M{
		"color": 1, "requestedBy": 1, "locations": 1, "dateLastActivity": 1, "receivedAt": 1, "startAt": 1, "dueAt": 1, "endAt": 1}))
	if err != nil {
		return
	}
	var docs []bson.M
	_ = cur.All(ctx, &docs)
	for _, d := range docs {
		if c := byID[docString(d, "_id")]; c != nil {
			fillCardExtras(c, d)
		}
	}
}

// minicardSides are the minicard's: its colour, who requested it, where,
// when it last changed, and the description's badge.
type minicardSides struct {
	ColorClass, RequestedBy, LastActivity string
	Locations                             []string
	DescriptionBadge                      bool
}

// boardFlag is a board's boolean setting, or def when it never set it.
func boardFlag(board bson.M, field string, def bool) bool {
	if v, ok := board[field].(bool); ok {
		return v
	}
	return def
}

func sidesOfMinicard(board bson.M, c *Card, dateFormat string) minicardSides {
	var out minicardSides
	if c.Color != "" && boardFlag(board, "allowsCardColorOnMinicard", true) {
		out.ColorClass = "minicard-" + c.Color
	}
	if c.RequestedBy != "" && boardFlag(board, "allowsRequestedByOnMinicard", true) {
		out.RequestedBy = c.RequestedBy
	}
	if boardFlag(board, "allowsLocationOnMinicard", false) {
		for _, l := range c.Locations {
			if l.Name != "" {
				out.Locations = append(out.Locations, l.Name)
			} else if l.Address != "" {
				out.Locations = append(out.Locations, l.Address)
			}
		}
	}
	if !c.DateLastActivity.IsZero() && boardFlag(board, "allowsActivitiesOnMinicard", false) {
		if dateFormat == "" {
			dateFormat = "YYYY-MM-DD"
		}
		out.LastActivity = formatDateByUserPreference(c.DateLastActivity, dateFormat, true, "")
	}
	out.DescriptionBadge = c.Description != "" && !boardFlag(board, "allowsDescriptionTextOnMinicard", false) &&
		boardFlag(board, "allowsDescriptionBadgeOnMinicard", true)
	return out
}

// openedCardSides are the opened card's: the header's colour, the collapse
// caret and whether the user folded the card, the swimlane's name, and the
// description's badge.
type openedCardSides struct {
	ColorClass, SwimlaneName              string
	Collapse, Collapsed, DescriptionBadge bool
}

func (s *Server) sidesOfOpenedCard(ctx context.Context, b *Board, c *Card, u *User) openedCardSides {
	var board bson.M
	_ = s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(b.ID)}).Decode(&board)
	var out openedCardSides
	if c.Color != "" && boardFlag(board, "allowsCardColorOnCard", true) {
		out.ColorClass = "card-details-" + c.Color
	}
	// A board that turned the caret off has no collapsed cards.
	out.Collapse = boardFlag(board, "allowsCardCollapse", true)
	out.Collapsed = out.Collapse && u != nil && u.Profile.CardCollapsed
	if boardFlag(board, "allowsSwimlaneNameOnCard", false) {
		var sw struct {
			Title string `bson:"title"`
		}
		if s.db.Collection("swimlanes").FindOne(ctx, bson.M{"_id": eq(c.SwimlaneID), "boardId": b.ID}).Decode(&sw) == nil {
			out.SwimlaneName = sw.Title
		}
	}
	out.DescriptionBadge = c.Description != "" && boardFlag(board, "allowsDescriptionBadgeOnCard", false)
	return out
}

// methodSetCardCollapsed is WeKan's setCardCollapsed(value): the user's own
// fold of the opened card's details.
func (s *Server) methodSetCardCollapsed(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	var v bool
	if len(args) < 1 || json.Unmarshal(args[0], &v) != nil {
		return nil, &methodError{"Match.Error", "Expected boolean"}
	}
	_, err := s.db.Collection("users").UpdateOne(r.Context(), bson.M{"_id": eq(u.ID)}, bson.M{"$set": bson.M{"profile.cardCollapsed": v}})
	return nil, err
}
