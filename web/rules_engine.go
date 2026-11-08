package web

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// WeKan's rules, run as Node.js WeKan runs them (server/rulesHelper.js,
// server/triggersDef.js, server/rulesButton.js): an activity finds the
// triggers it matches on its board, and each enabled rule of those runs its
// actions in order, as the user whose activity it was. A rule's own changes
// are activities too, so rules can start rules - at most maxRuleDepth deep.

// maxRuleDepth is WeKan's MAX_RULE_DEPTH.
const maxRuleDepth = 5

// ruleActingUser is WeKan's RULE_ACTING_USER_SENTINEL: "whoever started it".
const ruleActingUser = "_actinguser_"

// triggerFields is WeKan's TriggersDef: what each activity type is matched by.
var triggerFields = map[string][]string{
	"createCard":           {"boardId", "listName", "userId", "swimlaneName", "cardTitle"},
	"moveCard":             {"boardId", "listName", "oldListName", "userId", "swimlaneName", "cardTitle"},
	"archivedCard":         {"boardId", "userId", "cardTitle"},
	"addComment":           {"boardId", "userId", "cardTitle"},
	"restoredCard":         {"boardId", "userId", "cardTitle"},
	"joinMember":           {"boardId", "username", "userId"},
	"unjoinMember":         {"boardId", "username", "userId"},
	"joinAssignee":         {"boardId", "username", "userId"},
	"unjoinAssignee":       {"boardId", "username", "userId"},
	"addChecklist":         {"boardId", "checklistName", "userId"},
	"removeChecklist":      {"boardId", "checklistName", "userId"},
	"completeChecklist":    {"boardId", "checklistName", "userId"},
	"uncompleteChecklist":  {"boardId", "checklistName", "userId"},
	"addedChecklistItem":   {"boardId", "checklistItemName", "userId"},
	"removedChecklistItem": {"boardId", "checklistItemName", "userId"},
	"checkedItem":          {"boardId", "checklistItemName", "userId"},
	"uncheckedItem":        {"boardId", "checklistItemName", "userId"},
	"addAttachment":        {"boardId", "userId"},
	"deleteAttachment":     {"boardId", "userId"},
	"addedLabel":           {"boardId", "labelId", "userId"},
	"removedLabel":         {"boardId", "labelId", "userId"},
	"a-receivedAt":         {"boardId", "userId"},
	"a-startAt":            {"boardId", "userId"},
	"a-dueAt":              {"boardId", "userId"},
	"a-endAt":              {"boardId", "userId"},
}

type ruleDepthKey struct{}

func ruleDepth(ctx context.Context) int {
	d, _ := ctx.Value(ruleDepthKey{}).(int)
	return d
}

func docString(d bson.M, k string) string {
	v, _ := d[k].(string)
	return v
}

// cardTitleMatches is WeKan's cardTitleMatchList: a trigger's card title is
// "*", unset, the whole title or one of its words.
func cardTitleMatches(want any, title string) bool {
	w, _ := want.(string)
	if want == nil || w == "*" || w == title {
		return true
	}
	return slices.Contains(regexp.MustCompile(`\W`).Split(title, -1), w) && w != ""
}

// ruleValues is what each matching field is for this activity, looked up as
// WeKan's buildMatchingFieldsMap does: list and swimlane names by id, the card
// title from the card, a member's username.
func (s *Server) ruleValues(ctx context.Context, a bson.M, fields []string) map[string]string {
	out := map[string]string{}
	title := func(coll, id string) string { return s.titleOfID(ctx, coll, id) }
	for _, f := range fields {
		v := docString(a, f)
		switch f {
		case "listName":
			if v == "" {
				v = title("lists", docString(a, "listId"))
			}
		case "oldListName":
			v = title("lists", docString(a, "oldListId"))
		case "swimlaneName":
			if v == "" {
				v = title("swimlanes", docString(a, "swimlaneId"))
			}
		case "cardTitle":
			if v == "" {
				v = title("cards", docString(a, "cardId"))
			}
		case "checklistName":
			if v == "" {
				v = title("checklists", docString(a, "checklistId"))
			}
		case "checklistItemName":
			if v == "" {
				v = title("checklistItems", docString(a, "checklistItemId"))
			}
		case "username":
			if v == "" {
				id := docString(a, "memberId")
				if id == "" {
					id = docString(a, "assigneeId")
				}
				var u User
				if validID(id) && s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&u) == nil {
					v = u.Username
				}
			}
		}
		out[f] = v
	}
	return out
}

// matchingRules is WeKan's findMatchingRules: the enabled rules of the
// activity's own board (RepointBleed) one of whose triggers it matches - a
// trigger field that is unset or "*" matching anything - each rule once.
func (s *Server) matchingRules(ctx context.Context, a bson.M) []bson.M {
	kind, boardID := docString(a, "activityType"), docString(a, "boardId")
	fields, known := triggerFields[kind]
	if !known || !validID(boardID) {
		return nil
	}
	cur, err := s.db.Collection("triggers").Find(ctx, bson.M{"activityType": eq(kind)})
	if err != nil {
		return nil
	}
	var triggers []bson.M
	_ = cur.All(ctx, &triggers)
	values := s.ruleValues(ctx, a, fields)
	var rules []bson.M
	seen := map[string]bool{}
	for _, t := range triggers {
		tb, has := t["boardId"]
		if has && tb != nil && tb != "*" && tb != boardID {
			continue
		}
		ok := true
		for _, f := range fields {
			if f == "boardId" {
				continue
			}
			want, has := t[f]
			if f == "cardTitle" {
				ok = ok && (!has || cardTitleMatches(want, values[f]))
				continue
			}
			if has && want != nil && want != "*" && want != values[f] {
				ok = false
			}
		}
		if !ok {
			continue
		}
		tid := docString(t, "_id")
		cur, err := s.db.Collection("rules").Find(ctx, bson.M{"boardId": eq(boardID), "$or": bson.A{
			bson.M{"triggerId": eq(tid)}, bson.M{"extraTriggerIds": eq(tid)}}})
		if err != nil {
			continue
		}
		var found []bson.M
		_ = cur.All(ctx, &found)
		for _, r := range found {
			id := docString(r, "_id")
			if r["enabled"] == false || seen[id] {
				continue
			}
			seen[id] = true
			rules = append(rules, r)
		}
	}
	return rules
}

// ruleActions are a rule's actions in order: its own, then the extra ones (#4294).
func (s *Server) ruleActions(ctx context.Context, rule bson.M) []bson.M {
	ids := []string{docString(rule, "actionId")}
	ids = append(ids, stringsOf(rule["extraActionIds"])...)
	var out []bson.M
	for _, id := range ids {
		var act bson.M
		if validID(id) && s.db.Collection("actions").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&act) == nil {
			out = append(out, act)
		}
	}
	return out
}

// executeRules runs the rules an activity starts. A rule started by rules
// more than maxRuleDepth deep is stopped and recorded (WeKan's dos.rule-loop).
func (s *Server) executeRules(ctx context.Context, a bson.M) {
	depth := ruleDepth(ctx)
	if depth >= maxRuleDepth {
		s.recordSecurityCtx(ctx, securityEvent{Key: "dos.rule-loop", Action: "detected", Source: "rules",
			Detail: fmt.Sprintf("rules started by rules %d deep on board %s; stopped", depth, docString(a, "boardId"))})
		return
	}
	ctx = context.WithValue(ctx, ruleDepthKey{}, depth+1)
	for _, rule := range s.matchingRules(ctx, a) {
		for _, act := range s.ruleActions(ctx, rule) {
			if err := s.performAction(ctx, a, act); err != nil {
				s.log.Warn("rule action", "rule", rule["_id"], "action", act["actionType"], "error", err)
			}
		}
	}
}

// ruleVars are WeKan's buildRuleVars: what an action's text may name.
func (s *Server) ruleVars(ctx context.Context, a bson.M, c *Card, actor *User) map[string]string {
	now := time.Now()
	v := map[string]string{"date": now.Format("2006-01-02"), "time": now.Format("15:04:05"), "datetime": now.Format("2006-01-02 15:04:05")}
	v["boardname"] = s.titleOfID(ctx, "boards", docString(a, "boardId"))
	if actor != nil {
		v["username"] = actor.Username
		v["user"] = actor.Username
	}
	if c != nil {
		v["cardname"], v["cardtitle"], v["description"] = c.Title, c.Title, c.Description
		v["cardnumber"] = strconv.FormatInt(c.CardNumber, 10)
		v["listname"] = s.titleOfID(ctx, "lists", c.ListID)
		v["swimlanename"] = s.titleOfID(ctx, "swimlanes", c.SwimlaneID)
	}
	return v
}

var ruleVarPattern = regexp.MustCompile(`\{(\w+)\}`)

// substituteVars is WeKan's: {name} becomes the variable, an unknown one stays.
func substituteVars(text string, vars map[string]string) string {
	return ruleVarPattern.ReplaceAllStringFunc(text, func(m string) string {
		if v, ok := vars[strings.ToLower(m[1:len(m)-1])]; ok {
			return v
		}
		return m
	})
}

// ruleUsers are the people an action's username names: the acting user, "*"
// for all of them on the card (removal only), or usernames - a {token} naming
// several, comma-separated, as WeKan's ruleUsernames.
func (s *Server) ruleUsers(ctx context.Context, a bson.M, value string, vars map[string]string) []string {
	if value == ruleActingUser {
		if id := docString(a, "userId"); id != "" {
			return []string{id}
		}
		return nil
	}
	names := []string{value}
	if strings.Contains(value, "{") {
		names = strings.Split(substituteVars(value, vars), ",")
	}
	var ids []string
	for _, n := range names {
		n = strings.TrimSpace(n)
		var u User
		if n != "" && !strings.ContainsAny(n, "{}") && s.db.Collection("users").FindOne(ctx, bson.M{"username": eq(n)}).Decode(&u) == nil {
			ids = append(ids, u.ID)
		}
	}
	return ids
}

// performAction is WeKan's: one action of a rule, on the activity's card, as
// the activity's user.
func (s *Server) performAction(ctx context.Context, a bson.M, act bson.M) error {
	kind := docString(act, "actionType")
	boardID := docString(a, "boardId")
	var b Board
	if !validID(boardID) || s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(boardID)}).Decode(&b) != nil {
		return nil
	}
	var actor *User
	var au User
	if id := docString(a, "userId"); validID(id) && s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&au) == nil {
		actor = &au
	}
	var c *Card
	var card Card
	if id := docString(a, "cardId"); validID(id) && s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&card) == nil {
		c = &card
	}
	boardLevel := kind == "createCard" || kind == "addSwimlane"
	if c == nil && !boardLevel {
		return nil
	}
	vars := s.ruleVars(ctx, a, c, actor)
	userID := docString(a, "userId")
	cards := s.db.Collection("cards")
	now := time.Now().UTC()
	touch := func(update bson.M) error {
		set, _ := update["$set"].(bson.M)
		if set == nil {
			set = bson.M{}
			update["$set"] = set
		}
		set["modifiedAt"] = now
		_, err := cards.UpdateOne(ctx, bson.M{"_id": c.ID}, update)
		s.hub.publish(c.BoardID, "board-changed")
		return err
	}
	cardActivity := func(kind string, extra bson.M) {
		fields := bson.M{"boardId": c.BoardID, "cardId": c.ID, "listId": c.ListID, "swimlaneId": c.SwimlaneID, "cardTitle": c.Title}
		for k, v := range extra {
			fields[k] = v
		}
		s.activity(ctx, userID, kind, fields)
	}
	switch kind {
	case "moveCardToTop", "moveCardToBottom":
		return s.ruleMoveCard(ctx, a, act, &b, c, actor, vars)
	case "archive":
		if !c.Archived {
			if err := touch(bson.M{"$set": bson.M{"archived": true, "archivedAt": now}}); err != nil {
				return err
			}
			cardActivity("archivedCard", nil)
		}
	case "unarchive":
		if c.Archived {
			if err := touch(bson.M{"$set": bson.M{"archived": false}}); err != nil {
				return err
			}
			cardActivity("restoredCard", nil)
		}
	case "setColor":
		return touch(bson.M{"$set": bson.M{"color": docString(act, "selectedColor")}})
	case "addLabel", "removeLabel":
		label := docString(act, "labelId")
		if !validID(label) {
			return nil
		}
		has := slices.Contains(c.LabelIDs, label)
		if kind == "addLabel" && !has {
			if err := touch(bson.M{"$addToSet": bson.M{"labelIds": label}}); err != nil {
				return err
			}
			cardActivity("addedLabel", bson.M{"labelId": label})
		}
		if kind == "removeLabel" && has {
			if err := touch(bson.M{"$pull": bson.M{"labelIds": label}}); err != nil {
				return err
			}
			cardActivity("removedLabel", bson.M{"labelId": label})
		}
	case "removeAllLabels":
		return touch(bson.M{"$set": bson.M{"labelIds": bson.A{}}})
	case "addMember", "removeMember", "addAssignee", "removeAssignee":
		field, join, unjoin, idKey := "members", "joinMember", "unjoinMember", "memberId"
		current := c.Members
		if strings.HasSuffix(kind, "Assignee") {
			field, join, unjoin, idKey, current = "assignees", "joinAssignee", "unjoinAssignee", "assigneeId", c.Assignees
		}
		var ids []string
		if strings.HasPrefix(kind, "remove") && docString(act, "username") == "*" {
			ids = slices.Clone(current)
		} else {
			ids = s.ruleUsers(ctx, a, docString(act, "username"), vars)
		}
		for _, id := range ids {
			if strings.HasPrefix(kind, "add") && !slices.Contains(current, id) && b.MemberOf(id) {
				if err := touch(bson.M{"$addToSet": bson.M{field: id}}); err != nil {
					return err
				}
				current = append(current, id)
				cardActivity(join, bson.M{idKey: id})
			}
			if strings.HasPrefix(kind, "remove") && slices.Contains(current, id) {
				if err := touch(bson.M{"$pull": bson.M{field: id}}); err != nil {
					return err
				}
				current = slices.DeleteFunc(current, func(x string) bool { return x == id })
				cardActivity(unjoin, bson.M{idKey: id})
			}
		}
	case "checkAll", "uncheckAll", "checkItem", "uncheckItem":
		var cl struct {
			ID string `bson:"_id"`
		}
		if s.db.Collection("checklists").FindOne(ctx, bson.M{"cardId": c.ID, "title": eq(docString(act, "checklistName"))}).Decode(&cl) != nil {
			return nil // #5283: no such checklist on the card - nothing to do
		}
		filter := bson.M{"checklistId": cl.ID}
		if strings.HasSuffix(kind, "Item") {
			filter["title"] = eq(docString(act, "checkItemName"))
		}
		_, err := s.db.Collection("checklistItems").UpdateMany(ctx, filter, bson.M{"$set": bson.M{"isFinished": strings.HasPrefix(kind, "check")}})
		s.hub.publish(c.BoardID, "board-changed")
		return err
	case "addChecklist", "addChecklistWithItems":
		id := randomID()
		title := substituteVars(docString(act, "checklistName"), vars)
		if _, err := s.db.Collection("checklists").InsertOne(ctx, bson.M{"_id": id, "title": title, "cardId": c.ID, "boardId": c.BoardID, // inserted, not a filter
			"sort": 0, "createdAt": now}); err != nil { // inserted, not a filter
			return err
		}
		cardActivity("addChecklist", bson.M{"checklistId": id, "checklistName": title})
		if kind == "addChecklistWithItems" {
			for i, t := range strings.Split(substituteVars(docString(act, "checklistItems"), vars), ",") {
				if _, err := s.db.Collection("checklistItems").InsertOne(ctx, bson.M{"_id": randomID(), "title": t, "checklistId": id, // inserted, not a filter
					"cardId": c.ID, "boardId": c.BoardID, "sort": i, "isFinished": false}); err != nil { // inserted, not a filter
					return err
				}
			}
		}
		s.hub.publish(c.BoardID, "board-changed")
	case "removeChecklist":
		var cl struct {
			ID string `bson:"_id"`
		}
		if s.db.Collection("checklists").FindOne(ctx, bson.M{"cardId": c.ID, "title": eq(docString(act, "checklistName"))}).Decode(&cl) == nil {
			_, _ = s.db.Collection("checklistItems").DeleteMany(ctx, bson.M{"checklistId": cl.ID})
			_, _ = s.db.Collection("checklists").DeleteOne(ctx, bson.M{"_id": cl.ID})
			s.hub.publish(c.BoardID, "board-changed")
		}
	case "addSwimlane":
		_, err := s.db.Collection("swimlanes").InsertOne(ctx, bson.M{"_id": randomID(), "title": substituteVars(docString(act, "swimlaneName"), vars), // inserted, not a filter
			"boardId": b.ID, "sort": 0, "archived": false, "type": "swimlane", "createdAt": now}) // inserted, not a filter
		s.hub.publish(b.ID, "board-changed")
		return err
	case "createCard":
		return s.ruleCreateCard(ctx, a, act, &b, c, actor, vars)
	case "addComment":
		text := strings.TrimSpace(substituteVars(docString(act, "comment"), vars))
		if text == "" || actor == nil {
			return nil
		}
		id := randomID()
		if _, err := s.db.Collection("card_comments").InsertOne(ctx, bson.M{"_id": id, "boardId": c.BoardID, "cardId": c.ID, // inserted, not a filter
			"text": text, "userId": userID, "createdAt": now, "modifiedAt": now}); err != nil { // inserted, not a filter
			return err
		}
		cardActivity("addComment", bson.M{"commentId": id})
	case "markCardComplete", "markCardIncomplete":
		return touch(bson.M{"$set": bson.M{"dueComplete": kind == "markCardComplete"}})
	case "setDate", "updateDate", "removeDate":
		field := docString(act, "dateField")
		if !slices.Contains([]string{"startAt", "endAt", "dueAt", "receivedAt"}, field) {
			return nil
		}
		if kind == "removeDate" {
			return touch(bson.M{"$unset": bson.M{field: ""}})
		}
		if err := touch(bson.M{"$set": bson.M{field: now}}); err != nil {
			return err
		}
		cardActivity("a-"+field, nil)
	}
	return nil
}

// ruleMoveCard is WeKan's moveCardTarget and move: to the top or the bottom
// of a list - named, or the card's own - in a swimlane - named, the card's
// own ("*"), or the destination's first - on this board or another. Another
// board is reached only when the user may change it (RuleBleed).
func (s *Server) ruleMoveCard(ctx context.Context, a, act bson.M, b *Board, c *Card, actor *User, vars map[string]string) error {
	toID := docString(act, "boardId")
	if toID == "" {
		toID = b.ID
	}
	if c.BoardID != b.ID && toID == b.ID { // an earlier action moved it on
		toID = c.BoardID
	}
	to := b
	if toID != b.ID {
		var tb Board
		if !validID(toID) || s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(toID)}).Decode(&tb) != nil || actor == nil || !tb.Writable(actor, "write", nil) {
			// WeKan's canary rule.cross-board-write: noticed, the write never made.
			s.recordSecurityCtx(ctx, securityEvent{Key: "authz.canary", Action: "detected", Source: "canary:rule.cross-board-write",
				UserID: docString(a, "userId"), Detail: "A rule action named a board its user may not change."})
			return nil
		}
		to = &tb
	}
	listName := docString(act, "listName")
	var list List
	listFound := false
	if listName == "" || listName == "*" {
		own := s.titleOfID(ctx, "lists", c.ListID)
		listFound = s.db.Collection("lists").FindOne(ctx, bson.M{"boardId": to.ID, "title": eq(own)}).Decode(&list) == nil
	} else {
		listFound = s.db.Collection("lists").FindOne(ctx, bson.M{"boardId": to.ID, "title": eq(substituteVars(listName, vars))}).Decode(&list) == nil
	}
	swimlaneID := ""
	fellBack := false
	if !listFound { // #6472: the card's own list, on its own board
		if s.db.Collection("lists").FindOne(ctx, bson.M{"_id": eq(c.ListID)}).Decode(&list) != nil {
			return nil
		}
		to, swimlaneID, fellBack = b, c.SwimlaneID, true
		if c.BoardID != b.ID {
			var cb Board
			if s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(c.BoardID)}).Decode(&cb) != nil {
				return nil
			}
			to = &cb
		}
	}
	if !fellBack {
		swName := docString(act, "swimlaneName")
		var sw Swimlane
		if swName == "*" || swName == "" {
			own := s.titleOfID(ctx, "swimlanes", c.SwimlaneID)
			if s.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": to.ID, "title": eq(own)}).Decode(&sw) == nil {
				swimlaneID = sw.ID
			}
		} else if s.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": to.ID, "title": eq(substituteVars(swName, vars))}).Decode(&sw) == nil {
			swimlaneID = sw.ID
		}
		if swimlaneID == "" {
			swimlaneID = s.firstSwimlane(ctx, to.ID) // #5536
		}
	}
	cur, err := s.db.Collection("cards").Find(ctx, bson.M{"listId": list.ID, "swimlaneId": eq(swimlaneID), "archived": false})
	if err != nil {
		return err
	}
	var siblings []Card
	_ = cur.All(ctx, &siblings)
	lo, hi, any := math.Inf(1), math.Inf(-1), false
	for _, x := range siblings {
		if x.ID != c.ID && !math.IsInf(x.Sort, 0) && !math.IsNaN(x.Sort) {
			lo, hi, any = min(lo, x.Sort), max(hi, x.Sort), true
		}
	}
	if !any {
		lo, hi = 0, 0
	}
	sort := hi + 1
	if docString(act, "actionType") == "moveCardToTop" {
		sort = lo - 1
	}
	if to.ID != c.BoardID {
		var from Board
		var raw bson.M
		if s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(c.BoardID)}).Decode(&from) != nil ||
			s.db.Collection("cards").FindOne(ctx, bson.M{"_id": c.ID}).Decode(&raw) != nil {
			return nil
		}
		if err := s.moveCardTo(ctx, actor, raw, &from, to, swimlaneID, list.ID); err != nil {
			return err
		}
		_, err := s.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": c.ID}, bson.M{"$set": bson.M{"sort": sort}})
		s.hub.publish(to.ID, "board-changed")
		return err
	}
	if c.ListID == list.ID && c.SwimlaneID == swimlaneID && c.Sort == sort {
		return nil
	}
	if _, err := s.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": c.ID}, bson.M{"$set": bson.M{
		"listId": list.ID, "swimlaneId": swimlaneID, "sort": sort, "modifiedAt": time.Now().UTC()}}); err != nil {
		return err
	}
	s.hub.publish(c.BoardID, "board-changed")
	if c.ListID != list.ID || c.SwimlaneID != swimlaneID {
		s.activity(ctx, docString(a, "userId"), "moveCard", bson.M{"boardId": c.BoardID, "cardId": c.ID, "cardTitle": c.Title,
			"listId": list.ID, "oldListId": c.ListID, "swimlaneId": swimlaneID, "oldSwimlaneId": c.SwimlaneID})
	}
	return nil
}

// ruleCreateCard is WeKan's createCard action: a card in a list - named, or
// the card's own - of the rule's board, in its first swimlane or the named one.
func (s *Server) ruleCreateCard(ctx context.Context, a, act bson.M, b *Board, c *Card, actor *User, vars map[string]string) error {
	title := strings.TrimSpace(substituteVars(docString(act, "cardName"), vars))
	if title == "" || actor == nil {
		return nil
	}
	var list List
	name := docString(act, "listName")
	if name != "" && name != "*" {
		if s.db.Collection("lists").FindOne(ctx, bson.M{"boardId": b.ID, "title": eq(substituteVars(name, vars))}).Decode(&list) != nil {
			return nil
		}
	} else if c == nil || s.db.Collection("lists").FindOne(ctx, bson.M{"_id": eq(c.ListID)}).Decode(&list) != nil {
		return nil
	}
	swimlaneID := ""
	if n := docString(act, "swimlaneName"); n != "" && n != "*" {
		var sw Swimlane
		if s.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID, "title": eq(substituteVars(n, vars))}).Decode(&sw) == nil {
			swimlaneID = sw.ID
		}
	} else if c != nil {
		swimlaneID = c.SwimlaneID
	}
	if swimlaneID == "" {
		swimlaneID = s.firstSwimlane(ctx, b.ID)
	}
	_, err := s.AddCard(ctx, b, actor, list.ID, swimlaneID, title)
	return err
}

// recordSecurityCtx records a security event outside a request.
func (s *Server) recordSecurityCtx(ctx context.Context, e securityEvent) {
	r, _ := http.NewRequestWithContext(ctx, "POST", "/", nil)
	s.recordSecurity(r, e)
}

// methodRunButton is WeKan's rules.runButton(ruleId, cardId): a button rule of
// a board, run by whoever may change that board - on a card of that board,
// and an assigned-only member only on a card assigned to them. Anything else
// is not-authorized and recorded as RuleButtonBleed.
func (s *Server) methodRunButton(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	ruleID, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	cardID, err := optionalString(args, 1)
	if err != nil {
		return nil, err
	}
	ctx := r.Context()
	var rule bson.M
	if !validID(ruleID) || s.db.Collection("rules").FindOne(ctx, bson.M{"_id": eq(ruleID)}).Decode(&rule) != nil {
		return nil, &methodError{"not-found", "Rule not found"}
	}
	if rule["enabled"] == false {
		return nil, &methodError{"rule-disabled", "Rule is disabled"}
	}
	var trigger bson.M
	if s.db.Collection("triggers").FindOne(ctx, bson.M{"_id": eq(docString(rule, "triggerId"))}).Decode(&trigger) != nil || trigger["activityType"] != "button" {
		return nil, &methodError{"not-a-button", "Rule is not a button rule"}
	}
	var b Board
	allowed := s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(docString(rule, "boardId"))}).Decode(&b) == nil && b.Can(u.ID, "write")
	if allowed && cardID != "" {
		var c Card
		allowed = validID(cardID) && s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(cardID)}).Decode(&c) == nil && c.BoardID == b.ID &&
			(b.member(u.ID) == nil || !b.member(u.ID).AssignedOnly() || slices.Contains(c.Assignees, u.ID))
	}
	if !allowed {
		s.recordSecurity(r, securityEvent{Key: "authz.rule-button", Action: "blocked", Source: "rule:button",
			Detail: "Button rule denied because its caller or card is outside the writable board context."})
		return nil, &methodError{"not-authorized", "Button rule context is not authorized."}
	}
	a := bson.M{"activityType": "button", "boardId": b.ID, "userId": u.ID}
	if cardID != "" {
		a["cardId"] = cardID
	}
	rctx := context.WithValue(ctx, ruleDepthKey{}, 1)
	for _, act := range s.ruleActions(ctx, rule) {
		if err := s.performAction(rctx, a, act); err != nil {
			return nil, err
		}
	}
	return true, nil
}

// ruleButton is a button rule as the board shows it: WeKan's boardButtons
// and cardButtons.
type ruleButton struct{ ID, Label string }

// buttonRules are the board's enabled button rules of a type, "board" or
// "card", each labelled with its button label or its title.
func (s *Server) buttonRules(ctx context.Context, boardID, buttonType string) []ruleButton {
	cur, err := s.db.Collection("rules").Find(ctx, bson.M{"boardId": eq(boardID), "buttonType": eq(buttonType)},
		options.Find().SetSort(bson.D{{Key: "createdAt", Value: 1}}))
	if err != nil {
		return nil
	}
	var rules []bson.M
	_ = cur.All(ctx, &rules)
	var out []ruleButton
	for _, r := range rules {
		if r["enabled"] == false {
			continue
		}
		label := docString(r, "buttonLabel")
		if label == "" {
			label = docString(r, "title")
		}
		out = append(out, ruleButton{ID: docString(r, "_id"), Label: label})
	}
	return out
}

// ruleRow is a rule as the Rules page lists it.
type ruleRow struct {
	ID, Title, Trigger string
	Actions            []string
	Enabled            bool
}

// rulesPage is WeKan's rulesMain: for a board admin, the board's rules and
// the wizard that makes one; for anybody else, nothing of them.
func (s *Server) rulesPage(w http.ResponseWriter, r *http.Request, u *User) {
	b, err := s.boardFor(r.Context(), r.PathValue("board"), u)
	lang := languageOf(r, u).Tag
	if err != nil {
		s.render(w, r, "notfound", http.StatusNotFound, map[string]any{"Title": tr(lang, "board-not-found"), "User": u})
		return
	}
	data := map[string]any{"Title": tr(lang, "rules"), "User": u, "Board": b, "CanEdit": b.AdminOf(u.ID)}
	if b.AdminOf(u.ID) {
		ctx := r.Context()
		cur, err := s.db.Collection("rules").Find(ctx, bson.M{"boardId": b.ID}, options.Find().SetSort(bson.D{{Key: "createdAt", Value: 1}}))
		rows := []ruleRow{}
		if err == nil {
			var rules []bson.M
			_ = cur.All(ctx, &rules)
			for _, x := range rules {
				row := ruleRow{ID: docString(x, "_id"), Title: docString(x, "title"), Enabled: x["enabled"] != false}
				var t bson.M
				if s.db.Collection("triggers").FindOne(ctx, bson.M{"_id": eq(docString(x, "triggerId"))}).Decode(&t) == nil {
					row.Trigger = docString(t, "desc")
					if row.Trigger == "" {
						row.Trigger = docString(t, "activityType")
					}
				}
				for _, a := range s.ruleActions(ctx, x) {
					d := docString(a, "desc")
					if d == "" {
						d = docString(a, "actionType")
					}
					row.Actions = append(row.Actions, d)
				}
				rows = append(rows, row)
			}
			// ruleDetails.js: each description shown with a capital first letter.
			for i := range rows {
				rows[i].Trigger = capitalFirst(rows[i].Trigger)
				for j := range rows[i].Actions {
					rows[i].Actions[j] = capitalFirst(rows[i].Actions[j])
				}
			}
		}
		data["Rules"] = rows
		data["Labels"] = b.Labels
		boards, _ := s.boardsOf(ctx, u.ID)
		writable := []Board{}
		for _, x := range boards {
			if x.Writable(u, "write", nil) {
				writable = append(writable, x)
			}
		}
		data["Boards"] = writable
	}
	s.render(w, r, "rules", http.StatusOK, data)
}

// adminRule is a rule of a board the user administers, or the error to answer.
func (s *Server) adminRule(r *http.Request, u *User, args []json.RawMessage) (bson.M, error) {
	ruleID, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	var rule bson.M
	if !validID(ruleID) || s.db.Collection("rules").FindOne(r.Context(), bson.M{"_id": eq(ruleID)}).Decode(&rule) != nil {
		return nil, &methodError{"not-found", "Rule not found"}
	}
	var b Board
	if s.db.Collection("boards").FindOne(r.Context(), bson.M{"_id": eq(docString(rule, "boardId"))}).Decode(&b) != nil || !b.AdminOf(u.ID) {
		return nil, &methodError{"not-authorized", "Must be a board admin"}
	}
	return rule, nil
}

// methodDeleteRule is WeKan's rules.deleteRule: a board admin removes a rule,
// with its trigger and action when no other rule shares them.
func (s *Server) methodDeleteRule(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	rule, err := s.adminRule(r, u, args)
	if err != nil {
		return nil, err
	}
	return true, s.removeRule(r.Context(), rule)
}

// methodSetRuleEnabled is WeKan's rules.setEnabled (#2322): a disabled rule
// keeps its trigger and action, and never fires.
func (s *Server) methodSetRuleEnabled(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	rule, err := s.adminRule(r, u, args)
	if err != nil {
		return nil, err
	}
	var on bool
	if len(args) < 2 || json.Unmarshal(args[1], &on) != nil {
		return nil, &methodError{"Match.Error", "Expected boolean"}
	}
	_, err = s.db.Collection("rules").UpdateOne(r.Context(), bson.M{"_id": rule["_id"]}, bson.M{"$set": bson.M{"enabled": on, "modifiedAt": time.Now().UTC()}})
	return true, err
}

// capitalFirst is the text with its first letter in capitals.
func capitalFirst(s string) string {
	for i, r := range s {
		return strings.ToUpper(string(r)) + s[i+len(string(r)):]
	}
	return s
}
