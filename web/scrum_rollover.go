package web

// A large sprint's close, as WeKan's server/scrum.js startRollover and
// runRollover (maintainer decision of 2026-10-03: sprints with no card cap):
// a close over one chunk of cards returns at once, and its cards roll over in
// the background, a chunk at a time under the board's lock, each chunk in the
// close's History batch so one undo walks the whole close. One server at a
// time runs a sprint's rollover - a lease it renews each step, which another
// takes over once it runs out - and a restart resumes it. A failure is kept
// on the sprint for the Scrum view, and closing again retries it.

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const scrumRolloverLease = time.Minute

var (
	scrumServerID = randomID()
	rollovers     sync.Map
)

// closeBatchID is closeBatchId: the History batch of one close.
func closeBatchID(sprintID string, revision any) string {
	return fmt.Sprintf("scrum-close-%s-%s", sprintID, scrumNum(revision))
}

// smallRollover: a plan kept in the sprint, or one chunk, rolls over in the
// close's own call.
func smallRollover(sp map[string]any) bool {
	_, list := sp["rolloverPending"].([]any)
	return list || numberOf(sp["rolloverTotal"]) <= scrumSnapshotChunk
}

func (s *Server) claimRollover(ctx context.Context, sprintID string) bool {
	now := time.Now().UTC()
	res, err := s.db.Collection("scrumSprints").UpdateOne(ctx, bson.M{"_id": eq(sprintID), "$or": bson.A{bson.M{"rolloverLease": bson.M{"$exists": false}},
		bson.M{"rolloverLease.owner": eq(scrumServerID)}, bson.M{"rolloverLease.until": bson.M{"$lt": now}}}},
		bson.M{"$set": bson.M{"rolloverLease": bson.M{"owner": scrumServerID, "until": now.Add(scrumRolloverLease)}}})
	return err == nil && res.MatchedCount == 1
}

// startRollover runs a sprint's rollover in the background, once.
func (s *Server) startRollover(sprintID string) {
	if _, running := rollovers.LoadOrStore(sprintID, true); running {
		return
	}
	go func() {
		defer rollovers.Delete(sprintID)
		ctx := context.Background()
		if err := s.runRolloverJob(ctx, sprintID); err != nil {
			reason := err.Error()
			if me, ok := err.(*methodError); ok {
				reason = me.Reason
			}
			if len(reason) > 500 {
				reason = reason[:500]
			}
			_, _ = s.db.Collection("scrumSprints").UpdateOne(ctx, bson.M{"_id": eq(sprintID), "rolloverPending": bson.M{"$exists": true}},
				bson.M{"$set": bson.M{"rolloverError": reason}})
		}
	}()
}

func (s *Server) runRolloverJob(ctx context.Context, sprintID string) error {
	if !s.claimRollover(ctx, sprintID) {
		return nil
	}
	_, _ = s.db.Collection("scrumSprints").UpdateOne(ctx, bson.M{"_id": eq(sprintID)}, bson.M{"$unset": bson.M{"rolloverError": ""}})
	for {
		sp := s.findDoc(ctx, "scrumSprints", bson.M{"_id": eq(sprintID)})
		if sp == nil || sp["rolloverPending"] != true || !s.claimRollover(ctx, sprintID) {
			return nil
		}
		done, err := s.rolloverStep(ctx, sp)
		if err != nil || done {
			return err
		}
	}
}

// rolloverStep is one chunk applied, or the plan finished; true when
// nothing is left.
func (s *Server) rolloverStep(ctx context.Context, sp map[string]any) (bool, error) {
	boardID, sprintID := docString(sp, "boardId"), docString(sp, "_id")
	unlock := scrumLock(boardID)
	defer unlock()
	op := &scrumOp{s: s, ctx: ctx, u: &User{ID: docString(sp, "updatedBy")}, boardID: boardID, batchID: closeBatchID(sprintID, sp["closedFromRevision"])}
	var chunk bson.M
	if s.db.Collection("scrumRolloverRows").FindOne(ctx, bson.M{"sprintId": eq(sprintID)}, options.FindOne().SetSort(bson.D{{Key: "chunk", Value: 1}})).Decode(&chunk) != nil {
		_, _ = s.db.Collection("scrumSprints").UpdateOne(ctx, bson.M{"_id": eq(sprintID), "rolloverPending": true},
			bson.M{"$unset": bson.M{"rolloverPending": "", "rolloverTotal": "", "rolloverDone": "", "rolloverError": "", "rolloverLease": ""}})
		s.hub.publish(boardID, "board-changed")
		return true, nil
	}
	c := plain(chunk).(map[string]any)
	if docString(c, "boardId") != boardID {
		return false, scrumConflict()
	}
	rows, _ := c["rows"].([]any)
	for _, x := range rows {
		row, _ := x.(map[string]any)
		if err := s.applyScrumRolloverRow(op, boardID, row); err != nil {
			op.flush()
			return false, err
		}
	}
	op.flush()
	_, _ = s.db.Collection("scrumRolloverRows").DeleteOne(ctx, bson.M{"_id": eq(docString(c, "_id"))})
	_, _ = s.db.Collection("scrumSprints").UpdateOne(ctx, bson.M{"_id": eq(sprintID)}, bson.M{"$inc": bson.M{"rolloverDone": int32(len(rows))}})
	s.hub.publish(boardID, "board-changed")
	return false, nil
}

// resumeRollovers: the rollovers a restart interrupted continue.
func (s *Server) resumeRollovers() {
	for _, sp := range s.findDocs(context.Background(), "scrumSprints", bson.M{"rolloverPending": true}, options.Find().SetProjection(bson.M{"_id": 1})) {
		s.startRollover(docString(sp, "_id"))
	}
}
