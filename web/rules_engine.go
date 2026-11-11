package web

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"slices"
	"sort"
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
	if !validID(boardID) {
		return nil
	}
	var triggers []bson.M
	if known {
		cur, err := s.db.Collection("triggers").Find(ctx, bson.M{"activityType": eq(kind)})
		if err != nil {
			return nil
		}
		_ = cur.All(ctx, &triggers)
	}
	values := s.ruleValues(ctx, a, fields)
	var rules []bson.M
	seen := map[string]bool{}
	// #4294 / #3195: a trigger value may hold the action {tokens}, read for
	// the card the activity is about - built once, only when one does.
	var vars map[string]string
	tokenValues := func() map[string]string {
		if vars == nil {
			var c *Card
			var card Card
			if s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(docString(a, "cardId"))}).Decode(&card) == nil {
				c = &card
			}
			var actor *User
			var au User
			if s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(docString(a, "userId"))}).Decode(&au) == nil {
				actor = &au
			}
			vars = s.ruleVars(ctx, a, c, actor)
		}
		return vars
	}
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
			if triggerHasVars(want) {
				actual := values[f]
				if f == "userId" {
					// People tokens resolve to usernames; the activity names its actor by id.
					var au User
					actual = ""
					if s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(values[f])}).Decode(&au) == nil {
						actual = au.Username
					}
				}
				ok = ok && triggerVarMatches(want.(string), actual, tokenValues())
				continue
			}
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
		rules = s.addTriggerRules(ctx, boardID, docString(t, "_id"), seen, rules)
	}
	// #2076: "a card is moved forward / back" - to a later or earlier list in
	// the board's list order - is matched by comparing the two lists.
	if oldList, newList := docString(a, "oldListId"), docString(a, "listId"); kind == "moveCard" && validID(oldList) && validID(newList) && oldList != newList {
		if direction := s.moveDirection(ctx, oldList, newList); direction != "" {
			cur, err := s.db.Collection("triggers").Find(ctx, bson.M{"activityType": "moveCardDirection", "boardId": eq(boardID)})
			if err == nil {
				var dt []bson.M
				_ = cur.All(ctx, &dt)
				title := ""
				var card struct {
					Title string `bson:"title"`
				}
				if s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(docString(a, "cardId"))}).Decode(&card) == nil {
					title = card.Title
				}
				for _, t := range dt {
					want, has := t["cardTitle"]
					if docString(t, "direction") != direction || (has && !cardTitleMatches(want, title)) {
						continue
					}
					rules = s.addTriggerRules(ctx, boardID, docString(t, "_id"), seen, rules)
				}
			}
		}
	}
	// #3092: "a card matches the advanced filter" - the Filter sidebar's own
	// language, against the card's current state, on any card activity.
	if cardID := docString(a, "cardId"); validID(cardID) {
		cur, err := s.db.Collection("triggers").Find(ctx, bson.M{"activityType": "advancedFilterTrigger", "boardId": eq(boardID)})
		if err == nil {
			var at []bson.M
			_ = cur.All(ctx, &at)
			for _, t := range at {
				if s.cardMatchesAdvancedFilter(ctx, boardID, cardID, docString(t, "advancedFilter")) {
					rules = s.addTriggerRules(ctx, boardID, docString(t, "_id"), seen, rules)
				}
			}
		}
	}
	// #2194: "a card's title or description contains" - the card's text now,
	// case-insensitive - on what changes that text: a new card, a title or a
	// description changed.
	if (kind == "createCard" || kind == "a-changedTitle" || kind == "a-changedDescription") && validID(docString(a, "cardId")) {
		var card struct {
			Title       string `bson:"title"`
			Description string `bson:"description"`
		}
		if s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(docString(a, "cardId")), "boardId": eq(boardID)}).Decode(&card) == nil {
			cur, err := s.db.Collection("triggers").Find(ctx, bson.M{"activityType": "textContainsTrigger", "boardId": eq(boardID)})
			if err == nil {
				var tt []bson.M
				_ = cur.All(ctx, &tt)
				for _, t := range tt {
					needle := docString(t, "textContains")
					if textContains(card.Title, needle) || textContains(card.Description, needle) {
						rules = s.addTriggerRules(ctx, boardID, docString(t, "_id"), seen, rules)
					}
				}
			}
		}
	}
	return rules
}

// textContains is WeKan's textContainsMatch: a case-insensitive substring; an
// empty value, or an empty text, never matches.
func textContains(text, needle string) bool {
	return strings.TrimSpace(needle) != "" && text != "" && strings.Contains(strings.ToLower(text), strings.ToLower(needle))
}

// copyBoard is a board the Copy Card action may name, with its lists and
// swimlanes.
type copyBoard struct {
	ID, Title        string
	Current          bool
	Lists, Swimlanes []copyTarget
}

// copyTarget is a list or a swimlane of a board the copy may name.
type copyTarget struct{ ID, Title, Board string }

// copyBoards are WeKan's boardActions boards: those the user may change, not
// archived and not templates - the current one first - with their lists and
// swimlanes, as the Copy Card action offers them.
func (s *Server) copyBoards(ctx context.Context, u *User, current string) []copyBoard {
	cur, err := s.db.Collection("boards").Find(ctx, bson.M{"members.userId": eq(u.ID), "archived": bson.M{"$ne": true}},
		options.Find().SetSort(bson.D{{Key: "title", Value: 1}}).SetLimit(500))
	if err != nil {
		return nil
	}
	var boards []Board
	_ = cur.All(ctx, &boards)
	var out []copyBoard
	for i := range boards {
		b := &boards[i]
		if !b.Can(u.ID, "write") || strings.HasPrefix(b.Type, "template") {
			continue
		}
		cb := copyBoard{ID: b.ID, Title: b.Title, Current: b.ID == current}
		for _, part := range []struct {
			coll string
			to   *[]copyTarget
		}{{"lists", &cb.Lists}, {"swimlanes", &cb.Swimlanes}} {
			c2, err := s.db.Collection(part.coll).Find(ctx, bson.M{"boardId": b.ID, "archived": bson.M{"$ne": true}},
				options.Find().SetSort(bson.D{{Key: "sort", Value: 1}}).SetProjection(bson.M{"title": 1}).SetLimit(500))
			if err != nil {
				continue
			}
			var docs []bson.M
			_ = c2.All(ctx, &docs)
			for _, d := range docs {
				*part.to = append(*part.to, copyTarget{ID: docString(d, "_id"), Title: docString(d, "title"), Board: b.ID})
			}
		}
		if cb.Current {
			out = append([]copyBoard{cb}, out...)
		} else {
			out = append(out, cb)
		}
	}
	return out
}

// ruleVariableTokens is WeKan's models/lib/ruleVariablePicker.js: the card's
// people, its card, list and board names, and a {customField:Name} for each
// of the board's custom fields - not an admin-only one, nor a name with a
// brace, which the token syntax cannot spell.
func (s *Server) ruleVariableTokens(ctx context.Context, boardID string) []string {
	out := []string{"{creator}", "{assignees}", "{members}", "{card}", "{list}", "{board}"}
	cur, err := s.db.Collection("customFields").Find(ctx, bson.M{"boardIds": eq(boardID)}, options.Find().SetSort(bson.D{{Key: "name", Value: 1}}))
	if err != nil {
		return out
	}
	var defs []struct {
		Name      string `bson:"name"`
		AdminOnly bool   `bson:"adminOnly"`
	}
	_ = cur.All(ctx, &defs)
	for _, d := range defs {
		name := strings.TrimSpace(d.Name)
		if tok := "{customField:" + name + "}"; !d.AdminOnly && name != "" && !strings.ContainsAny(name, "{}") && !slices.Contains(out, tok) {
			out = append(out, tok)
		}
	}
	return out
}

// moveDirection is WeKan's models/lib/ruleMoveDirection.js: "forward" to a
// list later in the board's list order, "back" to an earlier one - the
// lists' sort, not the screen, so it holds in right-to-left languages too.
func (s *Server) moveDirection(ctx context.Context, oldListID, newListID string) string {
	var from, to struct {
		BoardID string `bson:"boardId"`
		Sort    any    `bson:"sort"`
	}
	if s.db.Collection("lists").FindOne(ctx, bson.M{"_id": eq(oldListID)}).Decode(&from) != nil ||
		s.db.Collection("lists").FindOne(ctx, bson.M{"_id": eq(newListID)}).Decode(&to) != nil || from.BoardID != to.BoardID {
		return ""
	}
	a, b := numberOf(from.Sort), numberOf(to.Sort)
	if math.IsNaN(a) || math.IsNaN(b) || math.IsInf(a, 0) || math.IsInf(b, 0) || a == b {
		return ""
	}
	if b > a {
		return "forward"
	}
	return "back"
}

// numberOf is a stored number, NaN for anything else.
func numberOf(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int32:
		return float64(x)
	case int64:
		return float64(x)
	}
	return math.NaN()
}

// addTriggerRules adds the board's enabled rules of a trigger, each once.
func (s *Server) addTriggerRules(ctx context.Context, boardID, tid string, seen map[string]bool, rules []bson.M) []bson.M {
	cur, err := s.db.Collection("rules").Find(ctx, bson.M{"boardId": eq(boardID), "$or": bson.A{
		bson.M{"triggerId": eq(tid)}, bson.M{"extraTriggerIds": eq(tid)}}})
	if err != nil {
		return rules
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
	// A card date cleared is not "a card's date is set or changed" (#2474).
	if isTimingClear(a) {
		return
	}
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
	// #3304: {member} - the member the activity is about, else its actor.
	member := docString(a, "memberId")
	if member == "" {
		member = docString(a, "userId")
	}
	var mu User
	if validID(member) && s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(member)}).Decode(&mu) == nil {
		v["membername"], v["member"] = mu.Username, mu.Username
	}
	// #3304: the short aliases.
	if c != nil {
		v["card"], v["list"] = v["cardname"], v["listname"]
	}
	v["board"] = v["boardname"]
	if c != nil {
		s.cardPeopleAndFields(ctx, v, c)
		// #3301: {cardLink}, the card's own address.
		var cb Board
		if s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(c.BoardID)}).Decode(&cb) == nil {
			if link := cardLink(&cb, c); link != "" {
				v["cardlink"] = link
			}
		}
	}
	return v
}

// cardPeopleAndFields is WeKan's addCardPeopleAndFields: {creator},
// {assignees} and {members} by username, and one {customField:Name} a custom
// field of the board with a value on the card - its displayed value, a
// dropdown's option names. Admin-only fields are left out: a rule must not
// copy them into card text or mail.
func (s *Server) cardPeopleAndFields(ctx context.Context, v map[string]string, c *Card) {
	names := func(ids []string) string {
		var out []string
		for _, id := range ids {
			var u User
			if validID(id) && s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&u) == nil && u.Username != "" && !slices.Contains(out, u.Username) {
				out = append(out, u.Username)
			}
		}
		return strings.Join(out, ", ")
	}
	v["creator"], v["assignees"], v["members"] = names([]string{c.UserID}), names(c.Assignees), names(c.Members)
	var raw bson.M
	if s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(c.ID)}).Decode(&raw) != nil {
		return
	}
	values, _ := raw["customFields"].(bson.A)
	for _, e := range values {
		f := docMap(e.(bson.D))
		var def bson.M
		if s.db.Collection("customFields").FindOne(ctx, bson.M{"_id": eq(strOf(f["_id"])), "boardIds": eq(c.BoardID)}).Decode(&def) != nil || def["adminOnly"] == true {
			continue
		}
		value := ""
		switch def["type"] {
		case "dropdown", "dropdownMultiSelect":
			opts := dropdownItems(def)
			ids := stringsOf(f["value"])
			if one, ok := f["value"].(string); ok {
				ids = []string{one}
			}
			var got []string
			for _, id := range ids {
				if opts[id] != "" {
					got = append(got, opts[id])
				}
			}
			value = strings.Join(got, ", ")
		default:
			switch x := plain(f["value"]).(type) {
			case string:
				value = x
			case nil:
			case []any:
				var parts []string
				for _, p := range x {
					parts = append(parts, fmt.Sprint(p))
				}
				value = strings.Join(parts, ", ")
			case time.Time:
				value = x.Format(time.RFC3339)
			default:
				value = fmt.Sprint(x)
			}
		}
		if name := strings.ToLower(strings.TrimSpace(strOf(def["name"]))); name != "" && value != "" {
			v["customfield:"+name] = value
		}
	}
}

var ruleVarPattern = regexp.MustCompile(`\{(\w+)(?::([^{}]+))?\}`)

// substituteVars is WeKan's: {name} becomes the variable, {customField:Name}
// the card's value of that field (any case) - and an unknown one stays.
func substituteVars(text string, vars map[string]string) string {
	return ruleVarPattern.ReplaceAllStringFunc(text, func(m string) string {
		p := ruleVarPattern.FindStringSubmatch(m)
		key := strings.ToLower(p[1])
		if strings.Contains(m, ":") {
			if key != "customfield" {
				return m
			}
			key = "customfield:" + strings.ToLower(strings.TrimSpace(p[2]))
		}
		if v, ok := vars[key]; ok {
			return v
		}
		return m
	})
}

// triggerHasVars is WeKan's triggerValueHasVars.
func triggerHasVars(v any) bool {
	s, ok := v.(string)
	return ok && ruleVarPattern.MatchString(s)
}

// listTokens name several people; a trigger value that is one of them
// matches any one of those names.
var listTokens = regexp.MustCompile(`(?i)^\{(?:creator|assignees|members)\}$`)

// triggerVarMatches is WeKan's: the value with its variables resolved for the
// card is what the activity carries.
func triggerVarMatches(pattern, actual string, vars map[string]string) bool {
	if actual == "" {
		return false
	}
	resolved := substituteVars(pattern, vars)
	if listTokens.MatchString(strings.TrimSpace(pattern)) {
		for _, n := range strings.Split(resolved, ",") {
			if strings.TrimSpace(n) == actual {
				return true
			}
		}
		return false
	}
	return resolved == actual
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
	case "copyCard":
		return s.ruleCopyCard(ctx, a, act, c)
	case "linkCard":
		return s.ruleLinkCard(ctx, a, act, c)
	case "sendEmail":
		return s.ruleSendEmail(ctx, a, act, c, vars)
	case "dispatchBot":
		// Board Settings / Bots: hand the card to a bot of its own board
		// (bots.go) - the bot is sent the card, and nothing else.
		if id := docString(act, "integrationId"); id != "" {
			_, err := s.dispatchToBot(ctx, id, c.ID, userID, "rule")
			return err
		}
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
		if _, err := s.insertRecorded(ctx, docString(a, "userId"), "checklist", bson.M{"_id": id, "title": title, "cardId": c.ID, "boardId": c.BoardID, // inserted, not a filter
			"sort": 0, "createdAt": now}); err != nil { // inserted, not a filter
			return err
		}
		cardActivity("addChecklist", bson.M{"checklistId": id, "checklistName": title})
		if kind == "addChecklistWithItems" {
			for i, t := range strings.Split(substituteVars(docString(act, "checklistItems"), vars), ",") {
				if _, err := s.insertRecorded(ctx, docString(a, "userId"), "checklistItem", bson.M{"_id": randomID(), "title": t, "checklistId": id, // inserted, not a filter
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
			_, _ = s.deleteOneRecorded(ctx, docString(a, "userId"), "checklist", bson.M{"_id": cl.ID})
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
		if _, err := s.insertRecorded(ctx, docString(a, "userId"), "comment", bson.M{"_id": id, "boardId": c.BoardID, "cardId": c.ID, // inserted, not a filter
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
	case "setDateRelative":
		// #5621: a date field set to now and an offset - minutes, hours, days
		// (the unit of rules made before there was a choice), weeks or months.
		field := docString(act, "dateField")
		if !slices.Contains([]string{"startAt", "endAt", "dueAt", "receivedAt"}, field) {
			return nil
		}
		if err := touch(bson.M{"$set": bson.M{field: relativeDateOffset(now, act["days"], docString(act, "unit"))}}); err != nil {
			return err
		}
		cardActivity("a-"+field, nil)
	case "sortList":
		return s.ruleSortList(ctx, act, c)
	}
	return nil
}

// relativeDateOffset is WeKan's models/lib/relativeDateOffset.js: base moved
// by a whole number of units - parseInt's, so "3 days" is 3 and nonsense 0 -
// months by the calendar, as JavaScript's setMonth: 31 January and one month
// is early March.
func relativeDateOffset(base time.Time, value any, unit string) time.Time {
	amount := 0
	switch v := value.(type) {
	case float64:
		amount = int(v)
	case int32:
		amount = int(v)
	case int64:
		amount = int(v)
	case string:
		m := regexp.MustCompile(`^\s*([+-]?\d+)`).FindStringSubmatch(v)
		if m != nil {
			amount, _ = strconv.Atoi(m[1])
		}
	}
	switch unit {
	case "minutes":
		return base.Add(time.Duration(amount) * time.Minute)
	case "hours":
		return base.Add(time.Duration(amount) * time.Hour)
	case "weeks":
		return base.Add(time.Duration(amount) * 7 * 24 * time.Hour)
	case "months":
		return base.AddDate(0, amount, 0)
	}
	return base.Add(time.Duration(amount) * 24 * time.Hour)
}

// ruleSortList is WeKan's sortList action (models/lib/ruleSortList.js): the
// cards of the card's list - or the list it names, on the card's board - in
// the card's swimlane, by name, creation, modification or due date (the
// default, cards without one last), each card then taking its index as its sort.
func (s *Server) ruleSortList(ctx context.Context, act bson.M, c *Card) error {
	listID := c.ListID
	if name := docString(act, "listName"); name != "" && name != "*" {
		var l struct {
			ID string `bson:"_id"`
		}
		if s.db.Collection("lists").FindOne(ctx, bson.M{"title": eq(name), "boardId": eq(c.BoardID)}).Decode(&l) != nil {
			return nil
		}
		listID = l.ID
	}
	cur, err := s.db.Collection("cards").Find(ctx, bson.M{"listId": eq(listID), "swimlaneId": eq(c.SwimlaneID), "archived": false},
		options.Find().SetSort(bson.D{{Key: "sort", Value: 1}}))
	if err != nil {
		return err
	}
	var cards []bson.M
	_ = cur.All(ctx, &cards)
	key := func(card bson.M) any {
		t := func(k string) float64 {
			if v, ok := plain(card[k]).(time.Time); ok {
				return float64(v.UnixMilli())
			}
			return 0
		}
		switch docString(act, "sortField") {
		case "name":
			return strings.ToLower(docString(card, "title"))
		case "created":
			return t("createdAt")
		case "modified":
			return t("modifiedAt")
		}
		if v, ok := plain(card["dueAt"]).(time.Time); ok {
			return float64(v.UnixMilli())
		}
		return float64(1<<53 - 1)
	}
	sort.SliceStable(cards, func(i, j int) bool {
		a, b := key(cards[i]), key(cards[j])
		if as, ok := a.(string); ok {
			return as < b.(string)
		}
		return a.(float64) < b.(float64)
	})
	for i, card := range cards {
		if _, err := s.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": eq(docString(card, "_id"))}, bson.M{"$set": bson.M{"sort": float64(i)}}); err != nil {
			return err
		}
	}
	s.hub.publish(c.BoardID, "board-changed")
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
		_, err := s.cardsUpdateOne(ctx, actorID(actor), bson.M{"_id": c.ID}, bson.M{"$set": bson.M{"sort": sort}})
		s.hub.publish(to.ID, "board-changed")
		return err
	}
	if c.ListID == list.ID && c.SwimlaneID == swimlaneID && c.Sort == sort {
		return nil
	}
	if _, err := s.cardsUpdateOne(ctx, actorID(actor), bson.M{"_id": c.ID}, bson.M{"$set": bson.M{
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
	TriggerDoc         string // the trigger in the Rules export's portable shape, as JSON
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
	view := r.URL.Query().Get("view")
	if !slices.Contains([]string{"list", "workflow", "import", "bots", "blocks"}, view) {
		view = "list"
	}
	data := map[string]any{"Title": tr(lang, "rules"), "User": u, "Board": b, "CanEdit": b.AdminOf(u.ID), "View": view,
		"RuleTokens": s.ruleVariableTokens(r.Context(), b.ID), "CopyBoards": s.copyBoards(r.Context(), u, b.ID),
		"TriggerPalette": paletteChips(lang, triggerPalette), "ActionPalette": paletteChips(lang, actionPalette)}
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
					if raw, err := json.Marshal(portableRule(t)); err == nil {
						row.TriggerDoc = string(raw)
					}
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
		switch view {
		case "bots":
			s.botsViewData(ctx, b, r.URL.Query().Get("card"), lang, data)
		case "blocks":
			s.blocksViewData(ctx, b, lang, languageOf(r, u).RTL, data)
		}
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
	ruleID := docString(rule, "_id")
	beforeRule := s.ruleSnapshot(r.Context(), ruleID)
	defer func() { s.recordRuleChange(r.Context(), beforeRule, s.ruleSnapshot(r.Context(), ruleID), u.ID) }()
	return true, s.removeRule(r.Context(), rule)
}

// methodSetRuleEnabled is WeKan's rules.setEnabled (#2322): a disabled rule
// keeps its trigger and action, and never fires.
func (s *Server) methodSetRuleEnabled(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	rule, err := s.adminRule(r, u, args)
	if err != nil {
		return nil, err
	}
	ruleID := docString(rule, "_id")
	beforeRule := s.ruleSnapshot(r.Context(), ruleID)
	defer func() { s.recordRuleChange(r.Context(), beforeRule, s.ruleSnapshot(r.Context(), ruleID), u.ID) }()
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

// rulesFormat is WeKan's RULES_FORMAT, the version of an exported rules file.
const rulesFormat = "wekan-rules-1.0.0"

// portableRule is a trigger or an action without what ties it to one board
// (WeKan's stripDoc).
func portableRule(doc bson.M) bson.M {
	out := bson.M{}
	for k, v := range doc {
		if !slices.Contains([]string{"_id", "boardId", "createdAt", "modifiedAt", "updatedAt"}, k) {
			out[k] = v
		}
	}
	return out
}

// rulesExport is WeKan's rules export (rulesImportExport.js): the board's
// rules - the ticked ones, when some are - as JSON, or as CSV with WeKan's
// columns, for a board admin.
func (s *Server) rulesExport(w http.ResponseWriter, r *http.Request, u *User) {
	b, err := s.boardFor(r.Context(), r.PathValue("board"), u)
	if err != nil || !b.AdminOf(u.ID) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	ctx := r.Context()
	cur, err := s.db.Collection("rules").Find(ctx, bson.M{"boardId": b.ID}, options.Find().SetSort(bson.D{{Key: "createdAt", Value: 1}}))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var rules []bson.M
	_ = cur.All(ctx, &rules)
	picked := strings.Split(r.URL.Query().Get("ids"), ",")
	entries := []bson.M{}
	for _, rule := range rules {
		if r.URL.Query().Get("ids") != "" && !slices.Contains(picked, docString(rule, "_id")) {
			continue
		}
		var t bson.M
		acts := s.ruleActions(ctx, rule)
		if s.db.Collection("triggers").FindOne(ctx, bson.M{"_id": eq(docString(rule, "triggerId"))}).Decode(&t) != nil || len(acts) == 0 {
			continue
		}
		e := bson.M{"title": rule["title"], "trigger": portableRule(t), "action": portableRule(acts[0])}
		if len(acts) > 1 {
			extra := []bson.M{}
			for _, a := range acts[1:] {
				extra = append(extra, portableRule(a))
			}
			e["extraActions"] = extra
		}
		entries = append(entries, e)
	}
	// The board as a Wrike workflow: every rule, since the workflow is the board's.
	if r.URL.Query().Get("format") == "wrike" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", `attachment; filename="wrike-workflow.json"`)
		// WeKan's JSON.stringify(workflow, null, 2).
		_, _ = w.Write(jsJSON(wrikeWorkflowFromBoard(b.Title, s.boardLists(ctx, b.ID), s.boardRuleParts(ctx, b.ID)), "  "))
		return
	}
	if r.URL.Query().Get("format") == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="wekan-rules.csv"`)
		cw := csv.NewWriter(w)
		cw.UseCRLF = true
		_ = cw.Write([]string{"title", "triggerType", "triggerFields", "actionType", "actionFields"})
		for _, e := range entries {
			t, a := e["trigger"].(bson.M), e["action"].(bson.M)
			tf, af := bson.M{}, bson.M{}
			for k, v := range t {
				if k != "activityType" {
					tf[k] = v
				}
			}
			for k, v := range a {
				if k != "actionType" {
					af[k] = v
				}
			}
			tj, _ := json.Marshal(tf)
			aj, _ := json.Marshal(af)
			title, _ := e["title"].(string)
			_ = cw.Write([]string{title, docString(t, "activityType"), string(tj), docString(a, "actionType"), string(aj)})
		}
		cw.Flush()
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="wekan-rules.json"`)
	out, _ := json.MarshalIndent(map[string]any{"_format": rulesFormat, "boardId": b.ID, "rules": entries}, "", "  ")
	_, _ = w.Write(out)
}
