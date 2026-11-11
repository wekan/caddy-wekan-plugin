package web

// Two of Meteor's client collection writes that WeKan's pages and specs send
// with their allow rules, as Weddy answers them: /integrations/update
// (server/permissions/integrations.js) and /customFields/update
// (server/permissions/customFields.js). Each takes a $set of the fields a
// page edits and nothing else - another operator or field is denied as
// Meteor denies a write no allow rule accepts - and a write that moves the
// document to another board is RepointBleed (security_attempts.go), refused
// and recorded. Each records a move FIRST, before deciding: neither accepts
// boardId or boardIds at all, so a move it records is always one it refuses.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

var accessDenied = &methodError{"403", "Access denied"}

// selectorID is a collection write's selector: an id, or {_id: id}.
func selectorID(args []json.RawMessage) string {
	if len(args) == 0 {
		return ""
	}
	var sel struct {
		ID string `json:"_id"`
	}
	if json.Unmarshal(args[0], &sel) != nil {
		_ = json.Unmarshal(args[0], &sel.ID)
	}
	if !validID(sel.ID) {
		return ""
	}
	return sel.ID
}

// setOnly is the modifier's $set, when $set is all it has.
func setOnly(args []json.RawMessage) (map[string]any, bool) {
	var mod map[string]any
	if len(args) < 2 || json.Unmarshal(args[1], &mod) != nil || len(mod) != 1 {
		return nil, false
	}
	set, ok := mod["$set"].(map[string]any)
	return set, ok && len(set) > 0
}

// methodUpdateIntegration: a board admin's - or a site admin's - edit of a
// webhook of their board: its title, address (checked as a new one is),
// whether it is on, its activities, token, type and Mattermost direct
// messages. A bot keeps listening to the dispatch only.
func (s *Server) methodUpdateIntegration(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	s.noteDeniedCollectionWrite(r, u, "/integrations/update", args)
	ctx := r.Context()
	id := selectorID(args)
	var hook bson.M
	if id == "" || s.db.Collection("integrations").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&hook) != nil {
		return nil, accessDenied
	}
	boardID := docString(hook, "boardId")
	var b Board
	if !u.IsAdmin && (s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(boardID)}).Decode(&b) != nil || !b.AdminOf(u.ID)) {
		return nil, accessDenied
	}
	set, ok := setOnly(args)
	if !ok {
		return nil, accessDenied
	}
	out := bson.M{}
	for k, v := range set {
		switch k {
		case "title", "token":
			str, isStr := v.(string)
			if !isStr || len(str) > 1000 {
				return nil, accessDenied
			}
			out[k] = str
		case "url":
			str, _ := v.(string)
			if why := validateOutboundURL(ctx, str); why != "" {
				return nil, &methodError{"invalid-webhook-url", why}
			}
			out[k] = str
		case "enabled", "mattermostDirectMessages":
			on, isBool := v.(bool)
			if !isBool {
				return nil, accessDenied
			}
			out[k] = on
		case "type":
			if v != "outgoing-webhooks" && v != "bidirectional-webhooks" {
				return nil, accessDenied
			}
			out[k] = v
		case "activities":
			list, isList := v.([]any)
			if !isList || hook["bot"] == true {
				return nil, accessDenied
			}
			acts := bson.A{}
			for _, a := range list {
				str, isStr := a.(string)
				if !isStr || len(str) > 100 {
					return nil, accessDenied
				}
				acts = append(acts, str)
			}
			out[k] = acts
		default:
			return nil, accessDenied
		}
	}
	out["modifiedAt"] = time.Now().UTC()
	res, err := s.db.Collection("integrations").UpdateOne(ctx, bson.M{"_id": eq(id), "boardId": eq(boardID)}, bson.M{"$set": out})
	if err != nil {
		return nil, err
	}
	s.hub.publishFrom(boardID, "board-changed", r.Header.Get("X-Weddy-Page"))
	return res.ModifiedCount, nil
}

// methodUpdateCustomField: an edit of a custom field's definition by
// somebody who may write to one of its boards - its name, type, settings and
// where it shows. An admin-only field, or one being made admin-only, needs a
// board admin of every board it is on; anyone else is AdminFieldBleed.
func (s *Server) methodUpdateCustomField(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	s.noteDeniedCollectionWrite(r, u, "/customFields/update", args)
	ctx := r.Context()
	id := selectorID(args)
	var f bson.M
	if id == "" || s.db.Collection("customFields").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&f) != nil {
		return nil, accessDenied
	}
	boardIDs := stringsOf(f["boardIds"])
	set, ok := setOnly(args)
	if !ok || !s.anyBoardWritable(ctx, u, boardIDs) {
		return nil, accessDenied
	}
	out := bson.M{}
	for k, v := range set {
		switch {
		case k == "name":
			name, _ := v.(string)
			if name = strings.TrimSpace(name); name == "" || len(name) > 200 {
				return nil, accessDenied
			}
			out[k] = name
		case k == "type":
			typ, _ := v.(string)
			if !customFieldTypes[typ] {
				return nil, accessDenied
			}
			out[k] = typ
		case k == "settings":
			settings, isMap := v.(map[string]any)
			if !isMap {
				return nil, accessDenied
			}
			out[k] = cleanImported(settings, "")
		case k == "adminOnly" || isCustomFieldFlag(k):
			on, isBool := v.(bool)
			if !isBool {
				return nil, accessDenied
			}
			out[k] = on
		default:
			return nil, accessDenied
		}
	}
	if f["adminOnly"] == true || out["adminOnly"] == true {
		for _, bid := range boardIDs {
			var b Board
			if !u.IsAdmin && (s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(bid)}).Decode(&b) != nil || !b.AdminOf(u.ID)) {
				s.recordSecurity(r, securityEvent{Key: "authz.admin-only-field", Action: "blocked", Source: "ddp:customFields.definition", UserID: u.ID,
					Detail: "Tried to change an admin-only custom field without being an admin of its boards."})
				return nil, accessDenied
			}
		}
	}
	res, err := s.db.Collection("customFields").UpdateOne(ctx, bson.M{"_id": eq(id)}, bson.M{"$set": out})
	if err != nil {
		return nil, err
	}
	for _, bid := range boardIDs {
		s.hub.publishFrom(bid, "board-changed", r.Header.Get("X-Weddy-Page"))
	}
	return res.ModifiedCount, nil
}

func isCustomFieldFlag(k string) bool {
	for _, f := range customFieldFlags {
		if f == k {
			return true
		}
	}
	return false
}

// anyBoardWritable is WeKan's allowIsAnyBoardMemberWithWriteAccess.
func (s *Server) anyBoardWritable(ctx context.Context, u *User, ids []string) bool {
	for _, id := range ids {
		var b Board
		if validID(id) && s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&b) == nil && b.Writable(u, "write", nil) {
			return true
		}
	}
	return false
}
