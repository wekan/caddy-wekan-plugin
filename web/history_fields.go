package web

// Change history, step 4: field edits - WeKan's models/lib/changeHistoryGroups.js
// (which field is which group, and the content a row stores for it) and
// server/models/changeHistoryHooks.js recordUpdate (one row per changed
// group; a save that changes several groups shares a batch, so one undo puts
// the whole save back). WeKan records from a hook after every collection
// update; Weddy writes the database from each route, so a write is recorded
// by going through updateRecorded, and a guard (history_fields_test.go)
// fails on a card write that does not.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// historyFieldGroups: field -> group, per entity (FIELDS_BY_ENTITY). A field
// that is not here is not recorded.
var historyFieldGroups = map[string]map[string]string{
	"card": {"title": "title", "description": "description", "labelIds": "labels", "members": "members", "assignees": "assignees",
		"requesters": "members", "assigners": "assignees", "receivedAt": "dates", "startAt": "dates", "dueAt": "dates", "endAt": "dates",
		"dueComplete": "dates", "spentTime": "dates", "isOvertime": "dates", "customFields": "customFields", "cardDependencies": "dependencies",
		"color": "title", "vote": "customFields", "poker": "customFields", "archived": "lifecycle", "deletedAt": "lifecycle"},
	"list":          {"title": "title", "color": "title", "wipLimit": "title", "archived": "lifecycle", "deletedAt": "lifecycle"},
	"swimlane":      {"title": "title", "color": "title", "archived": "lifecycle", "deletedAt": "lifecycle"},
	"checklist":     {"title": "checklists", "hideCheckedChecklistItems": "checklists", "hideAllChecklistItems": "checklists"},
	"checklistItem": {"title": "checklists", "isFinished": "checklists"},
	"comment":       {"text": "comments"},
	"attachment":    {"name": "attachments"},
}

var historyNeverRecord = map[string]bool{"_id": true, "modifiedAt": true, "dateLastActivity": true, "createdAt": true, "updatedAt": true,
	"sort": true, "subtaskSort": true, "boardId": true, "swimlaneId": true, "listId": true, "cardId": true, "parentId": true}

// historyCollections: each entity's collection.
var historyCollections = map[string]string{"card": "cards", "list": "lists", "swimlane": "swimlanes", "checklist": "checklists",
	"checklistItem": "checklistItems", "comment": "card_comments", "attachment": "attachments"}

func historyGroupFor(entity, field string) string {
	if historyNeverRecord[field] {
		return ""
	}
	return historyFieldGroups[entity][field]
}

// jsMillis is a Date as JavaScript's getTime().
func jsMillis(t time.Time) float64 { return float64(t.UnixMilli()) }

// historyContent is contentForField: nil for a missing value.
func historyContent(field string, value any, present bool) any {
	if !present {
		return nil
	}
	value = plain(value)
	switch x := value.(type) {
	case nil:
		return map[string]any{"field": field, "value": nil}
	case time.Time:
		return map[string]any{"field": field, "value": jsMillis(x), "isDate": true}
	case string, bool:
		return map[string]any{"field": field, "value": x}
	case float64, int32, int64, int:
		return map[string]any{"field": field, "value": historyNumber(x)}
	case []any, map[string]any, []string:
		return nestedHistoryContent(field, x)
	}
	return nil
}

func historyNumber(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int32:
		return float64(x)
	case int64:
		return float64(x)
	case int:
		return float64(x)
	}
	return math.NaN()
}

// nestedHistoryContent: dates as numbers, their paths kept (as JSON strings,
// the form FerretDB stores) so they come back as dates.
func nestedHistoryContent(field string, value any) any {
	var paths []any
	var encode func(v any, path []string) any
	encode = func(v any, path []string) any {
		switch x := v.(type) {
		case time.Time:
			p, _ := json.Marshal(path)
			paths = append(paths, string(p))
			return jsMillis(x)
		case []string:
			out := make([]any, len(x))
			for i, e := range x {
				out[i] = e
			}
			return out
		case []any:
			out := make([]any, len(x))
			for i, e := range x {
				out[i] = encode(e, append(append([]string{}, path...), strconv.Itoa(i)))
			}
			return out
		case map[string]any:
			out := map[string]any{}
			for k, e := range x {
				out[k] = encode(e, append(append([]string{}, path...), k))
			}
			return out
		case int32, int64, int:
			return historyNumber(x)
		}
		return v
	}
	out := map[string]any{"field": field, "value": encode(value, nil)}
	if len(paths) > 0 {
		out["datePaths"] = paths
	}
	return out
}

// historyValue is valueFromContent: the value to write back, ok false when
// the content says nothing.
func historyValue(content map[string]any) (any, bool) {
	v, ok := content["value"]
	if !ok {
		return nil, false
	}
	if content["isDate"] == true {
		if v == nil {
			return nil, true
		}
		return time.UnixMilli(int64(historyNumber(v))).UTC(), true
	}
	paths, _ := content["datePaths"].([]any)
	if len(paths) == 0 {
		return v, true
	}
	raw, _ := json.Marshal(v)
	var copied any
	_ = json.Unmarshal(raw, &copied)
	for _, p := range paths {
		var path []string
		if s, ok := p.(string); !ok || json.Unmarshal([]byte(s), &path) != nil || len(path) == 0 {
			return nil, false
		}
		parent := copied
		for _, k := range path[:len(path)-1] {
			switch x := parent.(type) {
			case map[string]any:
				parent = x[k]
			case []any:
				i, err := strconv.Atoi(k)
				if err != nil || i < 0 || i >= len(x) {
					return nil, false
				}
				parent = x[i]
			default:
				return nil, false
			}
		}
		last := path[len(path)-1]
		switch x := parent.(type) {
		case map[string]any:
			f, ok := x[last].(float64)
			if !ok {
				return nil, false
			}
			x[last] = time.UnixMilli(int64(f)).UTC()
		case []any:
			i, err := strconv.Atoi(last)
			if err != nil || i < 0 || i >= len(x) {
				return nil, false
			}
			f, ok := x[i].(float64)
			if !ok {
				return nil, false
			}
			x[i] = time.UnixMilli(int64(f)).UTC()
		default:
			return nil, false
		}
	}
	return copied, true
}

func historyEmpty(v any, present bool) bool {
	if !present || v == nil || v == "" {
		return true
	}
	switch x := v.(type) {
	case []any:
		return len(x) == 0
	case []string:
		return len(x) == 0
	}
	return false
}

// historyChanged is changed(): the value really differs.
func historyChanged(before any, hb bool, after any, ha bool) bool {
	bEmpty, aEmpty := !hb || before == nil, !ha || after == nil
	if bEmpty || aEmpty {
		return bEmpty != aEmpty
	}
	return canonical(historyContent("", before, true)) != canonical(historyContent("", after, true))
}

// historyFieldChange is one row of a diff.
type historyFieldChange struct {
	Group, ChangeType           string
	PreviousContent, NewContent any
}

// diffHistoryFields is diffFields over the fields an update named.
func diffHistoryFields(entity string, before, after map[string]any, fields []string) []historyFieldChange {
	var out []historyFieldChange
	for _, f := range fields {
		group := historyGroupFor(entity, f)
		if group == "" {
			continue
		}
		b, hb := before[f]
		a, ha := after[f]
		if f == "cardDependencies" {
			if !hb || b == nil {
				b, hb = []any{}, true
			}
			if !ha || a == nil {
				a, ha = []any{}, true
			}
		}
		b, a = plain(b), plain(a)
		if !historyChanged(b, hb, a, ha) {
			continue
		}
		pc, nc := historyContent(f, b, hb), historyContent(f, a, ha)
		if pc == nil && nc == nil {
			continue
		}
		ct := "edited"
		if historyEmpty(b, hb) && !historyEmpty(a, ha) {
			ct = "added"
		} else if !historyEmpty(b, hb) && historyEmpty(a, ha) {
			ct = "removed"
		}
		out = append(out, historyFieldChange{Group: group, ChangeType: ct, PreviousContent: pc, NewContent: nc})
	}
	return out
}

// updateFields: the top-level fields an update modifier names.
func updateFields(update bson.M) []string {
	seen := map[string]bool{}
	var out []string
	for op, v := range update {
		m, ok := v.(bson.M)
		if !ok {
			if mm, isMap := v.(map[string]any); isMap {
				m, ok = bson.M(mm), true
			}
		}
		if !ok || len(op) == 0 || op[0] != '$' {
			continue
		}
		for k := range m {
			top := k
			for i, c := range k {
				if c == '.' {
					top = k[:i]
					break
				}
			}
			if !seen[top] {
				seen[top] = true
				out = append(out, top)
			}
		}
	}
	return out
}

// updateRecorded is UpdateOne of one entity by id, recorded as WeKan's hook
// records it: the document read before and after, each changed group a row,
// several sharing a batch.
func (s *Server) updateRecorded(ctx context.Context, userID, entity, id string, update bson.M) error {
	coll := s.db.Collection(historyCollections[entity])
	var before bson.M
	_ = coll.FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&before)
	if _, err := coll.UpdateOne(ctx, bson.M{"_id": eq(id)}, update); err != nil {
		return err
	}
	if before == nil || userID == "" {
		return nil
	}
	var after bson.M
	if coll.FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&after) != nil {
		return nil
	}
	s.recordFieldChanges(ctx, userID, entity, plain(before).(map[string]any), plain(after).(map[string]any), updateFields(update))
	return nil
}

// recordFieldChanges is recordUpdate: located on its board, each change a row.
func (s *Server) recordFieldChanges(ctx context.Context, userID, entity string, before, after map[string]any, fields []string) {
	defer func() { _ = recover() }() // recording never fails the write
	changes := diffHistoryFields(entity, before, after, fields)
	if entity == "card" {
		if p := historyPositionChange(before, after, fields); p != nil {
			changes = append(changes, *p)
		}
	}
	if len(changes) == 0 {
		return
	}
	where := s.historyLocate(ctx, entity, after)
	if where.BoardID == "" {
		return
	}
	var batch any
	if len(changes) > 1 {
		batch = fmt.Sprintf("edit-%s-%d", docString(after, "_id"), time.Now().UnixMilli())
	}
	for _, c := range changes {
		row := where
		row.EntityType, row.EntityID, row.Group, row.ChangeType = entity, docString(after, "_id"), c.Group, c.ChangeType
		row.PreviousContent, row.NewContent, row.UserID, row.BatchID = c.PreviousContent, c.NewContent, userID, batch
		s.recordHistory(ctx, row)
	}
}

// historyLocate is locate: the board, swimlane, list and card a row sits in.
func (s *Server) historyLocate(ctx context.Context, entity string, doc map[string]any) historyChange {
	card := func(id string) historyChange {
		var c bson.M
		if s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&c) != nil {
			return historyChange{}
		}
		return historyChange{BoardID: docString(c, "boardId"), SwimlaneID: c["swimlaneId"], ListID: c["listId"], CardID: docString(c, "_id")}
	}
	switch entity {
	case "card":
		return historyChange{BoardID: docString(doc, "boardId"), SwimlaneID: doc["swimlaneId"], ListID: doc["listId"], CardID: docString(doc, "_id")}
	case "list":
		return historyChange{BoardID: docString(doc, "boardId"), SwimlaneID: doc["swimlaneId"], ListID: docString(doc, "_id")}
	case "swimlane":
		return historyChange{BoardID: docString(doc, "boardId"), SwimlaneID: docString(doc, "_id")}
	case "checklist", "checklistItem", "comment":
		return card(docString(doc, "cardId"))
	case "attachment":
		meta, _ := doc["meta"].(map[string]any)
		if id, _ := meta["cardId"].(string); id != "" {
			if w := card(id); w.BoardID != "" {
				return w
			}
		}
		b, _ := meta["boardId"].(string)
		return historyChange{BoardID: b}
	}
	return historyChange{}
}

// cardsUpdateOne is UpdateOne on cards, recorded: the card the filter names
// read before and after, its changes a row each. What the routes of the
// card details and the REST API write goes through it.
func (s *Server) cardsUpdateOne(ctx context.Context, userID string, filter any, update any, opts ...options.Lister[options.UpdateOneOptions]) (*mongo.UpdateResult, error) {
	return s.entityUpdateOne(ctx, userID, "card", filter, update, opts...)
}

// entityUpdateOne is cardsUpdateOne for any recorded entity: a list, a
// swimlane, a checklist, an item or a comment.
func (s *Server) entityUpdateOne(ctx context.Context, userID, entity string, filter any, update any, opts ...options.Lister[options.UpdateOneOptions]) (*mongo.UpdateResult, error) {
	coll := s.db.Collection(historyCollections[entity])
	var before bson.M
	_ = coll.FindOne(ctx, filter).Decode(&before)
	res, err := coll.UpdateOne(ctx, filter, update, opts...)
	if err != nil || before == nil || userID == "" || res.ModifiedCount == 0 {
		return res, err
	}
	var after bson.M
	if coll.FindOne(ctx, bson.M{"_id": before["_id"]}).Decode(&after) == nil {
		u, _ := update.(bson.M)
		s.recordFieldChanges(ctx, userID, entity, plain(before).(map[string]any), plain(after).(map[string]any), updateFields(u))
	}
	return res, nil
}

// historyPositionChange is timeHistory.js positionChange: a card moved - its
// board, swimlane, list or sort changed - is one "moved" row of its whole
// position, its move reason with it.
func historyPositionChange(before, after map[string]any, fields []string) *historyFieldChange {
	keys := []string{"boardId", "swimlaneId", "listId", "sort"}
	moved := false
	for _, k := range keys {
		for _, f := range fields {
			if f == k && canonical(plain(before[k])) != canonical(plain(after[k])) {
				moved = true
			}
		}
	}
	if !moved {
		return nil
	}
	snap := func(d map[string]any) map[string]any {
		out := map[string]any{}
		for _, k := range append(keys, "lastMoveReason") {
			v, ok := d[k]
			switch {
			case !ok && k == "lastMoveReason":
				out[k] = ""
			case !ok:
				out[k] = nil
			case k == "sort":
				out[k] = historyNumber(v)
			default:
				out[k] = v
			}
		}
		return out
	}
	return &historyFieldChange{Group: "position", ChangeType: "moved", PreviousContent: snap(before), NewContent: snap(after)}
}

// actorID is a rule's actor, "" when a rule runs without one (and so is not
// recorded, as WeKan's hooks record nothing without an author).
func actorID(u *User) string {
	if u == nil {
		return ""
	}
	return u.ID
}

// historyLifecycleGroups: the group of a sub-entity's added and removed rows
// (recordLifecycle).
var historyLifecycleGroups = map[string]string{"checklist": "checklists", "checklistItem": "checklists", "comment": "comments", "attachment": "attachments"}

// recordLifecycle is WeKan's: a checklist, an item, a comment or an
// attachment added or removed, its whole document the row's content.
func (s *Server) recordLifecycle(ctx context.Context, userID, entity string, doc map[string]any, changeType string) {
	defer func() { _ = recover() }() // recording never fails the write
	if userID == "" || doc == nil {
		return
	}
	where := s.historyLocate(ctx, entity, doc)
	if where.BoardID == "" {
		return
	}
	raw, _ := json.Marshal(jsonForHistory(doc))
	var snapshot any
	_ = json.Unmarshal(raw, &snapshot)
	row := where
	row.EntityType, row.EntityID, row.Group, row.ChangeType, row.UserID = entity, docString(doc, "_id"), historyLifecycleGroups[entity], changeType, userID
	if changeType == "removed" {
		row.PreviousContent = map[string]any{"document": snapshot}
	} else {
		row.NewContent = map[string]any{"document": snapshot}
	}
	s.recordHistory(ctx, row)
}

// jsonForHistory is a document as JSON.parse(JSON.stringify(doc)) gives it:
// a date as its ISO string.
func jsonForHistory(v any) any {
	switch x := plain(v).(type) {
	case time.Time:
		return x.UTC().Format("2006-01-02T15:04:05.000Z")
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			out[k] = jsonForHistory(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = jsonForHistory(e)
		}
		return out
	}
	return v
}

// insertRecorded is InsertOne of a sub-entity, recorded as added.
func (s *Server) insertRecorded(ctx context.Context, userID, entity string, doc any) (*mongo.InsertOneResult, error) {
	coll := s.db.Collection(historyCollections[entity])
	res, err := coll.InsertOne(ctx, doc)
	if err == nil {
		var saved bson.M
		if coll.FindOne(ctx, bson.M{"_id": res.InsertedID}).Decode(&saved) == nil {
			s.recordLifecycle(ctx, userID, entity, plain(saved).(map[string]any), "added")
		}
	}
	return res, err
}

// deleteOneRecorded is DeleteOne of a sub-entity, recorded as removed.
func (s *Server) deleteOneRecorded(ctx context.Context, userID, entity string, filter any) (*mongo.DeleteResult, error) {
	coll := s.db.Collection(historyCollections[entity])
	var doc bson.M
	_ = coll.FindOne(ctx, filter).Decode(&doc)
	res, err := coll.DeleteOne(ctx, filter)
	if err == nil && res.DeletedCount > 0 && doc != nil {
		s.recordLifecycle(ctx, userID, entity, plain(doc).(map[string]any), "removed")
	}
	return res, err
}
