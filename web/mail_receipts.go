package web

import (
	"context"
	"os"
	"strconv"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// The e-mail outbox's receipts, as Node.js WeKan's
// server/lib/emailReceiptRetention.js: a sent or cancelled job, and a
// finished control request, keeps its metadata for EMAIL_RECEIPT_METADATA_DAYS
// (30) and then becomes a receipt in the same collection - its id, its end,
// and for a request the hash of who asked what. The id stays, so the same
// event is never queued again and an old request never replays; a receipt is
// never deleted or moved, which would open that race again.

func emailReceiptDays() int {
	if v, err := strconv.Atoi(os.Getenv("EMAIL_RECEIPT_METADATA_DAYS")); err == nil && v >= 1 && v <= 3650 {
		return v
	}
	return 30
}

func emailReceiptInterval() time.Duration {
	if v, err := strconv.Atoi(os.Getenv("EMAIL_RECEIPT_SWEEP_INTERVAL_MS")); err == nil && v >= 1000 && v <= 86400000 {
		return time.Duration(v) * time.Millisecond
	}
	return time.Minute
}

// receiptCursor is where a sweep goes on from: (finishedAt, _id).
type receiptCursor struct {
	at time.Time
	id string
}

// compactEmailReceipts compacts a hundred of one collection's finished rows,
// oldest first, moving past any it cannot read so they hide no others.
func (s *Server) compactEmailReceipts(ctx context.Context, jobs bool, cutoff time.Time, after *receiptCursor) (int, error) {
	coll, field, done := "notificationEmailCommands", "status", bson.A{"completed", "superseded"}
	identity := []string{"userId", "action", "actorId"}
	if jobs {
		coll, field, done, identity = "notificationEmailJobs", "state", bson.A{"sent", "cancelled"}, []string{"userId", "eventId"}
	}
	q := bson.M{field: bson.M{"$in": done}, "compactReceiptVersion": bson.M{"$exists": false},
		"finishedAt": bson.M{"$type": "date", "$lte": cutoff}}
	q["_id"] = bson.M{"$type": "string"}
	if after.id != "" {
		// The cursor is the last row this sweep read from the database.
		tie := bson.M{"finishedAt": after.at}
		tie["_id"] = bson.M{"$gt": after.id}
		q["$or"] = bson.A{bson.M{"finishedAt": bson.M{"$gt": after.at}}, tie}
	}
	proj := bson.M{"_id": 1, field: 1, "finishedAt": 1}
	for _, k := range identity {
		proj[k] = 1
	}
	cur, err := s.db.Collection(coll).Find(ctx, q, options.Find().SetProjection(proj).SetSort(bson.D{{Key: "finishedAt", Value: 1}, {Key: "_id", Value: 1}}).SetLimit(100))
	if err != nil {
		return 0, err
	}
	var rows []bson.M
	if err := cur.All(ctx, &rows); err != nil {
		return 0, err
	}
	confirmed := 0
	for _, raw := range rows {
		row := plain(raw).(map[string]any)
		id := strOf(row["_id"])
		at, _ := row["finishedAt"].(time.Time)
		*after = receiptCursor{at, id}
		receipt := bson.D{{Key: "_id", Value: id}, {Key: field, Value: row[field]}, {Key: "compactReceiptVersion", Value: int32(1)}}
		user := strOf(row["userId"])
		if jobs {
			if user == "" || strOf(row["eventId"]) == "" || id != emailJobID(user, strOf(row["eventId"])) {
				continue // not WeKan's shape: never touched
			}
		} else {
			action, actor := strOf(row["action"]), strOf(row["actorId"])
			valid := false
			for _, a := range emailActions {
				valid = valid || a == action
			}
			if !requestIDPattern.MatchString(id) || user == "" || actor == "" || !valid {
				continue
			}
			receipt = append(receipt, bson.E{Key: "identityHash", Value: emailCommandIdentity(user, action, actor)})
		}
		sel := bson.M{"_id": eq(id), field: row[field], "finishedAt": at, "compactReceiptVersion": bson.M{"$exists": false}}
		for _, k := range identity {
			sel[k] = row[k]
		}
		if res, err := s.db.Collection(coll).ReplaceOne(ctx, sel, receipt); err == nil && res.MatchedCount == 1 {
			confirmed++
		}
	}
	if len(rows) < 100 {
		*after = receiptCursor{}
	}
	return confirmed, nil
}

// runEmailReceipts sweeps both collections every EMAIL_RECEIPT_SWEEP_INTERVAL_MS.
func (s *Server) runEmailReceipts(stop <-chan struct{}) {
	t := time.NewTicker(emailReceiptInterval())
	defer t.Stop()
	var jobs, commands receiptCursor
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			cutoff := time.Now().UTC().Add(-time.Duration(emailReceiptDays()) * 24 * time.Hour)
			_, _ = s.compactEmailReceipts(ctx, true, cutoff, &jobs)
			_, _ = s.compactEmailReceipts(ctx, false, cutoff, &commands)
			cancel()
		}
	}
}
