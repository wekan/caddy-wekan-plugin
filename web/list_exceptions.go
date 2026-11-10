package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Setting a list's exceptions (#4527), as Node.js WeKan's
// server/listPermissions.js lists.setRoleExceptions and
// client/components/lists/listPermissions.js: for the board's admins (or a
// site admin), one choice per role and capability - as the board says,
// allowed, or refused in this list - cleaned as WeKan's cleanExceptions
// before it is stored. An empty set clears the list's exceptions.

// listRoles are WeKan's BOARD_ROLES a list may make an exception for: every
// role but the board's admins, whom an exception never restricts.
var listRoles = []string{"normal", "normal-assigned-only", "no-comments", "comment-only", "comment-assigned-only",
	"worker", "read-only", "read-assigned-only"}

var listCapabilities = []string{"write", "comment"}

var listRolePattern = regexp.MustCompile(`^(custom:[A-Za-z0-9_-]+|signed-in|[a-z-]+)$`)

// The page's words, named so cmd/sync-i18n copies them.
var _ = []string{msg("listPermissionsPopup-title"), msg("list-permissions-desc"), msg("list-permission-board"),
	msg("list-permission-allow"), msg("list-permission-deny"), msg("list-permission-signed-in"), msg("edit"), msg("comment"),
	msg("normal"), msg("normal-assigned-only"), msg("no-comments"), msg("comment-only"), msg("comment-assigned-only"),
	msg("worker"), msg("read-only"), msg("read-assigned-only")}

// cleanListExceptions is WeKan's cleanExceptions: known capabilities,
// booleans only, a role name of WeKan's shape and never the board's admins.
func cleanListExceptions(in any) bson.M {
	out := bson.M{}
	m, _ := in.(map[string]any)
	for role, caps := range m {
		if role == "" || role == "board-admin" || len(role) > 100 || !listRolePattern.MatchString(role) {
			continue
		}
		c, _ := caps.(map[string]any)
		entry := bson.M{}
		for _, capability := range listCapabilities {
			if v, ok := c[capability].(bool); ok {
				entry[capability] = v
			}
		}
		if len(entry) > 0 {
			out[role] = entry
		}
	}
	return out
}

// setListExceptions stores a list's exceptions for one of its board's admins.
func (s *Server) setListExceptions(ctx context.Context, u *User, listID string, exceptions any) (bson.M, error) {
	notAuthorized := &methodError{"error-notAuthorized", "error-notAuthorized"}
	var l struct {
		BoardID string `bson:"boardId"`
	}
	if !validID(listID) || s.db.Collection("lists").FindOne(ctx, bson.M{"_id": eq(listID)}).Decode(&l) != nil {
		return nil, notAuthorized
	}
	var b Board
	if !validID(l.BoardID) || s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(l.BoardID)}).Decode(&b) != nil {
		return nil, notAuthorized
	}
	if !u.IsAdmin && !b.AdminOf(u.ID) {
		return nil, notAuthorized
	}
	clean := cleanListExceptions(exceptions)
	mod := bson.M{"$unset": bson.M{"roleExceptions": ""}}
	if len(clean) > 0 {
		mod = bson.M{"$set": bson.M{"roleExceptions": clean}}
	}
	if _, err := s.db.Collection("lists").UpdateOne(ctx, bson.M{"_id": eq(listID), "boardId": b.ID}, mod); err != nil {
		return nil, err
	}
	s.hub.publish(b.ID, "board-changed")
	return clean, nil
}

// methodSetListExceptions is WeKan's lists.setRoleExceptions(listId, exceptions).
func (s *Server) methodSetListExceptions(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	listID, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	var exceptions any
	if len(args) > 1 && json.Unmarshal(args[1], &exceptions) != nil {
		return nil, &methodError{"Match.Error", "Expected object or null"}
	}
	if _, isObject := exceptions.(map[string]any); exceptions != nil && !isObject {
		return nil, &methodError{"Match.Error", "Expected object or null"}
	}
	clean, err := s.setListExceptions(r.Context(), u, listID, exceptions)
	if err != nil {
		return nil, err
	}
	return plain(clean), nil
}

type listPermissionCell struct {
	Role, Capability, Name, Aria, Choice string
}

type listPermissionRow struct {
	Key, Label string
	Cells      []listPermissionCell
}

// listForAdmin is a list of a board this user administers, for its page.
func (s *Server) listForAdmin(r *http.Request, u *User) (*Board, bson.M, bool) {
	b, err := s.boardFor(r.Context(), r.PathValue("board"), u)
	if err != nil || (!u.IsAdmin && !b.AdminOf(u.ID)) {
		return nil, nil, false
	}
	var l bson.M
	id := r.PathValue("list")
	if !validID(id) || s.db.Collection("lists").FindOne(r.Context(), bson.M{"_id": eq(id), "boardId": b.ID}).Decode(&l) != nil {
		return nil, nil, false
	}
	return b, l, true
}

// listPermissionsPage is WeKan's listPermissionsPopup, as a page.
func (s *Server) listPermissionsPage(w http.ResponseWriter, r *http.Request, u *User) {
	lang := languageOf(r, u).Tag
	b, l, ok := s.listForAdmin(r, u)
	if !ok {
		s.render(w, r, "notfound", http.StatusNotFound, map[string]any{"Title": tr(lang, "board-not-found"), "User": u})
		return
	}
	stored, _ := plain(l["roleExceptions"]).(map[string]any)
	roles := append([]string{}, listRoles...)
	if b.Permission == "public" || b.Permission == "instance" {
		roles = append(roles, "signed-in")
	}
	capLabels := map[string]string{"write": tr(lang, "edit"), "comment": tr(lang, "comment")}
	rows := []listPermissionRow{}
	for _, role := range roles {
		label := role
		if role == "signed-in" {
			label = "list-permission-signed-in"
		}
		row := listPermissionRow{Key: role, Label: tr(lang, label)}
		entry, _ := stored[role].(map[string]any)
		for _, capability := range listCapabilities {
			choice := ""
			if v, ok := entry[capability].(bool); ok {
				choice = map[bool]string{true: "allow", false: "deny"}[v]
			}
			row.Cells = append(row.Cells, listPermissionCell{Role: role, Capability: capability, Name: role + "." + capability,
				Aria: row.Label + " - " + capLabels[capability], Choice: choice})
		}
		rows = append(rows, row)
	}
	s.render(w, r, "listpermissions", http.StatusOK, map[string]any{"Title": tr(lang, "listPermissionsPopup-title"), "User": u,
		"Board": b, "ListID": docString(l, "_id"), "ListTitle": docString(l, "title"), "Rows": rows, "Saved": r.URL.Query().Get("saved")})
}

// saveListPermissions is the page's form: role.capability = "", allow or deny.
func (s *Server) saveListPermissions(w http.ResponseWriter, r *http.Request, u *User) {
	b, l, ok := s.listForAdmin(r, u)
	if !ok {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	exceptions := map[string]any{}
	for key, values := range r.PostForm {
		role, capability, found := cutLast(key, ".")
		if !found || len(values) == 0 || (values[0] != "allow" && values[0] != "deny") {
			continue
		}
		entry, _ := exceptions[role].(map[string]any)
		if entry == nil {
			entry = map[string]any{}
			exceptions[role] = entry
		}
		entry[capability] = values[0] == "allow"
	}
	if _, err := s.setListExceptions(r.Context(), u, docString(l, "_id"), exceptions); err != nil {
		var me *methodError
		if errors.As(err, &me) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/b/"+b.ID+"/"+b.Slug+"/lists/"+docString(l, "_id")+"/permissions?saved=1", http.StatusSeeOther)
}

func cutLast(s, sep string) (string, string, bool) {
	for i := len(s) - len(sep); i >= 0; i-- {
		if s[i:i+len(sep)] == sep {
			return s[:i], s[i+len(sep):], true
		}
	}
	return s, "", false
}
