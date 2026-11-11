package web

// Scrum's History, as WeKan's server/lib/scrumHistory.js: a row's records
// put back - undone, redone or restored - by whoever may write each of them
// (a card's by whoever may change the card, a list's or swimlane's by a
// member who may write, the settings and the records by a board admin). An
// undo or redo is refused (scrum-conflict) when any record changed after the
// row; a Restore takes the older state on purpose. Every record is checked
// before the first write: references still on the board, no sprint or
// release deleted while something else names it, and an active sprint's
// estimate settings unchanged. Each write is compare-and-set on the record's
// revision, as WeKan's.

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func scrumHistoryRecords(content any) ([]map[string]any, bool) {
	c, _ := plain(content).(map[string]any)
	list, _ := c["records"].([]any)
	if len(list) == 0 {
		return nil, false
	}
	seen := map[string]bool{}
	out := []map[string]any{}
	for _, x := range list {
		r, _ := x.(map[string]any)
		kind, id := docString(r, "type"), docString(r, "id")
		if r == nil || scrumHistoryCollections[kind] == "" || id == "" || seen[kind+":"+id] {
			return nil, false
		}
		seen[kind+":"+id] = true
		out = append(out, r)
	}
	return out, true
}

// scrumHistorySnapshot is the row's records as they are now.
func (s *Server) scrumHistorySnapshot(ctx context.Context, row map[string]any) any {
	content := row["newContent"]
	if content == nil {
		content = row["previousContent"]
	}
	records, ok := scrumHistoryRecords(content)
	if !ok {
		return nil
	}
	out := bson.A{}
	for _, r := range records {
		kind := docString(r, "type")
		doc := s.findDoc(ctx, scrumHistoryCollections[kind], bson.M{"_id": eq(docString(r, "id"))})
		out = append(out, map[string]any{"type": kind, "id": r["id"], "document": scrumHistoryDocument(kind, doc)})
	}
	return map[string]any{"records": out}
}

// applyScrumHistory puts a row's side back: false when it is not one to
// apply, an error when it may not or no longer can be.
func (s *Server) applyScrumHistory(ctx context.Context, u *User, row map[string]any, direction string) (bool, error) {
	boardID := docString(row, "boardId")
	unlock := scrumLock(boardID)
	defer unlock()
	content := row["newContent"]
	switch {
	case direction == "undo":
		content = row["previousContent"]
	case direction == "restore" && content == nil:
		content = row["previousContent"]
	}
	targets, ok := scrumHistoryRecords(content)
	if !ok {
		return false, &methodError{"invalid-scrum-history", "invalid-scrum-history"}
	}
	b, err := s.boardFor(ctx, boardID, u)
	if err != nil {
		return false, scrumNotAuthorized()
	}
	var raw bson.M
	_ = s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(boardID)}).Decode(&raw)
	if s.db.Collection("scrumSprints").FindOne(ctx, bson.M{"boardId": eq(boardID), "$or": bson.A{bson.M{"rolloverPending": true}, bson.M{"rolloverPending.0": bson.M{"$exists": true}}}}).Err() == nil {
		return false, scrumConflict()
	}
	if err := s.scrumImportPending(ctx, boardID); err != nil {
		return false, err
	}
	current := make([]map[string]any, len(targets))
	for i, t := range targets {
		current[i] = s.findDoc(ctx, scrumHistoryCollections[docString(t, "type")], bson.M{"_id": eq(docString(t, "id"))})
	}
	if err := s.validateScrumTargets(ctx, b, plain(raw).(map[string]any), u, targets, current); err != nil {
		return false, err
	}
	live := bson.A{}
	for i, t := range targets {
		live = append(live, map[string]any{"type": t["type"], "id": t["id"], "document": scrumHistoryDocument(docString(t, "type"), current[i])})
	}
	expected := row["previousContent"]
	if direction == "undo" {
		expected = row["newContent"]
	}
	if direction != "restore" && canonical(map[string]any{"records": live}) != canonical(expected) {
		return false, scrumConflict()
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	for i, t := range targets {
		kind := docString(t, "type")
		coll := s.db.Collection(scrumHistoryCollections[kind])
		target, _ := t["document"].(map[string]any)
		cur := current[i]
		if canonical(scrumHistoryDocument(kind, cur)) == canonical(target) {
			continue
		}
		id := docString(t, "id")
		metadata := scrumMetadataType(kind)
		sel := bson.M{"_id": eq(id)}
		if cur != nil {
			if metadata {
				sel = scrumRevisionSelector(cur)
				sel["_id"] = eq(id)
			} else {
				sel["revision"] = cur["revision"]
			}
		}
		switch {
		case target == nil:
			res, err := coll.DeleteOne(ctx, sel)
			if err != nil || res.DeletedCount == 0 {
				return false, scrumConflict()
			}
		case metadata:
			res, err := coll.UpdateOne(ctx, sel, bson.M{"$set": bson.M{"scrum": target["scrum"], "scrumRevision": int32(revisionOf(cur["scrumRevision"]) + 1)}})
			if err != nil || res.MatchedCount == 0 {
				return false, scrumConflict()
			}
		case cur == nil:
			doc := bson.M{}
			for k, v := range target {
				doc[k] = v
			}
			doc["revision"], doc["updatedAt"], doc["updatedBy"], doc["incarnation"] = int32(1), now, u.ID, randomID()
			if _, err := coll.InsertOne(ctx, doc); err != nil {
				return false, scrumConflict()
			}
		default:
			set := bson.M{}
			for k, v := range target {
				if k != "_id" {
					set[k] = v
				}
			}
			set["revision"], set["updatedAt"], set["updatedBy"] = int32(revisionOf(cur["revision"])+1), now, u.ID
			update := bson.M{"$set": set}
			unset := bson.M{}
			for k := range cur {
				if _, keep := target[k]; !keep && k != "_id" && k != "revision" && k != "updatedAt" && k != "updatedBy" && k != "incarnation" {
					unset[k] = ""
				}
			}
			if len(unset) > 0 {
				update["$unset"] = unset
			}
			res, err := coll.UpdateOne(ctx, sel, update)
			if err != nil || res.MatchedCount == 0 {
				return false, scrumConflict()
			}
		}
	}
	s.hub.publish(boardID, "board-changed")
	return true, nil
}

// validateScrumTargets is validateTargets(): may u write every record, and
// would the result still be consistent.
func (s *Server) validateScrumTargets(ctx context.Context, b *Board, board map[string]any, u *User, targets, current []map[string]any) error {
	final := map[string]map[string]any{}
	has := map[string]bool{}
	for _, t := range targets {
		key := docString(t, "type") + ":" + docString(t, "id")
		has[key] = true
		final[key], _ = t["document"].(map[string]any)
	}
	badRef := &methodError{"invalid-scrum-reference", "A referenced Scrum record is no longer on this board."}
	reference := func(kind, id string) error {
		if id == "" {
			return nil
		}
		doc, ok := final[kind+":"+id]
		if !has[kind+":"+id] || !ok {
			doc = s.findDoc(ctx, scrumHistoryCollections[kind], bson.M{"_id": eq(id)})
		}
		if doc == nil || docString(doc, "boardId") != b.ID {
			return badRef
		}
		return nil
	}
	for i, t := range targets {
		kind, id := docString(t, "type"), docString(t, "id")
		doc, target := current[i], final[kind+":"+id]
		owner := docString(doc, "boardId")
		if kind == "board" {
			owner = docString(doc, "_id")
		}
		if doc != nil && owner != b.ID {
			return scrumConflict()
		}
		if target != nil && (docString(target, "_id") != id || docString(target, "boardId") != b.ID) {
			return scrumConflict()
		}
		switch kind {
		case "card":
			if doc == nil {
				return scrumNotAuthorized()
			}
			var c Card
			_ = s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&c)
			if !b.WritableIn(ctx, s, u, "write", &c) {
				return scrumNotAuthorized()
			}
		case "list", "swimlane":
			if !b.Writable(u, "write", nil) {
				return scrumNotAuthorized()
			}
		default:
			if !b.AdminOf(u.ID) {
				return scrumNotAuthorized()
			}
		}
		meta := target
		if scrumMetadataType(kind) {
			if doc == nil || target == nil {
				return scrumConflict()
			}
			meta, _ = target["scrum"].(map[string]any)
			if err := scrumValidate(func() {
				if kind == "board" {
					normalizeScrumSettings(meta)
				} else {
					normalizeScrumMetadata(kind, meta)
				}
			}); err != nil {
				return err
			}
		}
		if err := reference("scrum-sprint", docString(meta, "sprintId")); err != nil {
			return err
		}
		for _, rid := range cardReleaseIds(meta) {
			if err := reference("scrum-release", rid); err != nil {
				return err
			}
		}
		for _, sid := range stringsOf(meta["pastSprintIds"]) {
			if err := reference("scrum-sprint", sid); err != nil {
				return err
			}
		}
		if kind == "board" && meta["estimateSource"] == "customField" {
			if s.findDoc(ctx, "customFields", bson.M{"_id": eq(docString(meta, "estimateCustomFieldId")), "boardIds": eq(b.ID), "type": "number"}) == nil {
				return badRef
			}
		}
		if kind == "scrum-event" && target != nil {
			for _, cid := range stringsOf(target["followUpCardIds"]) {
				if s.findDoc(ctx, "cards", bson.M{"_id": eq(cid), "boardId": eq(b.ID)}) == nil {
					return badRef
				}
			}
		}
		// Never delete a sprint or release something outside this row names.
		if target == nil && (kind == "scrum-sprint" || kind == "scrum-release") {
			field := "sprintId"
			if kind == "scrum-release" {
				field = "releaseId"
			}
			type ref struct {
				kind, coll string
				filter     bson.M
			}
			refs := []ref{}
			if field == "sprintId" {
				refs = append(refs, ref{"card", "cards", bson.M{"boardId": eq(b.ID), "$or": bson.A{bson.M{"scrum.sprintId": eq(id)}, bson.M{"scrum.pastSprintIds": eq(id)}}}},
					ref{"swimlane", "swimlanes", bson.M{"boardId": eq(b.ID), "scrum.sprintId": eq(id)}},
					ref{"scrum-event", "scrumEvents", bson.M{"boardId": eq(b.ID), "sprintId": eq(id)}})
			} else {
				refs = append(refs, ref{"card", "cards", bson.M{"boardId": eq(b.ID), "$or": bson.A{bson.M{"scrum.releaseId": eq(id)}, bson.M{"scrum.releaseIds": eq(id)}}}},
					ref{"swimlane", "swimlanes", bson.M{"boardId": eq(b.ID), "scrum.releaseId": eq(id)}})
			}
			for _, rf := range refs {
				for _, d := range s.findDocs(ctx, rf.coll, rf.filter) {
					key := rf.kind + ":" + docString(d, "_id")
					if !has[key] {
						return scrumConflict()
					}
					after := final[key]
					m := after
					if scrumMetadataType(rf.kind) {
						m, _ = after["scrum"].(map[string]any)
					}
					if docString(m, field) == id || field == "sprintId" && containsString(stringsOf(m["pastSprintIds"]), id) ||
						field == "releaseId" && containsString(cardReleaseIds(m), id) {
						return scrumConflict()
					}
				}
			}
		}
	}
	// An active sprint keeps the estimate settings it started with.
	settingsDoc := board["scrum"]
	if t := final["board:"+b.ID]; t != nil {
		settingsDoc = t["scrum"]
	}
	settings := scrumSettingsOf(settingsDoc)
	sprints := map[string]map[string]any{}
	for _, sp := range s.findDocs(ctx, "scrumSprints", bson.M{"boardId": eq(b.ID), "state": "active"}) {
		sprints[docString(sp, "_id")] = sp
	}
	for _, t := range targets {
		if docString(t, "type") == "scrum-sprint" {
			sprints[docString(t, "id")] = final["scrum-sprint:"+docString(t, "id")]
		}
	}
	for _, sp := range sprints {
		start, _ := sp["startSnapshot"].(map[string]any)
		if sp == nil || sp["state"] != "active" || start == nil {
			continue
		}
		for _, p := range [][2]string{{"estimateSource", "estimateSource"}, {"estimateCustomFieldId", "estimateCustomFieldId"}, {"estimateUnit", "unit"}, {"completionPolicy", "completionPolicy"}} {
			if canonical(settings[p[0]]) != canonical(start[p[1]]) {
				return scrumConflict()
			}
		}
	}
	return nil
}
