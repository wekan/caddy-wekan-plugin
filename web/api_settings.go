package web

import (
	"net/http"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// A board's domains, and Admin Panel's global settings, as Node.js WeKan
// (server/models/boards.js, server/models/settings.js).
func (s *Server) apiSettingsRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/boards/{board}/domains", s.api(s.apiBoardDomains))
	m.HandleFunc("POST /api/boards/{board}/domains", s.api(s.apiAddBoardDomain))
	m.HandleFunc("DELETE /api/boards/{board}/domains/{domain}", s.api(s.apiDeleteBoardDomain))
	m.HandleFunc("GET /api/settings", s.api(s.apiSettings))
	m.HandleFunc("PUT /api/settings", s.api(s.apiSetSettings))
}

// restSettingsFields are the settings a site admin reads and writes over REST:
// WeKan's REST_SETTINGS_FIELDS - never mailServer, which holds SMTP credentials.
var restSettingsFields = []string{
	"disableRegistration",
	"disableForgotPassword",
	"productName",
	"displayAuthenticationMethod",
	"defaultAuthenticationMethod",
	"spinnerName",
	"hideLogo",
	"hideCardCounterList",
	"hideBoardMemberList",
	"customLoginLogoImageUrl",
	"customLoginLogoLinkUrl",
	"customHelpLinkUrl",
	"textBelowCustomLoginLogo",
	"loginBackgroundColor",
	"automaticLinkedUrlSchemes",
	"customTopLeftCornerLogoImageUrl",
	"customTopLeftCornerLogoLinkUrl",
	"customTopLeftCornerLogoHeight",
	"oidcBtnText",
	"mailDomainName",
	"legalNotice",
	"customHeadEnabled",
	"customHeadMetaTags",
	"customHeadLinkTags",
	"customManifestEnabled",
	"customManifestContent",
	"customAssetLinksEnabled",
	"customAssetLinksContent",
	"accessibilityPageEnabled",
	"accessibilityTitle",
	"accessibilityContent",
	"supportPopupText",
	"supportPageEnabled",
	"supportPagePublic",
	"supportTitle",
	"supportPageText",
}

// domainProblem is WeKan's check of a board domain: text with a dot, no @, no space.
func domainProblem(d string) bool {
	return d == "" || !strings.Contains(d, ".") || strings.Contains(d, "@") || strings.ContainsAny(d, " \t\r\n\f\v") || len(d) > 253
}

type boardDomain struct {
	Domain   string `bson:"domain" json:"domain"`
	IsActive bool   `bson:"isActive" json:"isActive"`
}

func (s *Server) domainsOf(r *http.Request, boardID string) []boardDomain {
	var b struct {
		Domains []boardDomain `bson:"domains"`
	}
	_ = s.db.Collection("boards").FindOne(r.Context(), bson.M{"_id": eq(boardID)}).Decode(&b)
	if b.Domains == nil {
		return []boardDomain{}
	}
	return b.Domains
}

// apiBoardDomains: a site admin reads a board's domains.
func (s *Server) apiBoardDomains(w http.ResponseWriter, r *http.Request, u *User) {
	if !siteAdmin(w, u) {
		return
	}
	id := r.PathValue("board")
	if !validID(id) || s.db.Collection("boards").FindOne(r.Context(), bson.M{"_id": eq(id)}).Err() != nil {
		apiError(w, http.StatusNotFound, "Board not found")
		return
	}
	writeJSON(w, http.StatusOK, s.domainsOf(r, id))
}

func (s *Server) setDomains(w http.ResponseWriter, r *http.Request, b *Board, domains []boardDomain) {
	if _, err := s.db.Collection("boards").UpdateOne(r.Context(), bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"domains": domains}}); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.domainsOf(r, b.ID))
}

// apiAddBoardDomain: a board admin or a site admin adds a domain, once.
func (s *Server) apiAddBoardDomain(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardAdmin(w, r, u)
	if b == nil {
		return
	}
	var req map[string]any
	_ = decodeJSON(r, &req)
	raw, ok := req["domain"].(string)
	if !ok {
		apiError(w, http.StatusBadRequest, "Missing domain")
		return
	}
	d := strings.ToLower(strings.TrimSpace(raw))
	if domainProblem(d) {
		apiError(w, http.StatusBadRequest, "Invalid domain: "+raw)
		return
	}
	domains := s.domainsOf(r, b.ID)
	for _, x := range domains {
		if x.Domain == d {
			writeJSON(w, http.StatusOK, domains)
			return
		}
	}
	s.setDomains(w, r, b, append(domains, boardDomain{Domain: d, IsActive: true}))
}

func (s *Server) apiDeleteBoardDomain(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardAdmin(w, r, u)
	if b == nil {
		return
	}
	d := strings.ToLower(strings.TrimSpace(r.PathValue("domain")))
	kept := []boardDomain{}
	for _, x := range s.domainsOf(r, b.ID) {
		if x.Domain != d {
			kept = append(kept, x)
		}
	}
	s.setDomains(w, r, b, kept)
}

func pickSettings(doc bson.M) map[string]any {
	out := map[string]any{"_id": doc["_id"]}
	for _, f := range restSettingsFields {
		if v, ok := doc[f]; ok {
			out[f] = v
		}
	}
	return out
}

func (s *Server) apiSettings(w http.ResponseWriter, r *http.Request, u *User) {
	if !siteAdmin(w, u) {
		return
	}
	var doc bson.M
	_ = s.db.Collection("settings").FindOne(r.Context(), bson.M{}).Decode(&doc)
	writeJSON(w, http.StatusOK, pickSettings(doc))
}

// apiSetSettings: a site admin sets any of the REST settings - text, a number
// or true or false, as WeKan's settings schema has them.
func (s *Server) apiSetSettings(w http.ResponseWriter, r *http.Request, u *User) {
	if !siteAdmin(w, u) {
		return
	}
	var req map[string]any
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	set := bson.M{}
	for _, f := range restSettingsFields {
		v, ok := req[f]
		if !ok || v == nil {
			continue
		}
		switch x := v.(type) {
		case bool, float64:
		case string:
			if len(x) > 1<<20 {
				apiError(w, http.StatusBadRequest, f+" is too long")
				return
			}
		default:
			apiError(w, http.StatusBadRequest, f+" is text, a number, or true or false")
			return
		}
		set[f] = v
	}
	ctx := r.Context()
	var doc bson.M
	if s.db.Collection("settings").FindOne(ctx, bson.M{}).Decode(&doc) != nil {
		// WeKan makes its one settings document at start; Weddy when it is first written.
		doc = bson.M{"_id": randomID(), "createdAt": time.Now().UTC()} // inserted, not a filter
		if _, err := s.db.Collection("settings").InsertOne(ctx, doc); err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if len(set) > 0 {
		if _, err := s.db.Collection("settings").UpdateOne(ctx, bson.M{"_id": doc["_id"]}, bson.M{"$set": set}); err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	doc = bson.M{}
	_ = s.db.Collection("settings").FindOne(ctx, bson.M{}).Decode(&doc)
	writeJSON(w, http.StatusOK, pickSettings(doc))
}
