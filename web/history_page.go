package web

// changeHistory.page, the one read behind every History view (WeKan's
// server/models/changeHistory.js, models/lib/changeHistoryQuery.js,
// server/lib/historyReadScope.js, server/lib/historyPageScan.js): a board,
// swimlane, list or card - each with what is inside it - or a member's own
// changes, newest first, searched in the change's type, group, kind and
// content, a page at a time with each contributor's count.
//
// Only rows the caller may read: of a board they see, and of an assigned-only
// member's own cards only (AssignedBleed) - and an admin-only custom field's
// value hidden from whoever may not read it (AdminFieldBleed). A scope named
// on a board the caller cannot see says nothing, not even how many rows.

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

var historyScopeColumn = map[string]string{"board": "boardId", "swimlane": "swimlaneId", "list": "listId", "card": "cardId"}

type historyPageRequest struct {
	Scope, ScopeID, Group, UserID, EntityType, EntityID, Search string
	Page, PageSize                                              float64
}

// historyContentText is contentText: the text a search looks in.
func historyContentText(v any, depth int, out []string) []string {
	if v == nil || depth > 4 {
		return out
	}
	switch x := v.(type) {
	case string:
		return append(out, x)
	case float64, int32, int64, int, bool:
		return append(out, jsonNumberOrBool(x))
	case time.Time:
		return append(out, x.UTC().Format("2006-01-02T15:04:05.000Z"))
	case []any:
		for _, e := range x {
			out = historyContentText(e, depth+1, out)
		}
	case map[string]any:
		for _, e := range x {
			out = historyContentText(e, depth+1, out)
		}
	}
	return out
}

func jsonNumberOrBool(v any) string {
	if b, ok := v.(bool); ok {
		if b {
			return "true"
		}
		return "false"
	}
	return jsNumber(historyNumber(v))
}

// historyMatches is matchesSearch.
func historyMatches(row map[string]any, term string) bool {
	needle := strings.ToLower(strings.TrimSpace(term))
	if needle == "" {
		return true
	}
	parts := []string{docString(row, "changeType"), docString(row, "group"), docString(row, "entityType")}
	parts = historyContentText(row["newContent"], 0, parts)
	parts = historyContentText(row["previousContent"], 0, parts)
	return strings.Contains(strings.ToLower(strings.Join(parts, "\n")), needle)
}

// historyReader is what one caller may read, board by board.
type historyReader struct {
	s      *Server
	ctx    context.Context
	u      *User
	boards map[string]*historyBoardScope
}

type historyBoardScope struct {
	visible, assignedOnly bool
	board                 *Board
	cards                 map[string]bool
	fields                *fieldReader
}

func (hr *historyReader) scope(boardID string) *historyBoardScope {
	if sc, ok := hr.boards[boardID]; ok {
		return sc
	}
	sc := &historyBoardScope{}
	if b, err := hr.s.boardFor(hr.ctx, boardID, hr.u); err == nil {
		sc.visible, sc.board = true, b
		if m := b.member(hr.u.ID); m != nil && m.AssignedOnly() && !hr.u.IsAdmin {
			sc.assignedOnly, sc.cards = true, map[string]bool{}
			cur, err := hr.s.db.Collection("cards").Find(hr.ctx, bson.M{"boardId": eq(b.ID), "assignees": eq(hr.u.ID)}, options.Find().SetProjection(bson.M{"_id": 1}))
			if err == nil {
				var cs []struct {
					ID string `bson:"_id"`
				}
				_ = cur.All(hr.ctx, &cs)
				for _, c := range cs {
					sc.cards[c.ID] = true
				}
			}
		}
		sc.fields = hr.s.fieldReaderFor(hr.ctx, b, hr.u)
	}
	hr.boards[boardID] = sc
	return sc
}

// readable is readableHistoryRow, and the row's admin-only values hidden.
func (hr *historyReader) readable(row map[string]any) bool {
	sc := hr.scope(docString(row, "boardId"))
	if !sc.visible {
		return false
	}
	if sc.assignedOnly {
		card := docString(row, "cardId")
		if card == "" && docString(row, "entityType") == "card" {
			card = docString(row, "entityId")
		}
		if card == "" || !sc.cards[card] {
			return false
		}
	}
	for _, side := range []string{"previousContent", "newContent"} {
		c, ok := row[side].(map[string]any)
		if !ok || c["field"] != "customFields" {
			continue
		}
		vals, _ := c["value"].([]any)
		for i, v := range vals {
			if f, ok := v.(map[string]any); ok && !sc.fields.mayRead(docString(f, "_id")) {
				vals[i] = map[string]any{"_id": f["_id"], "value": nil}
			}
		}
	}
	return true
}

func (s *Server) historyPage(r *http.Request, u *User, q historyPageRequest) (map[string]any, error) {
	ctx := r.Context()
	sel := bson.M{}
	if q.Scope != "" {
		col, ok := historyScopeColumn[q.Scope]
		if !ok || q.ScopeID == "" {
			return nil, &methodError{"bad-scope", "changeHistory: unknown scope or no scopeId"}
		}
		sel[col] = eq(q.ScopeID)
		boardID := q.ScopeID
		if q.Scope != "board" {
			var owner bson.M
			if s.db.Collection(map[string]string{"swimlane": "swimlanes", "list": "lists", "card": "cards"}[q.Scope]).FindOne(ctx, bson.M{"_id": eq(q.ScopeID)}).Decode(&owner) != nil {
				return nil, &methodError{"not-found", "No such scope."}
			}
			boardID = docString(owner, "boardId")
		}
		if _, err := s.boardFor(ctx, boardID, u); err != nil {
			return nil, &methodError{"not-authorized", "not-authorized"}
		}
	} else if q.ScopeID != "" {
		return nil, &methodError{"bad-scope", "changeHistory: scopeId without a scope"}
	} else if q.UserID == "" {
		return nil, &methodError{"bad-scope", "Ask for a scope or a user."}
	}
	for k, v := range map[string]string{"entityType": q.EntityType, "entityId": q.EntityID, "group": q.Group, "userId": q.UserID} {
		if v != "" {
			sel[k] = eq(v)
		}
	}
	size := int64(25)
	if q.PageSize >= 1 {
		size = int64(math.Min(200, math.Floor(q.PageSize)))
	}
	page := int64(1)
	if q.Page >= 1 && !math.IsInf(q.Page, 0) {
		page = int64(q.Page)
	}
	cur, err := s.db.Collection("changeHistory").Find(ctx, sel, options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}}).SetBatchSize(100))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	hr := &historyReader{s: s, ctx: ctx, u: u, boards: map[string]*historyBoardScope{}}
	var total int64
	selected, last := []map[string]any{}, []map[string]any{}
	counts := map[string]int64{}
	for cur.Next(ctx) {
		var raw bson.M
		if cur.Decode(&raw) != nil {
			continue
		}
		row := plain(raw).(map[string]any)
		if row["boardId"] == historyFenceBoard || !hr.readable(row) || !historyMatches(row, q.Search) {
			continue
		}
		if total%size == 0 {
			last = last[:0]
		}
		last = append(last, row)
		if total >= (page-1)*size && total < page*size {
			selected = append(selected, row)
		}
		total++
		counts[docString(row, "userId")]++
	}
	pages := max(1, (total+size-1)/size)
	rows := selected
	if page > pages {
		page, rows = pages, last
	}
	contributors := []map[string]any{}
	for id, n := range counts {
		contributors = append(contributors, map[string]any{"userId": id, "count": n})
	}
	slices.SortFunc(contributors, func(a, b map[string]any) int { return int(b["count"].(int64) - a["count"].(int64)) })
	return map[string]any{"rows": rows, "total": total, "page": page, "pageSize": size, "contributors": contributors}, nil
}

func (s *Server) methodHistoryPage(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	var q historyPageRequest
	if len(args) < 1 || json.Unmarshal(args[0], &q) != nil {
		return nil, &methodError{"Match.Error", "Match failed"}
	}
	return s.historyPage(r, u, q)
}
