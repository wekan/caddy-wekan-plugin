package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Admin Panel / People's Organizations and Teams panes, as Node.js WeKan's
// peopleBody and server/models/org.js, team.js: a site admin makes an org or
// a team (newOrgPopup, newTeamPopup: setCreateOrg, setCreateTeam) - its
// short name nobody else's - and edits one (setOrgAllFields,
// setTeamAllFields), its new display name written into its members' own
// lists too. The *FromOidc methods are the OIDC sign-in's own, server-side:
// a client calling one is refused and recorded as OIDCBleed
// (GHSA-cv95-8h7c-2ffq).

type groupFields struct {
	DisplayName, Desc, ShortName, AutoAdd, Website string
	IsActive                                       bool
}

// groupSpec names a kind's fields.
type groupSpec struct{ coll, prefix, list, idField, taken string }

var groupSpecs = map[string]groupSpec{
	"org":  {"org", "org", "orgs", "orgId", "orgname-already-taken"},
	"team": {"team", "team", "teams", "teamId", "teamname-already-taken"},
}

func (s *Server) orgTeamAdminMethods() map[string]methodFunc {
	m := map[string]methodFunc{
		"setCreateOrg":     s.groupCreateMethod("org"),
		"setCreateTeam":    s.groupCreateMethod("team"),
		"setOrgAllFields":  s.groupEditMethod("org"),
		"setTeamAllFields": s.groupEditMethod("team"),
	}
	for _, name := range []string{"setCreateOrgFromOidc", "setCreateTeamFromOidc", "setOrgAllFieldsFromOidc", "setTeamAllFieldsFromOidc"} {
		m[name] = func(r *http.Request, u *User, args []json.RawMessage) (any, error) {
			s.recordSecurity(r, securityEvent{Key: "auth-race.oidc", Action: "blocked", Source: "ddp:oidc-internal-method", UserID: u.ID,
				Detail: "Client called an OIDC-login-only organization/team method."})
			return nil, &methodError{"not-authorized", "not-authorized"}
		}
	}
	return m
}

// groupArgs reads WeKan's argument order: display name, description, short
// name, [auto-add domain, website | website], active.
func groupArgs(kind string, args []json.RawMessage) (groupFields, error) {
	var f groupFields
	n := 5
	if kind == "org" {
		n = 6
	}
	if len(args) < n {
		return f, &methodError{"Match.Error", "Expected the fields"}
	}
	str := make([]string, n-1)
	for i := range str {
		if json.Unmarshal(args[i], &str[i]) != nil || len(str[i]) > 10000 {
			return f, &methodError{"Match.Error", "Expected String"}
		}
	}
	if json.Unmarshal(args[n-1], &f.IsActive) != nil {
		return f, &methodError{"Match.Error", "Expected Boolean"}
	}
	f.DisplayName, f.Desc, f.ShortName = str[0], str[1], str[2]
	if kind == "org" {
		f.AutoAdd, f.Website = str[3], str[4]
	} else {
		f.Website = str[3]
	}
	return f, nil
}

func (g groupFields) doc(kind string) bson.M {
	p := groupSpecs[kind].prefix
	d := bson.M{p + "DisplayName": g.DisplayName, p + "Desc": g.Desc, p + "ShortName": g.ShortName,
		p + "Website": g.Website, p + "IsActive": g.IsActive}
	if kind == "org" {
		d["orgAutoAddUsersWithDomainName"] = g.AutoAdd
	}
	return d
}

func (s *Server) createGroup(ctx context.Context, kind string, g groupFields) error {
	sp := groupSpecs[kind]
	if strings.TrimSpace(g.DisplayName) == "" || strings.TrimSpace(g.ShortName) == "" {
		return &methodError{"Match.Error", "a display name and a short name"}
	}
	if n, _ := s.db.Collection(sp.coll).CountDocuments(ctx, bson.M{sp.prefix + "ShortName": eq(g.ShortName)}); n > 0 {
		return &methodError{sp.taken, sp.taken}
	}
	doc := g.doc(kind)
	doc["_id"], doc["createdAt"] = randomID(), time.Now().UTC() // inserted, not a filter
	_, err := s.db.Collection(sp.coll).InsertOne(ctx, doc)
	return err
}

func (s *Server) editGroup(ctx context.Context, kind, id string, g groupFields) error {
	sp := groupSpecs[kind]
	var other bson.M
	if s.db.Collection(sp.coll).FindOne(ctx, bson.M{sp.prefix + "ShortName": eq(g.ShortName)}).Decode(&other) == nil && strOf(other["_id"]) != id {
		return &methodError{sp.taken, sp.taken}
	}
	res, err := s.db.Collection(sp.coll).UpdateOne(ctx, bson.M{"_id": eq(id)}, bson.M{"$set": g.doc(kind)})
	if err != nil || res.MatchedCount == 0 {
		return err
	}
	// setUsersOrgsOrgDisplayName: the members' own lists say the new name.
	// The whole list: FerretDB has no positional update.
	cur, err := s.db.Collection("users").Find(ctx, bson.M{sp.list + "." + sp.idField: eq(id)}, options.Find().SetProjection(bson.M{sp.list: 1}))
	if err != nil {
		return nil
	}
	var users []bson.M
	_ = cur.All(ctx, &users)
	for _, u := range users {
		list, _ := u[sp.list].(bson.A)
		for i, e := range list {
			d, ok := e.(bson.D)
			if !ok {
				continue
			}
			m := bson.M(docMap(d))
			if m[sp.idField] == id {
				m[sp.prefix+"DisplayName"] = g.DisplayName
				list[i] = m
			}
		}
		_, _ = s.db.Collection("users").UpdateOne(ctx, bson.M{"_id": eq(strOf(u["_id"]))}, bson.M{"$set": bson.M{sp.list: list}})
	}
	return nil
}

func (s *Server) groupCreateMethod(kind string) methodFunc {
	return func(r *http.Request, u *User, args []json.RawMessage) (any, error) {
		g, err := groupArgs(kind, args)
		if err != nil {
			return nil, err
		}
		if !u.IsAdmin {
			return nil, nil // WeKan's: nothing, for anybody else
		}
		return nil, s.createGroup(r.Context(), kind, g)
	}
}

func (s *Server) groupEditMethod(kind string) methodFunc {
	return func(r *http.Request, u *User, args []json.RawMessage) (any, error) {
		if len(args) < 1 {
			return nil, &methodError{"Match.Error", "Expected Object"}
		}
		g, err := groupArgs(kind, args[1:])
		if err != nil {
			return nil, err
		}
		if !u.IsAdmin {
			return nil, nil
		}
		id, ok := groupSelector(args[0])
		if !ok {
			return nil, &methodError{"Match.Error", "Expected {_id: String}"}
		}
		return nil, s.editGroup(r.Context(), kind, id, g)
	}
}

// The panes' forms.

func (s *Server) orgTeamAdminRoutes(m *http.ServeMux) {
	for _, kind := range []string{"org", "team"} {
		m.HandleFunc("POST /admin/"+kind+"s", s.page(s.groupFormPage(kind, false)))
		m.HandleFunc("POST /admin/"+kind+"s/{id}", s.page(s.groupFormPage(kind, true)))
	}
}

func (s *Server) groupFormPage(kind string, edit bool) pageHandler {
	return func(w http.ResponseWriter, r *http.Request, u *User) {
		if !u.IsAdmin {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		g := groupFields{DisplayName: strings.TrimSpace(r.FormValue("displayName")), Desc: r.FormValue("desc"),
			ShortName: strings.TrimSpace(r.FormValue("shortName")), AutoAdd: r.FormValue("autoAdd"),
			Website: r.FormValue("website"), IsActive: r.FormValue("isActive") == "true"}
		var err error
		if edit {
			id := r.PathValue("id")
			if !validID(id) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			err = s.editGroup(r.Context(), kind, id, g)
		} else {
			err = s.createGroup(r.Context(), kind, g)
		}
		to := "/admin/people?pane=" + kind + "s"
		if me, ok := err.(*methodError); ok {
			to += "&error=" + me.Code
		} else if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, to, http.StatusSeeOther)
	}
}

// groupRow is an org or a team as the pane shows it.
type groupRow struct {
	ID, DisplayName, Desc, ShortName, AutoAdd, Website, Kind string
	IsActive, SharedTemplates, Propagate, SyncFromAuth       bool
}

func (s *Server) groupRows(ctx context.Context, kind string) []groupRow {
	p := groupSpecs[kind].prefix
	cur, err := s.db.Collection(kind).Find(ctx, bson.M{}, options.Find().SetSort(bson.D{{Key: p + "DisplayName", Value: 1}}).SetLimit(1000))
	if err != nil {
		return nil
	}
	var docs []bson.M
	_ = cur.All(ctx, &docs)
	rows := []groupRow{}
	for _, d := range docs {
		rows = append(rows, groupRow{ID: strOf(d["_id"]), Kind: kind, DisplayName: strOf(d[p+"DisplayName"]), Desc: strOf(d[p+"Desc"]),
			ShortName: strOf(d[p+"ShortName"]), AutoAdd: strOf(d["orgAutoAddUsersWithDomainName"]), Website: strOf(d[p+"Website"]),
			IsActive: d[p+"IsActive"] == true, SharedTemplates: d[p+"SharedTemplates"] == true,
			Propagate: d[p+"PropagateMembersToBoards"] == true, SyncFromAuth: d[p+"SyncMembersFromAuth"] == true})
	}
	return rows
}
