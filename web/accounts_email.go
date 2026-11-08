package web

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// E-mail verification and forgotten passwords, as Meteor's accounts-password
// does them for Node.js WeKan: a token in the user's services - verification
// tokens in services.email.verificationTokens, a reset in
// services.password.reset - and a link to it by mail.

// resetTokenLifetime is Meteor's passwordResetTokenExpirationInDays: 3 days.
const resetTokenLifetime = 3 * 24 * time.Hour

// resetMailInterval: one reset mail per address a minute, so a flood of
// requests cannot fill an inbox.
const resetMailInterval = time.Minute

func (s *Server) accountEmailRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /verify-email/{token}", s.verifyEmail)
	m.HandleFunc("GET /forgot-password", s.forgotPasswordPage)
	m.HandleFunc("POST /forgot-password", s.forgotPassword)
	m.HandleFunc("GET /reset-password/{token}", s.resetPasswordPage)
	m.HandleFunc("POST /reset-password/{token}", s.resetPassword)
}

// tokenSafe: a token from a link is plain text of a sane length.
func tokenSafe(t string) bool {
	return t != "" && len(t) <= 200 && !strings.ContainsAny(t, "\x00$.{}")
}

// signInAs makes a session for the user and goes to All Boards.
func (s *Server) signInAs(w http.ResponseWriter, r *http.Request, userID string) {
	token, expires, err := s.newLoginToken(r.Context(), userID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	setSessionCookie(w, token, expires)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// verifyEmail is Meteor's verifyEmail (#1426): the token's address verified,
// the token spent, and the user signed in. A token that is not one signs in
// nobody and verifies nothing.
func (s *Server) verifyEmail(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	ctx := r.Context()
	var u struct {
		ID       string  `bson:"_id"`
		Emails   []Email `bson:"emails"`
		Disabled bool    `bson:"loginDisabled"`
		Services struct {
			Email struct {
				Tokens []struct {
					Token   string `bson:"token"`
					Address string `bson:"address"`
				} `bson:"verificationTokens"`
			} `bson:"email"`
		} `bson:"services"`
	}
	if !tokenSafe(token) || s.db.Collection("users").FindOne(ctx, bson.M{"services.email.verificationTokens.token": eq(token)}).Decode(&u) != nil || u.Disabled {
		http.Redirect(w, r, "/sign-in", http.StatusSeeOther)
		return
	}
	address := ""
	for _, t := range u.Services.Email.Tokens {
		if t.Token == token {
			address = t.Address
		}
	}
	emails := slices.Clone(u.Emails)
	for i := range emails {
		if strings.EqualFold(emails[i].Address, address) {
			emails[i].Verified = true
		}
	}
	// The whole list: FerretDB has no positional $ to set one entry.
	if _, err := s.db.Collection("users").UpdateOne(ctx, bson.M{"_id": u.ID}, bson.M{"$set": bson.M{"emails": emails},
		"$pull": bson.M{"services.email.verificationTokens": bson.M{"address": address}}}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.signInAs(w, r, u.ID)
}

func (s *Server) forgotPasswordPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "forgot", http.StatusOK, map[string]any{"Title": tr(languageOf(r, nil).Tag, "forgot-password")})
}

// forgotPassword is Meteor's forgotPassword: a reset link by mail to the
// address of an account. The answer is the same whether there is one or not,
// so the page tells nobody which addresses have accounts.
func (s *Server) forgotPassword(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-site request refused", http.StatusForbidden)
		return
	}
	lang := languageOf(r, nil).Tag
	email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
	if email != "" && len(email) <= 254 {
		s.sendReset(r, email, lang)
	}
	s.render(w, r, "forgot", http.StatusOK, map[string]any{"Title": tr(lang, "forgot-password"), "Sent": true})
}

func (s *Server) sendReset(r *http.Request, email, lang string) {
	ctx := r.Context()
	var u struct {
		ID       string `bson:"_id"`
		Username string `bson:"username"`
		Disabled bool   `bson:"loginDisabled"`
		Services struct {
			Password struct {
				Reset struct {
					When time.Time `bson:"when"`
				} `bson:"reset"`
			} `bson:"password"`
		} `bson:"services"`
	}
	if s.db.Collection("users").FindOne(ctx, bson.M{"emails.address": eq(email)}).Decode(&u) != nil || u.Disabled {
		return
	}
	if time.Since(u.Services.Password.Reset.When) < resetMailInterval {
		return // one mail a minute: the link already sent still works
	}
	token := invitationCode() + invitationCode()
	if _, err := s.db.Collection("users").UpdateOne(ctx, bson.M{"_id": u.ID}, bson.M{"$set": bson.M{"services.password.reset": bson.M{
		"token": token, "email": email, "when": time.Now().UTC(), "reason": "reset"}}}); err != nil {
		return
	}
	scheme := "https://"
	if r.TLS == nil {
		scheme = "http://"
	}
	params := map[string]string{"user": u.Username, "url": scheme + r.Host + "/reset-password/" + token, "siteName": "WeKan"}
	if err := s.sendMail(ctx, r.Host, mailMessage{To: []string{email},
		Subject: fillParams(tr(lang, "email-resetPassword-subject"), params),
		Text:    fillParams(tr(lang, "email-resetPassword-text"), params)}); err != nil {
		s.log.Warn("reset password mail", "error", err)
	}
}

// resetUser is the user of a reset token that has not expired.
func (s *Server) resetUser(ctx context.Context, token string) (id, username, email string) {
	var u struct {
		ID       string `bson:"_id"`
		Username string `bson:"username"`
		Services struct {
			Password struct {
				Reset struct {
					Email string    `bson:"email"`
					When  time.Time `bson:"when"`
				} `bson:"reset"`
			} `bson:"password"`
		} `bson:"services"`
	}
	if !tokenSafe(token) || s.db.Collection("users").FindOne(ctx, bson.M{"services.password.reset.token": eq(token)}).Decode(&u) != nil ||
		time.Since(u.Services.Password.Reset.When) > resetTokenLifetime {
		return "", "", ""
	}
	return u.ID, u.Username, u.Services.Password.Reset.Email
}

func (s *Server) resetPasswordPage(w http.ResponseWriter, r *http.Request) {
	lang := languageOf(r, nil).Tag
	if id, _, _ := s.resetUser(r.Context(), r.PathValue("token")); id == "" {
		s.render(w, r, "reset", http.StatusNotFound, map[string]any{"Title": tr(lang, "change-password"), "Expired": true})
		return
	}
	s.render(w, r, "reset", http.StatusOK, map[string]any{"Title": tr(lang, "change-password"), "Token": r.PathValue("token")})
}

// resetPassword is Meteor's resetPassword: a new password - by the same rules
// as at sign-up - the token spent, every other session signed out, the
// address verified (the mail reached it), and the user signed in.
func (s *Server) resetPassword(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-site request refused", http.StatusForbidden)
		return
	}
	lang := languageOf(r, nil).Tag
	token := r.PathValue("token")
	id, username, email := s.resetUser(r.Context(), token)
	if id == "" {
		s.render(w, r, "reset", http.StatusNotFound, map[string]any{"Title": tr(lang, "change-password"), "Expired": true})
		return
	}
	password := r.FormValue("password")
	problem := passwordProblem(password, username, email)
	if problem == "" && password != r.FormValue("password2") {
		problem = msg("password-again")
	}
	if problem != "" {
		s.render(w, r, "reset", http.StatusBadRequest, map[string]any{"Title": tr(lang, "change-password"), "Token": token, "Error": problem})
		return
	}
	hash, err := HashPassword(password)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ctx := r.Context()
	var doc struct {
		Emails []Email `bson:"emails"`
	}
	_ = s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&doc)
	for i := range doc.Emails {
		if strings.EqualFold(doc.Emails[i].Address, email) {
			doc.Emails[i].Verified = true
		}
	}
	if _, err := s.db.Collection("users").UpdateOne(ctx, bson.M{"_id": eq(id), "services.password.reset.token": eq(token)}, bson.M{
		"$set":   bson.M{"services.password.bcrypt": hash, "services.resume.loginTokens": bson.A{}, "emails": doc.Emails},
		"$unset": bson.M{"services.password.reset": ""}}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.signInAs(w, r, id)
}
