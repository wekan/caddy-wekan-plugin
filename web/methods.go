package web

import (
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// WeKan's Meteor methods that the browser - WeKan's scripts and tests - calls
// with Meteor.call / Meteor.callAsync: POST /_methods/{name} with the
// arguments as a JSON array, answering the result, or {error, reason} as a
// Meteor.Error does. A method Weddy has not got answers 404, never a guess.
type methodFunc func(r *http.Request, u *User, args []json.RawMessage) (any, error)

// methodError is a Meteor.Error: its error code and reason.
type methodError struct{ Code, Reason string }

func (e *methodError) Error() string { return e.Code }

func (s *Server) methods() map[string]methodFunc {
	return map[string]methodFunc{
		"setLanguage": s.methodSetLanguage,
	}
}

func (s *Server) methodRoutes(m *http.ServeMux) {
	m.HandleFunc("POST /_methods/{name}", func(w http.ResponseWriter, r *http.Request) {
		u := s.currentUser(r)
		if u == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not-authorized", "reason": "You must be logged in."})
			return
		}
		// JSON only: a form on another site cannot send it without a CORS
		// preflight, on top of the session cookie's SameSite=Lax.
		if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "application/json" {
			writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "415", "reason": "application/json only"})
			return
		}
		fn := s.methods()[r.PathValue("name")]
		if fn == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "404", "reason": "Method '" + r.PathValue("name") + "' not found"})
			return
		}
		var args []json.RawMessage
		r.Body = http.MaxBytesReader(w, r.Body, 16<<20)
		if err := json.NewDecoder(r.Body).Decode(&args); err != nil && r.ContentLength > 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Match.Error", "reason": "the arguments are a JSON array"})
			return
		}
		out, err := fn(r, u, args)
		if err != nil {
			var me *methodError
			if errors.As(err, &me) {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": me.Code, "reason": me.Reason})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "500", "reason": "Internal server error"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"result": out})
	})
}

// stringArg is argument i as text, or a Match.Error.
func stringArg(args []json.RawMessage, i int) (string, error) {
	var s string
	if i >= len(args) || json.Unmarshal(args[i], &s) != nil {
		return "", &methodError{"Match.Error", "Expected string"}
	}
	return s, nil
}

// methodSetLanguage is WeKan's setLanguage: one of WeKan's languages, kept in
// profile.language.
func (s *Server) methodSetLanguage(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	code, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	l := translations.lookup(code)
	if l == nil || !strings.EqualFold(langKey(l.Tag), langKey(code)) && !strings.EqualFold(langKey(l.Code), langKey(code)) {
		return nil, &methodError{"invalid-language", "no such language"}
	}
	_, err = s.db.Collection("users").UpdateOne(r.Context(), bson.M{"_id": eq(u.ID)}, bson.M{"$set": bson.M{"profile.language": l.Code}})
	return nil, err
}
