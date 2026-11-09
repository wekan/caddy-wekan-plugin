package web

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// The e-mail outbox's administrator controls, as Node.js WeKan's
// server/lib/emailOutboxControl.js, emailOutboxReport.js and
// server/methods/emailRecovery.js, in its collections and shapes:
//
//   - notificationEmailControls has a recipient's generation, paused, and
//     cancelBefore: the queue up to that time is cancelled, mail queued later
//     is kept;
//   - notificationEmailCommands has each request, so a reconnect that sends
//     an old cancel again cannot cancel mail queued after it, and a request a
//     newer one replaced answers "superseded" and changes nothing;
//   - both are changed only under the recipient's lease in
//     notificationEmailLeases - the one the outbox delivers under - so a
//     control never races a delivery: it answers busy, and is tried again.

var (
	errEmailControlInvalid  = errors.New("invalid-email-control")
	errEmailCommandMismatch = errors.New("email-control-identity-mismatch")
	errEmailGeneration      = errors.New("email-control-generation-mismatch")
)

// emailFailureReasons is WeKan's EMAIL_FAILURE_REASONS, in its order.
var emailFailureReasons = []string{"smtp-temporary", "smtp-rejected", "smtp-authentication", "smtp-configuration",
	"recipient-unavailable", "delivery-unconfirmed", "acknowledgement-failed", "delivery-failed", "retry-limit"}

var emailActions = []string{"pause", "resume", "cancel", "retry"}

// emailCommandIdentity is WeKan's commandIdentity: sha256 of
// JSON.stringify([userId, action, actorId]).
func emailCommandIdentity(userID, action, actorID string) string {
	return sha256Hex(canonical([]any{userID, action, actorID}))
}

// matchesEmailCommand is WeKan's: the same request, or its compacted receipt.
func matchesEmailCommand(row map[string]any, requestID, userID, action, actorID string) bool {
	if row == nil || row["_id"] != requestID {
		return false
	}
	if _, compact := row["compactReceiptVersion"]; !compact {
		return row["userId"] == userID && row["action"] == action && row["actorId"] == actorID
	}
	v, _ := safeInt(row["compactReceiptVersion"])
	status := strOf(row["status"])
	return v == 1 && (status == "completed" || status == "superseded") && row["identityHash"] == emailCommandIdentity(userID, action, actorID) && len(row) == 4
}

// controlEmailOutbox is WeKan's controlEmailOutbox.
func (s *Server) controlEmailOutbox(ctx context.Context, userID, action, requestID, actorID string, assertAdmin func() error) (map[string]any, error) {
	if userID == "" || len(userID) > 200 || !requestIDPattern.MatchString(requestID) || actorID == "" || assertAdmin == nil {
		return nil, errEmailControlInvalid
	}
	valid := false
	for _, a := range emailActions {
		valid = valid || a == action
	}
	if !valid {
		return nil, errEmailControlInvalid
	}
	var result map[string]any
	err := s.withLease(ctx, "notificationEmailLeases", userID, func() error {
		if err := assertAdmin(); err != nil {
			return err
		}
		commands, controls := s.db.Collection("notificationEmailCommands"), s.db.Collection("notificationEmailControls")
		read := func(coll, id string) map[string]any {
			var raw bson.M
			if s.db.Collection(coll).FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&raw) != nil {
				return nil
			}
			return plain(raw).(map[string]any)
		}
		command := read("notificationEmailCommands", requestID)
		if command != nil && !matchesEmailCommand(command, requestID, userID, action, actorID) {
			return errEmailCommandMismatch
		}
		if st := strOf(command["status"]); command != nil && (st == "completed" || st == "superseded") {
			result = map[string]any{"status": st}
			return nil
		}
		current := read("notificationEmailControls", userID)
		curGen, _ := safeInt(current["generation"])
		now := time.Now().UTC().Truncate(time.Millisecond)
		if command == nil {
			command = map[string]any{"_id": requestID, "userId": userID, "action": action, "actorId": actorID, "cutoff": now,
				"generation": curGen + 1, "status": "pending"}
			if _, err := commands.InsertOne(ctx, bson.M(command)); err != nil { // inserted, not a filter
				saved := read("notificationEmailCommands", requestID)
				if !matchesEmailCommand(saved, requestID, userID, action, actorID) {
					return err
				}
				command = saved
			}
		}
		gen, _ := safeInt(command["generation"])
		cutoff, _ := command["cutoff"].(time.Time)
		if cutoff.IsZero() {
			return errEmailGeneration
		}
		if err := assertAdmin(); err != nil {
			return err
		}
		finish := func(status string) error {
			_, _ = commands.UpdateOne(ctx, bson.M{"_id": eq(requestID), "status": "pending"}, bson.M{"$set": bson.M{"status": status, "finishedAt": time.Now().UTC()}})
			if saved := read("notificationEmailCommands", requestID); strOf(saved["status"]) != status {
				return errors.New("email-control-not-confirmed")
			}
			result = map[string]any{"status": status}
			return assertAdmin()
		}
		if curGen > gen || (current != nil && curGen == gen && current["lastRequestId"] != requestID) {
			return finish("superseded")
		}
		if curGen < gen {
			if curGen != gen-1 {
				return errEmailGeneration
			}
			set := bson.M{"generation": gen, "lastRequestId": requestID, "lastAction": action, "changedAt": cutoff, "changedBy": actorID}
			if action == "pause" || action == "resume" {
				set["paused"] = action == "pause"
			}
			if action == "cancel" {
				before, _ := current["cancelBefore"].(time.Time)
				if cutoff.After(before) {
					before = cutoff
				}
				set["cancelBefore"] = before
			}
			sel := bson.M{"_id": eq(userID), "generation": bson.M{"$exists": false}}
			if current != nil && current["generation"] != nil {
				sel["generation"] = current["generation"]
			}
			res, err := controls.UpdateOne(ctx, sel, bson.M{"$set": set, "$inc": bson.M{action + "Count": 1}})
			if err == nil && res.MatchedCount == 0 {
				if current == nil {
					doc := bson.M{"_id": userID, action + "Count": 1} // inserted, not a filter
					for k, v := range set {
						doc[k] = v
					}
					_, err = controls.InsertOne(ctx, doc)
				} else {
					err = errEmailGeneration
				}
			}
			if err != nil {
				if saved := read("notificationEmailControls", userID); saved == nil || saved["lastRequestId"] != requestID {
					return err
				}
			}
			current = read("notificationEmailControls", userID)
			curGen, _ = safeInt(current["generation"])
		}
		if curGen != gen || current["lastRequestId"] != requestID {
			return errEmailGeneration
		}
		if err := assertAdmin(); err != nil {
			return err
		}
		jobs := s.db.Collection("notificationEmailJobs")
		pending := bson.M{"userId": eq(userID), "state": "pending", "createdAt": bson.M{"$lte": cutoff}}
		switch action {
		case "cancel":
			pending["state"] = bson.M{"$in": bson.A{"pending", "failed"}}
			_, _ = jobs.UpdateMany(ctx, pending, bson.M{"$set": bson.M{"state": "cancelled", "finishedAt": time.Now().UTC()}, "$unset": emailPayloadFields})
			if n, _ := jobs.CountDocuments(ctx, pending); n > 0 {
				return errors.New("email-control-cancel-incomplete")
			}
		case "retry":
			failed := bson.M{"userId": eq(userID), "state": "failed", "createdAt": bson.M{"$lte": cutoff}, "retryRequestId": bson.M{"$ne": requestID}}
			_, _ = jobs.UpdateMany(ctx, failed, bson.M{"$set": bson.M{"state": "pending", "cycleAttempts": 0, "retryRequestId": requestID, "nextAttemptAt": cutoff},
				"$unset": bson.M{"failedAt": "", "lastFailure": "", "attemptId": ""}})
			if n, _ := jobs.CountDocuments(ctx, failed); n > 0 {
				return errors.New("email-control-retry-incomplete")
			}
		case "resume":
			_, _ = jobs.UpdateMany(ctx, pending, bson.M{"$set": bson.M{"nextAttemptAt": cutoff}})
			waiting := bson.M{"userId": eq(userID), "state": "pending", "createdAt": bson.M{"$lte": cutoff}, "nextAttemptAt": bson.M{"$gt": cutoff}}
			if n, _ := jobs.CountDocuments(ctx, waiting); n > 0 {
				return errors.New("email-control-resume-incomplete")
			}
		}
		return finish("completed")
	})
	return result, err
}

// emailPayloadFields is WeKan's PAYLOAD_FIELDS: what a cancelled job loses.
var emailPayloadFields = bson.M{"html": "", "text": "", "subject": "", "language": "", "cardId": "", "nextAttemptAt": "", "lastFailure": "", "failedAt": ""}

// emailOutboxReport is WeKan's: one row a recipient, ten a page, a paused
// recipient with nothing queued too - never a message, a subject or an address.
func (s *Server) emailOutboxReport(ctx context.Context, search string, page int64) (map[string]any, error) {
	match := bson.M{"state": bson.M{"$in": bson.A{"pending", "failed"}}}
	paused := bson.M{"paused": true}
	if q := strings.TrimSpace(search); q != "" {
		pattern := bson.M{"$regex": regexp.QuoteMeta(q), "$options": "i"}
		match["userId"], paused["_id"] = pattern, pattern
	}
	cur, err := s.db.Collection("notificationEmailJobs").Find(ctx, match,
		options.Find().SetProjection(bson.M{"userId": 1, "state": 1, "attempts": 1, "createdAt": 1, "nextAttemptAt": 1, "lastFailure": 1}))
	if err != nil {
		return nil, err
	}
	type row struct {
		UserID                   string
		Queued, Retrying, Failed int
		Attempts                 int64
		Oldest, Next             time.Time
		Reasons                  map[string]int
	}
	byUser := map[string]*row{}
	get := func(id string) *row {
		if byUser[id] == nil {
			byUser[id] = &row{UserID: id, Reasons: map[string]int{}}
		}
		return byUser[id]
	}
	for cur.Next(ctx) {
		var j struct {
			UserID      string    `bson:"userId"`
			State       string    `bson:"state"`
			Attempts    any       `bson:"attempts"`
			CreatedAt   time.Time `bson:"createdAt"`
			NextAttempt time.Time `bson:"nextAttemptAt"`
			LastFailure string    `bson:"lastFailure"`
		}
		if cur.Decode(&j) != nil || j.UserID == "" {
			continue
		}
		r := get(j.UserID)
		r.Queued++
		a, _ := safeInt(j.Attempts)
		r.Attempts = max(r.Attempts, a)
		if j.State == "failed" {
			r.Failed++
		} else if a > 0 {
			r.Retrying++
		}
		if !j.CreatedAt.IsZero() && (r.Oldest.IsZero() || j.CreatedAt.Before(r.Oldest)) {
			r.Oldest = j.CreatedAt
		}
		if !j.NextAttempt.IsZero() && (r.Next.IsZero() || j.NextAttempt.Before(r.Next)) {
			r.Next = j.NextAttempt
		}
		if j.LastFailure != "" {
			r.Reasons[j.LastFailure]++
		}
	}
	_ = cur.Close(ctx)
	pcur, err := s.db.Collection("notificationEmailControls").Find(ctx, paused, options.Find().SetProjection(bson.M{"_id": 1}))
	if err != nil {
		return nil, err
	}
	for pcur.Next(ctx) {
		var c struct {
			ID string `bson:"_id"`
		}
		if pcur.Decode(&c) == nil && c.ID != "" {
			get(c.ID)
		}
	}
	_ = pcur.Close(ctx)
	ids := make([]string, 0, len(byUser))
	for id := range byUser {
		ids = append(ids, id)
	}
	sort.Strings(ids) // byte order, as WeKan's Buffer.compare
	total := int64(len(ids))
	page = min(page, max(1, (total+9)/10))
	from, to := (page-1)*10, min(page*10, total)
	rows := []any{}
	for _, id := range ids[from:to] {
		r := byUser[id]
		var c struct {
			Paused     bool      `bson:"paused"`
			LastAction string    `bson:"lastAction"`
			ChangedAt  time.Time `bson:"changedAt"`
			ChangedBy  string    `bson:"changedBy"`
		}
		_ = s.db.Collection("notificationEmailControls").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&c)
		var u struct {
			Username string `bson:"username"`
		}
		_ = s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(id)}, options.FindOne().SetProjection(bson.M{"username": 1})).Decode(&u)
		failures := []any{}
		for _, reason := range emailFailureReasons {
			if n := r.Reasons[reason]; n > 0 {
				failures = append(failures, map[string]any{"reason": reason, "count": n})
			}
		}
		at := func(t time.Time) any {
			if t.IsZero() {
				return nil
			}
			return t
		}
		item := map[string]any{"userId": id, "username": u.Username, "queued": r.Queued, "retrying": r.Retrying, "failed": r.Failed,
			"failures": failures, "attempts": r.Attempts, "oldest": at(r.Oldest), "nextAttemptAt": at(r.Next), "paused": c.Paused,
			"lastAction": nil, "changedAt": at(c.ChangedAt), "changedBy": nil}
		if c.LastAction != "" {
			item["lastAction"] = c.LastAction
		}
		if c.ChangedBy != "" {
			item["changedBy"] = c.ChangedBy
		}
		rows = append(rows, item)
	}
	return map[string]any{"total": total, "page": page, "rows": rows}, nil
}

func (s *Server) emailRecoveryMethods() map[string]methodFunc {
	admin := func(r *http.Request, u *User) func() error {
		return func() error { return s.assertRecoveryAdmin(r.Context(), u.ID) }
	}
	return map[string]methodFunc{
		"emailRecoveryReport": func(r *http.Request, u *User, args []json.RawMessage) (any, error) {
			q, err := exactObject(args, 0, map[string]string{"search": "string", "page": "number"})
			if err != nil {
				return nil, err
			}
			if err := admin(r, u)(); err != nil {
				return nil, err
			}
			search, page := q["search"].(string), q["page"].(float64)
			if len(search) > 100 || page != math.Trunc(page) || page < 1 || page > 100000 {
				return nil, &methodError{"email-recovery-unavailable", "invalid-email-report"}
			}
			out, err := s.emailOutboxReport(r.Context(), search, int64(page))
			if err != nil {
				return nil, &methodError{"email-recovery-unavailable", "email-recovery-unavailable"}
			}
			return out, admin(r, u)()
		},
		"controlEmailRecovery": func(r *http.Request, u *User, args []json.RawMessage) (any, error) {
			q, err := exactObject(args, 0, map[string]string{"userId": "string", "action": "string", "requestId": "string"})
			if err != nil {
				return nil, err
			}
			if err := admin(r, u)(); err != nil {
				return nil, err
			}
			out, err := s.controlEmailOutbox(r.Context(), q["userId"].(string), q["action"].(string), q["requestId"].(string), u.ID, admin(r, u))
			if err != nil {
				var me *methodError
				if errors.As(err, &me) && me.Code == "not-authorized" {
					return nil, err
				}
				if errors.Is(err, errLeaseBusy) {
					return nil, &methodError{"email-recovery-busy", "email-recovery-busy"}
				}
				return nil, &methodError{"email-recovery-failed", "email-recovery-failed"}
			}
			return out, admin(r, u)()
		},
	}
}
