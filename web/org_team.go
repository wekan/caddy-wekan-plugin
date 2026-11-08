package web

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Organizations and teams' feature flags in Admin Panel -> People, as Node.js
// WeKan's server/models/org.js and team.js: a site admin sets an org's or a
// team's shared templates, member propagation and sync from the sign-in
// source, one at a time or all at once. A per-record call by anybody else is
// a silent no-op, as WeKan's; an all-at-once call is refused. The record is
// named by {_id: <string>} only - never by whatever selector a client sends.
//
// Turning "Propagate Members To Boards" on (#4737, #6559) adds the group's
// members to the regular boards that list it, add-only, template boards
// never, each membership recording the group it came through (grantedBy,
// #4178).

var groupFeatures = map[string][]string{
	"org":  {"orgSharedTemplates", "orgPropagateMembersToBoards", "orgSyncMembersFromAuth"},
	"team": {"teamSharedTemplates", "teamPropagateMembersToBoards", "teamSyncMembersFromAuth"},
}

func (s *Server) orgTeamMethods() map[string]methodFunc {
	m := map[string]methodFunc{}
	for kind, fields := range groupFeatures {
		for _, f := range fields {
			m["set"+upperFirst(f)] = s.groupFeatureMethod(kind, f)
		}
		all := map[string]string{"org": "setAllOrgsFeature", "team": "setAllTeamsFeature"}[kind]
		m[all] = s.allGroupsFeatureMethod(kind)
	}
	return m
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return string(s[0]-'a'+'A') + s[1:]
}

// groupSelector is the record a method names: {_id: <id>} and nothing else.
func groupSelector(raw json.RawMessage) (string, bool) {
	var sel map[string]any
	if json.Unmarshal(raw, &sel) != nil || len(sel) != 1 {
		return "", false
	}
	id, ok := sel["_id"].(string)
	return id, ok && validID(id)
}

func (s *Server) groupFeatureMethod(kind, field string) methodFunc {
	return func(r *http.Request, u *User, args []json.RawMessage) (any, error) {
		var value bool
		if len(args) < 2 || json.Unmarshal(args[1], &value) != nil {
			return nil, &methodError{"Match.Error", "Expected Boolean"}
		}
		var sel map[string]any
		if json.Unmarshal(args[0], &sel) != nil {
			return nil, &methodError{"Match.Error", "Expected Object"}
		}
		if !u.IsAdmin {
			return nil, nil // WeKan's: a silent no-op
		}
		id, ok := groupSelector(args[0])
		if !ok {
			return nil, &methodError{"Match.Error", "Expected {_id: String}"}
		}
		ctx := r.Context()
		if _, err := s.db.Collection(kind).UpdateOne(ctx, bson.M{"_id": eq(id)}, bson.M{"$set": bson.M{field: value}}); err != nil {
			return nil, err
		}
		if value && field == kind+"PropagateMembersToBoards" {
			s.propagateGroupMembers(ctx, kind, id)
		}
		return nil, nil
	}
}

func (s *Server) allGroupsFeatureMethod(kind string) methodFunc {
	return func(r *http.Request, u *User, args []json.RawMessage) (any, error) {
		field, err := stringArg(args, 0)
		if err != nil {
			return nil, err
		}
		var value bool
		if len(args) < 2 || json.Unmarshal(args[1], &value) != nil {
			return nil, &methodError{"Match.Error", "Expected Boolean"}
		}
		if !u.IsAdmin {
			return nil, &methodError{"not-authorized", "not-authorized"}
		}
		if !slices.Contains(groupFeatures[kind], field) {
			return nil, &methodError{"invalid-field", "invalid-field"}
		}
		ctx := r.Context()
		if _, err := s.db.Collection(kind).UpdateMany(ctx, bson.M{}, bson.M{"$set": bson.M{field: value}}); err != nil {
			return nil, err
		}
		// Orgs only for the org column, teams only for the team column.
		if value && field == kind+"PropagateMembersToBoards" {
			cur, err := s.db.Collection(kind).Find(ctx, bson.M{field: true})
			if err == nil {
				var groups []struct {
					ID string `bson:"_id"`
				}
				_ = cur.All(ctx, &groups)
				for _, g := range groups {
					s.propagateGroupMembers(ctx, kind, g.ID)
				}
			}
		}
		return nil, nil
	}
}

// propagateGroupMembers is WeKan's propagateGroupMembersToBoards: the users
// whose orgs (teams) name the group join the regular, unarchived boards that
// list it, or - already there through another group, or lapsed - gain this
// grant; a direct member is left as they are. Add-only.
func (s *Server) propagateGroupMembers(ctx context.Context, kind, groupID string) (boards, added int) {
	field, idField := "orgs", "orgId"
	if kind == "team" {
		field, idField = "teams", "teamId"
	}
	grant := kind + ":" + groupID
	cur, err := s.db.Collection("users").Find(ctx, bson.M{field + "." + idField: eq(groupID)})
	if err != nil {
		return
	}
	var users []struct {
		ID string `bson:"_id"`
	}
	_ = cur.All(ctx, &users)
	if len(users) == 0 {
		return
	}
	bc, err := s.db.Collection("boards").Find(ctx, bson.M{"type": "board", "archived": bson.M{"$ne": true}, field + "." + idField: eq(groupID)})
	if err != nil {
		return
	}
	var raws []bson.M
	_ = bc.All(ctx, &raws)
	for _, raw := range raws {
		// The group must be on the board, and active there.
		listed := false
		if gs, ok := raw[field].(bson.A); ok {
			for _, g := range gs {
				if nestedField(g, idField) == groupID && nestedField(g, "isActive") == true {
					listed = true
				}
			}
		}
		if !listed {
			continue
		}
		var b Board
		doc, _ := bson.Marshal(raw)
		if bson.Unmarshal(doc, &b) != nil {
			continue
		}
		changed, n := false, 0
		for _, u := range users {
			i := slices.IndexFunc(b.Members, func(m Member) bool { return m.UserID == u.ID })
			if i < 0 {
				b.Members = append(b.Members, Member{UserID: u.ID, IsActive: true, Rest: map[string]any{"grantedBy": bson.A{grant}}})
				changed, n = true, n+1
				continue
			}
			g, derived := b.Members[i].GrantedBy()
			if !derived || (b.Members[i].IsActive && slices.Contains(g, grant)) {
				continue // direct, or already through this group
			}
			if !slices.Contains(g, grant) {
				g = append(g, grant)
			}
			a := bson.A{}
			for _, x := range g {
				a = append(a, x)
			}
			if b.Members[i].Rest == nil {
				b.Members[i].Rest = map[string]any{}
			}
			b.Members[i].IsActive, b.Members[i].Rest["grantedBy"] = true, a
			changed = true
		}
		if !changed {
			continue
		}
		// The whole list: FerretDB has no positional update.
		if _, err := s.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": eq(b.ID)}, bson.M{"$set": bson.M{"members": b.Members}}); err == nil {
			boards, added = boards+1, added+n
			s.hub.publish(b.ID, "board-changed")
		}
	}
	return
}
