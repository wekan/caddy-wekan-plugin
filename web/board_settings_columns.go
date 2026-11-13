package web

import (
	"encoding/json"
	"net/http"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// WeKan's board settings columns (models/lib/boardSettingsColumns.js,
// server/moveBoardObjects.js setBoardSettingsColumn): a column of Board
// Settings / Swimlane, List or Card - Draggable, Settings, or a Card
// Settings side - set all at once, Select all or Unselect all. Explicit
// columns, never field names from the client.

const scrumVisibilityPath = "scrum.visibility."

// dragSections are DRAG_SETTINGS' sections.
var dragSections = map[string][]string{"swimlane": {"swimlane"}, "list": {"list"}, "card": {"card", "checklist", "item", "subtask"}}

// columnFields is WeKan's columnFields(section, column).
func columnFields(section, column string) []string {
	if column == "draggable" {
		var out []string
		for _, kind := range dragSections[section] {
			out = append(out, boardDragFields[kind])
		}
		return out
	}
	if section == "card" && (column == "card" || column == "minicard") {
		var out []string
		for i := range cardSettings.Rows {
			sp := cardSettings.Rows[i].side(column)
			if sp == nil || sp.Kind == "personal" || sp.NeedsCard {
				continue
			}
			if sp.Kind == "scrum" {
				out = append(out, scrumVisibilityPath+sp.Scrum)
			} else {
				out = append(out, sp.Field)
			}
		}
		return out
	}
	if column == "settings" {
		switch section {
		case "swimlane":
			return []string{"swimlaneHeightResizeLocked"}
		case "list":
			return []string{"listWidthResizeLocked", "sameWidthForAllLists"}
		}
	}
	return nil
}

// boardSettingsFlags are the Settings columns' fields: a board admin's,
// through /boards/update as WeKan's Boards.update writes them.
var boardSettingsFlags = map[string]bool{"swimlaneHeightResizeLocked": true, "listWidthResizeLocked": true, "sameWidthForAllLists": true}

// methodSetBoardSettingsColumn is WeKan's setBoardSettingsColumn(boardId,
// section, column, enabled): an unknown column is invalid-setting, a board
// the caller is no admin of not-authorized; a Card column's Scrum rows go
// through scrum.configure, which keeps its lock, revision and history.
func (s *Server) methodSetBoardSettingsColumn(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	boardID, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	section, err := stringArg(args, 1)
	if err != nil {
		return nil, err
	}
	column, err := stringArg(args, 2)
	if err != nil {
		return nil, err
	}
	var enabled bool
	if len(args) < 4 || json.Unmarshal(args[3], &enabled) != nil {
		return nil, &methodError{"Match.Error", "Expected boolean"}
	}
	set, visibility := bson.M{}, map[string]any{}
	for _, f := range columnFields(section, column) {
		if strings.HasPrefix(f, scrumVisibilityPath) {
			visibility[strings.TrimPrefix(f, scrumVisibilityPath)] = enabled
		} else {
			set[f] = enabled
		}
	}
	if len(set) == 0 {
		return nil, &methodError{"invalid-setting", "invalid-setting"}
	}
	var b Board
	if !validID(boardID) || s.db.Collection("boards").FindOne(r.Context(), bson.M{"_id": eq(boardID)}).Decode(&b) != nil || (!u.IsAdmin && !b.AdminOf(u.ID)) {
		return nil, &methodError{"not-authorized", "not-authorized"}
	}
	res, err := s.db.Collection("boards").UpdateOne(r.Context(), bson.M{"_id": b.ID}, bson.M{"$set": set})
	if err != nil {
		return nil, err
	}
	if len(visibility) > 0 {
		vis, _ := json.Marshal(map[string]any{"visibility": visibility})
		id, _ := json.Marshal(b.ID)
		if _, err := s.methodScrumConfigure(r, u, []json.RawMessage{id, vis, json.RawMessage("null")}); err != nil {
			return nil, err
		}
	}
	s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	return res.ModifiedCount, nil
}

// dragRowsOf are one section's Draggable rows (boardDragSettings).
func dragRowsOf(board bson.M, section, lang string) []boardDragRow {
	labels := map[string]string{"swimlane": msg("swimlane"), "list": msg("list"), "card": msg("card"), "checklist": msg("checklists"),
		"item": msg("r-item"), "subtask": msg("subtasks")}
	var out []boardDragRow
	for _, kind := range dragSections[section] {
		out = append(out, boardDragRow{kind, tr(lang, labels[kind]), board[boardDragFields[kind]] != false})
	}
	return out
}
