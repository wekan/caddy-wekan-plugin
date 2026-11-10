package web

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Per-list exceptions (#4527), as Node.js WeKan's models/lib/listPermissions.js
// and server/lib/listPermissionGate.js: a board admin can give a list its own
// rule for a role - "Normal members may read this list but not write in it" -
// stored as list.roleExceptions {role: {write, comment}}. Weddy shares WeKan's
// database, so a list locked in WeKan is locked here too: every card write
// asks the card's list, and a move or a new card asks the list it goes to.
//
// A list that REFUSES a role is applied everywhere - the pages, the methods
// and the REST API - and a refused write is recorded in Admin Panel / Problems
// as ListLockBleed (authz.list-locked): the pages never offer it, so reaching
// the server is an attempt. A list that GRANTS a role more than the board
// gives opens what WeKan's allow rules open (server/listPermissions.js):
// adding a card to it and commenting on its cards, from the pages and the
// methods - canAddCardIn and canCommentIn. As WeKan's assertListNotRefused
// says, a grant never opens the REST API to a role the board does not let
// write (TestListGrantsNeverReachREST).

// listRoleIn is WeKan's roleIn: the user's role on the board - "signed-in"
// on a public or instance board they are not a member of - or "" for none.
func listRoleIn(b *Board, userID string) string {
	if b == nil || userID == "" {
		return ""
	}
	for _, m := range b.Members {
		if m.UserID == userID {
			return m.Role() // "" when removed: no role at all
		}
	}
	if b.Permission == "public" || b.Permission == "instance" {
		return "signed-in"
	}
	return ""
}

// listException is WeKan's exceptionFor: the list's rule for this user's role
// and capability - a pointer to true or false - or nil when the board decides.
func (s *Server) listException(ctx context.Context, b *Board, listID, userID, capability string) *bool {
	if b == nil || !validID(listID) {
		return nil
	}
	role := listRoleIn(b, userID)
	if role == "" || role == "board-admin" {
		return nil
	}
	var l struct {
		BoardID        string                    `bson:"boardId"`
		RoleExceptions map[string]map[string]any `bson:"roleExceptions"`
	}
	// A list on another board has no say here.
	if s.db.Collection("lists").FindOne(ctx, bson.M{"_id": eq(listID)}).Decode(&l) != nil || l.BoardID != b.ID {
		return nil
	}
	if capability == "moveCard" {
		capability = "write"
	}
	if v, ok := l.RoleExceptions[role][capability].(bool); ok {
		return &v
	}
	return nil
}

// listRefuses: the list says no to this user's role.
func (s *Server) listRefuses(ctx context.Context, b *Board, listID, userID, capability string) bool {
	e := s.listException(ctx, b, listID, userID, capability)
	return e != nil && !*e
}

// refuseInList records a refusal the list made (ListLockBleed) and answers
// true when it did.
func (s *Server) refuseInList(ctx context.Context, b *Board, listID string, u *User, capability, source string) bool {
	if u == nil || !s.listRefuses(ctx, b, listID, u.ID, capability) {
		return false
	}
	func() {
		defer func() { _ = recover() }() // logging must never break the guard
		s.recordSecurityCtx(ctx, securityEvent{Key: "authz.list-locked", Action: "blocked", Source: source, UserID: u.ID,
			Detail: "A " + capability + " in a list whose exception refuses the caller's role."})
	}()
	return true
}

// WritableIn is Writable for a card, asking its list too: the board's rule,
// then the list's refusal. Every card write goes through it
// (TestEveryCardWriteAsksTheList).
func (b *Board) WritableIn(ctx context.Context, s *Server, u *User, capability string, c *Card) bool {
	if !b.Writable(u, capability, c) {
		return false
	}
	return c == nil || !s.refuseInList(ctx, b, c.ListID, u, capability, "list-locked:"+capability)
}

// CommentableIn: may u comment on c - the board's rule (an assigned-only
// member only on a card assigned to them), then the card's list.
func (b *Board) CommentableIn(ctx context.Context, s *Server, u *User, c *Card) bool {
	if u == nil || c == nil {
		return false
	}
	if !u.IsAdmin {
		m := b.member(u.ID)
		if !b.Can(u.ID, "comment") || m == nil || (m.AssignedOnly() && !containsString(c.Assignees, u.ID)) {
			return false
		}
	}
	return !s.refuseInList(ctx, b, c.ListID, u, "comment", "list-locked:comment")
}

func containsString(list []string, v string) bool {
	for _, e := range list {
		if e == v {
			return true
		}
	}
	return false
}

// listOfCard is the list a card is in, "" when there is none.
func (s *Server) listOfCard(ctx context.Context, cardID string) string {
	var c struct {
		ListID string `bson:"listId"`
	}
	if validID(cardID) {
		_ = s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(cardID)}).Decode(&c)
	}
	return c.ListID
}

// canAddCardIn is WeKan's canWriteInList for a new card: the list's rule for
// the user's role when it has one, else the board's. The pages' card
// composer and its form only - never REST.
func (s *Server) canAddCardIn(ctx context.Context, b *Board, listID string, u *User) bool {
	if u == nil {
		return false
	}
	if e := s.listException(ctx, b, listID, u.ID, "write"); e != nil {
		return *e
	}
	return b.Writable(u, "write", nil)
}

// canCommentIn is WeKan's canCommentInList for a card: a list that grants
// its comments to the user's role lets them comment there whatever the board
// says - an assigned-only member still only on a card assigned to them -
// else CommentableIn. The pages and the methods only - never REST.
func (s *Server) canCommentIn(ctx context.Context, b *Board, u *User, c *Card) bool {
	if u == nil || c == nil {
		return false
	}
	if e := s.listException(ctx, b, c.ListID, u.ID, "comment"); e != nil && *e {
		m := b.member(u.ID)
		return u.IsAdmin || m == nil || !m.AssignedOnly() || containsString(c.Assignees, u.ID)
	}
	return b.CommentableIn(ctx, s, u, c)
}
