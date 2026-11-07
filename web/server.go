package web

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Joker/jade"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/wekan/caddy-ferretdb-plugin/store"
)

//go:embed templates/*.pug
var templateFiles embed.FS

//go:embed static
var staticFiles embed.FS

// Server is the WeKan of one domain.
type Server struct {
	db       *mongo.Database
	release  func() error
	pages    map[string]*template.Template
	hub      *hub
	throttle loginThrottle
	mux      *http.ServeMux
	log      *slog.Logger
	// Tenant: a customer's WeKan in a many-domain Weddy - no Cloud menu.
	Tenant bool
}

// Open starts the WeKan of the domain whose files are in dir: its database is
// dir/files/db (wekan.sqlite), as for Node.js WeKan. dbListen, when set, is a TCP
// address where the database also answers, for MongoDB tools and backups.
func Open(ctx context.Context, dir, dbListen string, log *slog.Logger) (*Server, error) {
	db, release, err := store.Acquire(ctx, filepath.Join(dir, "files", "db"), &store.Options{TCP: dbListen, Logger: log})
	if err != nil {
		return nil, err
	}
	s, err := New(db.DB, log)
	if err != nil {
		_ = release()
		return nil, err
	}
	s.release = release
	return s, nil
}

// New makes a Server over an open database (the tests give one directly).
func New(db *mongo.Database, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Server{db: db, pages: map[string]*template.Template{}, hub: newHub(), log: log}
	if err := s.compileTemplates(); err != nil {
		return nil, err
	}
	s.routes()
	return s, nil
}

// DB is the domain's database.
func (s *Server) DB() *mongo.Database { return s.db }

// Close releases the database.
func (s *Server) Close() error {
	s.hub.closeAll()
	if s.release != nil {
		return s.release()
	}
	return nil
}

var funcs = template.FuncMap{
	"memberName": func(u *User) string { return u.Name() },
	"t":          tr,
}

// compileTemplates turns every templates/<page>.pug into an html/template, inside
// templates/layout.pug.
func (s *Server) compileTemplates() error {
	layoutSrc, err := templateFiles.ReadFile("templates/layout.pug")
	if err != nil {
		return err
	}
	layout, err := jade.Parse("layout", layoutSrc)
	if err != nil {
		return fmt.Errorf("layout.pug: %w", err)
	}
	entries, err := fs.ReadDir(templateFiles, "templates")
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".pug")
		if name == "layout" {
			continue
		}
		src, err := templateFiles.ReadFile("templates/" + e.Name())
		if err != nil {
			return err
		}
		body, err := jade.Parse(name, src)
		if err != nil {
			return fmt.Errorf("%s: %w", e.Name(), err)
		}
		t, err := template.New("layout").Funcs(funcs).Parse(layout)
		if err != nil {
			return fmt.Errorf("layout.pug: %w", err)
		}
		if _, err := t.New("content").Parse(body); err != nil {
			return fmt.Errorf("%s: %w", e.Name(), err)
		}
		s.pages[name] = t
	}
	return nil
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, page string, status int, data map[string]any) {
	u, _ := data["User"].(*User)
	lang := languageOf(r, u)
	data["L"], data["Lang"] = lang.Tag, lang
	t := s.pages[page]
	if t == nil {
		http.Error(w, "no page "+page, http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", data); err != nil {
		s.log.Error("render", "page", page, "error", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

// ServeHTTP answers a request of this domain.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) routes() {
	m := http.NewServeMux()
	static, _ := fs.Sub(staticFiles, "static")
	m.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	m.HandleFunc("GET /sign-in", s.signInPage)
	m.HandleFunc("POST /sign-in", s.signIn)
	m.HandleFunc("POST /logout", s.logout)
	m.HandleFunc("GET /{$}", s.page(s.allBoards))
	m.HandleFunc("POST /boards", s.page(s.createBoard))
	m.HandleFunc("GET /b/{board}/{slug}", s.page(s.boardPage))
	m.HandleFunc("GET /b/{board}/{slug}/{card}", s.page(s.boardPage))
	m.HandleFunc("POST /b/{board}/lists", s.page(s.addList))
	m.HandleFunc("POST /b/{board}/cards", s.page(s.addCard))
	m.HandleFunc("POST /b/{board}/move", s.page(s.moveCards))
	m.HandleFunc("GET /b/{board}/live", s.page(s.live))
	s.apiRoutes(m)
	s.mux = m
}

type pageHandler func(w http.ResponseWriter, r *http.Request, u *User)

// page requires a signed-in user; others go to the sign-in page (or get 401 for
// a script's request).
func (s *Server) page(h pageHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "cross-site request refused", http.StatusForbidden)
			return
		}
		u := s.currentUser(r)
		if u == nil {
			if r.Method == http.MethodGet && r.Header.Get("Accept") != "text/event-stream" {
				http.Redirect(w, r, "/sign-in", http.StatusSeeOther)
				return
			}
			http.Error(w, "sign in first", http.StatusUnauthorized)
			return
		}
		h(w, r, u)
	}
}

// msg names a translation key a page shows later ({{t $.L .Error}}), so that
// cmd/sync-i18n copies it.
func msg(key string) string { return key }

// sameOrigin refuses a state-changing request a page of another site sent
// (CSRF): its Origin, when the browser gives one, must be this host.
func sameOrigin(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	o := r.Header.Get("Origin")
	if o == "" {
		return true // not a browser's cross-site form: curl, a script with a token
	}
	u, err := url.Parse(o)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

func (s *Server) signInPage(w http.ResponseWriter, r *http.Request) {
	if s.currentUser(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, r, "signin", http.StatusOK, map[string]any{"Title": tr(languageOf(r, nil).Tag, "log-in")})
}

func (s *Server) signIn(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-site request refused", http.StatusForbidden)
		return
	}
	name, password := r.FormValue("username"), r.FormValue("password")
	key, now := clientKey(r), time.Now()
	if s.throttle.blocked(key, now) {
		s.render(w, r, "signin", http.StatusTooManyRequests, map[string]any{"Title": tr(languageOf(r, nil).Tag, "log-in"), "Error": msg("account-locked"), "Username": name})
		return
	}
	u, err := s.findLoginUser(r.Context(), name)
	if err == nil && u.LoginDisabled {
		err = errBadPassword
	}
	if err == nil {
		err = CheckPassword(u, password)
	}
	if err != nil {
		s.throttle.failed(key, now)
		s.render(w, r, "signin", http.StatusUnauthorized, map[string]any{"Title": tr(languageOf(r, nil).Tag, "log-in"), "Error": msg("error-ldap-login"), "Username": name})
		return
	}
	s.throttle.succeeded(key)
	token, expires, err := s.newLoginToken(r.Context(), u.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	setSessionCookie(w, r, token, expires)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.dropToken(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	http.Redirect(w, r, "/sign-in", http.StatusSeeOther)
}

func (s *Server) boardsOf(ctx context.Context, userID string) ([]Board, error) {
	cur, err := s.db.Collection("boards").Find(ctx, bson.M{"archived": false, "members": bson.M{"$elemMatch": bson.M{"userId": userID, "isActive": true}},
		"type": bson.M{"$nin": []string{"template-container", "template-board"}}}, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}, {Key: "title", Value: 1}}))
	if err != nil {
		return nil, err
	}
	var boards []Board
	return boards, cur.All(ctx, &boards)
}

func (s *Server) allBoards(w http.ResponseWriter, r *http.Request, u *User) {
	boards, err := s.boardsOf(r.Context(), u.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "boards", http.StatusOK, map[string]any{"Title": tr(languageOf(r, u).Tag, "all-boards"), "User": u, "Boards": boards})
}

func slugify(title string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(title) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "board"
	}
	return out
}

// CreateBoard makes a board with WeKan's default swimlane, owned by u.
func (s *Server) CreateBoard(ctx context.Context, u *User, title string) (*Board, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, errors.New("a board needs a title")
	}
	now := time.Now().UTC()
	b := &Board{ID: randomID(), Title: title, Slug: slugify(title), Permission: "private", Type: "board", CreatedAt: now,
		Members: []Member{{UserID: u.ID, IsAdmin: true, IsActive: true}}}
	if _, err := s.db.Collection("boards").InsertOne(ctx, b); err != nil {
		return nil, err
	}
	sw := Swimlane{ID: randomID(), Title: "Default", BoardID: b.ID, Type: "swimlane"}
	if _, err := s.db.Collection("swimlanes").InsertOne(ctx, sw); err != nil {
		return nil, err
	}
	return b, nil
}

func (s *Server) createBoard(w http.ResponseWriter, r *http.Request, u *User) {
	b, err := s.CreateBoard(r.Context(), u, r.FormValue("title"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/b/"+b.ID+"/"+b.Slug, http.StatusSeeOther)
}

// boardFor is the board, if u may see it: an active member, or a public board.
func (s *Server) boardFor(ctx context.Context, id string, u *User) (*Board, error) {
	var b Board
	if err := s.db.Collection("boards").FindOne(ctx, bson.M{"_id": id}).Decode(&b); err != nil {
		return nil, err
	}
	if !b.MemberOf(u.ID) && b.Permission != "public" {
		return nil, mongo.ErrNoDocuments // not yours: as if it did not exist
	}
	return &b, nil
}

// lane is a swimlane with its lists, and each list with its cards in that lane.
type lane struct {
	Swimlane
	Lists []laneList
}

type laneList struct {
	List
	Lane  string // the swimlane this list is shown in
	Cards []Card
}

func (s *Server) boardData(ctx context.Context, b *Board) ([]lane, []List, error) {
	find := func(coll string, filter bson.M, out any) error {
		cur, err := s.db.Collection(coll).Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}}))
		if err != nil {
			return err
		}
		return cur.All(ctx, out)
	}
	var swimlanes []Swimlane
	var lists []List
	var cards []Card
	if err := find("swimlanes", bson.M{"boardId": b.ID, "archived": false}, &swimlanes); err != nil {
		return nil, nil, err
	}
	if err := find("lists", bson.M{"boardId": b.ID, "archived": false}, &lists); err != nil {
		return nil, nil, err
	}
	if err := find("cards", bson.M{"boardId": b.ID, "archived": false}, &cards); err != nil {
		return nil, nil, err
	}
	out := make([]lane, 0, len(swimlanes))
	for _, sw := range swimlanes {
		l := lane{Swimlane: sw}
		for _, li := range lists {
			if li.SwimlaneID != "" && li.SwimlaneID != sw.ID {
				continue
			}
			ll := laneList{List: li, Lane: sw.ID}
			for _, c := range cards {
				if c.ListID == li.ID && (c.SwimlaneID == sw.ID || (c.SwimlaneID == "" && len(swimlanes) == 1)) {
					ll.Cards = append(ll.Cards, c)
				}
			}
			sort.SliceStable(ll.Cards, func(i, j int) bool { return ll.Cards[i].Sort < ll.Cards[j].Sort })
			l.Lists = append(l.Lists, ll)
		}
		out = append(out, l)
	}
	return out, lists, nil
}

func (s *Server) boardPage(w http.ResponseWriter, r *http.Request, u *User) {
	b, err := s.boardFor(r.Context(), r.PathValue("board"), u)
	if err != nil {
		s.render(w, r, "notfound", http.StatusNotFound, map[string]any{"Title": tr(languageOf(r, u).Tag, "board-not-found"), "User": u})
		return
	}
	lanes, _, err := s.boardData(r.Context(), b)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := map[string]any{"Title": b.Title, "User": u, "Board": b, "Lanes": lanes, "CanEdit": b.MemberOf(u.ID)}
	if id := r.PathValue("card"); id != "" {
		var c Card
		if err := s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": id, "boardId": b.ID}).Decode(&c); err == nil {
			data["Card"] = c
		}
	}
	s.render(w, r, "board", http.StatusOK, data)
}

func (s *Server) editableBoard(w http.ResponseWriter, r *http.Request, u *User) *Board {
	b, err := s.boardFor(r.Context(), r.PathValue("board"), u)
	if err != nil || !b.MemberOf(u.ID) {
		http.Error(w, "not found", http.StatusNotFound)
		return nil
	}
	return b
}

func (s *Server) nextSort(ctx context.Context, coll string, filter bson.M) float64 {
	var last struct {
		Sort float64 `bson:"sort"`
	}
	err := s.db.Collection(coll).FindOne(ctx, filter, options.FindOne().SetSort(bson.D{{Key: "sort", Value: -1}})).Decode(&last)
	if err != nil {
		return 0
	}
	return last.Sort + 1
}

// AddList adds a list at the end of the board.
func (s *Server) AddList(ctx context.Context, b *Board, title string) (*List, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, errors.New("a list needs a title")
	}
	l := &List{ID: randomID(), Title: title, BoardID: b.ID, Type: "list",
		Sort: s.nextSort(ctx, "lists", bson.M{"boardId": b.ID})}
	_, err := s.db.Collection("lists").InsertOne(ctx, l)
	return l, err
}

func (s *Server) addList(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.editableBoard(w, r, u)
	if b == nil {
		return
	}
	if _, err := s.AddList(r.Context(), b, r.FormValue("title")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.hub.publish(b.ID, "board-changed")
	http.Redirect(w, r, "/b/"+b.ID+"/"+b.Slug, http.StatusSeeOther)
}

// AddCard adds a card at the end of a list in a swimlane.
func (s *Server) AddCard(ctx context.Context, b *Board, u *User, listID, swimlaneID, title string) (*Card, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, errors.New("a card needs a title")
	}
	for coll, id := range map[string]string{"lists": listID, "swimlanes": swimlaneID} {
		if n, _ := s.db.Collection(coll).CountDocuments(ctx, bson.M{"_id": id, "boardId": b.ID}); n == 0 {
			return nil, fmt.Errorf("no such %s on this board", strings.TrimSuffix(coll, "s"))
		}
	}
	c := &Card{ID: randomID(), Title: title, BoardID: b.ID, ListID: listID, SwimlaneID: swimlaneID, UserID: u.ID,
		Type: "cardType-card", CreatedAt: time.Now().UTC(),
		Sort: s.nextSort(ctx, "cards", bson.M{"boardId": b.ID, "listId": listID, "swimlaneId": swimlaneID})}
	_, err := s.db.Collection("cards").InsertOne(ctx, c)
	return c, err
}

func (s *Server) addCard(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.editableBoard(w, r, u)
	if b == nil {
		return
	}
	if _, err := s.AddCard(r.Context(), b, u, r.FormValue("listId"), r.FormValue("swimlaneId"), r.FormValue("title")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.hub.publish(b.ID, "board-changed")
	http.Redirect(w, r, "/b/"+b.ID+"/"+b.Slug, http.StatusSeeOther)
}

// Move is one card's new place.
type Move struct {
	CardID     string  `json:"cardId"`
	ListID     string  `json:"listId"`
	SwimlaneID string  `json:"swimlaneId"`
	Sort       float64 `json:"sort"`
}

// MoveCards moves many cards at once - MultiDrag: every finger its own card - each
// only within the board and to a list and swimlane of it.
func (s *Server) MoveCards(ctx context.Context, b *Board, moves []Move) error {
	if len(moves) == 0 || len(moves) > 500 {
		return errors.New("1 to 500 moves")
	}
	for _, m := range moves {
		for coll, id := range map[string]string{"lists": m.ListID, "swimlanes": m.SwimlaneID} {
			if n, _ := s.db.Collection(coll).CountDocuments(ctx, bson.M{"_id": id, "boardId": b.ID}); n == 0 {
				return fmt.Errorf("no such %s on this board", strings.TrimSuffix(coll, "s"))
			}
		}
		res, err := s.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": m.CardID, "boardId": b.ID},
			bson.M{"$set": bson.M{"listId": m.ListID, "swimlaneId": m.SwimlaneID, "sort": m.Sort, "modifiedAt": time.Now().UTC()}})
		if err != nil {
			return err
		}
		if res.MatchedCount == 0 {
			return fmt.Errorf("no card %s on this board", m.CardID)
		}
	}
	return nil
}

func (s *Server) moveCards(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.editableBoard(w, r, u)
	if b == nil {
		return
	}
	var req struct {
		Moves []Move `json:"moves"`
	}
	if err := decodeJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.MoveCards(r.Context(), b, req.Moves); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.hub.publish(b.ID, "board-changed")
	w.WriteHeader(http.StatusNoContent)
}

// live streams "board-changed" to every open page of the board (Server-Sent
// Events); the page then reloads its board with Unpoly.
func (s *Server) live(w http.ResponseWriter, r *http.Request, u *User) {
	b, err := s.boardFor(r.Context(), r.PathValue("board"), u)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	ch := s.hub.subscribe(b.ID)
	defer s.hub.unsubscribe(b.ID, ch)
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, open := <-ch:
			if !open {
				return
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev, b.ID)
			flusher.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// hub sends a board's events to its open pages.
type hub struct {
	mu   sync.Mutex
	subs map[string]map[chan string]bool
}

func newHub() *hub { return &hub{subs: map[string]map[chan string]bool{}} }

func (h *hub) subscribe(board string) chan string {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan string, 8)
	if h.subs[board] == nil {
		h.subs[board] = map[chan string]bool{}
	}
	h.subs[board][ch] = true
	return ch
}

func (h *hub) unsubscribe(board string, ch chan string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs[board][ch] {
		delete(h.subs[board], ch)
		close(ch)
	}
}

func (h *hub) publish(board, ev string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[board] {
		select {
		case ch <- ev:
		default: // a slow page gets the next one; it reloads the whole board anyway
		}
	}
}

func (h *hub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for board, chans := range h.subs {
		for ch := range chans {
			close(ch)
		}
		delete(h.subs, board)
	}
}
