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
	for _, fields := range mod {
		for k := range fields {
			if k == "customFields" || strings.HasPrefix(k, "customFields.") {
				return s.cardsUpdateCustomFields(r, u, id, mod)
			}
		}
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
	res, err := s.cardsUpdateOne(ctx, u.ID, bson.M{"_id": eq(id), "boardId": b.ID}, update)
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
	s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	return res.MatchedCount, nil // Meteor answers the number it matched
}

// cardsUpdateCustomFields is /cards/update of a card's custom fields, under
// WeKan's rules: the modifier is applied to a copy of the card and checked
// with assertFieldWrite - a change to a value the caller may not read, or to
// a read-only one they may not write, is AdminFieldBleed, refused and
// recorded (a replace, a $push or an $unset of the array included). What a
// card editor may then write is the one change WeKan's card details make, a
// field's value by its index: $set customFields.N.value. Anything else is
// Meteor's 403.
func (s *Server) cardsUpdateCustomFields(r *http.Request, u *User, id string, mod map[string]map[string]any) (any, error) {
	denied := &methodError{"403", "Access denied"}
	ctx := r.Context()
	var raw bson.M
	if s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&raw) != nil {
		return 0, nil
	}
	var card Card
	_ = s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&card)
	b, err := s.boardFor(ctx, card.BoardID, u)
	if err != nil || !b.WritableIn(ctx, s, u, "write", &card) {
		return nil, denied
	}
	before := plain(raw).(map[string]any)
	fields := fieldValues(before)
	after := map[string]any{}
	for k, v := range before {
		after[k] = v
	}
	valueEdit := true
	for op, set := range mod {
		for k, v := range set {
			parts := strings.Split(k, ".")
			switch {
			case op == "$set" && k == "customFields":
				after["customFields"] = v
				valueEdit = false
			case op == "$push" && k == "customFields":
				list := []any{}
				for _, f := range fieldValues(after) {
					list = append(list, f)
				}
				after["customFields"] = append(list, v)
				valueEdit = false
			case op == "$unset" && k == "customFields":
				delete(after, "customFields")
				valueEdit = false
			case op == "$set" && len(parts) == 3 && parts[0] == "customFields" && digits.MatchString(parts[1]) && parts[2] == "value":
				i := 0
				for _, c := range parts[1] {
					if i = i*10 + int(c-'0'); i >= len(fields) {
						return nil, denied
					}
				}
				list := fieldValues(after)
				list[i]["value"] = v
				anyList := []any{}
				for _, f := range list {
					anyList = append(anyList, f)
				}
				after["customFields"] = anyList
			default:
				return nil, denied
			}
		}
	}
	if s.fieldWriteRefused(r, u, b, before, after, "ddp:cards.update") {
		return nil, &methodError{"not-authorized", "Protected custom field"}
	}
	if !valueEdit {
		return nil, denied
	}
	fr := s.fieldReaderFor(ctx, b, u)
	next := bson.A{}
	for i, f := range fieldValues(after) {
		if canonical(f) != canonical(fields[i]) && !fr.mayWrite(f["_id"].(string)) {
			return nil, denied // a field no longer on the board, or one they may not write
		}
		next = append(next, f)
	}
	if _, err := s.cardsUpdateOne(ctx, u.ID, bson.M{"_id": card.ID}, bson.M{"$set": bson.M{"customFields": next, "modifiedAt": time.Now().UTC()}}); err != nil {
		return nil, err
	}
	s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	return 1, nil
}
