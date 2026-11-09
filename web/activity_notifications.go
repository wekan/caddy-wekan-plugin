package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// An activity's notifications, delivered durably, as Node.js WeKan's #6768
// pipeline (server/notifications/activityPlans.js and server/lib/
// activityNotification*.js), in its collections and shapes, so either server
// finishes what the other began:
//
//  1. an intent - the activity's snapshot - is written first
//     (activityNotificationIntents, pending);
//  2. under a per-intent lease (activityNotificationLeases), a plan freezes
//     who gets what: the drawer, and the e-mail job (activityNotificationPlans);
//  3. the plan is delivered - each step idempotent - and the intent becomes a
//     receipt without the snapshot, and the plan a receipt without its body.
//
// A background scan resumes any intent left pending, a crashed worker's too,
// from its plan when there is one. An intent whose activity is gone is never
// recreated: it may have been deleted. An administrator pauses or cancels one,
// and one that can never finish is given up (activity_recovery.go).

// canonical is WeKan's models/lib/changeHistoryIntegrity.js canonical(): JSON
// with sorted keys and a date as {"$date": ISO}, as JavaScript writes it.
func canonical(v any) string {
	var b strings.Builder
	writeCanonical(&b, plain(v))
	return b.String()
}

func jsonString(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	out := strings.TrimSuffix(b.String(), "\n")
	// JSON.stringify leaves U+2028 and U+2029 as they are.
	return strings.NewReplacer(`\u2028`, "\u2028", `\u2029`, "\u2029").Replace(out)
}

// jsNumber is a number as JavaScript's JSON.stringify writes it.
func jsNumber(f float64) string {
	if f == 0 {
		return "0"
	}
	a := math.Abs(f)
	if a >= 1e-6 && a < 1e21 {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	s := strconv.FormatFloat(f, 'e', -1, 64)
	mant, exp, _ := strings.Cut(s, "e")
	n, _ := strconv.Atoi(exp)
	sign := "+"
	if n < 0 {
		sign, n = "-", -n
	}
	return mant + "e" + sign + strconv.Itoa(n)
}

func writeCanonical(b *strings.Builder, v any) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case time.Time:
		b.WriteString(`{"$date":` + jsonString(x.UTC().Format("2006-01-02T15:04:05.000Z")) + "}")
	case []any:
		b.WriteString("[")
		for i, e := range x {
			if i > 0 {
				b.WriteString(",")
			}
			writeCanonical(b, e)
		}
		b.WriteString("]")
	case []string:
		b.WriteString("[")
		for i, e := range x {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(jsonString(e))
		}
		b.WriteString("]")
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		// JavaScript sorts keys by UTF-16 code units; for the keys of these
		// documents that is the byte order.
		sort.Strings(keys)
		b.WriteString("{")
		for i, k := range keys {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(jsonString(k) + ":")
			writeCanonical(b, x[k])
		}
		b.WriteString("}")
	case string:
		b.WriteString(jsonString(x))
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case float64:
		b.WriteString(jsNumber(x))
	case int32:
		b.WriteString(strconv.Itoa(int(x)))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case int:
		b.WriteString(strconv.Itoa(x))
	default:
		raw, _ := json.Marshal(x)
		b.Write(raw)
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func intentIDFor(activityID string) string {
	return sha256Hex(canonical([]any{"activity-notification-intent", activityID}))
}

func planIDFor(activityID string) string {
	return sha256Hex(canonical([]any{"activity-notification-plan", activityID}))
}

var errOrphanActivity = errors.New("activity-notification-activity-unconfirmed")

// captureIntent writes the activity's intent once.
func (s *Server) captureIntent(ctx context.Context, activity map[string]any, dispatch any) {
	id := intentIDFor(strOf(activity["_id"]))
	var row bson.M
	if s.db.Collection("activityNotificationIntents").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&row) == nil {
		return
	}
	_, _ = s.db.Collection("activityNotificationIntents").InsertOne(ctx, bson.M{"_id": id, "version": 1, "state": "pending", // inserted, not a filter
		"activity": activity, "activityHash": sha256Hex(canonical(activity)), "dispatchUserId": dispatch, "writerId": randomID() + randomID()})
}

// withActivityLease is WeKan's withSyncLease on activityNotificationLeases:
// one worker an intent, its reservation renewed while it works.
func (s *Server) withActivityLease(ctx context.Context, id string, work func() error) error {
	leases := s.db.Collection("activityNotificationLeases")
	owner := randomID() + randomID()
	now := time.Now().UTC()
	res, err := leases.UpdateOne(ctx, bson.M{"_id": eq(id), "expiresAt": bson.M{"$lte": now}},
		bson.M{"$set": bson.M{"owner": owner, "expiresAt": now.Add(time.Minute)}})
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		// No lease, or somebody else's: only a missing one may be made.
		if _, err := leases.InsertOne(ctx, bson.M{"_id": id, "owner": owner, "expiresAt": now.Add(time.Minute)}); err != nil { // inserted, not a filter
			return errLeaseBusy
		}
	}
	defer func() { _, _ = leases.DeleteOne(context.WithoutCancel(ctx), bson.M{"_id": eq(id), "owner": owner}) }()
	return work()
}

var errLeaseBusy = errors.New("sync-busy")

// deliverIntent runs one pending intent to its end: plan, delivery, receipts.
func (s *Server) deliverIntent(ctx context.Context, intentID string) error {
	_, err := s.deliverIntentStatus(ctx, intentID)
	return err
}

// deliverIntentStatus is WeKan's resumeActivityNotifications: "completed", or
// "skipped" when there is nothing pending; paused or cancelled by an
// administrator, it delivers nothing - a cancelled one is compacted.
func (s *Server) deliverIntentStatus(ctx context.Context, intentID string) (string, error) {
	status := "skipped"
	err := s.withActivityLease(ctx, intentID, func() error {
		var row bson.M
		if s.db.Collection("activityNotificationIntents").FindOne(ctx, bson.M{"_id": eq(intentID)}).Decode(&row) != nil {
			return nil
		}
		intent := plain(row).(map[string]any)
		if intent["state"] == "cancelled" {
			return errActivityCancelled
		}
		if intent["state"] != "pending" {
			return nil
		}
		if err := s.assertActivityUnpaused(ctx, intentID); err != nil {
			if errors.Is(err, errActivityCancelled) {
				_, _ = s.compactCancelledActivity(ctx, intentID)
			}
			return err
		}
		snapshot, _ := intent["activity"].(map[string]any)
		activityID := strOf(snapshot["_id"])
		if activityID == "" || sha256Hex(canonical(snapshot)) != strOf(intent["activityHash"]) {
			return errIntentInvalid
		}
		var stored bson.M
		if s.db.Collection("activities").FindOne(ctx, bson.M{"_id": eq(activityID)}).Decode(&stored) != nil {
			return errOrphanActivity // never recreated: it may have been deleted
		}
		activity := plain(stored).(map[string]any)
		if canonical(activity) != canonical(snapshot) {
			return errOrphanActivity
		}
		dispatch := intent["dispatchUserId"]
		plan, err := s.activityPlan(ctx, activity, dispatch)
		if err != nil {
			return err
		}
		recipients, _ := plan["recipients"].([]any)
		for _, r := range recipients {
			rec, _ := r.(map[string]any)
			uid := strOf(rec["userId"])
			// Asked again before each delivery: a recipient who lost access,
			// or changed their mind, since the plan was frozen gets nothing.
			if rec["tray"] == true {
				if err := s.assertNotificationAccess(ctx, activity, uid, "Tray"); err != nil {
					return err
				}
			}
			if rec["email"] != nil {
				if err := s.assertNotificationAccess(ctx, activity, uid, "Email"); err != nil {
					return err
				}
			}
			if rec["tray"] == true {
				_, _ = s.db.Collection("users").UpdateOne(ctx, bson.M{"_id": eq(uid), "profile.notifications.activity": bson.M{"$ne": activityID}},
					bson.M{"$push": bson.M{"profile.notifications": bson.M{"activity": activityID, "read": nil}}})
			}
			if job, ok := rec["email"].(map[string]any); ok {
				if err := s.enqueueEmail(ctx, emailJobFromPlan(job)); err != nil {
					return err
				}
			}
		}
		// The intent becomes its receipt, without the snapshot.
		receipt := bson.M{"_id": intentID, "version": 1, "state": "completed", "activityHash": intent["activityHash"], // inserted, not a filter
			"dispatchUserId": dispatch, "writerId": intent["writerId"]}
		if _, err := s.db.Collection("activityNotificationIntents").ReplaceOne(ctx, bson.M{"_id": eq(intentID), "state": "pending"}, receipt); err != nil {
			return err
		}
		status = "completed"
		// And the plan its receipt, without the body.
		var p bson.M
		pid := planIDFor(activityID)
		if s.db.Collection("activityNotificationPlans").FindOne(ctx, bson.M{"_id": eq(pid)}).Decode(&p) == nil {
			_, _ = s.db.Collection("activityNotificationPlans").ReplaceOne(ctx, bson.M{"_id": eq(pid), "checksum": p["checksum"]},
				bson.M{"_id": pid, "activityHash": intent["activityHash"], "checksum": p["checksum"], "compactReceiptVersion": 1}) // inserted, not a filter
		}
		return nil
	})
	return status, err
}

// assertNotificationAccess is WeKan's assertAccess of a recipient: an active
// user, still on the activity's board - an assigned-only member only for a
// card assigned to them - with that kind of notification on.
func (s *Server) assertNotificationAccess(ctx context.Context, activity map[string]any, userID, service string) error {
	var u User
	if !validID(userID) || s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(userID)}).Decode(&u) != nil || u.LoginDisabled {
		return errRecipientDenied
	}
	boardID := strOf(activity["boardId"])
	if boardID != "" {
		var b Board
		if s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(boardID)}).Decode(&b) != nil || !b.MemberOf(userID) {
			return errRecipientDenied
		}
		var card Card
		hasCard := false
		if id := strOf(activity["cardId"]); id != "" {
			hasCard = s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&card) == nil
			if hasCard && card.BoardID != boardID {
				return errRecipientDenied
			}
		}
		if id := strOf(activity["listId"]); id != "" {
			var l struct {
				BoardID string `bson:"boardId"`
			}
			if s.db.Collection("lists").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&l) == nil && l.BoardID != boardID {
				return errRecipientDenied
			}
		}
		if m := b.member(userID); m != nil && m.AssignedOnly() && (!hasCard || !slices.Contains(card.Assignees, userID)) {
			return errRecipientDenied
		}
	}
	if !s.notificationsOn(ctx, service, userID, boardID) {
		return errPreferenceChanged
	}
	return nil
}

// activityPlan is the activity's frozen plan: the stored one, or - built once
// - who is notified, in the drawer and by e-mail.
func (s *Server) activityPlan(ctx context.Context, activity map[string]any, dispatch any) (map[string]any, error) {
	activityID := strOf(activity["_id"])
	pid := planIDFor(activityID)
	plans := s.db.Collection("activityNotificationPlans")
	read := func() (map[string]any, error) {
		var row bson.M
		if plans.FindOne(ctx, bson.M{"_id": eq(pid)}).Decode(&row) != nil {
			return nil, nil
		}
		r := plain(row).(map[string]any)
		if r["compactReceiptVersion"] != nil {
			return nil, errors.New("activity-notification-plan-compacted")
		}
		plan, _ := r["plan"].(map[string]any)
		if plan == nil || strOf(r["checksum"]) != sha256Hex(canonical(plan)) || strOf(plan["activityId"]) != activityID {
			return nil, errors.New("activity-notification-plan-invalid")
		}
		return plan, nil
	}
	if plan, err := read(); plan != nil || err != nil {
		return plan, err
	}
	actor, _ := dispatch.(string)
	recipients := []any{}
	for _, uid := range s.notificationRecipients(ctx, actor, bson.M(activity)) {
		var email any
		if job := s.prepareEmailJob(ctx, uid, bson.M(activity)); job != nil {
			email = job
		}
		// Only what the recipient has on now: a later change stops delivery
		// rather than silently changing the frozen audience.
		recipients = append(recipients, map[string]any{"userId": uid, "tray": s.notificationsOn(ctx, "Tray", uid, strOf(activity["boardId"])), "email": email})
	}
	plan := map[string]any{"version": 1, "activityId": activityID, "activityHash": sha256Hex(canonical(activity)),
		"dispatchUserId": dispatch, "recipients": recipients}
	_, _ = plans.InsertOne(ctx, bson.M{"_id": pid, "checksum": sha256Hex(canonical(plan)), "plan": plan}) // inserted, not a filter
	return read()
}

// emailJobFromPlan is a plan's frozen e-mail job.
func emailJobFromPlan(m map[string]any) emailJob {
	j := emailJob{UserID: strOf(m["userId"]), EventID: strOf(m["eventId"]), Subject: strOf(m["subject"]), HTML: strOf(m["html"]),
		Language: strOf(m["language"]), CardID: strOf(m["cardId"]), BoardID: strOf(m["boardId"])}
	if t, ok := m["text"].(string); ok {
		j.Text = &t
	}
	if g, ok := m["groupKey"].(string); ok {
		j.GroupKey = &g
	}
	switch v := m["deliverAt"].(type) {
	case float64:
		t := time.UnixMilli(int64(v)).UTC()
		j.DeliverAt = &t
	case int64:
		t := time.UnixMilli(v).UTC()
		j.DeliverAt = &t
	}
	return j
}

// activityRecoveryInterval is ACTIVITY_NOTIFICATION_RECOVERY_INTERVAL_MS, a
// second unless set, from 1 s to a minute.
func activityRecoveryInterval() time.Duration {
	if v, err := strconv.Atoi(os.Getenv("ACTIVITY_NOTIFICATION_RECOVERY_INTERVAL_MS")); err == nil && v >= 1000 && v <= 60000 {
		return time.Duration(v) * time.Millisecond
	}
	return time.Second
}

// recoverActivityNotifications resumes the pending intents, a hundred at a
// time, moving past the ones that cannot finish so they hide no others.
func (s *Server) recoverActivityNotifications(ctx context.Context, after *string) recoveryTotals {
	var t recoveryTotals
	filter := bson.M{"state": "pending"}
	if *after != "" {
		filter["_id"] = bson.M{"$gt": *after}
	}
	cur, err := s.db.Collection("activityNotificationIntents").Find(ctx, filter,
		options.Find().SetProjection(bson.M{"_id": 1, "activity.createdAt": 1}).SetSort(bson.D{{Key: "_id", Value: 1}}).SetLimit(100))
	if err != nil {
		return t
	}
	var rows []struct {
		ID       string `bson:"_id"`
		Activity struct {
			CreatedAt time.Time `bson:"createdAt"`
		} `bson:"activity"`
	}
	_ = cur.All(ctx, &rows)
	now := time.Now()
	t.Visited = len(rows)
	for _, r := range rows {
		s.recoverOne(ctx, r.ID, r.Activity.CreatedAt, now, &t)
	}
	*after = ""
	if len(rows) == 100 {
		*after = rows[len(rows)-1].ID
	}
	return t
}

// runActivityRecovery scans for pending intents until stop.
func (s *Server) runActivityRecovery(stop <-chan struct{}) {
	t := time.NewTicker(activityRecoveryInterval())
	defer t.Stop()
	after := ""
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			s.recoverActivityNotifications(ctx, &after)
			cancel()
		}
	}
}

// activityPlanCleanupInterval is ACTIVITY_NOTIFICATION_PLAN_CLEANUP_INTERVAL_MS,
// a minute unless set, from 1 s to a day.
func activityPlanCleanupInterval() time.Duration {
	if v, err := strconv.Atoi(os.Getenv("ACTIVITY_NOTIFICATION_PLAN_CLEANUP_INTERVAL_MS")); err == nil && v >= 1000 && v <= 86400000 {
		return time.Duration(v) * time.Millisecond
	}
	return time.Minute
}

// cleanupActivityPlans is WeKan's createActivityPlanCleanup: a plan whose
// intent is a completed receipt for the same activity loses its body - its
// recipients and their e-mail - and keeps its identity. A plan whose intent
// is still pending keeps everything.
func (s *Server) cleanupActivityPlans(ctx context.Context, after *string) {
	filter := bson.M{"compactReceiptVersion": bson.M{"$exists": false}}
	if *after != "" {
		filter["_id"] = bson.M{"$gt": *after}
	}
	plans := s.db.Collection("activityNotificationPlans")
	cur, err := plans.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}).SetLimit(100))
	if err != nil {
		return
	}
	var rows []bson.M
	_ = cur.All(ctx, &rows)
	for _, raw := range rows {
		row := plain(raw).(map[string]any)
		plan, _ := row["plan"].(map[string]any)
		id, activityID := strOf(row["_id"]), strOf(plan["activityId"])
		if plan == nil || id != planIDFor(activityID) || strOf(row["checksum"]) != sha256Hex(canonical(plan)) {
			continue // not WeKan's shape: never touched
		}
		_ = s.withActivityLease(ctx, intentIDFor(activityID), func() error {
			var intent bson.M
			if s.db.Collection("activityNotificationIntents").FindOne(ctx, bson.M{"_id": eq(intentIDFor(activityID))}).Decode(&intent) != nil ||
				intent["state"] != "completed" || intent["activityHash"] != plan["activityHash"] || intent["activity"] != nil {
				return nil
			}
			_, err := plans.ReplaceOne(ctx, bson.M{"_id": eq(id), "checksum": row["checksum"]},
				bson.M{"_id": id, "activityHash": plan["activityHash"], "checksum": row["checksum"], "compactReceiptVersion": 1}) // inserted, not a filter
			return err
		})
	}
	*after = ""
	if len(rows) == 100 {
		*after = strOf(rows[len(rows)-1]["_id"])
	}
}

func (s *Server) runActivityPlanCleanup(stop <-chan struct{}) {
	t := time.NewTicker(activityPlanCleanupInterval())
	defer t.Stop()
	after := ""
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			s.cleanupActivityPlans(ctx, &after)
			cancel()
		}
	}
}
