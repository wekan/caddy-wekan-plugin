package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Admin Panel -> People, as Node.js WeKan's peopleBody: a site admin edits a
// user in editUserPopup - username, full name, initials, admin, address,
// active, a new password - and makes one with setCreateUser. Nobody else
// changes anything; the admin cannot take away their own admin or disable
// themselves. A new password signs the user out everywhere.

func (s *Server) adminPeopleRoutes(m *http.ServeMux) {
	m.HandleFunc("POST /admin/people/{user}/edit", s.page(s.editPerson))
	m.HandleFunc("GET /people", s.page(s.peoplePage))
	s.orgTeamAdminRoutes(m)
}

func (s *Server) editPerson(w http.ResponseWriter, r *http.Request, u *User) {
	if !u.IsAdmin {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	ctx := r.Context()
	id := r.PathValue("user")
	var target User
	if !validID(id) || s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&target) != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	back := func(key string) {
		to := "/admin/people"
		if key != "" {
			to += "?error=" + key
		}
		http.Redirect(w, r, to, http.StatusSeeOther)
	}
	username := strings.TrimSpace(r.FormValue("username"))
	email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
	fullname := strings.TrimSpace(r.FormValue("fullname"))
	initials := strings.TrimSpace(r.FormValue("initials"))
	set := bson.M{"profile.fullname": fullname, "profile.initials": initials, "modifiedAt": time.Now().UTC()}
	if len(fullname) > 200 || len([]rune(initials)) > 8 {
		back(msg("error-user-notCreated"))
		return
	}
	if username != "" && username != target.Username {
		if !usernamePattern.MatchString(username) {
			back(msg("error-user-notCreated"))
			return
		}
		if n, _ := s.db.Collection("users").CountDocuments(ctx, bson.M{"username": eq(username)}); n > 0 {
			back(msg("error-username-taken"))
			return
		}
		set["username"] = username
	}
	if email != "" && (len(target.Emails) == 0 || !strings.EqualFold(target.Emails[0].Address, email)) {
		if !email_.MatchString(email) || len(email) > 254 {
			back(msg("error-user-notCreated"))
			return
		}
		if n, _ := s.db.Collection("users").CountDocuments(ctx, bson.M{"emails.address": eq(email)}); n > 0 {
			back(msg("error-email-taken"))
			return
		}
		set["emails"] = bson.A{bson.M{"address": email, "verified": r.FormValue("verified") == "true"}}
	}
	if id != u.ID {
		// One's own admin and access are not one's own to take away here.
		set["isAdmin"] = r.FormValue("isAdmin") == "true"
		set["loginDisabled"] = r.FormValue("loginDisabled") == "true"
	}
	update := bson.M{"$set": set}
	if pw := r.FormValue("password"); pw != "" {
		if p := s.passwordProblem(r.Context(), pw, target.Username, email); p != "" {
			back(p)
			return
		}
		hash, err := HashPassword(pw)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		set["services.password.bcrypt"], set["services.resume.loginTokens"] = hash, bson.A{}
		update["$unset"] = bson.M{"services.password.argon2": ""}
	}
	if _, err := s.db.Collection("users").UpdateOne(ctx, bson.M{"_id": eq(id)}, update); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	back("")
}

// methodSetCreateUser is WeKan's setCreateUser: a site admin makes a user -
// "true"/"false" strings for admin and for the form's inverted Active select,
// which says loginDisabled - with a password that meets the policy.
func (s *Server) methodSetCreateUser(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	str := make([]string, 7)
	for i := range str {
		v, err := stringArg(args, i)
		if err != nil {
			return nil, err
		}
		str[i] = v
	}
	for i := 7; i < 10; i++ {
		var a []any
		if len(args) <= i || json.Unmarshal(args[i], &a) != nil {
			return nil, &methodError{"Match.Error", "Expected Array"}
		}
	}
	fullname, username, initials, password, isAdmin, isActive, email := str[0], str[1], str[2], str[3], str[4], str[5], strings.ToLower(str[6])
	for _, v := range []string{fullname, username, email, initials} {
		if strings.Contains(v, "/") {
			return false, nil
		}
	}
	if !u.IsAdmin || u.LoginDisabled {
		return nil, &methodError{"not-authorized", "Only administrators can create users"}
	}
	ctx := r.Context()
	if n, _ := s.db.Collection("users").CountDocuments(ctx, bson.M{"username": eq(username)}); n > 0 {
		return nil, &methodError{"username-already-taken", "username-already-taken"}
	}
	if n, _ := s.db.Collection("users").CountDocuments(ctx, bson.M{"emails.address": eq(email)}); n > 0 {
		return nil, &methodError{"email-already-taken", "email-already-taken"}
	}
	if (isAdmin != "true" && isAdmin != "false") || (isActive != "true" && isActive != "false") {
		return nil, &methodError{"invalid-parameters", "invalid-parameters"}
	}
	if !usernamePattern.MatchString(username) || !email_.MatchString(email) || len(fullname) > 200 || len([]rune(initials)) > 8 {
		return nil, &methodError{"error-user-notCreated", "error-user-notCreated"}
	}
	if p := s.passwordProblem(r.Context(), password, username, email); p != "" {
		return nil, &methodError{"error-password-policy", tr("en", p)}
	}
	nu, err := s.CreateUser(ctx, username, email, password, isAdmin == "true")
	if err != nil {
		return nil, &methodError{"error-user-notCreated", "error-user-notCreated"}
	}
	_, _ = s.db.Collection("users").UpdateOne(ctx, bson.M{"_id": eq(nu.ID)}, bson.M{"$set": bson.M{
		"profile.fullname": fullname, "profile.initials": initials, "loginDisabled": isActive == "true"}})
	return nu.ID, nil
}
