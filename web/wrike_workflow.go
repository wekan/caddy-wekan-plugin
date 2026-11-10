package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// A Wrike workflow and the board lists and rules it corresponds to, as
// Node.js WeKan's models/lib/wrikeWorkflow.js: a workflow is a list of custom
// statuses, each in one of four groups - Active, Completed, Deferred,
// Cancelled - and a status in a Completed or Cancelled group closes a task.
// A board's lists are the statuses, in board order; a rule "moved to X, mark
// it complete / incomplete" is X's group; a list no rule speaks for takes its
// group from its name. Import reads Wrike's GET /workflows JSON into lists and
// those rules; export writes the board back as one.

var wrikeGroups = []string{"Active", "Completed", "Deferred", "Cancelled"}

var wrikeGroupColor = map[string]string{"Active": "Blue", "Completed": "Green", "Deferred": "Gray", "Cancelled": "Red"}

var wrikeToWeKan = map[string]string{"Brown": "saddlebrown", "DarkCyan": "sky", "Gray": "gray", "Blue": "blue", "DarkBlue": "navy",
	"Orange": "orange", "Red": "red", "YellowGreen": "lime", "Purple": "purple", "Yellow": "yellow", "Indigo": "indigo",
	"DarkRed": "crimson", "Turquoise": "paleturquoise", "Green": "green"}

var wekanToWrike = func() map[string]string {
	m := map[string]string{"white": "Gray", "black": "Gray", "silver": "Gray", "peachpuff": "Orange", "pink": "Red", "mistyrose": "Red",
		"plum": "Purple", "magenta": "Purple", "darkgreen": "Green", "slateblue": "Indigo", "gold": "Yellow"}
	for wrike, wekan := range wrikeToWeKan {
		m[wekan] = wrike
	}
	return m
}()

var wrikeNamedGroups = []struct {
	group string
	re    *regexp.Regexp
}{
	{"Completed", regexp.MustCompile(`(?i)^(completed?|done|closed|finished|resolved|shipped|released|delivered|approved|accepted)$`)},
	{"Cancelled", regexp.MustCompile(`(?i)^(cancell?ed|canceled|rejected|won'?t (do|fix)|wontfix|abandoned|dropped|declined|obsolete|invalid|duplicate)$`)},
	{"Deferred", regexp.MustCompile(`(?i)^(deferred|on hold|paused|someday|later|parked|icebox|postponed|waiting)$`)},
}

const maxWrikeName = 128

// wrikeGroupForName is the group a list's name alone suggests.
func wrikeGroupForName(name string) string {
	text := strings.TrimSpace(name)
	for _, g := range wrikeGroups {
		if strings.EqualFold(g, text) {
			return g
		}
	}
	for _, n := range wrikeNamedGroups {
		if n.re.MatchString(text) {
			return n.group
		}
	}
	return "Active"
}

// wrikeName is WeKan's: trim() and slice(0, 128), which counts UTF-16
// units (a pair cut in two is left out whole).
func wrikeName(name string) string { return jsSlice16(jsTrim(name), maxWrikeName) }

// jsSlice16 is s.slice(0, n): the first n UTF-16 units.
func jsSlice16(s string, n int) string {
	units := 0
	for i, r := range s {
		w := 1
		if r > 0xFFFF {
			w = 2
		}
		if units+w > n {
			return s[:i]
		}
		units += w
	}
	return s
}

// wrikeCompletion is WeKan's completionByList: the lists a move rule marks
// complete or incomplete, the last rule winning.
func wrikeCompletion(rules []bson.M) map[string]string {
	out := map[string]string{}
	for _, r := range rules {
		t, _ := r["trigger"].(bson.M)
		a, _ := r["action"].(bson.M)
		list := docString(t, "listName")
		if docString(t, "activityType") != "moveCard" || list == "" || list == "*" {
			continue
		}
		switch docString(a, "actionType") {
		case "markCardComplete":
			out[list] = "complete"
		case "markCardIncomplete":
			out[list] = "incomplete"
		}
	}
	return out
}

func wrikeGroupForList(title string, completion map[string]string) string {
	byName := wrikeGroupForName(title)
	switch completion[title] {
	case "complete":
		if byName == "Cancelled" {
			return byName
		}
		return "Completed"
	case "incomplete":
		if byName == "Deferred" {
			return byName
		}
		return "Active"
	}
	return byName
}

// wrikeWorkflowFromBoard is WeKan's: the board's lists as statuses, with an
// Active and a Completed one added when no list is one, as Wrike requires -
// keys in WeKan's order, so the Rules page's download and the board export
// are WeKan's byte for byte.
func wrikeWorkflowFromBoard(title string, lists []bson.M, rules []bson.M) jsObject {
	wf, _ := wrikeWorkflowParts(title, lists, rules)
	return wf
}

// wrikeWorkflowParts is the workflow and WeKan's statusOf: a list's status
// name and group by its trimmed title, else the first Active one.
func wrikeWorkflowParts(title string, lists []bson.M, rules []bson.M) (jsObject, func(string) (string, string)) {
	completion := wrikeCompletion(rules)
	byTitle := map[string][2]string{}
	var statuses []jsObject
	seen := map[string]bool{}
	taken := func(name string) bool {
		for _, st := range statuses {
			if st[0].Value == name {
				return true
			}
		}
		return false
	}
	status := func(name, color, group string) jsObject {
		o := jsObject{}
		for _, kv := range []struct {
			k string
			v any
		}{{"name", name}, {"standardName", false}, {"color", color}, {"standard", false}, {"group", group}, {"hidden", false}} {
			o.set(kv.k, kv.v)
		}
		return o
	}
	groupOf := func(st jsObject) any {
		for _, kv := range st {
			if kv.Key == "group" {
				return kv.Value
			}
		}
		return nil
	}
	for _, l := range lists {
		t := jsTrim(docString(l, "title"))
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		name := wrikeName(t)
		for n := 2; taken(name); n++ {
			name = fmt.Sprintf("%s (%d)", jsSlice16(wrikeName(t), maxWrikeName-4), n)
		}
		group := wrikeGroupForList(t, completion)
		color := wekanToWrike[strings.ToLower(docString(l, "color"))]
		if color == "" {
			color = wrikeGroupColor[group]
		}
		if color == "" {
			color = "Gray"
		}
		statuses = append(statuses, status(name, color, group))
		byTitle[t] = [2]string{name, group}
	}
	for _, group := range []string{"Active", "Completed"} {
		has := false
		for _, st := range statuses {
			has = has || groupOf(st) == group
		}
		if has {
			continue
		}
		name := group
		for n := 2; taken(name); n++ {
			name = fmt.Sprintf("%s (%d)", group, n)
		}
		added := status(name, wrikeGroupColor[group], group)
		if group == "Active" {
			statuses = append([]jsObject{added}, statuses...)
		} else {
			statuses = append(statuses, added)
		}
	}
	name := wrikeName(title)
	if name == "" {
		name = "WeKan board"
	}
	wf := jsObject{}
	for _, kv := range []struct {
		k string
		v any
	}{{"name", name}, {"standard", false}, {"hidden", false}, {"customStatuses", statuses}} {
		wf.set(kv.k, kv.v)
	}
	out := jsObject{}
	out.set("kind", "workflows")
	out.set("data", []any{wf})
	statusOf := func(t string) (string, string) {
		if st, ok := byTitle[jsTrim(t)]; ok {
			return st[0], st[1]
		}
		for _, st := range statuses {
			if groupOf(st) == "Active" {
				return st[0].Value.(string), "Active"
			}
		}
		return "", ""
	}
	return out, statusOf
}

type wrikeStatus struct{ Name, Group, Color string }

// readWrikeWorkflow is WeKan's: GET /workflows' {kind, data}, its data, or
// one workflow; the first not hidden and not Wrike's standard one is read.
func readWrikeWorkflow(text string) (string, []wrikeStatus, int, error) {
	var v any
	if json.Unmarshal([]byte(text), &v) != nil {
		return "", nil, 0, errors.New("Wrike workflow is not JSON")
	}
	var all []any
	switch x := v.(type) {
	case []any:
		all = x
	case map[string]any:
		if d, ok := x["data"].([]any); ok {
			all = d
		} else if _, ok := x["customStatuses"].([]any); ok {
			all = []any{x}
		}
	}
	if len(all) == 0 {
		return "", nil, 0, errors.New(`Wrike workflow JSON has no workflow: expected GET /workflows' { "kind": "workflows", "data": [...] }`)
	}
	var chosen map[string]any
	pick := func(ok func(w map[string]any) bool) {
		for _, e := range all {
			w, _ := e.(map[string]any)
			if _, has := w["customStatuses"].([]any); has && chosen == nil && ok(w) {
				chosen = w
			}
		}
	}
	pick(func(w map[string]any) bool { return w["hidden"] != true && w["standard"] != true })
	pick(func(w map[string]any) bool { return w["hidden"] != true })
	pick(func(map[string]any) bool { return true })
	if chosen == nil {
		return "", nil, 0, errors.New("Wrike workflow JSON has no workflow with customStatuses")
	}
	notRead := len(all) - 1
	var statuses []wrikeStatus
	for _, e := range chosen["customStatuses"].([]any) {
		st, _ := e.(map[string]any)
		name := strings.TrimSpace(fmt.Sprint(st["name"]))
		if st["name"] == nil || name == "" || st["hidden"] == true {
			notRead++
			continue
		}
		group, _ := st["group"].(string)
		known := false
		for _, g := range wrikeGroups {
			known = known || g == group
		}
		if !known {
			group = wrikeGroupForName(name)
			notRead++
		}
		dup := false
		for _, s := range statuses {
			dup = dup || s.Name == name
		}
		if dup {
			notRead++
			continue
		}
		color, _ := st["color"].(string)
		statuses = append(statuses, wrikeStatus{name, group, wrikeToWeKan[color]})
	}
	wname := strings.TrimSpace(fmt.Sprint(chosen["name"]))
	if chosen["name"] == nil || wname == "" {
		wname = "Wrike workflow"
	}
	if len(statuses) == 0 {
		return "", nil, 0, fmt.Errorf("Wrike workflow %q has no status to read", wname)
	}
	return wname, statuses, notRead, nil
}

func lowerFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 {
		return s
	}
	return string(unicode.ToLower(r)) + s[n:]
}

// methodImportWrikeWorkflow is WeKan's rules.importWrikeWorkflow(boardId,
// text): a board admin's import - a status without a list becomes a list,
// after the one before it, in its color; and each list without a complete
// or incomplete move rule gets the one its group means. A second import adds
// nothing.
func (s *Server) methodImportWrikeWorkflow(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	boardID, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	text, err := stringArg(args, 1)
	if err != nil {
		return nil, err
	}
	if len(text) > 1<<20 {
		return nil, &methodError{"invalid-import-format", "Wrike workflow JSON is larger than 1 MB"}
	}
	ctx := r.Context()
	var b Board
	if !validID(boardID) || s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(boardID)}).Decode(&b) != nil {
		return nil, &methodError{"not-found", "Board not found"}
	}
	if !b.AdminOf(u.ID) {
		return nil, &methodError{"not-authorized", "Must be a board admin"}
	}
	name, statuses, notRead, err := readWrikeWorkflow(text)
	if err != nil {
		return nil, &methodError{"invalid-import-format", err.Error()}
	}
	if len(statuses) > 200 {
		notRead += len(statuses) - 200
		statuses = statuses[:200]
	}
	lists := s.boardLists(ctx, b.ID)
	byTitle := map[string]bson.M{}
	for _, l := range lists {
		if _, ok := byTitle[docString(l, "title")]; !ok {
			byTitle[docString(l, "title")] = l
		}
	}
	listsCreated := 0
	var previous bson.M
	for _, st := range statuses {
		if l, ok := byTitle[st.Name]; ok {
			previous = l
			continue
		}
		sort := s.sortAfter(lists, previous)
		made, err := s.AddList(ctx, &b, st.Name)
		if err != nil {
			return nil, err
		}
		set := bson.M{"sort": sort, "modifiedAt": time.Now().UTC()}
		if st.Color != "" {
			set["color"] = st.Color
		}
		if _, err := s.db.Collection("lists").UpdateOne(ctx, bson.M{"_id": eq(made.ID)}, bson.M{"$set": set}); err != nil {
			return nil, err
		}
		l := bson.M{"_id": made.ID, "title": st.Name, "sort": sort}
		lists = append(lists, l)
		byTitle[st.Name] = l
		previous = l
		listsCreated++
	}
	// The lists that already have a complete or incomplete move rule.
	governed := map[string]bool{}
	for _, rule := range s.boardRuleParts(ctx, b.ID) {
		t, _ := rule["trigger"].(bson.M)
		a, _ := rule["action"].(bson.M)
		if docString(t, "activityType") == "moveCard" && (docString(a, "actionType") == "markCardComplete" || docString(a, "actionType") == "markCardIncomplete") {
			governed[docString(t, "listName")] = true
		}
	}
	lang := languageOf(r, u).Tag
	rulesCreated := 0
	for _, st := range statuses {
		if governed[st.Name] {
			continue
		}
		closes := st.Group == "Completed" || st.Group == "Cancelled"
		kind, key := "markCardIncomplete", "r-mark-incomplete"
		if closes {
			kind, key = "markCardComplete", "r-mark-complete"
		}
		triggerDesc := tr(lang, "r-when-a-card-is-moved") + ": " + st.Name
		actionDesc := tr(lang, key)
		trigger := bson.M{"activityType": "moveCard", "listName": st.Name, "oldListName": "*", "swimlaneName": "*", "cardTitle": "*", "userId": "*", "desc": triggerDesc} // inserted, not a filter
		action := bson.M{"actionType": kind, "desc": actionDesc}
		title := "Wrike " + st.Group + ": When " + lowerFirst(triggerDesc) + ", then " + lowerFirst(actionDesc)
		if _, err := s.insertRule(ctx, b.ID, title, trigger, action); err != nil {
			return nil, err
		}
		governed[st.Name] = true
		rulesCreated++
	}
	s.hub.publish(b.ID, "board-changed")
	return map[string]any{"name": name, "statuses": len(statuses), "listsCreated": listsCreated, "rulesCreated": rulesCreated, "notRead": notRead}, nil
}

// boardLists are the board's lists not archived, in board order.
func (s *Server) boardLists(ctx context.Context, boardID string) []bson.M {
	cur, err := s.db.Collection("lists").Find(ctx, bson.M{"boardId": eq(boardID), "archived": bson.M{"$ne": true}},
		options.Find().SetSort(bson.D{{Key: "sort", Value: 1}}))
	if err != nil {
		return nil
	}
	var out []bson.M
	_ = cur.All(ctx, &out)
	return out
}

// sortAfter is the sort of a list placed right after previous - before every
// list when there is none - as WeKan's createListAfter places it.
func (s *Server) sortAfter(lists []bson.M, previous bson.M) float64 {
	sorts := make([]float64, 0, len(lists))
	for _, l := range lists {
		sorts = append(sorts, numberOrZero(l["sort"]))
	}
	if previous == nil {
		low := 0.0
		for i, v := range sorts {
			if i == 0 || v < low {
				low = v
			}
		}
		return low - 1
	}
	at := numberOrZero(previous["sort"])
	next, found := 0.0, false
	for _, v := range sorts {
		if v > at && (!found || v < next) {
			next, found = v, true
		}
	}
	if !found {
		return at + 1
	}
	return (at + next) / 2
}

func numberOrZero(v any) float64 {
	if n := numberOf(v); n == n {
		return n
	}
	return 0
}

// boardRuleParts are the board's rules with their trigger and action.
func (s *Server) boardRuleParts(ctx context.Context, boardID string) []bson.M {
	cur, err := s.db.Collection("rules").Find(ctx, bson.M{"boardId": eq(boardID)})
	if err != nil {
		return nil
	}
	var rules []bson.M
	_ = cur.All(ctx, &rules)
	var out []bson.M
	for _, r := range rules {
		var t, a bson.M
		if s.db.Collection("triggers").FindOne(ctx, bson.M{"_id": eq(docString(r, "triggerId"))}).Decode(&t) != nil ||
			s.db.Collection("actions").FindOne(ctx, bson.M{"_id": eq(docString(r, "actionId"))}).Decode(&a) != nil {
			continue
		}
		out = append(out, bson.M{"trigger": t, "action": a})
	}
	return out
}
