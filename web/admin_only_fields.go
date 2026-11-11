package web

// AdminFieldBleed, the read side: an admin-only custom field's value is read
// only by an admin of the card's board, as WeKan's
// models/lib/adminOnlyCustomFields.js redact. Weddy's card page already left
// such fields out (cardFieldDefinitions), but the REST API sent the card
// document whole - GET /api/cards/{card}, the list's card and the cards of a
// custom field's value - so any member read the value; and finding cards BY
// that value told them which card held it without showing it.
//
// As in WeKan the field's identity and position stay - the card editors
// address customFields by index - and only the value becomes null; a value
// whose definition is missing or belongs to another board is hidden too, and
// so are the card's integrityHash and previousHash, a digest of a short
// hidden value being a guessing oracle.
//
// Not recorded: an ordinary member's ordinary read reaches these routes.

import (
	"context"
	"slices"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// fieldReader is what one user may read of a board's custom fields.
type fieldReader struct {
	boardID string
	admin   bool
	defs    map[string]bson.M
}

// fieldReaderFor reads the board's definitions once, for u.
func (s *Server) fieldReaderFor(ctx context.Context, b *Board, u *User) *fieldReader {
	fr := &fieldReader{boardID: b.ID, admin: u != nil && b.AdminOf(u.ID), defs: map[string]bson.M{}}
	cur, err := s.db.Collection("customFields").Find(ctx, bson.M{"boardIds": b.ID})
	if err != nil {
		return fr
	}
	var defs []bson.M
	_ = cur.All(ctx, &defs)
	for _, d := range defs {
		fr.defs[docString(d, "_id")] = d
	}
	return fr
}

// mayRead is WeKan's mayReadField, for this board.
func (fr *fieldReader) mayRead(fieldID string) bool {
	d := fr.defs[fieldID]
	if d == nil || !slices.Contains(stringsOf(d["boardIds"]), fr.boardID) {
		return false
	}
	return d["adminOnly"] != true || fr.admin
}

// redactCard hides, in place, what of the card this reader may not read.
func (fr *fieldReader) redactCard(card bson.M) bson.M {
	if card == nil {
		return card
	}
	if !fr.admin {
		for _, k := range []string{"integrityHash", "previousHash"} {
			if _, ok := card[k]; ok {
				card[k] = nil
			}
		}
	}
	var fields []any
	switch v := card["customFields"].(type) {
	case bson.A:
		fields = v
	case []any:
		fields = v
	default:
		return card
	}
	out := make(bson.A, 0, len(fields))
	for _, f := range fields {
		m, ok := f.(bson.M)
		if !ok {
			if d, isD := f.(bson.D); isD {
				m = bson.M{}
				for _, e := range d {
					m[e.Key] = e.Value
				}
				ok = true
			}
		}
		if !ok {
			out = append(out, f)
			continue
		}
		if _, has := m["value"]; has && !fr.mayRead(docString(m, "_id")) {
			out = append(out, bson.M{"_id": m["_id"], "value": nil})
			continue
		}
		out = append(out, m)
	}
	card["customFields"] = out
	return card
}
