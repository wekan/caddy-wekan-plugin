package web

// A stopped Scrum History operation, as WeKan's
// server/lib/scrumHistoryRecovery.js and scrum.inspectHistoryCheckpoint /
// resolveHistoryCheckpoint: a checkpoint whose retries keep failing - a
// conflict, its author no longer able to write, a changed source row - blocks
// the board's Scrum writes. Any member who may write sees that it does and
// whether it is theirs; a board admin sees what it holds and rolls it back
// (every record it wrote returned to what it was) or discards it (the board
// kept as it is). Rollback is refused when somebody changed one of its
// records since, or its History rows were already written. Each resolution,
// and each failed one, is a row in Admin Panel -> Problems -> Recovery.

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

const (
	checkpointSettle  = 2 * time.Second
	checkpointLease   = 5 * time.Minute
	checkpointTargets = 50
)

var errCheckpointChanged = "A Scrum record changed while the checkpoint was being resolved. Inspect it again."
var errCheckpointTaken = "Another recovery took over this checkpoint. Inspect it again."

func recoveryErr(msg string) error { return &methodError{"scrum-history-recovery", msg} }

type checkpointTarget struct {
	kind, id      string
	before, after any
	revision      any
	inc           map[string]any
}

type checkpointExam struct {
	present            bool
	checkpoint         map[string]any
	targets            []checkpointTarget
	states             []string
	board              *Board
	reasons            []string
	stuck, recorded    bool
	rollbackBlocked    string
	key                string
	total, applied     int
	pending, conflicts int
	busy               bool
	resolving          map[string]any
}

func checkpointKey(j map[string]any) string {
	if op := docString(j, "operationId"); op != "" {
		return op
	}
	return "row:" + docString(j, "rowId")
}

func checkpointKeyFilter(j map[string]any) bson.M {
	if op := docString(j, "operationId"); op != "" {
		return bson.M{"operationId": eq(op)}
	}
	return bson.M{"operationId": bson.M{"$exists": false}, "rowId": eq(docString(j, "rowId"))}
}

func checkpointTargetsOf(j map[string]any) ([]checkpointTarget, bool) {
	if !validJournal(j) {
		return nil, false
	}
	content := j["content"].(map[string]any)["records"].([]any)
	before := j["before"].(map[string]any)["records"].([]any)
	revs := j["revisions"].([]any)
	incs, _ := j["incarnations"].([]any)
	seen := map[string]bool{}
	out := []checkpointTarget{}
	for i, x := range content {
		e, _ := x.(map[string]any)
		b, _ := before[i].(map[string]any)
		kind, id := docString(e, "type"), docString(e, "id")
		if e == nil || b == nil || scrumHistoryCollections[kind] == "" || id == "" || docString(b, "type") != kind || docString(b, "id") != id || seen[kind+":"+id] {
			return nil, false
		}
		seen[kind+":"+id] = true
		t := checkpointTarget{kind: kind, id: id, after: e["document"], before: b["document"], revision: revs[i]}
		if i < len(incs) {
			t.inc, _ = incs[i].(map[string]any)
		}
		out = append(out, t)
	}
	return out, true
}

func (t checkpointTarget) state(current map[string]any) string {
	st, ok := scrumWriteState(t.kind, current, t.before, t.after, t.revision, t.inc)
	if !ok {
		return "conflict"
	}
	return st
}

// atBefore: the record is as it was before the operation.
func (t checkpointTarget) atBefore(state string) bool {
	return state == "pending" || state == "applied" && canonical(t.before) == canonical(t.after)
}

func (s *Server) examineCheckpoint(ctx context.Context, boardID string) checkpointExam {
	j := s.scrumJournal(ctx, boardID)
	if j == nil {
		return checkpointExam{}
	}
	ex := checkpointExam{present: true, checkpoint: j, key: checkpointKey(j)}
	targets, ok := checkpointTargetsOf(j)
	if !ok {
		ex.reasons = append(ex.reasons, "malformed")
	} else {
		ex.targets = targets
		for _, t := range targets {
			st := t.state(s.findDoc(ctx, scrumHistoryCollections[t.kind], bson.M{"_id": eq(t.id)}))
			ex.states = append(ex.states, st)
			switch st {
			case "applied":
				ex.applied++
			case "pending":
				ex.pending++
			default:
				ex.conflicts++
			}
		}
		ex.total = len(targets)
		if ex.conflicts > 0 {
			ex.reasons = append(ex.reasons, "target-conflict")
		}
	}
	direction := docString(j, "direction")
	row := s.findDoc(ctx, "changeHistory", bson.M{"_id": eq(docString(j, "rowId"))})
	var content any
	if row != nil {
		content = row["newContent"]
		if direction == "undo" || direction == "restore" && content == nil {
			content = row["previousContent"]
		}
	}
	if row == nil || !rowHashIsValid(row) || docString(row, "boardId") != boardID || docString(row, "entityType") != "scrum" ||
		direction != "restore" && docString(row, "userId") != docString(j, "userId") || direction == "redo" && row["superseded"] == true ||
		canonical(content) != canonical(j["content"]) {
		ex.reasons = append(ex.reasons, "source-changed")
	}
	var b Board
	if s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(boardID)}).Decode(&b) == nil {
		ex.board = &b
	} else {
		ex.reasons = append(ex.reasons, "board-missing")
	}
	author := s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(docString(j, "userId"))}).Err() == nil
	if ex.board != nil && (!author || !ex.board.Can(docString(j, "userId"), "write")) {
		ex.reasons = append(ex.reasons, "author-cannot-retry")
	}
	if lf, _ := j["lastFailure"].(map[string]any); lf != nil && containsString(scrumRetryFailures, docString(lf, "error")) {
		ex.reasons = append(ex.reasons, "retry-failed")
	}
	if op := docString(j, "operationId"); op != "" {
		ex.recorded = s.findDoc(ctx, "changeHistory", bson.M{"boardId": eq(boardID), "batchId": eq(op), "changeType": "restored"}) != nil
	}
	ex.stuck = len(ex.reasons) > 0
	switch {
	case !ex.stuck:
		ex.rollbackBlocked = "This undo or redo has not stopped on a conflict; its author can retry it."
	case ex.targets == nil:
		ex.rollbackBlocked = "The checkpoint is damaged; it can only be discarded."
	case ex.board == nil:
		ex.rollbackBlocked = "The board no longer exists; the checkpoint can only be discarded."
	case ex.conflicts > 0:
		ex.rollbackBlocked = "Someone changed some of its records since; rolling back would overwrite their changes. It can only be discarded."
	case ex.recorded:
		ex.rollbackBlocked = "Its History records were already written; it can only be discarded."
	}
	if r, _ := j["resolving"].(map[string]any); r != nil {
		ex.resolving = r
		at, ok := r["at"].(time.Time)
		ex.busy = r["offline"] != true && ok && time.Since(at) < checkpointLease
	}
	return ex
}

// inspectCheckpoint is inspectScrumHistoryCheckpoint's report.
func (ex checkpointExam) report(boardID string) map[string]any {
	j := ex.checkpoint
	targets := []any{}
	for i, t := range ex.targets {
		if i >= checkpointTargets {
			break
		}
		targets = append(targets, map[string]any{"type": t.kind, "id": t.id, "state": ex.states[i]})
	}
	var resolving, lastFailure any
	if ex.resolving != nil {
		resolving = map[string]any{"action": ex.resolving["action"], "offline": ex.resolving["offline"] == true, "at": ex.resolving["at"]}
	}
	if lf, _ := j["lastFailure"].(map[string]any); lf != nil {
		lastFailure = map[string]any{"error": lf["error"], "at": lf["at"]}
	}
	return map[string]any{"present": true, "key": ex.key, "boardId": boardID, "rowId": j["rowId"], "userId": j["userId"], "direction": j["direction"],
		"stuck": ex.stuck, "reasons": ex.reasons, "total": ex.total, "applied": ex.applied, "pending": ex.pending, "conflicted": ex.conflicts,
		"canRollback": ex.rollbackBlocked == "", "rollbackBlocked": ex.rollbackBlocked, "canDiscard": ex.stuck, "historyRecorded": ex.recorded,
		"resolving": resolving, "busy": ex.busy, "lastFailure": lastFailure, "targets": targets, "moreTargets": max(0, len(ex.targets)-checkpointTargets)}
}

// inspectCheckpointFor is inspectScrumHistoryCheckpointOnline: the report
// for a board admin, whether it blocks and whose for any other writer.
func (s *Server) inspectCheckpointFor(ctx context.Context, u *User, boardID string) (map[string]any, error) {
	b, err := s.boardFor(ctx, boardID, u)
	if err != nil || !b.Writable(u, "write", nil) {
		return nil, scrumNotAuthorized()
	}
	ex := s.examineCheckpoint(ctx, boardID)
	if !ex.present {
		return nil, nil
	}
	if b.AdminOf(u.ID) {
		r := ex.report(boardID)
		r["canResolve"] = true
		return r, nil
	}
	return map[string]any{"present": true, "key": ex.key, "direction": ex.checkpoint["direction"], "stuck": ex.stuck,
		"own": docString(ex.checkpoint, "userId") == u.ID, "canResolve": false}, nil
}

// resolveCheckpoint is resolveScrumHistoryCheckpoint, online.
func (s *Server) resolveCheckpoint(ctx context.Context, boardID, action, key, actor string, settle time.Duration) (map[string]any, error) {
	if action != "rollback" && action != "discard" {
		return nil, recoveryErr("Choose rollback or discard.")
	}
	if key == "" || len(key) > 300 {
		return nil, recoveryErr("Name the checkpoint to resolve, as its inspection reported it.")
	}
	absent := map[string]any{"changed": false, "state": "absent", "action": action, "key": key}
	first := s.examineCheckpoint(ctx, boardID)
	if !first.present || first.key != key {
		return absent, nil
	}
	if !first.stuck || action == "rollback" && first.rollbackBlocked != "" {
		return nil, recoveryErr(first.rollbackBlocked)
	}
	if first.resolving != nil && first.resolving["offline"] == true {
		return nil, recoveryErr("An offline recovery of this checkpoint started; finish it offline.")
	}
	if first.busy {
		return nil, recoveryErr("Another administrator is resolving this checkpoint; try again in a few minutes.")
	}
	original := first.checkpoint
	token := randomID()
	claim := checkpointKeyFilter(original)
	claim["_id"] = eq(boardID)
	if w, ok := original["worker"]; ok {
		claim["worker"] = bson.M{"$eq": w}
	} else {
		claim["worker"] = bson.M{"$exists": false}
	}
	pendingColl := s.db.Collection("scrumHistoryPending")
	res, err := pendingColl.UpdateOne(ctx, claim, bson.M{"$set": bson.M{"worker": token,
		"resolving": bson.M{"token": token, "action": action, "offline": false, "by": nilIfEmpty(actor), "at": time.Now().UTC()}}})
	if err != nil || res.MatchedCount == 0 {
		if again := s.scrumJournal(ctx, boardID); again == nil || checkpointKey(again) != key {
			return absent, nil
		}
		return nil, recoveryErr("The checkpoint changed while it was being claimed. Inspect it again.")
	}
	held := func() error {
		r, err := pendingColl.UpdateOne(ctx, bson.M{"_id": eq(boardID), "worker": eq(token)}, bson.M{"$set": bson.M{"resolving.at": time.Now().UTC()}})
		if err != nil || r.MatchedCount == 0 {
			return recoveryErr(errCheckpointTaken)
		}
		return nil
	}
	release := func(err error) (map[string]any, error) {
		_, _ = pendingColl.UpdateOne(ctx, bson.M{"_id": eq(boardID), "worker": eq(token)}, bson.M{"$unset": bson.M{"resolving": ""}})
		return nil, err
	}
	if settle > 0 {
		time.Sleep(settle)
	}
	if err := held(); err != nil {
		return release(err)
	}
	second := s.examineCheckpoint(ctx, boardID)
	if !second.present || second.key != key || docString(second.checkpoint, "worker") != token {
		return release(recoveryErr(errCheckpointTaken))
	}
	if !second.stuck || action == "rollback" && second.rollbackBlocked != "" {
		return release(recoveryErr(second.rollbackBlocked))
	}
	reverted, removed, recreated := 0, 0, 0
	if action == "rollback" {
		for i := len(second.targets) - 1; i >= 0; i-- {
			t := second.targets[i]
			if t.atBefore(second.states[i]) {
				continue
			}
			coll := s.db.Collection(scrumHistoryCollections[t.kind])
			if err := held(); err != nil {
				return release(err)
			}
			current := s.findDoc(ctx, scrumHistoryCollections[t.kind], bson.M{"_id": eq(t.id)})
			st := t.state(current)
			if t.atBefore(st) {
				continue
			}
			if st != "applied" {
				return release(recoveryErr(errCheckpointChanged))
			}
			before, _ := t.before.(map[string]any)
			fence, _ := s.scrumWriteSelector(ctx, t.kind, scrumHistoryCollections[t.kind], t.id)
			switch {
			case before == nil:
				if r, err := coll.DeleteOne(ctx, fence); err != nil || r.DeletedCount == 0 {
					return release(recoveryErr(errCheckpointChanged))
				}
				removed++
			case current == nil:
				doc := bson.M{}
				for k, v := range before {
					doc[k] = v
				}
				doc["revision"], doc["updatedAt"] = t.revision, time.Now().UTC().Truncate(time.Millisecond)
				if inc, _ := t.inc["before"].(string); inc != "" {
					doc["incarnation"] = inc
				}
				if _, err := coll.InsertOne(ctx, doc); err != nil {
					return release(recoveryErr(errCheckpointChanged))
				}
				recreated++
			case scrumMetadataType(t.kind):
				sc := before["scrum"]
				if sc == nil {
					sc = map[string]any{}
				}
				if r, err := coll.UpdateOne(ctx, fence, bson.M{"$set": bson.M{"scrum": sc, "scrumRevision": t.revision}}); err != nil || r.MatchedCount == 0 {
					return release(recoveryErr(errCheckpointChanged))
				}
			default:
				set := bson.M{}
				for k, v := range before {
					if k != "_id" {
						set[k] = v
					}
				}
				set["revision"] = t.revision
				update := bson.M{"$set": set}
				unset := bson.M{}
				keep := []string{"_id", "revision", "updatedAt", "updatedBy", "incarnation", "rolloverPending", "rolloverTotal", "rolloverDone", "rolloverError", "rolloverLease"}
				for k := range current {
					if _, has := before[k]; !has && !containsString(keep, k) {
						unset[k] = ""
					}
				}
				if len(unset) > 0 {
					update["$unset"] = unset
				}
				if r, err := coll.UpdateOne(ctx, fence, update); err != nil || r.MatchedCount == 0 {
					return release(recoveryErr(errCheckpointChanged))
				}
			}
			reverted++
		}
	}
	if err := held(); err != nil {
		return release(err)
	}
	final := s.examineCheckpoint(ctx, boardID)
	if !final.present || final.key != key || docString(final.checkpoint, "worker") != token {
		return release(recoveryErr(errCheckpointTaken))
	}
	if action == "rollback" {
		for i, t := range final.targets {
			if !t.atBefore(final.states[i]) {
				return release(recoveryErr(errCheckpointChanged))
			}
		}
		if final.recorded {
			return release(recoveryErr(errCheckpointChanged))
		}
	} else if canonical(final.states) != canonical(second.states) {
		return release(recoveryErr(errCheckpointChanged))
	}
	del := checkpointKeyFilter(original)
	del["_id"], del["worker"] = eq(boardID), eq(token)
	if r, err := pendingColl.DeleteOne(ctx, del); err != nil || r.DeletedCount == 0 {
		return release(recoveryErr(errCheckpointTaken))
	}
	state := "discarded"
	if action == "rollback" {
		state = "rolled-back"
	}
	title := ""
	if second.board != nil {
		title = second.board.Title
	}
	s.hub.publish(boardID, "board-changed")
	return map[string]any{"changed": true, "state": state, "action": action, "key": key, "boardId": boardID, "boardTitle": title,
		"rowId": original["rowId"], "userId": original["userId"], "direction": original["direction"], "reasons": second.reasons,
		"total": second.total, "applied": second.applied, "pending": second.pending, "conflicted": second.conflicts,
		"reverted": reverted, "removedRecords": removed, "recreatedRecords": recreated, "batchJobRemoved": false, "offline": false}, nil
}

// describeResolution is WeKan's.
func describeResolution(r map[string]any, who string) string {
	n := func(k string) string { return strconv.Itoa(int(numberOf(r[k]))) }
	what := "rolled back " + n("reverted") + " of " + n("total") + " records to their values before it"
	if r["state"] != "rolled-back" {
		what = "discarded its checkpoint and left the records as they were (" + n("applied") + " of " + n("total") + " already written, " +
			n("pending") + " not written, " + n("conflicted") + " changed by someone else since)"
	}
	out := who + " resolved the stopped Scrum History " + docString(r, "direction") + " of user " + docString(r, "userId") +
		" (History row " + docString(r, "rowId") + ", operation " + docString(r, "key") + ") on board " + docString(r, "boardId") + ": " + what + ". " +
		"Why it was stuck: " + strings.Join(stringsOf(r["reasons"]), ", ") + "."
	if numberOf(r["removedRecords"]) > 0 {
		out += " Removed " + n("removedRecords") + " planning records the " + docString(r, "direction") + " had created."
	}
	return out
}

func (s *Server) checkpointMethods() map[string]methodFunc {
	return map[string]methodFunc{
		"scrum.inspectHistoryCheckpoint": func(r *http.Request, u *User, args []json.RawMessage) (any, error) {
			boardID, err := stringArg(args, 0)
			if err != nil {
				return nil, err
			}
			return s.inspectCheckpointFor(r.Context(), u, boardID)
		},
		"scrum.resolveHistoryCheckpoint": func(r *http.Request, u *User, args []json.RawMessage) (any, error) {
			boardID, err := stringArg(args, 0)
			if err != nil {
				return nil, err
			}
			key, err := stringArg(args, 1)
			if err != nil {
				return nil, err
			}
			action, err := stringArg(args, 2)
			if err != nil || action != "rollback" && action != "discard" {
				return nil, &methodError{"Match.Error", "Match failed"}
			}
			ctx := r.Context()
			b, err := s.boardFor(ctx, boardID, u)
			if err != nil || !b.AdminOf(u.ID) {
				return nil, scrumNotAuthorized()
			}
			who := "Board admin " + u.Username + " (" + u.ID + ")"
			unlock := scrumLock(boardID)
			res, rerr := s.resolveCheckpoint(ctx, boardID, action, key, u.ID, checkpointSettleFor())
			unlock()
			verb := "discard"
			if action == "rollback" {
				verb = "roll back"
			}
			if rerr != nil {
				me, _ := rerr.(*methodError)
				reason := "unknown error"
				if me != nil {
					reason = me.Reason
				}
				s.recordCheckpointAudit(ctx, u, b, false, false, "error", who+" could not "+verb+" the stopped Scrum History operation "+key+" on board "+boardID+": "+reason)
				return nil, rerr
			}
			if res["changed"] == true {
				s.recordCheckpointAudit(ctx, u, b, true, numberOf(res["removedRecords"]) > 0, "warning", describeResolution(res, who))
			}
			return res, nil
		},
	}
}

// checkpointSettleFor is SETTLE_MS: a moment for a retry already running.
var checkpointSettleFor = func() time.Duration { return checkpointSettle }

// recordCheckpointAudit is recordRecoveryAudit's row: never in the way of
// the resolution it describes.
func (s *Server) recordCheckpointAudit(ctx context.Context, u *User, b *Board, done, deleted bool, severity, detail string) {
	defer func() { _ = recover() }()
	_, _ = s.db.Collection("recoveryEvents").InsertOne(context.WithoutCancel(ctx), bson.M{"_id": randomID(), "type": "scrum-history-checkpoint-resolved", // inserted, not a filter
		"done": done, "deletedData": deleted, "userId": u.ID, "username": u.Username, "boardIds": bson.A{b.ID}, "boardTitles": bson.A{b.Title},
		"detail": detail, "severity": severity, "source": "admin-panel", "createdAt": time.Now().UTC()})
}
