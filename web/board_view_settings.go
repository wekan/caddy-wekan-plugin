package web

import (
	"context"
	_ "embed"
	"encoding/json"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// WeKan's Board Settings / Board View (sidebar.jade boardViewSettingsPopup):
// each board view's row - the default and shown views of a public and of a
// private board, its icon and name - in the board's order, dragged or moved
// by the Up and Down keys. The views are WeKan's BOARD_VIEWS, written to
// board_views.json by cmd/board-views/generate.cjs. A board admin's: written
// through /boards/update, as WeKan's Boards.update, checked by
// boardViewRequest (an unknown view, or hiding a side's default, refused).

//go:embed board_views.json
var boardViewsJSON []byte

var boardViewsTable struct {
	Views []struct {
		View     string `json:"view"`
		LabelKey string `json:"labelKey"`
		Icon     string `json:"icon"`
	} `json:"views"`
}

func init() {
	if err := json.Unmarshal(boardViewsJSON, &boardViewsTable); err != nil {
		panic("board_views.json: " + err.Error())
	}
}

// boardViewRow is one row of Board Settings / Board View.
type boardViewRow struct {
	View, Icon, Label                                      string
	DefaultPublic, ShowPublic, DefaultPrivate, ShowPrivate bool
	Handles                                                bool
}

func (s *Server) boardViewRows(ctx context.Context, b *Board, u *User, lang string) []boardViewRow {
	var doc bson.M
	_ = s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(b.ID)}).Decode(&doc)
	st := boardViewStateOf(doc)
	meta := map[string]int{}
	for i, v := range boardViewsTable.Views {
		meta[v.View] = i
	}
	var rows []boardViewRow
	for _, view := range normalizeBoardViewOrder(st.Order) {
		i, ok := meta[view]
		if !ok {
			continue
		}
		m := boardViewsTable.Views[i]
		rows = append(rows, boardViewRow{View: view, Icon: m.Icon, Label: tr(lang, m.LabelKey),
			DefaultPublic: st.defaultView("public") == view, ShowPublic: st.shown(view, "public"),
			DefaultPrivate: st.defaultView("private") == view, ShowPrivate: st.shown(view, "private"),
			Handles: u != nil && u.Profile.ShowDesktopDragHandles})
	}
	return rows
}

// boardViewWrite turns a /boards/update $set of Board View's fields into
// boardViewRequest's input: ok false when anything else is in it.
func boardViewWrite(set map[string]any) (map[string]any, bool) {
	input := map[string]any{}
	table := map[string]any{}
	for path, v := range set {
		switch {
		case path == "defaultPublicBoardView" || path == "defaultPrivateBoardView" || path == "boardViewOrder":
			input[path] = v
		case strings.HasPrefix(path, "boardViewSettings."):
			parts := strings.Split(path, ".")
			if len(parts) != 3 || (parts[2] != "showOnPublic" && parts[2] != "showOnPrivate") {
				return nil, false
			}
			entry, _ := table[parts[1]].(map[string]any)
			if entry == nil {
				entry = map[string]any{}
				table[parts[1]] = entry
			}
			entry[parts[2]] = v
		default:
			return nil, false
		}
	}
	if len(table) > 0 {
		input["boardViewSettings"] = table
	}
	return input, len(input) > 0
}
