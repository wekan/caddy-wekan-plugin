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
	"net/http"
	"slices"
	"strings"

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

// AdminFieldBleed, the write side: WeKan's assertFieldWrite
// (server/lib/adminOnlyCustomFields.js). A user's card write may not change
// the values of the fields that user may not read - an admin-only field of a
// board they do not administer - nor, unless an admin, a read-only field's
// value (#3143). The card before and after are compared; any difference is
// refused and recorded, whatever path the write took.

// mayWrite is WeKan's mayWriteField, for this board.
func (fr *fieldReader) mayWrite(fieldID string) bool {
	if !fr.mayRead(fieldID) {
		return false
	}
	return fr.defs[fieldID]["readOnly"] != true || fr.admin
}

// fieldValues are the card's custom fields as {_id, value} pairs, in order.
func fieldValues(card map[string]any) []bson.M {
	var list []any
	switch v := card["customFields"].(type) {
	case bson.A:
		list = v
	case []any:
		list = v
	}
	out := []bson.M{}
	for _, f := range list {
		var m map[string]any
		switch x := f.(type) {
		case bson.M:
			m = x
		case map[string]any:
			m = x
		case bson.D:
			m = map[string]any{}
			for _, e := range x {
				m[e.Key] = e.Value
			}
		default:
			continue
		}
		out = append(out, bson.M{"_id": docString(m, "_id"), "value": plain(m["value"])}) // inserted, not a filter
	}
	return out
}

// protectedValues is WeKan's: the set values of fields the reader may not read.
func (fr *fieldReader) protectedValues(card map[string]any) string {
	var out []bson.M
	for _, f := range fieldValues(card) {
		if f["value"] != nil && !fr.mayRead(f["_id"].(string)) {
			out = append(out, f)
		}
	}
	return canonical(out)
}

// writeProtectedValues is WeKan's: the set values of read-only fields the
// reader may read but not write, by field id.
func (fr *fieldReader) writeProtectedValues(card map[string]any) string {
	var out []bson.M
	for _, f := range fieldValues(card) {
		id := f["_id"].(string)
		if f["value"] != nil && fr.defs[id]["readOnly"] == true && fr.mayRead(id) && !fr.mayWrite(id) {
			out = append(out, f)
		}
	}
	slices.SortFunc(out, func(a, b bson.M) int { return strings.Compare(a["_id"].(string), b["_id"].(string)) })
	return canonical(out)
}

// fieldWriteRefused is assertFieldWrite: true - recorded - when the write
// from before to after changes a value the user may not.
func (s *Server) fieldWriteRefused(r *http.Request, u *User, b *Board, before, after map[string]any, source string) bool {
	if u == nil {
		return false // trusted server-side writes have no requester
	}
	fr := s.fieldReaderFor(r.Context(), b, u)
	if before == nil {
		before = map[string]any{}
	}
	if after == nil {
		after = map[string]any{}
	}
	if fr.protectedValues(before) == fr.protectedValues(after) && fr.writeProtectedValues(before) == fr.writeProtectedValues(after) {
		return false
	}
	s.recordSecurity(r, securityEvent{Key: "authz.admin-only-field", Action: "blocked", Source: source, UserID: u.ID,
		Detail: "Protected custom-field mutation refused"})
	return true
}

// readableFieldsOnly is the copy's custom fields as WeKan's Card.copy keeps
// them for its actor: only those the copier may read.
func (fr *fieldReader) readableFieldsOnly(v any) bson.A {
	out := bson.A{}
	for _, f := range fieldValues(map[string]any{"customFields": v}) {
		if fr.mayRead(f["_id"].(string)) {
			out = append(out, f)
		}
	}
	return out
}
