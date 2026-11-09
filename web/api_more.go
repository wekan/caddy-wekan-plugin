package web

import (
	"net/http"
	"slices"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// More of WeKan's REST API, in WeKan's shapes and collections: swimlanes and
// lists one by one, custom fields, checklists and their items, card comments
// and logging out. WeKan's own tests/playwright/specs/23-rest-api-more.e2e.js
// runs these against Weddy.
func (s *Server) apiMoreRoutes(m *http.ServeMux) {
	m.HandleFunc("POST /users/logout", s.api(s.apiLogout))
	m.HandleFunc("POST /api/boards/{board}/swimlanes", s.api(s.apiAddSwimlane))
	m.HandleFunc("GET /api/boards/{board}/swimlanes/{id}", s.api(s.apiGetOne("swimlanes")))
	m.HandleFunc("PUT /api/boards/{board}/swimlanes/{id}", s.api(s.apiRename("swimlanes")))
	m.HandleFunc("DELETE /api/boards/{board}/swimlanes/{id}", s.api(s.apiDelete("swimlanes")))
	m.HandleFunc("GET /api/boards/{board}/lists/{id}", s.api(s.apiGetOne("lists")))
	m.HandleFunc("PUT /api/boards/{board}/lists/{id}", s.api(s.apiRename("lists")))
	m.HandleFunc("DELETE /api/boards/{board}/lists/{id}", s.api(s.apiDelete("lists")))
	m.HandleFunc("GET /api/boards/{board}/custom-fields", s.api(s.apiCustomFields))
	m.HandleFunc("POST /api/boards/{board}/custom-fields", s.api(s.apiAddCustomField))
	m.HandleFunc("GET /api/boards/{board}/custom-fields/{id}", s.api(s.apiCustomField))
	m.HandleFunc("DELETE /api/boards/{board}/custom-fields/{id}", s.api(s.apiDeleteCustomField))
	m.HandleFunc("GET /api/boards/{board}/cards/{card}/checklists", s.api(s.apiChecklists))
	m.HandleFunc("POST /api/boards/{board}/cards/{card}/checklists", s.api(s.apiAddChecklist))
	m.HandleFunc("GET /api/boards/{board}/cards/{card}/checklists/{id}", s.api(s.apiChecklist))
	m.HandleFunc("PUT /api/boards/{board}/cards/{card}/checklists/{id}", s.api(s.apiEditChecklist))
	m.HandleFunc("DELETE /api/boards/{board}/cards/{card}/checklists/{id}", s.api(s.apiDeleteChecklist))
	m.HandleFunc("POST /api/boards/{board}/cards/{card}/checklists/{id}/items", s.api(s.apiAddChecklistItem))
	m.HandleFunc("GET /api/boards/{board}/cards/{card}/comments", s.api(s.apiComments))
	m.HandleFunc("POST /api/boards/{board}/cards/{card}/comments", s.api(s.apiAddComment))
}

// apiLogout ends the session of the presented token, as WeKan's #1437.
func (s *Server) apiLogout(w http.ResponseWriter, r *http.Request, u *User) {
	s.dropToken(r.Context(), strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
	writeJSON(w, http.StatusOK, map[string]string{"_id": u.ID})
}

func titleOf(w http.ResponseWriter, r *http.Request) (map[string]any, string, bool) {
	var req map[string]any
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return nil, "", false
	}
	title, _ := req["title"].(string)
	title = strings.TrimSpace(title)
	if title == "" || len(title) > 1000 {
		apiError(w, http.StatusBadRequest, "a title is needed")
		return nil, "", false
	}
	return req, title, true
}

func (s *Server) apiAddSwimlane(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, true)
	if b == nil {
		return
	}
	_, title, ok := titleOf(w, r)
	if !ok {
		return
	}
	now := time.Now().UTC()
	sw := bson.M{"_id": randomID(), "title": title, "boardId": b.ID, "archived": false, "type": "swimlane",
		"sort": s.nextSort(r.Context(), "swimlanes", bson.M{"boardId": b.ID}), "createdAt": now, "modifiedAt": now}
	if _, err := s.db.Collection("swimlanes").InsertOne(r.Context(), sw); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.activity(r.Context(), u.ID, "createSwimlane", bson.M{"boardId": b.ID, "swimlaneId": sw["_id"]})
	s.hub.publish(b.ID, "board-changed")
	writeJSON(w, http.StatusOK, map[string]any{"_id": sw["_id"]})
}

// oneOf is the document of coll with the path's id on the path's board.
func (s *Server) oneOf(w http.ResponseWriter, r *http.Request, b *Board, coll string) bson.M {
	id := r.PathValue("id")
	var doc bson.M
	if !validID(id) || s.db.Collection(coll).FindOne(r.Context(), bson.M{"_id": eq(id), "boardId": b.ID}).Decode(&doc) != nil {
		apiError(w, http.StatusNotFound, "not found")
		return nil
	}
	return doc
}

func (s *Server) apiGetOne(coll string) apiHandler {
	return func(w http.ResponseWriter, r *http.Request, u *User) {
		if b := s.apiBoardFor(w, r, u, false); b != nil {
			if doc := s.oneOf(w, r, b, coll); doc != nil {
				writeJSON(w, http.StatusOK, doc)
			}
		}
	}
}

func (s *Server) apiRename(coll string) apiHandler {
	return func(w http.ResponseWriter, r *http.Request, u *User) {
		b := s.apiBoardFor(w, r, u, true)
		if b == nil {
			return
		}
		doc := s.oneOf(w, r, b, coll)
		if doc == nil {
			return
		}
		_, title, ok := titleOf(w, r)
		if !ok {
			return
		}
		if _, err := s.db.Collection(coll).UpdateOne(r.Context(), bson.M{"_id": doc["_id"]},
			bson.M{"$set": bson.M{"title": title, "modifiedAt": time.Now().UTC()}}); err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.hub.publish(b.ID, "board-changed")
		writeJSON(w, http.StatusOK, map[string]any{"_id": doc["_id"]})
	}
}

// apiDelete removes a swimlane or a list - and its cards, as WeKan does.
func (s *Server) apiDelete(coll string) apiHandler {
	return func(w http.ResponseWriter, r *http.Request, u *User) {
		b := s.apiBoardFor(w, r, u, true)
		if b == nil {
			return
		}
		doc := s.oneOf(w, r, b, coll)
		if doc == nil {
			return
		}
		field := map[string]string{"swimlanes": "swimlaneId", "lists": "listId"}[coll]
		if _, err := s.db.Collection("cards").DeleteMany(r.Context(), bson.M{"boardId": b.ID, field: doc["_id"]}); err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if _, err := s.db.Collection(coll).DeleteOne(r.Context(), bson.M{"_id": doc["_id"]}); err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.hub.publish(b.ID, "board-changed")
		writeJSON(w, http.StatusOK, map[string]any{"_id": doc["_id"]})
	}
}

var customFieldTypes = map[string]bool{"text": true, "number": true, "date": true, "dropdown": true, "currency": true,
	"checkbox": true, "stringtemplate": true}

func (s *Server) apiCustomFields(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, false)
	if b == nil {
		return
	}
	cur, err := s.db.Collection("customFields").Find(r.Context(), bson.M{"boardIds": b.ID})
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	docs := []bson.M{}
	_ = cur.All(r.Context(), &docs)
	out := make([]map[string]any, 0, len(docs))
	for _, d := range docs {
		out = append(out, map[string]any{"_id": d["_id"], "name": d["name"], "type": d["type"]})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiAddCustomField(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, true)
	if b == nil {
		return
	}
	var req struct {
		Name                string         `json:"name"`
		Type                string         `json:"type"`
		Settings            map[string]any `json:"settings"`
		ShowOnCard          bool           `json:"showOnCard"`
		AutomaticallyOnCard bool           `json:"automaticallyOnCard"`
		ShowLabelOnMiniCard bool           `json:"showLabelOnMiniCard"`
		ShowSumAtTopOfList  bool           `json:"showSumAtTopOfList"`
	}
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 200 || !customFieldTypes[req.Type] {
		apiError(w, http.StatusBadRequest, "a name and a known type are needed")
		return
	}
	if req.Settings == nil {
		req.Settings = map[string]any{}
	}
	doc := bson.M{"_id": randomID(), "name": req.Name, "type": req.Type, "settings": cleanImported(req.Settings, ""), // never an operator or a path
		"showOnCard": req.ShowOnCard, "automaticallyOnCard": req.AutomaticallyOnCard,
		"showLabelOnMiniCard": req.ShowLabelOnMiniCard, "showSumAtTopOfList": req.ShowSumAtTopOfList,
		"boardIds": []string{b.ID}, "createdAt": time.Now().UTC()}
	if _, err := s.db.Collection("customFields").InsertOne(r.Context(), doc); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"_id": doc["_id"]})
}

func (s *Server) customFieldOf(w http.ResponseWriter, r *http.Request, b *Board) bson.M {
	id := r.PathValue("id")
	var doc bson.M
	if !validID(id) || s.db.Collection("customFields").FindOne(r.Context(), bson.M{"_id": eq(id), "boardIds": b.ID}).Decode(&doc) != nil {
		apiError(w, http.StatusNotFound, "no such custom field on this board")
		return nil
	}
	return doc
}

func (s *Server) apiCustomField(w http.ResponseWriter, r *http.Request, u *User) {
	if b := s.apiBoardFor(w, r, u, false); b != nil {
		if doc := s.customFieldOf(w, r, b); doc != nil {
			writeJSON(w, http.StatusOK, doc)
		}
	}
}

func (s *Server) apiDeleteCustomField(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, true)
	if b == nil {
		return
	}
	doc := s.customFieldOf(w, r, b)
	if doc == nil {
		return
	}
	if _, err := s.db.Collection("customFields").DeleteOne(r.Context(), bson.M{"_id": doc["_id"]}); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_, _ = s.db.Collection("cards").UpdateMany(r.Context(), bson.M{"boardId": b.ID},
		bson.M{"$pull": bson.M{"customFields": bson.M{"_id": doc["_id"]}}})
	writeJSON(w, http.StatusOK, map[string]any{"_id": doc["_id"]})
}

// cardOnBoard is the path's card, on the path's board.
func (s *Server) cardOnBoard(w http.ResponseWriter, r *http.Request, b *Board) *Card {
	id := r.PathValue("card")
	var c Card
	if !validID(id) || s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": eq(id), "boardId": b.ID}).Decode(&c) != nil {
		apiError(w, http.StatusNotFound, "Card not found or does not belong to the specified board")
		return nil
	}
	return &c
}

func (s *Server) checklistWithItems(r *http.Request, cl bson.M) bson.M {
	cur, err := s.db.Collection("checklistItems").Find(r.Context(), bson.M{"checklistId": cl["_id"]},
		options.Find().SetSort(bson.D{{Key: "sort", Value: 1}}))
	items := []bson.M{}
	if err == nil {
		_ = cur.All(r.Context(), &items)
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		out = append(out, map[string]any{"_id": it["_id"], "title": it["title"], "isFinished": it["isFinished"]})
	}
	cl["items"] = out
	return cl
}

func (s *Server) apiChecklists(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, false)
	if b == nil {
		return
	}
	c := s.cardOnBoard(w, r, b)
	if c == nil {
		return
	}
	cur, err := s.db.Collection("checklists").Find(r.Context(), bson.M{"cardId": c.ID}, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}}))
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	docs := []bson.M{}
	_ = cur.All(r.Context(), &docs)
	for i := range docs {
		docs[i] = s.checklistWithItems(r, docs[i])
	}
	writeJSON(w, http.StatusOK, docs)
}

func (s *Server) checklistOf(w http.ResponseWriter, r *http.Request, c *Card) bson.M {
	id := r.PathValue("id")
	var doc bson.M
	if !validID(id) || s.db.Collection("checklists").FindOne(r.Context(), bson.M{"_id": eq(id), "cardId": c.ID}).Decode(&doc) != nil {
		apiError(w, http.StatusNotFound, "Checklist not found or does not belong to the specified card")
		return nil
	}
	return doc
}

func (s *Server) apiChecklist(w http.ResponseWriter, r *http.Request, u *User) {
	if b := s.apiBoardFor(w, r, u, false); b != nil {
		if c := s.cardOnBoard(w, r, b); c != nil {
			if cl := s.checklistOf(w, r, c); cl != nil {
				writeJSON(w, http.StatusOK, s.checklistWithItems(r, cl))
			}
		}
	}
}

func (s *Server) insertItem(r *http.Request, c *Card, checklistID any, title string, sort int) (any, error) {
	id := randomID()
	_, err := s.db.Collection("checklistItems").InsertOne(r.Context(), bson.M{"_id": id, "cardId": c.ID,
		"checklistId": checklistID, "title": title, "sort": sort, "isFinished": false, "createdAt": time.Now().UTC()})
	return id, err
}

func (s *Server) apiAddChecklist(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, true)
	if b == nil {
		return
	}
	c := s.cardOnBoard(w, r, b)
	if c == nil {
		return
	}
	// A list that refuses the caller's role refuses the API too.
	if s.refuseInList(r.Context(), b, c.ListID, u, "write", "rest:checklist") {
		apiError(w, http.StatusForbidden, "Forbidden")
		return
	}
	req, title, ok := titleOf(w, r)
	if !ok {
		return
	}
	var items []string
	switch v := req["items"].(type) {
	case string:
		if v != "" {
			items = []string{v}
		}
	case []any:
		for _, it := range v {
			if s, ok := it.(string); ok && strings.TrimSpace(s) != "" {
				items = append(items, s)
			}
		}
	}
	if len(items) > 500 {
		apiError(w, http.StatusBadRequest, "at most 500 items")
		return
	}
	id := randomID()
	if _, err := s.db.Collection("checklists").InsertOne(r.Context(), bson.M{"_id": id, "cardId": c.ID, "title": title,
		"sort": 0, "createdAt": time.Now().UTC()}); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for i, it := range items {
		if _, err := s.insertItem(r, c, id, it, i); err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	s.activity(r.Context(), u.ID, "addChecklist", bson.M{"boardId": b.ID, "cardId": c.ID, "checklistId": id,
		"checklistName": title, "listId": c.ListID, "swimlaneId": c.SwimlaneID})
	s.hub.publish(b.ID, "board-changed")
	writeJSON(w, http.StatusOK, map[string]any{"_id": id})
}

func (s *Server) apiDeleteChecklist(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, true)
	if b == nil {
		return
	}
	c := s.cardOnBoard(w, r, b)
	if c == nil {
		return
	}
	// A list that refuses the caller's role refuses the API too.
	if s.refuseInList(r.Context(), b, c.ListID, u, "write", "rest:checklist") {
		apiError(w, http.StatusForbidden, "Forbidden")
		return
	}
	cl := s.checklistOf(w, r, c)
	if cl == nil {
		return
	}
	_, _ = s.db.Collection("checklistItems").DeleteMany(r.Context(), bson.M{"checklistId": cl["_id"]})
	if _, err := s.db.Collection("checklists").DeleteOne(r.Context(), bson.M{"_id": cl["_id"]}); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.hub.publish(b.ID, "board-changed")
	writeJSON(w, http.StatusOK, map[string]any{"_id": cl["_id"]})
}

// apiEditChecklist is WeKan's PUT of a checklist (#1037): its title, its due
// date (null clears it), or both - the other per-checklist settings are not
// on the REST API in WeKan either.
func (s *Server) apiEditChecklist(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, true)
	if b == nil {
		return
	}
	c := s.cardOnBoard(w, r, b)
	if c == nil {
		return
	}
	// A list that refuses the caller's role refuses the API too.
	if s.refuseInList(r.Context(), b, c.ListID, u, "write", "rest:checklist") {
		apiError(w, http.StatusForbidden, "Forbidden")
		return
	}
	cl := s.checklistOf(w, r, c)
	if cl == nil {
		return
	}
	var req map[string]any
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	hasDue, due, err := parseDueAt(req)
	rawTitle, hasTitle := req["title"]
	title, isString := rawTitle.(string)
	if err != nil || (hasTitle && (!isString || strings.TrimSpace(title) == "")) || (!hasTitle && !hasDue) {
		msg := "title or dueAt is required"
		if err != nil {
			msg = err.Error()
		}
		apiError(w, http.StatusBadRequest, msg)
		return
	}
	set, update := bson.M{}, bson.M{}
	if hasTitle {
		set["title"] = title
	}
	if hasDue {
		if due == nil {
			update["$unset"] = bson.M{"dueAt": ""}
		} else {
			set["dueAt"] = *due
		}
	}
	if len(set) > 0 {
		update["$set"] = set
	}
	if _, err := s.db.Collection("checklists").UpdateOne(r.Context(), bson.M{"_id": cl["_id"], "cardId": eq(c.ID)}, update); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.hub.publish(b.ID, "board-changed")
	writeJSON(w, http.StatusOK, map[string]any{"_id": cl["_id"]})
}

func (s *Server) apiAddChecklistItem(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, true)
	if b == nil {
		return
	}
	c := s.cardOnBoard(w, r, b)
	if c == nil {
		return
	}
	// A list that refuses the caller's role refuses the API too.
	if s.refuseInList(r.Context(), b, c.ListID, u, "write", "rest:checklist-item") {
		apiError(w, http.StatusForbidden, "Forbidden")
		return
	}
	cl := s.checklistOf(w, r, c)
	if cl == nil {
		return
	}
	_, title, ok := titleOf(w, r)
	if !ok {
		return
	}
	n, _ := s.db.Collection("checklistItems").CountDocuments(r.Context(), bson.M{"checklistId": cl["_id"]})
	id, err := s.insertItem(r, c, cl["_id"], title, int(n))
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.hub.publish(b.ID, "board-changed")
	writeJSON(w, http.StatusOK, map[string]any{"_id": id})
}

func (s *Server) apiComments(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, false)
	if b == nil {
		return
	}
	c := s.cardOnBoard(w, r, b)
	if c == nil {
		return
	}
	cur, err := s.db.Collection("card_comments").Find(r.Context(), bson.M{"boardId": b.ID, "cardId": c.ID},
		options.Find().SetSort(bson.D{{Key: "createdAt", Value: 1}}))
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	docs := []bson.M{}
	_ = cur.All(r.Context(), &docs)
	out := make([]map[string]any, 0, len(docs))
	for _, d := range docs {
		out = append(out, map[string]any{"_id": d["_id"], "comment": d["text"], "authorId": d["userId"], "createdAt": d["createdAt"]})
	}
	writeJSON(w, http.StatusOK, out)
}

// apiAddComment: the author is the caller (GHSA-6jr3-42jf-vhm5), never a body's authorId.
func (s *Server) apiAddComment(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardCan(w, r, u, "comment") // WeKan: the comment capability, not write
	if b == nil {
		return
	}
	c := s.cardOnBoard(w, r, b)
	if c == nil {
		return
	}
	// An assigned-only member comments only on their own cards (GHSA-r4gm-mjj9-g3x8):
	// another card answers as if it did not exist, and the attempt is recorded.
	if m := b.member(u.ID); m != nil && m.AssignedOnly() && !u.IsAdmin && !slices.Contains(c.Assignees, u.ID) {
		s.recordSecurity(r, securityEvent{Key: "authz.assigned", Action: "blocked", Source: "rest:comment-insert",
			UserID: u.ID, Detail: "Assigned-only member commented on a card not assigned to them."})
		apiError(w, http.StatusNotFound, "Card not found")
		return
	}
	// A list that refuses the caller's role a comment refuses the API too.
	if s.refuseInList(r.Context(), b, c.ListID, u, "comment", "rest:comment-insert") {
		apiError(w, http.StatusForbidden, "Forbidden")
		return
	}
	var req struct {
		Comment string `json:"comment"`
	}
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.Comment) == "" || len(req.Comment) > 100000 {
		apiError(w, http.StatusBadRequest, "a comment is needed")
		return
	}
	now := time.Now().UTC()
	id := randomID()
	if _, err := s.db.Collection("card_comments").InsertOne(r.Context(), bson.M{"_id": id, "boardId": b.ID, "cardId": c.ID,
		"text": req.Comment, "userId": u.ID, "createdAt": now, "modifiedAt": now}); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.activity(r.Context(), u.ID, "addComment", bson.M{"boardId": b.ID, "cardId": c.ID, "commentId": id,
		"listId": c.ListID, "swimlaneId": c.SwimlaneID})
	s.hub.publish(b.ID, "board-changed")
	writeJSON(w, http.StatusOK, map[string]any{"_id": id})
}
