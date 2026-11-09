package web

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Meteor's /cards/update - the client's own write of a card, as WeKan's
// Cards.allow (server/permissions/cards.js canUpdateCard) lets a board member
// with write access make it - for the fields Weddy's pages and WeKan's tests
// write this way: the title, the description and the color, by $set or
// $unset, and archived by $set, of one card named by its id. Any other field, operator or selector
// is Meteor's 403, as every collection write Weddy has no rule for: the
// fields WeKan guards with deny rules - votes, poker, its board or parent,
// custom fields, links, Scrum - never get this far.

var cardClientFields = map[string]bool{"title": true, "description": true, "color": true, "archived": true}

// hexColor is WeKan's isHexColor: a strict #rrggbb (#5514), never anything a
// style could be broken out of.
var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// cardColors are WeKan's ALLOWED_COLORS (config/const.js).
var cardColors = map[string]bool{"white": true, "green": true, "yellow": true, "orange": true, "red": true, "purple": true, "blue": true,
	"sky": true, "lime": true, "pink": true, "black": true, "silver": true, "peachpuff": true, "crimson": true, "plum": true,
	"darkgreen": true, "slateblue": true, "magenta": true, "gold": true, "navy": true, "gray": true, "saddlebrown": true,
	"paleturquoise": true, "mistyrose": true, "indigo": true}

func (s *Server) methodCardsUpdate(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	denied := &methodError{"403", "Access denied"}
	if len(args) < 2 {
		return nil, &methodError{"Match.Error", "Match failed"}
	}
	// The selector: an id, or {_id: id} - as Meteor allows an untrusted client.
	var id string
	if json.Unmarshal(args[0], &id) != nil {
		var sel map[string]any
		if json.Unmarshal(args[0], &sel) != nil || len(sel) != 1 {
			return nil, denied
		}
		id, _ = sel["_id"].(string)
	}
	if !validID(id) {
		return nil, denied
	}
	var mod map[string]map[string]any
	if json.Unmarshal(args[1], &mod) != nil || len(mod) == 0 {
		return nil, denied
	}
	set, unset := bson.M{}, bson.M{}
	for op, fields := range mod {
		if (op != "$set" && op != "$unset") || len(fields) == 0 {
			return nil, denied
		}
		for k, v := range fields {
			if !cardClientFields[k] {
				return nil, denied
			}
			if k == "archived" {
				on, ok := v.(bool)
				if op != "$set" || !ok {
					return nil, &methodError{"400", "archived is true or false"}
				}
				set[k] = on
				continue
			}
			if op == "$unset" {
				if k == "title" {
					return nil, &methodError{"400", "Title is required"}
				}
				unset[k] = ""
				continue
			}
			str, ok := v.(string)
			switch {
			case !ok:
				return nil, &methodError{"400", k + " must be text"}
			case k == "title" && (strings.TrimSpace(str) == "" || len(str) > 10000):
				return nil, &methodError{"400", "Title is required"}
			case k == "description" && len(str) > 1<<20:
				return nil, &methodError{"400", "the description is too long"}
			case k == "color" && str != "" && !cardColors[str] && !hexColor.MatchString(str):
				return nil, &methodError{"400", "not a card color"}
			}
			set[k] = str
		}
	}
	ctx := r.Context()
	var card Card
	if s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&card) != nil {
		return 0, nil // as Meteor: no such card is no document updated
	}
	b, err := s.boardFor(ctx, card.BoardID, u)
	if err != nil || !b.WritableIn(ctx, s, u, "write", &card) {
		return nil, denied
	}
	now := time.Now().UTC()
	set["modifiedAt"] = now
	archiving, archive := set["archived"].(bool)
	if archive && archiving && !card.Archived {
		set["archivedAt"] = now
	}
	update := bson.M{"$set": set}
	if len(unset) > 0 {
		update["$unset"] = unset
	}
	res, err := s.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": eq(id), "boardId": b.ID}, update)
	if err != nil {
		return nil, err
	}
	// WeKan's after.update hooks: the title and description activities - and
	// with them the rules - once the write succeeded.
	if _, cleared := unset["description"]; cleared {
		set["description"] = ""
	}
	s.cardTextActivities(ctx, u, &card, set)
	// Archived or restored: once, when it changed.
	if archive && archiving != card.Archived {
		if archiving {
			s.activity(ctx, u.ID, "archivedCard", bson.M{"boardId": b.ID, "cardId": card.ID, "listId": card.ListID, "swimlaneId": card.SwimlaneID, "cardTitle": card.Title})
		} else {
			s.activity(ctx, u.ID, "restoredCard", bson.M{"boardId": b.ID, "cardId": card.ID, "listId": card.ListID, "swimlaneId": card.SwimlaneID, "cardTitle": card.Title})
		}
	}
	s.hub.publish(b.ID, "board-changed")
	return res.MatchedCount, nil // Meteor answers the number it matched
}
