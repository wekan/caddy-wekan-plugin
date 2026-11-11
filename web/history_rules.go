package web

// Rules' history, as WeKan's server/lib/ruleHistory.js: a rule is recorded
// as one snapshot of itself, its trigger and its action - and its further
// triggers and actions, in order (#4294) - without the timestamps, before and
// after each operation, so a compound edit (title, trigger and action at
// once) is ONE row. Undo and redo put a snapshot back as a board admin's, and
// refuse when the rule has changed since the row (another admin's newer
// edit is never overwritten); Restore takes an older snapshot on purpose.
// A trigger or an action another rule shares is never replaced, and parts
// no rule uses any more are removed.

import (
	"context"
	"net/http"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// ruleDocument is document(): a rule part without its timestamps.
func ruleDocument(d bson.M) any {
	if d == nil {
		return nil
	}
	m := plain(d).(map[string]any)
	for _, k := range []string{"createdAt", "updatedAt", "modifiedAt"} {
		delete(m, k)
	}
	return m
}

// ruleSnapshot is WeKan's: {rule: null} for a rule that is not there.
func (s *Server) ruleSnapshot(ctx context.Context, id string) map[string]any {
	var rule bson.M
	if !validID(id) || s.db.Collection("rules").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&rule) != nil {
		return map[string]any{"rule": nil}
	}
	part := func(coll string, id any) any {
		var d bson.M
		if s, ok := id.(string); !ok || !validID(s) || s == "" {
			return nil
		}
		_ = s.db.Collection(coll).FindOne(ctx, bson.M{"_id": eq(id.(string))}).Decode(&d)
		return ruleDocument(d)
	}
	snap := map[string]any{"rule": ruleDocument(rule), "trigger": part("triggers", rule["triggerId"]), "action": part("actions", rule["actionId"])}
	for _, p := range []struct{ field, key, coll string }{{"extraTriggerIds", "extraTriggers", "triggers"}, {"extraActionIds", "extraActions", "actions"}} {
		if ids := stringsOf(rule[p.field]); len(ids) > 0 {
			docs := []any{}
			for _, id := range ids {
				docs = append(docs, part(p.coll, id))
			}
			snap[p.key] = docs
		}
	}
	return snap
}

// recordRuleChange is WeKan's: one row, added, removed or edited, unless
// nothing changed.
func (s *Server) recordRuleChange(ctx context.Context, before, after map[string]any, userID string) {
	if userID == "" || canonical(before) == canonical(after) {
		return
	}
	rule, _ := after["rule"].(map[string]any)
	if rule == nil {
		rule, _ = before["rule"].(map[string]any)
	}
	if rule == nil {
		return
	}
	ct := "edited"
	if before["rule"] == nil {
		ct = "added"
	} else if after["rule"] == nil {
		ct = "removed"
	}
	s.recordHistory(ctx, historyChange{BoardID: docString(rule, "boardId"), EntityType: "rule", EntityID: docString(rule, "_id"), Group: "rules",
		ChangeType: ct, PreviousContent: before, NewContent: after, UserID: userID})
	// The board's open pages draw its rules' buttons again - whichever way
	// the rule was written: the REST API's writes used to leave them stale.
	s.hub.publish(docString(rule, "boardId"), "board-changed")
}

// withRuleHistory runs write between two snapshots of the rule; id may come
// from the write itself (a new rule).
func (s *Server) withRuleHistory(ctx context.Context, id, userID string, write func() (string, error)) error {
	before := s.ruleSnapshot(ctx, id)
	newID, err := write()
	if newID != "" {
		id = newID
	}
	s.recordRuleChange(ctx, before, s.ruleSnapshot(ctx, id), userID)
	return err
}

// applyRuleHistory is WeKan's: a board admin's, a snapshot put back.
func (s *Server) applyRuleHistory(ctx context.Context, u *User, row map[string]any, direction string) (bool, error) {
	boardID := docString(row, "boardId")
	var b Board
	if s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(boardID)}).Decode(&b) != nil || !(b.AdminOf(u.ID) || u.IsAdmin) {
		return false, &methodError{"not-authorized", "Must be a board admin"}
	}
	current := s.ruleSnapshot(ctx, docString(row, "entityId"))
	if r, _ := current["rule"].(map[string]any); r != nil && docString(r, "boardId") != boardID {
		return false, &methodError{"not-authorized", "not-authorized"}
	}
	expected := row["previousContent"]
	content, _ := row["newContent"].(map[string]any)
	switch direction {
	case "undo":
		expected = row["newContent"]
		content, _ = row["previousContent"].(map[string]any)
	case "restore":
		if content == nil {
			content, _ = row["previousContent"].(map[string]any)
		}
	}
	// Undo and redo never overwrite a later edit by somebody else.
	if direction != "restore" && canonical(current) != canonical(plain(expected)) {
		return false, &methodError{"history-conflict", "The rule changed after this history entry"}
	}
	if content == nil {
		return false, nil
	}
	if _, has := content["rule"]; !has {
		return false, nil
	}
	rule, _ := content["rule"].(map[string]any)
	if rule == nil {
		if cur, _ := current["rule"].(map[string]any); cur != nil {
			return true, s.removeRule(ctx, bson.M(cur))
		}
		return false, nil
	}
	trigger, _ := content["trigger"].(map[string]any)
	action, _ := content["action"].(map[string]any)
	if docString(rule, "_id") != docString(row, "entityId") || docString(rule, "boardId") != boardID || trigger == nil || action == nil ||
		docString(trigger, "_id") != docString(rule, "triggerId") || docString(action, "_id") != docString(rule, "actionId") || docString(trigger, "boardId") != boardID {
		return false, nil
	}
	type part struct {
		coll, field string
		doc         map[string]any
	}
	parts := []part{{"triggers", "trigger", trigger}, {"actions", "action", action}}
	for _, p := range []struct{ key, coll, field, ids string }{{"extraTriggers", "triggers", "trigger", "extraTriggerIds"}, {"extraActions", "actions", "action", "extraActionIds"}} {
		docs, _ := content[p.key].([]any)
		ids := stringsOf(rule[p.ids])
		if len(docs) != len(ids) {
			return false, nil
		}
		for i, d := range docs {
			m, _ := d.(map[string]any)
			if m == nil || docString(m, "_id") != ids[i] || docString(m, "boardId") != boardID {
				return false, nil
			}
			parts = append(parts, part{p.coll, p.field, m})
		}
	}
	// Every target checked before the first write: another board's document,
	// or another rule's shared part that differs, is never replaced.
	shared := map[string]bool{}
	for _, p := range parts {
		var existing bson.M
		id := docString(p.doc, "_id")
		_ = s.db.Collection(p.coll).FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&existing)
		if existing != nil && docString(existing, "boardId") != docString(p.doc, "boardId") {
			return false, nil
		}
		extra := "extraTriggerIds"
		if p.field == "action" {
			extra = "extraActionIds"
		}
		// Another rule using the part: one of those that use it is not this one.
		usedElsewhere := false
		if cur, err := s.db.Collection("rules").Find(ctx, bson.M{"$or": bson.A{bson.M{p.field + "Id": eq(id)}, bson.M{extra: eq(id)}}}); err == nil {
			var users []struct {
				ID string `bson:"_id"`
			}
			_ = cur.All(ctx, &users)
			for _, x := range users {
				usedElsewhere = usedElsewhere || x.ID != docString(rule, "_id")
			}
		}
		if usedElsewhere {
			if existing == nil || canonical(ruleDocument(existing)) != canonical(p.doc) {
				return false, nil
			}
			shared[id] = true
		}
	}
	write := func(coll string, doc map[string]any) error {
		id := docString(doc, "_id")
		fields := bson.M{}
		for k, v := range doc {
			if k != "_id" {
				fields[k] = v
			}
		}
		var existing bson.M
		if s.db.Collection(coll).FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&existing) != nil {
			fields["_id"] = id
			_, err := s.db.Collection(coll).InsertOne(ctx, fields)
			return err
		}
		update := bson.M{"$set": fields}
		unset := bson.M{}
		for k := range existing {
			if _, keep := fields[k]; !keep && k != "_id" && k != "createdAt" && k != "updatedAt" && k != "modifiedAt" {
				unset[k] = ""
			}
		}
		if len(unset) > 0 {
			update["$unset"] = unset
		}
		_, err := s.db.Collection(coll).UpdateOne(ctx, bson.M{"_id": eq(id)}, update)
		return err
	}
	for _, p := range parts {
		if !shared[docString(p.doc, "_id")] {
			if err := write(p.coll, p.doc); err != nil {
				return false, err
			}
		}
	}
	if err := write("rules", rule); err != nil {
		return false, err
	}
	// The parts the rule used before and uses no more.
	if cur, _ := current["rule"].(map[string]any); cur != nil {
		s.removeUnusedRuleParts(ctx, cur)
	}
	s.hub.publish(boardID, "board-changed")
	return true, nil
}

// removeUnusedRuleParts: the rule's triggers and actions no rule uses now.
func (s *Server) removeUnusedRuleParts(ctx context.Context, rule map[string]any) {
	for _, p := range []struct{ field, extra, coll string }{{"trigger", "extraTriggerIds", "triggers"}, {"action", "extraActionIds", "actions"}} {
		ids := append([]string{docString(rule, p.field+"Id")}, stringsOf(rule[p.extra])...)
		for _, id := range ids {
			if id == "" {
				continue
			}
			if s.db.Collection("rules").FindOne(ctx, bson.M{"$or": bson.A{bson.M{p.field + "Id": eq(id)}, bson.M{p.extra: eq(id)}}}).Err() != nil {
				_, _ = s.db.Collection(p.coll).DeleteOne(ctx, bson.M{"_id": eq(id)})
			}
		}
	}
}

var _ = http.MethodPost
