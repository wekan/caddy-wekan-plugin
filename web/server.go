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
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	db      *mongo.Database
	release func() error
	pages   map[string]*template.Template
	hub     *hub
	// boardWatchOff stops the board watch's looks (board_watch.go), for a
	// test that looks itself.
	boardWatchOff atomic.Bool
	throttle      loginThrottle
	mux           *http.ServeMux
	log           *slog.Logger
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
	// stopMail ends the notification e-mail outbox's pass (mail_outbox.go).
	stopMail chan struct{}
	// framing: FrameBleed's headers, on every response (frame_policy.go).
	framing map[string]string
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
	s := &Server{db: db, pages: map[string]*template.Template{}, hub: newHub(), log: log, framing: framingHeadersFromEnv()}
	if err := s.compileTemplates(); err != nil {
		return nil, err
	}
	if err := s.ensureSettings(context.Background()); err != nil {
		return nil, err
	}
	s.routes()
	s.emailIndexes(context.Background())
	// WeKan's indexes, for a database Weddy alone runs (#6892) - before the
	// first request, as WeKan does at startup: on SQLite a query that meets a
	// CREATE INDEX fails with "database schema has changed". Each is created
	// once; on later starts it already exists and the call returns at once.
	if n := s.ensureWekanIndexes(context.Background()); n > 0 {
		s.log.Debug("wekan indexes: some were not created", "count", n)
	}
	s.stopMail = make(chan struct{})
	go s.runEmailOutbox(s.stopMail)
	go s.runActivityRecovery(s.stopMail)
	go s.runActivityPlanCleanup(s.stopMail)
	go s.runEmailReceipts(s.stopMail)
	go s.runScheduledRules(s.stopMail)
	go s.runWebhookQueue(s.stopMail)
	go s.runScrumDaily(s.stopMail)
	go s.resumeRollovers()
	go s.resumeBatchJobs()
	s.hub.stamp = func(board string) string { return s.boardStamp(context.Background(), board) }
	go s.runBoardWatch(s.stopMail)
	return s, nil
}

// ensureSettings makes the site's one settings document when there is none,
// with Node.js WeKan's defaults (server/models/settings.js, at startup), so
// Admin Panel and WeKan's tools always find it. Mail is off until set; its
// sender is left to Admin Panel, as Weddy serves more than one domain.
func (s *Server) ensureSettings(ctx context.Context) error {
	// WeKan's collectionBootstrap.js: Admin Panel's account switches, off until
	// an admin turns them on - an existing one is never changed.
	for i, id := range []string{"accounts-allowEmailChange", "accounts-allowUserNameChange", "accounts-allowUserDelete"} {
		sort := 0
		if i == 1 {
			sort = 1
		}
		if _, err := s.db.Collection("accountSettings").UpdateOne(ctx, bson.M{"_id": eq(id)},
			bson.M{"$setOnInsert": bson.M{"booleanValue": false, "sort": sort}}, options.UpdateOne().SetUpsert(true)); err != nil {
			return err
		}
	}
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
	if s.stopMail != nil {
		close(s.stopMail)
		s.stopMail = nil
	}
	if s.release != nil {
		return s.release()
	}
	return nil
}

var funcs = template.FuncMap{
	"memberName": func(u *User) string { return u.Name() },
	// avatarURL is WeKan's avatarDisplayUrl, for a stored profile.avatarUrl.
	"avatarURL": avatarDisplayURL,
	"t":         tr,
	// tv is t with WeKan's __name__ placeholders filled: tv lang key name value ...
	"tv": trVars,
	// viewer is WeKan's +viewer: a text written in Markdown, shown safely.
	"viewer": viewer,
	// attachmentURL is getAttachmentUrl, for the attachment menu.
	"attachmentURL": attachmentURL,
	// visibilityIcon is WeKan's: the lock, the users, or the globe.
	"visibilityIcon": visibilityIcon,
	// copula is WeKan's ruleTriggerCopula: the linking verb of a rule's trigger,
	// none where the language's own phrase already holds it.
	"copula": func(lang string) string {
		if regexp.MustCompile(`(?i)^(?:eu|th|kn|ml|la|ga)(?:[-_]|$)`).MatchString(lang) {
			return ""
		}
		return tr(lang, msg("r-is"))
	},
	// nameBeforeSubject is WeKan's ruleNameBeforeSubject: a Basque named
	// subject ends with its demonstrative, so the name comes first.
	"nameBeforeSubject": func(lang string) bool { return regexp.MustCompile(`(?i)^eu(?:[-_]|$)`).MatchString(lang) },
	// watching is whether the user is one of the watchers.
	"watching": func(u *User, watchers []string) bool { return u != nil && slices.Contains(watchers, u.ID) },
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
	// templates/_<name>.pug are partials: every page may call them, as
	// {{template "_<name>" .}}.
	partials := map[string]string{}
	for _, e := range entries {
		if name := strings.TrimSuffix(e.Name(), ".pug"); strings.HasPrefix(name, "_") {
			src, err := templateFiles.ReadFile("templates/" + e.Name())
			if err != nil {
				return err
			}
			if partials[name], err = jade.Parse(name, src); err != nil {
				return fmt.Errorf("%s: %w", e.Name(), err)
			}
		}
	}
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".pug")
		if name == "layout" || strings.HasPrefix(name, "_") {
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
		for pn, psrc := range partials {
			if _, err := t.New(pn).Parse(psrc); err != nil {
				return fmt.Errorf("%s.pug: %w", pn, err)
			}
		}
		s.pages[name] = t
	}
	return nil
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, page string, status int, data map[string]any) {
	u, _ := data["User"].(*User)
	lang := languageOf(r, u)
	data["L"], data["Lang"] = lang.Tag, lang
	data["TextPolicy"] = s.textPolicy(r.Context())
	// The password fields' minlength: the enforced minimum, else none - WeKan's
	// default only reports a short password (users.go).
	if p := s.passwordPolicy(r.Context()); p.Enforce {
		data["PasswordMin"] = p.MinLength
	} else {
		data["PasswordMin"] = 0
	}
	if u != nil {
		data["Unread"] = s.unreadNotifications(r.Context(), u.ID)
		data["Drawer"] = s.drawerGroups(r.Context(), u, s.drawerItems(r.Context(), u, lang.Tag), lang.Tag)
		data["CloudMenu"] = s.cloudOn() && u.IsAdmin
		// The header's Create Board icon: for whoever may make a board -
		// checked, not attempted, so nothing is recorded (boardpolicy.go).
		data["MayCreateBoard"] = u.IsAdmin || !s.tableSetting(r.Context(), "tableVisibilityMode-boardCreationAdminOnly")
		data["PanelMenu"] = s.panelAllowed(r, u)
		data["AllowEmailChange"] = s.accountSetting(r.Context(), "accounts-allowEmailChange")
		data["Email"] = ""
		if len(u.Emails) > 0 {
			data["Email"] = u.Emails[0].Address
		}
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
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setFramingHeaders(w, s.framing)
	s.mux.ServeHTTP(w, r)
}

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
	m.HandleFunc("GET /b/{board}/{slug}/rules", s.page(s.rulesPage))
	m.HandleFunc("GET /b/{board}/{slug}/scrum", s.page(s.scrumPage))
	m.HandleFunc("GET /b/{board}/{slug}/notifications", s.page(s.boardNotificationsPage))
	m.HandleFunc("POST /b/{board}/notifications", s.page(s.saveBoardDelivery))
	m.HandleFunc("GET /b/{board}/{slug}/lists/{list}/permissions", s.page(s.listPermissionsPage))
	m.HandleFunc("POST /b/{board}/lists/{list}/permissions", s.page(s.saveListPermissions))
	m.HandleFunc("POST /b/{board}/integrations/{hook}/notifications", s.page(s.saveHookDelivery))
	m.HandleFunc("GET /admin/notifications", s.page(s.adminNotificationsPage))
	m.HandleFunc("POST /admin/notifications", s.page(s.saveAdminDelivery))
	m.HandleFunc("GET /b/{board}/{slug}/rules/export", s.page(s.rulesExport))
	m.HandleFunc("POST /b/{board}/cards/{card}/people", s.page(s.cardPeoplePage))
	m.HandleFunc("POST /b/{board}/cards/{card}/archive", s.page(s.archiveCardPage))
	m.HandleFunc("POST /b/{board}/cards/{card}/title", s.page(s.cardTitlePage))
	s.registerChecklistWrites(m)
	s.cardDateRoutes(m)
	s.cardCustomFieldRoutes(m)
	m.HandleFunc("POST /b/{board}/cards/{card}/complete", s.page(s.cardCompletePage))
	m.HandleFunc("POST /b/{board}/title", s.page(s.renameBoardPage))
	m.HandleFunc("POST /b/{board}/restore", s.page(s.restoreBoardPage))
	m.HandleFunc("POST /b/{board}/visibility", s.page(s.visibilityPage))
	m.HandleFunc("POST /b/{board}/cards/{card}/labels/{label}", s.page(s.toggleCardLabelPage))
	m.HandleFunc("POST /b/{board}/labels", s.page(s.createLabelPage))
	m.HandleFunc("POST /b/{board}/archive/restore", s.page(s.restoreArchivedPage))
	m.HandleFunc("POST /b/{board}/archive/restore-all-cards", s.page(s.restoreAllArchivedCardsPage))
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
	s.problemsRoutes(m)
	s.historyViewRoutes(m)
	m.HandleFunc("POST /language", s.page(s.setLanguage))
	s.apiRoutes(m)
	s.cloudRoutes(m)
	s.attachmentRoutes(m)
	s.avatarRoutes(m)
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
		// A short password while the minimum is enforced: the change first.
		if s.passwordChangeGate(w, r, u) {
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
	s.noteSignInPassword(r, u, password)
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
	listed := map[string]bool{}
	for _, b := range boards {
		listed[b.ID] = true
	}
	remaining := slices.Clone(boards)
	for _, b := range starred {
		if !listed[b.ID] {
			remaining = append(remaining, b)
		}
	}
	templates, _ := s.templateBoardsOf(r.Context(), u.ID)
	archived, _ := s.archivedBoardsOf(r.Context(), u.ID)
	section := strings.SplitN(r.PathValue("section"), "/", 2)[0]
	if !slices.Contains([]string{"starred", "templates", "remaining", "archive"}, section) {
		section = "remaining"
		if len(starred) > 0 {
			section = "starred" // WeKan opens on the starred boards when there are some
		}
	}
	boards = map[string][]Board{"starred": starred, "templates": templates, "remaining": remaining, "archive": archived}[section]
	sections := []boardSection{{"starred", msg("allboards.starred"), "fa-star", len(starred)},
		{"templates", msg("allboards.templates"), "fa-clone", len(templates)},
		{"remaining", msg("allboards.remaining"), "fa-th-large", len(remaining)}}
	s.render(w, r, "boards", http.StatusOK, map[string]any{"Title": tr(languageOf(r, u).Tag, "all-boards"), "User": u, "Boards": boards,
		"Sections": sections, "Section": section})
}

// boardSection is one of All Boards' sections.
type boardSection struct {
	Type, Label, Icon string
	Count             int
}

// archivedBoardsOf are the archived boards the user is an active member of.
func (s *Server) archivedBoardsOf(ctx context.Context, userID string) ([]Board, error) {
	cur, err := s.db.Collection("boards").Find(ctx, bson.M{"archived": true, "members": bson.M{"$elemMatch": bson.M{"userId": eq(userID), "isActive": true}}},
		options.Find().SetSort(bson.D{{Key: "title", Value: 1}}))
	if err != nil {
		return nil, err
	}
	var boards []Board
	return boards, cur.All(ctx, &boards)
}

// restoreBoardPage brings a board back from the Archive, for its admin.
func (s *Server) restoreBoardPage(w http.ResponseWriter, r *http.Request, u *User) {
	id := r.PathValue("board")
	var b Board
	if !validID(id) || s.db.Collection("boards").FindOne(r.Context(), bson.M{"_id": eq(id), "archived": true}).Decode(&b) != nil || !b.AdminOf(u.ID) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if _, err := s.db.Collection("boards").UpdateOne(r.Context(), bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"archived": false, "modifiedAt": time.Now().UTC()}}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/b/"+b.ID+"/"+b.Slug, http.StatusSeeOther)
}

// templateBoardsOf are the user's template boards.
func (s *Server) templateBoardsOf(ctx context.Context, userID string) ([]Board, error) {
	cur, err := s.db.Collection("boards").Find(ctx, bson.M{"archived": false, "members": bson.M{"$elemMatch": bson.M{"userId": eq(userID), "isActive": true}},
		"type": bson.M{"$in": []string{"template-container", "template-board"}}}, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}, {Key: "title", Value: 1}}))
	if err != nil {
		return nil, err
	}
	var boards []Board
	return boards, cur.All(ctx, &boards)
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
	if err := s.mayCreateBoard(ctx, u, "board", "createBoard"); err != nil {
		return nil, err
	}
	b, err := s.insertBoard(ctx, u, title, "board", s.boardPermission(ctx, "private", "createBoard"))
	if err != nil {
		return nil, err
	}
	sw := Swimlane{ID: randomID(), Title: "Default", BoardID: b.ID, Type: "swimlane"}
	if _, err := s.db.Collection("swimlanes").InsertOne(ctx, sw); err != nil {
		return nil, err
	}
	s.activity(ctx, u.ID, "createBoard", bson.M{"type": "board", "activityTypeId": b.ID, "boardId": b.ID})
	return b, nil
}

// insertBoard makes the board itself: its maker its admin, WeKan's six
// default labels, unnamed. The caller has decided that u may make it.
func (s *Server) insertBoard(ctx context.Context, u *User, title, typ, permission string) (*Board, error) {
	now := time.Now().UTC()
	b := &Board{ID: randomID(), Title: title, Slug: slugify(title), Permission: permission, Type: typ, CreatedAt: now,
		Members: []Member{{UserID: u.ID, IsAdmin: true, IsActive: true}}}
	// WeKan's six default labels, unnamed.
	for _, color := range []string{"green", "yellow", "orange", "red", "purple", "blue"} {
		b.Labels = append(b.Labels, Label{ID: randomString(idAlphabet, 6), Color: color})
	}
	if _, err := s.db.Collection("boards").InsertOne(ctx, b); err != nil {
		return nil, err
	}
	return b, nil
}

func (s *Server) createBoard(w http.ResponseWriter, r *http.Request, u *User) {
	// WeKan's createBoardForm with its multiline title choice: one board, or
	// - "Many" chosen - one a line, in order; the page goes to the last made.
	titles := creationTitles(r.FormValue("title"), r.FormValue("titleMode") == "separate")
	if len(titles) == 0 {
		http.Error(w, "a board needs a title", http.StatusBadRequest)
		return
	}
	var b *Board
	for _, title := range titles {
		var err error
		b, err = s.CreateBoard(r.Context(), u, title)
		if errors.Is(err, errBoardCreation) {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	http.Redirect(w, r, "/b/"+b.ID+"/"+b.Slug, http.StatusSeeOther)
}

// creationTitles is WeKan's models/lib/multilineTitles.js creationTitles: the
// text as one title, or one a line with blank lines left out.
func creationTitles(value string, separate bool) []string {
	title := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(value, "\r\n", "\n"), "\r", "\n"))
	if !separate {
		if title == "" {
			return nil
		}
		return []string{title}
	}
	var out []string
	for _, line := range strings.Split(title, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
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
	// #3249: an instance board is read by every signed-in user - u is one -
	// and a public board by anybody; changing either still needs membership.
	if !b.MemberOf(u.ID) && b.Permission != "public" && b.Permission != "instance" {
		return nil, mongo.ErrNoDocuments // not yours: as if it did not exist
	}
	return &b, nil
}

// visibilityIcon is WeKan's Board.visibilityIcon.
func visibilityIcon(permission string) string {
	switch permission {
	case "public":
		return "fa-globe"
	case "instance":
		return "fa-users"
	}
	return "fa-lock"
}

// visibilityPage is WeKan's boardChangeVisibilityPopup: a board admin makes
// the board private, visible to signed-in users, or public - never open while
// Admin Panel allows only private boards (refused and recorded).
func (s *Server) visibilityPage(w http.ResponseWriter, r *http.Request, u *User) {
	b, err := s.boardFor(r.Context(), r.PathValue("board"), u)
	if err != nil || !b.AdminOf(u.ID) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	perm := r.FormValue("visibility")
	if perm != "private" && perm != "instance" && perm != "public" {
		http.Error(w, "private, instance or public", http.StatusBadRequest)
		return
	}
	if s.boardPermission(r.Context(), perm, "boardChangeVisibility") != perm {
		http.Error(w, "only private boards are allowed", http.StatusForbidden)
		return
	}
	if _, err := s.db.Collection("boards").UpdateOne(r.Context(), bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"permission": perm, "modifiedAt": time.Now().UTC()}}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	http.Redirect(w, r, "/b/"+b.ID+"/"+b.Slug, http.StatusSeeOther)
}

// lane is a swimlane with its lists, and each list with its cards in that lane.
type lane struct {
	Swimlane
	Lists []laneList
	// Collapsed: the user folded the swimlane (profile.collapsedSwimlanes).
	Collapsed bool
}

type laneList struct {
	List
	Lane      string // the swimlane this list is shown in
	Cards     []Card
	Collapsed bool // folded by this user, in this swimlane (#6712)
	CanAdd    bool // this user may add a card here: the list's rule, else the board's (#4527)
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
	if err := find("swimlanes", bson.M{"boardId": b.ID, "archived": false, "deletedAt": nil}, &swimlanes); err != nil {
		return nil, nil, err
	}
	if err := find("lists", bson.M{"boardId": b.ID, "archived": false, "deletedAt": nil}, &lists); err != nil {
		return nil, nil, err
	}
	if err := find("cards", bson.M{"boardId": b.ID, "archived": false, "deletedAt": nil}, &cards); err != nil {
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
	// The Filter sidebar's Advanced Filter: the typed one when it parses, else
	// the last that did (?last=), as WeKan's sidebar keeps its last selector.
	typed, applied := strings.TrimSpace(r.URL.Query().Get("advanced")), ""
	if len(typed) > 2000 {
		typed = typed[:2000]
	}
	var shown map[string]bool
	for _, f := range []string{typed, strings.TrimSpace(r.URL.Query().Get("last"))} {
		if f == "" {
			continue
		}
		if ids, ok := s.filteredCardIDs(r.Context(), b.ID, f); ok {
			shown, applied = ids, f
			break
		}
	}
	if shown != nil {
		for i := range lanes {
			for j := range lanes[i].Lists {
				ll := &lanes[i].Lists[j]
				kept := ll.Cards[:0]
				for _, c := range ll.Cards {
					if shown[c.ID] {
						kept = append(kept, c)
					}
				}
				ll.Cards = kept
			}
		}
	}
	s.annotateMinicards(r.Context(), b, lanes, u.Profile.DateFormat)
	s.annotateScrumMinicards(r.Context(), b, languageOf(r, u).Tag, lanes)
	starred := slices.Contains(u.Profile.StarredBoards, b.ID)
	invited := slices.Contains(u.Profile.InvitedBoards, b.ID)
	folded := s.collapsedLists(r.Context(), u.ID, b.ID)
	folds := s.foldsOf(r.Context(), u.ID, b.ID)
	for i := range lanes {
		lanes[i].Collapsed = folds.Swimlanes[lanes[i].ID]
		for j := range lanes[i].Lists {
			ll := &lanes[i].Lists[j]
			for k := range ll.Cards {
				c := &ll.Cards[k]
				c.MinicardCollapsed = folds.Cards[c.ID]
				for x := range c.MinicardChecklists {
					c.MinicardChecklists[x].Collapsed = folds.Sections[c.ID]["checklist-"+c.MinicardChecklists[x].ID]
				}
			}
			v, ok := folded[ll.ID+":"+ll.Lane]
			if !ok {
				v = folded[ll.ID]
			}
			ll.Collapsed = v
			ll.CanAdd = s.canAddCardIn(r.Context(), b, ll.ID, u)
		}
	}
	data := map[string]any{"Title": b.Title, "User": u, "Board": b, "Lanes": lanes, "CanEdit": b.Writable(u, "write", nil), "Starred": starred, "Invited": invited,
		"AdvancedFilter": typed, "AppliedFilter": applied, "FilterActive": applied != ""}
	// WeKan's sidebar: the board menu for a member who is not comment-only or a
	// worker; its Export for whoever may export the board (models/lib/exportAccess.js).
	m := b.member(u.ID)
	data["MenuOn"] = m != nil && !m.IsCommentOnly && !m.IsWorker
	data["CanExport"] = (m != nil && !m.AssignedOnly()) || (m == nil && b.Permission == "public")
	data["ExportTools"] = exportTools
	lang := languageOf(r, u).Tag
	// What to include: every section, as WeKan's selection starts.
	data["ExportFields"], data["ExportFieldsAll"] = exportFieldRows(lang), strings.Join(exportFieldKeys, ",")
	data["WatchLevel"] = s.watchLevel(r.Context(), b.ID, u.ID)
	data["SidebarMembers"] = s.sidebarMembers(r, b, u)
	data["Archived"] = s.archivedItems(r, b, lang)
	// A restore its list refused: that card's popup, opened on load.
	if id := r.URL.Query().Get("restoreBlocked"); validID(id) {
		data["RestoreBlocked"] = id
	}
	// Restore all left cards out: WeKan's restoreAllSkippedPopup, opened on load.
	if n, err := strconv.Atoi(r.URL.Query().Get("restoreSkipped")); err == nil && n > 0 {
		data["RestoreSkipped"] = strings.ReplaceAll(tr(lang, "restore-all-skipped-wip-limit"), "__count__", strconv.Itoa(n))
	}
	data["CanInvite"] = canInviteToBoard(u, b, s.inviteRoles(r))
	data["IsBoardAdmin"] = b.AdminOf(u.ID)
	data["CardSettings"] = s.cardSettingsPanes(r.Context(), b, u, lang)
	// The board as this page draws it, for its live connection (board_watch.go).
	data["BoardStamp"] = s.boardStamp(r.Context(), b.ID)
	data["Views"] = s.boardViewEntries(b, lang, "")
	// A stopped Scrum History operation blocks the board's Scrum writes
	// (historyRecoveryNotice.jade): who may resolve it, and how.
	if cp, err := s.inspectCheckpointFor(r.Context(), u, b.ID); err == nil && cp != nil && cp["stuck"] == true {
		if cp["canResolve"] == true {
			cp["Counts"] = strings.NewReplacer("__applied__", scrumNum(cp["applied"]), "__total__", scrumNum(cp["total"]),
				"__pending__", scrumNum(cp["pending"]), "__conflicted__", scrumNum(cp["conflicted"])).Replace(tr(lang, msg("scrum-history-checkpoint-counts")))
		}
		data["HistoryCheckpoint"] = cp
	}
	data["PrivateOnly"] = s.tableSetting(r.Context(), "tableVisibilityMode-allowPrivateOnly")
	if b.Writable(u, "write", nil) || (m != nil && b.Can(u.ID, "write")) {
		data["BoardButtons"] = s.buttonRules(r.Context(), b.ID, "board")
		data["CardButtons"] = s.buttonRules(r.Context(), b.ID, "card")
	}
	// sidebar.jade: the board's activities, for a board admin.
	if b.AdminOf(u.ID) {
		data["BoardActivities"] = s.activityRows(r.Context(), b, bson.M{"boardId": b.ID}, lang, 50)
	}
	if id := r.PathValue("card"); validID(id) {
		var c Card
		if err := s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": eq(id), "boardId": b.ID}).Decode(&c); err == nil {
			data["Card"] = c
			data["CardSides"] = s.sidesOfOpenedCard(r.Context(), b, &c, u)
			// The card's Scrum fields, and their editor for whoever may change it.
			if n := s.scrumNamesOf(r.Context(), b.ID); len(n.visibility) > 0 {
				lang := languageOf(r, u).Tag
				data["CardScrum"] = n.rows(lang, "card", plainScrum(c.Scrum), false)
				if b.Writable(u, "write", &c) && !s.listRefuses(r.Context(), b, c.ListID, u.ID, "write") {
					data["CardScrumEditors"] = n.rows(lang, "card", plainScrum(c.Scrum), true)
				}
			}
			data["Attachments"] = s.cardAttachments(r, b, c.ID)
			data["CanAddAttachment"] = b.WritableIn(r.Context(), s, u, "write", &c)
			data["Comments"] = s.cardComments(r.Context(), b, c.ID)
			data["Checklists"] = s.cardChecklists(r.Context(), c.ID)
			data["Subtasks"], data["SubtaskCount"] = s.cardSubtasks(r.Context(), u, c.ID)
			data["CanModifyCard"] = b.WritableIn(r.Context(), s, u, "write", &c)
			data["CanCheckItems"] = s.canCheckChecklistItem(r.Context(), b, u, &c)
			data["ChecklistDue"] = s.boardAllows(r.Context(), b.ID, "allowsChecklistDueDate")
			data["DateFormat"] = u.Profile.DateFormat
			var rawCard, rawBoard bson.M
			_ = s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": eq(c.ID)}).Decode(&rawCard)
			_ = s.db.Collection("boards").FindOne(r.Context(), bson.M{"_id": eq(b.ID)}).Decode(&rawBoard)
			data["CardDates"] = orderCardDates(s.cardDates(r.Context(), b.ID, rawCard), rawBoard)
			data["CanSetDates"] = s.canSetCardDates(r.Context(), b, u, &c)
			// cardFieldSectionDescription: shown to an editor always, to anyone
			// else when there is a description; its title and text as the
			// board allows (allowsDescriptionTitle, allowsDescriptionText).
			canDesc := s.canSetCardDates(r.Context(), b, u, &c)
			data["CanEditDescription"] = canDesc
			data["DescriptionTitle"] = rawBoardAllows(rawBoard, "allowsDescriptionTitle")
			data["DescriptionText"] = rawBoardAllows(rawBoard, "allowsDescriptionText")
			data["DescriptionShown"] = canDesc || c.Description != ""
			// cardFieldSectionCustomFields: for whoever may change the card, on
			// a board that allows custom fields (canShowCustomFieldsOnCard).
			if b.WritableIn(r.Context(), s, u, "write", &c) && rawBoardAllows(rawBoard, "allowsCustomFields") {
				data["CardFieldsShown"] = true
				data["CardFields"], data["BoardFields"] = s.cardCustomFields(r.Context(), b, u, rawCard, u.Profile.DateFormat)
			}
			data["CardActivities"] = s.activityRows(r.Context(), b, bson.M{"boardId": b.ID, "cardId": c.ID}, lang, 100)
			data["CardMembers"], data["BoardMembers"] = s.cardMemberChoices(r.Context(), b, &c)
			if cls, ok := data["Checklists"].([]cardChecklist); ok {
				bm, _ := data["BoardMembers"].([]memberChoice)
				nameChecklistAssignees(cls, bm)
			}
			data["Vote"] = s.cardVote(r.Context(), b, u, c.ID)
			data["Poker"] = s.cardPoker(r.Context(), b, u, c.ID, lang)
			data["People"] = s.cardPeople(r.Context(), b, u, &c)
			data["CardLabels"] = cardLabels(b, &c)
			data["TargetBoards"] = s.targetBoards(r, u, b)
			data["Lists"], data["Swimlanes"] = s.boardPlaces(r, b.ID)
			data["LabelPalette"] = labelPalette
			data["WatchingCard"] = slices.Contains(s.cardWatchers(r.Context(), c.ID), u.ID)
			data["CanComment"] = s.canCommentIn(r.Context(), b, u, &c)
			data["CardSections"] = cardSectionsOf(data, rawBoard, lang)
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
	if !b.WritableIn(r.Context(), s, u, "write", &c) {
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
	if _, err := s.cardsUpdateOne(r.Context(), u.ID, bson.M{"_id": c.ID}, update); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.activity(r.Context(), u.ID, kind, bson.M{"boardId": b.ID, "cardId": c.ID, "memberId": who, "listId": c.ListID, "swimlaneId": c.SwimlaneID})
	s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
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
	if _, err := s.cardsUpdateOne(r.Context(), u.ID, bson.M{"_id": c.ID}, update); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
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
	_, err := s.cardsUpdateOne(ctx, userID, bson.M{"_id": eq(cardID)}, update)
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
	s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
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
	s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	http.Redirect(w, r, "/b/"+b.ID+"/"+b.Slug, http.StatusSeeOther)
}

// AddCard adds a card at the end of a list in a swimlane.
func (s *Server) AddCard(ctx context.Context, b *Board, u *User, listID, swimlaneID, title string) (*Card, error) {
	return s.AddCardWith(ctx, b, u, listID, swimlaneID, title, nil)
}

// AddCardWith is AddCard with more of the card's fields - a due date, a sort -
// stored before its createCard activity, so the rules see the card whole.
func (s *Server) AddCardWith(ctx context.Context, b *Board, u *User, listID, swimlaneID, title string, extra bson.M) (*Card, error) {
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
	if len(extra) > 0 {
		if _, err := s.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": c.ID}, bson.M{"$set": extra}); err != nil {
			return nil, err
		}
	}
	s.activity(ctx, u.ID, "createCard", bson.M{"boardId": b.ID, "listId": listID, "swimlaneId": swimlaneID, "cardId": c.ID, // inserted, not a filter
		"cardTitle": c.Title, "listName": s.titleOfID(ctx, "lists", listID), "swimlaneName": s.titleOfID(ctx, "swimlanes", swimlaneID)})
	return c, nil
}

func (s *Server) addCard(w http.ResponseWriter, r *http.Request, u *User) {
	ctx := r.Context()
	b, err := s.boardFor(ctx, r.PathValue("board"), u)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// The list's rule for the user's role, else the board's: a list may grant
	// a role the board does not let write (#4527), or refuse one it does.
	if !s.canAddCardIn(ctx, b, r.FormValue("listId"), u) {
		if s.refuseInList(ctx, b, r.FormValue("listId"), u, "write", "page:add-card") {
			http.Error(w, "this list does not let your role add cards", http.StatusForbidden)
			return
		}
		http.Error(w, "your role on this board cannot change it", http.StatusForbidden)
		return
	}
	top := s.topSort(ctx, b.ID, r.FormValue("listId"))
	c, err := s.AddCard(ctx, b, u, r.FormValue("listId"), r.FormValue("swimlaneId"), r.FormValue("title"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Add Card to Top of List: before the list's first card; otherwise last.
	if r.FormValue("position") == "top" {
		_, _ = s.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": c.ID}, bson.M{"$set": bson.M{"sort": top}})
	}
	s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
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
		// The list it leaves and the list it goes to (list_permissions.go).
		if old.ID != "" && (s.refuseInList(ctx, b, old.ListID, u, "moveCard", "list-locked:move-out") ||
			old.ListID != m.ListID && s.refuseInList(ctx, b, m.ListID, u, "moveCard", "list-locked:move-into")) {
			return errForbidden
		}
		res, err := s.cardsUpdateOne(ctx, u.ID, bson.M{"_id": eq(m.CardID), "boardId": b.ID},
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
		if !b.WritableIn(r.Context(), s, u, "moveCard", &c) {
			http.Error(w, "your role on this board cannot move this card", http.StatusForbidden)
			return
		}
	}
	if err := s.MoveCards(r.Context(), b, u, req.Moves); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
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
	s.liveSince(r.Context(), b.ID, r.URL.Query().Get("v"), ch)
	if u != nil {
		s.liveAs(r.Context(), u.ID, ch)
	}
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
			// An event a page caused itself names that page (publishFrom), so
			// it can skip the echo of a change it already shows.
			name, from, _ := strings.Cut(ev, "\x00")
			data := b.ID
			if from != "" {
				data += " " + from
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
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
	// seen is the stamp of each open board as the board watch, or Weddy's
	// own last announcement, left it; stamp reads one (board_watch.go).
	seen  map[string]string
	stamp func(board string) string
	// owner is the signed-in user of each page's channel, and seenUsers each
	// such user's profile stamp at the watch's last look (board_watch.go).
	owner     map[chan string]string
	seenUsers map[string]string
}

func newHub() *hub {
	return &hub{subs: map[string]map[chan string]bool{}, seen: map[string]string{}, owner: map[chan string]string{}, seenUsers: map[string]string{}}
}

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
	delete(h.owner, ch)
	if h.subs[board][ch] {
		delete(h.subs[board], ch)
		close(ch)
	}
}

// publishFrom is publish, naming the page (its X-Weddy-Page header) whose
// own change it is: that page already shows it and skips the refresh.
func (h *hub) publishFrom(board, ev, page string) {
	if !validID(page) {
		h.publish(board, ev)
		return
	}
	h.publish(board, ev+"\x00"+page)
}

func (h *hub) publish(board, ev string) {
	// The board as Weddy's own change left it, for the board watch: read
	// before the lock, and only for a board somebody has open.
	h.mu.Lock()
	open := len(h.subs[board]) > 0
	h.mu.Unlock()
	if open && h.stamp != nil {
		if st := h.stamp(board); st != "" {
			h.mu.Lock()
			h.seen[board] = st
			h.mu.Unlock()
		}
	}
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
	editable := b.Writable(u, "write", c) && (m == nil || !m.IsWorker) && !s.listRefuses(ctx, b, c.ListID, u.ID, "write")
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
	if s.refuseInList(r.Context(), b, c.ListID, u, "write", "page:card-people") {
		http.Error(w, "this list does not let your role change its cards", http.StatusForbidden)
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
	if _, err := s.cardsUpdateOne(r.Context(), u.ID, bson.M{"_id": c.ID},
		bson.M{"$set": bson.M{person.Field: value, "modifiedAt": time.Now().UTC()}}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
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
	s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	http.Redirect(w, r, "/b/"+b.ID+"/"+b.Slug, http.StatusSeeOther)
}

// archiveCardPage is cardDetailsActionsPopup's Archive: the card archived by
// whoever may change it, and closed.
func (s *Server) archiveCardPage(w http.ResponseWriter, r *http.Request, u *User) {
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
	if !b.WritableIn(r.Context(), s, u, "write", &c) {
		http.Error(w, "your role on this board cannot change it", http.StatusForbidden)
		return
	}
	if !c.Archived {
		now := time.Now().UTC()
		if _, err := s.cardsUpdateOne(r.Context(), u.ID, bson.M{"_id": c.ID}, bson.M{"$set": bson.M{"archived": true, "archivedAt": now, "modifiedAt": now}}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.activity(r.Context(), u.ID, "archivedCard", bson.M{"boardId": b.ID, "cardId": c.ID, "listId": c.ListID, "swimlaneId": c.SwimlaneID, "cardTitle": c.Title})
		s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	}
	http.Redirect(w, r, "/b/"+b.ID+"/"+b.Slug, http.StatusSeeOther)
}

// renameBoardPage is WeKan's boardChangeTitlePopup: a board admin sets the
// board's title and description.
func (s *Server) renameBoardPage(w http.ResponseWriter, r *http.Request, u *User) {
	b, err := s.boardFor(r.Context(), r.PathValue("board"), u)
	if err != nil || !b.AdminOf(u.ID) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	title, desc := strings.TrimSpace(r.FormValue("title")), strings.TrimSpace(r.FormValue("description"))
	if title == "" || len(title) > 1000 || len(desc) > 100000 {
		http.Error(w, "a board needs a title", http.StatusBadRequest)
		return
	}
	slug := slugify(title)
	if _, err := s.db.Collection("boards").UpdateOne(r.Context(), bson.M{"_id": b.ID}, bson.M{"$set": bson.M{
		"title": title, "description": desc, "slug": slug, "modifiedAt": time.Now().UTC()}}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	http.Redirect(w, r, "/b/"+b.ID+"/"+slug, http.StatusSeeOther)
}

// archivedItem is one thing in the board's Archive.
type archivedItem struct {
	Kind, ID, Title string
	// Blocked: a card whose list is gone, or full at a hard WIP limit, is
	// restored into a list chosen from these (wip_limits.go).
	Blocked *restoreBlocked
}

// archivedItems are the board's archived cards, lists and swimlanes.
func (s *Server) archivedItems(r *http.Request, b *Board, lang string) []archivedItem {
	var out []archivedItem
	var targets []wipList
	targetsOnce := false
	lazyTargets := func() []wipList {
		if !targetsOnce {
			targets, targetsOnce = s.listsWithRoom(r.Context(), b.ID), true
		}
		return targets
	}
	for _, kind := range []string{"cards", "lists", "swimlanes"} {
		cur, err := s.db.Collection(kind).Find(r.Context(), bson.M{"boardId": b.ID, "archived": true},
			options.Find().SetProjection(bson.M{"title": 1, "listId": 1}).SetLimit(200))
		if err != nil {
			continue
		}
		var docs []bson.M
		_ = cur.All(r.Context(), &docs)
		for _, d := range docs {
			it := archivedItem{Kind: kind, ID: docString(d, "_id"), Title: docString(d, "title")}
			if kind == "cards" {
				it.Blocked = s.restoreBlockFor(r.Context(), b.ID, docString(d, "listId"), lang, lazyTargets)
			}
			out = append(out, it)
		}
	}
	return out
}

// restoreArchivedPage brings an archived card, list or swimlane back, for
// whoever may change the board.
func (s *Server) restoreArchivedPage(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.editableBoard(w, r, u)
	if b == nil {
		return
	}
	kind, id := r.FormValue("kind"), r.FormValue("id")
	if !slices.Contains([]string{"cards", "lists", "swimlanes"}, kind) || !validID(id) {
		http.Error(w, "nothing to restore", http.StatusBadRequest)
		return
	}
	set := bson.M{"archived": false, "modifiedAt": time.Now().UTC()}
	if kind == "cards" {
		// #6772: a card goes back to its own list only when it has room;
		// else to the open list with room it was given.
		var c struct {
			ListID     string `bson:"listId"`
			SwimlaneID string `bson:"swimlaneId"`
		}
		if s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": eq(id), "boardId": b.ID, "archived": true}).Decode(&c) != nil {
			http.Redirect(w, r, "/b/"+b.ID+"/"+b.Slug, http.StatusSeeOther)
			return
		}
		listID := c.ListID
		if to := r.FormValue("toList"); to != "" {
			listID = to
		}
		l, room := s.listHasRoom(r.Context(), b.ID, listID)
		if !room && r.FormValue("toList") == "" {
			// Its own list filled up since the page was drawn: the board
			// again, opening the card's "restore it to another list".
			http.Redirect(w, r, "/b/"+b.ID+"/"+b.Slug+"?restoreBlocked="+url.QueryEscape(id), http.StatusSeeOther)
			return
		}
		if !room {
			http.Error(w, "that list has reached its WIP limit", http.StatusConflict)
			return
		}
		if listID != c.ListID {
			set["listId"] = l.ID
			if l.Swimlane != "" {
				set["swimlaneId"] = l.Swimlane
			} else if s.db.Collection("swimlanes").FindOne(r.Context(), bson.M{"_id": eq(c.SwimlaneID), "boardId": b.ID, "archived": false}).Err() != nil {
				var sw struct {
					ID string `bson:"_id"`
				}
				if s.db.Collection("swimlanes").FindOne(r.Context(), bson.M{"boardId": b.ID, "archived": false},
					options.FindOne().SetSort(bson.D{{Key: "sort", Value: 1}})).Decode(&sw) == nil {
					set["swimlaneId"] = sw.ID
				}
			}
		}
	}
	res, err := s.db.Collection(kind).UpdateOne(r.Context(), bson.M{"_id": eq(id), "boardId": b.ID, "archived": true},
		bson.M{"$set": set})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if res.ModifiedCount > 0 && kind == "cards" {
		s.activity(r.Context(), u.ID, "restoredCard", bson.M{"boardId": b.ID, "cardId": id})
	}
	s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	http.Redirect(w, r, "/b/"+b.ID+"/"+b.Slug, http.StatusSeeOther)
}

// restoreAllArchivedCardsPage is the Archive's Restore all: every archived
// card that has room goes back to its list - counted as it goes, a restored
// card filling the list for the next - and the board says how many were left
// out (#6772).
func (s *Server) restoreAllArchivedCardsPage(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.editableBoard(w, r, u)
	if b == nil {
		return
	}
	ctx := r.Context()
	cur, err := s.db.Collection("cards").Find(ctx, bson.M{"boardId": b.ID, "archived": true},
		options.Find().SetProjection(bson.M{"listId": 1}).SetSort(bson.D{{Key: "sort", Value: 1}}))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var cards []struct {
		ID     string `bson:"_id"`
		ListID string `bson:"listId"`
	}
	_ = cur.All(ctx, &cards)
	skipped := 0
	for _, c := range cards {
		if _, room := s.listHasRoom(ctx, b.ID, c.ListID); !room {
			skipped++
			continue
		}
		res, err := s.cardsUpdateOne(ctx, u.ID, bson.M{"_id": eq(c.ID), "boardId": b.ID, "archived": true},
			bson.M{"$set": bson.M{"archived": false, "modifiedAt": time.Now().UTC()}})
		if err == nil && res.ModifiedCount > 0 {
			s.activity(ctx, u.ID, "restoredCard", bson.M{"boardId": b.ID, "cardId": c.ID})
		}
	}
	s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	to := "/b/" + b.ID + "/" + b.Slug
	if skipped > 0 {
		to += "?restoreSkipped=" + strconv.Itoa(skipped)
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// cardTitlePage saves the card's title, for whoever may change the card.
func (s *Server) cardTitlePage(w http.ResponseWriter, r *http.Request, u *User) {
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
	title := strings.TrimSpace(r.FormValue("title"))
	if !b.WritableIn(r.Context(), s, u, "write", &c) {
		http.Error(w, "your role on this board cannot change it", http.StatusForbidden)
		return
	}
	if title == "" || len(title) > 10000 {
		http.Error(w, "a card needs a title", http.StatusBadRequest)
		return
	}
	if title != c.Title {
		if _, err := s.cardsUpdateOne(r.Context(), u.ID, bson.M{"_id": c.ID}, bson.M{"$set": bson.M{"title": title, "modifiedAt": time.Now().UTC()}}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.cardTextActivities(r.Context(), u, &c, bson.M{"title": title})
		s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	}
	http.Redirect(w, r, "/b/"+b.ID+"/"+b.Slug+"/"+c.ID, http.StatusSeeOther)
}

// cardCompletePage is the minicard's "Mark as complete" (WeKan's
// setDueComplete): for whoever may change the card.
func (s *Server) cardCompletePage(w http.ResponseWriter, r *http.Request, u *User) {
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
	if !b.WritableIn(r.Context(), s, u, "write", &c) {
		http.Error(w, "your role on this board cannot change it", http.StatusForbidden)
		return
	}
	done := r.FormValue("complete") == "true"
	if done != c.DueComplete {
		if _, err := s.cardsUpdateOne(r.Context(), u.ID, bson.M{"_id": c.ID}, bson.M{"$set": bson.M{"dueComplete": done, "modifiedAt": time.Now().UTC()}}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	}
	w.WriteHeader(http.StatusNoContent)
}
