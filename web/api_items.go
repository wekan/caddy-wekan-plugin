package web

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// A checklist item and a comment one by one, at WeKan's addresses
// (server/models/checklistItems.js, server/models/cardComments.js).
func (s *Server) apiItemRoutes(m *http.ServeMux) {
	item := "/api/boards/{board}/cards/{card}/checklists/{checklist}/items/{item}"
	m.HandleFunc("GET "+item, s.api(s.apiChecklistItem))
	m.HandleFunc("PUT "+item, s.api(s.apiEditChecklistItem))
	m.HandleFunc("DELETE "+item, s.api(s.apiDeleteChecklistItem))
	comment := "/api/boards/{board}/cards/{card}/comments/{comment}"
	m.HandleFunc("GET "+comment, s.api(s.apiComment))
	m.HandleFunc("PUT "+comment, s.api(s.apiEditComment))
	m.HandleFunc("DELETE "+comment, s.api(s.apiDeleteComment))
	card := "/api/boards/{board}/lists/{list}/cards/{card}"
	m.HandleFunc("POST "+card+"/archive", s.api(s.apiArchiveCard(true)))
	m.HandleFunc("POST "+card+"/unarchive", s.api(s.apiArchiveCard(false)))
	m.HandleFunc("POST "+card+"/customFields/{field}", s.api(s.apiSetCardField))
	m.HandleFunc("GET /api/boards/{board}/cardsByCustomField/{field}/{value}", s.api(s.apiCardsByField))
	m.HandleFunc("GET /api/boards/{board}/swimlanes/{swimlane}/cards", s.api(s.apiSwimlaneCards))
	m.HandleFunc("GET /api/cards/{card}", s.api(s.apiCardByID))
}

// itemOf is the request's checklist item, when it is of that checklist, of that
// card, of that board - or 404.
func (s *Server) itemOf(w http.ResponseWriter, r *http.Request, b *Board) bson.M {
	card, cl, id := r.PathValue("card"), r.PathValue("checklist"), r.PathValue("item")
	var it bson.M
	var c Card
	if !validID(card) || !validID(cl) || !validID(id) ||
		s.db.Collection("checklistItems").FindOne(r.Context(), bson.M{"_id": eq(id), "cardId": eq(card), "checklistId": eq(cl)}).Decode(&it) != nil ||
		s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": eq(card), "boardId": b.ID}).Decode(&c) != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{})
		return nil
	}
	return it
}

func (s *Server) apiChecklistItem(w http.ResponseWriter, r *http.Request, u *User) {
	if b := s.apiBoardFor(w, r, u, false); b != nil {
		if it := s.itemOf(w, r, b); it != nil {
			writeJSON(w, http.StatusOK, it)
		}
	}
}

var isoDateTime = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,3})?(?:Z|[+-]\d{2}:\d{2})$`)

// parseDueAt is WeKan's server/lib/checklistDeadlines.js: absent, null, or an
// ISO 8601 date-time with its time zone.
func parseDueAt(req map[string]any) (set bool, due *time.Time, err error) {
	v, ok := req["dueAt"]
	if !ok {
		return false, nil, nil
	}
	if v == nil {
		return true, nil, nil
	}
	str, isString := v.(string)
	if !isString || !isoDateTime.MatchString(str) {
		return false, nil, errors.New("dueAt must be an ISO 8601 date-time with timezone, or null")
	}
	t, perr := time.Parse(time.RFC3339Nano, str)
	if perr != nil {
		return false, nil, errors.New("dueAt is not a valid date-time")
	}
	t = t.UTC()
	return true, &t, nil
}

// isTrue is WeKan's: the text "true", or a true boolean.
func isTrue(v any) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	str, _ := v.(string)
	return strings.ToLower(str) == "true"
}

func (s *Server) apiEditChecklistItem(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, true)
	if b == nil {
		return
	}
	it := s.itemOf(w, r, b)
	if it == nil {
		return
	}
	var req map[string]any
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	update := bson.M{}
	set := bson.M{}
	hasDue, due, err := parseDueAt(req)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if hasDue && due == nil {
		update["$unset"] = bson.M{"dueAt": ""}
	} else if hasDue {
		set["dueAt"] = *due
	}
	if v, ok := req["isFinished"]; ok {
		set["isFinished"] = isTrue(v)
	}
	if v, ok := req["title"]; ok {
		t, isString := v.(string)
		if !isString || strings.TrimSpace(t) == "" || len(t) > 10000 {
			apiError(w, http.StatusBadRequest, "a checklist item needs a title")
			return
		}
		set["title"] = t
	}
	if len(set) > 0 {
		update["$set"] = set
	}
	if len(update) > 0 {
		if _, err := s.db.Collection("checklistItems").UpdateOne(r.Context(), bson.M{"_id": it["_id"]}, update); err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.hub.publish(b.ID, "board-changed")
	}
	writeJSON(w, http.StatusOK, map[string]any{"_id": it["_id"]})
}

func (s *Server) apiDeleteChecklistItem(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, true)
	if b == nil {
		return
	}
	it := s.itemOf(w, r, b)
	if it == nil {
		return
	}
	if _, err := s.db.Collection("checklistItems").DeleteOne(r.Context(), bson.M{"_id": it["_id"]}); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.hub.publish(b.ID, "board-changed")
	writeJSON(w, http.StatusOK, map[string]any{"_id": it["_id"]})
}

func (s *Server) commentOf(w http.ResponseWriter, r *http.Request, b *Board) bson.M {
	card, id := r.PathValue("card"), r.PathValue("comment")
	var cm bson.M
	if !validID(card) || !validID(id) ||
		s.db.Collection("card_comments").FindOne(r.Context(), bson.M{"_id": eq(id), "cardId": eq(card), "boardId": b.ID}).Decode(&cm) != nil {
		apiError(w, http.StatusNotFound, "Comment not found")
		return nil
	}
	return cm
}

func (s *Server) apiComment(w http.ResponseWriter, r *http.Request, u *User) {
	if b := s.apiBoardFor(w, r, u, false); b != nil {
		if cm := s.commentOf(w, r, b); cm != nil {
			writeJSON(w, http.StatusOK, cm)
		}
	}
}

// mayChangeComment is WeKan's assertCanMutateComment: the author always; a
// board admin unless the board restricts comment editing (GHSA-pqr4-rxgp-hv2m).
// Trying another's comment is a canary (CommentBleed), never a disabled account.
func (s *Server) mayChangeComment(w http.ResponseWriter, r *http.Request, u *User, b *Board, cm bson.M, verb string) bool {
	author, _ := cm["userId"].(string)
	if author == "" || author == u.ID {
		return true
	}
	var raw bson.M
	_ = s.db.Collection("boards").FindOne(r.Context(), bson.M{"_id": b.ID}).Decode(&raw)
	if b.AdminOf(u.ID) && raw["restrictCommentEditing"] != true {
		return true
	}
	s.recordSecurity(r, securityEvent{Key: "authz.comment", Action: "detected", Source: "canary:comment.foreign-" + verb, UserID: u.ID,
		Detail: "tried to " + verb + " another user's comment over the REST API"})
	apiError(w, http.StatusForbidden, "You are not allowed to edit or delete another user's comment on this board.")
	return false
}

func (s *Server) apiEditComment(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, false)
	if b == nil {
		return
	}
	cm := s.commentOf(w, r, b)
	if cm == nil {
		return
	}
	var req struct {
		Comment string `json:"comment"`
	}
	_ = decodeJSON(r, &req)
	text := strings.TrimSpace(req.Comment)
	if text == "" || len(text) > 100000 {
		apiError(w, http.StatusBadRequest, "Missing required parameter: comment")
		return
	}
	if !s.mayChangeComment(w, r, u, b, cm, "edit") {
		return
	}
	if _, err := s.db.Collection("card_comments").UpdateOne(r.Context(), bson.M{"_id": cm["_id"]},
		bson.M{"$set": bson.M{"text": text, "modifiedAt": time.Now().UTC()}}); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var c Card
	_ = s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": eq(idString(cm["cardId"]))}).Decode(&c)
	s.activity(r.Context(), u.ID, "editComment", bson.M{"boardId": b.ID, "cardId": c.ID, "commentId": cm["_id"],
		"commentText": text, "listId": c.ListID, "swimlaneId": c.SwimlaneID})
	s.hub.publish(b.ID, "board-changed")
	writeJSON(w, http.StatusOK, map[string]any{"_id": cm["_id"]})
}

func (s *Server) apiDeleteComment(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, false)
	if b == nil {
		return
	}
	cm := s.commentOf(w, r, b)
	if cm == nil || !s.mayChangeComment(w, r, u, b, cm, "delete") {
		return
	}
	if _, err := s.db.Collection("card_comments").DeleteOne(r.Context(), bson.M{"_id": cm["_id"]}); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.hub.publish(b.ID, "board-changed")
	writeJSON(w, http.StatusOK, map[string]any{"_id": r.PathValue("card")}) // WeKan answers the card's id
}

// apiArchiveCard is WeKan's archive and unarchive of one card: write access,
// an open card archived, an archived one restored.
func (s *Server) apiArchiveCard(archive bool) apiHandler {
	return func(w http.ResponseWriter, r *http.Request, u *User) {
		b := s.apiBoardFor(w, r, u, true)
		if b == nil {
			return
		}
		card, list := r.PathValue("card"), r.PathValue("list")
		var c Card
		if !validID(card) || !validID(list) || s.db.Collection("cards").FindOne(r.Context(),
			bson.M{"_id": eq(card), "listId": eq(list), "boardId": b.ID, "archived": !archive}).Decode(&c) != nil {
			apiError(w, http.StatusNotFound, "Card not found")
			return
		}
		now := time.Now().UTC()
		update := bson.M{"$set": bson.M{"archived": true, "archivedAt": now, "modifiedAt": now}}
		answer := map[string]any{"_id": c.ID, "archived": true, "archivedAt": now}
		kind := "archivedCard"
		if !archive {
			update = bson.M{"$set": bson.M{"archived": false, "modifiedAt": now}, "$unset": bson.M{"archivedAt": ""}}
			answer, kind = map[string]any{"_id": c.ID, "archived": false}, "restoredCard"
		}
		if _, err := s.db.Collection("cards").UpdateOne(r.Context(), bson.M{"_id": c.ID}, update); err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.activity(r.Context(), u.ID, kind, bson.M{"boardId": b.ID, "cardId": c.ID, "listId": c.ListID, "swimlaneId": c.SwimlaneID})
		s.hub.publish(b.ID, "board-changed")
		writeJSON(w, http.StatusOK, answer)
	}
}

// apiSetCardField sets the value of one of the card's custom fields, as WeKan:
// only a field the card already has is changed.
func (s *Server) apiSetCardField(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, true)
	if b == nil {
		return
	}
	card, list, field := r.PathValue("card"), r.PathValue("list"), r.PathValue("field")
	var raw bson.M
	if !validID(card) || !validID(list) || s.db.Collection("cards").FindOne(r.Context(),
		bson.M{"_id": eq(card), "listId": eq(list), "boardId": b.ID, "archived": false}).Decode(&raw) != nil {
		apiError(w, http.StatusNotFound, "Card not found")
		return
	}
	var req map[string]any
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	value := req["value"]
	if !ruleValue(value, true) { // text, a number, true or false, or a list of them
		apiError(w, http.StatusBadRequest, "a custom field's value is text, a number, true or false, or a list of them")
		return
	}
	fields := bson.A{}
	arr, _ := raw["customFields"].(bson.A)
	for _, e := range arr {
		d, _ := e.(bson.D)
		m := docMap(d)
		if m["_id"] == field {
			m = map[string]any{"_id": m["_id"], "value": value}
		}
		fields = append(fields, m)
	}
	if _, err := s.db.Collection("cards").UpdateOne(r.Context(), bson.M{"_id": raw["_id"]}, bson.M{"$set": bson.M{"customFields": fields}}); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.hub.publish(b.ID, "board-changed")
	writeJSON(w, http.StatusOK, map[string]any{"_id": raw["_id"], "customFields": fields})
}

func (s *Server) findCards(w http.ResponseWriter, r *http.Request, filter bson.M) []bson.M {
	cur, err := s.db.Collection("cards").Find(r.Context(), filter, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}}))
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return nil
	}
	cards := []bson.M{}
	if err := cur.All(r.Context(), &cards); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return nil
	}
	return cards
}

// apiCardsByField: the board's open cards whose custom field has the value.
func (s *Server) apiCardsByField(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, false)
	if b == nil {
		return
	}
	field := r.PathValue("field")
	if !validID(field) {
		writeJSON(w, http.StatusOK, []bson.M{})
		return
	}
	if cards := s.findCards(w, r, bson.M{"boardId": b.ID, "archived": false,
		"customFields": bson.M{"$elemMatch": bson.M{"_id": eq(field), "value": eq(r.PathValue("value"))}}}); cards != nil {
		writeJSON(w, http.StatusOK, cards)
	}
}

// apiSwimlaneCards: a swimlane's open cards, in WeKan's short form.
func (s *Server) apiSwimlaneCards(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, false)
	if b == nil {
		return
	}
	sw := r.PathValue("swimlane")
	if !validID(sw) {
		writeJSON(w, http.StatusOK, []bson.M{})
		return
	}
	cards := s.findCards(w, r, bson.M{"boardId": b.ID, "swimlaneId": eq(sw), "archived": false})
	if cards == nil {
		return
	}
	out := make([]map[string]any, 0, len(cards))
	for _, c := range cards {
		out = append(out, map[string]any{"_id": c["_id"], "title": c["title"], "description": c["description"], "listId": c["listId"],
			"receivedAt": c["receivedAt"], "startAt": c["startAt"], "dueAt": c["dueAt"], "endAt": c["endAt"],
			"assignees": c["assignees"], "sort": c["sort"]})
	}
	writeJSON(w, http.StatusOK, out)
}

// cardWithChecklists is WeKan's: the card, and its checklists with their items.
func (s *Server) cardWithChecklists(r *http.Request, card bson.M) bson.M {
	cur, err := s.db.Collection("checklists").Find(r.Context(), bson.M{"cardId": card["_id"]}, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}}))
	lists := []bson.M{}
	if err == nil {
		_ = cur.All(r.Context(), &lists)
	}
	out := make([]bson.M, 0, len(lists))
	for _, cl := range lists {
		ic, err := s.db.Collection("checklistItems").Find(r.Context(), bson.M{"checklistId": cl["_id"], "cardId": card["_id"]},
			options.Find().SetSort(bson.D{{Key: "sort", Value: 1}}))
		items := []bson.M{}
		if err == nil {
			_ = ic.All(r.Context(), &items)
		}
		short := make([]bson.M, 0, len(items))
		for _, it := range items {
			short = append(short, bson.M{"_id": it["_id"], "title": it["title"], "isFinished": it["isFinished"], "dueAt": it["dueAt"]})
		}
		cl["items"] = short
		if cl["dueAt"] == nil {
			cl["dueAt"] = nil
		}
		out = append(out, cl)
	}
	card["checklists"] = out
	return card
}

// apiCardByID: a card by its id alone, for whoever may read its board.
func (s *Server) apiCardByID(w http.ResponseWriter, r *http.Request, u *User) {
	id := r.PathValue("card")
	var card bson.M
	if !validID(id) || s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": eq(id)}).Decode(&card) != nil {
		apiError(w, http.StatusNotFound, "Card not found")
		return
	}
	boardID, _ := card["boardId"].(string)
	r.SetPathValue("board", boardID)
	if b := s.apiBoardFor(w, r, u, false); b != nil {
		writeJSON(w, http.StatusOK, s.cardWithChecklists(r, card))
	}
}
