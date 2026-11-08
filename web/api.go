package web

import (
	"encoding/json"
	"errors"
	"mime"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// The REST API at WeKan's own addresses and in WeKan's own shapes
// (server/apiAuthRoutes.js, server/models/*.js), so a script written for WeKan
// works against Weddy unchanged.
func (s *Server) apiRoutes(m *http.ServeMux) {
	m.HandleFunc("POST /users/login", s.apiLogin)
	m.HandleFunc("GET /api/user", s.api(s.apiUser))
	m.HandleFunc("GET /api/users/{user}/boards", s.api(s.apiUserBoards))
	m.HandleFunc("POST /api/boards", s.api(s.apiCreateBoard))
	m.HandleFunc("GET /api/boards/{board}", s.api(s.apiBoard))
	m.HandleFunc("GET /api/boards/{board}/swimlanes", s.api(s.apiSwimlanes))
	m.HandleFunc("GET /api/boards/{board}/lists", s.api(s.apiLists))
	m.HandleFunc("POST /api/boards/{board}/lists", s.api(s.apiAddList))
	m.HandleFunc("GET /api/boards/{board}/lists/{list}/cards", s.api(s.apiCards))
	m.HandleFunc("GET /api/boards/{board}/cards_count", s.api(s.apiCardsCount))
	m.HandleFunc("GET /api/boards/{board}/lists/{list}/cards_count", s.api(s.apiCardsCount))
	m.HandleFunc("POST /api/boards/{board}/lists/{list}/cards", s.api(s.apiAddCard))
	m.HandleFunc("POST /api/boards/{board}/lists/{list}/cards/bulk", s.api(s.apiBulkAddCards))
	m.HandleFunc("GET /api/boards/{board}/lists/{list}/cards/{card}", s.api(s.apiCard))
	m.HandleFunc("PUT /api/boards/{board}/lists/{list}/cards/{card}", s.api(s.apiEditCard))
	m.HandleFunc("DELETE /api/boards/{board}/lists/{list}/cards/{card}", s.api(s.apiDeleteCard))
	s.apiMoreRoutes(m)
	s.apiMemberRoutes(m)
	s.apiCardRoutes(m)
	s.apiDependencyRoutes(m)
	s.apiRuleRoutes(m)
	s.exportRoutes(m)
	s.importRoutes(m)
	s.apiItemRoutes(m)
	s.apiLaneRoutes(m)
	s.apiBoardRoutes(m)
	s.apiBoardAttachmentRoutes(m)
	s.apiFieldRoutes(m)
	s.apiIntegrationRoutes(m)
	s.apiUserRoutes(m)
	s.apiSettingsRoutes(m)
	s.apiAdminRoutes(m)
}

// parentStatus is a refused parentId's own status, else 400.
func parentStatus(err error) int {
	var pe *parentError
	if errors.As(err, &pe) {
		return pe.status
	}
	return http.StatusBadRequest
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg, "statusCode": status})
}

// decodeJSON reads a JSON body, or a form, into v (at most 1 MiB).
func decodeJSON(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20)
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct == "application/x-www-form-urlencoded" || ct == "multipart/form-data" {
		if err := r.ParseForm(); err != nil {
			return err
		}
		flat := map[string]string{}
		for k, vs := range r.PostForm {
			flat[k] = vs[0]
		}
		b, _ := json.Marshal(flat)
		return json.Unmarshal(b, v)
	}
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return errors.New("the body is not JSON")
	}
	return nil
}

type apiHandler func(w http.ResponseWriter, r *http.Request, u *User)

func (s *Server) api(h apiHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := s.currentUser(r)
		if u == nil {
			apiError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}
		h(w, r, u)
	}
}

// loginThrottle refuses a client that failed loginFailures logins in a row, for
// loginLockout - WeKan's REST login throttle (GHSA-2g94-9x3m-hv37).
type loginThrottle struct {
	mu   sync.Mutex
	fail map[string]*throttleEntry
}

type throttleEntry struct {
	n     int
	until time.Time
}

const (
	loginFailures = 5
	loginLockout  = 15 * time.Minute
)

func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (t *loginThrottle) blocked(key string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.fail[key]
	return e != nil && now.Before(e.until)
}

func (t *loginThrottle) failed(key string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.fail == nil {
		t.fail = map[string]*throttleEntry{}
	}
	e := t.fail[key]
	if e == nil {
		e = &throttleEntry{}
		t.fail[key] = e
	}
	e.n++
	if e.n >= loginFailures {
		e.until = now.Add(loginLockout)
		e.n = 0
	}
	if len(t.fail) > 100000 { // an attacker rotating addresses must not grow this without end
		for k, v := range t.fail {
			if now.After(v.until) {
				delete(t.fail, k)
			}
		}
	}
}

func (t *loginThrottle) succeeded(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.fail, key)
}

func (s *Server) apiLogin(w http.ResponseWriter, r *http.Request) {
	key, now := clientKey(r), time.Now()
	if s.throttle.blocked(key, now) {
		w.Header().Set("Retry-After", "900")
		apiError(w, http.StatusTooManyRequests, "Too many failed login attempts. Try again later.")
		return
	}
	var req struct {
		Username string `json:"username"`
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	name := req.Username
	if name == "" {
		name = req.Email
	}
	u, err := s.findLoginUser(r.Context(), name)
	if err == nil && u.LoginDisabled {
		err = errBadPassword
	}
	if err == nil {
		err = CheckPassword(u, req.Password)
	}
	if err != nil {
		s.throttle.failed(key, now)
		apiError(w, http.StatusUnauthorized, "Login failed")
		return
	}
	s.throttle.succeeded(key)
	token, expires, err := s.newLoginToken(r.Context(), u.ID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": u.ID, "token": token, "tokenExpires": expires})
}

// apiUser is WeKan's GET /api/user: the caller without secrets, and the boards
// they are an active member of (StaleBleed: never one they were removed from).
func (s *Server) apiUser(w http.ResponseWriter, r *http.Request, u *User) {
	var doc bson.M
	if s.db.Collection("users").FindOne(r.Context(), bson.M{"_id": u.ID}).Decode(&doc) != nil {
		apiError(w, http.StatusNotFound, "User not found")
		return
	}
	doc["boards"] = s.memberships(r, u.ID, true)
	writeJSON(w, http.StatusOK, withoutSecrets(doc))
}

func (s *Server) apiUserBoards(w http.ResponseWriter, r *http.Request, u *User) {
	id := r.PathValue("user")
	if (id != u.ID && !u.IsAdmin) || !validID(id) {
		apiError(w, http.StatusForbidden, "Forbidden")
		return
	}
	boards, err := s.boardsOf(r.Context(), id)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]string, 0, len(boards))
	for _, b := range boards {
		out = append(out, map[string]string{"_id": b.ID, "title": b.Title})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiCreateBoard(w http.ResponseWriter, r *http.Request, u *User) {
	var req struct {
		Title string `json:"title"`
		Owner string `json:"owner"`
	}
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Owner != "" && req.Owner != u.ID {
		apiError(w, http.StatusForbidden, "a board is created for yourself")
		return
	}
	b, err := s.CreateBoard(r.Context(), u, req.Title)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	var sw Swimlane
	_ = s.db.Collection("swimlanes").FindOne(r.Context(), bson.M{"boardId": b.ID}).Decode(&sw)
	writeJSON(w, http.StatusOK, map[string]string{"_id": b.ID, "defaultSwimlaneId": sw.ID})
}

// apiBoardFor is the board the caller may read, or writes 403 and returns nil.
// write: the caller's role must have the write capability.
func (s *Server) apiBoardFor(w http.ResponseWriter, r *http.Request, u *User, write bool) *Board {
	if write {
		return s.apiBoardCan(w, r, u, "write")
	}
	return s.apiBoardCan(w, r, u, "")
}

// apiBoardCan is WeKan's server/authentication.js for a REST route: capability ""
// reads the whole board (checkBoardAccess), "write" changes it
// (checkBoardWriteAccess), "comment" comments. A site admin may do all of them,
// on any board. A read needs a role that sees the whole board, so an
// assigned-only member is refused (AssignedBleed), and a write refuses an
// assigned-only member too: these routes act on any card of the board by id.
// Not recorded: an API client of such a member reaches it in ordinary use.
func (s *Server) apiBoardCan(w http.ResponseWriter, r *http.Request, u *User, capability string) *Board {
	b, err := s.boardFor(r.Context(), r.PathValue("board"), u)
	if err != nil && u.IsAdmin && validID(r.PathValue("board")) {
		var any Board
		if s.db.Collection("boards").FindOne(r.Context(), bson.M{"_id": eq(r.PathValue("board"))}).Decode(&any) == nil {
			b, err = &any, nil
		}
	}
	if err != nil {
		apiError(w, http.StatusForbidden, "Forbidden")
		return nil
	}
	if u.IsAdmin {
		return b
	}
	m := b.member(u.ID)
	ok := true
	switch capability {
	case "":
		ok = m == nil || m.ReadsWholeBoard() // a public board's reader, or a member who sees every card
	case "write":
		ok = m != nil && b.Can(u.ID, "write") && !m.AssignedOnly()
	default:
		ok = m != nil && b.Can(u.ID, capability)
	}
	if !ok {
		apiError(w, http.StatusForbidden, "Forbidden")
		return nil
	}
	return b
}

func (s *Server) apiBoard(w http.ResponseWriter, r *http.Request, u *User) {
	if b := s.apiBoardFor(w, r, u, false); b != nil {
		writeJSON(w, http.StatusOK, b)
	}
}

func (s *Server) idTitles(w http.ResponseWriter, r *http.Request, coll string, filter bson.M) {
	cur, err := s.db.Collection(coll).Find(r.Context(), filter, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}}))
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var docs []struct {
		ID    string `bson:"_id" json:"_id"`
		Title string `bson:"title" json:"title"`
	}
	if err := cur.All(r.Context(), &docs); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if docs == nil {
		docs = docs[:0]
	}
	writeJSON(w, http.StatusOK, docs)
}

func (s *Server) apiSwimlanes(w http.ResponseWriter, r *http.Request, u *User) {
	if b := s.apiBoardFor(w, r, u, false); b != nil {
		s.idTitles(w, r, "swimlanes", bson.M{"boardId": b.ID, "archived": false})
	}
}

func (s *Server) apiLists(w http.ResponseWriter, r *http.Request, u *User) {
	if b := s.apiBoardFor(w, r, u, false); b != nil {
		s.idTitles(w, r, "lists", bson.M{"boardId": b.ID, "archived": false})
	}
}

func (s *Server) apiAddList(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, true)
	if b == nil {
		return
	}
	var req struct {
		Title string `json:"title"`
	}
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	l, err := s.AddList(r.Context(), b, req.Title)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.activity(r.Context(), u.ID, "createList", bson.M{"boardId": b.ID, "listId": l.ID, "title": l.Title})
	s.hub.publish(b.ID, "board-changed")
	writeJSON(w, http.StatusOK, map[string]string{"_id": l.ID})
}

func (s *Server) apiCards(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, false)
	if b == nil {
		return
	}
	list := r.PathValue("list")
	if !validID(list) {
		apiError(w, http.StatusNotFound, "no such list")
		return
	}
	cur, err := s.db.Collection("cards").Find(r.Context(), bson.M{"boardId": b.ID, "listId": eq(list), "archived": false},
		options.Find().SetSort(bson.D{{Key: "sort", Value: 1}}))
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var cards []Card
	if err := cur.All(r.Context(), &cards); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(cards))
	for _, c := range cards {
		out = append(out, map[string]any{"_id": c.ID, "title": c.Title, "description": c.Description, "swimlaneId": c.SwimlaneID})
	}
	writeJSON(w, http.StatusOK, out)
}

// apiCardsCount: the board's or a list's open cards, as WeKan answers
// {board_cards_count} or {list_cards_count}. A non-member gets 403 (ErrorBleed:
// WeKan once answered 200 with the error object).
func (s *Server) apiCardsCount(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, false)
	if b == nil {
		return
	}
	filter, key := bson.M{"boardId": b.ID, "archived": false}, "board_cards_count"
	if list := r.PathValue("list"); list != "" {
		if !validID(list) {
			apiError(w, http.StatusNotFound, "no such list")
			return
		}
		filter["listId"], key = eq(list), "list_cards_count"
	}
	n, err := s.db.Collection("cards").CountDocuments(r.Context(), filter)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{key: n})
}

func (s *Server) apiAddCard(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, true)
	if b == nil {
		return
	}
	var req map[string]any
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	swimlane, _ := req["swimlaneId"].(string)
	// #5897: a linked card shows another card - one the caller may read.
	var linked *Card
	if v, ok := req["linkedId"]; ok && v != nil && v != "" {
		id, _ := v.(string)
		var src Card
		if !validID(id) || s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": eq(id)}).Decode(&src) != nil {
			apiError(w, http.StatusNotFound, "no such card to link")
			return
		}
		sb, err := s.boardFor(r.Context(), src.BoardID, u)
		if err != nil || (sb.member(u.ID) != nil && sb.member(u.ID).AssignedOnly() && !slices.Contains(src.Assignees, u.ID)) {
			apiError(w, http.StatusForbidden, "Forbidden")
			return
		}
		linked = &src
		if t, _ := req["title"].(string); strings.TrimSpace(t) == "" {
			req["title"] = src.Title
		}
	}
	id, err := s.addCardFrom(r, b, u, r.PathValue("list"), swimlane, req)
	if err != nil {
		apiError(w, parentStatus(err), err.Error())
		return
	}
	if linked != nil {
		target := linked.ID
		if linked.Type == "cardType-linkedCard" { // a link of a link shows the original
			var raw bson.M
			if s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": linked.ID}).Decode(&raw) == nil {
				if l, ok := raw["linkedId"].(string); ok && l != "" {
					target = l
				}
			}
		}
		if _, err := s.db.Collection("cards").UpdateOne(r.Context(), bson.M{"_id": eq(id)},
			bson.M{"$set": bson.M{"type": "cardType-linkedCard", "linkedId": target}}); err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	s.hub.publish(b.ID, "board-changed")
	writeJSON(w, http.StatusOK, map[string]string{"_id": id})
}

func (s *Server) cardOf(w http.ResponseWriter, r *http.Request, b *Board) *Card {
	card, list := r.PathValue("card"), r.PathValue("list")
	if !validID(card) || !validID(list) {
		apiError(w, http.StatusNotFound, "no such card")
		return nil
	}
	var c Card
	err := s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": eq(card), "boardId": b.ID, "listId": eq(list)}).Decode(&c)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			apiError(w, http.StatusNotFound, "no such card")
		} else {
			apiError(w, http.StatusInternalServerError, err.Error())
		}
		return nil
	}
	return &c
}

// apiCard is WeKan's: the whole card, with its checklists and their items.
func (s *Server) apiCard(w http.ResponseWriter, r *http.Request, u *User) {
	if b := s.apiBoardFor(w, r, u, false); b != nil {
		if c := s.cardOf(w, r, b); c != nil {
			var raw bson.M
			_ = s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": c.ID}).Decode(&raw)
			writeJSON(w, http.StatusOK, s.cardWithChecklists(r, raw))
		}
	}
}

func (s *Server) apiEditCard(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, true)
	if b == nil {
		return
	}
	c := s.cardOf(w, r, b)
	if c == nil {
		return
	}
	var req map[string]any
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	set, unset, err := cardFields(req)
	if err == nil {
		err = s.setParent(r, u, req, set)
	}
	if err != nil {
		apiError(w, parentStatus(err), err.Error())
		return
	}
	set["modifiedAt"] = time.Now().UTC()
	// A move to another board: newBoardId, newSwimlaneId and newListId together
	// (WeKan's isBoardMove), with write access there and a list and swimlane of
	// that board.
	newBoard, _ := req["newBoardId"].(string)
	newSwimlane, _ := req["newSwimlaneId"].(string)
	newList, _ := req["newListId"].(string)
	var moveTo *Board
	if newBoard != "" && newSwimlane != "" && newList != "" && newBoard != b.ID {
		if moveTo = s.writableBoard(r.Context(), newBoard, u); moveTo == nil {
			apiError(w, http.StatusForbidden, "Forbidden")
			return
		}
		if !validID(newList) || s.db.Collection("lists").FindOne(r.Context(), bson.M{"_id": eq(newList), "boardId": moveTo.ID, "archived": false}).Err() != nil {
			apiError(w, http.StatusNotFound, "Destination list not found or does not belong to destination board")
			return
		}
		if !validID(newSwimlane) || s.db.Collection("swimlanes").FindOne(r.Context(), bson.M{"_id": eq(newSwimlane), "boardId": moveTo.ID, "archived": false}).Err() != nil {
			apiError(w, http.StatusNotFound, "Destination swimlane not found or does not belong to destination board")
			return
		}
	}
	listID, hasList := req["listId"].(string)
	swimlaneID, hasSwimlane := req["swimlaneId"].(string)
	if moveTo != nil {
		hasList, hasSwimlane = false, false
	}
	if hasList || hasSwimlane {
		m := Move{CardID: c.ID, ListID: c.ListID, SwimlaneID: c.SwimlaneID, Sort: c.Sort}
		if hasList {
			m.ListID = listID
		}
		if hasSwimlane {
			m.SwimlaneID = swimlaneID
		}
		if err := s.MoveCards(r.Context(), b, u, []Move{m}); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	update := bson.M{"$set": set}
	if len(unset) > 0 {
		update["$unset"] = unset
	}
	if _, err := s.db.Collection("cards").UpdateOne(r.Context(), bson.M{"_id": c.ID}, update); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if moveTo != nil {
		var raw bson.M
		_ = s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": c.ID}).Decode(&raw)
		if err := s.moveCardTo(r.Context(), u, raw, b, moveTo, newSwimlane, newList); err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.hub.publish(moveTo.ID, "board-changed")
	}
	s.hub.publish(b.ID, "board-changed")
	writeJSON(w, http.StatusOK, map[string]string{"_id": c.ID})
}

func (s *Server) apiDeleteCard(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, true)
	if b == nil {
		return
	}
	c := s.cardOf(w, r, b)
	if c == nil {
		return
	}
	if err := s.DeleteCard(r.Context(), b, u, c); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.hub.publish(b.ID, "board-changed")
	writeJSON(w, http.StatusOK, map[string]string{"_id": c.ID})
}
