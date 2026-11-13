package web

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// GroupLinkBleed (GHSA-v544-rmr5-38gx): a board's organizations, teams and
// domains are written only by WeKan's setBoardOrgs, setBoardTeams and
// setBoardDomains, which let a board admin add only the groups they belong to
// (#5141); a client collection write naming them is refused and recorded, and
// so is one that adds a member past inviteUserToBoard (#6116). WeKan's
// models/lib/boardWriteGuards.js and boardGroupLinks.js, in Go.

var groupLinkFields = []string{"orgs", "teams", "domains"}

// isGroupLinkField is isGroupLinkField: orgs, teams or domains, or a path in one.
func isGroupLinkField(field string) bool {
	for _, f := range groupLinkFields {
		if field == f || strings.HasPrefix(field, f+".") {
			return true
		}
	}
	return false
}

// groupLinksInInsert is groupLinksInInsert: an inserted board that names any.
func groupLinksInInsert(doc map[string]any) bool {
	for _, f := range groupLinkFields {
		if a, ok := doc[f].([]any); ok && len(a) > 0 {
			return true
		}
	}
	return false
}

func isMembersPath(p string) bool { return p == "members" || strings.HasPrefix(p, "members.") }

var memberSafeOps = map[string]bool{"$set": true, "$unset": true, "$pull": true, "$pullAll": true, "$pop": true}
var digits = regexp.MustCompile(`^\d+$`)

// addsBoardMember is addsBoardMember: does the modifier put a user into the
// board's members who is not an active one now - a new entry, another user in
// an existing slot, or a removed member made active again?
func addsBoardMember(current []Member, modifier map[string]any) bool {
	ids, inactive := map[string]bool{}, map[string]bool{}
	for _, m := range current {
		ids[m.UserID] = true
		if !m.IsActive {
			inactive[m.UserID] = true
		}
	}
	member := func(v any) (string, bool, bool) { // userId, isActive true, an object
		m, ok := v.(map[string]any)
		if !ok {
			return "", false, false
		}
		id, _ := m["userId"].(string)
		active, _ := m["isActive"].(bool)
		return id, active, true
	}
	for op, raw := range modifier {
		args, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if op == "$rename" {
			for k, v := range args {
				if to, _ := v.(string); isMembersPath(k) || isMembersPath(to) {
					return true
				}
			}
			continue
		}
		var paths []string
		for k := range args {
			if isMembersPath(k) {
				paths = append(paths, k)
			}
		}
		if len(paths) == 0 {
			continue
		}
		if !memberSafeOps[op] {
			return true
		}
		if op != "$set" {
			continue
		}
		for _, path := range paths {
			value := args[path]
			parts := strings.Split(path, ".")
			if len(parts) == 1 {
				list, ok := value.([]any)
				if !ok {
					return true
				}
				for _, v := range list {
					id, active, ok := member(v)
					if !ok || !ids[id] || (active && inactive[id]) {
						return true
					}
				}
				continue
			}
			if !digits.MatchString(parts[1]) {
				return true
			}
			i := 0
			for _, c := range parts[1] {
				i = i*10 + int(c-'0')
				if i > len(current) {
					break
				}
			}
			if i >= len(current) {
				return true
			}
			existing := current[i].UserID
			if len(parts) == 2 {
				id, active, ok := member(value)
				if !ok || id != existing || (active && inactive[id]) {
					return true
				}
				continue
			}
			if parts[2] == "userId" {
				return true
			}
			if len(parts) == 3 && parts[2] == "isActive" && value == true && inactive[existing] {
				return true
			}
		}
	}
	return false
}

// forbiddenNewGroupIDs is forbiddenNewGroupIds: the groups next adds that
// previous did not have and the user does not belong to; none for a site admin.
func forbiddenNewGroupIDs(previous, next []string, own []string, siteAdmin bool) []string {
	if siteAdmin {
		return nil
	}
	var out []string
	for _, id := range next {
		if id != "" && !slices.Contains(previous, id) && !slices.Contains(own, id) && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// memberRoleFlags are what WeKan's setMemberPermission writes on a member,
// and all a client update of one member may change here.
var memberRoleFlags = map[string]bool{"isAdmin": true, "isNoComments": true, "isCommentOnly": true, "isWorker": true,
	"isNormalAssignedOnly": true, "isCommentAssignedOnly": true, "isReadOnly": true, "isReadAssignedOnly": true}

// methodBoardsUpdate is the boards collection's own /boards/update as WeKan's
// allow and deny rules let it through (server/permissions/boards.js): the
// deny rules first - a group link or a member added is GroupLinkBleed, a
// background of another board BackgroundBleed - each recorded; then the
// board admin's (or site admin's) allow rule, for the one write WeKan's
// client makes on a board's people, a member's role flags, and for Board
// Settings / Card's own fields (cardSettingsBoardWrite). Anything else is
// the numeric 403 "Access denied" an unhandled write gets.
func (s *Server) methodBoardsUpdate(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	denied := &methodError{"403", "Access denied"}
	refuse := func() (any, error) {
		s.noteDeniedCollectionWrite(r, u, "/boards/update", args)
		return nil, denied
	}
	if len(args) < 2 {
		return refuse()
	}
	var id string
	if json.Unmarshal(args[0], &id) != nil {
		var sel map[string]any
		if json.Unmarshal(args[0], &sel) != nil || len(sel) != 1 {
			return refuse()
		}
		id, _ = sel["_id"].(string)
	}
	var mod map[string]any
	if !validID(id) || json.Unmarshal(args[1], &mod) != nil || len(mod) != 1 {
		return refuse()
	}
	set, ok := mod["$set"].(map[string]any)
	if !ok || len(set) == 0 {
		return refuse()
	}
	ctx := r.Context()
	var b Board
	if s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&b) != nil {
		return 0, nil // as Meteor: no such board is no document updated
	}
	if addsBoardMember(b.Members, mod) {
		return refuse()
	}
	// Board Settings / Card: the board's own Card Settings fields
	// (card_settings.go), a board admin's as the rest.
	// Board Settings / Board View's fields, checked as the REST API checks
	// them (boardViewRequest), a board admin's.
	if input, ok := boardViewWrite(set); ok {
		if !u.IsAdmin && !b.AdminOf(u.ID) {
			return nil, denied
		}
		var doc bson.M
		_ = s.db.Collection("boards").FindOne(ctx, bson.M{"_id": b.ID}).Decode(&doc)
		clean, problem := boardViewRequest(boardViewStateOf(doc), input)
		if problem != "" || len(clean) == 0 {
			return nil, &methodError{"invalid-board-view-settings", problem}
		}
		if _, err := s.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$set": clean}); err != nil {
			return nil, err
		}
		s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
		return 1, nil
	}
	if clean, ok := cardSettingsBoardWrite(set); ok {
		if !u.IsAdmin && !b.AdminOf(u.ID) {
			return nil, denied
		}
		if s.subtaskDepositRefused(r, u, &b, clean) {
			return nil, denied
		}
		clean["modifiedAt"] = time.Now().UTC()
		if _, err := s.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$set": clean}); err != nil {
			return nil, err
		}
		s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
		return 1, nil
	}
	members := slices.Clone(b.Members)
	for path, v := range set {
		parts := strings.Split(path, ".")
		on, isBool := v.(bool)
		if len(parts) != 3 || parts[0] != "members" || !digits.MatchString(parts[1]) || !memberRoleFlags[parts[2]] || !isBool {
			return refuse()
		}
		i := 0
		for _, c := range parts[1] {
			if i = i*10 + int(c-'0'); i >= len(members) {
				return refuse()
			}
		}
		setMemberFlag(&members[i], parts[2], on)
	}
	if !u.IsAdmin && !b.AdminOf(u.ID) {
		return nil, denied
	}
	admins := 0
	for _, m := range members {
		if m.IsActive && m.IsAdmin {
			admins++
		}
	}
	if admins == 0 {
		return nil, denied // WeKan's: never the board's last active admin
	}
	if _, err := s.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"members": members, "modifiedAt": time.Now().UTC()}}); err != nil {
		return nil, err
	}
	s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	return 1, nil
}

func setMemberFlag(m *Member, flag string, on bool) {
	switch flag {
	case "isAdmin":
		m.IsAdmin = on
	case "isNoComments":
		m.IsNoComments = on
	case "isCommentOnly":
		m.IsCommentOnly = on
	case "isWorker":
		m.IsWorker = on
	case "isNormalAssignedOnly":
		m.IsNormalAssignedOnly = on
	case "isCommentAssignedOnly":
		m.IsCommentAssignedOnly = on
	case "isReadOnly":
		m.IsReadOnly = on
	case "isReadAssignedOnly":
		m.IsReadAssignedOnly = on
	}
}

type boardTeam struct {
	TeamID          string `json:"teamId" bson:"teamId"`
	TeamDisplayName string `json:"teamDisplayName" bson:"teamDisplayName"`
	IsActive        bool   `json:"isActive" bson:"isActive"`
}

// methodSetBoardTeams is WeKan's setBoardTeams(teams, members, boardId): a
// board admin sets the board's teams - only teams they belong to may be added
// (#5141), any for a site admin - and the members the teams bring, reconciled
// against the board's own members (#5730): an active admin is never dropped,
// a member the caller still lists stays, one they omit leaves (cleaned up and
// recorded as a removal), and a new one comes in with the active teams it
// came through (#4178). Unlike WeKan's, a new member must belong to one of
// those teams: anybody else would come in past inviteUserToBoard (#6116).
func (s *Server) methodSetBoardTeams(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	match := &methodError{"Match.Error", "Match failed"}
	if len(args) < 3 {
		return nil, match
	}
	var teams []boardTeam
	var rawTeams, rawMembers []map[string]any
	if json.Unmarshal(args[0], &rawTeams) != nil || json.Unmarshal(args[1], &rawMembers) != nil {
		return nil, match
	}
	for _, t := range rawTeams {
		id, ok1 := t["teamId"].(string)
		name, ok2 := t["teamDisplayName"].(string)
		active, ok3 := t["isActive"].(bool)
		if !ok1 || !ok2 || !ok3 {
			return nil, match
		}
		teams = append(teams, boardTeam{id, name, active})
	}
	var listed []Member
	for _, m := range rawMembers {
		id, ok := m["userId"].(string)
		_, okA := m["isAdmin"].(bool)
		_, okB := m["isActive"].(bool)
		if !ok || !okA || !okB {
			return nil, match
		}
		for k := range memberRoleFlags {
			if v, has := m[k]; has {
				if _, isBool := v.(bool); !isBool {
					return nil, match
				}
			}
		}
		flag := func(k string) bool { v, _ := m[k].(bool); return v }
		active := true
		if v, has := m["isActive"].(bool); has {
			active = v
		}
		listed = append(listed, Member{UserID: id, IsAdmin: flag("isAdmin"), IsActive: active, IsNoComments: flag("isNoComments"),
			IsCommentOnly: flag("isCommentOnly"), IsWorker: flag("isWorker"), IsNormalAssignedOnly: flag("isNormalAssignedOnly"),
			IsCommentAssignedOnly: flag("isCommentAssignedOnly"), IsReadOnly: flag("isReadOnly"), IsReadAssignedOnly: flag("isReadAssignedOnly")})
	}
	boardID, err := stringArg(args, 2)
	if err != nil {
		return nil, err
	}
	ctx := r.Context()
	var b Board
	var raw struct {
		Teams []boardTeam `bson:"teams"`
	}
	if !validID(boardID) || s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(boardID)}).Decode(&b) != nil {
		return nil, &methodError{"board-not-found", "Board not found."}
	}
	_ = s.db.Collection("boards").FindOne(ctx, bson.M{"_id": b.ID}).Decode(&raw)
	if !b.AdminOf(u.ID) {
		return nil, &methodError{"not-authorized", "You must be a board admin to perform this action."}
	}
	var previous, next []string
	for _, t := range raw.Teams {
		previous = append(previous, t.TeamID)
	}
	for _, t := range teams {
		next = append(next, t.TeamID)
	}
	if len(forbiddenNewGroupIDs(previous, next, s.userTeamIDs(ctx, u.ID), u.IsAdmin)) > 0 {
		return nil, &methodError{"not-authorized", "You can add only teams you belong to."}
	}
	var activeTeams []string
	for _, t := range teams {
		if t.IsActive {
			activeTeams = append(activeTeams, t.TeamID)
		}
	}
	members, removed, err := s.reconcileBoardTeamMembers(ctx, b.Members, listed, activeTeams)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$set": bson.M{
		"members": members, "teams": teams, "modifiedAt": time.Now().UTC()}}); err != nil {
		return nil, err
	}
	// The same cleanup and audit trail a removal has, as WeKan's: the card's
	// members and watchers, the lists' watchers, a private board's star.
	for _, id := range removed {
		s.teamLeaveCleanup(ctx, u.ID, &b, id)
		s.activity(ctx, u.ID, "removeBoardMember", bson.M{"boardId": b.ID, "memberId": id, "type": "member"})
	}
	s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	return nil, nil
}

// userTeamIDs are the teams the user's document lists (orgTeamIDs').
func (s *Server) userTeamIDs(ctx context.Context, userID string) []string {
	_, teams := s.orgTeamIDs(ctx, userID)
	out := make([]string, 0, len(teams))
	for id := range teams {
		out = append(out, id)
	}
	return out
}

// reconcileBoardTeamMembers is WeKan's, against the stored members: an
// existing member the client lists, or an active admin, stays; any other
// leaves; a new one - who belongs to one of the active teams - comes in with
// those teams as its grantedBy.
func (s *Server) reconcileBoardTeamMembers(ctx context.Context, stored, listed []Member, activeTeams []string) ([]Member, []string, error) {
	byID := map[string]bool{}
	for _, m := range listed {
		byID[m.UserID] = true
	}
	known := map[string]bool{}
	var out []Member
	var removed []string
	for _, m := range stored {
		known[m.UserID] = true
		if byID[m.UserID] || (m.IsActive && m.IsAdmin) {
			out = append(out, m)
		} else {
			removed = append(removed, m.UserID)
		}
	}
	for _, m := range listed {
		if known[m.UserID] || !validID(m.UserID) {
			continue
		}
		var grants bson.A
		for _, t := range s.userTeamIDs(ctx, m.UserID) {
			if slices.Contains(activeTeams, t) {
				grants = append(grants, "team:"+t)
			}
		}
		if len(grants) == 0 {
			continue // not in one of the board's teams: inviteUserToBoard's to decide
		}
		m.Rest = map[string]any{"grantedBy": grants}
		known[m.UserID] = true
		out = append(out, m)
	}
	return out, removed, nil
}

// teamLeaveCleanup is setBoardTeams' cleanup of a member the teams no longer
// bring: off the cards' members and watchers (each card recorded in the change
// history by the admin), the lists' watchers, and a private board's star.
func (s *Server) teamLeaveCleanup(ctx context.Context, actorID string, b *Board, id string) {
	cur, err := s.db.Collection("cards").Find(ctx, bson.M{"boardId": b.ID, "$or": bson.A{bson.M{"members": eq(id)}, bson.M{"watchers": eq(id)}}})
	if err == nil {
		var cards []bson.M
		_ = cur.All(ctx, &cards)
		for _, c := range cards {
			_, _ = s.cardsUpdateOne(ctx, actorID, bson.M{"_id": c["_id"]}, bson.M{"$pull": bson.M{"members": id, "watchers": id}})
		}
	}
	_, _ = s.db.Collection("lists").UpdateMany(ctx, bson.M{"boardId": b.ID}, bson.M{"$pull": bson.M{"watchers": id}})
	if b.Permission != "public" && b.Permission != "instance" {
		_, _ = s.db.Collection("users").UpdateOne(ctx, bson.M{"_id": eq(id)}, bson.M{"$pull": bson.M{"profile.starredBoards": b.ID}})
	}
}

type boardOrg struct {
	OrgID          string `json:"orgId" bson:"orgId"`
	OrgDisplayName string `json:"orgDisplayName" bson:"orgDisplayName"`
	IsActive       bool   `json:"isActive" bson:"isActive"`
}

// boardAdminTarget is the board-admin check WeKan's three board group methods
// begin with.
func (s *Server) boardAdminTarget(r *http.Request, u *User, boardID string) (*Board, error) {
	var b Board
	if !validID(boardID) || s.db.Collection("boards").FindOne(r.Context(), bson.M{"_id": eq(boardID)}).Decode(&b) != nil {
		return nil, &methodError{"board-not-found", "Board not found."}
	}
	if !b.AdminOf(u.ID) {
		return nil, &methodError{"not-authorized", "You must be a board admin to perform this action."}
	}
	return &b, nil
}

// methodSetBoardOrgs is WeKan's setBoardOrgs(orgs, boardId): a board admin
// sets the board's organizations, adding only ones they belong to (#5141),
// any for a site admin.
func (s *Server) methodSetBoardOrgs(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	match := &methodError{"Match.Error", "Match failed"}
	var raw []map[string]any
	if len(args) < 2 || json.Unmarshal(args[0], &raw) != nil {
		return nil, match
	}
	var orgs []boardOrg
	for _, o := range raw {
		id, ok1 := o["orgId"].(string)
		name, ok2 := o["orgDisplayName"].(string)
		active, ok3 := o["isActive"].(bool)
		if !ok1 || !ok2 || !ok3 {
			return nil, match
		}
		orgs = append(orgs, boardOrg{id, name, active})
	}
	boardID, err := stringArg(args, 1)
	if err != nil {
		return nil, err
	}
	b, err := s.boardAdminTarget(r, u, boardID)
	if err != nil {
		return nil, err
	}
	ctx := r.Context()
	var stored struct {
		Orgs []boardOrg `bson:"orgs"`
	}
	_ = s.db.Collection("boards").FindOne(ctx, bson.M{"_id": b.ID}).Decode(&stored)
	var previous, next, own []string
	for _, o := range stored.Orgs {
		previous = append(previous, o.OrgID)
	}
	for _, o := range orgs {
		next = append(next, o.OrgID)
	}
	mine, _ := s.orgTeamIDs(ctx, u.ID)
	for id := range mine {
		own = append(own, id)
	}
	if len(forbiddenNewGroupIDs(previous, next, own, u.IsAdmin)) > 0 {
		return nil, &methodError{"not-authorized", "You can add only organizations you belong to."}
	}
	if orgs == nil {
		orgs = []boardOrg{}
	}
	if _, err := s.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"orgs": orgs, "modifiedAt": time.Now().UTC()}}); err != nil {
		return nil, err
	}
	s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	return nil, nil
}

// normalizeBoardDomains is setBoardDomains' validation (#5850): each domain
// trimmed and lowercased, with a "." and no "@" or whitespace, once.
func normalizeBoardDomains(raw []map[string]any) ([]boardDomain, error) {
	out := []boardDomain{}
	seen := map[string]bool{}
	for _, e := range raw {
		d, ok1 := e["domain"].(string)
		active, ok2 := e["isActive"].(bool)
		if !ok1 || !ok2 {
			return nil, &methodError{"Match.Error", "Match failed"}
		}
		domain := strings.ToLower(strings.TrimSpace(d))
		if domainProblem(domain) {
			return nil, &methodError{"invalid-domain", "Invalid domain: " + d}
		}
		if seen[domain] {
			continue
		}
		seen[domain] = true
		out = append(out, boardDomain{Domain: domain, IsActive: active})
	}
	return out, nil
}

func (s *Server) setBoardDomains(r *http.Request, u *User, raw []map[string]any, boardID string) (any, error) {
	b, err := s.boardAdminTarget(r, u, boardID)
	if err != nil {
		return nil, err
	}
	domains, err := normalizeBoardDomains(raw)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.Collection("boards").UpdateOne(r.Context(), bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"domains": domains, "modifiedAt": time.Now().UTC()}}); err != nil {
		return nil, err
	}
	s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	return nil, nil
}

// methodSetBoardDomains is WeKan's setBoardDomains(domains, boardId).
func (s *Server) methodSetBoardDomains(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	var raw []map[string]any
	if len(args) < 2 || json.Unmarshal(args[0], &raw) != nil {
		return nil, &methodError{"Match.Error", "Match failed"}
	}
	boardID, err := stringArg(args, 1)
	if err != nil {
		return nil, err
	}
	return s.setBoardDomains(r, u, raw, boardID)
}

// boardDomainsRaw is the board's stored domains as setBoardDomains takes them.
func (s *Server) boardDomainsRaw(r *http.Request, boardID string) []map[string]any {
	out := []map[string]any{}
	if validID(boardID) {
		for _, d := range s.domainsOf(r, boardID) {
			out = append(out, map[string]any{"domain": d.Domain, "isActive": d.IsActive})
		}
	}
	return out
}

// methodAddBoardDomain is WeKan's addBoardDomain(domain, boardId): the
// board's domains and this one, active, through setBoardDomains' checks.
func (s *Server) methodAddBoardDomain(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	domain, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	boardID, err := stringArg(args, 1)
	if err != nil {
		return nil, err
	}
	raw := s.boardDomainsRaw(r, boardID)
	normalized := strings.ToLower(strings.TrimSpace(domain))
	if !slices.ContainsFunc(raw, func(d map[string]any) bool { return d["domain"] == normalized }) {
		raw = append(raw, map[string]any{"domain": normalized, "isActive": true})
	}
	return s.setBoardDomains(r, u, raw, boardID)
}

// methodRemoveBoardDomain is WeKan's removeBoardDomain(domain, boardId).
func (s *Server) methodRemoveBoardDomain(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	domain, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	boardID, err := stringArg(args, 1)
	if err != nil {
		return nil, err
	}
	normalized := strings.ToLower(strings.TrimSpace(domain))
	raw := slices.DeleteFunc(s.boardDomainsRaw(r, boardID), func(d map[string]any) bool { return d["domain"] == normalized })
	return s.setBoardDomains(r, u, raw, boardID)
}
