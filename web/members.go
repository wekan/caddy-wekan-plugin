package web

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
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
	// Avatar is where the browser loads the member's picture, if any.
	Avatar string
	Self   bool
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
		avatar := avatarDisplayURL(x.Profile.AvatarURL)
		if avatar != "" && b.Permission == "public" && strings.HasPrefix(avatar, "/cdn/storage/avatars/") {
			// So a public board's visitors may see it (avatarIsOnAPublicBoard).
			avatar += "?boardId=" + b.ID
		}
		out = append(out, sidebarMember{ID: x.ID, Username: x.Username, Fullname: x.Profile.Fullname, Role: m.Role(), Self: x.ID == u.ID, Avatar: avatar})
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
			if b.MemberOf(x.ID) || len(out) >= 10 || !s.mayAddToBoard(r.Context(), u, b, x.ID) {
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
	if !s.mayAddToBoard(r.Context(), u, b, x.ID) {
		http.Error(w, tr(languageOf(r, u).Tag, "error-user-notSameOrgOrTeam"), http.StatusForbidden)
		return
	}
	if _, err := s.addMemberToBoard(r, u, b, x.ID, true); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/b/"+b.ID+"/"+b.Slug, http.StatusSeeOther)
}

// addMemberToBoard makes userID an active member - an earlier membership made
// active again - with an invitation to accept in the sidebar, and records the
// addBoardMember activity, which puts it in their bell (#3136). Somebody
// already an active member is left as they are: no second activity.
//
// An existing account is told in its bell directly, whatever it watches
// (server/lib/boardInviteTray.js), when its tray settings let it; a new
// account (tray false) has nothing to sign in to yet, and is mailed.
func (s *Server) addMemberToBoard(r *http.Request, u *User, b *Board, userID string, tray bool) (bool, error) {
	members := slices.Clone(b.Members)
	if i := slices.IndexFunc(members, func(m Member) bool { return m.UserID == userID }); i >= 0 {
		if members[i].IsActive {
			return false, nil
		}
		members[i].IsActive = true
	} else {
		members = append(members, Member{UserID: userID, IsActive: true})
	}
	ctx := r.Context()
	if _, err := s.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"members": members, "modifiedAt": time.Now().UTC()}}); err != nil {
		return false, err
	}
	b.Members = members
	_, _ = s.db.Collection("users").UpdateOne(ctx, bson.M{"_id": eq(userID)}, bson.M{"$addToSet": bson.M{"profile.invitedBoards": b.ID}})
	since := time.Now().UTC().Add(-time.Second)
	s.activity(ctx, u.ID, "addBoardMember", bson.M{"boardId": b.ID, "memberId": userID, "type": "member"})
	if tray && s.notificationsOn(ctx, "Tray", userID, b.ID) {
		var a struct {
			ID string `bson:"_id"`
		}
		if s.db.Collection("activities").FindOne(ctx, bson.M{"activityType": "addBoardMember", "boardId": b.ID, "memberId": eq(userID), "createdAt": bson.M{"$gte": since}},
			options.FindOne().SetSort(bson.D{{Key: "createdAt", Value: -1}})).Decode(&a) == nil {
			_, _ = s.db.Collection("users").UpdateOne(ctx, bson.M{"_id": eq(userID), "profile.notifications.activity": bson.M{"$ne": a.ID}},
				bson.M{"$push": bson.M{"profile.notifications": bson.M{"activity": a.ID, "read": nil}}})
		}
	}
	s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	return true, nil
}

// mailBoardInvite is WeKan's invitation mail: in the invitee's language, or
// the inviter's for a new account (#5664). Best effort: the membership
// stands whether or not the mail goes.
func (s *Server) mailBoardInvite(r *http.Request, inviter *User, x *User, b *Board, isNew bool) {
	ctx := r.Context()
	var doc struct {
		Emails []struct {
			Address string `bson:"address"`
		} `bson:"emails"`
	}
	if s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(x.ID)}).Decode(&doc) != nil || len(doc.Emails) == 0 {
		return
	}
	lang := languageOf(r, inviter).Tag
	if !isNew {
		var full User
		if s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(x.ID)}).Decode(&full) == nil {
			lang = languageOf(r, &full).Tag
		}
	}
	scheme := "https://"
	if r.TLS == nil {
		scheme = "http://"
	}
	name := func(full, user string) string {
		if full != "" {
			return full + " (" + user + " )"
		}
		return user
	}
	params := map[string]string{"user": name(x.Profile.Fullname, x.Username), "inviter": name(inviter.Profile.Fullname, inviter.Username),
		"board": b.Title, "url": scheme + r.Host + "/b/" + b.ID + "/" + b.Slug}
	if err := s.sendMail(ctx, r.Host, mailMessage{To: []string{doc.Emails[0].Address},
		Subject: fillParams(tr(lang, "email-invite-subject"), params), Text: fillParams(tr(lang, "email-invite-text"), params)}); err != nil {
		s.log.Warn("board invitation mail", "error", err)
	}
}

// methodInviteUserToBoard is WeKan's inviteUserToBoard(username, boardId):
// whoever may invite to the board (Admin Panel / People / Roles) adds a user
// by username or by e-mail address - in any letter case, as a directory
// stores it (#6788) - or, for an address with no account and registration
// open, a new account (#6831), which is sent the link to set its password.
func (s *Server) methodInviteUserToBoard(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	name, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	boardID, err := stringArg(args, 1)
	if err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if strings.Contains(name, "/") || strings.Contains(boardID, "/") || name == "" || len(name) > 254 {
		return false, nil
	}
	ctx := r.Context()
	var b Board
	if !validID(boardID) || s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(boardID)}).Decode(&b) != nil {
		return nil, &methodError{"error-board-doesNotExist", "error-board-doesNotExist"}
	}
	if !u.IsAdmin && !b.MemberOf(u.ID) { // not a write: the roles rule below decides, as WeKan's
		return nil, &methodError{"error-board-notAMember", "error-board-notAMember"}
	}
	if !canInviteToBoard(u, &b, s.inviteRoles(r)) {
		return nil, &methodError{"error-notAllowed", "error-notAllowed"}
	}
	var x User
	found := false
	if strings.Contains(name, "@") {
		found = s.db.Collection("users").FindOne(ctx, bson.M{"emails.address": exactFold(name)}).Decode(&x) == nil
	} else {
		found = s.db.Collection("users").FindOne(ctx, bson.M{"$or": bson.A{bson.M{"_id": eq(name)}, bson.M{"username": eq(name)}}}).Decode(&x) == nil
	}
	isNew := !found
	if !found {
		if !strings.Contains(name, "@") || !email_.MatchString(name) {
			return nil, &methodError{"error-user-doesNotExist", "error-user-doesNotExist"}
		}
		if open, _ := s.registrationOpen(ctx); !open {
			return nil, &methodError{"error-user-notCreated", "error-user-notCreated"}
		}
		// A new account has no Organization or Team: under the restriction it
		// could not be added, so it is not made.
		if rs := s.boardMemberRestriction(ctx); !u.IsAdmin && (rs.Org || rs.Team) {
			return nil, &methodError{"error-user-notSameOrgOrTeam", "error-user-notSameOrgOrTeam"}
		}
		created, err := s.createInvitedUser(ctx, strings.ToLower(name))
		if err != nil {
			return nil, err
		}
		x = *created
		s.sendReset(r, strings.ToLower(name), languageOf(r, u).Tag)
	}
	if !s.mayAddToBoard(ctx, u, &b, x.ID) {
		return nil, &methodError{"error-user-notSameOrgOrTeam", "error-user-notSameOrgOrTeam"}
	}
	added, err := s.addMemberToBoard(r, u, &b, x.ID, !isNew)
	if err != nil {
		return nil, err
	}
	if added && !isNew {
		s.mailBoardInvite(r, u, &x, &b, false)
	}
	return x.ID, nil
}

// createInvitedUser is an account for an invited address: its name from the
// address, made unique, and no password until the link sets one.
func (s *Server) createInvitedUser(ctx context.Context, email string) (*User, error) {
	local, _, _ := strings.Cut(email, "@")
	base := strings.Trim(regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(local, ""), ".")
	if len(base) < 2 {
		base = "user"
	}
	if len(base) > 60 {
		base = base[:60]
	}
	username := base
	for i := 2; s.db.Collection("users").FindOne(ctx, bson.M{"username": eq(username)}).Err() == nil; i++ {
		username = base + strconv.Itoa(i)
	}
	doc := bson.M{"_id": randomID(), "username": username, "createdAt": time.Now().UTC(), "isAdmin": false, // inserted, not a filter
		"emails":  []bson.M{{"address": email, "verified": false}},
		"profile": bson.M{"fullname": username, "boardView": "board-view-swimlanes"}, "authenticationMethod": "password"}
	if _, err := s.db.Collection("users").InsertOne(ctx, doc); err != nil {
		return nil, err
	}
	return &User{ID: doc["_id"].(string), Username: username}, nil
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
	ctx := r.Context()
	if err := s.memberLeavesBoard(ctx, u.ID, b, i); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.activity(ctx, u.ID, "removeBoardMember", bson.M{"boardId": b.ID, "memberId": id})
	s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	http.Redirect(w, r, "/b/"+b.ID+"/"+b.Slug, http.StatusSeeOther)
}

// memberLeavesBoard is WeKan's removeMember with the Boards.before.update hook
// it sets off (server/models/boards.js, foreachRemovedMember): the entry stays,
// inactive and no longer an admin, without the groups it came through (#4178:
// a removal is final), and the person leaves the board's cards - members,
// requesters, assigners and watchers; assignees stay, as WeKan's - its lists'
// and the board's watchers, and their star unless the board is readable
// without membership (#3249). Each card is recorded in the change history as
// WeKan's after.update hook records it, by actorID: the admin who removed the
// member, or the member who left.
func (s *Server) memberLeavesBoard(ctx context.Context, actorID string, b *Board, i int) error {
	id := b.Members[i].UserID
	members := slices.Clone(b.Members)
	members[i].IsActive, members[i].IsAdmin = false, false
	if members[i].Rest != nil {
		rest := maps.Clone(members[i].Rest)
		delete(rest, "grantedBy")
		members[i].Rest = rest
	}
	if _, err := s.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{
		"$set":  bson.M{"members": members, "modifiedAt": time.Now().UTC()},
		"$pull": bson.M{"watchers": bson.M{"userId": b.Members[i].UserID}},
	}); err != nil {
		return err
	}
	b.Members = members
	pull := bson.M{"$pull": bson.M{"members": id, "requesters": id, "assigners": id, "watchers": id}}
	cur, err := s.db.Collection("cards").Find(ctx, bson.M{"boardId": b.ID, "$or": bson.A{
		bson.M{"members": id}, bson.M{"requesters": id}, bson.M{"assigners": id}, bson.M{"watchers": id}}},
		options.Find().SetProjection(bson.M{"_id": 1}))
	if err == nil {
		var cards []struct {
			ID string `bson:"_id"`
		}
		_ = cur.All(ctx, &cards)
		for _, c := range cards {
			_, _ = s.cardsUpdateOne(ctx, actorID, bson.M{"_id": c.ID}, pull)
		}
	}
	_, _ = s.db.Collection("lists").UpdateMany(ctx, bson.M{"boardId": b.ID}, bson.M{"$pull": bson.M{"watchers": id}})
	if b.Permission != "public" && b.Permission != "instance" {
		_, _ = s.db.Collection("users").UpdateOne(ctx, bson.M{"_id": eq(id)}, bson.M{"$pull": bson.M{"profile.starredBoards": b.ID}})
	}
	return nil
}

// methodSearchUsers is WeKan's searchUsers(query, boardId), the Members
// dialog's typeahead: for an active member (or a site admin), up to five users
// whose username, name or address contains the text in any letter case -
// never a disabled account (#1894), never one the org/team restriction would
// refuse (#6116) - read from fifty, so five that do not qualify cannot hide one
// that does (#6788).
func (s *Server) methodSearchUsers(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	query, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	boardID, err := stringArg(args, 1)
	if err != nil {
		return nil, err
	}
	ctx := r.Context()
	var b Board
	if !validID(boardID) || s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(boardID)}).Decode(&b) != nil {
		return nil, &methodError{"not-authorized", "User is not a member of this board"}
	}
	if !u.IsAdmin && !b.MemberOf(u.ID) { // not a write: the members' own search, as WeKan's
		return nil, &methodError{"not-authorized", "User is not a member of this board"}
	}
	out := []map[string]any{}
	if len([]rune(query)) < 2 || len(query) > 100 {
		return out, nil
	}
	// The text is quoted, so it is matched as text and never as a pattern.
	rx := bson.M{"$regex": regexp.QuoteMeta(query), "$options": "i"}
	or := bson.A{}
	for _, f := range []string{"username", "profile.fullname", "emails.address"} {
		or = append(or, bson.M{f: rx})
	}
	cur, err := s.db.Collection("users").Find(ctx, bson.M{"$or": or, "loginDisabled": bson.M{"$ne": true}},
		options.Find().SetLimit(50).SetProjection(bson.M{"username": 1, "profile.fullname": 1, "profile.initials": 1}))
	if err != nil {
		return out, nil
	}
	var users []User
	_ = cur.All(ctx, &users)
	for _, x := range users {
		if len(out) >= 5 {
			break
		}
		if !s.mayAddToBoard(ctx, u, &b, x.ID) {
			continue
		}
		out = append(out, map[string]any{"_id": x.ID, "username": x.Username, "profile": map[string]any{"fullname": x.Profile.Fullname}})
	}
	return out, nil
}
