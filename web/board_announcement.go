package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// WeKan's board announcement (#1566, models/lib/boardAnnouncement.js): a board
// admin writes one, every member sees it on the board - plain text - until
// they dismiss it, and sees it again when it is edited: the version is when it
// was last saved.

const maxBoardAnnouncement = 2000

// boardAnnouncement is the board's announcement as stored.
type boardAnnouncement struct {
	Enabled   bool      `bson:"enabled"`
	Body      string    `bson:"body"`
	UpdatedAt time.Time `bson:"updatedAt"`
}

// announcementOf reads it from the board document as decoded.
func announcementOf(rawB bson.M) *boardAnnouncement {
	v, ok := rawB["announcement"]
	if !ok || v == nil {
		return nil
	}
	raw, err := bson.Marshal(bson.M{"a": v})
	if err != nil {
		return nil
	}
	var out struct {
		A *boardAnnouncement `bson:"a"`
	}
	if bson.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out.A
}

// boardAnnouncementVersion is when the announcement was saved, or "".
func boardAnnouncementVersion(rawB bson.M) string {
	a := announcementOf(rawB)
	if a == nil || a.UpdatedAt.IsZero() {
		return ""
	}
	return jsISO(a.UpdatedAt)
}

// boardAnnouncementShown is the text to show this member, or "": enabled, not
// empty, and not dismissed at this version.
func boardAnnouncementShown(rawB bson.M, boardID string, dismissed map[string]string) string {
	a := announcementOf(rawB)
	if a == nil || !a.Enabled || strings.TrimSpace(a.Body) == "" {
		return ""
	}
	if v := boardAnnouncementVersion(rawB); v != "" && dismissed[boardID] == v {
		return ""
	}
	return a.Body
}

// cleanBoardAnnouncement is WeKan's: the trimmed text, enabled only with text,
// and when it was saved; refused past 2000 characters.
func cleanBoardAnnouncement(enabled bool, body string, now time.Time) (bson.M, bool) {
	text := strings.TrimSpace(body)
	if utf8.RuneCountInString(text) > maxBoardAnnouncement {
		return nil, false
	}
	return bson.M{"enabled": enabled && text != "", "body": text, "updatedAt": now.UTC()}, true
}

func (s *Server) boardAnnouncementRoutes(m *http.ServeMux) {
	m.HandleFunc("POST /b/{board}/announcement", s.page(s.saveBoardAnnouncement))
}

// saveBoardAnnouncement is the board menu's Board announcement, for a board
// admin, as WeKan's board.setAnnouncement under its allow rule.
func (s *Server) saveBoardAnnouncement(w http.ResponseWriter, r *http.Request, u *User) {
	b, err := s.boardFor(r.Context(), r.PathValue("board"), u)
	if err != nil || !b.AdminOf(u.ID) {
		http.Error(w, "only a board admin may change the board's announcement", http.StatusForbidden)
		return
	}
	a, ok := cleanBoardAnnouncement(r.FormValue("enabled") == "true", r.FormValue("body"), time.Now())
	if !ok {
		http.Error(w, "the announcement is longer than 2000 characters", http.StatusBadRequest)
		return
	}
	if _, err := s.db.Collection("boards").UpdateOne(r.Context(), bson.M{"_id": eq(b.ID)}, bson.M{"$set": bson.M{"announcement": a}}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	http.Redirect(w, r, "/b/"+b.ID+"/"+b.Slug, http.StatusSeeOther)
}

// methodDismissBoardAnnouncement is WeKan's dismissBoardAnnouncement: the
// version the member has seen, on a board they can see.
func (s *Server) methodDismissBoardAnnouncement(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	boardID, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	if s.refuseMapKeys(r, u, "dismissBoardAnnouncement", boardID) {
		return nil, &methodError{"invalid-map-key", "invalid-map-key"}
	}
	b, err := s.boardFor(r.Context(), boardID, u)
	if err != nil {
		return nil, &methodError{"not-found", "Board not found"}
	}
	var rawB bson.M
	if s.db.Collection("boards").FindOne(r.Context(), bson.M{"_id": eq(b.ID)}).Decode(&rawB) != nil {
		return nil, &methodError{"not-found", "Board not found"}
	}
	v := boardAnnouncementVersion(rawB)
	if v == "" {
		return nil, nil
	}
	// Through the profile writer every per-board setting uses: its keys
	// refused when unsafe, and ids only (PrototypeBleed).
	if err := s.setProfileMap(r, u, "dismissBoardAnnouncement", "dismissedBoardAnnouncements", v, b.ID); err != nil {
		return nil, err
	}
	return v, nil
}
