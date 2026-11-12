package web

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// The administrator's controls of activity notifications, as Node.js WeKan's
// #6768 (server/lib/activityNotificationControl.js, activityNotificationReport.js,
// activityNotificationCancellationRetention.js, activityNotificationExpiry.js
// and server/methods/activityNotificationRecovery.js), in its collections and
// shapes, so a pause or a cancellation made on either server holds on both:
//
//   - activityNotificationControls has one row an intent once an administrator
//     touched it: its revision, paused, and cancelled for good. A request names
//     the revision it saw; each change advances it once, so a delayed old
//     request cannot undo a newer decision. The row is never deleted.
//   - a cancelled intent becomes a receipt without its activity, and its plan a
//     tombstone without its recipients, so nothing is delivered later.
//   - a notification that can never be delivered - its activity gone, its
//     recipient refused - is given up after ten minutes, and any pending one
//     after ACTIVITY_NOTIFICATION_MAX_AGE_DAYS (7), through the same cancellation.
//   - Admin Panel / Problems / Recovery lists them ten a page, with no activity
//     content and no recipient, and pauses, resumes, cancels and retries them.

var (
	errControlInvalid      = errors.New("activity-notification-control-invalid")
	errControlConflict     = errors.New("activity-notification-control-conflict")
	errControlNotPending   = errors.New("activity-notification-control-not-pending")
	errControlUnconfirmed  = errors.New("activity-notification-control-unconfirmed")
	errActivityCancelled   = errors.New("activity-notification-cancelled")
	errActivityPaused      = errors.New("activity-notification-paused")
	errRecipientDenied     = errors.New("activity-notification-recipient-denied")
	errPreferenceChanged   = errors.New("activity-notification-preference-changed")
	errNotificationsOff    = errors.New("activity-notifications-disabled")
	errCancellationInvalid = errors.New("activity-cancellation-cleanup-invalid")
	errIntentInvalid       = errors.New("activity-notification-intent-invalid")

	hashPattern      = regexp.MustCompile(`^[a-f0-9]{64}$`)
	requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{20,64}$`)
)

// activityControl is a validated activityNotificationControls row; a missing
// row is revision 0, not paused.
type activityControl struct {
	Revision           int64
	Paused, Cancelled  bool
	RequestID, ActorID string
}

func controlText(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok && s != "" && len(s) <= 1024
}

// safeInt is a whole number JavaScript can hold exactly, whichever way it was
// written: int32 and double by WeKan, int64 by Weddy.
func safeInt(v any) (int64, bool) {
	switch x := v.(type) {
	case int32:
		return int64(x), true
	case int64:
		return x, x >= -(1<<53-1) && x <= 1<<53-1
	case float64:
		if x != math.Trunc(x) || math.Abs(x) > 1<<53-1 {
			return 0, false
		}
		return int64(x), true
	}
	return 0, false
}

func (s *Server) readActivityControl(ctx context.Context, intentID string) (activityControl, error) {
	if !hashPattern.MatchString(intentID) {
		return activityControl{}, errControlInvalid
	}
	var raw bson.M
	if err := s.db.Collection("activityNotificationControls").FindOne(ctx, bson.M{"_id": eq(intentID)}).Decode(&raw); err != nil {
		return activityControl{}, nil
	}
	row := plain(raw).(map[string]any)
	want := map[string]bool{"_id": true, "actorId": true, "changedAt": true, "paused": true, "requestId": true, "revision": true}
	if row["cancelled"] != nil {
		want["cancelled"] = true
	}
	c := activityControl{}
	var ok bool
	rev, revOK := safeInt(row["revision"])
	c.Revision = rev
	c.Paused, ok = row["paused"].(bool)
	c.RequestID, _ = row["requestId"].(string)
	actor, actorOK := controlText(row["actorId"])
	c.ActorID = actor
	at, atOK := row["changedAt"].(time.Time)
	if _, has := row["cancelled"]; has {
		c.Cancelled = row["cancelled"] == true
		if !c.Cancelled || !c.Paused {
			return activityControl{}, errControlInvalid
		}
	}
	if row["_id"] != intentID || !revOK || rev < 1 || !ok || !requestIDPattern.MatchString(c.RequestID) || !actorOK || !atOK || at.IsZero() || len(row) != len(want) {
		return activityControl{}, errControlInvalid
	}
	for k := range row {
		if !want[k] {
			return activityControl{}, errControlInvalid
		}
	}
	return c, nil
}

// assertActivityUnpaused is WeKan's assertActivityNotificationUnpaused.
func (s *Server) assertActivityUnpaused(ctx context.Context, intentID string) error {
	c, err := s.readActivityControl(ctx, intentID)
	switch {
	case err != nil:
		return err
	case c.Cancelled:
		return errActivityCancelled
	case c.Paused:
		return errActivityPaused
	}
	return nil
}

// mutateActivityControl is WeKan's mutateActivityNotificationControl: under
// the intent's lease, the revision the administrator saw becomes the next one.
// The same request again answers what it did, and changes nothing.
func (s *Server) mutateActivityControl(ctx context.Context, intentID string, paused, cancelled bool, expected int64,
	requestID, actorID string, assertAdmin func() error) (map[string]any, error) {
	if _, ok := controlText(actorID); !hashPattern.MatchString(intentID) || expected < 0 || expected >= 1<<53-1 ||
		!requestIDPattern.MatchString(requestID) || !ok || assertAdmin == nil {
		return nil, errControlInvalid
	}
	if err := assertAdmin(); err != nil {
		return nil, err
	}
	answer := func(rev int64, paused bool) map[string]any {
		out := map[string]any{"revision": rev, "paused": paused}
		if cancelled {
			out["cancelled"] = true
		}
		return out
	}
	var result map[string]any
	err := s.withActivityLease(ctx, intentID, func() error {
		if err := assertAdmin(); err != nil {
			return err
		}
		cur, err := s.readActivityControl(ctx, intentID)
		if err != nil {
			return err
		}
		if cur.Revision == expected+1 && cur.RequestID == requestID && cur.ActorID == actorID && cur.Paused == paused && cur.Cancelled == cancelled {
			result = answer(cur.Revision, cur.Paused)
			return nil
		}
		if cur.Cancelled {
			return errActivityCancelled
		}
		if cur.Revision != expected || cur.RequestID == requestID {
			return errControlConflict
		}
		var intent struct {
			State string `bson:"state"`
		}
		if s.db.Collection("activityNotificationIntents").FindOne(ctx, bson.M{"_id": eq(intentID)}).Decode(&intent) != nil || intent.State != "pending" {
			return errControlNotPending
		}
		next := bson.D{{Key: "_id", Value: intentID}, {Key: "revision", Value: expected + 1}, {Key: "paused", Value: paused},
			{Key: "requestId", Value: requestID}, {Key: "actorId", Value: actorID}, {Key: "changedAt", Value: time.Now().UTC().Truncate(time.Millisecond)}}
		if cancelled {
			next = append(next, bson.E{Key: "cancelled", Value: true})
		}
		if err := assertAdmin(); err != nil {
			return err
		}
		controls := s.db.Collection("activityNotificationControls")
		var werr error
		if expected == 0 {
			_, werr = controls.InsertOne(ctx, next) // inserted, not a filter
		} else {
			_, werr = controls.ReplaceOne(ctx, bson.M{"_id": eq(intentID), "revision": expected}, next)
		}
		saved, rerr := s.readActivityControl(ctx, intentID)
		if rerr != nil || saved.Revision != expected+1 || saved.RequestID != requestID || saved.Paused != paused || saved.Cancelled != cancelled {
			if werr != nil {
				return werr
			}
			return errControlUnconfirmed
		}
		result = answer(saved.Revision, saved.Paused)
		return assertAdmin()
	})
	return result, err
}

// compactCancelledActivity is WeKan's compactCancelledActivityNotification;
// the caller holds the intent's lease. The plan becomes a tombstone - even a
// plan never made, so a late first writer cannot make one - and then the
// intent a receipt without its activity.
func (s *Server) compactCancelledActivity(ctx context.Context, intentID string) (string, error) {
	control, err := s.readActivityControl(ctx, intentID)
	if err != nil {
		return "", err
	}
	if !control.Cancelled {
		return "pending", nil
	}
	intents, plans := s.db.Collection("activityNotificationIntents"), s.db.Collection("activityNotificationPlans")
	var raw bson.M
	if intents.FindOne(ctx, bson.M{"_id": eq(intentID)}).Decode(&raw) != nil {
		return "missing", nil
	}
	original := plain(raw).(map[string]any)
	var activityID string
	var receipt bson.D
	switch original["state"] {
	case "cancelled":
		activityID = strOf(original["activityId"])
		if intentIDFor(activityID) != intentID {
			return "", errCancellationInvalid
		}
	case "pending":
		activity, _ := original["activity"].(map[string]any)
		activityID = strOf(activity["_id"])
		createdAt, ok := activity["createdAt"].(time.Time)
		if activityID == "" || intentIDFor(activityID) != intentID || !ok || sha256Hex(canonical(activity)) != strOf(original["activityHash"]) {
			return "", errCancellationInvalid
		}
		orNull := func(k string) any {
			if v, ok := controlText(activity[k]); ok {
				return v
			}
			return nil
		}
		receipt = bson.D{{Key: "_id", Value: intentID}, {Key: "activityHash", Value: original["activityHash"]}, {Key: "activityId", Value: activityID},
			{Key: "boardId", Value: orNull("boardId")}, {Key: "cardId", Value: orNull("cardId")}, {Key: "createdAt", Value: createdAt},
			{Key: "dispatchUserId", Value: original["dispatchUserId"]}, {Key: "state", Value: "cancelled"}, {Key: "version", Value: int32(1)},
			{Key: "writerId", Value: original["writerId"]}}
	default:
		return "retained", nil
	}
	pid := planIDFor(activityID)
	var planRaw bson.M
	hasPlan := plans.FindOne(ctx, bson.M{"_id": eq(pid)}).Decode(&planRaw) == nil
	if !hasPlan || planRaw["cancelled"] != true {
		var checksum any
		if hasPlan {
			plan := plain(planRaw).(map[string]any)
			body, _ := plan["plan"].(map[string]any)
			if body == nil || len(plan) != 3 || strOf(plan["checksum"]) != sha256Hex(canonical(body)) || strOf(body["activityId"]) != activityID {
				return "", errCancellationInvalid
			}
			checksum = plan["checksum"]
		}
		tomb := bson.D{{Key: "_id", Value: pid}, {Key: "activityHash", Value: original["activityHash"]}, {Key: "checksum", Value: checksum},
			{Key: "compactReceiptVersion", Value: int32(1)}, {Key: "cancelled", Value: true}}
		if hasPlan {
			_, err = plans.ReplaceOne(ctx, bson.M{"_id": eq(pid), "checksum": planRaw["checksum"]}, tomb)
		} else {
			_, err = plans.InsertOne(ctx, tomb) // inserted, not a filter
		}
		if err != nil {
			return "", err
		}
	}
	if receipt != nil {
		if _, err := intents.ReplaceOne(ctx, bson.M{"_id": eq(intentID), "state": "pending"}, receipt); err != nil {
			return "", err
		}
	}
	return "compacted", nil
}

// cleanupCancelledActivity compacts a cancelled intent under its lease.
func (s *Server) cleanupCancelledActivity(ctx context.Context, intentID string) error {
	return s.withActivityLease(ctx, intentID, func() error {
		_, err := s.compactCancelledActivity(ctx, intentID)
		return err
	})
}

// giveUpActivity cancels a notification that cannot be delivered, or has
// expired, as the system - the cancellation an administrator uses.
func (s *Server) giveUpActivity(ctx context.Context, intentID, reason string) error {
	cur, err := s.readActivityControl(ctx, intentID)
	if err != nil {
		return err
	}
	if !cur.Cancelled {
		if reason == "" {
			reason = "expired"
		}
		if len(reason) > 100 {
			reason = reason[:100]
		}
		if _, err := s.mutateActivityControl(ctx, intentID, true, true, cur.Revision, randomString(idAlphabet, 32),
			"system:"+reason, func() error { return nil }); err != nil {
			return err
		}
	}
	return s.cleanupCancelledActivity(ctx, intentID)
}

// Recovery errors after which a notification can never be delivered.
var terminalRecoveryErrors = []error{errOrphanActivity, errRecipientDenied, errPreferenceChanged, errIntentInvalid}

const activityGrace = 10 * time.Minute

func activityMaxAge() time.Duration {
	if n, err := strconv.ParseFloat(os.Getenv("ACTIVITY_NOTIFICATION_MAX_AGE_DAYS"), 64); err == nil && n >= 1 && n <= 3650 {
		return time.Duration(n * float64(24*time.Hour))
	}
	return 7 * 24 * time.Hour
}

// recoveryTotals is what a recovery pass did.
type recoveryTotals struct{ Visited, Completed, Skipped, Failed, Busy, Cancelled int }

func (t recoveryTotals) add(o recoveryTotals) recoveryTotals {
	return recoveryTotals{t.Visited + o.Visited, t.Completed + o.Completed, t.Skipped + o.Skipped, t.Failed + o.Failed, t.Busy + o.Busy, t.Cancelled + o.Cancelled}
}

// recoverOne resumes one pending intent, giving it up when it expired, or
// when it can never finish and is past the grace period.
func (s *Server) recoverOne(ctx context.Context, id string, createdAt time.Time, now time.Time, t *recoveryTotals) {
	if !createdAt.IsZero() && now.Sub(createdAt) >= activityMaxAge() {
		if s.giveUpActivity(ctx, id, "expired") == nil {
			t.Cancelled++
		} else {
			t.Failed++
		}
		return
	}
	status, err := s.deliverIntentStatus(ctx, id)
	switch {
	case err == nil && status == "completed":
		t.Completed++
	case err == nil:
		t.Skipped++
	case errors.Is(err, errLeaseBusy):
		t.Busy++
	case errors.Is(err, errActivityPaused), errors.Is(err, errActivityCancelled):
		t.Skipped++
	default:
		terminal := false
		for _, e := range terminalRecoveryErrors {
			terminal = terminal || errors.Is(err, e)
		}
		if terminal && !createdAt.IsZero() && now.Sub(createdAt) >= activityGrace {
			if gerr := s.giveUpActivity(ctx, id, err.Error()); gerr == nil {
				t.Cancelled++
				return
			}
		}
		t.Failed++
		if !terminal {
			s.log.Warn("activity notification not delivered", "reason", err.Error())
		}
	}
}

// The report and its methods.

// activityReport is WeKan's activityNotificationReport: ten summaries a page,
// never the activity's content, its recipients or their mail.
func (s *Server) activityReport(ctx context.Context, search string, page int64) (map[string]any, error) {
	intents := s.db.Collection("activityNotificationIntents")
	query := bson.M{"state": bson.M{"$in": bson.A{"pending", "cancelled"}}}
	if q := strings.TrimSpace(search); q != "" {
		pattern := bson.M{"$regex": regexp.QuoteMeta(q), "$options": "i"}
		var or bson.A
		for _, k := range []string{"_id", "activity._id", "activity.boardId", "activity.cardId", "activityId", "boardId", "cardId"} {
			or = append(or, bson.M{k: pattern})
		}
		query["$or"] = or
	}
	total, err := intents.CountDocuments(ctx, query)
	if err != nil {
		return nil, err
	}
	page = min(page, max(1, (total+9)/10))
	cur, err := intents.Find(ctx, query, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}).SetSkip((page-1)*10).SetLimit(10).
		SetProjection(bson.M{"_id": 1, "version": 1, "state": 1, "activityHash": 1, "writerId": 1, "dispatchUserId": 1, "activityId": 1,
			"boardId": 1, "cardId": 1, "createdAt": 1, "activity._id": 1, "activity.createdAt": 1, "activity.boardId": 1, "activity.cardId": 1}))
	if err != nil {
		return nil, err
	}
	var found []bson.M
	if err := cur.All(ctx, &found); err != nil {
		return nil, err
	}
	rows := []any{}
	now := time.Now()
	for _, raw := range found {
		row := plain(raw).(map[string]any)
		summary, _ := row["activity"].(map[string]any)
		if row["state"] == "cancelled" {
			summary = map[string]any{"_id": row["activityId"], "boardId": row["boardId"], "cardId": row["cardId"], "createdAt": row["createdAt"]}
		}
		text := func(k string) string { v, _ := controlText(summary[k]); return v }
		intentID := strOf(row["_id"])
		if !hashPattern.MatchString(intentID) {
			intentID = ""
		}
		activityID := text("_id")
		item := map[string]any{"intentId": intentID, "activityId": activityID, "boardId": text("boardId"), "cardId": text("cardId"),
			"createdAt": nil, "status": "invalid", "canRetry": false, "paused": false, "controlRevision": nil, "canControl": false}
		if at, ok := summary["createdAt"].(time.Time); ok {
			item["createdAt"] = at
		}
		if row["state"] == "cancelled" {
			if c, err := s.readActivityControl(ctx, strOf(row["_id"])); err == nil && c.Cancelled && intentIDFor(activityID) == intentID {
				item["status"], item["paused"], item["controlRevision"] = "cancelled", true, c.Revision
			}
			rows = append(rows, item)
			continue
		}
		version, _ := safeInt(row["version"])
		_, writerOK := controlText(row["writerId"])
		_, dispatchOK := controlText(row["dispatchUserId"])
		valid := intentID != "" && activityID != "" && version == 1 && hashPattern.MatchString(strOf(row["activityHash"])) && writerOK &&
			(row["dispatchUserId"] == nil || dispatchOK) && intentID == intentIDFor(activityID)
		if valid {
			var lease struct {
				ExpiresAt time.Time `bson:"expiresAt"`
			}
			if s.db.Collection("activityNotificationLeases").FindOne(ctx, bson.M{"_id": eq(intentID)}).Decode(&lease) == nil && lease.ExpiresAt.After(now) {
				item["status"] = "processing"
			} else {
				var activity bson.M
				var plan bson.M
				switch {
				case s.db.Collection("activities").FindOne(ctx, bson.M{"_id": eq(activityID)}).Decode(&activity) != nil:
					item["status"] = "missing"
				case sha256Hex(canonical(plain(activity))) != strOf(row["activityHash"]):
					item["status"] = "changed"
				case s.db.Collection("activityNotificationPlans").FindOne(ctx, bson.M{"_id": eq(planIDFor(activityID))}).Decode(&plan) != nil:
					item["status"] = "preparing"
				default:
					p := plain(plan).(map[string]any)
					body, _ := p["plan"].(map[string]any)
					v, _ := safeInt(body["version"])
					if p["compactReceiptVersion"] != nil || !hashPattern.MatchString(strOf(p["checksum"])) || v != 1 ||
						strOf(body["activityId"]) != activityID || body["activityHash"] != row["activityHash"] || body["dispatchUserId"] != row["dispatchUserId"] {
						item["status"] = "inconsistent"
					} else {
						item["status"] = "pending"
					}
				}
			}
			if c, err := s.readActivityControl(ctx, intentID); err != nil {
				item["status"] = "invalid"
			} else {
				item["paused"], item["controlRevision"] = c.Paused, c.Revision
				if c.Cancelled {
					item["status"] = "cancelled"
				}
				item["canControl"] = !c.Cancelled && item["status"] != "processing" && c.Revision < 1<<53-1
			}
			status := item["status"]
			item["canRetry"] = item["controlRevision"] != nil && item["paused"] == false && (status == "pending" || status == "preparing")
		}
		rows = append(rows, item)
	}
	return map[string]any{"total": total, "page": page, "rows": rows}, nil
}

// assertRecoveryAdmin is WeKan's assertAdmin of these methods: read again
// each time, so an administrator demoted mid-way stops there.
func (s *Server) assertRecoveryAdmin(ctx context.Context, userID string) error {
	var u struct {
		IsAdmin       bool `bson:"isAdmin"`
		LoginDisabled bool `bson:"loginDisabled"`
	}
	if s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(userID)}).Decode(&u) != nil || !u.IsAdmin || u.LoginDisabled {
		return &methodError{"not-authorized", "not-authorized"}
	}
	return nil
}

// exactObject is Meteor's check(value, {key: Type, ...}): exactly these keys,
// each of its type ("string", "number" or "boolean").
func exactObject(args []json.RawMessage, i int, want map[string]string) (map[string]any, error) {
	bad := &methodError{"Match.Error", "Match failed"}
	if len(args) <= i {
		return nil, bad
	}
	var m map[string]any
	if json.Unmarshal(args[i], &m) != nil || m == nil || len(m) != len(want) {
		return nil, bad
	}
	for k, kind := range want {
		v, ok := m[k]
		if !ok {
			return nil, bad
		}
		switch kind {
		case "string":
			_, ok = v.(string)
		case "number":
			_, ok = v.(float64)
		case "boolean":
			_, ok = v.(bool)
		}
		if !ok {
			return nil, bad
		}
	}
	return m, nil
}

// leaseWait is how long an administrator's action waits for automatic
// recovery to release a notification's lease. Its pass holds one for a
// moment, every second, while a notification it cannot deliver is still in
// its grace; a click landing in that moment was answered "busy" at once.
const leaseWait = 1500 * time.Millisecond

// waitingForLease runs op again while the lease is busy, for at most
// leaseWait, and answers what it last answered.
func waitingForLease[T any](ctx context.Context, op func() (T, error)) (T, error) {
	deadline := time.Now().Add(leaseWait)
	for {
		out, err := op()
		if !errors.Is(err, errLeaseBusy) || time.Now().After(deadline) {
			return out, err
		}
		select {
		case <-ctx.Done():
			return out, err
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// recoveryError is how WeKan's methods name a failure to the page.
func recoveryError(err error, control bool) error {
	var me *methodError
	if errors.As(err, &me) && me.Code == "not-authorized" {
		return err
	}
	code := "activity-recovery-failed"
	switch {
	case errors.Is(err, errLeaseBusy):
		code = "activity-recovery-busy"
	case control && (errors.Is(err, errControlConflict) || errors.Is(err, errControlNotPending) || errors.Is(err, errActivityCancelled)):
		code = "activity-recovery-control-conflict"
	case control:
		code = "activity-recovery-control-failed"
	case errors.Is(err, errRecipientDenied), errors.Is(err, errPreferenceChanged):
		code = "activity-recovery-denied"
	case errors.Is(err, errOrphanActivity):
		code = "activity-recovery-source-unavailable"
	case errors.Is(err, errActivityCancelled):
		code = "activity-recovery-status-cancelled"
	case errors.Is(err, errActivityPaused):
		code = "activity-recovery-paused"
	case errors.Is(err, errNotificationsOff):
		code = "activity-recovery-disabled"
	}
	return &methodError{code, code}
}

func (s *Server) activityRecoveryMethods() map[string]methodFunc {
	admin := func(r *http.Request, u *User) func() error {
		return func() error { return s.assertRecoveryAdmin(r.Context(), u.ID) }
	}
	return map[string]methodFunc{
		"activityNotificationRecoveryReport": func(r *http.Request, u *User, args []json.RawMessage) (any, error) {
			q, err := exactObject(args, 0, map[string]string{"search": "string", "page": "number"})
			if err != nil {
				return nil, err
			}
			if err := admin(r, u)(); err != nil {
				return nil, err
			}
			search, page := q["search"].(string), q["page"].(float64)
			if len(search) > 100 || page != math.Trunc(page) || page < 1 || page > 100000 {
				return nil, &methodError{"activity-recovery-unavailable", "activity-recovery-unavailable"}
			}
			out, err := s.activityReport(r.Context(), search, int64(page))
			if err != nil {
				return nil, &methodError{"activity-recovery-unavailable", "activity-recovery-unavailable"}
			}
			return out, admin(r, u)()
		},
		"retryActivityNotification": func(r *http.Request, u *User, args []json.RawMessage) (any, error) {
			q, err := exactObject(args, 0, map[string]string{"intentId": "string"})
			if err != nil {
				return nil, err
			}
			if err := admin(r, u)(); err != nil {
				return nil, err
			}
			id := q["intentId"].(string)
			if !hashPattern.MatchString(id) {
				return nil, &methodError{"activity-recovery-invalid-request", "activity-recovery-invalid-request"}
			}
			status, err := waitingForLease(r.Context(), func() (string, error) { return s.deliverIntentStatus(r.Context(), id) })
			if err != nil {
				return nil, recoveryError(err, false)
			}
			return map[string]any{"status": status}, admin(r, u)()
		},
		"controlActivityNotificationRecovery": func(r *http.Request, u *User, args []json.RawMessage) (any, error) {
			q, err := exactObject(args, 0, map[string]string{"intentId": "string", "paused": "boolean", "expectedRevision": "number", "requestId": "string"})
			if err != nil {
				return nil, err
			}
			if err := admin(r, u)(); err != nil {
				return nil, err
			}
			rev, ok := safeInt(q["expectedRevision"])
			if !ok {
				return nil, recoveryError(errControlInvalid, true)
			}
			out, err := waitingForLease(r.Context(), func() (map[string]any, error) {
				return s.mutateActivityControl(r.Context(), q["intentId"].(string), q["paused"].(bool), false, rev, q["requestId"].(string), u.ID, admin(r, u))
			})
			if err != nil {
				return nil, recoveryError(err, true)
			}
			return out, nil
		},
		"cancelActivityNotificationRecovery": func(r *http.Request, u *User, args []json.RawMessage) (any, error) {
			q, err := exactObject(args, 0, map[string]string{"intentId": "string", "expectedRevision": "number", "requestId": "string"})
			if err != nil {
				return nil, err
			}
			if err := admin(r, u)(); err != nil {
				return nil, err
			}
			rev, ok := safeInt(q["expectedRevision"])
			if !ok {
				return nil, recoveryError(errControlInvalid, true)
			}
			id := q["intentId"].(string)
			out, err := waitingForLease(r.Context(), func() (map[string]any, error) {
				return s.mutateActivityControl(r.Context(), id, true, true, rev, q["requestId"].(string), u.ID, admin(r, u))
			})
			if err != nil {
				return nil, recoveryError(err, true)
			}
			if err := s.cleanupCancelledActivity(r.Context(), id); err != nil {
				s.log.Warn("cancelled activity payload cleanup incomplete; automatic recovery will retry", "reason", err.Error())
			}
			return out, nil
		},
		// #6768: the whole list at once.
		"cancelActivityNotificationsOlderThan": func(r *http.Request, u *User, args []json.RawMessage) (any, error) {
			q, err := exactObject(args, 0, map[string]string{"days": "number"})
			if err != nil {
				return nil, err
			}
			if err := admin(r, u)(); err != nil {
				return nil, err
			}
			days := q["days"].(float64)
			if days < 0 || days > 3650 {
				return nil, &methodError{"activity-recovery-invalid-request", "activity-recovery-invalid-request"}
			}
			older := bson.M{"state": "pending", "activity.createdAt": bson.M{"$lt": time.Now().UTC().Add(-time.Duration(days * float64(24*time.Hour)))}}
			intents := s.db.Collection("activityNotificationIntents")
			cur, err := intents.Find(r.Context(), older, options.Find().SetProjection(bson.M{"_id": 1}).SetLimit(5000))
			if err != nil {
				return nil, &methodError{"activity-recovery-failed", "activity-recovery-failed"}
			}
			var ids []struct {
				ID string `bson:"_id"`
			}
			_ = cur.All(r.Context(), &ids)
			cancelled := 0
			for _, row := range ids {
				if err := admin(r, u)(); err != nil {
					return nil, err
				}
				if s.giveUpActivity(r.Context(), row.ID, "admin") == nil {
					cancelled++
				}
			}
			remaining, _ := intents.CountDocuments(r.Context(), older)
			return map[string]any{"cancelled": cancelled, "remaining": remaining}, nil
		},
		"retryAllActivityNotifications": func(r *http.Request, u *User, args []json.RawMessage) (any, error) {
			if err := admin(r, u)(); err != nil {
				return nil, err
			}
			var t recoveryTotals
			after := ""
			for i := 0; i < 50; i++ {
				if err := admin(r, u)(); err != nil {
					return nil, err
				}
				t = t.add(s.recoverActivityNotifications(r.Context(), &after))
				if after == "" {
					break
				}
			}
			return map[string]any{"visited": t.Visited, "completed": t.Completed, "skipped": t.Skipped, "failed": t.Failed, "busy": t.Busy, "cancelled": t.Cancelled}, nil
		},
	}
}

// recoveryPage is Admin Panel / Problems / Recovery's pending activity
// notifications; the table is the report, filled by weddy.js.
func (s *Server) recoveryPage(w http.ResponseWriter, r *http.Request, u *User) {
	lang := languageOf(r, u).Tag
	if !u.IsAdmin {
		s.render(w, r, "notfound", http.StatusNotFound, map[string]any{"Title": tr(lang, "error-notAuthorized"), "User": u})
		return
	}
	data := map[string]any{"Title": tr(lang, "recoveryReportTitle"), "User": u}
	for k, v := range s.recoveryEventsPage(r) {
		data[k] = v
	}
	s.render(w, r, "problems", http.StatusOK, data)
}

func (s *Server) problemsRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /admin/problems/recovery", s.page(s.recoveryPage))
	m.HandleFunc("GET /admin/problems/security-report", s.page(s.securityReportPage))
	m.HandleFunc("GET /admin/problems/password-length", s.page(s.passwordLengthPage))
	m.HandleFunc("POST /admin/problems/password-length", s.page(s.savePasswordLength))
}
