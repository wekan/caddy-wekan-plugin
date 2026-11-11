package web

// WeKan's copyList and copySwimlane methods (server/models/lists.js,
// server/publications/swimlanes.js and Swimlane.copy): a new list, or a new
// swimlane with its lists, beside the one named, every card copied with it.
// Scrum follows models/lib/scrumCopy.js - a list keeps its category, a
// swimlane its purpose, and its sprint and release on its own board or by a
// unique name on another - with the copy's revision at 1. The source board
// must be one the user may change and read whole (an assigned-only member
// would copy cards they cannot see), the destination one they may change.

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func (s *Server) laneCopyBoards(r *http.Request, u *User, sourceBoard, toBoard string, write bool) (*Board, *Board, error) {
	from, err := s.boardFor(r.Context(), sourceBoard, u)
	denied := &methodError{"not-authorized", "You cannot copy or move this."}
	if err != nil || from.member(u.ID) == nil && !u.IsAdmin { // not a write: the source must be the user's board to read whole
		return nil, nil, denied
	}
	if write && !from.Writable(u, "write", nil) || assignedOnlyOn(from, u) {
		return nil, nil, denied
	}
	to, err := s.boardFor(r.Context(), toBoard, u)
	if err != nil {
		return nil, nil, &methodError{"board-not-found", "Target board not found"}
	}
	if !to.Writable(u, "write", nil) {
		return nil, nil, denied
	}
	return from, to, nil
}

// neighborSort is the sort beside a neighbor - after it, or before it
// ("left", "above") - else at the end.
func neighborSort(rows []map[string]any, neighbor string, before bool) float64 {
	sort.SliceStable(rows, func(i, j int) bool { return numberOf(rows[i]["sort"]) < numberOf(rows[j]["sort"]) })
	for i, r := range rows {
		if docString(r, "_id") != neighbor {
			continue
		}
		at := numberOf(r["sort"])
		if at != at {
			break
		}
		if before {
			if i > 0 && numberOf(rows[i-1]["sort"]) == numberOf(rows[i-1]["sort"]) {
				return (numberOf(rows[i-1]["sort"]) + at) / 2
			}
			return at - 1
		}
		if i+1 < len(rows) && numberOf(rows[i+1]["sort"]) == numberOf(rows[i+1]["sort"]) {
			return (at + numberOf(rows[i+1]["sort"])) / 2
		}
		return at + 1
	}
	return float64(len(rows))
}

// methodCopyList is copyList(listId, boardId, swimlaneId, title, neighborListId, position).
func (s *Server) methodCopyList(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	var str [4]string
	for i := range str {
		v, err := stringArg(args, i)
		if err != nil {
			return nil, err
		}
		str[i] = v
	}
	listID, boardID, swimlaneID, title := str[0], str[1], str[2], str[3]
	neighbor, err := optionalString(args, 4)
	if err != nil {
		return nil, err
	}
	position, err := optionalString(args, 5)
	if err != nil {
		return nil, err
	}
	ctx := r.Context()
	var list bson.M
	if !validID(listID) || s.db.Collection("lists").FindOne(ctx, bson.M{"_id": eq(listID)}).Decode(&list) != nil {
		return nil, &methodError{"list-not-found", "List not found"}
	}
	from, to, err := s.laneCopyBoards(r, u, docString(list, "boardId"), boardID, true)
	if err != nil {
		return nil, err
	}
	lists := s.findDocs(ctx, "lists", bson.M{"boardId": eq(to.ID), "archived": false})
	at := float64(len(lists))
	if neighbor != "" {
		at = neighborSort(s.findDocs(ctx, "lists", bson.M{"boardId": eq(to.ID), "swimlaneId": eq(swimlaneID), "archived": false}), neighbor, position == "left")
	}
	if jsTrim(title) == "" {
		title = docString(list, "title")
	}
	id := randomID()
	doc := bson.M{"_id": id, "title": title, "boardId": to.ID, "swimlaneId": swimlaneID, "type": list["type"], "archived": false, // inserted, not a filter
		"wipLimit": list["wipLimit"], "sort": at, "createdAt": time.Now().UTC(), "syncCredentialIncarnation": randomID()}
	for k, v := range copiedScrum(plain(list).(map[string]any), to.ID, nil) {
		doc[k] = v
	}
	if _, err := s.db.Collection("lists").InsertOne(ctx, doc); err != nil {
		return nil, err
	}
	for _, c := range s.laneCards(ctx, bson.M{"listId": eq(listID)}) {
		if _, err := s.copyCardTo(r, u, c, from, to, swimlaneID, id); err != nil {
			return nil, err
		}
	}
	s.hub.publishFrom(to.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	return id, nil
}

// methodCopySwimlane is copySwimlane(swimlaneId, toBoardId, targetSwimlaneId, position, title).
func (s *Server) methodCopySwimlane(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	laneID, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	toBoard, err := stringArg(args, 1)
	if err != nil {
		return nil, err
	}
	target, err := optionalString(args, 2)
	if err != nil {
		return nil, err
	}
	position, err := optionalString(args, 3)
	if err != nil {
		return nil, err
	}
	if position == "" {
		position = "below"
	}
	if position != "above" && position != "below" {
		return nil, &methodError{"Match.Error", "Match failed"}
	}
	title, err := optionalString(args, 4)
	if err != nil {
		return nil, err
	}
	ctx := r.Context()
	var lane bson.M
	if !validID(laneID) || s.db.Collection("swimlanes").FindOne(ctx, bson.M{"_id": eq(laneID)}).Decode(&lane) != nil {
		return nil, &methodError{"not-found", "not-found"}
	}
	from, to, err := s.laneCopyBoards(r, u, docString(lane, "boardId"), toBoard, false)
	if err != nil {
		return nil, err
	}
	lanes := s.findDocs(ctx, "swimlanes", bson.M{"boardId": eq(to.ID), "archived": false})
	at := float64(len(lanes))
	if target != "" {
		at = neighborSort(lanes, target, position == "above")
	}
	doc := bson.M{}
	for k, v := range lane {
		doc[k] = v
	}
	delete(doc, "scrum")
	delete(doc, "scrumRevision")
	for k, v := range copiedScrum(plain(lane).(map[string]any), to.ID, s.scrumPlanningPair(ctx, from.ID, to.ID)) {
		doc[k] = v
	}
	id := randomID()
	doc["_id"], doc["boardId"], doc["sort"] = id, to.ID, at
	if t := strings.TrimSpace(title); t != "" {
		doc["title"] = t
	}
	if _, err := s.db.Collection("swimlanes").InsertOne(ctx, doc); err != nil {
		return nil, err
	}
	first := s.firstSwimlane(ctx, from.ID) == laneID
	for _, l := range s.swimlaneLists(ctx, from, laneID) {
		listID := randomID()
		nl := bson.M{"_id": listID, "title": l["title"], "boardId": to.ID, "sort": l["sort"], "type": orDefault(str(l, "type"), "list"), // inserted, not a filter
			"archived": false, "wipLimit": l["wipLimit"], "swimlaneId": id, "color": l["color"], "width": l["width"], "createdAt": time.Now().UTC(),
			"syncCredentialIncarnation": randomID()}
		for k, v := range copiedScrum(plain(l).(map[string]any), to.ID, nil) {
			nl[k] = v
		}
		if _, err := s.db.Collection("lists").InsertOne(ctx, nl); err != nil {
			return nil, err
		}
		filter := bson.M{"listId": l["_id"], "swimlaneId": eq(laneID)}
		if first {
			filter["swimlaneId"] = bson.M{"$in": bson.A{laneID, "", nil}}
		}
		for _, c := range s.laneCards(ctx, filter) {
			if _, err := s.copyCardTo(r, u, c, from, to, id, listID); err != nil {
				return nil, err
			}
		}
	}
	s.hub.publishFrom(to.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	return true, nil
}

// methodMoveSwimlane is moveSwimlane(swimlaneId, toBoardId, targetSwimlaneId,
// position, title): a member of the swimlane's board who may change both.
func (s *Server) methodMoveSwimlane(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	laneID, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	toBoard, err := stringArg(args, 1)
	if err != nil {
		return nil, err
	}
	title, err := optionalString(args, 4)
	if err != nil {
		return nil, err
	}
	var lane bson.M
	if !validID(laneID) || s.db.Collection("swimlanes").FindOne(r.Context(), bson.M{"_id": eq(laneID)}).Decode(&lane) != nil {
		return nil, &methodError{"not-found", "not-found"}
	}
	from, to, err := s.laneCopyBoards(r, u, docString(lane, "boardId"), toBoard, true)
	if err != nil {
		return nil, err
	}
	if lerr := s.moveSwimlaneTo(r, u, from, to, lane, &laneRequest{ToBoardID: to.ID, Title: title}); lerr != nil {
		return nil, &methodError{"move-failed", lerr.msg}
	}
	return true, nil
}
