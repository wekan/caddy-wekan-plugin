package web

import (
	"net/http"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Users for a site admin, as Node.js WeKan (server/models/users.js): listed,
// read without their secrets, made, disabled and enabled, their boards taken
// over, login tokens made and deleted.
func (s *Server) apiUserRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/users", s.api(s.apiUsers))
	m.HandleFunc("GET /api/users/{user}", s.api(s.apiUserByID))
	m.HandleFunc("PUT /api/users/{user}", s.api(s.apiEditUser))
	m.HandleFunc("POST /api/users/", s.api(s.apiCreateUser))
	m.HandleFunc("POST /api/createtoken/{user}", s.api(s.apiCreateToken))
	m.HandleFunc("POST /api/deletetoken", s.api(s.apiDeleteToken))
}

// siteAdmin is WeKan's checkUserId: a site admin, or 403.
func siteAdmin(w http.ResponseWriter, u *User) bool {
	if !u.IsAdmin {
		apiError(w, http.StatusForbidden, "Forbidden")
		return false
	}
	return true
}

func (s *Server) apiUsers(w http.ResponseWriter, r *http.Request, u *User) {
	if !siteAdmin(w, u) {
		return
	}
	cur, err := s.db.Collection("users").Find(r.Context(), bson.M{}, options.Find().SetProjection(bson.M{"_id": 1, "username": 1}))
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var users []User
	_ = cur.All(r.Context(), &users)
	out := make([]map[string]string, 0, len(users))
	for _, x := range users {
		out = append(out, map[string]string{"_id": x.ID, "username": x.Username})
	}
	writeJSON(w, http.StatusOK, out)
}

// withoutSecrets is WeKan's (GHSA-6qpx-x7vr-p9w6): a user as an API answer may
// carry them - never the password hash, the login tokens or session data.
func withoutSecrets(doc bson.M) bson.M {
	delete(doc, "services")
	delete(doc, "sessionData")
	return doc
}

// memberships is the user's entry on each board, with the board's id.
func (s *Server) memberships(r *http.Request, userID string, activeOnly bool) []bson.M {
	filter := bson.M{"type": "board", "members.userId": eq(userID)}
	if activeOnly {
		filter = bson.M{"type": "board", "members": bson.M{"$elemMatch": bson.M{"userId": eq(userID), "isActive": true}}}
	}
	cur, err := s.db.Collection("boards").Find(r.Context(), filter, options.Find().SetProjection(bson.M{"_id": 1, "members": 1}))
	out := []bson.M{}
	if err != nil {
		return out
	}
	var boards []Board
	_ = cur.All(r.Context(), &boards)
	for _, b := range boards {
		for _, m := range b.Members {
			if m.UserID == userID {
				out = append(out, bson.M{"boardId": b.ID, "isAdmin": m.IsAdmin, "isActive": m.IsActive, "isNoComments": m.IsNoComments,
					"isCommentOnly": m.IsCommentOnly, "isWorker": m.IsWorker, "isReadOnly": m.IsReadOnly, "isReadAssignedOnly": m.IsReadAssignedOnly,
					"isNormalAssignedOnly": m.IsNormalAssignedOnly, "isCommentAssignedOnly": m.IsCommentAssignedOnly})
				break
			}
		}
	}
	return out
}

func (s *Server) apiUserByID(w http.ResponseWriter, r *http.Request, u *User) {
	if !siteAdmin(w, u) {
		return
	}
	id := r.PathValue("user")
	var doc bson.M
	if len(id) > 254 || (s.db.Collection("users").FindOne(r.Context(), bson.M{"_id": eq(id)}).Decode(&doc) != nil &&
		s.db.Collection("users").FindOne(r.Context(), bson.M{"username": eq(id)}).Decode(&doc) != nil) {
		apiError(w, http.StatusNotFound, "User not found")
		return
	}
	uid, _ := doc["_id"].(string)
	doc["boards"] = s.memberships(r, uid, false)
	writeJSON(w, http.StatusOK, withoutSecrets(doc))
}

func (s *Server) apiEditUser(w http.ResponseWriter, r *http.Request, u *User) {
	if !siteAdmin(w, u) {
		return
	}
	id := r.PathValue("user")
	var doc bson.M
	if !validID(id) || s.db.Collection("users").FindOne(r.Context(), bson.M{"_id": eq(id)}).Decode(&doc) != nil {
		apiError(w, http.StatusNotFound, "User not found")
		return
	}
	var req struct {
		Action string `json:"action"`
	}
	_ = decodeJSON(r, &req)
	ctx := r.Context()
	switch {
	case req.Action == "takeOwnership":
		// The admin takes the user's place on every board the user administers.
		cur, err := s.db.Collection("boards").Find(ctx, bson.M{"members": bson.M{"$elemMatch": bson.M{"userId": eq(id), "isAdmin": true}}},
			options.Find().SetSort(bson.D{{Key: "sort", Value: 1}}))
		if err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
		var boards []Board
		_ = cur.All(ctx, &boards)
		out := []map[string]string{}
		for _, b := range boards {
			members := []Member{}
			for _, m := range b.Members {
				if m.UserID == u.ID {
					continue // the admin's own earlier entry goes; they take the user's
				}
				if m.UserID == id {
					m.UserID = u.ID
				}
				members = append(members, m)
			}
			if _, err := s.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"members": members}}); err != nil {
				apiError(w, http.StatusInternalServerError, err.Error())
				return
			}
			out = append(out, map[string]string{"_id": b.ID, "title": b.Title})
		}
		writeJSON(w, http.StatusOK, out)
		return
	case req.Action == "disableLogin" && id != u.ID:
		// Signed out everywhere too: its login tokens go with it.
		_, _ = s.db.Collection("users").UpdateOne(ctx, bson.M{"_id": eq(id)},
			bson.M{"$set": bson.M{"loginDisabled": true, "services.resume.loginTokens": bson.A{}}})
	case req.Action == "enableLogin":
		_, _ = s.db.Collection("users").UpdateOne(ctx, bson.M{"_id": eq(id)}, bson.M{"$set": bson.M{"loginDisabled": false}})
	}
	doc = bson.M{}
	_ = s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&doc)
	writeJSON(w, http.StatusOK, withoutSecrets(doc))
}

// apiCreateUser is WeKan's POST /api/users/: a site admin makes an account,
// whatever Admin Panel says of registration.
func (s *Server) apiCreateUser(w http.ResponseWriter, r *http.Request, u *User) {
	if !siteAdmin(w, u) {
		return
	}
	var req struct {
		Username string `json:"username"`
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	req.Username, req.Email = strings.TrimSpace(req.Username), strings.TrimSpace(req.Email)
	if !usernamePattern.MatchString(req.Username) {
		apiError(w, http.StatusBadRequest, "a username is 2 to 64 letters, digits, dots, dashes or underscores")
		return
	}
	if a, err := mail.ParseAddress(req.Email); err != nil || a.Address != req.Email || len(req.Email) > 254 {
		apiError(w, http.StatusBadRequest, "an e-mail address is needed")
		return
	}
	if p := passwordProblem(req.Password, req.Username, req.Email); p != "" {
		apiError(w, http.StatusBadRequest, p)
		return
	}
	ctx := r.Context()
	if n, _ := s.db.Collection("users").CountDocuments(ctx, bson.M{"username": eq(req.Username)}); n > 0 {
		apiError(w, http.StatusForbidden, "Username already exists.")
		return
	}
	if n, _ := s.db.Collection("users").CountDocuments(ctx, bson.M{"emails.address": eq(strings.ToLower(req.Email))}); n > 0 {
		apiError(w, http.StatusForbidden, "Email already exists.")
		return
	}
	nu, err := s.CreateUser(ctx, req.Username, req.Email, req.Password, false)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"_id": nu.ID})
}

// apiCreateToken: a site admin makes a login token for another user - with a
// reason, kept in impersonatedUsers, as WeKan does.
func (s *Server) apiCreateToken(w http.ResponseWriter, r *http.Request, u *User) {
	if !siteAdmin(w, u) {
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	_ = decodeJSON(r, &req)
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		apiError(w, http.StatusBadRequest, "A reason is required when creating a token for another user")
		return
	}
	id := r.PathValue("user")
	if !validID(id) || s.db.Collection("users").FindOne(r.Context(), bson.M{"_id": eq(id)}).Err() != nil {
		apiError(w, http.StatusNotFound, "User not found")
		return
	}
	if len(reason) > 500 {
		reason = reason[:500]
	}
	if _, err := s.db.Collection("impersonatedUsers").InsertOne(r.Context(), bson.M{"_id": randomID(), "adminId": u.ID, "userId": id,
		"reason": "restCreateToken: " + reason, "createdAt": time.Now().UTC()}); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	token, _, err := s.newLoginToken(r.Context(), id)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"_id": id, "authToken": token})
}

// apiDeleteToken: a site admin deletes a user's login token - given as the
// token or as its hash, as WeKan stores it - or all of them.
func (s *Server) apiDeleteToken(w http.ResponseWriter, r *http.Request, u *User) {
	if !siteAdmin(w, u) {
		return
	}
	var req struct {
		UserID string `json:"userId"`
		Token  string `json:"token"`
	}
	_ = decodeJSON(r, &req)
	msg := "Expected a userId to be set but received none."
	if validID(req.UserID) {
		updates := []bson.M{{"$set": bson.M{"services.resume.loginTokens": bson.A{}}}}
		msg = "Delete all token from user: " + req.UserID
		if req.Token != "" {
			updates = []bson.M{{"$pull": bson.M{"services.resume.loginTokens": bson.M{"hashedToken": hashToken(req.Token)}}}}
			if storedHash(req.Token) != "" {
				updates = append(updates, bson.M{"$pull": bson.M{"services.resume.loginTokens": bson.M{"hashedToken": storedHash(req.Token)}}})
			}
			msg = "Delete token: [" + req.Token + "] from user: " + req.UserID
		}
		for _, update := range updates {
			if _, err := s.db.Collection("users").UpdateOne(r.Context(), bson.M{"_id": eq(req.UserID)}, update); err != nil {
				apiError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": msg})
}

var storedHashPattern = regexp.MustCompile(`^[A-Za-z0-9+/]{43}=$`)

// storedHash is a login token's hash as WeKan stores it - base64 of SHA-256 -
// or "" for anything else.
func storedHash(s string) string {
	if storedHashPattern.MatchString(s) {
		return s
	}
	return ""
}
