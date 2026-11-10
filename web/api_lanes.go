package web

import (
	"context"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Lists and swimlanes archived, restored, copied and moved, and a card moved to
// another board, as Node.js WeKan does (server/models/lists.js,
// server/models/swimlanes.js, server/models/cards.js, models/cards.js move).
func (s *Server) apiLaneRoutes(m *http.ServeMux) {
	list := "/api/boards/{board}/lists/{list}"
	m.HandleFunc("POST "+list+"/archive", s.api(s.apiArchiveList(true)))
	m.HandleFunc("POST "+list+"/unarchive", s.api(s.apiArchiveList(false)))
	m.HandleFunc("POST "+list+"/copy", s.api(s.apiCopyList))
	m.HandleFunc("POST "+list+"/move", s.api(s.apiMoveList))
	sw := "/api/boards/{board}/swimlanes/{swimlane}"
	m.HandleFunc("POST "+sw+"/copy", s.api(s.apiCopySwimlane))
	m.HandleFunc("POST "+sw+"/move", s.api(s.apiMoveSwimlane))
}

// computeSortForIndex is WeKan's: the sort that puts an item at a 0-based
// position among siblings sorted by sort.
func computeSortForIndex(siblings []float64, position int) float64 {
	if len(siblings) == 0 {
		return 0
	}
	switch {
	case position <= 0:
		return siblings[0] - 1
	case position >= len(siblings):
		return siblings[len(siblings)-1] + 1
	}
	return (siblings[position-1] + siblings[position]) / 2
}

// reposition puts a list or swimlane at a 0-based position of its board.
func (s *Server) reposition(ctx context.Context, coll, id, boardID string, position *float64) {
	if position == nil {
		return
	}
	cur, err := s.db.Collection(coll).Find(ctx, bson.M{"boardId": eq(boardID), "archived": false},
		options.Find().SetSort(bson.D{{Key: "sort", Value: 1}}))
	if err != nil {
		return
	}
	var docs []struct {
		ID   string  `bson:"_id"`
		Sort float64 `bson:"sort"`
	}
	_ = cur.All(ctx, &docs)
	sorts := []float64{}
	for _, d := range docs {
		if d.ID != id { // the siblings, not the item itself
			sorts = append(sorts, d.Sort)
		}
	}
	_, _ = s.db.Collection(coll).UpdateOne(ctx, bson.M{"_id": eq(id)},
		bson.M{"$set": bson.M{"sort": computeSortForIndex(sorts, int(math.Floor(*position)))}})
}

// writableBoard is a destination board the caller may write, by its id from a request.
func (s *Server) writableBoard(ctx context.Context, id string, u *User) *Board {
	b, err := s.boardFor(ctx, id, u)
	if err != nil || !b.Writable(u, "write", nil) {
		return nil
	}
	return b
}

// crossBoardLabels is WeKan's planCrossBoardLabels: a card's labels on another
// board are that board's labels of the same names; a missing one is created
// only by an admin of that board.
func crossBoardLabels(from, to *Board, labelIDs []string, canCreate bool) (mapped []string, create []Label) {
	byName := map[string]string{}
	for _, l := range to.Labels {
		if l.Name != "" {
			if _, ok := byName[l.Name]; !ok {
				byName[l.Name] = l.ID
			}
		}
	}
	mapped = []string{}
	for _, l := range from.Labels {
		if l.Name == "" || !slices.Contains(labelIDs, l.ID) {
			continue
		}
		if id, ok := byName[l.Name]; ok {
			if !slices.Contains(mapped, id) {
				mapped = append(mapped, id)
			}
		} else if canCreate {
			n := Label{ID: randomString(idAlphabet, 6), Name: l.Name, Color: l.Color}
			byName[l.Name] = n.ID
			create = append(create, n)
			mapped = append(mapped, n.ID)
		}
	}
	return mapped, create
}

// onBoard is a card's ids of one field that are active members of b.
func onBoard(b *Board, v any) bson.A {
	out := bson.A{}
	arr, _ := v.(bson.A)
	for _, e := range arr {
		if id, ok := e.(string); ok && b.MemberOf(id) {
			out = append(out, id)
		}
	}
	return out
}

func stringsOf(v any) []string {
	out := []string{}
	arr, _ := v.(bson.A)
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// boardFields keeps the custom field values of fields of the board (#6800).
func (s *Server) boardFields(ctx context.Context, boardID string, v any) bson.A {
	out := bson.A{}
	arr, _ := v.(bson.A)
	for _, e := range arr {
		d, _ := e.(bson.D)
		m := docMap(d)
		id, _ := m["_id"].(string)
		if id != "" && s.db.Collection("customFields").FindOne(ctx, bson.M{"_id": eq(id), "boardIds": eq(boardID)}).Err() == nil {
			out = append(out, bson.M{"_id": id, "value": m["value"]}) // inserted, not a filter
		}
	}
	return out
}

// topSort is the sort that puts a card on top of a list (WeKan's computeTopSort).
func (s *Server) topSort(ctx context.Context, boardID, listID string) float64 {
	var first struct {
		Sort float64 `bson:"sort"`
	}
	if s.db.Collection("cards").FindOne(ctx, bson.M{"boardId": eq(boardID), "listId": eq(listID), "archived": false},
		options.FindOne().SetSort(bson.D{{Key: "sort", Value: 1}})).Decode(&first) != nil {
		return 0
	}
	return first.Sort - 1
}

// crossBoardFields is what a card becomes on another board: its labels by name,
// a card number there, the custom fields of that board, the members and
// watchers who are members there.
func (s *Server) crossBoardFields(ctx context.Context, u *User, card bson.M, from, to *Board) (bson.M, error) {
	labels, create := crossBoardLabels(from, to, stringsOf(card["labelIds"]), to.AdminOf(u.ID) || u.IsAdmin)
	if len(create) > 0 {
		if _, err := s.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": to.ID}, bson.M{"$push": bson.M{"labels": bson.M{"$each": create}}}); err != nil {
			return nil, err
		}
		to.Labels = append(to.Labels, create...)
	}
	number, err := s.nextCardNumber(ctx, to.ID)
	if err != nil {
		return nil, err
	}
	return bson.M{"labelIds": labels, "cardNumber": number, "customFields": s.boardFields(ctx, to.ID, card["customFields"]),
		"members": onBoard(to, card["members"]), "watchers": onBoard(to, card["watchers"])}, nil
}

// moveCardTo moves a card to a list and swimlane of another board, on top of
// the list, and its comments, checklists and attachments with it (#6793).
func (s *Server) moveCardTo(ctx context.Context, u *User, card bson.M, from, to *Board, swimlaneID, listID string) error {
	set, err := s.crossBoardFields(ctx, u, card, from, to)
	if err != nil {
		return err
	}
	set["boardId"], set["swimlaneId"], set["listId"] = to.ID, swimlaneID, listID
	set["sort"], set["modifiedAt"] = s.topSort(ctx, to.ID, listID), time.Now().UTC()
	id, _ := card["_id"].(string)
	if _, err := s.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": eq(id), "boardId": from.ID}, bson.M{"$set": set}); err != nil {
		return err
	}
	byCard := bson.M{"cardId": eq(id)}
	_, _ = s.db.Collection("card_comments").UpdateMany(ctx, byCard, bson.M{"$set": bson.M{"boardId": to.ID}})
	_, _ = s.db.Collection("checklists").UpdateMany(ctx, bson.M{"cardId": eq(id), "boardId": bson.M{"$exists": true}}, bson.M{"$set": bson.M{"boardId": to.ID}})
	_, _ = s.db.Collection("attachments").UpdateMany(ctx, bson.M{"meta.cardId": eq(id)},
		bson.M{"$set": bson.M{"meta.boardId": to.ID, "meta.swimlaneId": swimlaneID, "meta.listId": listID}})
	title, _ := card["title"].(string)
	s.activity(ctx, u.ID, "moveCard", bson.M{"boardId": to.ID, "cardId": id, "cardTitle": title, "listId": listID, // inserted, not a filter
		"swimlaneId": swimlaneID, "oldBoardId": from.ID, "oldListId": card["listId"], "oldSwimlaneId": card["swimlaneId"]})
	return nil
}

// copyCardTo is WeKan's Card.copy: the whole card, on the list and swimlane of
// the board, with its checklists, attachments, the subtasks the copier may read
// and its comments.
func (s *Server) copyCardTo(r *http.Request, u *User, card bson.M, from, to *Board, swimlaneID, listID string) (string, error) {
	ctx := r.Context()
	doc := bson.M{}
	for k, v := range card {
		doc[k] = v
	}
	for _, k := range []string{"_id", "scrum", "scrumRevision", "linkedId"} {
		delete(doc, k)
	}
	if from.ID != to.ID {
		set, err := s.crossBoardFields(ctx, u, card, from, to)
		if err != nil {
			return "", err
		}
		for k, v := range set {
			doc[k] = v
		}
		deps := bson.A{}
		for _, d := range normalizeDependencies(card["cardDependencies"]) {
			if s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(d.CardID), "boardId": to.ID}).Err() == nil {
				deps = append(deps, d)
			}
		}
		doc["cardDependencies"] = deps
	}
	id := randomID()
	now := time.Now().UTC()
	doc["_id"], doc["boardId"], doc["swimlaneId"], doc["listId"] = id, to.ID, swimlaneID, listID
	doc["createdAt"], doc["modifiedAt"], doc["userId"] = now, now, u.ID
	if _, err := s.db.Collection("cards").InsertOne(ctx, doc); err != nil {
		return "", err
	}
	old, _ := card["_id"].(string)
	s.copyChecklists(ctx, old, id)
	var n, src Card
	_ = s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&n)
	_ = s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(old)}).Decode(&src)
	s.copyCardAttachments(r, u, from, &src, to, &n)
	s.copySubtasksAndComments(r, u, &src, &n, to)
	return id, nil
}

func (s *Server) apiArchiveList(archive bool) apiHandler {
	return func(w http.ResponseWriter, r *http.Request, u *User) {
		b := s.apiBoardFor(w, r, u, true)
		if b == nil {
			return
		}
		id := r.PathValue("list")
		var l List
		if !validID(id) || s.db.Collection("lists").FindOne(r.Context(), bson.M{"_id": eq(id), "boardId": b.ID, "archived": !archive}).Decode(&l) != nil {
			apiError(w, http.StatusNotFound, "List not found")
			return
		}
		set := bson.M{"archived": archive}
		if archive {
			set["archivedAt"] = time.Now().UTC()
		}
		if _, err := s.db.Collection("lists").UpdateOne(r.Context(), bson.M{"_id": l.ID}, bson.M{"$set": set}); err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
		kind := "archivedList"
		if !archive {
			kind = "restoredList"
		}
		s.activity(r.Context(), u.ID, kind, bson.M{"boardId": b.ID, "listId": l.ID})
		s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
		writeJSON(w, http.StatusOK, map[string]any{"_id": l.ID, "archived": archive})
	}
}

// laneRequest is the body of a list's or swimlane's copy and move.
type laneRequest struct {
	ToBoardID    string   `json:"toBoardId"`
	ToSwimlaneID string   `json:"toSwimlaneId"`
	Position     *float64 `json:"position"`
	Title        string   `json:"title"`
}

// laneSource decodes the body, and finds the source board and the destination.
func (s *Server) laneSource(w http.ResponseWriter, r *http.Request, u *User) (*Board, *Board, *laneRequest) {
	b := s.apiBoardFor(w, r, u, true)
	if b == nil {
		return nil, nil, nil
	}
	var req laneRequest
	if err := decodeJSON(r, &req); err != nil && r.ContentLength > 0 {
		apiError(w, http.StatusBadRequest, err.Error())
		return nil, nil, nil
	}
	to := b
	if req.ToBoardID != "" && req.ToBoardID != b.ID {
		if to = s.writableBoard(r.Context(), req.ToBoardID, u); to == nil {
			apiError(w, http.StatusForbidden, "Forbidden")
			return nil, nil, nil
		}
	}
	if req.ToSwimlaneID != "" && (!validID(req.ToSwimlaneID) ||
		s.db.Collection("swimlanes").FindOne(r.Context(), bson.M{"_id": eq(req.ToSwimlaneID), "boardId": to.ID}).Err() != nil) {
		apiError(w, http.StatusBadRequest, "toSwimlaneId is not a swimlane of the destination board")
		return nil, nil, nil
	}
	return b, to, &req
}

func (s *Server) listOf(w http.ResponseWriter, r *http.Request, b *Board) bson.M {
	id := r.PathValue("list")
	var l bson.M
	if !validID(id) || s.db.Collection("lists").FindOne(r.Context(), bson.M{"_id": eq(id), "boardId": b.ID}).Decode(&l) != nil {
		apiError(w, http.StatusNotFound, "List not found")
		return nil
	}
	return l
}

// laneCards is the open cards of a list (and, when swimlane is set, of a swimlane).
func (s *Server) laneCards(ctx context.Context, filter bson.M) []bson.M {
	filter["archived"] = false
	cur, err := s.db.Collection("cards").Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}}))
	cards := []bson.M{}
	if err == nil {
		_ = cur.All(ctx, &cards)
	}
	return cards
}

// apiCopyList is WeKan's list copy: on another board into its list of the same
// title when there is one, else a new list; the cards copied with it.
func (s *Server) apiCopyList(w http.ResponseWriter, r *http.Request, u *User) {
	b, to, req := s.laneSource(w, r, u)
	if b == nil {
		return
	}
	l := s.listOf(w, r, b)
	if l == nil {
		return
	}
	ctx := r.Context()
	listID := ""
	if to.ID != b.ID {
		var existing List
		if s.db.Collection("lists").FindOne(ctx, bson.M{"boardId": to.ID, "title": l["title"], "archived": false}).Decode(&existing) == nil {
			listID = existing.ID
		}
	}
	if listID == "" {
		doc := bson.M{}
		for k, v := range l {
			doc[k] = v
		}
		delete(doc, "scrum")
		delete(doc, "scrumRevision")
		listID = randomID()
		doc["_id"], doc["boardId"], doc["swimlaneId"], doc["createdAt"] = listID, to.ID, req.ToSwimlaneID, time.Now().UTC()
		if _, err := s.db.Collection("lists").InsertOne(ctx, doc); err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	keepSwimlanes := to.ID == b.ID && req.ToSwimlaneID == ""
	firstSwimlane := s.firstSwimlane(ctx, to.ID)
	for _, c := range s.laneCards(ctx, bson.M{"listId": l["_id"]}) {
		sw := req.ToSwimlaneID
		if keepSwimlanes {
			sw, _ = c["swimlaneId"].(string)
		}
		if sw == "" {
			sw = firstSwimlane
		}
		if _, err := s.copyCardTo(r, u, c, b, to, sw, listID); err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	s.reposition(ctx, "lists", listID, to.ID, req.Position)
	s.hub.publishFrom(to.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	writeJSON(w, http.StatusOK, map[string]string{"_id": listID})
}

func (s *Server) firstSwimlane(ctx context.Context, boardID string) string {
	var sw Swimlane
	_ = s.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": eq(boardID), "archived": false},
		options.FindOne().SetSort(bson.D{{Key: "sort", Value: 1}})).Decode(&sw)
	return sw.ID
}

// apiMoveList is WeKan's: on the same board a reposition (and a swimlane); to
// another board into its list of the same title or a new one, every card with it.
func (s *Server) apiMoveList(w http.ResponseWriter, r *http.Request, u *User) {
	b, to, req := s.laneSource(w, r, u)
	if b == nil {
		return
	}
	l := s.listOf(w, r, b)
	if l == nil {
		return
	}
	if err := s.moveListTo(r.Context(), u, b, to, l, req); err != nil {
		apiError(w, err.status, err.msg)
		return
	}
	id, _ := l["_id"].(string)
	writeJSON(w, http.StatusOK, map[string]string{"_id": id})
}

// laneError is a list move's failure, with the HTTP status that answers it.
type laneError struct {
	status int
	msg    string
}

// moveListTo moves list l of b to the board to - into its swimlane, at its
// position, with its title when given. On another board, the list joins the
// open list of the same title there, or becomes a new one, and its cards go
// with it. The caller has checked that the user may change both boards.
func (s *Server) moveListTo(ctx context.Context, u *User, b, to *Board, l bson.M, req *laneRequest) *laneError {
	id, _ := l["_id"].(string)
	title, _ := l["title"].(string)
	if t := strings.TrimSpace(req.Title); t != "" && len(t) <= 1000 {
		title = t
	}
	if to.ID == b.ID {
		set := bson.M{}
		if req.ToSwimlaneID != "" {
			set["swimlaneId"] = req.ToSwimlaneID
		}
		if title != l["title"] {
			set["title"] = title
		}
		if len(set) > 0 {
			_, _ = s.db.Collection("lists").UpdateOne(ctx, bson.M{"_id": eq(id)}, bson.M{"$set": set})
		}
		s.reposition(ctx, "lists", id, b.ID, req.Position)
		s.hub.publish(b.ID, "board-changed")
		return nil
	}
	listID := ""
	var existing List
	if s.db.Collection("lists").FindOne(ctx, bson.M{"boardId": to.ID, "title": eq(title), "archived": false}).Decode(&existing) == nil {
		listID = existing.ID
	} else {
		if strings.TrimSpace(title) == "" {
			return &laneError{http.StatusBadRequest, "This list has no title, so it cannot be moved to another board. Give it a title first."}
		}
		listID = randomID()
		if _, err := s.db.Collection("lists").InsertOne(ctx, bson.M{"_id": listID, "title": title, "boardId": to.ID, "type": l["type"], // inserted, not a filter
			"archived": false, "wipLimit": l["wipLimit"], "swimlaneId": req.ToSwimlaneID, "sort": l["sort"], "createdAt": time.Now().UTC()}); err != nil { // inserted, not a filter
			return &laneError{http.StatusInternalServerError, err.Error()}
		}
	}
	sw := req.ToSwimlaneID
	if sw == "" {
		sw = s.firstSwimlane(ctx, to.ID)
	}
	for _, c := range s.laneCards(ctx, bson.M{"listId": eq(id), "boardId": b.ID}) {
		if err := s.moveCardTo(ctx, u, c, b, to, sw, listID); err != nil {
			return &laneError{http.StatusInternalServerError, err.Error()}
		}
	}
	if listID != id { // the list itself moved: what is left of it on the old board goes
		_, _ = s.db.Collection("lists").DeleteOne(ctx, bson.M{"_id": eq(id), "boardId": b.ID})
	}
	s.reposition(ctx, "lists", listID, to.ID, req.Position)
	s.hub.publish(b.ID, "board-changed")
	s.hub.publish(to.ID, "board-changed")
	return nil
}

func (s *Server) swimlaneOf(w http.ResponseWriter, r *http.Request, b *Board) bson.M {
	id := r.PathValue("swimlane")
	var sw bson.M
	if !validID(id) || s.db.Collection("swimlanes").FindOne(r.Context(), bson.M{"_id": eq(id), "boardId": b.ID}).Decode(&sw) != nil {
		apiError(w, http.StatusNotFound, "Swimlane not found")
		return nil
	}
	return sw
}

// swimlaneLists is the lists a swimlane shows: its own, and - the board's first
// swimlane - the lists of no swimlane.
func (s *Server) swimlaneLists(ctx context.Context, b *Board, swimlaneID string) []bson.M {
	filter := bson.M{"boardId": b.ID, "archived": false, "swimlaneId": eq(swimlaneID)}
	if s.firstSwimlane(ctx, b.ID) == swimlaneID {
		filter["swimlaneId"] = bson.M{"$in": bson.A{swimlaneID, "", nil}}
	}
	cur, err := s.db.Collection("lists").Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}}))
	lists := []bson.M{}
	if err == nil {
		_ = cur.All(ctx, &lists)
	}
	return lists
}

// apiCopySwimlane is WeKan's Swimlane.copy: a new swimlane at the end of the
// destination board, its lists recreated in it and their cards copied.
func (s *Server) apiCopySwimlane(w http.ResponseWriter, r *http.Request, u *User) {
	b, to, req := s.laneSource(w, r, u)
	if b == nil {
		return
	}
	sw := s.swimlaneOf(w, r, b)
	if sw == nil {
		return
	}
	ctx := r.Context()
	old, _ := sw["_id"].(string)
	n, _ := s.db.Collection("swimlanes").CountDocuments(ctx, bson.M{"boardId": to.ID, "archived": false})
	doc := bson.M{}
	for k, v := range sw {
		doc[k] = v
	}
	delete(doc, "scrum")
	delete(doc, "scrumRevision")
	id := randomID()
	doc["_id"], doc["boardId"], doc["sort"] = id, to.ID, float64(n)
	if t := strings.TrimSpace(req.Title); t != "" {
		doc["title"] = t
	}
	if _, err := s.db.Collection("swimlanes").InsertOne(ctx, doc); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	first := s.firstSwimlane(ctx, b.ID) == old
	for _, l := range s.swimlaneLists(ctx, b, old) {
		listID := randomID()
		if _, err := s.db.Collection("lists").InsertOne(ctx, bson.M{"_id": listID, "title": l["title"], "boardId": to.ID, "sort": l["sort"],
			"type": orDefault(str(l, "type"), "list"), "archived": false, "wipLimit": l["wipLimit"], "swimlaneId": id,
			"color": l["color"], "width": l["width"], "createdAt": time.Now().UTC()}); err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
		filter := bson.M{"listId": l["_id"], "swimlaneId": eq(old)}
		if first {
			filter["swimlaneId"] = bson.M{"$in": bson.A{old, "", nil}}
		}
		for _, c := range s.laneCards(ctx, filter) {
			if _, err := s.copyCardTo(r, u, c, b, to, id, listID); err != nil {
				apiError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
	}
	s.reposition(ctx, "swimlanes", id, to.ID, req.Position)
	s.hub.publishFrom(to.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	writeJSON(w, http.StatusOK, map[string]string{"_id": id})
}

// apiMoveSwimlane is WeKan's Swimlane.move: on another board, each list's
// cards of the swimlane go to that board's list of the same title, or a new
// one, and the swimlane itself moves.
func (s *Server) apiMoveSwimlane(w http.ResponseWriter, r *http.Request, u *User) {
	b, to, req := s.laneSource(w, r, u)
	if b == nil {
		return
	}
	sw := s.swimlaneOf(w, r, b)
	if sw == nil {
		return
	}
	ctx := r.Context()
	id, _ := sw["_id"].(string)
	set := bson.M{}
	if t := strings.TrimSpace(req.Title); t != "" {
		set["title"] = t
	}
	if to.ID != b.ID {
		for _, l := range s.swimlaneLists(ctx, b, id) {
			title, _ := l["title"].(string)
			var existing List
			listID := ""
			if s.db.Collection("lists").FindOne(ctx, bson.M{"boardId": to.ID, "title": title, "archived": false}).Decode(&existing) == nil {
				listID = existing.ID
			} else {
				listID = randomID()
				if _, err := s.db.Collection("lists").InsertOne(ctx, bson.M{"_id": listID, "title": title, "boardId": to.ID, "type": l["type"],
					"archived": false, "wipLimit": l["wipLimit"], "swimlaneId": id, "sort": l["sort"], "createdAt": time.Now().UTC()}); err != nil {
					apiError(w, http.StatusInternalServerError, err.Error())
					return
				}
			}
			for _, c := range s.laneCards(ctx, bson.M{"listId": l["_id"], "swimlaneId": eq(id)}) {
				if err := s.moveCardTo(ctx, u, c, b, to, id, listID); err != nil {
					apiError(w, http.StatusInternalServerError, err.Error())
					return
				}
			}
		}
		n, _ := s.db.Collection("swimlanes").CountDocuments(ctx, bson.M{"boardId": to.ID, "archived": false})
		set["boardId"], set["sort"] = to.ID, float64(n)
	}
	if len(set) > 0 {
		if _, err := s.db.Collection("swimlanes").UpdateOne(ctx, bson.M{"_id": eq(id), "boardId": b.ID}, bson.M{"$set": set}); err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	s.reposition(ctx, "swimlanes", id, to.ID, req.Position)
	s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	s.hub.publishFrom(to.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	writeJSON(w, http.StatusOK, map[string]string{"_id": id})
}
