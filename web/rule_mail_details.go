package web

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// The mail action's "Details", as Node.js WeKan's server/lib/ruleCardDetails.js,
// for an ordinary card: its board, list and swimlane, its own fields and
// dates, its people by display name only, labels, stickers, locations, custom
// fields (an admin-only one only for a board admin), text notes, and the
// cards it names - parents, dependencies and subtasks - each only when the
// rule's user may read that card too. The card must be readable before the
// reads and after them; a board admin who stops being one mid-way gets none.
// Votes, planning poker and Scrum fields are rule_mail_voting.go's, a
// string-template field's display string_template.go's. Not here yet: a
// linked card's or a linked board's source.

var errRuleMailDetails = errors.New("rule-email-details-not-authorized")

// ruleMailScalar is WeKan's scalar: text, a number, true or false, or a date
// as ISO 8601; anything else is nothing.
func ruleMailScalar(v any) string {
	switch x := plain(v).(type) {
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case int32:
		return strconv.Itoa(int(x))
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case time.Time:
		if x.IsZero() {
			return ""
		}
		return x.UTC().Format("2006-01-02T15:04:05.000Z")
	}
	return ""
}

// ruleMailValues is WeKan's values: a list joined with ", ".
func ruleMailValues(v any) string {
	if list, ok := plain(v).([]any); ok {
		var out []string
		for _, e := range list {
			if s := ruleMailScalar(e); s != "" {
				out = append(out, s)
			}
		}
		return strings.Join(out, ", ")
	}
	return ruleMailScalar(v)
}

var ruleMailCardFields = [][2]string{{"cardNumber", "Card number"}, {"color", "Color"}, {"archived", "Archived"},
	{"createdAt", "Created"}, {"modifiedAt", "Modified"}, {"archivedAt", "Archived at"}, {"dateLastActivity", "Last activity"},
	{"sort", "Sort"}, {"subtaskSort", "Subtask sort"}, {"lastMoveReason", "Last move reason"}, {"listEnteredAt", "Entered list"},
	{"receivedAt", "Received"}, {"startAt", "Start"}, {"dueAt", "Due"}, {"endAt", "End"}, {"dueComplete", "Due complete"},
	{"requestedBy", "Requested by"}, {"assignedBy", "Assigned by"}, {"spentTime", "Spent time (hours)"}, {"isOvertime", "Overtime"},
	{"recurrenceInterval", "Recurrence"}, {"lastRecurrenceAt", "Last recurrence"},
	{"flowStartAt", "Flowtime started"}, {"flowInterruptions", "Flowtime interruptions"},
	{"pomodoroStartAt", "Pomodoro started"}, {"pomodoroPhase", "Pomodoro phase"},
	{"pomodoroCount", "Pomodoro completed intervals"}, {"pomodoroWorkMinutes", "Pomodoro work interval (minutes)"},
	{"locationName", "Location"}, {"locationAddress", "Address"}, {"locationLatitude", "Latitude"}, {"locationLongitude", "Longitude"}}

// The Gantt link types, as DHTMLX numbers them.
var ruleMailGanttTypes = []string{"finish-to-start", "start-to-start", "finish-to-finish", "start-to-finish"}

// ruleMailDetails is WeKan's prepareRuleCardDetails for an ordinary card.
func (s *Server) ruleMailDetails(ctx context.Context, userID, boardID, cardID string) (string, error) {
	if !s.ruleMailCardReadable(ctx, userID, boardID, cardID) {
		return "", errRuleMailDetails
	}
	var raw bson.M
	var b Board
	if s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(cardID)}).Decode(&raw) != nil ||
		s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(boardID)}).Decode(&b) != nil {
		return "", errRuleMailDetails
	}
	card, _ := plain(raw).(map[string]any)
	if t, _ := card["type"].(string); t == "cardType-linkedCard" || t == "cardType-linkedBoard" {
		return "", nil // a link's cached fields may be a now-private source's: not exported here
	}
	admin := b.AdminOf(userID)
	var lines []string
	size := 0
	tooLarge := false
	add := func(label string, v any) {
		text := ruleMailValues(v)
		if text == "" {
			return
		}
		line := label + ": " + text
		size += len(line) + 1
		if size > ruleMailDiscussionMax {
			tooLarge = true
			return
		}
		lines = append(lines, line)
	}
	str := func(v any) string { s, _ := v.(string); return s }
	ids := func(v any) []string {
		var out []string
		list, _ := v.([]any)
		for _, e := range list {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	titleIn := func(coll, id string) string {
		var d struct {
			Title   string `bson:"title"`
			BoardID string `bson:"boardId"`
		}
		if validID(id) && s.db.Collection(coll).FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&d) == nil && d.BoardID == boardID {
			return d.Title
		}
		return ""
	}

	add("Board", b.Title)
	add("List", titleIn("lists", str(card["listId"])))
	add("Swimlane", titleIn("swimlanes", str(card["swimlaneId"])))
	for _, f := range ruleMailCardFields {
		add(f[1], card[f[0]])
	}
	if id := str(card["userId"]); id != "" {
		add("Created by", s.ruleMailDisplayName(ctx, id))
	}
	stickers, _ := card["stickers"].([]any)
	for _, st := range stickers {
		m, ok := st.(map[string]any)
		if !ok {
			continue
		}
		var parts []any
		for _, k := range []string{"name", "icon", "highlight", "position"} {
			if v := ruleMailScalar(m[k]); v != "" {
				parts = append(parts, v)
			}
		}
		add("Sticker", parts)
	}
	for _, f := range [][2]string{{"flowUserId", "Flowtime user"}, {"pomodoroUserId", "Pomodoro user"}} {
		if id := str(card[f[0]]); id != "" {
			add(f[1], s.ruleMailDisplayName(ctx, id))
		}
	}
	rawBoard := s.ruleMailRawBoard(ctx, boardID)
	votingPolicy := ruleMailVotingPolicy(card, rawBoard)
	scrumPolicy := canonical(ruleMailScrumPolicy(rawBoard))
	s.ruleMailVoting(ctx, card, rawBoard, add)
	scrumCurrent := s.ruleMailScrum(ctx, card, rawBoard, boardID, add)
	labels := map[string]string{}
	for _, l := range b.Labels {
		name := l.Name
		if name == "" {
			name = l.Color
		}
		labels[l.ID] = name
	}
	var labelNames []any
	for _, id := range ids(card["labelIds"]) {
		if n := labels[id]; n != "" {
			labelNames = append(labelNames, n)
		}
	}
	add("Labels", labelNames)
	for _, f := range [][2]string{{"members", "Members"}, {"assignees", "Assignees"}, {"requesters", "Requesters"}, {"assigners", "Assigners"}} {
		var names []any
		for _, id := range ids(card[f[0]]) {
			var u User
			if s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&u) == nil {
				names = append(names, s.ruleMailDisplayName(ctx, id))
			}
		}
		add(f[1], names)
	}
	locations, _ := card["locations"].([]any)
	for _, l := range locations {
		m, _ := l.(map[string]any)
		var parts []any
		for _, k := range []string{"name", "address", "latitude", "longitude"} {
			if v := ruleMailScalar(m[k]); v != "" {
				parts = append(parts, v)
			}
		}
		add("Location", parts)
	}

	// Custom fields: this board's definitions, in their own order; an
	// admin-only one for a board admin alone; a dropdown as its item's name.
	definitions := func() []bson.M {
		cur, err := s.db.Collection("customFields").Find(ctx, bson.M{"boardIds": eq(boardID)}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
		if err != nil {
			return nil
		}
		var out []bson.M
		_ = cur.All(ctx, &out)
		return out
	}
	defs := definitions()
	defOf := map[string]map[string]any{}
	for _, d := range defs {
		m, _ := plain(d).(map[string]any)
		if m["adminOnly"] == true && !admin {
			continue
		}
		defOf[str(m["_id"])] = m
	}
	type cfRow struct {
		def   map[string]any
		value any
	}
	var rows []cfRow
	cfs, _ := card["customFields"].([]any)
	for _, e := range cfs {
		m, _ := e.(map[string]any)
		if d := defOf[str(m["_id"])]; d != nil {
			rows = append(rows, cfRow{d, m["value"]})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, aok := rows[i].def["sort"].(float64)
		c, cok := rows[j].def["sort"].(float64)
		if aok && cok && a != c {
			return a < c
		}
		if aok != cok {
			return aok
		}
		return str(rows[i].def["name"]) < str(rows[j].def["name"])
	})
	for _, r := range rows {
		settings, _ := r.def["settings"].(map[string]any)
		items, _ := settings["dropdownItems"].([]any)
		itemName := func(id any) any {
			for _, it := range items {
				if m, _ := it.(map[string]any); m != nil && m["_id"] == id {
					return m["name"]
				}
			}
			return id
		}
		value := r.value
		switch str(r.def["type"]) {
		case "dropdown":
			value = itemName(r.value)
		case "dropdownMultiSelect":
			var names []string
			list, _ := r.value.([]any)
			for _, id := range list {
				names = append(names, ruleMailScalar(itemName(id)))
			}
			value = strings.Join(names, ", ")
		case "stringtemplate":
			listTitle, laneTitle := titleIn("lists", str(card["listId"])), titleIn("swimlanes", str(card["swimlaneId"]))
			value = formatStringTemplate(r.value, str(settings["stringtemplateFormat"]), str(settings["stringtemplateSeparator"]),
				map[string]string{"card.title": str(card["title"]), "board.title": b.Title, "list.title": listTitle, "swimlane.title": laneTitle})
		case "currency":
			value = strings.TrimSpace(ruleMailScalar(r.value) + " " + str(settings["currencyCode"]))
		}
		add(str(r.def["name"]), value)
	}

	// Text notes.
	if cur, err := s.db.Collection("card_text_notes").Find(ctx, bson.M{"cardId": eq(cardID), "boardId": eq(boardID), "deletedAt": nil},
		options.Find().SetSort(bson.D{{Key: "createdAt", Value: 1}, {Key: "_id", Value: 1}})); err == nil {
		var notes []bson.M
		_ = cur.All(ctx, &notes)
		for _, n := range notes {
			title := docString(n, "title")
			if title == "" {
				title = "Note"
			}
			add(title, docString(n, "text"))
		}
	}

	// The cards it names, each only when its user may read that card.
	relation := func(label, id string) {
		var rc Card
		if validID(id) && s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&rc) == nil &&
			s.ruleMailCardReadable(ctx, userID, rc.BoardID, rc.ID) {
			add(label, rc.Title)
		}
	}
	seen := map[string]bool{}
	for _, id := range append([]string{str(card["parentId"])}, ids(card["parentIds"])...) {
		if id != "" && !seen[id] {
			seen[id] = true
			relation("Parent", id)
		}
	}
	var order []string
	labelsOf := map[string][]string{}
	addRel := func(id, label string) {
		if id == "" {
			return
		}
		if _, ok := labelsOf[id]; !ok {
			order = append(order, id)
		}
		if !strings.Contains(" / "+strings.Join(labelsOf[id], " / ")+" / ", " / "+label+" / ") {
			labelsOf[id] = append(labelsOf[id], label)
		}
	}
	deps, _ := card["cardDependencies"].([]any)
	for _, d := range deps {
		switch x := d.(type) {
		case string:
			addRel(x, defaultDependencyType)
		case map[string]any:
			dep := dependency{CardID: str(x["cardId"]), Type: str(x["type"])}
			if dep.CardID != "" {
				addRel(dep.CardID, dep.normalized().Type)
			}
		}
	}
	targets, _ := card["targetId_gantt"].([]any)
	types, _ := card["linkType_gantt"].([]any)
	for i, t := range targets {
		id, _ := t.(string)
		label := "Gantt target"
		if len(targets) == len(types) {
			if n, ok := types[i].(float64); ok && n == float64(int(n)) && int(n) >= 0 && int(n) < len(ruleMailGanttTypes) {
				label = "Gantt " + ruleMailGanttTypes[int(n)]
			} else if n, ok := types[i].(int32); ok && int(n) >= 0 && int(n) < len(ruleMailGanttTypes) {
				label = "Gantt " + ruleMailGanttTypes[n]
			}
		}
		addRel(id, label)
	}
	for _, id := range order {
		relation(strings.Join(labelsOf[id], " / "), id)
	}
	if cur, err := s.db.Collection("cards").Find(ctx, bson.M{"deletedAt": nil, "$or": bson.A{bson.M{"parentId": eq(cardID)}, bson.M{"parentIds": eq(cardID)}}}); err == nil {
		var subs []Card
		_ = cur.All(ctx, &subs)
		for _, sub := range subs {
			relation("Subtask", sub.ID)
		}
	}
	if tooLarge {
		return "", errors.New("rule-email-details-too-large")
	}
	// Afterwards: the definitions unchanged, the card still readable, an
	// admin still one.
	// canonical, not fmt: a bson.M prints its keys in map order.
	sameDefs := func(a, b []bson.M) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if canonical(a[i]) != canonical(b[i]) {
				return false
			}
		}
		return true
	}
	if !sameDefs(definitions(), defs) {
		return "", errors.New("rule-email-details-changed")
	}
	if err := scrumCurrent(); err != nil {
		return "", err
	}
	if !s.ruleMailCardReadable(ctx, userID, boardID, cardID) {
		return "", errRuleMailDetails
	}
	// What decided the votes and Scrum fields shown is what it is now.
	var nowCard bson.M
	nowBoard := s.ruleMailRawBoard(ctx, boardID)
	if s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(cardID)}).Decode(&nowCard) != nil || nowBoard == nil {
		return "", errRuleMailDetails
	}
	if nc, _ := plain(nowCard).(map[string]any); ruleMailVotingPolicy(nc, nowBoard) != votingPolicy ||
		canonical(ruleMailScrumPolicy(nowBoard)) != scrumPolicy {
		return "", errRuleMailVotingChanged
	}
	if admin {
		var now Board
		if s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(boardID)}).Decode(&now) != nil || !now.AdminOf(userID) {
			return "", errRuleMailDetails
		}
	}
	return strings.Join(lines, "\n"), nil
}
