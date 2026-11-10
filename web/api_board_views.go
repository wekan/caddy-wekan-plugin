package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// A board's views - which the Board View menu shows on a public and a private
// board, each side's default, and their order - as Node.js WeKan
// (models/lib/boardViewSettings.js, server/models/boards.js boardViewSettings).
func (s *Server) boardViewRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/boards/{board}/boardViewSettings", s.api(s.apiBoardViewSettings))
	m.HandleFunc("PUT /api/boards/{board}/boardViewSettings", s.api(s.apiSetBoardViewSettings))
}

// boardViewKeys is WeKan's BOARD_VIEWS, in its order.
var boardViewKeys = []string{"board-view-swimlanes", "board-view-lists", "board-view-table", "board-view-cal", "board-view-calendar-mode", "board-view-multiboard-cal", "board-view-time", "board-view-timeline", "board-view-stats", "board-view-group-by-assignee", "board-view-gantt", "board-view-gantt-frappe", "board-view-gantt-dhtmlx", "board-view-product-backlog", "board-view-sprints", "board-view-sprint-report", "board-view-velocity", "board-view-roadmap", "board-view-dashboard", "board-view-bigboard", "board-view-burndown", "board-view-burnup", "board-view-cumulative-flow", "board-view-control-chart", "board-view-cycle-time", "board-view-flow-efficiency", "board-view-lead-time", "board-view-throughput-histogram", "board-view-wip-run", "board-view-pulse", "board-view-aging-wip", "board-view-blocker-analysis", "board-view-monte-carlo", "board-view-process-behavior", "board-view-size-cycle-time", "board-view-map", "board-view-email", "board-view-chat", "board-view-version-control", "board-view-column-sets"}

// defaultBoardViewOrder is WeKan's DEFAULT_BOARD_VIEW_ORDER.
var defaultBoardViewOrder = []string{"board-view-swimlanes", "board-view-lists", "board-view-table", "board-view-cal", "board-view-calendar-mode", "board-view-multiboard-cal", "board-view-time", "board-view-timeline", "board-view-stats", "board-view-group-by-assignee", "board-view-gantt", "board-view-gantt-frappe", "board-view-gantt-dhtmlx", "board-view-product-backlog", "board-view-sprints", "board-view-sprint-report", "board-view-velocity", "board-view-roadmap", "board-view-dashboard", "board-view-bigboard", "board-view-burndown", "board-view-burnup", "board-view-cumulative-flow", "board-view-control-chart", "board-view-cycle-time", "board-view-flow-efficiency", "board-view-lead-time", "board-view-throughput-histogram", "board-view-wip-run", "board-view-pulse", "board-view-aging-wip", "board-view-blocker-analysis", "board-view-monte-carlo", "board-view-process-behavior", "board-view-size-cycle-time", "board-view-map", "board-view-email", "board-view-chat", "board-view-version-control", "board-view-column-sets"}

var viewsAddedLater = []string{"board-view-calendar-mode", "board-view-email", "board-view-chat", "board-view-version-control", "board-view-column-sets"}

const defaultBoardView = "board-view-swimlanes"

func isKnownBoardView(v string) bool { return slices.Contains(boardViewKeys, v) }

// normalizeBoardViewOrder is WeKan's: unknown and repeated views dropped; an
// order that is the default order of its time is the default; a missing view
// appended in its default place.
func normalizeBoardViewOrder(stored []string) []string {
	seen := map[string]bool{}
	var result []string
	for _, v := range stored {
		if isKnownBoardView(v) && !seen[v] {
			seen[v] = true
			result = append(result, v)
		}
	}
	var older []string
	for _, v := range defaultBoardViewOrder {
		if seen[v] || !slices.Contains(viewsAddedLater, v) {
			older = append(older, v)
		}
	}
	if len(result) > 0 && slices.Equal(older, result) {
		result = nil
	}
	for _, v := range append(append([]string{}, defaultBoardViewOrder...), boardViewKeys...) {
		if !slices.Contains(result, v) {
			result = append(result, v)
		}
	}
	return result
}

func showField(side string) string {
	if side == "public" {
		return "showOnPublic"
	}
	return "showOnPrivate"
}

func defaultField(side string) string {
	if side == "public" {
		return "defaultPublicBoardView"
	}
	return "defaultPrivateBoardView"
}

// boardViewState is the part of a board these settings are.
type boardViewState struct {
	Settings map[string]map[string]any
	Defaults map[string]string // side -> view
	Order    []string
}

func boardViewStateOf(doc bson.M) *boardViewState {
	st := &boardViewState{Settings: map[string]map[string]any{}, Defaults: map[string]string{}}
	if d, ok := doc["boardViewSettings"].(bson.D); ok {
		for _, e := range d {
			if entry, ok := e.Value.(bson.D); ok {
				st.Settings[e.Key] = docMap(entry)
			}
		}
	}
	for _, side := range []string{"public", "private"} {
		st.Defaults[side], _ = doc[defaultField(side)].(string)
	}
	st.Order = stringsOf(doc["boardViewOrder"])
	return st
}

func (st *boardViewState) shown(view, side string) bool {
	entry, ok := st.Settings[view]
	if !ok {
		return true
	}
	return entry[showField(side)] != false
}

func (st *boardViewState) defaultView(side string) string {
	if v := st.Defaults[side]; isKnownBoardView(v) {
		return v
	}
	return defaultBoardView
}

// snapshot is WeKan's boardViewSettingsSnapshot: every view, both sides explicit.
func (st *boardViewState) snapshot() map[string]any {
	settings := map[string]any{}
	for _, v := range boardViewKeys {
		settings[v] = map[string]bool{"showOnPublic": st.shown(v, "public"), "showOnPrivate": st.shown(v, "private")}
	}
	return map[string]any{"boardViewSettings": settings, "defaultPublicBoardView": st.defaultView("public"),
		"defaultPrivateBoardView": st.defaultView("private"), "boardViewOrder": normalizeBoardViewOrder(st.Order), "keys": boardViewKeys}
}

func (st *boardViewState) setShown(view, side string, on bool) {
	if st.Settings[view] == nil {
		st.Settings[view] = map[string]any{}
	}
	st.Settings[view][showField(side)] = on
}

// boardViewRequest is WeKan's boardViewSettingsRequest: the body as ONE $set,
// defaults before show flags, an unknown view or hiding a side's default refused.
func boardViewRequest(st *boardViewState, input map[string]any) (bson.M, string) {
	set := bson.M{}
	for _, side := range []string{"public", "private"} {
		field := defaultField(side)
		v, ok := input[field]
		if !ok {
			continue
		}
		view, isString := v.(string)
		if !isString || !isKnownBoardView(view) {
			return nil, fmt.Sprintf("%s: unknown board view \"%v\"", field, v)
		}
		set[field] = view
		set["boardViewSettings."+view+"."+showField(side)] = true
		st.Defaults[side] = view
		st.setShown(view, side, true)
	}
	if v, ok := input["boardViewSettings"]; ok {
		table, isMap := v.(map[string]any)
		if !isMap {
			return nil, `boardViewSettings must be an object of { "<view>": { showOnPublic, showOnPrivate } }`
		}
		views := make([]string, 0, len(table))
		for view := range table {
			views = append(views, view)
		}
		slices.Sort(views)
		for _, view := range views {
			if !isKnownBoardView(view) {
				return nil, fmt.Sprintf("boardViewSettings: unknown board view \"%s\"", view)
			}
			entry, isMap := table[view].(map[string]any)
			if !isMap {
				return nil, fmt.Sprintf("boardViewSettings.%s must be an object with showOnPublic and/or showOnPrivate", view)
			}
			for _, side := range []string{"public", "private"} {
				flag := showField(side)
				raw, ok := entry[flag]
				if !ok {
					continue
				}
				on := raw == true || strings.EqualFold(fmt.Sprint(raw), "true")
				if !on && st.defaultView(side) == view {
					return nil, fmt.Sprintf("boardViewSettings.%s.%s: cannot hide the %s default view; set %s to another view first", view, flag, side, defaultField(side))
				}
				set["boardViewSettings."+view+"."+flag] = on
				st.setShown(view, side, on)
			}
		}
	}
	if v, ok := input["boardViewOrder"]; ok {
		arr, isArray := v.([]any)
		if !isArray {
			return nil, "boardViewOrder must be an array of board view keys"
		}
		var order []string
		for _, e := range arr {
			view, isString := e.(string)
			if !isString || !isKnownBoardView(view) {
				return nil, fmt.Sprintf("boardViewOrder: unknown board view \"%v\"", e)
			}
			order = append(order, view)
		}
		st.Order = normalizeBoardViewOrder(order)
		set["boardViewOrder"] = st.Order
	}
	return set, ""
}

func (s *Server) boardDoc(r *http.Request, b *Board) bson.M {
	var doc bson.M
	_ = s.db.Collection("boards").FindOne(r.Context(), bson.M{"_id": b.ID}).Decode(&doc)
	return doc
}

func (s *Server) apiBoardViewSettings(w http.ResponseWriter, r *http.Request, u *User) {
	if b := s.apiBoardFor(w, r, u, false); b != nil {
		writeJSON(w, http.StatusOK, boardViewStateOf(s.boardDoc(r, b)).snapshot())
	}
}

// apiSetBoardViewSettings: a board admin sets any of them - the structured
// fields also as JSON text, as a form sends them.
func (s *Server) apiSetBoardViewSettings(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardAdmin(w, r, u)
	if b == nil {
		return
	}
	var input map[string]any
	if err := decodeJSON(r, &input); err != nil || input == nil {
		apiError(w, http.StatusBadRequest, "body must be a JSON object")
		return
	}
	for _, k := range []string{"boardViewSettings", "boardViewOrder"} {
		if str, ok := input[k].(string); ok {
			var v any
			if json.Unmarshal([]byte(str), &v) == nil {
				input[k] = v
			} else if k == "boardViewOrder" {
				list := []any{}
				for _, x := range strings.Split(str, ",") {
					list = append(list, strings.TrimSpace(x))
				}
				input[k] = list
			}
		}
	}
	st := boardViewStateOf(s.boardDoc(r, b))
	set, problem := boardViewRequest(st, input)
	if problem != "" {
		apiError(w, http.StatusBadRequest, problem)
		return
	}
	if len(set) == 0 {
		apiError(w, http.StatusBadRequest, "no recognized board view settings in body")
		return
	}
	if _, err := s.db.Collection("boards").UpdateOne(r.Context(), bson.M{"_id": b.ID}, bson.M{"$set": set}); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	writeJSON(w, http.StatusOK, boardViewStateOf(s.boardDoc(r, b)).snapshot())
}
