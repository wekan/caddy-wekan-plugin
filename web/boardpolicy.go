package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Who may make a board, and how open it may be, as Node.js WeKan's
// server/lib/boardCreationGuard.js and Admin Panel's table visibility
// settings decide it, on every path that makes one: the page, REST, an
// import or a copy, and WeKan's createBoardWithInitialSwimlanes.

// errBoardCreation is WeKan's: "Board creation is restricted to admins."
var errBoardCreation = errors.New("Board creation is restricted to admins.")

// tableSetting is one of Admin Panel's tableVisibilityModeSettings switches.
func (s *Server) tableSetting(ctx context.Context, id string) bool {
	var doc struct {
		On bool `bson:"booleanValue"`
	}
	_ = s.db.Collection("tableVisibilityModeSettings").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&doc)
	return doc.On
}

// mayCreateBoard is WeKan's assertMayCreateBoard (CreateBleed): while Admin
// Panel lets only admins make boards, anybody else may make only a template
// board; a refusal is recorded - never blocking the account.
func (s *Server) mayCreateBoard(ctx context.Context, u *User, typ, source string) error {
	if typ == "" {
		typ = "board"
	}
	if !s.tableSetting(ctx, "tableVisibilityMode-boardCreationAdminOnly") || u.IsAdmin || typ == "template-board" {
		return nil
	}
	s.recordSecurityCtx(ctx, securityEvent{Key: "authz.board-creation", Action: "blocked", Source: source, UserID: u.ID,
		Detail: "Board creation refused: only admins may create boards (type " + typ + ")."})
	return errBoardCreation
}

// boardPermission is the visibility a new board gets: what was asked, but
// private while Admin Panel allows only private boards - and an open one
// asked for then is recorded (VisibilityBleed).
func (s *Server) boardPermission(ctx context.Context, requested, source string) string {
	if requested == "" {
		requested = "private"
	}
	if !s.tableSetting(ctx, "tableVisibilityMode-allowPrivateOnly") {
		return requested
	}
	if requested == "public" || requested == "instance" {
		s.recordSecurityCtx(ctx, securityEvent{Key: "authz.board-visibility", Action: "blocked", Source: source,
			Detail: "Public board creation overridden by private-only policy."})
	}
	return "private"
}

// methodCreateBoardWithInitialSwimlanes is WeKan's createBoardWithInitialSwimlanes:
// a board - its title, slug, visibility and type - with the swimlanes given,
// for whoever may make one.
func (s *Server) methodCreateBoardWithInitialSwimlanes(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	var p struct {
		Title      string `json:"title"`
		Slug       string `json:"slug"`
		Permission string `json:"permission"`
		Type       string `json:"type"`
		Swimlanes  []struct {
			Title string   `json:"title"`
			Sort  *float64 `json:"sort"`
			Type  string   `json:"type"`
		} `json:"swimlanes"`
	}
	if len(args) < 1 || json.Unmarshal(args[0], &p) != nil || strings.TrimSpace(p.Title) == "" || len(p.Title) > 1000 || len(p.Swimlanes) > 100 {
		return nil, &methodError{"Match.Error", "Expected a title and a slug"}
	}
	if p.Type == "" {
		p.Type = "board"
	}
	if p.Type != "board" && p.Type != "template-board" && p.Type != "template-container" {
		return nil, &methodError{"Match.Error", "Unknown board type"}
	}
	ctx := r.Context()
	if err := s.mayCreateBoard(ctx, u, p.Type, "createBoardWithInitialSwimlanes"); err != nil {
		return nil, &methodError{"not-authorized", err.Error()}
	}
	perm := s.boardPermission(ctx, p.Permission, "createBoardWithInitialSwimlanes")
	if perm != "private" && perm != "public" && perm != "instance" {
		perm = "private"
	}
	b, err := s.insertBoard(ctx, u, strings.TrimSpace(p.Title), p.Type, perm)
	if err != nil {
		return nil, err
	}
	for i, sw := range p.Swimlanes {
		sort := float64(i)
		if sw.Sort != nil {
			sort = *sw.Sort
		}
		typ := sw.Type
		if typ == "" {
			typ = "swimlane"
		}
		if _, err := s.db.Collection("swimlanes").InsertOne(ctx, bson.M{"_id": randomID(), "title": sw.Title, "boardId": b.ID, // inserted, not a filter
			"sort": sort, "type": typ, "archived": false, "createdAt": time.Now().UTC()}); err != nil { // inserted, not a filter
			return nil, err
		}
	}
	s.activity(ctx, u.ID, "createBoard", bson.M{"type": "board", "activityTypeId": b.ID, "boardId": b.ID})
	return b.ID, nil
}
