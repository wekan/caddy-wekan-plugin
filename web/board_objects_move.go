package web

// WeKan's moveBoardObjects (server/moveBoardObjects.js), the multi-selection's
// Move: swimlanes, lists, cards, checklists and checklist items, of one board,
// to a swimlane / list / card / checklist of the same or another board, before
// or after an anchor there.
//
// Everything is checked before anything is written: the caller may change
// both boards; every selected object is of the source board and open; an
// assigned-only member moves only their own cards and what is on them
// (GHSA-r4gm-mjj9-g3x8, recorded as AssignedBleed); the destination's parts
// belong together; nothing moves into itself. Only the topmost of what is
// selected moves - a card whose list is selected moves with the list
// (models/lib/structuralSelection.js). Checklists and items with no card to
// go to make one; items with no checklist, one.

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

var structuralKinds = []string{"swimlane", "list", "card", "checklist", "item"}

var structuralCollections = map[string]string{"swimlane": "swimlanes", "list": "lists", "card": "cards", "checklist": "checklists", "item": "checklistItems"}

// boardDragFields are DRAG_SETTINGS: a board's own "may be dragged".
var boardDragFields = map[string]string{"swimlane": "allowsSwimlaneDragging", "list": "allowsListDragging", "card": "allowsCardDragging",
	"checklist": "allowsChecklistDragging", "item": "allowsChecklistItemDragging"}

type structuralTarget struct {
	BoardID     string `json:"boardId"`
	SwimlaneID  string `json:"swimlaneId,omitempty"`
	ListID      string `json:"listId,omitempty"`
	CardID      string `json:"cardId,omitempty"`
	ChecklistID string `json:"checklistId,omitempty"`
	ItemID      string `json:"itemId,omitempty"`
	Position    string `json:"position,omitempty"`
}

// described is a selected or destination object, with what it lives in.
type described struct {
	Kind, ID, BoardID string
	Doc               bson.M
	Ancestors         []string
	Sort              float64
}

func (d described) key() string { return d.Kind + ":" + d.ID }

func (s *Server) describeObject(ctx context.Context, kind string, doc bson.M) (described, error) {
	d := described{Kind: kind, ID: docString(doc, "_id"), Doc: doc}
	if f, ok := doc["sort"].(float64); ok {
		d.Sort = f
	}
	var card bson.M
	if kind == "card" {
		card = doc
	}
	if kind == "item" {
		d.Ancestors = append(d.Ancestors, "checklist:"+docString(doc, "checklistId"))
	}
	if kind == "checklist" || kind == "item" {
		if s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(docString(doc, "cardId"))}).Decode(&card) != nil {
			return d, &methodError{"not-found", "not-found"}
		}
		d.Ancestors = append(d.Ancestors, "card:"+docString(card, "_id"))
	}
	if card != nil {
		d.Ancestors = append(d.Ancestors, "list:"+docString(card, "listId"), "swimlane:"+docString(card, "swimlaneId"))
		d.BoardID = docString(card, "boardId")
	} else {
		d.BoardID = docString(doc, "boardId")
	}
	if kind == "list" && docString(doc, "swimlaneId") != "" {
		d.Ancestors = append(d.Ancestors, "swimlane:"+docString(doc, "swimlaneId"))
	}
	return d, nil
}

// mutableBoard is requireBoardMutation: a board whose role table lets the
// caller write (memberCan) - an assigned-only member too, whose cards are
// asked about one by one after this.
func (s *Server) mutableBoard(r *http.Request, u *User, id, source string) (*Board, error) {
	var b Board
	if validID(id) && s.db.Collection("boards").FindOne(r.Context(), bson.M{"_id": eq(id)}).Decode(&b) == nil && (u.IsAdmin || b.Can(u.ID, "write")) {
		return &b, nil
	}
	s.recordSecurity(r, securityEvent{Key: "authz.mutation", Action: "blocked", Source: source,
		Detail: "Board mutation denied because the caller lacks write access."})
	return nil, &methodError{"not-authorized", "You cannot change this board."}
}

func (s *Server) methodMoveBoardObjects(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	ctx := r.Context()
	sourceID, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	var selection []struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	}
	var target structuralTarget
	if len(args) < 3 || json.Unmarshal(args[1], &selection) != nil || json.Unmarshal(args[2], &target) != nil || target.BoardID == "" {
		return nil, &methodError{"Match.Error", "Match failed"}
	}
	var opts struct {
		Drag bool `json:"drag"`
	}
	if len(args) > 3 && json.Unmarshal(args[3], &opts) != nil {
		return nil, &methodError{"Match.Error", "Match failed"}
	}
	if target.Position != "" && target.Position != "before" && target.Position != "after" {
		return nil, &methodError{"Match.Error", "Match failed"}
	}
	if len(selection) == 0 || len(selection) > 200 {
		return nil, &methodError{"invalid-selection", "invalid-selection"}
	}
	for _, e := range selection {
		if !slices.Contains(structuralKinds, e.Kind) || !validID(e.ID) {
			return nil, &methodError{"invalid-selection", "invalid-selection"}
		}
	}
	src, err := s.mutableBoard(r, u, sourceID, "moveBoardObjects")
	if err != nil {
		return nil, err
	}
	if opts.Drag {
		var raw bson.M
		_ = s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(src.ID)}).Decode(&raw)
		for _, e := range selection {
			if raw[boardDragFields[e.Kind]] == false {
				return nil, &methodError{"drag-disabled", "drag-disabled"}
			}
		}
	}
	dest, destBoard, err := s.structuralDestination(r, u, target)
	if err != nil {
		return nil, err
	}
	m := src.member(u.ID)
	var entries []described
	for _, e := range selection {
		var doc bson.M
		if s.db.Collection(structuralCollections[e.Kind]).FindOne(ctx, bson.M{"_id": eq(e.ID)}).Decode(&doc) != nil || doc["archived"] == true {
			return nil, &methodError{"not-found", "not-found"}
		}
		d, err := s.describeObject(ctx, e.Kind, doc)
		if err != nil {
			return nil, err
		}
		if d.BoardID != src.ID {
			return nil, &methodError{"invalid-selection", "invalid-selection"}
		}
		// An assigned-only member moves only their own cards and what is on them.
		if m != nil && m.AssignedOnly() && !u.IsAdmin {
			var owner bson.M
			switch e.Kind {
			case "card":
				owner = doc
			case "checklist", "item":
				_ = s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(docString(doc, "cardId"))}).Decode(&owner)
			}
			if owner == nil || !slices.Contains(stringsOf(owner["assignees"]), u.ID) {
				s.recordSecurity(r, securityEvent{Key: "authz.assigned", Action: "blocked", Source: "move-board-objects", UserID: u.ID,
					Detail: "Assigned-only member tried to move something they cannot see."})
				return nil, &methodError{"not-found", "not-found"}
			}
		}
		entries = append(entries, d)
	}
	roots := selectionRoots(entries)
	rootKeys := map[string]bool{}
	for _, d := range roots {
		rootKeys[d.key()] = true
	}
	for _, d := range dest {
		for _, k := range append([]string{d.key()}, d.Ancestors...) {
			if rootKeys[k] {
				return nil, &methodError{"invalid-destination", "Choose a destination outside the selected objects."}
			}
		}
	}
	needsLane, needsList := false, false
	grouped := map[string][]described{}
	for _, d := range roots {
		grouped[d.Kind] = append(grouped[d.Kind], d)
		needsLane = needsLane || d.Kind != "swimlane"
		needsList = needsList || d.Kind == "card" || d.Kind == "checklist" || d.Kind == "item"
	}
	if (needsLane && dest["swimlane"].ID == "") || (needsList && dest["list"].ID == "") {
		return nil, &methodError{"invalid-destination", "Choose a destination swimlane and list."}
	}

	// Every check is done: now the writes.
	now := time.Now().UTC()
	set := func(coll string, id string, fields bson.M) {
		fields["modifiedAt"] = now
		_, _ = s.db.Collection(coll).UpdateOne(ctx, bson.M{"_id": eq(id)}, bson.M{"$set": fields})
	}
	moveCard := func(card bson.M, laneID, listID string) error {
		if docString(card, "boardId") != destBoard.ID {
			var from Board
			if s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(docString(card, "boardId"))}).Decode(&from) != nil {
				return &methodError{"not-found", "not-found"}
			}
			sortWas := card["sort"]
			if err := s.moveCardTo(ctx, u, card, &from, destBoard, laneID, listID); err != nil {
				return err
			}
			set("cards", docString(card, "_id"), bson.M{"sort": sortWas})
			return nil
		}
		_, err := s.cardsUpdateOne(ctx, u.ID, bson.M{"_id": eq(docString(card, "_id"))},
			bson.M{"$set": bson.M{"swimlaneId": laneID, "listId": listID, "modifiedAt": now}})
		return err
	}
	cardsWhere := func(filter bson.M) []bson.M {
		cur, err := s.db.Collection("cards").Find(ctx, filter)
		if err != nil {
			return nil
		}
		var out []bson.M
		_ = cur.All(ctx, &out)
		return out
	}
	for _, lane := range grouped["swimlane"] {
		set("swimlanes", lane.ID, bson.M{"boardId": destBoard.ID})
		if src.ID != destBoard.ID {
			cur, _ := s.db.Collection("lists").Find(ctx, bson.M{"boardId": src.ID, "swimlaneId": eq(lane.ID)})
			var lists []bson.M
			if cur != nil {
				_ = cur.All(ctx, &lists)
			}
			mapping := map[string]string{}
			for _, l := range lists {
				set("lists", docString(l, "_id"), bson.M{"boardId": destBoard.ID})
				mapping[docString(l, "_id")] = docString(l, "_id")
			}
			for _, card := range cardsWhere(bson.M{"boardId": src.ID, "swimlaneId": eq(lane.ID)}) {
				listID := docString(card, "listId")
				if _, ok := mapping[listID]; !ok {
					// A board-wide list of the source: a copy of it goes with the lane.
					var l bson.M
					if s.db.Collection("lists").FindOne(ctx, bson.M{"_id": eq(listID)}).Decode(&l) == nil {
						l["_id"], l["boardId"], l["swimlaneId"] = randomID(), destBoard.ID, lane.ID
						_, _ = s.db.Collection("lists").InsertOne(ctx, l)
						mapping[listID] = docString(l, "_id")
					}
				}
				if err := moveCard(card, lane.ID, mapping[listID]); err != nil {
					return nil, err
				}
			}
		}
	}
	if len(grouped["swimlane"]) > 0 && src.ID != destBoard.ID {
		// The source keeps a lane to show its board-wide lists in.
		if s.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": src.ID, "archived": false}).Err() != nil {
			_, _ = s.db.Collection("swimlanes").InsertOne(ctx, bson.M{"_id": randomID(), "title": "Default", "boardId": src.ID, "type": "swimlane", // inserted, not a filter
				"archived": false, "sort": 0, "createdAt": now})
		}
	}
	for _, l := range grouped["list"] {
		set("lists", l.ID, bson.M{"boardId": destBoard.ID, "swimlaneId": dest["swimlane"].ID})
		for _, card := range cardsWhere(bson.M{"boardId": src.ID, "listId": eq(l.ID)}) {
			if err := moveCard(card, dest["swimlane"].ID, l.ID); err != nil {
				return nil, err
			}
		}
	}
	for _, c := range grouped["card"] {
		var live bson.M
		if s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(c.ID)}).Decode(&live) == nil {
			if err := moveCard(live, dest["swimlane"].ID, dest["list"].ID); err != nil {
				return nil, err
			}
		}
	}
	cardID := dest["card"].ID
	if (len(grouped["checklist"]) > 0 || len(grouped["item"]) > 0) && cardID == "" {
		first := append(append([]described{}, grouped["checklist"]...), grouped["item"]...)[0]
		n, err := s.AddCard(ctx, destBoard, u, dest["list"].ID, dest["swimlane"].ID, docString(first.Doc, "title"))
		if err != nil {
			return nil, err
		}
		cardID = n.ID
		grouped["card"] = append(grouped["card"], described{Kind: "card", ID: cardID})
	}
	for _, cl := range grouped["checklist"] {
		_, _ = s.db.Collection("activities").UpdateMany(ctx, bson.M{"checklistId": eq(cl.ID)}, bson.M{"$set": bson.M{"cardId": cardID}})
		_, _ = s.db.Collection("checklistItems").UpdateMany(ctx, bson.M{"checklistId": eq(cl.ID)}, bson.M{"$set": bson.M{"cardId": cardID, "boardId": destBoard.ID, "modifiedAt": now}})
		set("checklists", cl.ID, bson.M{"cardId": cardID, "boardId": destBoard.ID})
	}
	checklistID := dest["checklist"].ID
	if len(grouped["item"]) > 0 && checklistID == "" {
		checklistID = randomID()
		_, _ = s.db.Collection("checklists").InsertOne(ctx, bson.M{"_id": checklistID, "cardId": cardID, "boardId": destBoard.ID, // inserted, not a filter
			"title": docString(grouped["item"][0].Doc, "title"), "sort": 0.0, "createdAt": now})
		grouped["checklist"] = append(grouped["checklist"], described{Kind: "checklist", ID: checklistID})
	}
	for _, it := range grouped["item"] {
		set("checklistItems", it.ID, bson.M{"cardId": cardID, "checklistId": checklistID, "boardId": destBoard.ID})
		_, _ = s.db.Collection("activities").UpdateMany(ctx, bson.M{"checklistItemId": eq(it.ID)},
			bson.M{"$set": bson.M{"cardId": cardID, "checklistId": checklistID, "boardId": destBoard.ID}})
	}
	lane, list := dest["swimlane"].ID, dest["list"].ID
	selectors := map[string]bson.M{
		"swimlane": {"boardId": destBoard.ID, "archived": false},
		// The lane's own lists and the board-wide ones (no swimlaneId, or "").
		"list":      {"boardId": destBoard.ID, "archived": false, "$or": bson.A{bson.M{"swimlaneId": eq(lane)}, bson.M{"swimlaneId": bson.M{"$in": bson.A{nil, ""}}}}},
		"card":      {"boardId": destBoard.ID, "swimlaneId": eq(lane), "listId": eq(list), "archived": false},
		"checklist": {"cardId": cardID},
		"item":      {"checklistId": checklistID},
	}
	anchors := map[string]string{"swimlane": target.SwimlaneID, "list": target.ListID, "card": target.CardID, "checklist": target.ChecklistID, "item": target.ItemID}
	for _, kind := range structuralKinds {
		if len(grouped[kind]) == 0 {
			continue
		}
		var ids []string
		for _, d := range grouped[kind] {
			ids = append(ids, d.ID)
		}
		if err := s.positionGroup(ctx, structuralCollections[kind], ids, selectors[kind], anchors[kind], target.Position != "before"); err != nil {
			return nil, err
		}
	}
	s.hub.publishFrom(src.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	if destBoard.ID != src.ID {
		s.hub.publishFrom(destBoard.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	}
	return map[string]any{"moved": len(roots), "cardId": cardID, "checklistId": checklistID}, nil
}

// structuralDestination checks the target: a board the caller may change,
// and its parts open and of it, each in the one before it.
func (s *Server) structuralDestination(r *http.Request, u *User, t structuralTarget) (map[string]described, *Board, error) {
	ctx := r.Context()
	b, err := s.mutableBoard(r, u, t.BoardID, "moveBoardObjects")
	if err != nil {
		return nil, nil, err
	}
	bad := &methodError{"invalid-destination", "invalid-destination"}
	out := map[string]described{}
	for _, p := range []struct{ kind, id string }{{"swimlane", t.SwimlaneID}, {"list", t.ListID}, {"card", t.CardID}, {"checklist", t.ChecklistID}, {"item", t.ItemID}} {
		if p.id == "" {
			continue
		}
		var doc bson.M
		if !validID(p.id) || s.db.Collection(structuralCollections[p.kind]).FindOne(ctx, bson.M{"_id": eq(p.id)}).Decode(&doc) != nil || doc["archived"] == true {
			return nil, nil, bad
		}
		d, err := s.describeObject(ctx, p.kind, doc)
		if err != nil || d.BoardID != b.ID {
			return nil, nil, bad
		}
		out[p.kind] = d
	}
	if l, ok := out["list"]; ok && docString(l.Doc, "swimlaneId") != "" && docString(l.Doc, "swimlaneId") != t.SwimlaneID {
		return nil, nil, bad
	}
	if c, ok := out["card"]; ok && (docString(c.Doc, "listId") != t.ListID || docString(c.Doc, "swimlaneId") != t.SwimlaneID) {
		return nil, nil, bad
	}
	if c, ok := out["checklist"]; ok && docString(c.Doc, "cardId") != t.CardID {
		return nil, nil, bad
	}
	if c, ok := out["item"]; ok && docString(c.Doc, "checklistId") != t.ChecklistID {
		return nil, nil, bad
	}
	return out, b, nil
}

// selectionRoots is structuralSelection.js: once each, without what lives in
// another selected object, in kind order, then sort, then id.
func selectionRoots(entries []described) []described {
	seen := map[string]bool{}
	var unique []described
	for _, e := range entries {
		if !seen[e.key()] {
			seen[e.key()] = true
			unique = append(unique, e)
		}
	}
	var out []described
	for _, e := range unique {
		nested := false
		for _, a := range e.Ancestors {
			nested = nested || seen[a]
		}
		if !nested {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		ki, kj := slices.Index(structuralKinds, out[i].Kind), slices.Index(structuralKinds, out[j].Kind)
		if ki != kj {
			return ki < kj
		}
		if out[i].Sort != out[j].Sort {
			return out[i].Sort < out[j].Sort
		}
		return strings.Compare(out[i].ID, out[j].ID) < 0
	})
	return out
}

// positionGroup puts ids, in their order, before or after the anchor among
// their new siblings (at the end, or the start, without one), numbering the
// siblings again so repeated moves never run out of room between sorts.
func (s *Server) positionGroup(ctx context.Context, coll string, ids []string, selector bson.M, anchor string, after bool) error {
	cur, err := s.db.Collection(coll).Find(ctx, selector, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}, {Key: "_id", Value: 1}}).SetProjection(bson.M{"_id": 1}))
	if err != nil {
		return err
	}
	var docs []struct {
		ID string `bson:"_id"`
	}
	_ = cur.All(ctx, &docs)
	var order []string
	for _, d := range docs {
		if !slices.Contains(ids, d.ID) {
			order = append(order, d.ID)
		}
	}
	index := 0
	switch {
	case anchor != "":
		index = slices.Index(order, anchor)
		if index < 0 {
			return &methodError{"invalid-position", "invalid-position"}
		}
		if after {
			index++
		}
	case after:
		index = len(order)
	}
	order = slices.Insert(order, index, ids...)
	for i, id := range order {
		_, _ = s.db.Collection(coll).UpdateOne(ctx, bson.M{"_id": eq(id)}, bson.M{"$set": bson.M{"sort": float64(i)}})
	}
	return nil
}
