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
	"slices"
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
	// CloudDir: the primary's WeKan in a many-domain Weddy - Admin Panel /
	// People / Cloud, and the Cloud Panel (cloudpanel.go).
	CloudDir string
	// FilesDir: <dir>/files, where attachments/ is (attachments.go).
	FilesDir string
	// OIDC: sign in with OAuth2 / OpenID Connect (oidc.go), from WeKan's OAUTH2_*.
	OIDC      *OIDCConfig
	cloudOnce sync.Once
	cloudF    *cloudFiles
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
	s.FilesDir = filepath.Join(dir, "files")
	s.OIDC = OIDCFromEnv(nil)
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
	if err := s.ensureSettings(context.Background()); err != nil {
		return nil, err
	}
	s.routes()
	return s, nil
}

// ensureSettings makes the site's one settings document when there is none,
// with Node.js WeKan's defaults (server/models/settings.js, at startup), so
// Admin Panel and WeKan's tools always find it. Mail is off until set; its
// sender is left to Admin Panel, as Weddy serves more than one domain.
func (s *Server) ensureSettings(ctx context.Context) error {
	if n, err := s.db.Collection("settings").CountDocuments(ctx, bson.M{}); err != nil || n > 0 {
		return err
	}
	now := time.Now().UTC()
	mail := bson.M{"username": "", "password": "", "host": "", "port": "", "enableTLS": false, "from": "", // inserted, not a filter
		"enabled": false, "service": "SMTP", "configurations": bson.M{"SMTP": bson.M{}}, // inserted, not a filter
		"passwords": bson.M{}, "passwordSet": bson.M{}} // inserted, not a filter
	_, err := s.db.Collection("settings").InsertOne(ctx, bson.M{"_id": randomID(), "disableRegistration": false, "mailServer": mail, // inserted, not a filter
		"createdAt": now, "modifiedAt": now, "displayAuthenticationMethod": true, "defaultAuthenticationMethod": "password"}) // inserted, not a filter
	return err
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
	// starred is whether the board is one of the user's favorites.
	"starred": func(u *User, id string) bool { return u != nil && slices.Contains(u.Profile.StarredBoards, id) },
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
	if u != nil {
		data["Unread"] = s.unreadNotifications(r.Context(), u.ID)
		data["CloudMenu"] = s.cloudOn() && u.IsAdmin
		data["PanelMenu"] = s.panelAllowed(r, u)
	}
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
	m.HandleFunc("GET /allboards/{section...}", s.page(s.allBoards))
	m.HandleFunc("POST /boards", s.page(s.createBoard))
	m.HandleFunc("GET /b/{board}/{slug}", s.page(s.boardPage))
	m.HandleFunc("GET /b/{board}/{slug}/{card}", s.page(s.boardPage))
	m.HandleFunc("POST /b/{board}/cards/{card}/people", s.page(s.cardPeoplePage))
	m.HandleFunc("POST /b/{board}/lists", s.page(s.addList))
	m.HandleFunc("POST /b/{board}/swimlanes", s.page(s.addSwimlanePage))
	m.HandleFunc("POST /b/{board}/cards", s.page(s.addCard))
	m.HandleFunc("POST /b/{board}/cards/{card}/comments", s.page(s.addCommentPage))
	m.HandleFunc("POST /b/{board}/cards/{card}/members/{user}", s.page(s.toggleCardMemberPage))
	m.HandleFunc("POST /b/{board}/cards/{card}/vote", s.page(s.votePage))
	m.HandleFunc("POST /b/{board}/cards/{card}/poker", s.page(s.pokerPage))
	m.HandleFunc("POST /b/{board}/cards/{card}/watch", s.page(s.watchCardPage))
	m.HandleFunc("POST /b/{board}/watch", s.page(s.watchBoardPage))
	m.HandleFunc("POST /b/{board}/move", s.page(s.moveCards))
	m.HandleFunc("GET /b/{board}/live", s.page(s.live))
	m.HandleFunc("GET /language", s.page(s.languagePage))
	s.notificationRoutes(m)
	s.methodRoutes(m)
	m.HandleFunc("POST /language", s.page(s.setLanguage))
	s.apiRoutes(m)
	s.cloudRoutes(m)
	s.attachmentRoutes(m)
	s.userRoutes(m)
	s.oidcRoutes(m)
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

// languagePage is WeKan's Change Language: every language, named in itself.
func (s *Server) languagePage(w http.ResponseWriter, r *http.Request, u *User) {
	lang := languageOf(r, u)
	s.render(w, r, "language", http.StatusOK, map[string]any{"Title": tr(lang.Tag, "changeLanguagePopup-title"),
		"User": u, "Languages": Languages(), "Current": lang.Tag})
}

// setLanguage keeps the choice in profile.language, as WeKan does - only a
// language WeKan has.
func (s *Server) setLanguage(w http.ResponseWriter, r *http.Request, u *User) {
	l := translations.lookup(r.FormValue("language"))
	if l == nil || !strings.EqualFold(langKey(l.Tag), langKey(r.FormValue("language"))) && !strings.EqualFold(langKey(l.Code), langKey(r.FormValue("language"))) {
		http.Error(w, "no such language", http.StatusBadRequest)
		return
	}
	if _, err := s.db.Collection("users").UpdateOne(r.Context(), bson.M{"_id": eq(u.ID)},
		bson.M{"$set": bson.M{"profile.language": l.Code}}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
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
	s.render(w, r, "signin", http.StatusOK, map[string]any{"Title": tr(languageOf(r, nil).Tag, "log-in"), "OIDC": s.OIDC != nil})
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
	setSessionCookie(w, token, expires)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.dropToken(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/sign-in", http.StatusSeeOther)
}

func (s *Server) boardsOf(ctx context.Context, userID string) ([]Board, error) {
	cur, err := s.db.Collection("boards").Find(ctx, bson.M{"archived": false, "members": bson.M{"$elemMatch": bson.M{"userId": eq(userID), "isActive": true}},
		"type": bson.M{"$nin": []string{"template-container", "template-board"}}}, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}, {Key: "title", Value: 1}}))
	if err != nil {
		return nil, err
	}
	var boards []Board
	return boards, cur.All(ctx, &boards)
}

// starredBoards are the user's favorites they may still see: an active
// member's, or a public board's - a stale star never brings back a board that
// turned private (WeKan's starredPublicBoardSelector, #6713).
func (s *Server) starredBoards(ctx context.Context, u *User) ([]Board, error) {
	ids := bson.A{}
	for _, id := range u.Profile.StarredBoards {
		ids = append(ids, id)
	}
	cur, err := s.db.Collection("boards").Find(ctx, bson.M{"_id": dbIn(ids), "archived": false},
		options.Find().SetSort(bson.D{{Key: "sort", Value: 1}, {Key: "title", Value: 1}}))
	if err != nil {
		return nil, err
	}
	var all, out []Board
	if err := cur.All(ctx, &all); err != nil {
		return nil, err
	}
	for _, b := range all {
		if b.MemberOf(u.ID) || b.Permission == "public" {
			out = append(out, b)
		}
	}
	return out, nil
}

// allBoards is All Boards: the user's boards, and the public boards they
// starred; /allboards/starred only the starred ones (WeKan's sections).
func (s *Server) allBoards(w http.ResponseWriter, r *http.Request, u *User) {
	boards, err := s.boardsOf(r.Context(), u.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	starred, err := s.starredBoards(r.Context(), u)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if r.PathValue("section") == "starred" {
		boards = starred
	} else {
		listed := map[string]bool{}
		for _, b := range boards {
			listed[b.ID] = true
		}
		for _, b := range starred {
			if !listed[b.ID] {
				boards = append(boards, b)
			}
		}
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
	// WeKan's six default labels, unnamed.
	for _, color := range []string{"green", "yellow", "orange", "red", "purple", "blue"} {
		b.Labels = append(b.Labels, Label{ID: randomString(idAlphabet, 6), Color: color})
	}
	if _, err := s.db.Collection("boards").InsertOne(ctx, b); err != nil {
		return nil, err
	}
	sw := Swimlane{ID: randomID(), Title: "Default", BoardID: b.ID, Type: "swimlane"}
	if _, err := s.db.Collection("swimlanes").InsertOne(ctx, sw); err != nil {
		return nil, err
	}
	s.activity(ctx, u.ID, "createBoard", bson.M{"type": "board", "activityTypeId": b.ID, "boardId": b.ID})
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
	if !validID(id) {
		return nil, mongo.ErrNoDocuments
	}
	var b Board
	if err := s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&b); err != nil {
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
	Lane      string // the swimlane this list is shown in
	Cards     []Card
	Collapsed bool // folded by this user, in this swimlane (#6712)
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
	starred := slices.Contains(u.Profile.StarredBoards, b.ID)
	folded := s.collapsedLists(r.Context(), u.ID, b.ID)
	for i := range lanes {
		for j := range lanes[i].Lists {
			ll := &lanes[i].Lists[j]
			v, ok := folded[ll.ID+":"+ll.Lane]
			if !ok {
				v = folded[ll.ID]
			}
			ll.Collapsed = v
		}
	}
	data := map[string]any{"Title": b.Title, "User": u, "Board": b, "Lanes": lanes, "CanEdit": b.Writable(u, "write", nil), "Starred": starred}
	// WeKan's sidebar: the board menu for a member who is not comment-only or a
	// worker; its Export for whoever may export the board (models/lib/exportAccess.js).
	m := b.member(u.ID)
	data["MenuOn"] = m != nil && !m.IsCommentOnly && !m.IsWorker
	data["CanExport"] = (m != nil && !m.AssignedOnly()) || (m == nil && b.Permission == "public")
	lang := languageOf(r, u).Tag
	data["WatchLevel"] = s.watchLevel(r.Context(), b.ID, u.ID)
	// sidebar.jade: the board's activities, for a board admin.
	if b.AdminOf(u.ID) {
		data["BoardActivities"] = s.activityRows(r.Context(), b, bson.M{"boardId": b.ID}, lang, 50)
	}
	if id := r.PathValue("card"); validID(id) {
		var c Card
		if err := s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": eq(id), "boardId": b.ID}).Decode(&c); err == nil {
			data["Card"] = c
			data["Attachments"] = s.cardAttachments(r, b, c.ID)
			data["Comments"] = s.cardComments(r.Context(), b, c.ID)
			data["CardActivities"] = s.activityRows(r.Context(), b, bson.M{"boardId": b.ID, "cardId": c.ID}, lang, 100)
			data["CardMembers"], data["BoardMembers"] = s.cardMemberChoices(r.Context(), b, &c)
			data["Vote"] = s.cardVote(r.Context(), b, u, c.ID)
			data["Poker"] = s.cardPoker(r.Context(), b, u, c.ID, lang)
			data["People"] = s.cardPeople(r.Context(), b, u, &c)
			data["WatchingCard"] = slices.Contains(s.cardWatchers(r.Context(), c.ID), u.ID)
			data["CanComment"] = u.IsAdmin || (b.Can(u.ID, "comment") && (!m.AssignedOnly() || slices.Contains(c.Assignees, u.ID)))
		}
	}
	s.render(w, r, "board", http.StatusOK, data)
}

// memberChoice is a board member in the card's members pop-over.
type memberChoice struct {
	ID, Username, Fullname string
	Selected               bool
}

// cardMemberChoices are the card's members, and every active board member with
// whether they are one of them.
func (s *Server) cardMemberChoices(ctx context.Context, b *Board, c *Card) ([]memberChoice, []memberChoice) {
	var card, board []memberChoice
	for _, m := range b.Members {
		if !m.IsActive {
			continue
		}
		var u User
		if s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(m.UserID)}).Decode(&u) != nil {
			continue
		}
		ch := memberChoice{ID: u.ID, Username: u.Username, Fullname: u.Profile.Fullname, Selected: slices.Contains(c.Members, u.ID)}
		board = append(board, ch)
		if ch.Selected {
			card = append(card, ch)
		}
	}
	return card, board
}

// toggleCardMemberPage adds a board member to the card, or takes them off it.
func (s *Server) toggleCardMemberPage(w http.ResponseWriter, r *http.Request, u *User) {
	b, err := s.boardFor(r.Context(), r.PathValue("board"), u)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var c Card
	id, who := r.PathValue("card"), r.PathValue("user")
	if !validID(id) || s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": eq(id), "boardId": b.ID}).Decode(&c) != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if !b.Writable(u, "write", &c) {
		http.Error(w, "your role on this board cannot change this card", http.StatusForbidden)
		return
	}
	update, kind := bson.M{"$pull": bson.M{"members": who}}, "unjoinMember"
	if !slices.Contains(c.Members, who) {
		if !validID(who) || !b.MemberOf(who) {
			http.Error(w, "only a member of the board can be a card member", http.StatusBadRequest)
			return
		}
		update, kind = bson.M{"$addToSet": bson.M{"members": who}}, "joinMember"
	}
	if _, err := s.db.Collection("cards").UpdateOne(r.Context(), bson.M{"_id": c.ID}, update); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.activity(r.Context(), u.ID, kind, bson.M{"boardId": b.ID, "cardId": c.ID, "memberId": who, "listId": c.ListID, "swimlaneId": c.SwimlaneID})
	s.hub.publish(b.ID, "board-changed")
	http.Redirect(w, r, "/b/"+b.ID+"/"+b.Slug+"/"+c.ID, http.StatusSeeOther)
}

// voteView is a card's vote as the card details show it.
type voteView struct {
	Question           string
	Public, CanVote    bool
	Positive, Negative int
	For, Against       bool
}

// cardVote is the card's vote, nil when it has none. WeKan's showVotingButtons:
// a board member - or anybody, when the vote allows non-members - until it ends.
func (s *Server) cardVote(ctx context.Context, b *Board, u *User, cardID string) *voteView {
	var c struct {
		Vote *struct {
			Question             string     `bson:"question"`
			Public               bool       `bson:"public"`
			AllowNonBoardMembers bool       `bson:"allowNonBoardMembers"`
			Positive             []string   `bson:"positive"`
			Negative             []string   `bson:"negative"`
			End                  *time.Time `bson:"end"`
		} `bson:"vote"`
	}
	if s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(cardID)}).Decode(&c) != nil || c.Vote == nil || c.Vote.Question == "" {
		return nil
	}
	v := c.Vote
	expired := v.End != nil && v.End.Before(time.Now())
	return &voteView{Question: v.Question, Public: v.Public, Positive: len(v.Positive), Negative: len(v.Negative),
		For: slices.Contains(v.Positive, u.ID), Against: slices.Contains(v.Negative, u.ID),
		CanVote: (b.MemberOf(u.ID) || v.AllowNonBoardMembers) && !expired}
}

// votePage is WeKan's cards.vote from a click: the other side, or no vote yet,
// votes for that side; one's own side again takes the vote back.
func (s *Server) votePage(w http.ResponseWriter, r *http.Request, u *User) {
	b, err := s.boardFor(r.Context(), r.PathValue("board"), u)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	id := r.PathValue("card")
	var c Card
	if !validID(id) || s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": eq(id), "boardId": b.ID}).Decode(&c) != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	v := s.cardVote(r.Context(), b, u, c.ID)
	if v == nil || !v.CanVote {
		http.Error(w, "this vote is not open to you", http.StatusForbidden)
		return
	}
	forIt := r.FormValue("vote") == "positive"
	update := bson.M{"$pull": bson.M{"vote.positive": u.ID, "vote.negative": u.ID}}
	if (!v.For && !v.Against) || (v.Against && forIt) || (v.For && !forIt) {
		add, pull := "vote.negative", "vote.positive"
		if forIt {
			add, pull = "vote.positive", "vote.negative"
		}
		update = bson.M{"$pull": bson.M{pull: u.ID}, "$addToSet": bson.M{add: u.ID}}
	}
	if _, err := s.db.Collection("cards").UpdateOne(r.Context(), bson.M{"_id": c.ID}, update); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.hub.publish(b.ID, "board-changed")
	http.Redirect(w, r, "/b/"+b.ID+"/"+b.Slug+"/"+c.ID, http.StatusSeeOther)
}

// boardWatcher is one of a board's watchers: WeKan's complexWatchable.
type boardWatcher struct {
	UserID string `bson:"userId"`
	Level  string `bson:"level"`
}

var watchLevels = []string{"watching", "tracking", "muted"}

// watchLevel is u's level on the board; "muted" - WeKan's default - without one.
func (s *Server) watchLevel(ctx context.Context, boardID, userID string) string {
	var b struct {
		Watchers []boardWatcher `bson:"watchers"`
	}
	_ = s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(boardID)}).Decode(&b)
	for _, w := range b.Watchers {
		if w.UserID == userID {
			return w.Level
		}
	}
	return "muted"
}

// watchBoardPage sets u's level on a board they may see; muted, the default,
// takes their entry away. The list is written whole: the embedded FerretDB has
// no positional $ operator.
func (s *Server) watchBoardPage(w http.ResponseWriter, r *http.Request, u *User) {
	b, err := s.boardFor(r.Context(), r.PathValue("board"), u)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	level := r.FormValue("level")
	if !slices.Contains(watchLevels, level) {
		http.Error(w, "watching, tracking or muted", http.StatusBadRequest)
		return
	}
	var doc struct {
		Watchers []boardWatcher `bson:"watchers"`
	}
	_ = s.db.Collection("boards").FindOne(r.Context(), bson.M{"_id": b.ID}).Decode(&doc)
	list := []boardWatcher{}
	for _, x := range doc.Watchers {
		if x.UserID != u.ID {
			list = append(list, x)
		}
	}
	if level != "muted" {
		list = append(list, boardWatcher{UserID: u.ID, Level: level})
	}
	if _, err := s.db.Collection("boards").UpdateOne(r.Context(), bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"watchers": list}}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/b/"+b.ID+"/"+b.Slug, http.StatusSeeOther)
}

func (s *Server) cardWatchers(ctx context.Context, cardID string) []string {
	var c struct {
		Watchers []string `bson:"watchers"`
	}
	_ = s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(cardID)}).Decode(&c)
	return c.Watchers
}

// watchCardPage starts or stops u watching a card; an assigned-only member
// watches only their own cards (GHSA-r4gm-mjj9-g3x8), and trying another is
// recorded as WeKan does.
func (s *Server) watchCardPage(w http.ResponseWriter, r *http.Request, u *User) {
	b, err := s.boardFor(r.Context(), r.PathValue("board"), u)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	id := r.PathValue("card")
	var c Card
	if !validID(id) || s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": eq(id), "boardId": b.ID}).Decode(&c) != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if m := b.member(u.ID); m != nil && m.AssignedOnly() && !slices.Contains(c.Assignees, u.ID) {
		s.recordSecurity(r, securityEvent{Key: "authz.assigned", Action: "blocked", Source: "watch:card", UserID: u.ID,
			Detail: "Assigned-only member tried to watch a card not assigned to them."})
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	update := bson.M{"$addToSet": bson.M{"watchers": u.ID}}
	if slices.Contains(s.cardWatchers(r.Context(), c.ID), u.ID) {
		update = bson.M{"$pull": bson.M{"watchers": u.ID}}
	}
	if _, err := s.db.Collection("cards").UpdateOne(r.Context(), bson.M{"_id": c.ID}, update); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/b/"+b.ID+"/"+b.Slug+"/"+c.ID, http.StatusSeeOther)
}

// pokerFields are WeKan's Planning Poker cards, in the deck's order: the
// field, the class suffix and the translation key of each.
var pokerFields = [][3]string{{"one", "one", msg("poker-one")}, {"two", "two", msg("poker-two")}, {"three", "three", msg("poker-three")},
	{"five", "five", msg("poker-five")}, {"eight", "eight", msg("poker-eight")}, {"thirteen", "thirteen", msg("poker-thirteen")},
	{"twenty", "twenty", msg("poker-twenty")}, {"forty", "forty", msg("poker-forty")}, {"oneHundred", "one-hundred", msg("poker-oneHundred")},
	{"unsure", "unsure", msg("poker-unsure")}}

type pokerCard struct {
	Field, Class, Label string
	Voted               bool
}

type pokerView struct {
	Question string
	CanVote  bool
	Cards    []pokerCard
}

// cardPoker is the card's Planning Poker, nil when it has none or the board
// does not allow it (allowsPoker, on unless turned off). WeKan's
// showPlanningPokerButtons: a member, or anybody when it allows non-members,
// until it ends.
func (s *Server) cardPoker(ctx context.Context, b *Board, u *User, cardID, lang string) *pokerView {
	var board struct {
		AllowsPoker *bool `bson:"allowsPoker"`
	}
	_ = s.db.Collection("boards").FindOne(ctx, bson.M{"_id": b.ID}).Decode(&board)
	if board.AllowsPoker != nil && !*board.AllowsPoker {
		return nil
	}
	var c struct {
		Poker bson.M `bson:"poker"`
	}
	if s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(cardID)}).Decode(&c) != nil || c.Poker == nil {
		return nil
	}
	q, _ := c.Poker["question"].(string)
	if q == "" {
		return nil
	}
	expired := false
	if end, ok := c.Poker["end"].(bson.DateTime); ok && end.Time().Before(time.Now()) {
		expired = true
	}
	p := &pokerView{Question: q, CanVote: (b.MemberOf(u.ID) || c.Poker["allowNonBoardMembers"] == true) && !expired}
	for _, f := range pokerFields {
		p.Cards = append(p.Cards, pokerCard{Field: f[0], Class: f[1], Label: tr(lang, f[2]), Voted: slices.Contains(stringsOf(c.Poker[f[0]]), u.ID)})
	}
	return p
}

// setPoker is WeKan's: u's choice is that card, and no other.
func (s *Server) setPoker(ctx context.Context, cardID, userID, state string) error {
	pull := bson.M{}
	for _, f := range pokerFields {
		if f[0] != state {
			pull["poker."+f[0]] = userID
		}
	}
	update := bson.M{"$pull": pull, "$set": bson.M{"modifiedAt": time.Now().UTC(), "dateLastActivity": time.Now().UTC()}}
	if slices.ContainsFunc(pokerFields, func(f [3]string) bool { return f[0] == state }) {
		update["$addToSet"] = bson.M{"poker." + state: userID}
	}
	_, err := s.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": eq(cardID)}, update)
	return err
}

func (s *Server) pokerPage(w http.ResponseWriter, r *http.Request, u *User) {
	b, err := s.boardFor(r.Context(), r.PathValue("board"), u)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	id := r.PathValue("card")
	var c Card
	if !validID(id) || s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": eq(id), "boardId": b.ID}).Decode(&c) != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if p := s.cardPoker(r.Context(), b, u, c.ID, "en"); p == nil || !p.CanVote {
		http.Error(w, "this Planning Poker is not open to you", http.StatusForbidden)
		return
	}
	if err := s.setPoker(r.Context(), c.ID, u.ID, r.FormValue("state")); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.hub.publish(b.ID, "board-changed")
	http.Redirect(w, r, "/b/"+b.ID+"/"+b.Slug+"/"+c.ID, http.StatusSeeOther)
}

// addCommentPage is the card details' comment form.
func (s *Server) addCommentPage(w http.ResponseWriter, r *http.Request, u *User) {
	b, err := s.boardFor(r.Context(), r.PathValue("board"), u)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var c Card
	id := r.PathValue("card")
	if !validID(id) || s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": eq(id), "boardId": b.ID}).Decode(&c) != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if _, err := s.AddComment(r.Context(), b, u, &c, r.FormValue("comment")); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errForbidden) {
			code = http.StatusForbidden
		}
		http.Error(w, err.Error(), code)
		return
	}
	http.Redirect(w, r, "/b/"+b.ID+"/"+b.Slug+"/"+c.ID, http.StatusSeeOther)
}

// editableBoard is the board when u's role may change it: add lists and cards.
func (s *Server) editableBoard(w http.ResponseWriter, r *http.Request, u *User) *Board {
	b, err := s.boardFor(r.Context(), r.PathValue("board"), u)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return nil
	}
	if !b.Writable(u, "write", nil) {
		http.Error(w, "your role on this board cannot change it", http.StatusForbidden)
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
	l, err := s.AddList(r.Context(), b, r.FormValue("title"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.activity(r.Context(), u.ID, "createList", bson.M{"boardId": b.ID, "listId": l.ID, "title": l.Title})
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
		if !validID(id) {
			return nil, fmt.Errorf("no such %s on this board", strings.TrimSuffix(coll, "s"))
		}
		if n, _ := s.db.Collection(coll).CountDocuments(ctx, bson.M{"_id": eq(id), "boardId": b.ID}); n == 0 {
			return nil, fmt.Errorf("no such %s on this board", strings.TrimSuffix(coll, "s"))
		}
	}
	number, err := s.nextCardNumber(ctx, b.ID)
	if err != nil {
		return nil, err
	}
	c := &Card{ID: randomID(), Title: title, BoardID: b.ID, ListID: listID, SwimlaneID: swimlaneID, UserID: u.ID,
		Members: []string{}, Assignees: []string{}, LabelIDs: []string{}, CardNumber: number,
		Type: "cardType-card", CreatedAt: time.Now().UTC(),
		Sort: s.nextSort(ctx, "cards", bson.M{"boardId": b.ID, "listId": eq(listID), "swimlaneId": eq(swimlaneID)})}
	if _, err := s.db.Collection("cards").InsertOne(ctx, c); err != nil {
		return nil, err
	}
	s.activity(ctx, u.ID, "createCard", bson.M{"boardId": b.ID, "listId": listID, "swimlaneId": swimlaneID, "cardId": c.ID, // inserted, not a filter
		"cardTitle": c.Title, "listName": s.titleOfID(ctx, "lists", listID), "swimlaneName": s.titleOfID(ctx, "swimlanes", swimlaneID)})
	return c, nil
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
func (s *Server) MoveCards(ctx context.Context, b *Board, u *User, moves []Move) error {
	if len(moves) == 0 || len(moves) > 500 {
		return errors.New("1 to 500 moves")
	}
	for _, m := range moves {
		if !validID(m.CardID) {
			return fmt.Errorf("no card %q on this board", m.CardID)
		}
		for coll, id := range map[string]string{"lists": m.ListID, "swimlanes": m.SwimlaneID} {
			if !validID(id) {
				return fmt.Errorf("no such %s on this board", strings.TrimSuffix(coll, "s"))
			}
			if n, _ := s.db.Collection(coll).CountDocuments(ctx, bson.M{"_id": eq(id), "boardId": b.ID}); n == 0 {
				return fmt.Errorf("no such %s on this board", strings.TrimSuffix(coll, "s"))
			}
		}
		var old Card
		_ = s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(m.CardID), "boardId": b.ID}).Decode(&old)
		res, err := s.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": eq(m.CardID), "boardId": b.ID},
			bson.M{"$set": bson.M{"listId": m.ListID, "swimlaneId": m.SwimlaneID, "sort": m.Sort, "modifiedAt": time.Now().UTC()}})
		if err != nil {
			return err
		}
		if res.MatchedCount == 0 {
			return fmt.Errorf("no card %s on this board", m.CardID)
		}
		if old.ListID != m.ListID || old.SwimlaneID != m.SwimlaneID {
			s.activity(ctx, u.ID, "moveCard", bson.M{"boardId": b.ID, "cardId": old.ID, "cardTitle": old.Title,
				"oldListId": old.ListID, "listId": m.ListID, "listName": s.titleOfID(ctx, "lists", m.ListID),
				"oldSwimlaneId": old.SwimlaneID, "swimlaneId": m.SwimlaneID, "swimlaneName": s.titleOfID(ctx, "swimlanes", m.SwimlaneID),
				"moveReason": ""})
		}
	}
	return nil
}

// moveCards: the moveCard capability, which a Worker has too; an assigned-only
// member moves only the cards assigned to them.
func (s *Server) moveCards(w http.ResponseWriter, r *http.Request, u *User) {
	b, err := s.boardFor(r.Context(), r.PathValue("board"), u)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var req struct {
		Moves []Move `json:"moves"`
	}
	if err := decodeJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	for _, m := range req.Moves {
		var c Card
		if validID(m.CardID) {
			_ = s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": eq(m.CardID), "boardId": b.ID}).Decode(&c)
		}
		if !b.Writable(u, "moveCard", &c) {
			http.Error(w, "your role on this board cannot move this card", http.StatusForbidden)
			return
		}
	}
	if err := s.MoveCards(r.Context(), b, u, req.Moves); err != nil {
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
	// A ResponseController, not w.(http.Flusher): inside Caddy, w is Caddy's
	// wrapper, and the controller reaches the connection's Flush through it.
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	ch := s.hub.subscribe(b.ID)
	defer s.hub.unsubscribe(b.ID, ch)
	fmt.Fprint(w, ": connected\n\n")
	if err := rc.Flush(); err != nil {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
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
			rc.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			rc.Flush()
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

// cardPerson is the card's Requested By or Assigned By (cardDetails.jade): who,
// as free text, on a board that shows it.
type cardPerson struct {
	Field, Label, Icon, Value string
	Editable                  bool
}

// cardPeopleFields are WeKan's two, each with the board setting that shows it.
var cardPeopleFields = []struct{ Field, Allows, Label, Icon string }{
	{"requestedBy", "allowsRequestedBy", msg("requested-by"), "fa-shopping-cart"},
	{"assignedBy", "allowsAssignedBy", msg("assigned-by"), "fa-user-plus"},
}

// cardPeople are the card's Requested By and Assigned By that its board
// shows - both by default - editable for whoever may change the card but a
// worker (WeKan's canModifyCard, unless isWorker).
func (s *Server) cardPeople(ctx context.Context, b *Board, u *User, c *Card) []cardPerson {
	var board, card bson.M
	_ = s.db.Collection("boards").FindOne(ctx, bson.M{"_id": b.ID}).Decode(&board)
	_ = s.db.Collection("cards").FindOne(ctx, bson.M{"_id": c.ID}).Decode(&card)
	m := b.member(u.ID)
	editable := b.Writable(u, "write", c) && (m == nil || !m.IsWorker)
	var out []cardPerson
	for _, f := range cardPeopleFields {
		if board[f.Allows] == false {
			continue
		}
		v, _ := card[f.Field].(string)
		out = append(out, cardPerson{Field: f.Field, Label: f.Label, Icon: f.Icon, Value: v, Editable: editable})
	}
	return out
}

// cardPeoplePage saves the card's Requested By or Assigned By.
func (s *Server) cardPeoplePage(w http.ResponseWriter, r *http.Request, u *User) {
	b, err := s.boardFor(r.Context(), r.PathValue("board"), u)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	id := r.PathValue("card")
	var c Card
	if !validID(id) || s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": eq(id), "boardId": b.ID}).Decode(&c) != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	field, value := r.FormValue("field"), strings.TrimSpace(r.FormValue("value"))
	var person *cardPerson
	for _, p := range s.cardPeople(r.Context(), b, u, &c) {
		if p.Field == field {
			person = &p
		}
	}
	if person == nil || !person.Editable || len(value) > 1000 {
		http.Error(w, "this field is not yours to change", http.StatusForbidden)
		return
	}
	if _, err := s.db.Collection("cards").UpdateOne(r.Context(), bson.M{"_id": c.ID},
		bson.M{"$set": bson.M{person.Field: value, "modifiedAt": time.Now().UTC()}}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.hub.publish(b.ID, "board-changed")
	http.Redirect(w, r, "/b/"+b.ID+"/"+b.Slug+"/"+c.ID, http.StatusSeeOther)
}

// addSwimlanePage is WeKan's swimlaneAddPopup: a swimlane below - or above -
// the chosen one, sorted between it and its neighbour; at the end without one.
func (s *Server) addSwimlanePage(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.editableBoard(w, r, u)
	if b == nil {
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" || len(title) > 1000 {
		http.Error(w, "a swimlane needs a title", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	cur, err := s.db.Collection("swimlanes").Find(ctx, bson.M{"boardId": b.ID, "archived": false}, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}}))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var lanes []Swimlane
	_ = cur.All(ctx, &lanes)
	sort := s.nextSort(ctx, "swimlanes", bson.M{"boardId": b.ID})
	if i := slices.IndexFunc(lanes, func(l Swimlane) bool { return l.ID == r.FormValue("after") }); i >= 0 {
		if r.FormValue("placement") == "above" {
			sort = lanes[i].Sort - 1
			if i > 0 {
				sort = (lanes[i-1].Sort + lanes[i].Sort) / 2
			}
		} else {
			sort = lanes[i].Sort + 1
			if i+1 < len(lanes) {
				sort = (lanes[i].Sort + lanes[i+1].Sort) / 2
			}
		}
	}
	now := time.Now().UTC()
	sw := bson.M{"_id": randomID(), "title": title, "boardId": b.ID, "archived": false, "type": "swimlane", // inserted, not a filter
		"sort": sort, "createdAt": now, "modifiedAt": now} // inserted, not a filter
	if _, err := s.db.Collection("swimlanes").InsertOne(ctx, sw); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.activity(ctx, u.ID, "createSwimlane", bson.M{"boardId": b.ID, "swimlaneId": sw["_id"]})
	s.hub.publish(b.ID, "board-changed")
	http.Redirect(w, r, "/b/"+b.ID+"/"+b.Slug, http.StatusSeeOther)
}
