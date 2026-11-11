package web

// #6116, WeKan's server/lib/orgTeamRestriction.js: Admin Panel's "board
// members only from the same Organization" and "... the same Team" (and the
// older setting that is both). With either on, somebody who is not a site
// admin adds to a board only a user who shares an enabled kind - an
// Organization, a Team - with them or with another active member of the
// board. Weddy shares WeKan's settings document but never read these, so the
// Members widget and inviteUserToBoard added anybody.

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/bson"
)

type memberRestriction struct{ Org, Team bool }

func (s *Server) boardMemberRestriction(ctx context.Context) memberRestriction {
	var set struct {
		Org    bool `bson:"boardMembersFromSameOrgOnly"`
		Team   bool `bson:"boardMembersFromSameTeamOnly"`
		Legacy bool `bson:"boardMembersFromSameOrgOrTeamOnly"`
	}
	_ = s.db.Collection("settings").FindOne(ctx, bson.M{}).Decode(&set)
	return memberRestriction{Org: set.Org || set.Legacy, Team: set.Team || set.Legacy}
}

// orgTeamIDs are a user's Organization and Team ids, as WeKan keeps them.
func (s *Server) orgTeamIDs(ctx context.Context, userID string) (orgs, teams map[string]bool) {
	var d struct {
		Orgs []struct {
			ID string `bson:"orgId"`
		} `bson:"orgs"`
		Teams []struct {
			ID string `bson:"teamId"`
		} `bson:"teams"`
	}
	_ = s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(userID)}).Decode(&d)
	orgs, teams = map[string]bool{}, map[string]bool{}
	for _, o := range d.Orgs {
		if o.ID != "" {
			orgs[o.ID] = true
		}
	}
	for _, t := range d.Teams {
		if t.ID != "" {
			teams[t.ID] = true
		}
	}
	return orgs, teams
}

func sharesAny(a, b map[string]bool) bool {
	for k := range a {
		if b[k] {
			return true
		}
	}
	return false
}

// mayAddToBoard: may inviter add candidateID to b under the restriction?
func (s *Server) mayAddToBoard(ctx context.Context, inviter *User, b *Board, candidateID string) bool {
	if inviter.IsAdmin {
		return true
	}
	rs := s.boardMemberRestriction(ctx)
	if !rs.Org && !rs.Team {
		return true
	}
	cOrgs, cTeams := s.orgTeamIDs(ctx, candidateID)
	shares := func(userID string) bool {
		orgs, teams := s.orgTeamIDs(ctx, userID)
		return (rs.Org && sharesAny(orgs, cOrgs)) || (rs.Team && sharesAny(teams, cTeams))
	}
	if shares(inviter.ID) {
		return true
	}
	// An inviter with no Organization or Team adds from the board's own.
	for _, m := range b.Members {
		if m.IsActive && m.UserID != inviter.ID && shares(m.UserID) {
			return true
		}
	}
	return false
}
