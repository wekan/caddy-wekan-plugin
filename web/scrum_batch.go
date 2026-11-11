package web

// A Scrum change recorded over several History rows of one batch - a large
// close and its rollover - is undone and redone whole, as WeKan's
// server/models/changeHistory.js and server/lib/scrumBatchJobs.js: the rows
// after the first follow in the same call when a few are left, else in a
// background job (scrumBatchJobs, one per board) the Scrum view shows the
// progress of. While one runs, nothing else is undone or redone on the
// board; a job that stopped is resumed by pressing undo or redo again. Under
// a request id each further row has an id derived from it, so a retry
// resumes the same rows.

import (
	"context"
	"net/http"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	inlineBatchRows = 4
	batchJobLease   = time.Minute
)

var batchJobs sync.Map

func batchRequestID(requestID string, index int) string {
	return "batch-" + sha256Hex(canonical([]any{requestID, index}))[:48]
}

func isBatchPart(row map[string]any) bool {
	return row != nil && docString(row, "entityType") == "scrum" && docString(row, "batchId") != "" && row["isCheckpoint"] != true
}

// nextBatchRow is the batch's next row in the direction.
func (s *Server) nextBatchRow(ctx context.Context, userID, boardID, direction, batchID string) map[string]any {
	sortBy := "createdAt"
	if direction == "redo" {
		sortBy = "undoneAt"
	}
	var raw bson.M
	if s.db.Collection("changeHistory").FindOne(ctx, bson.M{"userId": eq(userID), "boardId": eq(boardID), "batchId": eq(batchID), "entityType": "scrum",
		"undone": direction == "redo", "isCheckpoint": bson.M{"$ne": true}, "superseded": bson.M{"$ne": true}},
		options.FindOne().SetSort(bson.D{{Key: sortBy, Value: -1}})).Decode(&raw) != nil {
		return nil
	}
	return plain(raw).(map[string]any)
}

func (s *Server) remainingBatchRows(ctx context.Context, userID, boardID, direction, batchID string) int64 {
	n, _ := s.db.Collection("changeHistory").CountDocuments(ctx, bson.M{"userId": eq(userID), "boardId": eq(boardID), "batchId": eq(batchID), "entityType": "scrum",
		"undone": direction == "redo", "isCheckpoint": bson.M{"$ne": true}, "superseded": bson.M{"$ne": true}})
	return n
}

// assertNoBatchJob is WeKan's: a job on the board keeps every other undo
// and redo out - and is started again here when nothing is running it.
func (s *Server) assertNoBatchJob(ctx context.Context, boardID string) error {
	if s.findDoc(ctx, "scrumBatchJobs", bson.M{"_id": eq(boardID)}) == nil {
		return nil
	}
	s.runBatchJob(boardID)
	return &methodError{"scrum-history-running", "A large undo or redo is still running on this board."}
}

// continueBatch is the rest of a batch after its first row: in the call when
// a few rows are left, else as a job. result gains continuing then.
func (s *Server) continueBatch(r *http.Request, u *User, row map[string]any, direction, requestID string, result map[string]any) error {
	if !isBatchPart(row) {
		return nil
	}
	ctx := r.Context()
	boardID, batchID := docString(row, "boardId"), docString(row, "batchId")
	key := map[string]string{"undo": "undone", "redo": "redone"}[direction]
	if left := s.remainingBatchRows(ctx, u.ID, boardID, direction, batchID); left > inlineBatchRows {
		var req any
		if requestID != "" {
			req = requestID
		}
		if _, err := s.db.Collection("scrumBatchJobs").InsertOne(ctx, bson.M{"_id": boardID, "boardId": boardID, "userId": u.ID, "direction": direction, // inserted, not a filter
			"batchId": batchID, "requestId": req, "index": int32(1), "done": int32(1), "total": int32(left + 1), "state": "running", "startedAt": time.Now().UTC()}); err != nil {
			return err
		}
		s.runBatchJob(boardID)
		result["continuing"] = true
		return nil
	}
	for index := 1; ; index++ {
		next, err := s.batchStep(r, u, boardID, direction, batchID, requestID, index)
		if err != nil {
			return err
		}
		if next == nil || next[key] != true {
			return nil
		}
	}
}

// batchStep applies the batch's next row: under its derived request id when
// the batch has one.
func (s *Server) batchStep(r *http.Request, u *User, boardID, direction, batchID, requestID string, index int) (map[string]any, error) {
	key := map[string]string{"undo": "undone", "redo": "redone"}[direction]
	if requestID != "" {
		return s.requestedScrumRow(r, u, boardID, direction, batchRequestID(requestID, index), batchID)
	}
	ctx := r.Context()
	next := s.nextBatchRow(ctx, u.ID, boardID, direction, batchID)
	if next == nil {
		return map[string]any{key: false}, nil
	}
	if err := s.requireHistoryIntegrity(r, u, next); err != nil {
		return nil, err
	}
	if err := s.applyScrumHistoryJournaled(ctx, u, next, direction, nil); err != nil {
		return nil, err
	}
	return map[string]any{key: true}, nil
}

func (s *Server) claimBatchJob(ctx context.Context, boardID string) bool {
	now := time.Now().UTC()
	res, err := s.db.Collection("scrumBatchJobs").UpdateOne(ctx, bson.M{"_id": eq(boardID), "$or": bson.A{bson.M{"lease": bson.M{"$exists": false}},
		bson.M{"lease.owner": eq(scrumServerID)}, bson.M{"lease.until": bson.M{"$lt": now}}}},
		bson.M{"$set": bson.M{"lease": bson.M{"owner": scrumServerID, "until": now.Add(batchJobLease)}}})
	return err == nil && res.MatchedCount == 1
}

// runBatchJob runs the board's job, once, as the user who asked.
func (s *Server) runBatchJob(boardID string) {
	if _, running := batchJobs.LoadOrStore(boardID, true); running {
		return
	}
	go func() {
		defer batchJobs.Delete(boardID)
		ctx := context.Background()
		r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/", nil)
		for {
			job := s.findDoc(ctx, "scrumBatchJobs", bson.M{"_id": eq(boardID)})
			if job == nil || !s.claimBatchJob(ctx, boardID) {
				return
			}
			var u User
			if s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(docString(job, "userId"))}).Decode(&u) != nil {
				s.failBatchJob(ctx, boardID, "The user of this undo or redo is gone.")
				return
			}
			direction := docString(job, "direction")
			next, err := s.batchStep(r, &u, boardID, direction, docString(job, "batchId"), docString(job, "requestId"), int(numberOf(job["index"])))
			if err != nil {
				reason := err.Error()
				if me, ok := err.(*methodError); ok {
					reason = me.Reason
				}
				s.failBatchJob(ctx, boardID, reason)
				return
			}
			if next == nil || next[map[string]string{"undo": "undone", "redo": "redone"}[direction]] != true {
				_, _ = s.db.Collection("scrumBatchJobs").DeleteOne(ctx, bson.M{"_id": eq(boardID)})
				s.hub.publish(boardID, "board-changed")
				return
			}
			_, _ = s.db.Collection("scrumBatchJobs").UpdateOne(ctx, bson.M{"_id": eq(boardID)}, bson.M{"$inc": bson.M{"done": int32(1), "index": int32(1)},
				"$set": bson.M{"state": "running"}, "$unset": bson.M{"error": ""}})
		}
	}()
}

func (s *Server) failBatchJob(ctx context.Context, boardID, reason string) {
	if len(reason) > 500 {
		reason = reason[:500]
	}
	_, _ = s.db.Collection("scrumBatchJobs").UpdateOne(ctx, bson.M{"_id": eq(boardID)}, bson.M{"$set": bson.M{"state": "failed", "error": reason},
		"$unset": bson.M{"lease": ""}})
	s.hub.publish(boardID, "board-changed")
}

// resumeBatchJobs: the jobs a restart interrupted continue.
func (s *Server) resumeBatchJobs() {
	for _, j := range s.findDocs(context.Background(), "scrumBatchJobs", bson.M{}, options.Find().SetProjection(bson.M{"_id": 1})) {
		s.runBatchJob(docString(j, "_id"))
	}
}
