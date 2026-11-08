package web

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// Rules at WeKan's addresses (server/models/rules.js): a rule is a document of
// rules naming one of triggers and one of actions, as WeKan's rules engine and
// its Rules page read them.
func (s *Server) apiRuleRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/boards/{board}/rules", s.api(s.apiRules))
	m.HandleFunc("GET /api/boards/{board}/rules/{rule}", s.api(s.apiRule))
	m.HandleFunc("POST /api/boards/{board}/rules", s.api(s.apiAddRule))
	m.HandleFunc("PUT /api/boards/{board}/rules/{rule}", s.api(s.apiEditRule))
	m.HandleFunc("DELETE /api/boards/{board}/rules/{rule}", s.api(s.apiDeleteRule))
}

// triggerMatchingFields is WeKan's server/triggersDef.js: a matching field a
// trigger leaves empty matches anything, "*".
var triggerMatchingFields = map[string][]string{
	"createCard":           {"boardId", "listName", "userId", "swimlaneName", "cardTitle"},
	"moveCard":             {"boardId", "listName", "oldListName", "userId", "swimlaneName", "cardTitle"},
	"archivedCard":         {"boardId", "userId", "cardTitle"},
	"addComment":           {"boardId", "userId", "cardTitle"},
	"restoredCard":         {"boardId", "userId", "cardTitle"},
	"joinMember":           {"boardId", "username", "userId"},
	"unjoinMember":         {"boardId", "username", "userId"},
	"joinAssignee":         {"boardId", "username", "userId"},
	"unjoinAssignee":       {"boardId", "username", "userId"},
	"addChecklist":         {"boardId", "checklistName", "userId"},
	"removeChecklist":      {"boardId", "checklistName", "userId"},
	"completeChecklist":    {"boardId", "checklistName", "userId"},
	"uncompleteChecklist":  {"boardId", "checklistName", "userId"},
	"addedChecklistItem":   {"boardId", "checklistItemName", "userId"},
	"removedChecklistItem": {"boardId", "checklistItemName", "userId"},
	"checkedItem":          {"boardId", "checklistItemName", "userId"},
	"uncheckedItem":        {"boardId", "checklistItemName", "userId"},
	"addAttachment":        {"boardId", "userId"},
	"deleteAttachment":     {"boardId", "userId"},
	"addedLabel":           {"boardId", "labelId", "userId"},
	"removedLabel":         {"boardId", "labelId", "userId"},
	"a-receivedAt":         {"boardId", "userId"},
	"a-startAt":            {"boardId", "userId"},
	"a-dueAt":              {"boardId", "userId"},
	"a-endAt":              {"boardId", "userId"},
}

// The fields WeKan strips from a trigger or action it is given, and from one it answers with.
var ruleStrip = map[string]bool{"_id": true, "boardId": true, "createdAt": true, "modifiedAt": true, "updatedAt": true}

// A trigger's or an action's own field: a plain name, never an operator or a path.
var ruleFieldName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)

// ruleComponent is a trigger or an action from a request: flat, named fields of
// text, numbers, booleans or lists of them - what WeKan's rule editor writes.
func ruleComponent(v any, kindField string) (bson.M, error) {
	in, ok := v.(map[string]any)
	if !ok || len(in) > 64 {
		return nil, errors.New("a trigger and an action are objects")
	}
	out := bson.M{}
	for k, val := range in {
		if ruleStrip[k] {
			continue
		}
		if !ruleFieldName.MatchString(k) {
			return nil, errors.New("a field of a trigger or an action is a plain name: " + k)
		}
		if !ruleValue(val, true) {
			return nil, errors.New("the value of " + k + " is text, a number, true or false, or a list of them")
		}
		out[k] = val
	}
	if kind, _ := out[kindField].(string); kind == "" {
		return nil, errors.New(kindField + " is required")
	}
	return out, nil
}

func ruleValue(v any, listOK bool) bool {
	switch x := v.(type) {
	case nil, bool, float64:
		return true
	case string:
		return len(x) <= 10000
	case []any:
		if !listOK || len(x) > 1000 {
			return false
		}
		for _, e := range x {
			if !ruleValue(e, false) {
				return false
			}
		}
		return true
	}
	return false
}

func normalizeTrigger(t bson.M) bson.M {
	kind, _ := t["activityType"].(string)
	for _, f := range triggerMatchingFields[kind] {
		if v, ok := t[f]; !ok || v == nil || v == "" {
			t[f] = "*"
		}
	}
	return t
}

// ruleButtonMetadata is WeKan's models/lib/ruleButtonMetadata.js.
func ruleButtonMetadata(trigger bson.M, title string) bson.M {
	if trigger["activityType"] != "button" {
		return bson.M{"$unset": bson.M{"buttonType": "", "buttonLabel": ""}}
	}
	typ, _ := trigger["buttonType"].(string)
	if typ != "board" {
		typ = "card"
	}
	label, _ := trigger["buttonLabel"].(string)
	if label == "" {
		label = title
	}
	if label == "" {
		label = "Rule"
	}
	return bson.M{"$set": bson.M{"buttonType": typ, "buttonLabel": label}}
}

// apiBoardAdmin is WeKan's checkBoardAdmin: a board admin or a site admin.
// Anybody else is refused and recorded (ManageBoardBleed), as in WeKan.
func (s *Server) apiBoardAdmin(w http.ResponseWriter, r *http.Request, u *User) *Board {
	b, err := s.boardFor(r.Context(), r.PathValue("board"), u)
	if err != nil && !u.IsAdmin {
		apiError(w, http.StatusForbidden, "Forbidden")
		return nil
	}
	if err != nil {
		var any Board
		if !validID(r.PathValue("board")) || s.db.Collection("boards").FindOne(r.Context(), bson.M{"_id": eq(r.PathValue("board"))}).Decode(&any) != nil {
			apiError(w, http.StatusNotFound, "Board not found")
			return nil
		}
		b = &any
	}
	if !u.IsAdmin && !b.AdminOf(u.ID) {
		s.recordSecurity(r, securityEvent{Key: "authz.manage-board", Action: "blocked", Source: "rest:board-admin", UserID: u.ID,
			Detail: "Board management denied because the caller is not an administrator."})
		apiError(w, http.StatusForbidden, "Forbidden")
		return nil
	}
	return b
}

func stripRule(doc bson.M) bson.M {
	if doc == nil {
		return nil
	}
	out := bson.M{}
	for k, v := range doc {
		if !ruleStrip[k] {
			out[k] = v
		}
	}
	return out
}

// serializeRule is WeKan's: the rule with its trigger and action embedded.
func (s *Server) serializeRule(ctx context.Context, rule bson.M) map[string]any {
	part := func(coll string, id any) bson.M {
		var d bson.M
		if s.db.Collection(coll).FindOne(ctx, bson.M{"_id": eq(idString(id))}).Decode(&d) != nil {
			return nil
		}
		return stripRule(d)
	}
	out := map[string]any{"_id": rule["_id"], "title": rule["title"], "enabled": rule["enabled"] != false,
		"trigger": part("triggers", rule["triggerId"]), "action": part("actions", rule["actionId"])}
	for field, coll := range map[string]string{"extraTriggerIds": "triggers", "extraActionIds": "actions"} {
		ids, _ := rule[field].(bson.A)
		if len(ids) == 0 {
			continue
		}
		var parts []bson.M
		for _, id := range ids {
			if p := part(coll, id); p != nil {
				parts = append(parts, p)
			}
		}
		out[map[string]string{"extraTriggerIds": "extraTriggers", "extraActionIds": "extraActions"}[field]] = parts
	}
	return out
}

func (s *Server) apiRules(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, false)
	if b == nil {
		return
	}
	cur, err := s.db.Collection("rules").Find(r.Context(), bson.M{"boardId": b.ID})
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var rules []bson.M
	if err := cur.All(r.Context(), &rules); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(rules))
	for _, rule := range rules {
		out = append(out, s.serializeRule(r.Context(), rule))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) ruleOf(w http.ResponseWriter, r *http.Request, b *Board) bson.M {
	id := r.PathValue("rule")
	var rule bson.M
	if !validID(id) || s.db.Collection("rules").FindOne(r.Context(), bson.M{"_id": eq(id), "boardId": b.ID}).Decode(&rule) != nil {
		apiError(w, http.StatusNotFound, "Rule not found")
		return nil
	}
	return rule
}

func (s *Server) apiRule(w http.ResponseWriter, r *http.Request, u *User) {
	if b := s.apiBoardFor(w, r, u, false); b != nil {
		if rule := s.ruleOf(w, r, b); rule != nil {
			writeJSON(w, http.StatusOK, s.serializeRule(r.Context(), rule))
		}
	}
}

// insertRule writes a rule with its trigger and action, as WeKan's POST does.
func (s *Server) insertRule(ctx context.Context, boardID, title string, trigger, action bson.M) (bson.M, error) {
	now := time.Now().UTC()
	if title == "" {
		title = "API rule"
	}
	trigger = normalizeTrigger(trigger)
	trigger["_id"], trigger["boardId"], trigger["createdAt"], trigger["updatedAt"] = randomID(), boardID, now, now
	action["_id"], action["boardId"], action["createdAt"], action["modifiedAt"] = randomID(), boardID, now, now
	if _, err := s.db.Collection("triggers").InsertOne(ctx, trigger); err != nil {
		return nil, err
	}
	if _, err := s.db.Collection("actions").InsertOne(ctx, action); err != nil {
		return nil, err
	}
	rule := bson.M{"_id": randomID(), "title": title, "triggerId": trigger["_id"], "actionId": action["_id"],
		"boardId": boardID, "enabled": true, "createdAt": now, "modifiedAt": now}
	if set, ok := ruleButtonMetadata(trigger, title)["$set"].(bson.M); ok {
		for k, v := range set {
			rule[k] = v
		}
	}
	if _, err := s.db.Collection("rules").InsertOne(ctx, rule); err != nil {
		return nil, err
	}
	return bson.M{"_id": rule["_id"], "triggerId": trigger["_id"], "actionId": action["_id"]}, nil
}

func (s *Server) apiAddRule(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardAdmin(w, r, u)
	if b == nil {
		return
	}
	var req map[string]any
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req["trigger"] == nil || req["action"] == nil {
		apiError(w, http.StatusBadRequest, "trigger and action are required")
		return
	}
	trigger, err := ruleComponent(req["trigger"], "activityType")
	if err == nil {
		var action bson.M
		if action, err = ruleComponent(req["action"], "actionType"); err == nil {
			title, _ := req["title"].(string)
			var ids bson.M
			if ids, err = s.insertRule(r.Context(), b.ID, title, trigger, action); err == nil {
				writeJSON(w, http.StatusOK, ids)
				return
			}
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	apiError(w, http.StatusBadRequest, err.Error())
}

// writeRuleComponent is WeKan's server/lib/ruleHistory.js with patch: the
// rule's own trigger or action is patched in place; one another rule shares
// is left alone, and a patched copy becomes this rule's.
func (s *Server) writeRuleComponent(ctx context.Context, rule bson.M, field string, fields bson.M) (any, error) {
	coll := map[string]string{"trigger": "triggers", "action": "actions"}[field]
	id := rule[field+"Id"]
	var existing bson.M
	if err := s.db.Collection(coll).FindOne(ctx, bson.M{"_id": eq(idString(id))}).Decode(&existing); err != nil && !errors.Is(err, mongo.ErrNoDocuments) {
		return nil, err
	}
	shared := false
	if existing != nil {
		n, _ := s.db.Collection("rules").CountDocuments(ctx, bson.M{field + "Id": id, "_id": bson.M{"$ne": rule["_id"]}})
		shared = n > 0
	}
	if existing == nil || shared {
		next := bson.M{}
		for k, v := range existing {
			next[k] = v
		}
		for k, v := range fields {
			next[k] = v
		}
		next["_id"] = randomID()
		_, err := s.db.Collection(coll).InsertOne(ctx, next)
		return next["_id"], err
	}
	_, err := s.db.Collection(coll).UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": fields})
	return id, err
}

func (s *Server) apiEditRule(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardAdmin(w, r, u)
	if b == nil {
		return
	}
	rule := s.ruleOf(w, r, b)
	if rule == nil {
		return
	}
	var req map[string]any
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	set := bson.M{"modifiedAt": time.Now().UTC()}
	title, _ := rule["title"].(string)
	if t, ok := req["title"].(string); ok {
		set["title"], title = t, t
	}
	if v, ok := req["enabled"]; ok && v != nil {
		set["enabled"] = v == true || v == "true"
	}
	var trigger, action bson.M
	var err error
	if req["trigger"] != nil {
		if trigger, err = ruleComponent(req["trigger"], "activityType"); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if req["action"] != nil {
		if action, err = ruleComponent(req["action"], "actionType"); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	update := bson.M{}
	if trigger != nil {
		trigger = normalizeTrigger(trigger)
		trigger["boardId"], trigger["updatedAt"] = b.ID, time.Now().UTC()
		id, err := s.writeRuleComponent(ctx, rule, "trigger", trigger)
		if err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
		set["triggerId"] = id
		meta := ruleButtonMetadata(trigger, title)
		if m, ok := meta["$set"].(bson.M); ok {
			for k, v := range m {
				set[k] = v
			}
		} else {
			update["$unset"] = meta["$unset"]
		}
	}
	if action != nil {
		action["boardId"], action["modifiedAt"] = b.ID, time.Now().UTC()
		id, err := s.writeRuleComponent(ctx, rule, "action", action)
		if err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
		set["actionId"] = id
	}
	update["$set"] = set
	if _, err := s.db.Collection("rules").UpdateOne(ctx, bson.M{"_id": rule["_id"]}, update); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"_id": rule["_id"]})
}

// removeRule is WeKan's removeRuleWithUnusedParts: the rule, and each of its
// triggers and actions no other rule uses.
func (s *Server) removeRule(ctx context.Context, rule bson.M) error {
	if _, err := s.db.Collection("rules").DeleteOne(ctx, bson.M{"_id": rule["_id"]}); err != nil {
		return err
	}
	for _, p := range []struct{ field, extra, coll string }{{"trigger", "extraTriggerIds", "triggers"}, {"action", "extraActionIds", "actions"}} {
		ids := []any{rule[p.field+"Id"]}
		if extra, ok := rule[p.extra].(bson.A); ok {
			ids = append(ids, extra...)
		}
		for _, id := range ids {
			if id == nil {
				continue
			}
			n, _ := s.db.Collection("rules").CountDocuments(ctx, bson.M{"$or": bson.A{bson.M{p.field + "Id": id}, bson.M{p.extra: id}}})
			if n == 0 {
				if _, err := s.db.Collection(p.coll).DeleteOne(ctx, bson.M{"_id": eq(idString(id))}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (s *Server) apiDeleteRule(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardAdmin(w, r, u)
	if b == nil {
		return
	}
	rule := s.ruleOf(w, r, b)
	if rule == nil {
		return
	}
	if err := s.removeRule(r.Context(), rule); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"_id": rule["_id"]})
}

// idString is an id read from a document, as text; anything else matches nothing.
func idString(v any) string {
	s, _ := v.(string)
	return s
}
