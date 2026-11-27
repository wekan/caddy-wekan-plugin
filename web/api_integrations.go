package web

import (
	"errors"
	"net/http"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// A board's webhooks, WeKan's integrations (server/models/integrations.js).
func (s *Server) apiIntegrationRoutes(m *http.ServeMux) {
	base := "/api/boards/{board}/integrations"
	m.HandleFunc("GET "+base, s.api(s.apiIntegrations))
	m.HandleFunc("GET "+base+"/{id}", s.api(s.apiIntegration))
	m.HandleFunc("POST "+base, s.api(s.apiAddIntegration))
	m.HandleFunc("PUT "+base+"/{id}", s.api(s.apiEditIntegration))
	m.HandleFunc("DELETE "+base+"/{id}", s.api(s.apiDeleteIntegration))
	m.HandleFunc("POST "+base+"/{id}/activities", s.api(s.apiIntegrationActivities(true)))
	m.HandleFunc("DELETE "+base+"/{id}/activities", s.api(s.apiIntegrationActivities(false)))
	global := "/api/admin/webhooks"
	m.HandleFunc("POST "+global, s.api(s.globalWebhookWrite(s.addIntegration)))
	m.HandleFunc("PUT "+global+"/{id}", s.api(s.globalWebhookWrite(s.editIntegration)))
	m.HandleFunc("DELETE "+global+"/{id}", s.api(s.globalWebhookWrite(s.deleteIntegration)))
}

const globalWebhookBoard = "_global"

type scopedWebhookWrite func(http.ResponseWriter, *http.Request, *User, string)

func (s *Server) globalWebhookWrite(write scopedWebhookWrite) apiHandler {
	return func(w http.ResponseWriter, r *http.Request, u *User) {
		if !u.IsAdmin {
			apiError(w, http.StatusForbidden, "Forbidden")
			return
		}
		write(w, r, u, globalWebhookBoard)
	}
}

// integrationFields is what a reader gets (HookUrlBleed, REST sibling): a board
// or site admin everything but the token; anybody else only whether it is on
// and what it sends - a chat webhook's URL carries its secret in the path.
func integrationFields(b *Board, u *User) bson.M {
	if b.AdminOf(u.ID) || u.IsAdmin {
		return bson.M{"token": 0}
	}
	return bson.M{"boardId": 1, "enabled": 1, "activities": 1}
}

func (s *Server) apiIntegrations(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, false)
	if b == nil {
		return
	}
	cur, err := s.db.Collection("integrations").Find(r.Context(), bson.M{"boardId": b.ID}, options.Find().SetProjection(integrationFields(b, u)))
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []bson.M{}
	_ = cur.All(r.Context(), &out)
	writeJSON(w, http.StatusOK, out)
}

// integrationOf is the board's integration of the request, with the fields
// given, or 404 (#6862).
func (s *Server) integrationOf(w http.ResponseWriter, r *http.Request, b *Board, fields bson.M) bson.M {
	return s.integrationOfBoard(w, r, b.ID, fields)
}

func (s *Server) integrationOfBoard(w http.ResponseWriter, r *http.Request, boardID string, fields bson.M) bson.M {
	id := r.PathValue("id")
	var doc bson.M
	if !validID(id) || s.db.Collection("integrations").FindOne(r.Context(), bson.M{"_id": eq(id), "boardId": boardID},
		options.FindOne().SetProjection(fields)).Decode(&doc) != nil {
		apiError(w, http.StatusNotFound, "Integration not found")
		return nil
	}
	return doc
}

func (s *Server) apiIntegration(w http.ResponseWriter, r *http.Request, u *User) {
	if b := s.apiBoardFor(w, r, u, false); b != nil {
		if doc := s.integrationOf(w, r, b, integrationFields(b, u)); doc != nil {
			writeJSON(w, http.StatusOK, doc)
		}
	}
}

func validWebhookType(value string) bool {
	return value == "outgoing-webhooks" || value == "bidirectional-webhooks"
}

func (s *Server) apiAddIntegration(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardAdmin(w, r, u)
	if b == nil {
		return
	}
	s.addIntegration(w, r, u, b.ID)
}

func (s *Server) addIntegration(w http.ResponseWriter, r *http.Request, u *User, boardID string) {
	source := "rest:POST /api/boards/:boardId/integrations"
	if boardID == globalWebhookBoard {
		source = "rest:POST /api/admin/webhooks"
	}
	if s.refuseWebhookWrite(r, u, source, source+" for "+boardID+" while webhooks are turned off.") {
		webhooksOffAnswer(w)
		return
	}
	var req struct {
		URL   string `json:"url"`
		Title string `json:"title"`
		Token string `json:"token"`
		Type  string `json:"type"`
	}
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	req.Type = orDefault(req.Type, "outgoing-webhooks")
	if !validWebhookType(req.Type) || len(req.Title) > 10000 || len(req.Token) > 10000 {
		apiError(w, http.StatusBadRequest, "Invalid webhook fields")
		return
	}
	if reason := validateOutboundURL(r.Context(), req.URL); reason != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid-webhook-url", "reason": reason})
		return
	}
	now := time.Now().UTC()
	id := randomID()
	if _, err := s.db.Collection("integrations").InsertOne(r.Context(), bson.M{"_id": id, "userId": u.ID, "boardId": boardID,
		"url": req.URL, "enabled": true, "type": req.Type, "title": req.Title, "token": req.Token, "activities": bson.A{"all"}, "createdAt": now, "modifiedAt": now}); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"_id": id})
}

// stringList is a request's list of text, or false.
func stringList(v any) ([]string, bool) {
	arr, ok := v.([]any)
	if !ok || len(arr) > 1000 {
		return nil, false
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		s, ok := e.(string)
		if !ok || len(s) > 200 {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

func (s *Server) apiEditIntegration(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardAdmin(w, r, u)
	if b == nil {
		return
	}
	s.editIntegration(w, r, u, b.ID)
}

func (s *Server) editIntegration(w http.ResponseWriter, r *http.Request, u *User, boardID string) {
	source := "rest:PUT /api/boards/:boardId/integrations/:intId"
	if boardID == globalWebhookBoard {
		source = "rest:PUT /api/admin/webhooks/:id"
	}
	if s.refuseWebhookWrite(r, u, source, source+" for "+boardID+" while webhooks are turned off.") {
		webhooksOffAnswer(w)
		return
	}
	doc := s.integrationOfBoard(w, r, boardID, bson.M{"_id": 1})
	if doc == nil {
		return
	}
	var req map[string]any
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	set := bson.M{"modifiedAt": time.Now().UTC()}
	if v, ok := req["enabled"]; ok {
		set["enabled"] = isTrue(v)
	}
	for _, k := range []string{"title", "token"} {
		if v, ok := req[k]; ok {
			str, isString := v.(string)
			if !isString || len(str) > 10000 {
				apiError(w, http.StatusBadRequest, k+" is text")
				return
			}
			set[k] = str
		}
	}
	if v, ok := req["url"]; ok {
		str, _ := v.(string)
		if reason := validateOutboundURL(r.Context(), str); reason != "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid-webhook-url", "reason": reason})
			return
		}
		set["url"] = str
	}
	if v, ok := req["type"]; ok {
		kind, _ := v.(string)
		if !validWebhookType(kind) {
			apiError(w, http.StatusBadRequest, "Invalid webhook type")
			return
		}
		set["type"] = kind
	}
	if v, ok := req["activities"]; ok {
		list, ok := stringList(v)
		if !ok {
			apiError(w, http.StatusBadRequest, "activities is a list of activity types")
			return
		}
		set["activities"] = list
	}
	// #3695: how this webhook delivers - { webhook: {...} }, or null to
	// inherit; validated before ANY field is written, as WeKan's.
	update := bson.M{}
	if v, ok := req["notificationDelivery"]; ok {
		nd, err := normalizeDeliverySettings(v, []string{"webhook"})
		if err != nil {
			reason := err.Error()
			var me *methodError
			if errors.As(err, &me) {
				reason = me.Reason
			}
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid-notification-delivery", "reason": reason})
			return
		}
		if nd == nil {
			update["$unset"] = bson.M{"notificationDelivery": ""}
		} else {
			set["notificationDelivery"] = nd
		}
	}
	update["$set"] = set
	if _, err := s.db.Collection("integrations").UpdateOne(r.Context(), bson.M{"_id": doc["_id"], "boardId": boardID}, update); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"_id": doc["_id"]})
}

func (s *Server) apiDeleteIntegration(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardAdmin(w, r, u)
	if b == nil {
		return
	}
	s.deleteIntegration(w, r, u, b.ID)
}

func (s *Server) deleteIntegration(w http.ResponseWriter, r *http.Request, u *User, boardID string) {
	id := r.PathValue("id")
	if validID(id) {
		if _, err := s.db.Collection("integrations").DeleteOne(r.Context(), bson.M{"_id": eq(id), "boardId": boardID}); err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"_id": id})
}

// apiIntegrationActivities adds or removes activity types a webhook sends.
func (s *Server) apiIntegrationActivities(add bool) apiHandler {
	return func(w http.ResponseWriter, r *http.Request, u *User) {
		b := s.apiBoardAdmin(w, r, u)
		if b == nil {
			return
		}
		route := "DELETE /api/boards/:boardId/integrations/:intId/activities"
		if add {
			route = "POST /api/boards/:boardId/integrations/:intId/activities"
		}
		if s.refuseWebhookWrite(r, u, "rest:"+route, "REST "+route+" on board "+b.ID+" while webhooks are turned off.") {
			webhooksOffAnswer(w)
			return
		}
		doc := s.integrationOf(w, r, b, bson.M{"_id": 1})
		if doc == nil {
			return
		}
		var req map[string]any
		_ = decodeJSON(r, &req)
		list, ok := stringList(req["activities"])
		if !ok {
			apiError(w, http.StatusBadRequest, "activities is a list of activity types")
			return
		}
		update := bson.M{"$pullAll": bson.M{"activities": list}}
		if add {
			update = bson.M{"$addToSet": bson.M{"activities": bson.M{"$each": list}}}
		}
		if _, err := s.db.Collection("integrations").UpdateOne(r.Context(), bson.M{"_id": doc["_id"]}, update); err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, s.integrationOf(w, r, b, bson.M{"_id": 1, "activities": 1}))
	}
}
