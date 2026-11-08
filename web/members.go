package web

import (
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// A board's members in its sidebar, as Node.js WeKan's membersWidget,
// addMemberPopup, memberPopup and removeMemberPopup: the active members, a
// search to add one for whoever may invite, and removal by a board admin.

// sidebarMember is a member as the sidebar shows them.
type sidebarMember struct {
	ID, Username, Fullname, Role string
	Self                         bool
}

// sidebarMembers are the board's active members, in the board's order.
func (s *Server) sidebarMembers(r *http.Request, b *Board, u *User) []sidebarMember {
	var out []sidebarMember
	for _, m := range b.Members {
		if !m.IsActive {
			continue
		}
		var x User
		if s.db.Collection("users").FindOne(r.Context(), bson.M{"_id": eq(m.UserID)}).Decode(&x) != nil {
			continue
		}
		out = append(out, sidebarMember{ID: x.ID, Username: x.Username, Fullname: x.Profile.Fullname, Role: m.Role(), Self: x.ID == u.ID})
	}
	return out
}

func (s *Server) memberRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /b/{board}/members/search", s.page(s.searchMembers))
	m.HandleFunc("POST /b/{board}/members", s.page(s.addBoardMember))
	m.HandleFunc("POST /b/{board}/members/{user}/remove", s.page(s.removeBoardMember))
}

// inviterBoard is the request's board, when the user may invite to it.
func (s *Server) inviterBoard(w http.ResponseWriter, r *http.Request, u *User) *Board {
	b, err := s.boardFor(r.Context(), r.PathValue("board"), u)
	if err != nil || !canInviteToBoard(u, b, s.inviteRoles(r)) {
		http.Error(w, "not found", http.StatusNotFound)
		return nil
	}
	return b
}

// searchMembers is addMemberPopup's search: up to ten users whose username,
// name or address starts with the text, who are not active members yet.
func (s *Server) searchMembers(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.inviterBoard(w, r, u)
	if b == nil {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	out := []map[string]string{}
	if q == "" || len(q) > 100 {
		writeJSON(w, http.StatusOK, out)
		return
	}
	// The text is quoted, so it is matched as text and never as a pattern.
	rx := bson.M{"$regex": "^" + regexp.QuoteMeta(q), "$options": "i"}
	or := bson.A{}
	for _, f := range []string{"username", "profile.fullname", "emails.address"} {
		or = append(or, bson.M{f: rx})
	}
	cur, err := s.db.Collection("users").Find(r.Context(), bson.M{"$or": or, "loginDisabled": bson.M{"$ne": true}},
		options.Find().SetLimit(20).SetProjection(bson.M{"username": 1, "profile.fullname": 1}))
	if err == nil {
		var users []User
		_ = cur.All(r.Context(), &users)
		for _, x := range users {
			if b.MemberOf(x.ID) || len(out) >= 10 {
				continue
			}
			out = append(out, map[string]string{"_id": x.ID, "username": x.Username, "fullname": x.Profile.Fullname})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// addBoardMember is WeKan's inviteUserToBoard for a user who has an account:
// an active member - an earlier membership made active again - with an
// invitation to accept in the sidebar.
func (s *Server) addBoardMember(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.inviterBoard(w, r, u)
	if b == nil {
		return
	}
	id := r.FormValue("userId")
	var x User
	if !validID(id) || s.db.Collection("users").FindOne(r.Context(), bson.M{"_id": eq(id)}).Decode(&x) != nil {
		http.Error(w, "no such user", http.StatusBadRequest)
		return
	}
	members := slices.Clone(b.Members)
	if i := slices.IndexFunc(members, func(m Member) bool { return m.UserID == x.ID }); i >= 0 {
		members[i].IsActive = true
	} else {
		members = append(members, Member{UserID: x.ID, IsActive: true})
	}
	ctx := r.Context()
	if _, err := s.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"members": members, "modifiedAt": time.Now().UTC()}}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = s.db.Collection("users").UpdateOne(ctx, bson.M{"_id": x.ID}, bson.M{"$addToSet": bson.M{"profile.invitedBoards": b.ID}})
	s.activity(ctx, u.ID, "addBoardMember", bson.M{"boardId": b.ID, "memberId": x.ID})
	s.hub.publish(b.ID, "board-changed")
	http.Redirect(w, r, "/b/"+b.ID+"/"+b.Slug, http.StatusSeeOther)
}

// removeBoardMember is WeKan's removeMember (#5122, #6479): a board admin
// takes another member off the board - the entry made inactive, never the
// last active admin's - and off the board's cards.
func (s *Server) removeBoardMember(w http.ResponseWriter, r *http.Request, u *User) {
	b, err := s.boardFor(r.Context(), r.PathValue("board"), u)
	if err != nil || !b.AdminOf(u.ID) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	id := r.PathValue("user")
	i := slices.IndexFunc(b.Members, func(m Member) bool { return m.UserID == id })
	if !validID(id) || i < 0 || id == u.ID {
		http.Error(w, "not a member of this board", http.StatusBadRequest)
		return
	}
	admins := 0
	for _, m := range b.Members {
		if m.IsActive && m.IsAdmin {
			admins++
		}
	}
	if b.Members[i].IsAdmin && admins <= 1 {
		http.Error(w, "the board's last admin stays", http.StatusBadRequest)
		return
	}
	members := slices.Clone(b.Members)
	members[i].IsActive = false
	ctx := r.Context()
	if _, err := s.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"members": members, "modifiedAt": time.Now().UTC()}}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = s.db.Collection("cards").UpdateMany(ctx, bson.M{"boardId": b.ID}, bson.M{"$pull": bson.M{"members": id, "assignees": id}})
	s.activity(ctx, u.ID, "removeBoardMember", bson.M{"boardId": b.ID, "memberId": id})
	s.hub.publish(b.ID, "board-changed")
	http.Redirect(w, r, "/b/"+b.ID+"/"+b.Slug, http.StatusSeeOther)
}
