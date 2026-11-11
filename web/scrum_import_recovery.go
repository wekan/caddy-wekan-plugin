package web

// An interrupted Scrum import, as WeKan's server/lib/scrumImportRecovery.js
// and scrum.resumeImport / discardImport: an import writes its plan
// (scrumImportSteps) and a checkpoint (scrumImportPending) before its first
// write, so a writer that stopped - its lease run out - leaves the board's
// Scrum blocked and its plan to finish. A board admin finishes it online
// from the Scrum view: every step checked against what is there - a step not
// yet written as it was, one written as the plan says, nothing else - then
// the rest applied, the sprints' import marks cleared and the plan removed.
// One whose plan was never fully saved can only be discarded. Offline
// recovery and rollback are WeKan's operator tools, not this.

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const scrumImportLease = 2 * time.Minute

var importStepCollections = map[string]string{"boards": "boards", "cards": "cards", "lists": "lists", "swimlanes": "swimlanes",
	"sprints": "scrumSprints", "releases": "scrumReleases", "events": "scrumEvents", "dailyObservations": "scrumDailySnapshots", "snapshotRows": "scrumSnapshotRows"}

func importRecoveryErr(msg string) error { return &methodError{"scrum-import-recovery", msg} }

func isMetadataStep(coll string) bool {
	return coll == "boards" || coll == "cards" || coll == "lists" || coll == "swimlanes"
}

// validateImportStep is validateStep.
func validateImportStep(row, cp map[string]any, index int) (map[string]any, error) {
	op := docString(cp, "operationId")
	if row == nil || docString(row, "_id") != op+":"+strconv.Itoa(index) || docString(row, "operationId") != op || docString(row, "boardId") != docString(cp, "_id") ||
		numberOf(row["index"]) != float64(index) {
		return nil, importRecoveryErr("The recovery plan is incomplete or out of order.")
	}
	step, _ := row["step"].(map[string]any)
	after, _ := step["after"].(map[string]any)
	coll := docString(step, "collection")
	if step == nil || importStepCollections[coll] == "" || after == nil {
		return nil, importRecoveryErr("Invalid recovery step.")
	}
	if isMetadataStep(coll) {
		fields := []string{"scrum", "scrumRevision"}
		if coll == "boards" {
			fields = append(fields, "scrumImportLosses")
		}
		before, _ := step["before"].(map[string]any)
		ok := docString(step, "kind") == "update" && docString(step, "boardId") == docString(cp, "_id") && docString(step, "id") != "" &&
			(coll != "boards" || docString(step, "id") == docString(cp, "_id")) && before != nil && len(after) == len(fields)
		for k := range before {
			ok = ok && containsString(fields, k)
		}
		for _, f := range fields {
			ok = ok && has(after, f)
		}
		rev, prev := numberOf(after["scrumRevision"]), numberOf(before["scrumRevision"])
		ok = ok && (rev == 1 || prev == prev && rev == prev+1)
		if !ok {
			return nil, importRecoveryErr("Invalid metadata recovery step.")
		}
	} else if docString(step, "kind") != "insert" || docString(after, "boardId") != docString(cp, "_id") || docString(after, "_id") == "" ||
		coll == "sprints" && after["scrumImportPending"] != true {
		return nil, importRecoveryErr("Invalid planning recovery step.")
	}
	return step, nil
}

func metadataMatches(current, values, after map[string]any) bool {
	for k := range after {
		if has(current, k) != has(values, k) || canonical(current[k]) != canonical(values[k]) {
			return false
		}
	}
	return true
}

func (s *Server) importSteps(ctx context.Context, boardID, op string) []map[string]any {
	return s.findDocs(ctx, "scrumImportSteps", bson.M{"boardId": eq(boardID), "operationId": eq(op)}, options.Find().SetSort(bson.D{{Key: "index", Value: 1}}))
}

// inspectImport is WeKan's, for resuming: the checkpoint and its whole plan,
// each step as it should be by now.
func (s *Server) inspectImport(ctx context.Context, boardID string) (map[string]any, error) {
	cp := s.findDoc(ctx, "scrumImportPending", bson.M{"_id": eq(boardID)})
	if cp == nil {
		return nil, importRecoveryErr("No recoverable Scrum import checkpoint exists for this board.")
	}
	state, total, next := docString(cp, "state"), numberOf(cp["total"]), numberOf(cp["next"])
	if docString(cp, "operationId") == "" || !containsString([]string{"preparing", "applying", "applied", "cleaning", "rolling-back", "rollback-cleaning"}, state) ||
		total != total || total < 1 || total > 70001 || next != next || next < 0 || next > total || state == "preparing" && next != 0 ||
		(state == "applied" || state == "cleaning") && next != total {
		return nil, importRecoveryErr("Invalid recovery checkpoint.")
	}
	if state == "rolling-back" || state == "rollback-cleaning" {
		return nil, importRecoveryErr("Rollback has started; continue with --rollback.")
	}
	if s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(boardID)}).Err() != nil {
		return nil, importRecoveryErr("The destination board no longer exists.")
	}
	if state == "cleaning" {
		return cp, nil
	}
	index, boards := 0, 0
	seen := map[string]bool{}
	for _, row := range s.importSteps(ctx, boardID, docString(cp, "operationId")) {
		step, err := validateImportStep(row, cp, index)
		if err != nil {
			return nil, err
		}
		coll := docString(step, "collection")
		after := step["after"].(map[string]any)
		id := docString(step, "id")
		if docString(step, "kind") == "insert" {
			id = docString(after, "_id")
		}
		if seen[coll+":"+id] {
			return nil, importRecoveryErr("The recovery plan writes a target more than once.")
		}
		seen[coll+":"+id] = true
		if coll == "boards" {
			boards++
			if float64(index) != total-1 {
				return nil, importRecoveryErr("Board settings must be the last recovery step.")
			}
		}
		current := s.findDoc(ctx, importStepCollections[coll], bson.M{"_id": eq(id)})
		if docString(step, "kind") == "insert" {
			expected := after
			if state == "applied" && coll == "sprints" && current != nil && !has(current, "scrumImportPending") {
				expected = map[string]any{}
				for k, v := range after {
					if k != "scrumImportPending" {
						expected[k] = v
					}
				}
			}
			if current != nil && canonical(current) != canonical(expected) || current == nil && float64(index) < next {
				return nil, importRecoveryErr("A planning or observation target changed or disappeared.")
			}
		} else {
			if current == nil || coll != "boards" && docString(current, "boardId") != boardID {
				return nil, importRecoveryErr("A metadata target moved or disappeared.")
			}
			before, _ := step["before"].(map[string]any)
			isBefore, isAfter := metadataMatches(current, before, after), metadataMatches(current, after, after)
			if !isAfter && !(float64(index) >= next && isBefore) {
				return nil, importRecoveryErr("A metadata target changed.")
			}
		}
		index++
	}
	if float64(index) != total || boards != 1 {
		return nil, importRecoveryErr("The recovery plan is incomplete; original import data is required.")
	}
	return cp, nil
}

// applyImportStep is WeKan's: an insert (the same document already there is
// it), or the metadata set where it is as the plan found it.
func (s *Server) applyImportStep(ctx context.Context, step map[string]any) error {
	coll := s.db.Collection(importStepCollections[docString(step, "collection")])
	after := step["after"].(map[string]any)
	if docString(step, "kind") == "insert" {
		if _, err := coll.InsertOne(ctx, after); err != nil {
			existing := s.findDoc(ctx, importStepCollections[docString(step, "collection")], bson.M{"_id": eq(docString(after, "_id"))})
			if !mongo.IsDuplicateKeyError(err) || canonical(existing) != canonical(after) {
				return err
			}
		}
		return nil
	}
	before, _ := step["before"].(map[string]any)
	id, boardID := docString(step, "id"), docString(step, "boardId")
	filter := bson.M{"_id": eq(id)}
	if docString(step, "collection") != "boards" {
		filter["boardId"] = eq(boardID)
	}
	if v, ok := before["scrumRevision"]; ok {
		filter["scrumRevision"] = bson.M{"$eq": v}
	} else {
		filter["scrumRevision"] = bson.M{"$exists": false}
	}
	current := s.findDoc(ctx, importStepCollections[docString(step, "collection")], bson.M{"_id": eq(id)})
	if current != nil && metadataMatches(current, before, after) {
		if r, err := coll.UpdateOne(ctx, filter, bson.M{"$set": after}); err == nil && r.MatchedCount == 1 {
			return nil
		}
	}
	current = s.findDoc(ctx, importStepCollections[docString(step, "collection")], bson.M{"_id": eq(id)})
	if current == nil || docString(step, "collection") != "boards" && docString(current, "boardId") != boardID || !metadataMatches(current, after, after) {
		return importRecoveryErr("An imported item changed or no longer belongs to the destination board")
	}
	return nil
}

// resumeImport is recoverImport(apply, online).
func (s *Server) resumeImport(ctx context.Context, boardID string) (map[string]any, error) {
	if s.db.Collection("scrumImportRecoveryLocks").FindOne(ctx, bson.M{"_id": eq(boardID)}).Err() == nil {
		return nil, importRecoveryErr("An offline recovery claim exists for this board; finish it offline.")
	}
	cp, err := s.inspectImport(ctx, boardID)
	if err != nil {
		return nil, err
	}
	op, token := docString(cp, "operationId"), randomID()
	pending := s.db.Collection("scrumImportPending")
	now := time.Now().UTC()
	owner := bson.M{"owner": bson.M{"$exists": false}}
	if v, ok := cp["owner"]; ok {
		owner = bson.M{"owner": v}
	}
	take := bson.M{"_id": eq(boardID), "operationId": eq(op), "$or": bson.A{bson.M{"leaseUntil": bson.M{"$exists": false}}, bson.M{"leaseUntil": bson.M{"$lt": now}}}}
	for k, v := range owner {
		take[k] = v
	}
	if r, err := pending.UpdateOne(ctx, take, bson.M{"$set": bson.M{"owner": token, "leaseUntil": now.Add(scrumImportLease)}}); err != nil || r.MatchedCount == 0 {
		return nil, importRecoveryErr("The import is still running; try again in a few minutes.")
	}
	ident := bson.M{"_id": eq(boardID), "operationId": eq(op), "owner": eq(token)}
	with := func(extra bson.M) bson.M {
		f := bson.M{}
		for k, v := range ident {
			f[k] = v
		}
		for k, v := range extra {
			f[k] = v
		}
		return f
	}
	changed := importRecoveryErr("The recovery checkpoint changed.")
	total := int(numberOf(cp["total"]))
	state := docString(cp, "state")
	cleanup := func() (map[string]any, error) {
		_, _ = s.db.Collection("scrumImportSteps").DeleteMany(ctx, bson.M{"boardId": eq(boardID), "operationId": eq(op)})
		if r, err := pending.DeleteOne(ctx, with(bson.M{"state": "cleaning"})); err != nil || r.DeletedCount == 0 {
			return nil, changed
		}
		s.hub.publish(boardID, "board-changed")
		return map[string]any{"changed": true, "scope": "scrum", "total": total, "next": total, "state": "completed"}, nil
	}
	if state == "cleaning" {
		return cleanup()
	}
	if state == "preparing" {
		if r, err := pending.UpdateOne(ctx, with(bson.M{"state": "preparing", "next": 0}), bson.M{"$set": bson.M{"state": "applying"}}); err != nil || r.MatchedCount == 0 {
			return nil, changed
		}
		state = "applying"
	}
	if state == "applying" {
		rows := s.importSteps(ctx, boardID, op)
		for index := int(numberOf(cp["next"])); index < total; index++ {
			if index >= len(rows) {
				return nil, importRecoveryErr("The recovery plan is incomplete or out of order.")
			}
			step, err := validateImportStep(rows[index], cp, index)
			if err != nil {
				return nil, err
			}
			if err := s.applyImportStep(ctx, step); err != nil {
				return nil, err
			}
			if r, err := pending.UpdateOne(ctx, with(bson.M{"state": "applying", "next": index}),
				bson.M{"$set": bson.M{"next": index + 1, "leaseUntil": time.Now().UTC().Add(scrumImportLease)}}); err != nil || r.MatchedCount == 0 {
				return nil, changed
			}
		}
		if r, err := pending.UpdateOne(ctx, with(bson.M{"state": "applying", "next": total}), bson.M{"$set": bson.M{"state": "applied"}}); err != nil || r.MatchedCount == 0 {
			return nil, changed
		}
	}
	if _, err := s.inspectImport(ctx, boardID); err != nil {
		return nil, err
	}
	for _, row := range s.findDocs(ctx, "scrumImportSteps", bson.M{"boardId": eq(boardID), "operationId": eq(op), "step.collection": "sprints"}) {
		after, _ := row["step"].(map[string]any)["after"].(map[string]any)
		_, _ = s.db.Collection("scrumSprints").UpdateOne(ctx, bson.M{"_id": eq(docString(after, "_id")), "boardId": eq(boardID)}, bson.M{"$unset": bson.M{"scrumImportPending": ""}})
	}
	if r, err := pending.UpdateOne(ctx, with(bson.M{"state": "applied"}), bson.M{"$set": bson.M{"state": "cleaning"}}); err != nil || r.MatchedCount == 0 {
		return nil, changed
	}
	return cleanup()
}

// discardPreparingImport is WeKan's: an import whose plan was never fully
// saved wrote nothing; its staging goes.
func (s *Server) discardPreparingImport(ctx context.Context, boardID string) (map[string]any, error) {
	pending := s.db.Collection("scrumImportPending")
	cp := s.findDoc(ctx, "scrumImportPending", bson.M{"_id": eq(boardID)})
	if cp == nil {
		return nil, importRecoveryErr("No recoverable Scrum import checkpoint exists for this board.")
	}
	if docString(cp, "state") != "preparing" || numberOf(cp["next"]) != 0 {
		return nil, importRecoveryErr("Only an import whose plan was never fully saved can be discarded here.")
	}
	if s.db.Collection("scrumImportRecoveryLocks").FindOne(ctx, bson.M{"_id": eq(boardID)}).Err() == nil {
		return nil, importRecoveryErr("An offline recovery claim exists for this board; finish it offline.")
	}
	op, token, now := docString(cp, "operationId"), randomID(), time.Now().UTC()
	take := bson.M{"_id": eq(boardID), "operationId": eq(op), "$or": bson.A{bson.M{"leaseUntil": bson.M{"$exists": false}}, bson.M{"leaseUntil": bson.M{"$lt": now}}}}
	if v, ok := cp["owner"]; ok {
		take["owner"] = v
	} else {
		take["owner"] = bson.M{"$exists": false}
	}
	if r, err := pending.UpdateOne(ctx, take, bson.M{"$set": bson.M{"owner": token, "leaseUntil": now.Add(scrumImportLease)}}); err != nil || r.MatchedCount == 0 {
		return nil, importRecoveryErr("The import is still running; try again in a few minutes.")
	}
	_, _ = s.db.Collection("scrumImportSteps").DeleteMany(ctx, bson.M{"boardId": eq(boardID), "operationId": eq(op)})
	if r, err := pending.DeleteOne(ctx, bson.M{"_id": eq(boardID), "operationId": eq(op), "owner": eq(token), "state": "preparing"}); err != nil || r.DeletedCount == 0 {
		return nil, importRecoveryErr("The recovery checkpoint changed.")
	}
	s.hub.publish(boardID, "board-changed")
	return map[string]any{"changed": true, "scope": "scrum", "state": "discarded"}, nil
}

func (s *Server) importRecoveryMethods() map[string]methodFunc {
	run := func(discard bool) methodFunc {
		return func(r *http.Request, u *User, args []json.RawMessage) (any, error) {
			boardID, err := stringArg(args, 0)
			if err != nil {
				return nil, err
			}
			ctx := r.Context()
			if _, _, err := s.scrumBoard(ctx, u, boardID, true); err != nil {
				return nil, err
			}
			unlock := scrumLock(boardID)
			defer unlock()
			// The whole import is being discarded: its board goes.
			if s.db.Collection("importRuns").FindOne(ctx, bson.M{"boardId": eq(boardID), "state": "discarding"}).Err() == nil {
				return nil, importRecoveryErr("The import of this board is being discarded.")
			}
			if discard {
				return s.discardPreparingImport(ctx, boardID)
			}
			return s.resumeImport(ctx, boardID)
		}
	}
	return map[string]methodFunc{"scrum.resumeImport": run(false), "scrum.discardImport": run(true)}
}
