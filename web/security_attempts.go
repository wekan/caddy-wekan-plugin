package web

// Attempts Weddy already refused, now recorded in Admin Panel -> Problems -
// and costing the sender the account - as Node.js WeKan's guards do, under the
// same Hall of Fame names:
//
//   - PrototypeBleed (injection.prototype, models/lib/safeMapKey.js): a
//     per-user layout method given '__proto__', 'constructor', 'prototype', a
//     '.' or a leading '$' as a map key.
//   - RepointBleed (authz.repoint, server/lib/boardRepointGuard.js and
//     server/permissions/customFields.js): a client collection write moving a
//     rule trigger, action, rule or webhook integration to another board, or
//     putting a custom field on, or taking it off, a board the sender cannot
//     write to.
//   - BackgroundBleed (authz.background, server/permissions/boards.js): a
//     board's background pointed at an attachment of another board.
//   - HookBleed (ssrf.webhook-forge, server/notifications/outgoing.js): a
//     client asking the server to send anything but the card-opened notice
//     through a board's webhook.
//
// Weddy writes none of these collections from a client - every such write is
// denied in methodRoutes - so these only name WHAT the denied write tried, and
// only for the shapes no Weddy or WeKan page sends: an ordinary denied write
// is not recorded (the test of "can a legitimate user reach this line").

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// unsafeMapKey is WeKan's !isSafeMapKey: what must never become a key of a
// per-user map such as profile.collapsedLists.
func unsafeMapKey(key string) bool {
	switch key {
	case "__proto__", "constructor", "prototype":
		return true
	}
	// An empty one is ordinary bad input, which validID refuses unrecorded.
	return len(key) > 256 || strings.Contains(key, ".") || strings.HasPrefix(key, "$")
}

// refuseMapKeys records a PrototypeBleed attempt when any key is unsafe, and
// reports whether one was.
func (s *Server) refuseMapKeys(r *http.Request, u *User, source string, keys ...string) bool {
	for _, k := range keys {
		if unsafeMapKey(k) {
			s.recordSecurity(r, securityEvent{Key: "injection.prototype", Action: "blocked", Source: source, UserID: u.ID,
				Detail: "refused map key " + strings.Trim(jsonQuote(k), `"`)})
			return true
		}
	}
	return false
}

func jsonQuote(s string) string {
	if len(s) > 40 {
		s = s[:40]
	}
	b, _ := json.Marshal(s)
	return string(b)
}

// repointCollections: the collections whose documents belong to ONE board
// by boardId, which no client changes once they exist.
var repointCollections = map[string]bool{"triggers": true, "actions": true, "rules": true, "integrations": true}

// modifierTouches reports whether a Mongo update modifier writes any of
// fields - by any operator, a dotted sub-path, or as a $rename's target.
func modifierTouches(mod map[string]any, fields ...string) bool {
	hits := func(name string) bool {
		for _, f := range fields {
			if name == f || strings.HasPrefix(name, f+".") {
				return true
			}
		}
		return false
	}
	for op, v := range mod {
		m, ok := v.(map[string]any)
		if !ok {
			if !strings.HasPrefix(op, "$") && hits(op) { // a replacement document
				return true
			}
			continue
		}
		for name, val := range m {
			if hits(name) {
				return true
			}
			if op == "$rename" {
				if to, ok := val.(string); ok && hits(to) {
					return true
				}
			}
		}
	}
	return false
}

// boardIDsIn: the board ids a value names - one id, or a list, or a
// $each list.
func boardIDsIn(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		var out []string
		for _, e := range t {
			out = append(out, boardIDsIn(e)...)
		}
		return out
	case map[string]any:
		if e, ok := t["$each"]; ok {
			return boardIDsIn(e)
		}
		if e, ok := t["$in"]; ok {
			return boardIDsIn(e)
		}
	}
	return nil
}

// noteDeniedCollectionWrite looks at a client collection write that
// methodRoutes is about to deny, and records it when it is one of the attacks
// above. It never changes the answer.
func (s *Server) noteDeniedCollectionWrite(r *http.Request, u *User, name string, args []json.RawMessage) {
	defer func() { _ = recover() }() // logging must never break the guard
	collection, op, _ := strings.Cut(strings.TrimPrefix(name, "/"), "/")
	ctx := r.Context()
	arg := func(i int) map[string]any {
		var m map[string]any
		if i < len(args) {
			_ = json.Unmarshal(args[i], &m)
		}
		return m
	}
	switch {
	case repointCollections[collection] && (op == "update" || op == "upsert"):
		if modifierTouches(arg(1), "boardId") {
			s.recordSecurity(r, securityEvent{Key: "authz.repoint", Action: "blocked", Source: "ddp:" + collection + "." + op, UserID: u.ID,
				Detail: "Tried to move a " + collection + " document to another board."})
		}
	case collection == "customFields" && (op == "insert" || op == "update" || op == "upsert"):
		var named []string
		if op == "insert" {
			named = boardIDsIn(arg(0)["boardIds"])
		} else {
			for _, v := range arg(1) {
				if m, ok := v.(map[string]any); ok {
					for field, val := range m {
						if field == "boardIds" || strings.HasPrefix(field, "boardIds.") {
							named = append(named, boardIDsIn(val)...)
						}
					}
				}
			}
		}
		if s.anyBoardWithoutWrite(ctx, u, named) {
			s.recordSecurity(r, securityEvent{Key: "authz.repoint", Action: "blocked", Source: "ddp:customFields." + op, UserID: u.ID,
				Detail: "Tried to put a custom field on, or take it off, a board without write access there."})
		}
	case collection == "boards" && (op == "update" || op == "upsert"):
		mod := arg(1)
		set, _ := mod["$set"].(map[string]any)
		id, _ := set["backgroundImageId"].(string)
		renamed := false
		if rn, ok := mod["$rename"].(map[string]any); ok {
			for _, to := range rn {
				renamed = renamed || to == "backgroundImageId"
			}
		}
		if id == "" && !renamed {
			return
		}
		boardID, _ := arg(0)["_id"].(string)
		if boardID == "" && len(args) > 0 {
			_ = json.Unmarshal(args[0], &boardID)
		}
		if !renamed && s.attachmentOnBoard(ctx, id, boardID) {
			return // its own image: what the background picker sets
		}
		s.recordSecurity(r, securityEvent{Key: "authz.background", Action: "blocked", Source: "ddp:boards." + op, UserID: u.ID,
			Detail: "Tried to set a board background to an attachment of another board."})
	}
}

// anyBoardWithoutWrite: does ids name a board u cannot write to?
func (s *Server) anyBoardWithoutWrite(ctx context.Context, u *User, ids []string) bool {
	for _, id := range ids {
		var b Board
		if !validID(id) || s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&b) != nil || !b.Writable(u, "write", nil) {
			return true
		}
	}
	return false
}

// attachmentOnBoard: is attachment id one of board boardID's?
func (s *Server) attachmentOnBoard(ctx context.Context, id, boardID string) bool {
	if !validID(id) || !validID(boardID) {
		return false
	}
	n, err := s.db.Collection("attachments").CountDocuments(ctx, bson.M{"_id": eq(id), "meta.boardId": eq(boardID)})
	return err == nil && n > 0
}

// methodOutgoingWebhooks is WeKan's outgoingWebhooks method as a client may
// call it: (integration, description, params). From a client the only
// legitimate call is the card-opened notice, CardSelected, for a card of the
// webhook's own board that the caller sees; it is built here from the STORED
// integration and the card, never from what the caller sent. Anything else is
// HookBleed - refused and recorded - and answers nothing, as WeKan's does.
func (s *Server) methodOutgoingWebhooks(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	var given map[string]any
	if len(args) < 3 || json.Unmarshal(args[0], &given) != nil {
		return nil, &methodError{"Match.Error", "Expected object"}
	}
	description, err := stringArg(args, 1)
	if err != nil {
		return nil, err
	}
	var params map[string]any
	if json.Unmarshal(args[2], &params) != nil || params == nil {
		return nil, &methodError{"Match.Error", "Expected object"}
	}
	ctx := r.Context()
	hook := s.storedIntegration(ctx, given)
	if hook == nil {
		return nil, nil
	}
	b, err := s.boardFor(ctx, docString(hook, "boardId"), u)
	if err != nil || !b.MemberOf(u.ID) { // not a write: WeKan's card-opened notice is any member's (outgoing.js hasMember)
		return nil, nil
	}
	var c Card
	cardID, _ := params["cardId"].(string)
	if description != "CardSelected" || !validID(cardID) ||
		s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(cardID), "boardId": eq(b.ID)}).Decode(&c) != nil {
		s.recordSecurity(r, securityEvent{Key: "ssrf.webhook-forge", Action: "blocked", Source: "outgoingWebhooks", UserID: u.ID,
			Detail: "client tried to send " + jsonQuote(description) + " through webhook " + docString(hook, "_id")})
		return nil, nil
	}
	if m := b.member(u.ID); m != nil && m.AssignedOnly() && !slices.Contains(c.Assignees, u.ID) {
		s.recordSecurity(r, securityEvent{Key: "authz.assigned", Action: "blocked", Source: "outgoingWebhooks:card-opened", UserID: u.ID,
			Detail: "Assigned-only member reported opening a card not assigned to them."})
		return nil, nil
	}
	p := map[string]any{"userId": u.ID, "cardId": c.ID, "boardId": c.BoardID, "listId": c.ListID, "user": u.Username, "url": ""}
	if body := s.webhookBody(ctx, hook, description, p); body != nil {
		go func() { _ = s.deliverWebhook(context.WithoutCancel(ctx), hook, body) }()
	}
	return nil, nil
}

// storedIntegration is WeKan's storedIntegrationOf: the enabled integration
// the caller's object names - by _id, or by its url on its board - read from
// the database, so nothing the caller sent decides where or how it is sent.
func (s *Server) storedIntegration(ctx context.Context, given map[string]any) bson.M {
	filter := bson.M{"enabled": bson.M{"$ne": false}}
	id, _ := given["_id"].(string)
	url, _ := given["url"].(string)
	boardID, _ := given["boardId"].(string)
	switch {
	case validID(id):
		filter["_id"] = eq(id)
	case url != "" && validID(boardID):
		filter["url"], filter["boardId"] = eq(url), eq(boardID)
	default:
		return nil
	}
	var hook bson.M
	if s.db.Collection("integrations").FindOne(ctx, filter).Decode(&hook) != nil {
		return nil
	}
	return hook
}
