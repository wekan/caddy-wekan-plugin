package web

import (
	"encoding/json"
	"math"
	"net/http"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// WeKan's setCardCustomFieldAssigned, setCardCustomFieldCheckbox and
// setCardCustomFieldCurrency (server/models/cards.js): for whoever may
// change the card - an assigned-only member only theirs - on a field of
// the card's own board, of the right type; an admin-only field (#3141) only
// by a board admin, its refusal recorded as AdminFieldBleed. A change is an
// activity (setCustomField with its value), as WeKan's cardCustomFields hook
// writes it, so watchers are notified.

func (s *Server) customFieldMethods() map[string]methodFunc {
	return map[string]methodFunc{
		"setCardCustomFieldAssigned": s.customFieldMethod("assigned"),
		"setCardCustomFieldCheckbox": s.customFieldMethod("checkbox"),
		"setCardCustomFieldCurrency": s.customFieldMethod("currency"),
		"setCreateUser":              s.methodSetCreateUser,
		// WeKan's isAvatarUploadBlocked (#4740): Admin Panel's avatar block.
		"isAvatarUploadBlocked": func(r *http.Request, u *User, args []json.RawMessage) (any, error) {
			return s.avatarUploadBlocked(r), nil
		},
	}
}

func (s *Server) customFieldMethod(kind string) methodFunc {
	return func(r *http.Request, u *User, args []json.RawMessage) (any, error) {
		cardID, err := stringArg(args, 0)
		if err != nil {
			return nil, err
		}
		fieldID, err := stringArg(args, 1)
		if err != nil {
			return nil, err
		}
		var value any
		if len(args) < 3 {
			return nil, &methodError{"Match.Error", "Expected a value"}
		}
		switch kind {
		case "assigned", "checkbox":
			var v bool
			if json.Unmarshal(args[2], &v) != nil {
				return nil, &methodError{"Match.Error", "Expected Boolean"}
			}
			value = v
		case "currency":
			var v float64
			if json.Unmarshal(args[2], &v) != nil {
				return nil, &methodError{"Match.Error", "Expected Number"}
			}
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return nil, &methodError{"invalid-custom-field-value", "invalid-custom-field-value"}
			}
			value = v
		}
		return s.setCardCustomField(r, u, cardID, fieldID, kind, func(bson.M) (any, error) { return value, nil })
	}
}

// setCardCustomField is the one place a card's custom field is put on it,
// taken off, or given a value - the Meteor methods above and the card
// details' forms alike: for whoever may change the card, on a field of the
// card's own board (of kind's type, for checkbox and currency); an
// admin-only field's value (#3141) only by a board admin, the refusal
// recorded as AdminFieldBleed. kind "assigned" puts the field on (true) or
// takes it off (false); "value" takes parse's value for the field's type.
func (s *Server) setCardCustomField(r *http.Request, u *User, cardID, fieldID, kind string, parse func(field bson.M) (any, error)) (any, error) {
	ctx := r.Context()
	var card Card
	var raw bson.M
	if !validID(cardID) || s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(cardID)}).Decode(&raw) != nil {
		return nil, &methodError{"not-found", "not-found"}
	}
	doc, _ := bson.Marshal(raw)
	_ = bson.Unmarshal(doc, &card)
	var b Board
	if s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(card.BoardID)}).Decode(&b) != nil || !b.WritableIn(ctx, s, u, "write", &card) {
		return nil, &methodError{"not-authorized", "not-authorized"}
	}
	filter := bson.M{"_id": eq(fieldID), "boardIds": b.ID}
	if kind == "checkbox" || kind == "currency" {
		filter["type"] = kind
	}
	var field bson.M
	if !validID(fieldID) || s.db.Collection("customFields").FindOne(ctx, filter).Decode(&field) != nil {
		return nil, &methodError{"custom-field-not-found", "custom-field-not-found"}
	}
	if field["adminOnly"] == true && kind != "assigned" {
		if m := b.member(u.ID); m == nil || !m.IsAdmin {
			source := map[string]string{"checkbox": "method:setCardCustomFieldCheckbox", "currency": "method:setCardCustomFieldCurrency"}[kind]
			if source == "" {
				source = "cardDetails:customField"
			}
			s.recordSecurity(r, securityEvent{Key: "authz.admin-only-field", Action: "blocked", Source: source,
				UserID: u.ID, Detail: "Protected custom-field mutation refused"})
			return nil, &methodError{"not-authorized", "Protected custom field"}
		}
	}
	value, err := parse(field)
	if err != nil {
		return nil, err
	}
	fields := bson.A{}
	if a, ok := raw["customFields"].(bson.A); ok {
		fields = a
	}
	index := -1
	for i, f := range fields {
		if nestedField(f, "_id") == fieldID {
			index = i
		}
	}
	var before any
	switch {
	case kind == "assigned" && value == true:
		if index >= 0 {
			return true, nil
		}
		fields = append(fields, bson.M{"_id": fieldID, "value": nil}) // inserted, not a filter
	case kind == "assigned":
		if index < 0 {
			return false, nil
		}
		before = nestedField(fields[index], "value")
		fields = append(fields[:index:index], fields[index+1:]...)
	default:
		if index < 0 {
			return nil, &methodError{"custom-field-not-on-card", "custom-field-not-on-card"}
		}
		before = nestedField(fields[index], "value")
		fields[index] = bson.M{"_id": fieldID, "value": value} // inserted, not a filter
	}
	// The whole array: FerretDB has no positional update.
	if _, err := s.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": card.ID}, bson.M{"$set": bson.M{"customFields": fields, "modifiedAt": time.Now().UTC()}}); err != nil {
		return nil, err
	}
	after := any(nil)
	if kind != "assigned" {
		after = value
	}
	if !sameFieldValue(before, after) {
		act := bson.M{"customFieldId": fieldID, "boardId": b.ID, "cardId": card.ID, "listId": card.ListID, "swimlaneId": card.SwimlaneID}
		typ := "unsetCustomField"
		if after != nil {
			act["value"], typ = after, "setCustomField"
		}
		s.activity(ctx, u.ID, typ, act)
		s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	}
	return value, nil
}

// sameFieldValue compares two stored values, arrays (a multi-select) too.
func sameFieldValue(a, b any) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}
