package web

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// methodCreateListAfter is WeKan's createListAfter({title, boardId,
// swimlaneId, afterListId, nextListId, type}) (server/models/lists.js), the
// inline Add List composer's (#6465): a writer adds a list right after the
// one whose header opened the composer - between it and the next list of the
// same swimlane, or after it when it is the last - or at the end. A list
// shown in every swimlane (no swimlaneId) is in the swimlane it was opened in;
// the new list is stored in that swimlane, as WeKan's.
func (s *Server) methodCreateListAfter(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	match := &methodError{"Match.Error", "Match failed"}
	var raw map[string]any
	if len(args) < 1 || json.Unmarshal(args[0], &raw) != nil {
		return nil, match
	}
	str := func(k string, required bool) (string, bool) {
		v, has := raw[k]
		if !has || v == nil {
			return "", !required
		}
		s, ok := v.(string)
		return s, ok
	}
	for k := range raw {
		if !map[string]bool{"title": true, "boardId": true, "swimlaneId": true, "afterListId": true, "nextListId": true, "type": true}[k] {
			return nil, match
		}
	}
	title, ok1 := str("title", true)
	boardID, ok2 := str("boardId", true)
	swimlaneID, ok3 := str("swimlaneId", false)
	afterID, ok4 := str("afterListId", false)
	nextID, ok5 := str("nextListId", false)
	typ, ok6 := str("type", false)
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 {
		return nil, match
	}
	if typ == "" {
		typ = "list"
	}
	ctx := r.Context()
	var b Board
	if !validID(boardID) || s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(boardID)}).Decode(&b) != nil {
		return nil, &methodError{"board-not-found", "Board not found"}
	}
	if !b.Writable(u, "write", nil) {
		return nil, &methodError{"not-authorized", "Access denied"}
	}
	title = strings.TrimSpace(title)
	if title == "" || len(title) > 10000 || (typ != "list" && typ != "template-list") {
		return nil, match
	}
	// The swimlane: the one given, if it is this board's, else the default -
	// the first active one (WeKan's pickDefaultSwimlane).
	var swimlanes []Swimlane
	if cur, err := s.db.Collection("swimlanes").Find(ctx, bson.M{"boardId": b.ID, "archived": false, "deletedAt": nil},
		options.Find().SetSort(bson.D{{Key: "sort", Value: 1}})); err == nil {
		_ = cur.All(ctx, &swimlanes)
	}
	target := ""
	for _, sw := range swimlanes {
		if sw.ID == swimlaneID {
			target = sw.ID
		}
	}
	if target == "" && len(swimlanes) > 0 {
		target = swimlanes[0].ID
	}
	resolved := func(l List) string {
		if l.SwimlaneID != "" {
			return l.SwimlaneID
		}
		return target
	}
	var lists []List
	if cur, err := s.db.Collection("lists").Find(ctx, bson.M{"boardId": b.ID, "archived": false, "deletedAt": nil}); err == nil {
		_ = cur.All(ctx, &lists)
	}
	sortAt := 0.0
	if afterID != "" {
		var selected *List
		for i := range lists {
			if lists[i].ID == afterID {
				selected = &lists[i]
			}
		}
		if selected == nil {
			return nil, &methodError{"list-not-found", "Selected list not found"}
		}
		target = resolved(*selected)
		var lane []List
		for _, l := range lists {
			if resolved(l) == target {
				lane = append(lane, l)
			}
		}
		sort.SliceStable(lane, func(i, j int) bool { return lane[i].Sort < lane[j].Sort })
		var next *List
		for i := range lane {
			if lane[i].ID == nextID && nextID != "" {
				next = &lane[i]
			}
		}
		if next == nil {
			for i := range lane {
				if lane[i].ID == selected.ID && i+1 < len(lane) {
					next = &lane[i+1]
				}
			}
		}
		if next != nil && next.Sort > selected.Sort {
			sortAt = selected.Sort + (next.Sort-selected.Sort)/2
		} else {
			sortAt = selected.Sort + 1
		}
	} else {
		last := -1.0
		for _, l := range lists {
			if resolved(l) == target && l.Sort > last {
				last = l.Sort
			}
		}
		sortAt = last + 1
	}
	l := &List{ID: randomID(), Title: title, BoardID: b.ID, Type: typ, Sort: sortAt, SwimlaneID: target}
	if _, err := s.db.Collection("lists").InsertOne(ctx, l); err != nil {
		return nil, err
	}
	s.activity(ctx, u.ID, "createList", bson.M{"boardId": b.ID, "listId": l.ID, "title": l.Title})
	s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	return l.ID, nil
}
