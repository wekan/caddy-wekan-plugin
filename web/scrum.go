package web

// Scrum, as WeKan's server/scrum.js: a board's Scrum settings, its sprints,
// releases and events, the Scrum metadata of its cards, lists and swimlanes,
// and a sprint's lifecycle - planned, started with a snapshot of its cards,
// closed with another and its unfinished cards rolled over to the next
// sprint, or cancelled. Every write is a board admin's but the metadata, which
// is whoever may change that card (or list, or swimlane); every write is
// compare-and-set on the record's revision (scrum-conflict when somebody
// else's came first), and one method call is one History row
// (entityType "scrum", WeKan's {records: [{type, id, document}]}), undone and
// redone as WeKan's server/lib/scrumHistory.js applies it.
//
// The stored shapes are WeKan's: scrumSprints, scrumReleases, scrumEvents,
// a snapshot's rows in scrumSnapshotRows chunks and a rollover plan in
// scrumRolloverRows chunks, so a sprint either server started, the other
// closes. Weddy runs a close's whole rollover in the call; WeKan's background
// rollover of a very large sprint, its interrupted-History journal and its
// Scrum import recovery are not here, and their markers refuse a write as
// WeKan does.

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const scrumSnapshotChunk = 2000

var scrumRecordCollections = map[string]string{"sprint": "scrumSprints", "release": "scrumReleases", "event": "scrumEvents"}

// scrumHistoryCollections are the record types of a Scrum History row.
var scrumHistoryCollections = map[string]string{"board": "boards", "card": "cards", "list": "lists", "swimlane": "swimlanes",
	"scrum-sprint": "scrumSprints", "scrum-release": "scrumReleases", "scrum-event": "scrumEvents"}

func scrumMetadataType(t string) bool {
	return t == "board" || t == "card" || t == "list" || t == "swimlane"
}

var scrumLocks sync.Map

// scrumLock serializes one board's Scrum writes in this process; the
// revisions' compare-and-set catches another server's.
func scrumLock(boardID string) func() {
	v, _ := scrumLocks.LoadOrStore(boardID, &sync.Mutex{})
	m := v.(*sync.Mutex)
	m.Lock()
	return m.Unlock
}

func scrumConflict() error {
	return &methodError{"scrum-conflict", "Scrum data changed. Reload before saving."}
}

func scrumInvalidErr(reason string) error { return &methodError{"invalid-scrum", reason} }

func scrumNotAuthorized() error { return &methodError{"not-authorized", "not-authorized"} }

// scrumChange is one record's change within a call.
type scrumChange struct {
	Type, ID      string
	Before, After map[string]any
}

// scrumOp is one call: its writes, recorded as one History row at the end.
type scrumOp struct {
	s       *Server
	ctx     context.Context
	u       *User
	boardID string
	changes []scrumChange
}

func (op *scrumOp) record(kind string, before, after map[string]any) {
	id := docString(after, "_id")
	if after == nil {
		id = docString(before, "_id")
	}
	op.changes = append(op.changes, scrumChange{Type: kind, ID: id, Before: before, After: after})
}

// scrumHistoryDocument is WeKan's historyDocument: what History keeps of a
// record - a card's, list's, swimlane's or board's Scrum metadata, or a
// sprint, release or event without its revision, its writer and its
// rollover's progress.
func scrumHistoryDocument(kind string, doc map[string]any) any {
	if doc == nil {
		return nil
	}
	doc, _ = plain(doc).(map[string]any)
	if scrumMetadataType(kind) {
		boardID := doc["boardId"]
		if kind == "board" {
			boardID = doc["_id"]
		}
		scrum, _ := doc["scrum"].(map[string]any)
		if scrum == nil {
			scrum = map[string]any{}
		}
		return map[string]any{"_id": doc["_id"], "boardId": boardID, "scrum": scrum}
	}
	out := map[string]any{}
	for k, v := range doc {
		switch k {
		case "revision", "updatedAt", "updatedBy", "rolloverPending", "rolloverTotal", "rolloverDone", "rolloverError", "rolloverLease", "incarnation":
		default:
			out[k] = v
		}
	}
	return out
}

// flush is recordBatch: the call's records, each its first before and its
// last after, the unchanged left out, as one row.
func (op *scrumOp) flush() {
	if op.u == nil || len(op.changes) == 0 {
		return
	}
	type rec struct {
		kind, id      string
		before, after any
		source        map[string]any
	}
	byKey := map[string]*rec{}
	keys := []string{}
	for _, c := range op.changes {
		key := c.Type + ":" + c.ID
		if r, ok := byKey[key]; ok {
			r.after = scrumHistoryDocument(c.Type, c.After)
			r.source = c.After
			continue
		}
		byKey[key] = &rec{c.Type, c.ID, scrumHistoryDocument(c.Type, c.Before), scrumHistoryDocument(c.Type, c.After), c.After}
		if c.After == nil {
			byKey[key].source = c.Before
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var records []*rec
	for _, k := range keys {
		if r := byKey[k]; canonical(r.before) != canonical(r.after) {
			records = append(records, r)
		}
	}
	if len(records) == 0 {
		return
	}
	side := func(after bool) map[string]any {
		list := bson.A{}
		for _, r := range records {
			d := r.before
			if after {
				d = r.after
			}
			list = append(list, map[string]any{"type": r.kind, "id": r.id, "document": d})
		}
		return map[string]any{"records": list}
	}
	c := historyChange{BoardID: op.boardID, EntityType: "scrum", EntityID: op.boardID, Group: "scrum", ChangeType: "edited",
		PreviousContent: side(false), NewContent: side(true), UserID: op.u.ID}
	if len(records) == 1 {
		r := records[0]
		c.EntityID = r.id
		switch {
		case r.before == nil:
			c.ChangeType = "added"
		case r.after == nil:
			c.ChangeType = "removed"
		}
		switch r.kind {
		case "card":
			c.CardID, c.ListID, c.SwimlaneID = r.id, r.source["listId"], r.source["swimlaneId"]
		case "list":
			c.ListID = r.id
		case "swimlane":
			c.SwimlaneID = r.id
		}
	}
	s := op.s
	s.recordHistory(op.ctx, c)
}

// scrumBoard is boardFor: the board, if u may see it, and a board admin's
// when admin is asked; its raw document carries its Scrum settings.
func (s *Server) scrumBoard(ctx context.Context, u *User, boardID string, admin bool) (*Board, bson.M, error) {
	if boardID == "" || len(boardID) > 200 {
		return nil, nil, scrumInvalidErr("Invalid board identifier")
	}
	b, err := s.boardFor(ctx, boardID, u)
	if err != nil {
		return nil, nil, scrumNotAuthorized()
	}
	if admin && !b.AdminOf(u.ID) {
		return nil, nil, scrumNotAuthorized()
	}
	var raw bson.M
	if s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(b.ID)}).Decode(&raw) != nil {
		return nil, nil, scrumNotAuthorized()
	}
	return b, raw, nil
}

// scrumExpect is expect(): the stored revision must be the caller's.
func scrumExpect(doc map[string]any, expected *float64, field string) error {
	if expected == nil {
		return nil
	}
	if *expected < 0 || *expected != float64(int64(*expected)) || *expected > 9007199254740991 {
		return scrumInvalidErr("Invalid revision")
	}
	if revisionOf(doc[field]) != *expected {
		return scrumConflict()
	}
	return nil
}

func revisionOf(v any) float64 {
	f := numberOf(v)
	if f != f {
		return 0
	}
	return f
}

// scrumRevisionSelector is WeKan's: the stored revision, or none stored.
func scrumRevisionSelector(doc map[string]any) bson.M {
	if v, ok := doc["scrumRevision"]; ok {
		return bson.M{"scrumRevision": v}
	}
	return bson.M{"scrumRevision": bson.M{"$exists": false}}
}

// scrumPending is pending(): an unfinished Scrum import, History operation
// or rollover refuses every other Scrum write.
func (s *Server) scrumPending(ctx context.Context, boardID string) error {
	if err := s.scrumImportPending(ctx, boardID); err != nil {
		return err
	}
	if s.db.Collection("scrumHistoryPending").FindOne(ctx, bson.M{"_id": eq(boardID)}).Err() == nil {
		return &methodError{"scrum-history-pending", "Retry the interrupted Scrum History operation first."}
	}
	if s.db.Collection("scrumSprints").FindOne(ctx, bson.M{"boardId": eq(boardID), "$or": bson.A{bson.M{"rolloverPending": true}, bson.M{"rolloverPending.0": bson.M{"$exists": true}}}}).Err() == nil {
		return &methodError{"scrum-rollover-pending", "Finish the pending sprint rollover first"}
	}
	return nil
}

func (s *Server) scrumImportPending(ctx context.Context, boardID string) error {
	if s.db.Collection("scrumImportPending").FindOne(ctx, bson.M{"_id": eq(boardID)}).Err() == nil ||
		s.db.Collection("scrumSprints").FindOne(ctx, bson.M{"boardId": eq(boardID), "scrumImportPending": true}).Err() == nil {
		return &methodError{"scrum-import-pending", "The Scrum import is incomplete. Scrum edits and report exports are unavailable."}
	}
	return nil
}

// assignedOnlyOn: u sees only the cards assigned to them on b.
func assignedOnlyOn(b *Board, u *User) bool {
	m := b.member(u.ID)
	return m != nil && m.AssignedOnly()
}

// scrumCardFilter is cardSelector(): the board's cards u may see.
func scrumCardFilter(b *Board, u *User) bson.M {
	f := bson.M{"boardId": eq(b.ID), "deletedAt": nil}
	if assignedOnlyOn(b, u) {
		f["assignees"] = eq(u.ID)
	}
	return f
}

func (s *Server) findDocs(ctx context.Context, coll string, filter bson.M, opts ...options.Lister[options.FindOptions]) []map[string]any {
	out := []map[string]any{}
	cur, err := s.db.Collection(coll).Find(ctx, filter, opts...)
	if err != nil {
		return out
	}
	var docs []bson.M
	_ = cur.All(ctx, &docs)
	for _, d := range docs {
		out = append(out, plain(d).(map[string]any))
	}
	return out
}

func (s *Server) findDoc(ctx context.Context, coll string, filter bson.M) map[string]any {
	var d bson.M
	if s.db.Collection(coll).FindOne(ctx, filter).Decode(&d) != nil {
		return nil
	}
	return plain(d).(map[string]any)
}

// scrumReferences is references(): a sprint, releases and past sprints of
// this board.
func (s *Server) scrumReferences(ctx context.Context, boardID string, meta map[string]any) error {
	bad := scrumInvalidErr("Reference does not belong to this board")
	if id, _ := meta["sprintId"].(string); id != "" && s.findDoc(ctx, "scrumSprints", bson.M{"_id": eq(id), "boardId": eq(boardID)}) == nil {
		return bad
	}
	for _, id := range cardReleaseIds(meta) {
		if s.findDoc(ctx, "scrumReleases", bson.M{"_id": eq(id), "boardId": eq(boardID)}) == nil {
			return bad
		}
	}
	for _, id := range stringsOf(meta["pastSprintIds"]) {
		if s.findDoc(ctx, "scrumSprints", bson.M{"_id": eq(id), "boardId": eq(boardID)}) == nil {
			return scrumInvalidErr("Historical sprint does not belong to this board")
		}
	}
	return nil
}

// ensureScrumSettings is ensureSettings(): a custom-field estimate names a
// numeric field of the board, and the accountabilities are active members.
func (s *Server) ensureScrumSettings(ctx context.Context, b *Board, settings map[string]any) error {
	if settings["estimateSource"] == "customField" {
		id, _ := settings["estimateCustomFieldId"].(string)
		if id == "" || s.findDoc(ctx, "customFields", bson.M{"_id": eq(id), "boardIds": eq(b.ID), "type": "number"}) == nil {
			return scrumInvalidErr("Select a numeric field on this board")
		}
	}
	ids := append([]string{}, stringsOf(settings["developerIds"])...)
	for _, k := range []string{"productOwnerId", "scrumMasterId"} {
		if id, _ := settings[k].(string); id != "" {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		if !b.MemberOf(id) { // not a write: whether a Scrum role names a member of the board
			return scrumInvalidErr("Scrum accountabilities must reference active board members")
		}
	}
	return nil
}

// scrumArg is argument i as JSON, and whether it was given.
func scrumArg(args []json.RawMessage, i int) (any, bool) {
	if i >= len(args) {
		return nil, false
	}
	var v any
	if json.Unmarshal(args[i], &v) != nil {
		return nil, false
	}
	return v, true
}

func scrumObjectArg(args []json.RawMessage, i int) (map[string]any, error) {
	v, _ := scrumArg(args, i)
	m, ok := v.(map[string]any)
	if !ok {
		return nil, &methodError{"Match.Error", "Expected object"}
	}
	return m, nil
}

// scrumRevisionArg is an expected revision: a number, or null (or left out
// when optional).
func scrumRevisionArg(args []json.RawMessage, i int, optional bool) (*float64, error) {
	v, given := scrumArg(args, i)
	if !given && optional || given && v == nil && optional {
		return nil, nil
	}
	f, ok := v.(float64)
	if !ok {
		return nil, &methodError{"Match.Error", "Expected number"}
	}
	return &f, nil
}

// scrumIDArg is a string, or null.
func scrumIDArg(args []json.RawMessage, i int) (string, bool, error) {
	v, given := scrumArg(args, i)
	if !given {
		return "", false, &methodError{"Match.Error", "Expected string or null"}
	}
	if v == nil {
		return "", false, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", false, &methodError{"Match.Error", "Expected string or null"}
	}
	return s, true, nil
}

// scrumCall runs one Scrum write: the board's lock, then the call's History
// row.
func (s *Server) scrumCall(r *http.Request, u *User, boardID string, fn func(op *scrumOp) (any, error)) (any, error) {
	unlock := scrumLock(boardID)
	defer unlock()
	op := &scrumOp{s: s, ctx: r.Context(), u: u, boardID: boardID}
	out, err := fn(op)
	op.flush()
	if len(op.changes) > 0 {
		s.hub.publishFrom(boardID, "board-changed", r.Header.Get("X-Weddy-Page"))
	}
	return out, err
}

func (s *Server) scrumMethods() map[string]methodFunc {
	m := map[string]methodFunc{
		"scrum.getBoardData": func(r *http.Request, u *User, args []json.RawMessage) (any, error) {
			boardID, err := stringArg(args, 0)
			if err != nil {
				return nil, err
			}
			return s.scrumBoardData(r.Context(), u, boardID)
		},
		"scrum.configure":       s.methodScrumConfigure,
		"scrum.getDailyHistory": s.methodScrumDailyHistory,
		"scrum.getScopeHistory": s.methodScrumScopeHistory,
		"scrum.importIntoBoard": s.methodScrumImportIntoBoard,
		"exportDownloadToken":   s.methodExportDownloadToken,
		"scrum.startSprint":     s.methodScrumStartSprint,
		"scrum.closeSprint":     s.methodScrumCloseSprint,
		"scrum.cancelSprint": func(r *http.Request, u *User, args []json.RawMessage) (any, error) {
			boardID, err := stringArg(args, 0)
			if err != nil {
				return nil, err
			}
			sprintID, err := stringArg(args, 1)
			if err != nil {
				return nil, err
			}
			rev, err := scrumRevisionArg(args, 2, false)
			if err != nil {
				return nil, err
			}
			reason, err := stringArg(args, 3)
			if err != nil {
				return nil, err
			}
			if jsTrim(reason) == "" || utf16Len(reason) > 10000 {
				return nil, scrumInvalidErr("Cancellation reason is required")
			}
			return s.scrumCall(r, u, boardID, func(op *scrumOp) (any, error) {
				_, sprint, err := s.scrumSprintFor(op, boardID, sprintID, rev)
				if err != nil {
					return nil, err
				}
				if err := s.scrumPending(op.ctx, boardID); err != nil {
					return nil, err
				}
				if st := sprint["state"]; st != "planned" && st != "active" {
					return nil, scrumInvalidErr("Sprint is already finished")
				}
				return s.updateSprint(op, sprint, map[string]any{"state": "cancelled", "cancelledAt": time.Now().UTC().Truncate(time.Millisecond), "cancellationReason": reason})
			})
		},
	}
	for name, kind := range map[string]string{"scrum.saveSprint": "sprint", "scrum.saveRelease": "release", "scrum.saveEvent": "event"} {
		kind := kind
		m[name] = func(r *http.Request, u *User, args []json.RawMessage) (any, error) {
			boardID, err := stringArg(args, 0)
			if err != nil {
				return nil, err
			}
			id, hasID, err := scrumIDArg(args, 1)
			if err != nil {
				return nil, err
			}
			changes, err := scrumObjectArg(args, 2)
			if err != nil {
				return nil, err
			}
			rev, err := scrumRevisionArg(args, 3, true)
			if err != nil {
				return nil, err
			}
			return s.scrumCall(r, u, boardID, func(op *scrumOp) (any, error) {
				return s.saveScrumRecord(op, kind, id, hasID, changes, rev)
			})
		}
	}
	for name, kind := range map[string]string{"scrum.updateCard": "card", "scrum.updateList": "list", "scrum.updateSwimlane": "swimlane"} {
		kind := kind
		m[name] = func(r *http.Request, u *User, args []json.RawMessage) (any, error) {
			boardID, err := stringArg(args, 0)
			if err != nil {
				return nil, err
			}
			id, err := stringArg(args, 1)
			if err != nil {
				return nil, err
			}
			meta, err := scrumObjectArg(args, 2)
			if err != nil {
				return nil, err
			}
			rev, err := scrumRevisionArg(args, 3, true)
			if err != nil {
				return nil, err
			}
			return s.scrumCall(r, u, boardID, func(op *scrumOp) (any, error) {
				return s.updateScrumMetadata(op, kind, id, meta, rev)
			})
		}
	}
	return m
}

// methodScrumConfigure is scrum.configure(boardId, changes, expectedRevision).
func (s *Server) methodScrumConfigure(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	boardID, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	changes, err := scrumObjectArg(args, 1)
	if err != nil {
		return nil, err
	}
	rev, err := scrumRevisionArg(args, 2, true)
	if err != nil {
		return nil, err
	}
	return s.scrumCall(r, u, boardID, func(op *scrumOp) (any, error) {
		ctx := op.ctx
		b, before, err := s.scrumBoard(ctx, u, boardID, true)
		if err != nil {
			return nil, err
		}
		if err := s.scrumPending(ctx, boardID); err != nil {
			return nil, err
		}
		if err := scrumExpect(before, rev, "scrumRevision"); err != nil {
			return nil, err
		}
		var clean map[string]any
		if err := scrumValidate(func() { clean = normalizeScrumSettings(changes) }); err != nil {
			return nil, err
		}
		previous := scrumSettingsOf(before["scrum"])
		scrum := scrumSettingsOf(before["scrum"])
		for k, v := range clean {
			scrum[k] = v
		}
		vis := map[string]any{}
		if old, ok := plain(before["scrum"]).(map[string]any); ok {
			if m, ok := old["visibility"].(map[string]any); ok {
				for k, v := range m {
					vis[k] = v
				}
			}
		}
		if m, ok := scrum["visibility"].(map[string]any); ok {
			for k, v := range m {
				vis[k] = v
			}
		}
		scrum["visibility"] = vis
		if err := s.ensureScrumSettings(ctx, b, scrum); err != nil {
			return nil, err
		}
		for _, k := range []string{"estimateSource", "estimateCustomFieldId", "estimateUnit", "completionPolicy"} {
			if canonical(scrum[k]) != canonical(previous[k]) {
				if s.findDoc(ctx, "scrumSprints", bson.M{"boardId": eq(boardID), "state": "active"}) != nil {
					return nil, scrumInvalidErr("Cannot change estimate units or completion policy during an active sprint")
				}
				break
			}
		}
		next := int32(revisionOf(before["scrumRevision"]) + 1)
		filter := scrumRevisionSelector(before)
		filter["_id"] = eq(boardID)
		res, err := s.db.Collection("boards").UpdateOne(ctx, filter, bson.M{"$set": bson.M{"scrum": scrum, "scrumRevision": next}})
		if err != nil {
			return nil, err
		}
		if res.MatchedCount == 0 {
			return nil, scrumConflict()
		}
		after := map[string]any{}
		for k, v := range plain(before).(map[string]any) {
			after[k] = v
		}
		after["scrum"], after["scrumRevision"] = scrum, next
		op.record("board", plain(before).(map[string]any), after)
		return map[string]any{"settings": scrum, "settingsRevision": next}, nil
	})
}

// saveScrumRecord is saveRecord(): a sprint, release or event, new (no id)
// or a planned one changed.
func (s *Server) saveScrumRecord(op *scrumOp, kind, id string, hasID bool, changes map[string]any, rev *float64) (any, error) {
	ctx, u, boardID := op.ctx, op.u, op.boardID
	b, _, err := s.scrumBoard(ctx, u, boardID, true)
	if err != nil {
		return nil, err
	}
	if err := s.scrumPending(ctx, boardID); err != nil {
		return nil, err
	}
	coll := scrumRecordCollections[kind]
	var before map[string]any
	if hasID {
		if before = s.findDoc(ctx, coll, bson.M{"_id": eq(id), "boardId": eq(boardID)}); before == nil {
			return nil, &methodError{"not-found", "not-found"}
		}
		if rev == nil {
			return nil, scrumConflict()
		}
		if err := scrumExpect(before, rev, "revision"); err != nil {
			return nil, err
		}
		if kind == "sprint" && before["state"] != "planned" {
			return nil, scrumInvalidErr("Only planned sprints can be edited")
		}
	}
	var clean map[string]any
	if err := scrumValidate(func() { clean = normalizeScrumRecord(kind, changes) }); err != nil {
		return nil, err
	}
	after := map[string]any{}
	for k, v := range before {
		after[k] = v
	}
	for k, v := range clean {
		after[k] = v
	}
	after["boardId"] = boardID
	if name, _ := after["name"].(string); kind != "event" && name == "" {
		return nil, scrumInvalidErr("Name is required")
	}
	if startsAfterEnd(after["plannedStart"], after["plannedEnd"]) {
		return nil, scrumInvalidErr("Start must precede end")
	}
	if kind == "event" {
		if docString(after, "sprintId") == "" || docString(after, "kind") == "" || after["startsAt"] == nil {
			return nil, scrumInvalidErr("Sprint, event kind and date are required")
		}
		if err := s.scrumReferences(ctx, boardID, after); err != nil {
			return nil, err
		}
		for _, cid := range stringsOf(after["followUpCardIds"]) {
			f := scrumCardFilter(b, u)
			f["_id"] = eq(cid)
			if s.findDoc(ctx, "cards", f) == nil {
				return nil, scrumInvalidErr("Follow-up card does not belong to this board")
			}
		}
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	after["revision"] = int32(revisionOf(before["revision"]) + 1)
	after["updatedAt"], after["updatedBy"] = now, u.ID
	if before == nil {
		after["createdAt"], after["createdBy"] = now, u.ID
		if kind != "event" && docString(after, "state") == "" {
			after["state"] = "planned"
		}
		// One lifetime of this record: History tells it from one deleted
		// and created again under the same _id.
		after["incarnation"] = randomID()
		after["_id"] = randomID()
		if _, err := s.db.Collection(coll).InsertOne(ctx, after); err != nil {
			return nil, err
		}
	} else {
		set := bson.M{}
		for k, v := range after {
			if k != "_id" {
				set[k] = v
			}
		}
		res, err := s.db.Collection(coll).UpdateOne(ctx, bson.M{"_id": eq(id), "boardId": eq(boardID), "revision": before["revision"]}, bson.M{"$set": set})
		if err != nil {
			return nil, err
		}
		if res.MatchedCount == 0 {
			return nil, scrumConflict()
		}
	}
	op.record("scrum-"+kind, before, after)
	return after, nil
}

// updateScrumMetadata is updateMetadata(): a card's, list's or swimlane's
// Scrum fields, by whoever may change it.
func (s *Server) updateScrumMetadata(op *scrumOp, kind, id string, meta map[string]any, rev *float64) (any, error) {
	ctx, u, boardID := op.ctx, op.u, op.boardID
	b, _, err := s.scrumBoard(ctx, u, boardID, false)
	if err != nil {
		return nil, err
	}
	if err := s.scrumPending(ctx, boardID); err != nil {
		return nil, err
	}
	coll := map[string]string{"card": "cards", "list": "lists", "swimlane": "swimlanes"}[kind]
	before := s.findDoc(ctx, coll, bson.M{"_id": eq(id), "boardId": eq(boardID)})
	if before == nil {
		return nil, &methodError{"not-found", "not-found"}
	}
	allowed := false
	if kind == "card" {
		var c Card
		_ = s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&c)
		allowed = b.WritableIn(ctx, s, u, "write", &c)
	} else {
		allowed = b.Writable(u, "write", nil)
	}
	if !allowed {
		return nil, scrumNotAuthorized()
	}
	if err := scrumExpect(before, rev, "scrumRevision"); err != nil {
		return nil, err
	}
	var clean, scrum map[string]any
	old, _ := before["scrum"].(map[string]any)
	if old == nil {
		old = map[string]any{}
	}
	if err := scrumValidate(func() {
		clean = normalizeScrumMetadata(kind, meta)
		if kind == "card" {
			scrum = applyCardReleaseChange(old, clean)
		} else {
			scrum = map[string]any{}
			for k, v := range old {
				scrum[k] = v
			}
			for k, v := range clean {
				scrum[k] = v
			}
		}
	}); err != nil {
		return nil, err
	}
	if err := s.scrumReferences(ctx, boardID, scrum); err != nil {
		return nil, err
	}
	if kind == "card" {
		// Historical membership is the lifecycle's record, never a form's.
		if past, ok := clean["pastSprintIds"]; ok && canonical(past) != canonical(orEmptyList(old["pastSprintIds"])) {
			return nil, scrumInvalidErr("Historical sprint membership is read-only")
		}
		if sid := docString(scrum, "sprintId"); sid != "" {
			target := s.findDoc(ctx, "scrumSprints", bson.M{"_id": eq(sid)})
			if st := target["state"]; st != "planned" && st != "active" && sid != docString(old, "sprintId") {
				return nil, scrumInvalidErr("Cannot assign work to a finished sprint")
			}
		}
		if prev := docString(old, "sprintId"); prev != "" && docString(scrum, "sprintId") != prev {
			scrum["pastSprintIds"] = appendUnique(stringsOf(old["pastSprintIds"]), prev)
		}
	}
	next := int32(revisionOf(before["scrumRevision"]) + 1)
	filter := scrumRevisionSelector(before)
	filter["_id"], filter["boardId"] = eq(id), eq(boardID)
	res, err := s.db.Collection(coll).UpdateOne(ctx, filter, bson.M{"$set": bson.M{"scrum": scrum, "scrumRevision": next}})
	if err != nil {
		return nil, err
	}
	if res.MatchedCount == 0 {
		return nil, scrumConflict()
	}
	after := map[string]any{}
	for k, v := range before {
		after[k] = v
	}
	after["scrum"], after["scrumRevision"] = scrum, next
	op.record(kind, before, after)
	return map[string]any{"_id": id, "scrum": scrum, "scrumRevision": next}, nil
}

func orEmptyList(v any) any {
	if v == nil {
		return []any{}
	}
	return v
}

func appendUnique(list []string, v string) []any {
	out := []any{}
	seen := map[string]bool{}
	for _, x := range append(list, v) {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// scrumSprintFor is sprintFor(): a sprint of the board, by a board admin, at
// the expected revision.
func (s *Server) scrumSprintFor(op *scrumOp, boardID, sprintID string, rev *float64) (bson.M, map[string]any, error) {
	_, board, err := s.scrumBoard(op.ctx, op.u, boardID, true)
	if err != nil {
		return nil, nil, err
	}
	sprint := s.findDoc(op.ctx, "scrumSprints", bson.M{"_id": eq(sprintID), "boardId": eq(boardID)})
	if sprint == nil {
		return nil, nil, &methodError{"not-found", "not-found"}
	}
	if err := scrumExpect(sprint, rev, "revision"); err != nil {
		return nil, nil, err
	}
	return board, sprint, nil
}

// updateSprint is updateSprint(): the sprint's lifecycle fields, its
// revision one up.
func (s *Server) updateSprint(op *scrumOp, before map[string]any, fields map[string]any) (map[string]any, error) {
	after := map[string]any{}
	for k, v := range before {
		after[k] = v
	}
	for k, v := range fields {
		after[k] = v
	}
	after["revision"] = int32(revisionOf(before["revision"]) + 1)
	after["updatedAt"], after["updatedBy"] = time.Now().UTC().Truncate(time.Millisecond), op.u.ID
	set := bson.M{}
	for k, v := range after {
		if k != "_id" {
			set[k] = v
		}
	}
	res, err := s.db.Collection("scrumSprints").UpdateOne(op.ctx, bson.M{"_id": eq(docString(before, "_id")), "boardId": eq(docString(before, "boardId")), "revision": before["revision"]}, bson.M{"$set": set})
	if err != nil {
		return nil, err
	}
	if res.MatchedCount == 0 {
		return nil, scrumConflict()
	}
	op.record("scrum-sprint", before, after)
	return after, nil
}

// scrumSnapshotInputs is loadScrumSnapshotInputs(): the sprint's cards and
// the board's lists.
func (s *Server) scrumSnapshotInputs(ctx context.Context, boardID, sprintID string, includeArchived bool) ([]map[string]any, []map[string]any) {
	f := bson.M{"boardId": eq(boardID), "scrum.sprintId": eq(sprintID)}
	if !includeArchived {
		f["archived"] = bson.M{"$ne": true}
	}
	cards := s.findDocs(ctx, "cards", f, options.Find().SetProjection(bson.M{"listId": 1, "swimlaneId": 1, "archived": 1, "dueComplete": 1, "poker.estimation": 1,
		"customFields": 1, "scrum": 1, "scrumRevision": 1, "boardId": 1}))
	lists := s.findDocs(ctx, "lists", bson.M{"boardId": eq(boardID)}, options.Find().SetProjection(bson.M{"scrum": 1}))
	return cards, lists
}

// storeScrumSnapshot is storeSnapshot(): the rows in immutable chunks, named
// by sprint, kind, time and number, and the header the sprint keeps.
func (s *Server) storeScrumSnapshot(ctx context.Context, boardID, sprintID, kind string, snapshot map[string]any) (map[string]any, error) {
	rows, _ := snapshot["cards"].([]any)
	at := snapshot["at"].(time.Time)
	chunks := 0
	for i := 0; i*scrumSnapshotChunk < len(rows); i++ {
		end := min((i+1)*scrumSnapshotChunk, len(rows))
		id := sprintID + ":" + kind + ":" + jsInt(at.UnixMilli()) + ":" + jsInt(int64(i))
		doc := bson.M{"_id": id, "boardId": boardID, "sprintId": sprintID, "kind": kind, "at": at, "chunk": int32(i), "rows": rows[i*scrumSnapshotChunk : end]} // inserted, not a filter
		if _, err := s.db.Collection("scrumSnapshotRows").InsertOne(ctx, doc); err != nil {
			// A retried snapshot finds its own chunk; any other is refused.
			existing := s.findDoc(ctx, "scrumSnapshotRows", bson.M{"_id": eq(id)})
			if !mongo.IsDuplicateKeyError(err) || canonical(existing) != canonical(doc) {
				return nil, err
			}
		}
		chunks++
	}
	header := map[string]any{}
	for k, v := range snapshot {
		if k != "cards" {
			header[k] = v
		}
	}
	header["stored"], header["rowCount"], header["chunks"] = "rows", int32(len(rows)), int32(chunks)
	return header, nil
}

func jsInt(n int64) string { return jsNumber(float64(n)) }

// scrumSnapshotRows is snapshotRows(): a snapshot's rows, inline in an older
// sprint, from its chunks otherwise.
func (s *Server) scrumSnapshotRows(ctx context.Context, sprint map[string]any, key string) ([]map[string]any, error) {
	snap, _ := sprint[key].(map[string]any)
	if snap == nil {
		return nil, nil
	}
	if snap["stored"] != "rows" {
		return snapshotCards(snap), nil
	}
	kind := map[string]string{"startSnapshot": "start", "closeSnapshot": "close", "daily": "daily"}[key]
	at, _ := snap["at"].(time.Time)
	chunks := s.findDocs(ctx, "scrumSnapshotRows", bson.M{"sprintId": eq(docString(sprint, "_id")), "kind": kind, "at": at},
		options.Find().SetSort(bson.D{{Key: "chunk", Value: 1}}))
	missing := &methodError{"scrum-snapshot-missing", "A sprint snapshot is incomplete."}
	if float64(len(chunks)) != numberOf(snap["chunks"]) {
		return nil, missing
	}
	out := []map[string]any{}
	for i, c := range chunks {
		if numberOf(c["chunk"]) != float64(i) {
			return nil, missing
		}
		out = append(out, snapshotCards(map[string]any{"cards": c["rows"]})...)
	}
	if float64(len(out)) != numberOf(snap["rowCount"]) {
		return nil, missing
	}
	return out, nil
}

func rowsAsAny(rows []map[string]any) []any {
	out := make([]any, len(rows))
	for i, r := range rows {
		out[i] = r
	}
	return out
}

// methodScrumStartSprint is scrum.startSprint(boardId, sprintId, revision).
func (s *Server) methodScrumStartSprint(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	boardID, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	sprintID, err := stringArg(args, 1)
	if err != nil {
		return nil, err
	}
	rev, err := scrumRevisionArg(args, 2, false)
	if err != nil {
		return nil, err
	}
	return s.scrumCall(r, u, boardID, func(op *scrumOp) (any, error) {
		ctx := op.ctx
		board, sprint, err := s.scrumSprintFor(op, boardID, sprintID, rev)
		if err != nil {
			return nil, err
		}
		if err := s.scrumPending(ctx, boardID); err != nil {
			return nil, err
		}
		if sprint["state"] != "planned" {
			return nil, scrumInvalidErr("Only a planned sprint can start")
		}
		if sprint["plannedStart"] == nil || sprint["plannedEnd"] == nil || startsAfterEnd(sprint["plannedStart"], sprint["plannedEnd"]) {
			return nil, scrumInvalidErr("Valid planned start and end dates are required")
		}
		settings := scrumSettingsOf(board["scrum"])
		b, _ := s.boardFor(ctx, boardID, u)
		if err := s.ensureScrumSettings(ctx, b, settings); err != nil {
			return nil, err
		}
		if sprint["capacity"] != nil && sprint["capacityUnit"] != settings["estimateUnit"] {
			return nil, scrumInvalidErr("Capacity and estimates must use the same unit")
		}
		cards, lists := s.scrumSnapshotInputs(ctx, boardID, sprintID, false)
		startedAt := time.Now().UTC().Truncate(time.Millisecond)
		snapshot := sprintSnapshot(cards, settings, lists, startedAt)
		header, err := s.storeScrumSnapshot(ctx, boardID, sprintID, "start", snapshot)
		if err != nil {
			return nil, err
		}
		return s.updateSprint(op, sprint, map[string]any{"state": "active", "startedAt": startedAt, "startSnapshot": header,
			"reportTotals": scrumReportTotals(snapshotCards(snapshot), nil)})
	})
}

// methodScrumCloseSprint is scrum.closeSprint(boardId, sprintId, revision,
// rolloverSprintId): the close snapshot, then every unfinished card to the
// rollover sprint (or the product backlog) and every card's past sprints
// naming this one - one History row with the close.
func (s *Server) methodScrumCloseSprint(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	boardID, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	sprintID, err := stringArg(args, 1)
	if err != nil {
		return nil, err
	}
	rev, err := scrumRevisionArg(args, 2, false)
	if err != nil {
		return nil, err
	}
	rollover := ""
	if v, given := scrumArg(args, 3); given && v != nil {
		if rollover, err = stringArg(args, 3); err != nil {
			return nil, err
		}
	}
	return s.scrumCall(r, u, boardID, func(op *scrumOp) (any, error) {
		ctx := op.ctx
		_, board, err := s.scrumBoard(ctx, u, boardID, true)
		if err != nil {
			return nil, err
		}
		sprint := s.findDoc(ctx, "scrumSprints", bson.M{"_id": eq(sprintID), "boardId": eq(boardID)})
		if sprint == nil {
			return nil, &methodError{"not-found", "not-found"}
		}
		if err := s.scrumImportPending(ctx, boardID); err != nil {
			return nil, err
		}
		if s.db.Collection("scrumHistoryPending").FindOne(ctx, bson.M{"_id": eq(boardID)}).Err() == nil {
			return nil, &methodError{"scrum-history-pending", "Retry the interrupted Scrum History operation first."}
		}
		// A retry of the same close finishes its rollover, if it stopped.
		if sprint["state"] == "closed" && rev != nil && revisionOf(sprint["closedFromRevision"]) == *rev && docString(sprint, "rolloverSprintId") == rollover {
			if sprint["rolloverPending"] != nil {
				return s.runScrumRollover(op, sprint)
			}
			return sprint, nil
		}
		if err := scrumExpect(sprint, rev, "revision"); err != nil {
			return nil, err
		}
		if err := s.scrumPending(ctx, boardID); err != nil {
			return nil, err
		}
		if sprint["state"] != "active" {
			return nil, scrumInvalidErr("Only an active sprint can close")
		}
		if rollover != "" && (rollover == sprintID || s.findDoc(ctx, "scrumSprints", bson.M{"_id": eq(rollover), "boardId": eq(boardID), "state": "planned"}) == nil) {
			return nil, scrumInvalidErr("Rollover must target a planned sprint on this board")
		}
		settings := scrumSettingsOf(board["scrum"])
		cards, lists := s.scrumSnapshotInputs(ctx, boardID, sprintID, true)
		completedAt := time.Now().UTC().Truncate(time.Millisecond)
		full := sprintSnapshot(cards, settings, lists, completedAt)
		done := map[string]bool{}
		for _, row := range snapshotCards(full) {
			done[docString(row, "cardId")] = row["done"] == true
		}
		header, err := s.storeScrumSnapshot(ctx, boardID, sprintID, "close", full)
		if err != nil {
			return nil, err
		}
		start, err := s.scrumSnapshotRows(ctx, sprint, "startSnapshot")
		if err != nil {
			return nil, err
		}
		totals := scrumReportTotals(start, snapshotCards(full))
		// The plan in WeKan's chunks, before the sprint is marked.
		_, _ = s.db.Collection("scrumRolloverRows").DeleteMany(ctx, bson.M{"sprintId": eq(sprintID)})
		plan := []any{}
		for _, c := range cards {
			old, _ := c["scrum"].(map[string]any)
			if old == nil {
				old = map[string]any{}
			}
			next := map[string]any{}
			for k, v := range old {
				next[k] = v
			}
			var target any
			if !done[docString(c, "_id")] && c["archived"] != true && rollover != "" {
				target = rollover
			}
			next["sprintId"] = target
			next["pastSprintIds"] = appendUnique(stringsOf(old["pastSprintIds"]), sprintID)
			plan = append(plan, map[string]any{"cardId": c["_id"], "revision": storedNum(revisionOf(c["scrumRevision"])), "before": old, "after": next})
		}
		for i := 0; i*scrumSnapshotChunk < len(plan); i++ {
			end := min((i+1)*scrumSnapshotChunk, len(plan))
			if _, err := s.db.Collection("scrumRolloverRows").InsertOne(ctx, bson.M{"_id": sprintID + ":rollover:" + jsInt(int64(i)), "boardId": boardID,
				"sprintId": sprintID, "chunk": int32(i), "rows": plan[i*scrumSnapshotChunk : end]}); err != nil {
				return nil, err
			}
		}
		var target any
		if rollover != "" {
			target = rollover
		}
		fields := map[string]any{"state": "closed", "completedAt": completedAt, "closeSnapshot": header, "reportTotals": totals,
			"closedFromRevision": sprint["revision"], "rolloverSprintId": target}
		if len(cards) > 0 {
			fields["rolloverPending"], fields["rolloverTotal"], fields["rolloverDone"] = true, int32(len(cards)), int32(0)
		}
		closed, err := s.updateSprint(op, sprint, fields)
		if err != nil || closed["rolloverPending"] == nil {
			return closed, err
		}
		return s.runScrumRollover(op, closed)
	})
}

// runScrumRollover is rolloverStep() to the end: each chunk's cards moved,
// never a card edited or moved since the close, then the mark cleared.
func (s *Server) runScrumRollover(op *scrumOp, sprint map[string]any) (map[string]any, error) {
	ctx := op.ctx
	sprintID, boardID := docString(sprint, "_id"), docString(sprint, "boardId")
	if list, ok := sprint["rolloverPending"].([]any); ok {
		// A plan kept in the sprint document before 2026-10-03.
		for _, x := range list {
			row, _ := x.(map[string]any)
			if err := s.applyScrumRolloverRow(op, boardID, row); err != nil {
				return nil, err
			}
			_, _ = s.db.Collection("scrumSprints").UpdateOne(ctx, bson.M{"_id": eq(sprintID)}, bson.M{"$pull": bson.M{"rolloverPending": bson.M{"cardId": row["cardId"]}}})
		}
	} else {
		for {
			chunk := s.findDoc(ctx, "scrumRolloverRows", bson.M{"sprintId": eq(sprintID)})
			if chunk == nil {
				break
			}
			if docString(chunk, "boardId") != boardID {
				return nil, scrumConflict()
			}
			rows, _ := chunk["rows"].([]any)
			for _, x := range rows {
				row, _ := x.(map[string]any)
				if err := s.applyScrumRolloverRow(op, boardID, row); err != nil {
					_, _ = s.db.Collection("scrumSprints").UpdateOne(ctx, bson.M{"_id": eq(sprintID)}, bson.M{"$set": bson.M{"rolloverError": err.Error()}})
					return nil, err
				}
			}
			_, _ = s.db.Collection("scrumRolloverRows").DeleteOne(ctx, bson.M{"_id": eq(docString(chunk, "_id"))})
			_, _ = s.db.Collection("scrumSprints").UpdateOne(ctx, bson.M{"_id": eq(sprintID)}, bson.M{"$inc": bson.M{"rolloverDone": int32(len(rows))}})
		}
	}
	_, _ = s.db.Collection("scrumSprints").UpdateOne(ctx, bson.M{"_id": eq(sprintID)},
		bson.M{"$unset": bson.M{"rolloverPending": "", "rolloverTotal": "", "rolloverDone": "", "rolloverError": "", "rolloverLease": ""}})
	return s.findDoc(ctx, "scrumSprints", bson.M{"_id": eq(sprintID)}), nil
}

func (s *Server) applyScrumRolloverRow(op *scrumOp, boardID string, row map[string]any) error {
	ctx := op.ctx
	card := s.findDoc(ctx, "cards", bson.M{"_id": eq(docString(row, "cardId")), "boardId": eq(boardID)})
	if card == nil {
		return scrumConflict()
	}
	current := card["scrum"]
	if current == nil {
		current = map[string]any{}
	}
	if canonical(current) == canonical(row["after"]) {
		return nil
	}
	if canonical(current) != canonical(row["before"]) || revisionOf(card["scrumRevision"]) != numberOf(row["revision"]) {
		return scrumConflict()
	}
	next := int32(numberOf(row["revision"]) + 1)
	filter := scrumRevisionSelector(card)
	filter["_id"], filter["boardId"] = eq(docString(card, "_id")), eq(boardID)
	res, err := s.db.Collection("cards").UpdateOne(ctx, filter, bson.M{"$set": bson.M{"scrum": row["after"], "scrumRevision": next}})
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return scrumConflict()
	}
	after := map[string]any{}
	for k, v := range card {
		after[k] = v
	}
	after["scrum"], after["scrumRevision"] = row["after"], next
	op.record("card", card, after)
	return nil
}

// scrumBoardData is getScrumBoardData(): what the Scrum view shows - the
// reader's cards, the board's lists, swimlanes, sprints with their reports
// (never their snapshots' rows), releases, events and numeric fields, and
// what the reader may do.
func (s *Server) scrumBoardData(ctx context.Context, u *User, boardID string) (any, error) {
	b, raw, err := s.scrumBoard(ctx, u, boardID, false)
	if err != nil {
		return nil, err
	}
	cards := s.findDocs(ctx, "cards", scrumCardFilter(b, u), options.Find().SetProjection(bson.M{"title": 1, "boardId": 1, "listId": 1, "swimlaneId": 1,
		"archived": 1, "dueComplete": 1, "scrum": 1, "scrumRevision": 1, "poker.estimation": 1, "customFields": 1, "members": 1, "assignees": 1,
		"sort": 1, "type": 1, "linkedId": 1, "parentId": 1}))
	small := options.Find().SetProjection(bson.M{"title": 1, "archived": 1, "scrum": 1, "scrumRevision": 1, "sort": 1})
	lists := s.findDocs(ctx, "lists", bson.M{"boardId": eq(boardID), "deletedAt": nil}, small)
	swimlanes := s.findDocs(ctx, "swimlanes", bson.M{"boardId": eq(boardID)}, small)
	sprints := s.findDocs(ctx, "scrumSprints", bson.M{"boardId": eq(boardID)})
	releases := s.findDocs(ctx, "scrumReleases", bson.M{"boardId": eq(boardID)})
	events := s.findDocs(ctx, "scrumEvents", bson.M{"boardId": eq(boardID)})
	fields := s.findDocs(ctx, "customFields", bson.M{"boardIds": eq(boardID), "type": "number"}, options.Find().SetProjection(bson.M{"name": 1, "type": 1}))
	visible := map[string]bool{}
	for _, c := range cards {
		visible[docString(c, "_id")] = true
	}
	restricted := assignedOnlyOn(b, u)
	for _, sp := range sprints {
		pending := sp["rolloverPending"] == true
		if list, ok := sp["rolloverPending"].([]any); ok {
			pending = len(list) > 0
			if restricted {
				pending = false
				for _, x := range list {
					if m, _ := x.(map[string]any); m != nil && visible[docString(m, "cardId")] {
						pending = true
					}
				}
			}
		} else if pending && restricted {
			ids := bson.A{}
			for id := range visible {
				ids = append(ids, id)
			}
			pending = s.findDoc(ctx, "scrumRolloverRows", bson.M{"sprintId": eq(docString(sp, "_id")), "rows.cardId": dbIn(ids)}) != nil
		}
		sp["rolloverPending"] = pending
		// The rollover's progress counts cards a restricted reader may not count.
		if restricted {
			delete(sp, "rolloverTotal")
			delete(sp, "rolloverDone")
		}
		hasRows := false
		start, end := []map[string]any{}, []map[string]any{}
		for _, key := range []string{"startSnapshot", "closeSnapshot"} {
			snap, _ := sp[key].(map[string]any)
			if snap == nil {
				continue
			}
			if snap["stored"] != "rows" || restricted {
				rows, err := s.scrumSnapshotRows(ctx, sp, key)
				if err != nil {
					return nil, err
				}
				hasRows = true
				if restricted {
					kept := []map[string]any{}
					missing, sum := 0, 0.0
					for _, row := range rows {
						if visible[docString(row, "cardId")] {
							kept = append(kept, row)
							if row["estimate"] == nil {
								missing++
							} else {
								sum += numberOf(row["estimate"])
							}
						}
					}
					rows = kept
					snap["partial"], snap["missingEstimates"], snap["totalEstimate"] = true, int32(missing), storedNum(sum)
				}
				if key == "startSnapshot" {
					start = rows
				} else {
					end = rows
				}
			}
		}
		if sp["startSnapshot"] != nil || sp["closeSnapshot"] != nil {
			totals, _ := sp["reportTotals"].(map[string]any)
			if hasRows || totals == nil {
				totals = scrumReportTotals(start, end)
			}
			sp["report"] = scrumSprintReport(sp, totals)
		} else {
			sp["report"] = nil
		}
		for _, key := range []string{"startSnapshot", "closeSnapshot"} {
			if snap, _ := sp[key].(map[string]any); snap != nil {
				h := map[string]any{}
				for k, v := range snap {
					if k != "cards" {
						h[k] = v
					}
				}
				if h["rowCount"] == nil {
					h["rowCount"] = int32(len(snapshotCards(snap)))
				}
				sp[key] = h
			}
		}
		delete(sp, "reportTotals")
		delete(sp, "rolloverLease")
	}
	for _, e := range events {
		kept := []any{}
		for _, id := range stringsOf(e["followUpCardIds"]) {
			if visible[id] {
				kept = append(kept, id)
			}
		}
		e["followUpCardIds"] = kept
	}
	importPending := false
	for _, sp := range sprints {
		importPending = importPending || sp["scrumImportPending"] == true
	}
	journal := s.db.Collection("scrumImportPending").FindOne(ctx, bson.M{"_id": eq(boardID)}).Err() == nil
	importPending = importPending || journal
	for _, c := range cards {
		var card Card
		card.ListID, card.Assignees = docString(c, "listId"), stringsOf(c["assignees"])
		// Rendering a capability is not an attempted write: no Problems row.
		c["canWrite"] = !importPending && b.Writable(u, "write", &card) && !s.listRefuses(ctx, b, card.ListID, u.ID, "write")
	}
	admin := b.AdminOf(u.ID)
	losses := any([]any{})
	if admin && raw["scrumImportLosses"] != nil {
		losses = plain(raw["scrumImportLosses"])
	}
	return map[string]any{"boardId": boardID, "settings": scrumSettingsOf(raw["scrum"]), "settingsRevision": storedNum(revisionOf(raw["scrumRevision"])),
		"sprints": sprints, "releases": releases, "events": events, "cards": cards, "lists": lists, "swimlanes": swimlanes, "customFields": fields,
		"importLosses": losses, "importPending": importPending, "canAdmin": !importPending && admin,
		"canRecoverImport": importPending && admin && journal,
		"canWrite":         !importPending && b.Writable(u, "write", nil), "historyJob": nil, "partial": restricted}, nil
}
