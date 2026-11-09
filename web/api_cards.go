package web

// Card numbers, deleting with children, bulk delete, copy, board card settings,
// My Cards and deleting users - WeKan's REST API as its
// tests/playwright/specs/17-rest-api.e2e.js runs it.

import (
	"context"
	"net/http"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func (s *Server) apiCardRoutes(m *http.ServeMux) {
	m.HandleFunc("DELETE /api/boards/{board}/cards/bulk", s.api(s.apiBulkDeleteCards))
	m.HandleFunc("POST /api/boards/{board}/lists/{list}/cards/{card}/copy", s.api(s.apiCopyCard))
	m.HandleFunc("GET /api/boards/{board}/cardSettings", s.api(s.apiCardSettings))
	m.HandleFunc("PUT /api/boards/{board}/cardSettings", s.api(s.apiSetCardSettings))
	m.HandleFunc("GET /api/user/cards", s.api(s.apiUserCards))
	m.HandleFunc("DELETE /api/users/{user}", s.api(s.apiDeleteUser))
}

// nextCardNumber is the board's next card number, unique on the board (#5813):
// the board's counter, first raised to the highest number its cards have - cards
// Node.js WeKan or a seed wrote without moving the counter - then incremented in
// one database operation, so cards made at the same time never share one.
func (s *Server) nextCardNumber(ctx context.Context, boardID string) (int64, error) {
	var top struct {
		CardNumber int64 `bson:"cardNumber"`
	}
	_ = s.db.Collection("cards").FindOne(ctx, bson.M{"boardId": eq(boardID)},
		options.FindOne().SetSort(bson.D{{Key: "cardNumber", Value: -1}})).Decode(&top)
	boards := s.db.Collection("boards")
	if _, err := boards.UpdateOne(ctx, bson.M{"_id": eq(boardID)}, bson.M{"$max": bson.M{"cardNumber": top.CardNumber}}); err != nil {
		return 0, err
	}
	var b struct {
		CardNumber int64 `bson:"cardNumber"`
	}
	err := boards.FindOneAndUpdate(ctx, bson.M{"_id": eq(boardID)}, bson.M{"$inc": bson.M{"cardNumber": 1}},
		options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&b)
	return b.CardNumber, err
}

// DeleteCard removes a card with its checklists, checklist items and comments,
// and records one deleteCard activity - not one per child (#3252). The card's
// earlier activities stay, for board reports (#1598).
func (s *Server) DeleteCard(ctx context.Context, b *Board, u *User, c *Card) error {
	for _, coll := range []string{"checklistItems", "checklists", "card_comments"} {
		if _, err := s.db.Collection(coll).DeleteMany(ctx, bson.M{"cardId": c.ID}); err != nil {
			return err
		}
	}
	if _, err := s.db.Collection("cards").DeleteOne(ctx, bson.M{"_id": c.ID, "boardId": b.ID}); err != nil {
		return err
	}
	s.activity(ctx, u.ID, "deleteCard", bson.M{"boardId": b.ID, "cardId": c.ID, "cardTitle": c.Title,
		"listId": c.ListID, "swimlaneId": c.SwimlaneID})
	return nil
}

// apiBulkDeleteCards deletes up to 500 cards of the board (#4743).
func (s *Server) apiBulkDeleteCards(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, true)
	if b == nil {
		return
	}
	var req struct {
		CardIDs []string `json:"cardIds"`
	}
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	in, ok := inIDs(req.CardIDs)
	if !ok || len(req.CardIDs) == 0 || len(req.CardIDs) > 500 {
		apiError(w, http.StatusBadRequest, "1 to 500 card ids")
		return
	}
	cur, err := s.db.Collection("cards").Find(r.Context(), bson.M{"boardId": b.ID, "_id": in})
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var cards []Card
	_ = cur.All(r.Context(), &cards)
	// Every card's list first: a refused one stops the whole batch.
	for i := range cards {
		if s.refuseInList(r.Context(), b, cards[i].ListID, u, "write", "rest:card-bulk-delete") {
			apiError(w, http.StatusForbidden, "Forbidden")
			return
		}
	}
	deleted := make([]string, 0, len(cards))
	for i := range cards {
		if err := s.DeleteCard(r.Context(), b, u, &cards[i]); err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
		deleted = append(deleted, cards[i].ID)
	}
	s.hub.publish(b.ID, "board-changed")
	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted})
}

// apiCopyCard copies a card, with its checklists and their items, to a list of a
// board the caller may change; position 0 puts it first.
func (s *Server) apiCopyCard(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, false)
	if b == nil {
		return
	}
	c := s.cardOf(w, r, b)
	if c == nil {
		return
	}
	var req struct {
		ToBoardID    string `json:"toBoardId"`
		ToSwimlaneID string `json:"toSwimlaneId"`
		ToListID     string `json:"toListId"`
		Position     *int   `json:"position"`
		Title        string `json:"title"`
	}
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.ToBoardID == "" {
		req.ToBoardID = b.ID
	}
	to, err := s.boardFor(r.Context(), req.ToBoardID, u)
	if err != nil || !to.Writable(u, "write", nil) || s.refuseInList(r.Context(), to, req.ToListID, u, "write", "rest:card-copy-into") {
		apiError(w, http.StatusForbidden, "Forbidden")
		return
	}
	title := c.Title
	if t := strings.TrimSpace(req.Title); t != "" {
		title = t
	}
	n, err := s.AddCard(r.Context(), to, u, req.ToListID, req.ToSwimlaneID, title)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	set := bson.M{"description": c.Description}
	if req.Position != nil && *req.Position == 0 {
		var first struct {
			Sort float64 `bson:"sort"`
		}
		err := s.db.Collection("cards").FindOne(r.Context(), bson.M{"boardId": to.ID, "listId": eq(req.ToListID), "_id": bson.M{"$ne": n.ID}},
			options.FindOne().SetSort(bson.D{{Key: "sort", Value: 1}})).Decode(&first)
		if err == nil {
			set["sort"] = first.Sort - 1
		}
	}
	if to.ID == b.ID {
		set["labelIds"], set["members"], set["assignees"] = nonNil(c.LabelIDs), nonNil(c.Members), nonNil(c.Assignees)
	}
	if _, err := s.db.Collection("cards").UpdateOne(r.Context(), bson.M{"_id": n.ID}, bson.M{"$set": set}); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.copyChecklists(r.Context(), c.ID, n.ID)
	s.copySubtasksAndComments(r, u, c, n, to)
	s.hub.publish(to.ID, "board-changed")
	writeJSON(w, http.StatusOK, map[string]string{"_id": n.ID})
}

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func (s *Server) copyChecklists(ctx context.Context, from, to string) {
	cur, err := s.db.Collection("checklists").Find(ctx, bson.M{"cardId": from})
	if err != nil {
		return
	}
	var lists []bson.M
	_ = cur.All(ctx, &lists)
	for _, cl := range lists {
		newID := randomID()
		old := cl["_id"]
		cl["_id"], cl["cardId"], cl["createdAt"] = newID, to, time.Now().UTC()
		if _, err := s.db.Collection("checklists").InsertOne(ctx, cl); err != nil {
			continue
		}
		ic, err := s.db.Collection("checklistItems").Find(ctx, bson.M{"checklistId": old})
		if err != nil {
			continue
		}
		var items []bson.M
		_ = ic.All(ctx, &items)
		for _, it := range items {
			it["_id"], it["checklistId"], it["cardId"] = randomID(), newID, to
			_, _ = s.db.Collection("checklistItems").InsertOne(ctx, it)
		}
	}
}

// copySubtasksAndComments is the rest of WeKan's card copy: the subtasks the
// copier may read, re-homed onto the destination with the copy as their parent
// and their checklists copied (#5874, #3185), and the comments, on the
// destination board with their author and time kept (#5166).
func (s *Server) copySubtasksAndComments(r *http.Request, u *User, from, to *Card, toBoard *Board) {
	ctx := r.Context()
	cur, err := s.db.Collection("cards").Find(ctx, bson.M{"parentId": from.ID, "archived": bson.M{"$ne": true}})
	if err == nil {
		var subs []bson.M
		_ = cur.All(ctx, &subs)
		for _, sub := range subs {
			old, _ := sub["_id"].(string)
			bid, _ := sub["boardId"].(string)
			if sb, err := s.boardFor(ctx, bid, u); err != nil || (sb.member(u.ID) != nil && !sb.member(u.ID).ReadsWholeBoard()) {
				continue // a subtask the copier could not read is not copied
			}
			for _, k := range []string{"syncExternalId", "syncSourceType", "syncLastSource", "syncSourceKey", "syncLabelIds",
				"syncIssueState", "scrum", "scrumRevision"} {
				delete(sub, k)
			}
			id := randomID()
			sub["_id"], sub["parentId"], sub["boardId"], sub["swimlaneId"], sub["listId"] = id, to.ID, toBoard.ID, to.SwimlaneID, to.ListID
			if _, err := s.db.Collection("cards").InsertOne(ctx, sub); err == nil {
				s.copyChecklists(ctx, old, id)
			}
		}
	}
	cur, err = s.db.Collection("card_comments").Find(ctx, bson.M{"cardId": from.ID})
	if err == nil {
		var comments []bson.M
		_ = cur.All(ctx, &comments)
		for _, cm := range comments {
			cm["_id"], cm["cardId"], cm["boardId"] = randomID(), to.ID, toBoard.ID
			_, _ = s.db.Collection("card_comments").InsertOne(ctx, cm)
		}
	}
}

// cardSettingKeys are the board's allows* card settings (#3062).
func cardSettingKeys(doc bson.M) bson.M {
	out := bson.M{}
	for k, v := range doc {
		if strings.HasPrefix(k, "allows") {
			out[k] = v
		}
	}
	return out
}

var defaultCardSettings = []string{"allowsReceivedDate", "allowsStartDate", "allowsDueDate", "allowsEndDate",
	"allowsMembers", "allowsAssignee", "allowsLabels", "allowsChecklists", "allowsAttachments", "allowsComments",
	"allowsDescriptionTitle", "allowsDescriptionText", "allowsSubtasks", "allowsCardNumber", "allowsActivities"}

func (s *Server) apiCardSettings(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, false)
	if b == nil {
		return
	}
	var doc bson.M
	_ = s.db.Collection("boards").FindOne(r.Context(), bson.M{"_id": b.ID}).Decode(&doc)
	out := cardSettingKeys(doc)
	for _, k := range defaultCardSettings {
		if _, ok := out[k]; !ok {
			out[k] = k != "allowsCardNumber" // WeKan's defaults: all on but the card number
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// apiSetCardSettings sets allows* booleans - only those, only a board admin.
func (s *Server) apiSetCardSettings(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, true)
	if b == nil {
		return
	}
	if !b.AdminOf(u.ID) {
		apiError(w, http.StatusForbidden, "only a board admin changes the card settings")
		return
	}
	var req map[string]any
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	set := bson.M{}
	for k, v := range req {
		on, isBool := v.(bool)
		if !strings.HasPrefix(k, "allows") || !isBool || len(k) > 64 || strings.ContainsAny(k, ".$") {
			apiError(w, http.StatusBadRequest, "only allows* settings, true or false: "+k)
			return
		}
		set[k] = on
	}
	if len(set) == 0 {
		apiError(w, http.StatusBadRequest, "nothing to set")
		return
	}
	if _, err := s.db.Collection("boards").UpdateOne(r.Context(), bson.M{"_id": b.ID}, bson.M{"$set": set}); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.hub.publish(b.ID, "board-changed")
	writeJSON(w, http.StatusOK, cardSettingKeys(set))
}

// apiUserCards is My Cards: the cards the caller is a member or assignee of, on
// boards they are an active member of; ?due=true only those with a due date (#4815).
func (s *Server) apiUserCards(w http.ResponseWriter, r *http.Request, u *User) {
	boards, err := s.boardsOf(r.Context(), u.ID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	ids := make([]string, 0, len(boards))
	for _, b := range boards {
		ids = append(ids, b.ID)
	}
	in, _ := inIDs(ids)
	if in == nil {
		in = bson.M{"$in": []string{}}
	}
	filter := bson.M{"archived": false, "boardId": in,
		"$or": []bson.M{{"members": u.ID}, {"assignees": u.ID}}}
	if r.URL.Query().Get("due") == "true" {
		filter["dueAt"] = bson.M{"$ne": nil}
	}
	cur, err := s.db.Collection("cards").Find(r.Context(), filter, options.Find().SetLimit(1000))
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var cards []bson.M
	_ = cur.All(r.Context(), &cards)
	out := make([]map[string]any, 0, len(cards))
	for _, c := range cards {
		out = append(out, map[string]any{"_id": c["_id"], "title": c["title"], "boardId": c["boardId"],
			"listId": c["listId"], "swimlaneId": c["swimlaneId"], "dueAt": c["dueAt"]})
	}
	writeJSON(w, http.StatusOK, out)
}

// apiDeleteUser deletes a user - only a site admin, never themself.
func (s *Server) apiDeleteUser(w http.ResponseWriter, r *http.Request, u *User) {
	if !u.IsAdmin {
		apiError(w, http.StatusForbidden, "Forbidden")
		return
	}
	id := r.PathValue("user")
	if id == u.ID {
		apiError(w, http.StatusBadRequest, "an admin does not delete themself")
		return
	}
	if !validID(id) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "User not found"})
		return
	}
	res, err := s.db.Collection("users").DeleteOne(r.Context(), bson.M{"_id": eq(id)})
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if res.DeletedCount == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "User not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"_id": id})
}
