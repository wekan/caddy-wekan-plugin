package web

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Notification delivery for webhooks (WeKan's #3695, models/lib/
// notificationDelivery.js, webhookPayload.js and server/lib/webhookOutbox.js):
// what a webhook carries - the sentence or not, and which groups of
// properties - and when: at once, or grouped by board or card and sent
// together, on an interval, daily, or after quiet hours. The settings are the
// webhook's own, then its board's (for the board's own webhooks; a global
// one is the instance administrator's), then Admin Panel's. A grouped or
// scheduled webhook is queued in WeKan's own collections
// (notificationWebhookJobs, notificationWebhookLeases), so either server
// sends what the other queued: a group is one POST, an "act-batch" of its
// bodies, retried with a growing wait, ten times at most.

// webhookPayloadGroups are WeKan's WEBHOOK_PAYLOAD_GROUPS beyond "standard".
var webhookPayloadGroups = map[string][]string{
	"ids": {"activityId", "boardId", "oldBoardId", "swimlaneId", "oldSwimlaneId", "listId", "oldListId", "cardId", "commentId",
		"checklistId", "checklistItemId", "labelId", "attachmentId", "customFieldId"},
	"names":   {"board", "oldBoard", "swimlane", "oldSwimlane", "list", "oldList", "card", "checklist", "checklistItem", "label", "attachment", "customField"},
	"links":   {"url", "cardUrl", "boardUrl"},
	"actor":   {"user", "username", "userId"},
	"people":  {"member", "memberUsername", "memberId", "assignee", "assigneeUsername", "assigneeId"},
	"details": {"comment", "cardDescription", "customFieldValue", "value", "oldValue", "timeKey", "timeValue", "timeOldValue"},
}

var webhookGroupings = []string{"none", "all", "board", "card"}

// webhookDelivery is a webhook's resolved settings.
type webhookDelivery struct {
	Text   bool
	Fields []string
	emailDelivery
}

// webhookDeliveryFor is WeKan's webhookDeliveryFor: each key from the first
// level that sets it validly, else the built-in default; quiet hours as one.
func (s *Server) webhookDeliveryFor(ctx context.Context, hook bson.M) webhookDelivery {
	var board, set bson.M
	if id := docString(hook, "boardId"); validID(id) && id != "_global" {
		_ = s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&board)
	}
	_ = s.db.Collection("settings").FindOne(ctx, bson.M{}).Decode(&set)
	levels := []any{nestedField(hook, "notificationDelivery", "webhook"), nestedField(board, "notificationDelivery", "webhook"),
		nestedField(set, "notificationDelivery", "webhook")}
	d := webhookDelivery{Text: true, Fields: []string{"standard"},
		emailDelivery: emailDelivery{Grouping: "none", Schedule: "immediate", IntervalMinutes: 60, DailyTime: "08:00"}}
	// key is the value of k at the first level that sets it validly.
	key := func(k string, ok func(any) bool) (any, bool) {
		for _, l := range levels {
			m, _ := plain(l).(map[string]any)
			if v, has := m[k]; has && ok(v) {
				return v, true
			}
		}
		return nil, false
	}
	if v, ok := key("text", func(v any) bool { _, b := v.(bool); return b }); ok {
		d.Text = v.(bool)
	}
	if v, ok := key("fields", func(v any) bool {
		list, isList := v.([]any)
		if !isList {
			return false
		}
		for _, e := range list {
			s, _ := e.(string)
			if _, known := webhookPayloadGroups[s]; !known && s != "standard" {
				return false
			}
		}
		return true
	}); ok {
		d.Fields = nil
		for _, e := range v.([]any) {
			d.Fields = append(d.Fields, e.(string))
		}
	}
	str := func(k string, valid func(string) bool) (string, bool) {
		v, ok := key(k, func(v any) bool { s, isStr := v.(string); return isStr && valid(s) })
		if !ok {
			return "", false
		}
		return v.(string), true
	}
	if v, ok := str("grouping", func(v string) bool { return slices.Contains(webhookGroupings, v) }); ok {
		d.Grouping = v
	}
	if v, ok := str("schedule", func(v string) bool { return slices.Contains(emailSchedules, v) }); ok {
		d.Schedule = v
	}
	if v, ok := key("intervalMinutes", func(v any) bool { return slices.Contains(emailIntervals, intOf(v)) }); ok {
		d.IntervalMinutes = intOf(v)
	}
	if v, ok := str("dailyTime", hhmm.MatchString); ok {
		d.DailyTime = v
	}
	if v, ok := str("timezone", validTimezone); ok {
		d.Timezone = v
	}
	// Quiet hours are one setting: both ends from the same level.
	for _, l := range levels {
		m, _ := plain(l).(map[string]any)
		qs, okS := m["quietStart"].(string)
		qe, okE := m["quietEnd"].(string)
		if okS && okE && hhmm.MatchString(qs) && hhmm.MatchString(qe) {
			d.QuietStart, d.QuietEnd = qs, qe
			break
		}
	}
	return d
}

// webhookPayload is WeKan's buildWebhookBody for a one-way webhook: the
// sentence unless turned off, the standard attributes and the groups asked
// for, never an admin-only field's value, and the description.
func webhookPayload(d webhookDelivery, description, text string, p map[string]any) map[string]any {
	body := map[string]any{}
	if d.Text {
		body["text"] = text
	}
	if slices.Contains(d.Fields, "standard") {
		for _, k := range webhookAttributes {
			if v, ok := p[k]; ok {
				body[k] = v
			}
		}
	}
	for group, keys := range webhookPayloadGroups {
		if !slices.Contains(d.Fields, group) {
			continue
		}
		for _, k := range keys {
			src := k
			if k == "cardDescription" {
				src = "description"
			}
			if v, ok := p[src]; ok {
				body[k] = v
			}
		}
	}
	if p["customFieldAdminOnly"] == true {
		for _, k := range []string{"customFieldValue", "value", "oldValue"} {
			delete(body, k)
		}
	}
	body["description"] = description
	if description == "act-editCard" {
		if f, ok := p["field"].(string); ok {
			body["field"] = f
		}
	}
	return body
}

// webhookQueueTarget is WeKan's: nil to send at once, else where and when.
func webhookQueueTarget(d webhookDelivery, boardID, cardID, eventID string, created time.Time) (groupKey string, deliverAt time.Time, queued bool) {
	grouped := d.Grouping != "" && d.Grouping != "none"
	if !grouped && d.Schedule == "immediate" && d.QuietStart == "" {
		return "", time.Time{}, false
	}
	grouping := "none"
	base := time.Duration(0)
	if grouped {
		grouping, base = d.Grouping, 30*time.Second // WEBHOOK_GROUP_WINDOW_MS
	}
	return emailGroupKey(grouping, boardID, cardID, eventID), nextDeliveryAt(created, d.emailDelivery, base), true
}

// webhookJobID is WeKan's jobIdFor.
func webhookJobID(integrationID, eventID string) string {
	return sha256Hex(canonical([]any{"webhook-job", integrationID, eventID}))
}

// enqueueWebhook is WeKan's webhookOutbox.enqueue: once per webhook and event.
func (s *Server) enqueueWebhook(ctx context.Context, integrationID, eventID, groupKey string, body map[string]any, deliverAt time.Time) error {
	raw, err := json.Marshal(body)
	if err != nil || len(raw) > 1<<20 {
		return err
	}
	now := time.Now().UTC()
	if deliverAt.Before(now) {
		deliverAt = now
	}
	id := webhookJobID(integrationID, eventID)
	_, err = s.db.Collection("notificationWebhookJobs").InsertOne(ctx, bson.M{"_id": id, "integrationId": integrationID, // inserted, not a filter
		"eventId": eventID, "groupKey": groupKey, "body": body, "state": "pending", "attempts": 0, "createdAt": now, "nextAttemptAt": deliverAt})
	if err != nil {
		var existing struct {
			IntegrationID string `bson:"integrationId"`
			EventID       string `bson:"eventId"`
		}
		if s.db.Collection("notificationWebhookJobs").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&existing) == nil &&
			existing.IntegrationID == integrationID && existing.EventID == eventID {
			return nil // already queued
		}
	}
	return err
}

// combineWebhookBodies is WeKan's: one body as it is, more as an act-batch.
func combineWebhookBodies(bodies []map[string]any) map[string]any {
	if len(bodies) == 1 {
		return bodies[0]
	}
	var texts []string
	items := make([]any, 0, len(bodies))
	for _, b := range bodies {
		if t, ok := b["text"].(string); ok && t != "" {
			texts = append(texts, t)
		}
		items = append(items, b)
	}
	out := map[string]any{"description": "act-batch", "count": len(bodies), "items": items}
	if len(texts) > 0 {
		out["text"] = strings.Join(texts, "\n\n")
	}
	return out
}

func webhookRetryDelay(attempts int) time.Duration {
	d := 30 * time.Second
	for i := 1; i < attempts && d < time.Hour; i++ {
		d *= 2
	}
	if d > time.Hour {
		d = time.Hour
	}
	return d
}

// drainWebhookIntegration is WeKan's drainIntegration: under the webhook's
// lease, the earliest due group, up to 100 jobs, sent as one.
func (s *Server) drainWebhookIntegration(ctx context.Context, integrationID string) {
	_ = s.withLease(ctx, "notificationWebhookLeases", "webhook:"+integrationID, func() error {
		jobs := s.db.Collection("notificationWebhookJobs")
		now := time.Now().UTC()
		cur, err := jobs.Find(ctx, bson.M{"integrationId": eq(integrationID), "state": "pending", "nextAttemptAt": bson.M{"$lte": now}},
			options.Find().SetSort(bson.D{{Key: "nextAttemptAt", Value: 1}, {Key: "createdAt", Value: 1}, {Key: "_id", Value: 1}}).SetLimit(100))
		if err != nil {
			return err
		}
		var due []bson.M
		if cur.All(ctx, &due) != nil || len(due) == 0 {
			return nil
		}
		groupKey := docString(due[0], "groupKey")
		var batch []bson.M
		var ids bson.A
		var bodies []map[string]any
		for _, j := range due {
			if docString(j, "groupKey") == groupKey {
				batch = append(batch, j)
				ids = append(ids, docString(j, "_id"))
				b, _ := plain(j["body"]).(map[string]any)
				bodies = append(bodies, b)
			}
		}
		attemptID := randomID()
		_, _ = jobs.UpdateMany(ctx, bson.M{"_id": dbIn(ids), "state": "pending"}, bson.M{"$set": bson.M{"attemptId": attemptID}})
		finish := func(filterIDs bson.A, state string, extra bson.M) {
			set := bson.M{"state": state, "finishedAt": time.Now().UTC()}
			for k, v := range extra {
				set[k] = v
			}
			_, _ = jobs.UpdateMany(ctx, bson.M{"_id": dbIn(filterIDs), "state": "pending", "attemptId": attemptID},
				bson.M{"$set": set, "$unset": bson.M{"body": "", "nextAttemptAt": ""}})
		}
		var hook bson.M
		if s.db.Collection("integrations").FindOne(ctx, bson.M{"_id": eq(integrationID)}).Decode(&hook) != nil ||
			hook["enabled"] == false || docString(hook, "type") != "outgoing-webhooks" || docString(hook, "url") == "" {
			finish(ids, "cancelled", nil)
			return nil
		}
		raw, _ := json.Marshal(combineWebhookBodies(bodies))
		if s.deliverWebhook(ctx, hook, raw) == nil {
			finish(ids, "sent", nil)
			return nil
		}
		for _, j := range batch {
			attempts := intOf(j["attempts"]) + 1
			if attempts >= 10 {
				finish(bson.A{docString(j, "_id")}, "failed", bson.M{"attempts": attempts})
				continue
			}
			_, _ = jobs.UpdateOne(ctx, bson.M{"_id": eq(docString(j, "_id")), "state": "pending", "attemptId": attemptID},
				bson.M{"$set": bson.M{"attempts": attempts, "nextAttemptAt": time.Now().UTC().Add(webhookRetryDelay(attempts))}})
		}
		return nil
	})
}

// drainWebhookQueue is WeKan's drain: the webhooks with due jobs, earliest
// first, a hundred at a time.
func (s *Server) drainWebhookQueue(ctx context.Context) {
	cur, err := s.db.Collection("notificationWebhookJobs").Aggregate(ctx, bson.A{
		bson.M{"$match": bson.M{"state": "pending", "nextAttemptAt": bson.M{"$lte": time.Now().UTC()}}},
		// A group's key is a field of the jobs, not a value: a specification, not a filter.
		bson.M{"$group": bson.D{{Key: "_id", Value: "$integrationId"}, {Key: "nextAttemptAt", Value: bson.M{"$min": "$nextAttemptAt"}}}},
		bson.M{"$sort": bson.D{{Key: "nextAttemptAt", Value: 1}, {Key: "_id", Value: 1}}},
		bson.M{"$limit": 100},
	})
	if err != nil {
		return
	}
	var rows []struct {
		ID string `bson:"_id"`
	}
	_ = cur.All(ctx, &rows)
	for _, r := range rows {
		s.drainWebhookIntegration(ctx, r.ID)
	}
}

// runWebhookQueue drains the queue every five seconds until stop.
func (s *Server) runWebhookQueue(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case <-time.After(5 * time.Second):
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			s.drainWebhookQueue(ctx)
			cancel()
		}
	}
}
