package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/mail"
	"slices"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Invitations, as Node.js WeKan sends them (server/models/settings.js
// sendInvitation, models/lib/invitationCodeEmail.js): an invitation code by
// e-mail, kept in invitation_codes with the boards the invitee joins on sign-up.

// inviteRolesDefault is WeKan's INVITE_TO_BOARD_ROLES_DEFAULT: board admins and
// normal members may invite, unless Admin Panel says otherwise.
var inviteRolesDefault = []string{"board-admin", "normal"}

func (s *Server) inviteRoles(r *http.Request) []string {
	var doc struct {
		AllowedRoles []string `bson:"allowedRoles"`
	}
	if s.db.Collection("inviteToBoardRolesSettings").FindOne(r.Context(), bson.M{}).Decode(&doc) == nil && doc.AllowedRoles != nil {
		return doc.AllowedRoles
	}
	return inviteRolesDefault
}

// mayInvite is WeKan's: a site admin; anybody else only while registration is
// closed and Admin Panel names the mail domain their address is in.
func (s *Server) mayInvite(r *http.Request, u *User) bool {
	if u.IsAdmin {
		return true
	}
	var set struct {
		DisableRegistration bool   `bson:"disableRegistration"`
		MailDomainName      string `bson:"mailDomainName"`
	}
	_ = s.db.Collection("settings").FindOne(r.Context(), bson.M{}).Decode(&set)
	if !set.DisableRegistration || set.MailDomainName == "" {
		return false
	}
	return slices.ContainsFunc(u.Emails, func(e Email) bool { return strings.HasSuffix(e.Address, set.MailDomainName) })
}

// canInviteToBoard is WeKan's: a site admin, or a member whose role Admin
// Panel lets invite.
func canInviteToBoard(u *User, b *Board, roles []string) bool {
	if u.IsAdmin {
		return true
	}
	m := b.member(u.ID)
	return m != nil && m.Role() != "" && slices.Contains(roles, m.Role())
}

// invitationCode is WeKan's generateInvitationCode: 16 random bytes, base64url.
func invitationCode() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// methodSendInvitation is WeKan's sendInvitation(emails, boards): for whoever
// may invite, to boards they may invite to - every board checked before any
// code is made or mail sent (InvitationBoardBleed) - an invitation code by
// e-mail to each address that has no account yet. A re-invite never sends a
// stale code; a new code whose mail failed is taken back.
func (s *Server) methodSendInvitation(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	var emails, boards []string
	if len(args) < 2 || json.Unmarshal(args[0], &emails) != nil || json.Unmarshal(args[1], &boards) != nil || len(emails) > 1000 || len(boards) > 1000 {
		return nil, &methodError{"Match.Error", "Expected [String], [String]"}
	}
	if !s.mayInvite(r, u) {
		return nil, &methodError{"not-allowed", "not-allowed"}
	}
	ctx := r.Context()
	roles := s.inviteRoles(r)
	for _, id := range boards {
		var b Board
		if !validID(id) || s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&b) != nil || !canInviteToBoard(u, &b, roles) {
			// As WeKan records it: no user, so the attempt never disables an account.
			s.recordSecurity(r, securityEvent{Key: "authz.invitation-boards", Action: "blocked", Source: "sendInvitation",
				Detail: "Invitation refused because the caller cannot invite to a requested board."})
			return nil, &methodError{"not-allowed", "not-allowed"}
		}
	}
	inviter := u.Username
	if u.Profile.Fullname != "" {
		inviter = u.Profile.Fullname + " (" + u.Username + " )"
	}
	lang := languageOf(r, u).Tag
	scheme := "https://"
	if r.TLS == nil {
		scheme = "http://"
	}
	for _, raw := range emails {
		email := strings.ToLower(strings.TrimSpace(raw))
		if a, err := mail.ParseAddress(email); err != nil || a.Address != email || len(email) > 254 {
			continue // WeKan skips what is not an address
		}
		if s.db.Collection("users").FindOne(ctx, bson.M{"emails.address": eq(email)}).Err() == nil {
			return nil, &methodError{"user-exist", "The user with the email " + email + " has already an account."}
		}
		var inv struct {
			ID    string `bson:"_id"`
			Code  any    `bson:"code"`
			Valid *bool  `bson:"valid"`
		}
		isNew := s.db.Collection("invitation_codes").FindOne(ctx, bson.M{"email": eq(email)}).Decode(&inv) != nil
		code, _ := inv.Code.(string)
		if isNew {
			inv.ID, code = randomID(), invitationCode()
			if _, err := s.db.Collection("invitation_codes").InsertOne(ctx, bson.M{"_id": inv.ID, "code": code, "email": email, // inserted, not a filter
				"boardsToBeInvited": boards, "createdAt": time.Now().UTC(), "authorId": u.ID, "valid": true}); err != nil { // inserted, not a filter
				return nil, &methodError{"invitation-generated-fail", "Failed to create invitation code"}
			}
		} else {
			set := bson.M{"boardsToBeInvited": boards}
			if code == "" || (inv.Valid != nil && !*inv.Valid) {
				code = invitationCode()
				set["code"], set["valid"] = code, true
			}
			if _, err := s.db.Collection("invitation_codes").UpdateOne(ctx, bson.M{"_id": inv.ID}, bson.M{"$set": set}); err != nil {
				return nil, &methodError{"invitation-generated-fail", "Failed to update invitation code"}
			}
		}
		params := map[string]string{"email": email, "inviter": inviter, "user": email[:strings.Index(email, "@")],
			"icode": code, "url": scheme + r.Host + "/sign-up"}
		err := s.sendMail(ctx, r.Host, mailMessage{To: []string{email},
			Subject: fillParams(tr(lang, "email-invite-register-subject"), params),
			Text:    fillParams(tr(lang, "email-invite-register-text"), params)})
		if err != nil {
			if isNew { // nothing was delivered for it; an older code was
				_, _ = s.db.Collection("invitation_codes").DeleteOne(ctx, bson.M{"_id": inv.ID})
			}
			return nil, &methodError{"email-fail", err.Error()}
		}
	}
	return 0, nil
}

// invitation is an invitation code, as invitation_codes keeps it.
type invitation struct {
	ID     string   `bson:"_id"`
	Code   string   `bson:"code"`
	Boards []string `bson:"boardsToBeInvited"`
}

// validInvitation is the valid invitation of that code for that address.
func (s *Server) validInvitation(ctx context.Context, code, email string) *invitation {
	code, email = strings.TrimSpace(code), strings.ToLower(strings.TrimSpace(email))
	if code == "" || len(code) > 200 || email == "" {
		return nil
	}
	var inv invitation
	if s.db.Collection("invitation_codes").FindOne(ctx, bson.M{"code": eq(code), "email": eq(email), "valid": true}).Decode(&inv) != nil {
		return nil
	}
	return &inv
}

// acceptInvitation is WeKan's Users.after.insert for an invited account: the
// user joins each board of the invitation - an earlier membership of theirs
// made active again - the boards are kept in profile.invitedBoards, and the
// code is spent.
func (s *Server) acceptInvitation(ctx context.Context, userID string, inv *invitation) {
	for _, id := range inv.Boards {
		var b Board
		if !validID(id) || s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&b) != nil {
			continue
		}
		members := slices.Clone(b.Members)
		if i := slices.IndexFunc(members, func(m Member) bool { return m.UserID == userID }); i >= 0 {
			members[i].IsActive = true
		} else {
			members = append(members, Member{UserID: userID, IsActive: true})
		}
		_, _ = s.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"members": members}})
		s.hub.publish(b.ID, "board-changed")
	}
	_, _ = s.db.Collection("users").UpdateOne(ctx, bson.M{"_id": eq(userID)},
		bson.M{"$set": bson.M{"profile.invitedBoards": inv.Boards, "profile.icode": inv.Code}})
	_, _ = s.db.Collection("invitation_codes").UpdateOne(ctx, bson.M{"_id": inv.ID}, bson.M{"$set": bson.M{"valid": false}})
}
