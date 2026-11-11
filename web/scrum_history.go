package web

// Scrum's History, as WeKan's server/lib/scrumHistory.js: a row's records
// as they are now, and the check before a row's records are put back
// (scrum_journal.go applies them) - by whoever may write each of them
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
	listRefusal := map[string]bool{}
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
			// The card as read; a list's say asked once per list.
			c := Card{ID: id, ListID: docString(doc, "listId"), Assignees: stringsOf(doc["assignees"])}
			if !b.Writable(u, "write", &c) {
				return scrumNotAuthorized()
			}
			refused, asked := listRefusal[c.ListID]
			if !asked {
				refused = s.refuseInList(ctx, b, c.ListID, u, "write", "list-locked:write")
				listRefusal[c.ListID] = refused
			}
			if refused {
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
