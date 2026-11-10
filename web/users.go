package web

// Users: signing up, the profile, changing the password, and Admin Panel /
// People - in WeKan's documents (users, and the settings document's
// disableRegistration).

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Password length for a new password: WeKan's default minimum, and its cap
// (hashing costs time in proportion to the length).
const (
	PasswordMinLength = 8
	PasswordMaxLength = 512
)

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{2,64}$`)

// passwordProblem is the translation key of what is wrong with a new password,
// or "".
func passwordProblem(password, username, email string) string {
	n := utf8.RuneCountInString(password)
	switch {
	case n < PasswordMinLength:
		return "password-too-short"
	case n > PasswordMaxLength:
		return "password-too-long"
	case strings.EqualFold(password, username) || (email != "" && strings.EqualFold(password, email)):
		return "password-same-as-username"
	}
	return ""
}

// registrationOpen: WeKan's Admin Panel setting - and always for the very first
// account, which becomes the administrator.
func (s *Server) registrationOpen(ctx context.Context) (open, first bool) {
	n, _ := s.db.Collection("users").EstimatedDocumentCount(ctx)
	if n == 0 {
		if c, _ := s.db.Collection("users").CountDocuments(ctx, bson.M{}); c == 0 {
			return true, true
		}
	}
	var set struct {
		DisableRegistration bool `bson:"disableRegistration"`
	}
	_ = s.db.Collection("settings").FindOne(ctx, bson.M{}).Decode(&set)
	return !set.DisableRegistration, false
}

// CreateUser makes an account as Meteor's accounts-password does.
func (s *Server) CreateUser(ctx context.Context, username, email, password string, admin bool) (*User, error) {
	hash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	u := bson.M{"_id": randomID(), "username": username, "createdAt": time.Now().UTC(), "isAdmin": admin, // inserted, not a filter
		"emails":   []bson.M{{"address": strings.ToLower(email), "verified": false}},
		"profile":  bson.M{"fullname": username, "boardView": "board-view-swimlanes"},
		"services": bson.M{"password": bson.M{"bcrypt": hash}}, "authenticationMethod": "password"}
	if _, err := s.db.Collection("users").InsertOne(ctx, u); err != nil {
		return nil, err
	}
	return &User{ID: u["_id"].(string), Username: username, IsAdmin: admin}, nil
}

func (s *Server) signUpPage(w http.ResponseWriter, r *http.Request) {
	open, _ := s.registrationOpen(r.Context())
	lang := languageOf(r, nil).Tag
	// Closed: only with an invitation code (WeKan's invitationcode field).
	s.render(w, r, "signup", http.StatusOK, map[string]any{"Title": tr(lang, "register"), "Invite": !open})
}

func (s *Server) signUp(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-site request refused", http.StatusForbidden)
		return
	}
	lang := languageOf(r, nil).Tag
	open, first := s.registrationOpen(r.Context())
	username := strings.TrimSpace(r.FormValue("username"))
	email := strings.TrimSpace(r.FormValue("email"))
	password := r.FormValue("password")
	fail := func(key string) {
		s.render(w, r, "signup", http.StatusBadRequest, map[string]any{"Title": tr(lang, "register"), "Error": key,
			"Username": username, "Email": email, "Invite": !open})
	}
	// Registration closed: only with a valid invitation code for this very
	// address (WeKan's onCreateUser). SignupBleed's lesson: the server refuses,
	// whatever the page showed. A wrong code is what guessing looks like
	// (InviteBleed); a typo looks the same, so it is detected, not blocked.
	var invite *invitation
	if !open {
		invite = s.validInvitation(r.Context(), r.FormValue("invitationcode"), email)
		if invite == nil {
			if r.FormValue("invitationcode") != "" {
				s.recordSecurity(r, securityEvent{Key: "brute.invite", Action: "detected", Source: "register:invitation-code",
					Detail: "Sign-up with an invitation code that does not exist for that email."})
			}
			s.render(w, r, "signup", http.StatusForbidden, map[string]any{"Title": tr(lang, "register"), "Invite": true,
				"Error": msg("error-invitation-code-not-exist"), "Username": username, "Email": email})
			return
		}
	}
	switch {
	case !usernamePattern.MatchString(username):
		fail(msg("error-user-notCreated"))
		return
	case !email_.MatchString(email) || len(email) > 254:
		fail(msg("error-user-notCreated"))
		return
	}
	if p := passwordProblem(password, username, email); p != "" {
		fail(p)
		return
	}
	if r.FormValue("password") != r.FormValue("password2") {
		fail(msg("password-again"))
		return
	}
	if n, _ := s.db.Collection("users").CountDocuments(r.Context(), bson.M{"username": eq(username)}); n > 0 {
		fail(msg("error-username-taken"))
		return
	}
	if n, _ := s.db.Collection("users").CountDocuments(r.Context(), bson.M{"emails.address": eq(strings.ToLower(email))}); n > 0 {
		fail(msg("error-email-taken"))
		return
	}
	u, err := s.CreateUser(r.Context(), username, email, password, first)
	if err != nil {
		fail(msg("error-user-notCreated"))
		return
	}
	if invite != nil {
		s.acceptInvitation(r.Context(), u.ID, invite)
	}
	s.sendVerification(r, u.ID, u.Username, strings.ToLower(email), lang)
	token, expires, err := s.newLoginToken(r.Context(), u.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	setSessionCookie(w, token, expires)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

var email_ = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)

func (s *Server) profilePage(w http.ResponseWriter, r *http.Request, u *User) {
	lang := languageOf(r, u).Tag
	key := r.URL.Query().Get("error")
	text := ""
	if key != "" {
		text = fillParams(tr(lang, key), map[string]string{"size": "72 kB"})
	}
	var me bson.M
	_ = s.db.Collection("users").FindOne(r.Context(), bson.M{"_id": eq(u.ID)}).Decode(&me)
	s.render(w, r, "profile", http.StatusOK, map[string]any{"Title": tr(lang, "edit-profile"), "User": u,
		"Saved": r.URL.Query().Get("saved"), "Error": key, "ErrorText": text,
		"Delivery": memberDeliveryForm(me, lang), "HideIdentity": nestedField(me, "profile", "webhookHideIdentity") == true})
}

func (s *Server) saveProfile(w http.ResponseWriter, r *http.Request, u *User) {
	fullname := strings.TrimSpace(r.FormValue("fullname"))
	initials := strings.TrimSpace(r.FormValue("initials"))
	if utf8.RuneCountInString(fullname) > 200 || utf8.RuneCountInString(initials) > 8 {
		http.Redirect(w, r, "/profile?error=fullname", http.StatusSeeOther)
		return
	}
	if _, err := s.db.Collection("users").UpdateOne(r.Context(), bson.M{"_id": eq(u.ID)},
		bson.M{"$set": bson.M{"profile.fullname": fullname, "profile.initials": initials}}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Edit Profile's address, when Admin Panel lets a user change it.
	current := ""
	if len(u.Emails) > 0 {
		current = u.Emails[0].Address
	}
	if email := strings.TrimSpace(r.FormValue("email")); r.Form.Has("email") && email != current {
		if err := s.setEmail(r.Context(), u, u.ID, email); err != nil {
			var me *methodError
			if errors.As(err, &me) {
				http.Redirect(w, r, "/profile?error="+url.QueryEscape(me.Code), http.StatusSeeOther)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	http.Redirect(w, r, "/profile?saved=profile", http.StatusSeeOther)
}

// changePassword needs the current password; the other sessions end, this one
// stays (a new token).
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, u *User) {
	if CheckPassword(u, r.FormValue("oldPassword")) != nil {
		http.Redirect(w, r, "/profile?error=error-ldap-login", http.StatusSeeOther)
		return
	}
	email := ""
	if len(u.Emails) > 0 {
		email = u.Emails[0].Address
	}
	password := r.FormValue("newPassword")
	if p := passwordProblem(password, u.Username, email); p != "" {
		http.Redirect(w, r, "/profile?error="+p, http.StatusSeeOther)
		return
	}
	if password != r.FormValue("newPassword2") {
		http.Redirect(w, r, "/profile?error=password-again", http.StatusSeeOther)
		return
	}
	hash, err := HashPassword(password)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := s.db.Collection("users").UpdateOne(r.Context(), bson.M{"_id": eq(u.ID)}, bson.M{
		"$set":   bson.M{"services.password.bcrypt": hash, "services.resume.loginTokens": []bson.M{}},
		"$unset": bson.M{"services.password.argon2": ""}}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	token, expires, err := s.newLoginToken(r.Context(), u.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	setSessionCookie(w, token, expires)
	http.Redirect(w, r, "/profile?saved=password", http.StatusSeeOther)
}

// Admin Panel / People: the site admins see every user and set admin and
// disabled - never on themselves, so the last admin cannot lock everyone out.
func (s *Server) peoplePage(w http.ResponseWriter, r *http.Request, u *User) {
	if !u.IsAdmin {
		s.render(w, r, "notfound", http.StatusNotFound, map[string]any{"Title": tr(languageOf(r, u).Tag, "error-notAuthorized"), "User": u})
		return
	}
	cur, err := s.db.Collection("users").Find(r.Context(), bson.M{}, options.Find().SetSort(bson.D{{Key: "username", Value: 1}}).SetLimit(1000))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var users []User
	_ = cur.All(r.Context(), &users)
	var set struct {
		DisableRegistration bool `bson:"disableRegistration"`
	}
	_ = s.db.Collection("settings").FindOne(r.Context(), bson.M{}).Decode(&set)
	lang := languageOf(r, u).Tag
	errText := ""
	if key := r.URL.Query().Get("error"); key != "" {
		errText = tr(lang, key)
	}
	s.render(w, r, "people", http.StatusOK, map[string]any{"Title": tr(lang, "people"), "User": u,
		"Users": users, "RegistrationDisabled": set.DisableRegistration, "ErrorText": errText,
		"Pane": map[string]string{"orgs": "orgs", "teams": "teams"}[r.URL.Query().Get("pane")],
		"Orgs": s.groupRows(r.Context(), "org"), "Teams": s.groupRows(r.Context(), "team")})
}

func (s *Server) setPerson(w http.ResponseWriter, r *http.Request, u *User) {
	if !u.IsAdmin {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	id := r.PathValue("user")
	if !validID(id) || id == u.ID {
		http.Error(w, "not yourself", http.StatusBadRequest)
		return
	}
	set := bson.M{}
	if v := r.FormValue("isAdmin"); v != "" {
		set["isAdmin"] = v == "true"
	}
	if v := r.FormValue("loginDisabled"); v != "" {
		set["loginDisabled"] = v == "true"
	}
	if len(set) == 0 {
		http.Error(w, "nothing to set", http.StatusBadRequest)
		return
	}
	update := bson.M{"$set": set}
	if set["loginDisabled"] == true {
		set["services.resume.loginTokens"] = []bson.M{} // a disabled user is signed out everywhere
	}
	res, err := s.db.Collection("users").UpdateOne(r.Context(), bson.M{"_id": eq(id)}, update)
	if err != nil || res.MatchedCount == 0 {
		http.Error(w, "no such user", http.StatusNotFound)
		return
	}
	http.Redirect(w, r, "/admin/people", http.StatusSeeOther)
}

func (s *Server) setRegistration(w http.ResponseWriter, r *http.Request, u *User) {
	if !u.IsAdmin {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	disabled := r.FormValue("disableRegistration") == "true"
	_, err := s.db.Collection("settings").UpdateOne(r.Context(), bson.M{}, bson.M{"$set": bson.M{"disableRegistration": disabled}},
		options.UpdateOne().SetUpsert(true))
	if err != nil && !errors.Is(err, mongo.ErrNoDocuments) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin/people", http.StatusSeeOther)
}

func (s *Server) userRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /sign-up", s.signUpPage)
	s.accountEmailRoutes(m)
	s.memberRoutes(m)
	s.cardMoveRoutes(m)
	m.HandleFunc("POST /sign-up", s.signUp)
	m.HandleFunc("GET /profile", s.page(s.profilePage))
	m.HandleFunc("POST /profile", s.page(s.saveProfile))
	m.HandleFunc("POST /profile/password", s.page(s.changePassword))
	m.HandleFunc("POST /profile/notifications", s.page(s.saveMemberDelivery))
	m.HandleFunc("GET /admin/people", s.page(s.peoplePage))
	m.HandleFunc("POST /admin/people/{user}", s.page(s.setPerson))
	s.adminPeopleRoutes(m)
	m.HandleFunc("POST /admin/registration", s.page(s.setRegistration))
}

// accountSetting is one of Admin Panel's accountSettings switches.
func (s *Server) accountSetting(ctx context.Context, id string) bool {
	var doc struct {
		On bool `bson:"booleanValue"`
	}
	_ = s.db.Collection("accountSettings").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&doc)
	return doc.On
}

// setEmail is WeKan's: a site admin sets anybody's address, a user their own
// only while Admin Panel allows it (accounts-allowEmailChange). The address
// is new, unverified, and nobody else's.
func (s *Server) setEmail(ctx context.Context, actor *User, userID, email string) error {
	email = strings.TrimSpace(email)
	if strings.Contains(userID, "/") || strings.Contains(email, "/") {
		return &methodError{"not-authorized", "not-authorized"}
	}
	if !actor.IsAdmin && (actor.ID != userID || !s.accountSetting(ctx, "accounts-allowEmailChange")) {
		return &methodError{"not-authorized", "not-authorized"}
	}
	if !email_.MatchString(email) || len(email) > 254 || !validID(userID) {
		return &methodError{"Match.Error", "an e-mail address is needed"}
	}
	if s.db.Collection("users").FindOne(ctx, bson.M{"emails.address": eq(strings.ToLower(email))}).Err() == nil ||
		s.db.Collection("users").FindOne(ctx, bson.M{"emails.address": eq(email)}).Err() == nil {
		return &methodError{"email-already-taken", "email-already-taken"}
	}
	_, err := s.db.Collection("users").UpdateOne(ctx, bson.M{"_id": eq(userID)},
		bson.M{"$set": bson.M{"emails": bson.A{bson.M{"address": email, "verified": false}}}})
	return err
}
