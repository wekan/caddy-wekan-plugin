package web

// Scrum History's durable operations, as WeKan's server/lib/scrumHistory.js
// applyScrumHistory with scrumHistoryPending, scrumHistoryOwnership,
// scrumHistoryWriteState, scrumHistoryWriteSelector, scrumHistoryRestoreWriter,
// scrumHistoryFinalizer, scrumHistoryCompletion and scrumHistoryRequest: an
// undo, redo or Restore of a Scrum row is a checkpoint (scrumHistoryPending,
// one per board) saved before its first write - the plan, what each record
// was and at which revision and lifetime - so an interrupted one resumes
// without repeating a write, a newer write by anybody else is never
// overwritten, and nothing else Scrum writes on the board meanwhile. Each
// write is fenced on the record as it was; its restore rows have ids fixed by
// the operation; the source row is flagged, a completion receipt saved and
// the checkpoint removed only once all of it is verified. A request id
// (changeHistory.undoLast(boardId, requestId)) pins the row it selected, so
// a retry after a lost reply repeats that answer and never undoes more.
// The stored shapes and digests are WeKan's, so either server finishes an
// operation the other began.

import (
	"context"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func historyPendingErr() error {
	return &methodError{"scrum-history-pending", "Retry the interrupted Scrum History operation first."}
}

// ejsonCanonical is js-bson's EJSON.serialize(v, {relaxed: false}) of a
// document as the JavaScript driver reads it: a number is a JavaScript
// number - $numberInt when it fits an int32, else $numberDouble - and a date
// {$date: {$numberLong}}.
func ejsonCanonical(v any) any {
	switch x := plain(v).(type) {
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			out[k] = ejsonCanonical(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = ejsonCanonical(e)
		}
		return out
	case time.Time:
		return map[string]any{"$date": map[string]any{"$numberLong": strconv.FormatInt(x.UnixMilli(), 10)}}
	case int32, int64, int, float64:
		f := numberOf(x)
		if f == math.Trunc(f) && f >= math.MinInt32 && f <= math.MaxInt32 && !(f == 0 && math.Signbit(f)) {
			return map[string]any{"$numberInt": strconv.FormatInt(int64(f), 10)}
		}
		if f == 0 && math.Signbit(f) {
			return map[string]any{"$numberDouble": "-0.0"}
		}
		if f == math.Trunc(f) && !math.IsInf(f, 0) {
			return map[string]any{"$numberDouble": strconv.FormatFloat(f, 'f', 1, 64)}
		}
		return map[string]any{"$numberDouble": jsNumber(f)}
	}
	return v
}

func ejsonDigest(v any) string { return sha256Hex(canonical(ejsonCanonical(v))) }

// --- the checkpoint -------------------------------------------------------

// validJournal is scrumHistorySelector's check: a checkpoint whose plan
// holds together.
func validJournal(j map[string]any) bool {
	content, _ := j["content"].(map[string]any)
	before, _ := j["before"].(map[string]any)
	cr, _ := content["records"].([]any)
	br, _ := before["records"].([]any)
	revs, ok := j["revisions"].([]any)
	if docString(j, "_id") == "" || docString(j, "rowId") == "" || docString(j, "userId") == "" || !ok || content == nil || before == nil ||
		!containsString([]string{"undo", "redo", "restore"}, docString(j, "direction")) || len(cr) != len(br) || len(cr) != len(revs) {
		return false
	}
	if op, has := j["operationId"]; has {
		if s, _ := op.(string); s == "" {
			return false
		}
	}
	if inc, has := j["incarnations"]; has {
		if list, ok := inc.([]any); !ok || len(list) != len(revs) {
			return false
		}
	}
	return true
}

// samePlan: cur is the checkpoint j names - the same plan, not resolved by
// an administrator. Compared by value: a plan's field order is the writer's.
func samePlan(cur, j map[string]any) bool {
	if cur == nil || has(cur, "resolving") {
		return false
	}
	for _, k := range []string{"_id", "rowId", "userId", "direction", "operationId", "content", "before", "revisions", "incarnations"} {
		if has(cur, k) != has(j, k) || canonical(cur[k]) != canonical(j[k]) {
			return false
		}
	}
	return true
}

// planSelector is the selector as WeKan's JavaScript holds it, for the
// completion's planHash.
func planSelector(j map[string]any) map[string]any {
	out := map[string]any{"_id": j["_id"], "rowId": j["rowId"], "userId": j["userId"], "direction": j["direction"],
		"content": map[string]any{"$eq": j["content"]}, "before": map[string]any{"$eq": j["before"]}, "revisions": map[string]any{"$eq": j["revisions"]}}
	if op, has := j["operationId"]; has {
		out["operationId"] = op
	} else {
		out["operationId"] = map[string]any{"$exists": false}
	}
	if inc, has := j["incarnations"]; has {
		out["incarnations"] = map[string]any{"$eq": inc}
	}
	return out
}

func (s *Server) scrumJournal(ctx context.Context, boardID string) map[string]any {
	return s.findDoc(ctx, "scrumHistoryPending", bson.M{"_id": eq(boardID)})
}

// pendingScrumHistoryRow is WeKan's: the board's interrupted operation, the
// caller's own and in the same direction, is the one to resume.
func (s *Server) pendingScrumHistoryRow(ctx context.Context, boardID, userID, direction string) (map[string]any, error) {
	j := s.scrumJournal(ctx, boardID)
	if j == nil {
		return nil, nil
	}
	if docString(j, "userId") != userID || docString(j, "direction") != direction {
		return nil, scrumConflict()
	}
	row := s.findDoc(ctx, "changeHistory", bson.M{"_id": eq(docString(j, "rowId"))})
	if row == nil {
		return nil, scrumConflict()
	}
	return row, nil
}

// --- write state ----------------------------------------------------------

func scrumRecordRevision(kind string, doc map[string]any) any {
	if doc == nil {
		return nil
	}
	if scrumMetadataType(kind) {
		return storedNum(revisionOf(doc["scrumRevision"]))
	}
	return storedNum(revisionOf(doc["revision"]))
}

func validRevision(v any) bool {
	f := numberOf(v)
	return f == f && f >= 0 && f == math.Trunc(f) && f <= 9007199254740991
}

// scrumWriteState is scrumHistoryWriteState: "applied", "pending", or a
// conflict - a record that is neither the plan's before nor its after, or
// either at another revision or lifetime.
func scrumWriteState(kind string, current map[string]any, before, after any, revision any, inc map[string]any) (string, bool) {
	if inc != nil {
		b, ba := inc["before"], inc["after"]
		bs, bok := b.(string)
		as, aok := ba.(string)
		if !(b == nil || bok && bs != "") || !(ba == nil || aok && as != "") {
			return "", false
		}
	}
	if before == nil && revision != nil || before != nil && !validRevision(revision) {
		return "", false
	}
	live := scrumHistoryDocument(kind, current)
	var curRev float64
	if current != nil {
		if scrumMetadataType(kind) {
			curRev = revisionOf(current["scrumRevision"])
		} else {
			curRev = revisionOf(current["revision"])
		}
		if !validRevision(curRev) {
			return "", false
		}
	}
	incOf := func() any {
		if current == nil {
			return nil
		}
		if s, ok := current["incarnation"].(string); ok {
			return s
		}
		return nil
	}
	same := func(a, b any) bool { return canonical(a) == canonical(b) }
	if same(live, after) {
		if current != nil {
			want := numberOf(revision) + 1
			if same(before, after) {
				want = numberOf(revision)
			} else if before == nil {
				want = 1
			}
			if curRev != want {
				return "", false
			}
			if inc != nil && incOf() != inc["after"] {
				return "", false
			}
		}
		return "applied", true
	}
	if !same(live, before) || current == nil && revision != nil || current != nil && curRev != numberOf(revision) {
		return "", false
	}
	if inc != nil && current != nil && incOf() != inc["before"] {
		return "", false
	}
	return "pending", true
}

// scrumWriteSelector is scrumHistoryWriteSelector: the record as it was,
// field by field - for metadata the Scrum fields and the card's placement
// and assignees, for a planning record all of it. The values are the stored
// ones, in their stored field order, which $eq on a document compares.
func (s *Server) scrumWriteSelector(ctx context.Context, kind, coll, id string) (bson.M, bool) {
	var raw bson.D
	if s.db.Collection(coll).FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&raw) != nil {
		return nil, false
	}
	stored := map[string]any{}
	for _, e := range raw {
		stored[e.Key] = e.Value
	}
	var fields []string
	if scrumMetadataType(kind) {
		fields = []string{"scrum", "scrumRevision"}
		if kind != "board" {
			fields = append(fields, "boardId")
		}
		if kind == "card" {
			fields = append(fields, "listId", "swimlaneId", "assignees")
		}
	} else {
		for _, e := range raw {
			if e.Key != "_id" {
				fields = append(fields, e.Key)
			}
		}
		if _, ok := stored["revision"]; !ok {
			fields = append(fields, "revision")
		}
	}
	sel := bson.M{"_id": eq(id)}
	for _, f := range fields {
		if v, ok := stored[f]; ok {
			sel[f] = bson.M{"$eq": v, "$exists": true}
		} else {
			sel[f] = bson.M{"$exists": false}
		}
	}
	return sel, true
}

// --- restore rows, completion and finish ----------------------------------

var restoreIDPattern = regexp.MustCompile(`^scrum-restore-[a-f0-9]{64}$`)

// recordScrumRestoreOnce is WeKan's: the operation's restore row for one
// author, at an id fixed by the operation - an existing one must be exactly
// this row.
func (s *Server) recordScrumRestoreOnce(ctx context.Context, c historyChange) error {
	batch, _ := c.BatchID.(string)
	id := "scrum-restore-" + sha256Hex(canonical([]any{c.BoardID, batch, c.UserID}))
	want := map[string]any{"boardId": c.BoardID, "swimlaneId": c.SwimlaneID, "listId": c.ListID, "cardId": c.CardID, "entityType": c.EntityType,
		"entityId": c.EntityID, "group": nilIfEmpty(c.Group), "changeType": "restored", "previousContent": c.PreviousContent, "newContent": c.NewContent,
		"userId": c.UserID, "batchId": batch, "restoredFromId": c.RestoredFromID, "restoredByUserId": c.RestoredByUserID, "isCheckpoint": c.IsCheckpoint}
	matches := func(row map[string]any) error {
		if !rowHashIsValid(row) {
			return historyPendingErr()
		}
		for k, v := range want {
			if canonical(row[k]) != canonical(v) {
				return historyPendingErr()
			}
		}
		return nil
	}
	if existing := s.findDoc(ctx, "changeHistory", bson.M{"boardId": eq(c.BoardID), "batchId": eq(batch), "userId": eq(c.UserID)}); existing != nil {
		return matches(existing)
	}
	c.ID = id
	s.recordHistory(ctx, c)
	committed := s.findDoc(ctx, "changeHistory", bson.M{"_id": eq(id)})
	if committed == nil {
		return historyPendingErr()
	}
	return matches(committed)
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func completionIdentity(j, row map[string]any) (map[string]any, bool) {
	if !validJournal(j) || docString(j, "operationId") == "" || !rowHashIsValid(row) ||
		docString(j, "rowId") != docString(row, "_id") || docString(j, "_id") != docString(row, "boardId") {
		return nil, false
	}
	return map[string]any{"_id": j["operationId"], "version": int32(1), "boardId": j["_id"], "rowId": j["rowId"], "userId": j["userId"],
		"direction": j["direction"], "sourceHash": row["integrityHash"], "planHash": ejsonDigest(planSelector(j))}, true
}

// readCompletion is WeKan's: an operation's receipt, checked whole; an
// error when one is there and not this operation's.
func (s *Server) readCompletion(ctx context.Context, expected map[string]any) (map[string]any, error) {
	saved := s.findDoc(ctx, "scrumHistoryCompletions", bson.M{"_id": eq(docString(expected, "_id"))})
	if saved == nil {
		return nil, nil
	}
	identity := map[string]any{}
	for k, v := range saved {
		if k != "completedAt" && k != "checksum" {
			identity[k] = v
		}
	}
	at, ok := saved["completedAt"].(time.Time)
	withAt := map[string]any{"completedAt": at}
	for k, v := range identity {
		withAt[k] = v
	}
	if !ok || ejsonDigest(identity) != ejsonDigest(expected) || docString(saved, "checksum") != ejsonDigest(withAt) {
		return nil, historyPendingErr()
	}
	return saved, nil
}

func (s *Server) saveCompletion(ctx context.Context, j, row map[string]any) error {
	id, ok := completionIdentity(j, row)
	if !ok {
		return historyPendingErr()
	}
	receipt := bson.M{}
	for k, v := range id {
		receipt[k] = v
	}
	receipt["completedAt"] = time.Now().UTC().Truncate(time.Millisecond)
	receipt["checksum"] = ejsonDigest(map[string]any(receipt))
	_, _ = s.db.Collection("scrumHistoryCompletions").InsertOne(ctx, receipt)
	saved, err := s.readCompletion(ctx, id)
	if err != nil {
		return err
	}
	if saved == nil {
		return historyPendingErr()
	}
	return nil
}

// verifySource is verifyScrumHistorySource: the row unchanged, and a redo
// never of a row a newer edit superseded.
func verifySource(current, row map[string]any, direction string, rehash bool) bool {
	if current == nil || rehash && !rowHashIsValid(current) {
		return false
	}
	return docString(current, "_id") == docString(row, "_id") && docString(current, "integrityHash") == docString(row, "integrityHash") &&
		docString(current, "boardId") == docString(row, "boardId") && docString(current, "userId") == docString(row, "userId") &&
		(current["superseded"] == true) == (row["superseded"] == true) && !(direction == "redo" && current["superseded"] == true)
}

// finishScrumHistory is WeKan's: the row flagged, its state verified, the
// receipt saved, then this operation's checkpoint removed.
func (s *Server) finishScrumHistory(ctx context.Context, row, j map[string]any) error {
	if !validJournal(j) || docString(j, "operationId") == "" || docString(j, "rowId") != docString(row, "_id") || docString(j, "_id") != docString(row, "boardId") {
		return historyPendingErr()
	}
	boardID, op := docString(j, "_id"), docString(j, "operationId")
	unresolved := func() bool { return samePlan(s.scrumJournal(ctx, boardID), j) }
	cleanup := func() error {
		cp := s.scrumJournal(ctx, boardID)
		if cp == nil || docString(cp, "operationId") != op {
			return nil
		}
		if !unresolved() {
			return historyPendingErr()
		}
		_, _ = s.db.Collection("scrumHistoryPending").DeleteOne(ctx, bson.M{"_id": eq(boardID), "operationId": eq(op), "resolving": bson.M{"$exists": false}})
		if rest := s.scrumJournal(ctx, boardID); rest != nil && docString(rest, "operationId") == op {
			return historyPendingErr()
		}
		return nil
	}
	id, ok := completionIdentity(j, row)
	if !ok {
		return historyPendingErr()
	}
	done, err := s.readCompletion(ctx, id)
	if err != nil {
		return err
	}
	if done != nil {
		return cleanup()
	}
	if !unresolved() {
		return historyPendingErr()
	}
	direction := docString(j, "direction")
	current := s.findDoc(ctx, "changeHistory", bson.M{"_id": eq(docString(row, "_id"))})
	if !verifySource(current, row, direction, true) {
		return historyPendingErr()
	}
	if direction != "restore" {
		undone := direction == "undo"
		if (current["undone"] == true) != undone {
			f := bson.M{"_id": eq(docString(row, "_id")), "boardId": eq(docString(row, "boardId")), "userId": eq(docString(row, "userId")),
				"integrityHash": row["integrityHash"], "undone": current["undone"]}
			if v, has := current["superseded"]; has {
				f["superseded"] = v
			} else {
				f["superseded"] = bson.M{"$exists": false}
			}
			var at any
			if undone {
				at = time.Now().UTC().Truncate(time.Millisecond)
			}
			res, err := s.db.Collection("changeHistory").UpdateOne(ctx, f, bson.M{"$set": bson.M{"undone": undone, "undoneAt": at}})
			if err != nil || res.MatchedCount != 1 {
				return historyPendingErr()
			}
		}
		current = s.findDoc(ctx, "changeHistory", bson.M{"_id": eq(docString(row, "_id"))})
		_, atOK := current["undoneAt"].(time.Time)
		if !verifySource(current, row, direction, true) || (current["undone"] == true) != undone || undone && !atOK || !undone && current["undoneAt"] != nil {
			return historyPendingErr()
		}
	}
	if !unresolved() {
		return historyPendingErr()
	}
	if err := s.saveCompletion(ctx, j, row); err != nil {
		return err
	}
	return cleanup()
}

// --- the operation ---------------------------------------------------------

// scrumRequest is a verified scrumHistoryRequests document.
type scrumRequest = map[string]any

// applyScrumHistoryJournaled is applyScrumHistory(row, content, direction,
// request).
func (s *Server) applyScrumHistoryJournaled(ctx context.Context, u *User, row map[string]any, direction string, request scrumRequest) error {
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
	b, err := s.boardFor(ctx, boardID, u)
	if err != nil {
		return scrumNotAuthorized()
	}
	var operationID string
	if request != nil {
		sel, _ := request["selection"].(map[string]any)
		if docString(request, "userId") != u.ID || docString(request, "boardId") != boardID || docString(request, "direction") != direction ||
			docString(sel, "rowId") != docString(row, "_id") || docString(sel, "sourceHash") != docString(row, "integrityHash") {
			return scrumConflict()
		}
		if !b.Writable(u, "write", nil) {
			return scrumNotAuthorized()
		}
		operationID = docString(request, "_id")
		// Another call finished it after this one read the request.
		if done, err := s.readRequestCompletion(ctx, request); err != nil {
			return err
		} else if done != nil {
			if j := s.scrumJournal(ctx, boardID); j != nil && docString(j, "operationId") == operationID {
				return s.finishScrumHistory(ctx, row, j)
			}
			return nil
		}
	}
	assertSource := func(rehash bool) error {
		if !verifySource(s.findDoc(ctx, "changeHistory", bson.M{"_id": eq(docString(row, "_id"))}), row, direction, rehash) {
			return scrumConflict()
		}
		return nil
	}
	if err := assertSource(true); err != nil {
		return err
	}
	targets, ok := scrumHistoryRecords(content)
	if !ok {
		return &methodError{"invalid-scrum-history", "invalid-scrum-history"}
	}
	j := s.scrumJournal(ctx, boardID)
	if j != nil && (docString(j, "rowId") != docString(row, "_id") || docString(j, "direction") != direction || docString(j, "userId") != u.ID ||
		operationID != "" && docString(j, "operationId") != operationID || canonical(j["content"]) != canonical(content)) {
		return scrumConflict()
	}
	if j != nil && has(j, "resolving") {
		return &methodError{"scrum-history-pending", "The interrupted Scrum History operation is being resolved by a board administrator."}
	}
	var raw bson.M
	_ = s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(boardID)}).Decode(&raw)
	worker := ""
	fail := func(err error) error {
		s.recordScrumHistoryFailure(ctx, j, worker, err)
		return err
	}
	read := func() []map[string]any {
		out := make([]map[string]any, len(targets))
		for i, t := range targets {
			out[i] = s.findDoc(ctx, scrumHistoryCollections[docString(t, "type")], bson.M{"_id": eq(docString(t, "id"))})
		}
		return out
	}
	if j == nil {
		if s.db.Collection("scrumSprints").FindOne(ctx, bson.M{"boardId": eq(boardID), "$or": bson.A{bson.M{"rolloverPending": true}, bson.M{"rolloverPending.0": bson.M{"$exists": true}}}}).Err() == nil {
			return scrumConflict()
		}
		current := read()
		if err := s.validateScrumTargets(ctx, b, plain(raw).(map[string]any), u, targets, current); err != nil {
			return err
		}
		if err := s.scrumImportPending(ctx, boardID); err != nil {
			return err
		}
		live := []any{}
		revs := []any{}
		incs := []any{}
		for i, t := range targets {
			kind := docString(t, "type")
			live = append(live, map[string]any{"type": kind, "id": t["id"], "document": scrumHistoryDocument(kind, current[i])})
			revs = append(revs, scrumRecordRevision(kind, current[i]))
			inc := map[string]any{"before": nil, "after": nil}
			if !scrumMetadataType(kind) {
				var was any
				if s, ok := current[i]["incarnation"].(string); ok {
					was = s
				}
				inc["before"] = was
				if t["document"] != nil {
					if current[i] != nil {
						inc["after"] = was
					} else {
						inc["after"] = randomID()
					}
				}
			}
			incs = append(incs, inc)
		}
		expected := row["previousContent"]
		if direction == "undo" {
			expected = row["newContent"]
		}
		if direction != "restore" && canonical(map[string]any{"records": live}) != canonical(expected) {
			return scrumConflict()
		}
		if operationID == "" {
			operationID = randomID()
		}
		j = map[string]any{"_id": boardID, "rowId": row["_id"], "direction": direction, "userId": u.ID, "operationId": operationID,
			"content": plain(content), "before": map[string]any{"records": live}, "revisions": revs, "incarnations": incs}
		if _, err := s.db.Collection("scrumHistoryPending").InsertOne(ctx, j); err != nil {
			return historyPendingErr()
		}
		j = s.scrumJournal(ctx, boardID)
	} else {
		current := read()
		if err := s.validateScrumTargets(ctx, b, plain(raw).(map[string]any), u, targets, current); err != nil {
			return fail(err)
		}
		if err := s.scrumImportPending(ctx, boardID); err != nil {
			return err
		}
	}
	if err := assertSource(true); err != nil {
		return fail(err)
	}
	if !validJournal(j) {
		return fail(scrumConflict())
	}
	// A checkpoint saved before operation ids: given one, by compare-and-set.
	if docString(j, "operationId") == "" {
		_, _ = s.db.Collection("scrumHistoryPending").UpdateOne(ctx, bson.M{"_id": eq(boardID), "rowId": eq(docString(j, "rowId")), "operationId": bson.M{"$exists": false}},
			bson.M{"$set": bson.M{"operationId": randomID()}})
		if j = s.scrumJournal(ctx, boardID); docString(j, "operationId") == "" {
			return historyPendingErr()
		}
	}
	// This worker's claim; a newer resume displaces it.
	worker = randomID()
	if !samePlan(s.scrumJournal(ctx, boardID), j) {
		return fail(scrumConflict())
	}
	_, _ = s.db.Collection("scrumHistoryPending").UpdateOne(ctx, bson.M{"_id": eq(boardID), "operationId": eq(docString(j, "operationId")),
		"resolving": bson.M{"$exists": false}}, bson.M{"$set": bson.M{"worker": worker}})
	assertCurrent := func() error {
		if s.db.Collection("scrumHistoryPending").FindOne(ctx, bson.M{"_id": eq(boardID), "rowId": eq(docString(j, "rowId")), "userId": eq(docString(j, "userId")),
			"direction": docString(j, "direction"), "operationId": eq(docString(j, "operationId")), "worker": eq(worker)}).Err() != nil {
			return scrumConflict()
		}
		return assertSource(false)
	}
	before, _ := j["before"].(map[string]any)
	beforeRecs, _ := before["records"].([]any)
	revs, _ := j["revisions"].([]any)
	incs, _ := j["incarnations"].([]any)
	incOf := func(i int) map[string]any {
		if i < len(incs) {
			m, _ := incs[i].(map[string]any)
			return m
		}
		return nil
	}
	state := func(i int, current map[string]any) (string, bool) {
		prev, _ := beforeRecs[i].(map[string]any)
		t := targets[i]
		if docString(prev, "type") != docString(t, "type") || docString(prev, "id") != docString(t, "id") {
			return "", false
		}
		return scrumWriteState(docString(t, "type"), current, prev["document"], t["document"], revs[i], incOf(i))
	}
	verifyAll := func(wantApplied bool) error {
		if err := assertCurrent(); err != nil {
			return err
		}
		docs := read()
		if err := assertCurrent(); err != nil {
			return err
		}
		for i := range targets {
			st, ok := state(i, docs[i])
			if !ok || wantApplied && st != "applied" {
				return scrumConflict()
			}
		}
		return nil
	}
	// Every visible conflict refused before the first unfinished write.
	if err := verifyAll(false); err != nil {
		return fail(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	for i, t := range targets {
		kind := docString(t, "type")
		coll := s.db.Collection(scrumHistoryCollections[kind])
		current := s.findDoc(ctx, scrumHistoryCollections[kind], bson.M{"_id": eq(docString(t, "id"))})
		fence, _ := s.scrumWriteSelector(ctx, kind, scrumHistoryCollections[kind], docString(t, "id"))
		st, ok := state(i, current)
		if !ok {
			return fail(scrumConflict())
		}
		if st == "applied" {
			continue
		}
		target, _ := t["document"].(map[string]any)
		orig := numberOf(revs[i])
		if err := assertCurrent(); err != nil {
			return fail(err)
		}
		switch {
		case target == nil:
			res, err := coll.DeleteOne(ctx, fence)
			if err != nil || res.DeletedCount == 0 {
				return fail(scrumConflict())
			}
		case scrumMetadataType(kind):
			res, err := coll.UpdateOne(ctx, fence, bson.M{"$set": bson.M{"scrum": target["scrum"], "scrumRevision": storedNum(orig + 1)}})
			if err != nil || res.MatchedCount == 0 {
				return fail(scrumConflict())
			}
		case current == nil:
			doc := bson.M{}
			for k, v := range target {
				doc[k] = v
			}
			doc["revision"], doc["updatedAt"], doc["updatedBy"] = int32(1), now, u.ID
			if inc := incOf(i); inc != nil && inc["after"] != nil {
				doc["incarnation"] = inc["after"]
			}
			if _, err := coll.InsertOne(ctx, doc); err != nil {
				return fail(scrumConflict())
			}
		default:
			set := bson.M{}
			for k, v := range target {
				if k != "_id" {
					set[k] = v
				}
			}
			set["revision"], set["updatedAt"], set["updatedBy"] = storedNum(orig+1), now, u.ID
			update := bson.M{"$set": set}
			unset := bson.M{}
			for k := range current {
				if _, keep := target[k]; !keep && !containsString([]string{"_id", "revision", "updatedAt", "updatedBy", "incarnation"}, k) {
					unset[k] = ""
				}
			}
			if len(unset) > 0 {
				update["$unset"] = unset
			}
			res, err := coll.UpdateOne(ctx, fence, update)
			if err != nil || res.MatchedCount == 0 {
				return fail(scrumConflict())
			}
		}
		written := s.findDoc(ctx, scrumHistoryCollections[kind], bson.M{"_id": eq(docString(t, "id"))})
		if st, ok := state(i, written); !ok || st != "applied" {
			return fail(scrumConflict())
		}
	}
	// The restore rows: the operation's, for whoever did it - and for a
	// Restore, the row's own author too.
	authors := []string{u.ID}
	if direction == "restore" && docString(row, "userId") != u.ID {
		authors = []string{docString(row, "userId"), u.ID}
	}
	for _, author := range authors {
		if err := verifyAll(true); err != nil {
			return fail(err)
		}
		if err := s.recordScrumRestoreOnce(ctx, historyChange{BoardID: boardID, SwimlaneID: row["swimlaneId"], ListID: row["listId"], CardID: row["cardId"],
			EntityType: docString(row, "entityType"), EntityID: docString(row, "entityId"), Group: docString(row, "group"), ChangeType: "restored",
			PreviousContent: j["before"], NewContent: plain(content), UserID: author, RestoredFromID: docString(row, "_id"), RestoredByUserID: u.ID,
			IsCheckpoint: direction != "restore", BatchID: docString(j, "operationId")}); err != nil {
			return fail(err)
		}
	}
	if err := verifyAll(true); err != nil {
		return fail(err)
	}
	if err := s.finishScrumHistory(ctx, row, j); err != nil {
		return fail(err)
	}
	s.hub.publish(boardID, "board-changed")
	return nil
}

var scrumRetryFailures = []string{"scrum-conflict", "not-authorized", "invalid-scrum-reference", "invalid-scrum-history", "invalid-scrum"}

// recordScrumHistoryFailure is WeKan's: a retry that failed for a reason the
// next would meet again marks its checkpoint, for a board admin to resolve.
func (s *Server) recordScrumHistoryFailure(ctx context.Context, j map[string]any, worker string, err error) {
	me, ok := err.(*methodError)
	if !ok || j == nil || !containsString(scrumRetryFailures, me.Code) || docString(j, "operationId") == "" {
		return
	}
	f := bson.M{"_id": eq(docString(j, "_id")), "rowId": eq(docString(j, "rowId")), "userId": eq(docString(j, "userId")), "direction": docString(j, "direction"),
		"operationId": eq(docString(j, "operationId")), "resolving": bson.M{"$exists": false}}
	if worker != "" {
		f["worker"] = eq(worker)
	}
	reason := me.Reason
	if len(reason) > 300 {
		reason = reason[:300]
	}
	_, _ = s.db.Collection("scrumHistoryPending").UpdateOne(ctx, f, bson.M{"$set": bson.M{"lastFailure": bson.M{"at": time.Now().UTC(), "error": me.Code, "reason": reason}}})
}

// --- request ids ----------------------------------------------------------

var scrumRequestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

func requestConflict(reason string) error {
	return &methodError{"scrum-history-request-conflict", reason}
}

// verifyScrumRequest is verifyRequest: the stored request whole, its checksum
// its own.
func verifyScrumRequest(saved, identity map[string]any) bool {
	keys := []string{}
	for k := range saved {
		keys = append(keys, k)
	}
	want := []string{"_id", "version", "userId", "boardId", "direction", "selection", "createdAt", "checksum"}
	sort.Strings(keys)
	sort.Strings(want)
	if canonical(keys) != canonical(want) {
		return false
	}
	for k, v := range identity {
		if canonical(saved[k]) != canonical(v) {
			return false
		}
	}
	if _, ok := saved["createdAt"].(time.Time); !ok {
		return false
	}
	content := map[string]any{}
	for k, v := range saved {
		if k != "checksum" {
			content[k] = v
		}
	}
	if docString(saved, "checksum") != sha256Hex(canonical(content)) {
		return false
	}
	sel, _ := saved["selection"].(map[string]any)
	kind := docString(sel, "kind")
	switch kind {
	case "empty", "unsupported":
		return len(sel) == 1
	case "scrum":
		return len(sel) == 3 && docString(sel, "rowId") != "" && regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(docString(sel, "sourceHash"))
	}
	return false
}

// prepareScrumRequest is prepareScrumHistoryRequest: the selection saved
// before any write, so an empty stack stays empty for this request.
func (s *Server) prepareScrumRequest(ctx context.Context, u *User, boardID, direction, requestID string, selectRow func() (map[string]any, error)) (scrumRequest, error) {
	if !scrumRequestIDPattern.MatchString(requestID) {
		return nil, requestConflict("Scrum History request conflict")
	}
	id := "scrum-request-" + sha256Hex(canonical([]any{u.ID, requestID}))
	identity := map[string]any{"_id": id, "version": int32(1), "userId": u.ID, "boardId": boardID, "direction": direction}
	saved := s.findDoc(ctx, "scrumHistoryRequests", bson.M{"_id": eq(id)})
	if saved == nil {
		if s.db.Collection("scrumHistoryCompletions").FindOne(ctx, bson.M{"_id": eq(id)}).Err() == nil ||
			s.db.Collection("scrumHistoryPending").FindOne(ctx, bson.M{"_id": eq(boardID), "operationId": eq(id)}).Err() == nil {
			return nil, requestConflict("The original request record is missing. Its recovery evidence was retained.")
		}
		row, err := selectRow()
		if err != nil {
			return nil, err
		}
		if row != nil && docString(row, "entityType") == "scrum" && (!rowHashIsValid(row) || docString(row, "boardId") != boardID || docString(row, "userId") != u.ID) {
			return nil, requestConflict("Scrum History request conflict")
		}
		sel := map[string]any{"kind": "empty"}
		if row != nil && docString(row, "entityType") != "scrum" {
			sel = map[string]any{"kind": "unsupported"}
		} else if row != nil {
			sel = map[string]any{"kind": "scrum", "rowId": row["_id"], "sourceHash": row["integrityHash"]}
		}
		doc := map[string]any{"selection": sel, "createdAt": time.Now().UTC().Truncate(time.Millisecond)}
		for k, v := range identity {
			doc[k] = v
		}
		doc["checksum"] = sha256Hex(canonical(doc))
		_, _ = s.db.Collection("scrumHistoryRequests").InsertOne(ctx, doc)
		// Concurrent selections adopt the first stored one.
		if saved = s.findDoc(ctx, "scrumHistoryRequests", bson.M{"_id": eq(id)}); saved == nil {
			return nil, requestConflict("Scrum History request unconfirmed")
		}
	}
	if !verifyScrumRequest(saved, identity) {
		return nil, requestConflict("Scrum History request conflict")
	}
	return saved, nil
}

// readRequestCompletion is readScrumHistoryRequestCompletion.
func (s *Server) readRequestCompletion(ctx context.Context, req scrumRequest) (map[string]any, error) {
	sel, _ := req["selection"].(map[string]any)
	if docString(sel, "kind") != "scrum" {
		return nil, requestConflict("Scrum History completion conflict")
	}
	saved := s.findDoc(ctx, "scrumHistoryCompletions", bson.M{"_id": eq(docString(req, "_id"))})
	if saved == nil {
		return nil, nil
	}
	plan := docString(saved, "planHash")
	if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(plan) {
		return nil, historyPendingErr()
	}
	return s.readCompletion(ctx, map[string]any{"_id": req["_id"], "version": int32(1), "boardId": req["boardId"], "userId": req["userId"],
		"direction": req["direction"], "rowId": sel["rowId"], "sourceHash": sel["sourceHash"], "planHash": plan})
}

// requestedScrumReversal is requestedScrumRow: an undo or redo under a
// request id - the row the request first selected, whatever came since.
func (s *Server) requestedScrumReversal(r *http.Request, u *User, boardID, direction, requestID string) (map[string]any, error) {
	ctx := r.Context()
	key := map[string]string{"undo": "undone", "redo": "redone"}[direction]
	req, err := s.prepareScrumRequest(ctx, u, boardID, direction, requestID, func() (map[string]any, error) {
		row, err := s.pendingScrumHistoryRow(ctx, boardID, u.ID, direction)
		if err != nil || row != nil {
			return row, err
		}
		return s.historyStackTop(ctx, u, boardID, direction), nil
	})
	if err != nil {
		return nil, err
	}
	sel, _ := req["selection"].(map[string]any)
	switch docString(sel, "kind") {
	case "empty":
		return map[string]any{key: false}, nil
	case "unsupported":
		return nil, &methodError{"scrum-history-request-unsupported", "Request IDs currently support Scrum History only. This request changed nothing."}
	}
	row := s.findDoc(ctx, "changeHistory", bson.M{"_id": eq(docString(sel, "rowId"))})
	if row == nil || docString(row, "boardId") != boardID || docString(row, "userId") != u.ID || docString(row, "integrityHash") != docString(sel, "sourceHash") ||
		docString(row, "entityType") != "scrum" {
		return nil, requestConflict("The saved History source is missing or changed.")
	}
	if err := s.requireHistoryIntegrity(r, u, row); err != nil {
		return nil, err
	}
	if err := s.requireHistoryRowAccess(ctx, u, row); err != nil {
		return nil, err
	}
	if err := s.applyScrumHistoryJournaled(ctx, u, row, direction, req); err != nil {
		if _, ok := err.(*methodError); ok {
			return nil, err
		}
		return nil, requestConflict("The request could not be confirmed. Keep its ID when retrying; do not select a new operation.")
	}
	return map[string]any{key: true, "entityType": row["entityType"], "entityId": row["entityId"], "group": row["group"]}, nil
}
