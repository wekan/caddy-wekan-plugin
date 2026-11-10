package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// An import from another tool, as Node.js WeKan's models/kanboardCreator.js
// makes it: each tool's parser (import_parsers*.go, as WeKan's
// externalParsers.js) gives one "Kanboard shape" document - board, columns,
// swimlanes, tasks - and importGeneral writes it: the board and its labels
// from the tasks' tags, the swimlanes and lists in order with their WIP
// limits, a card per task with what importedTaskPlan decides (import_plan.go),
// its checklists and comments, the board's custom fields, and then the
// parent and dependency links between the import's own cards.
//
// The file's people are the person importing (WeKan's "me" mode): Weddy has
// no placeholder users or members-mapping step yet. Activities are written
// as WeKan's .direct inserts, without rules or notifications for an import.

// jsLookup is o[k] and whether o has the key - JavaScript's undefined is a
// missing key, which JSON.stringify leaves out; null is kept.
func jsLookup(o any, k string) (any, bool) {
	obj, ok := o.(jsObject)
	if !ok {
		return nil, false
	}
	for _, kv := range obj {
		if kv.Key == k {
			return kv.Value, true
		}
	}
	return nil, false
}

// setDefined sets k when the value is defined, as a spread of a source's
// property does.
func (o *jsObject) setDefined(k string, v any, defined bool) {
	if defined {
		o.set(k, v)
	}
}

// jsOr is a || b.
func jsOr(vs ...any) any {
	for _, v := range vs {
		if jsTruthy(v) {
			return v
		}
	}
	if len(vs) == 0 {
		return nil
	}
	return vs[len(vs)-1]
}

// allowedColors are WeKan's ALLOWED_COLORS (config/const.js): its card and
// label colors.
var allowedColors = append([]string{"white"}, labelPalette...)

type generalImport struct {
	s       *Server
	ctx     context.Context
	u       *User
	source  string
	now     time.Time
	members map[string]string
	lists   map[string]string
	order   []string // list names in order
	lanes   map[string]string
	laneIDs []string
}

func importTasks(doc any) []any {
	if a, ok := doc.([]any); ok {
		return a
	}
	tasks, _ := jsGet(doc, "tasks").([]any)
	return tasks
}

func importNames(doc any, key string, nameKeys []string, taskKeys []string, fallback string) []string {
	if list, ok := jsGet(doc, key).([]any); ok && len(list) > 0 {
		var out []string
		for _, c := range list {
			if n, ok := jsOr(jsGet(c, nameKeys[0]), jsGet(c, nameKeys[1])).(string); ok && n != "" {
				out = append(out, n)
			}
		}
		return out
	}
	var out []string
	for _, t := range importTasks(doc) {
		n, _ := jsOr(jsGet(t, taskKeys[0]), jsGet(t, taskKeys[1]), fallback).(string)
		if n != "" && !contains(out, n) {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return []string{fallback}
	}
	return out
}

// columnLimit is _columnLimit: wip_limit or Kanboard's task_limit, a whole
// number above 0.
func columnLimit(doc any, name string) int {
	list, _ := jsGet(doc, "columns").([]any)
	for _, c := range list {
		if jsOr(jsGet(c, "title"), jsGet(c, "name")) != name {
			continue
		}
		v, ok := jsLookup(c, "wip_limit")
		if !ok || v == nil {
			v = jsGet(c, "task_limit")
		}
		if n, ok := jsToNumber(v); ok && n > 0 && n == float64(int64(n)) {
			return int(n)
		}
		return 0
	}
	return 0
}

// importGeneral is KanboardCreator.create for one parsed document.
func (s *Server) importGeneral(ctx context.Context, u *User, doc any, source string) (*Board, error) {
	if _, ok := doc.(jsObject); !ok {
		if _, isArr := doc.([]any); !isArr {
			return nil, errors.New("error-json-schema")
		}
	}
	if err := s.mayCreateBoard(ctx, u, "board", "import:"+source); err != nil {
		return nil, err
	}
	g := &generalImport{s: s, ctx: ctx, u: u, source: source, now: time.Now().UTC(), lists: map[string]string{}, lanes: map[string]string{}}
	tasks := importTasks(doc)
	// "me": every person of the file is the person importing.
	g.members = map[string]string{}
	for _, p := range importedPeople(tasks) {
		g.members[p] = u.ID
	}
	title, _ := jsOr(jsGet(jsGet(doc, "board"), "name"), jsGet(jsGet(doc, "board"), "title")).(string)
	if title == "" {
		title = "Imported Kanboard Board " + g.now.Format(time.RFC1123)
	}
	b, err := s.insertBoard(ctx, u, title, "board", s.boardPermission(ctx, "private", "import:"+source))
	if err != nil {
		return nil, err
	}
	// The tasks' tags as the board's labels, black unless the source gives
	// a color - on the tag, or in label_colors.
	sourceColors, _ := jsGet(doc, "label_colors").(jsObject)
	b.Labels = nil
	for _, l := range planImportedLabels(tasks, allowedColors) {
		color := l.Color
		if named, ok := jsGet(sourceColors, l.Name).(string); ok && color == "black" && contains(allowedColors, named) {
			color = named
		}
		b.Labels = append(b.Labels, Label{ID: randomString(idAlphabet, 6), Color: color, Name: l.Name})
	}
	if b.Labels == nil {
		b.Labels = []Label{}
	}
	if _, err := s.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"labels": b.Labels}}); err != nil {
		return nil, err
	}
	_, _ = s.db.Collection("activities").InsertOne(ctx, bson.M{"_id": randomID(), "activityType": "importBoard", "boardId": b.ID, // inserted, not a filter
		"createdAt": g.now, "source": bson.M{"id": b.ID, "system": "Kanboard"}, "userId": u.ID})
	for n, name := range importNames(doc, "swimlanes", []string{"name", "title"}, []string{"swimlane_name", "swimlane"}, "Default") {
		id := randomID()
		if _, err := s.db.Collection("swimlanes").InsertOne(ctx, bson.M{"_id": id, "archived": false, "boardId": b.ID, // inserted, not a filter
			"createdAt": g.now, "title": name, "sort": float64(n), "type": "swimlane"}); err != nil {
			return nil, err
		}
		if _, dup := g.lanes[name]; !dup {
			g.lanes[name] = id
		}
		g.laneIDs = append(g.laneIDs, id)
	}
	columns := importNames(doc, "columns", []string{"title", "name"}, []string{"column_name", "column"}, "Imported")
	for n, name := range columns {
		id := randomID()
		list := bson.M{"_id": id, "archived": false, "boardId": b.ID, "createdAt": g.now, "title": name, "sort": float64(n), // inserted, not a filter
			"type": "list", "swimlaneId": ""}
		if limit := columnLimit(doc, name); limit > 0 {
			list["wipLimit"] = bson.M{"value": limit, "enabled": true, "soft": false}
		}
		if _, err := s.db.Collection("lists").InsertOne(ctx, list); err != nil {
			return nil, err
		}
		if _, dup := g.lists[name]; !dup {
			g.lists[name] = id
			g.order = append(g.order, id)
		}
	}
	if err := g.cards(b, tasks, columns); err != nil {
		return nil, err
	}
	s.hub.publish(b.ID, "board-changed")
	return b, nil
}

// importedPeople are the keys the task plan looks people up by: owners,
// assignees and watchers as they are, comment authors trimmed.
func importedPeople(tasks []any) []string {
	var out []string
	add := func(k string, ok bool) {
		if ok && k != "" && !contains(out, k) {
			out = append(out, k)
		}
	}
	for _, t := range tasks {
		add(importKey(jsOr(jsGet(t, "owner_id"), jsGet(t, "owner_username"), jsGet(t, "owner_name"))))
		for _, f := range []string{"assignees", "watchers"} {
			list, _ := jsGet(t, f).([]any)
			for _, k := range list {
				add(importKey(k))
			}
		}
		comments, _ := jsGet(t, "comments").([]any)
		for _, c := range comments {
			k := jsTrim(importText(jsGet(c, "author")))
			add(k, k != "")
		}
	}
	return out
}

func (g *generalImport) cards(b *Board, tasks []any, columns []string) error {
	s, ctx := g.s, g.ctx
	fields := planImportedCustomFields(tasks)
	fieldIDs := map[string]string{}
	for _, f := range fields.Fields {
		id := randomID()
		if _, err := s.db.Collection("customFields").InsertOne(ctx, bson.M{"_id": id, "boardIds": bson.A{b.ID}, "name": f.Name, // inserted, not a filter
			"type": f.Type, "settings": bson.M{}, "showOnCard": false, "automaticallyOnCard": false, "alwaysOnCard": false,
			"showLabelOnMiniCard": false, "createdAt": g.now}); err != nil {
			return err
		}
		fieldIDs[f.Name] = id
	}
	cardIDs := make([]string, len(tasks))
	for n, task := range tasks {
		column, _ := jsOr(jsGet(task, "column_name"), jsGet(task, "column")).(string)
		if column == "" && len(columns) > 0 {
			column = columns[0]
		}
		lane, _ := jsOr(jsGet(task, "swimlane_name"), jsGet(task, "swimlane"), "Default").(string)
		plan := planImportedTask(task, g.members, allowedColors, []string{g.u.ID})
		card := bson.M{"_id": randomID(), "boardId": b.ID, "dateLastActivity": g.now, "sort": float64(n), "userId": g.u.ID, // inserted, not a filter
			"labelIds": bson.A{}, "type": "cardType-card"}
		for _, kv := range plan.Card {
			if d, ok := kv.Value.(jsDate); ok {
				card[kv.Key] = time.Time(d)
			} else {
				card[kv.Key] = kv.Value
			}
		}
		if _, ok := card["createdAt"]; !ok {
			card["createdAt"] = g.now
		}
		card["listId"] = g.lists[column]
		if card["listId"] == "" && len(g.order) > 0 {
			card["listId"] = g.order[0]
		}
		card["swimlaneId"] = g.lanes[lane]
		if card["swimlaneId"] == "" && len(g.laneIDs) > 0 {
			card["swimlaneId"] = g.laneIDs[0]
		}
		if card["archived"] == true {
			card["archivedAt"] = g.now
		}
		tags, _ := jsGet(task, "tags").([]any)
		for _, t := range tags {
			name, ok := t.(string)
			if !ok {
				name, _ = jsGet(t, "name").(string)
			}
			for _, l := range b.Labels {
				if name != "" && l.Name == name {
					card["labelIds"] = append(card["labelIds"].(bson.A), l.ID)
					break
				}
			}
		}
		if len(plan.MemberIDs) > 0 {
			card["members"] = plan.MemberIDs
		}
		if len(plan.WatcherIDs) > 0 {
			card["watchers"] = plan.WatcherIDs
		}
		var values bson.A
		for _, v := range importedCustomFieldValues(task, fields.Fields) {
			values = append(values, bson.M{"_id": fieldIDs[v.Name], "value": v.Value}) // inserted, not a filter
		}
		if len(values) > 0 {
			card["customFields"] = values
		}
		if _, err := s.db.Collection("cards").InsertOne(ctx, card); err != nil {
			return err
		}
		cardID := card["_id"].(string)
		cardIDs[n] = cardID
		// Subtasks of many sources arrive as checklists.
		for _, cl := range plan.Checklists {
			clID := randomID()
			_, _ = s.db.Collection("checklists").InsertOne(ctx, bson.M{"_id": clID, "boardId": b.ID, "cardId": cardID, "title": cl.Title, // inserted, not a filter
				"sort": float64(cl.Sort), "createdAt": g.now})
			for _, it := range cl.Items {
				_, _ = s.db.Collection("checklistItems").InsertOne(ctx, bson.M{"_id": randomID(), "boardId": b.ID, "cardId": cardID, // inserted, not a filter
					"checklistId": clID, "title": it.Title, "sort": float64(it.Sort), "isFinished": it.IsFinished})
			}
		}
		for _, c := range plan.Comments {
			at := g.now
			if c.CreatedAt != nil {
				at = time.Time(*c.CreatedAt)
			}
			by := g.u.ID
			if c.UserID != nil {
				by = *c.UserID
			}
			commentID := randomID()
			_, _ = s.db.Collection("card_comments").InsertOne(ctx, bson.M{"_id": commentID, "boardId": b.ID, "cardId": cardID, // inserted, not a filter
				"createdAt": at, "modifiedAt": at, "text": c.Text, "userId": by})
			_, _ = s.db.Collection("activities").InsertOne(ctx, bson.M{"_id": randomID(), "activityType": "addComment", "boardId": b.ID, // inserted, not a filter
				"cardId": cardID, "commentId": commentID, "createdAt": at, "userId": by})
		}
	}
	// Parents and dependencies point at cards of this import.
	links := planImportedLinks(tasks)
	for _, p := range links.Parents {
		_, _ = s.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": eq(cardIDs[p.Index])}, bson.M{"$set": bson.M{"parentId": cardIDs[p.Parent]}})
	}
	for _, d := range links.Dependencies {
		var deps bson.A
		for _, x := range d.Deps {
			deps = append(deps, dependency{CardID: cardIDs[x.Target], Type: x.Type, Color: defaultDependencyColor, Icon: defaultDependencyIcon})
		}
		_, _ = s.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": eq(cardIDs[d.Index])}, bson.M{"$set": bson.M{"cardDependencies": deps}})
	}
	return nil
}

// The import's refusals, named so cmd/sync-i18n copies them.
var _ = []string{msg("error-json-malformed"), msg("error-json-schema"), msg("import-board-instruction-kanboard")}

// importGeneralPage is the import page's one step for another tool's JSON:
// its file or pasted text, parsed by the source's parser and written; the
// new board opens. A file that is not the source's JSON is refused, as
// WeKan's error-json-malformed and error-json-schema.
func (s *Server) importGeneralPage(w http.ResponseWriter, r *http.Request, u *User, source string) {
	r.Body = http.MaxBytesReader(w, r.Body, maxImportForm+1<<20)
	data := s.importData(r, u, source)
	fail := func(code string) {
		data["Error"] = code
		s.render(w, r, "import", http.StatusBadRequest, data)
	}
	var raw []byte
	if f, _, err := r.FormFile("file"); err == nil {
		defer f.Close()
		raw, err = io.ReadAll(io.LimitReader(f, maxImportForm+1))
		if err != nil || len(raw) > maxImportForm {
			fail("error-json-malformed")
			return
		}
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		raw = []byte(r.FormValue("text"))
	}
	var parsed jsObject
	if parse, ok := importTextParsers[source]; ok {
		// WeKan's check(board, String): the text as it is.
		var err error
		if parsed, err = parse(string(raw)); err != nil {
			fail("error-json-schema")
			return
		}
		s.importParsed(w, r, u, source, parsed, fail)
		return
	}
	doc, err := decodeJS(raw)
	if err != nil {
		fail("error-json-malformed")
		return
	}
	// WeKan's check(board, Object): an object, or an array of tasks; never a
	// string or a number.
	_, isObj := doc.(jsObject)
	_, isArr := doc.([]any)
	if !isObj && !isArr {
		fail("error-json-schema")
		return
	}
	parsed, err = importParsers[source](doc)
	if err != nil {
		fail("error-json-schema")
		return
	}
	s.importParsed(w, r, u, source, parsed, fail)
}

// importParsed writes a parsed document and opens the new board.
func (s *Server) importParsed(w http.ResponseWriter, r *http.Request, u *User, source string, parsed jsObject, fail func(string)) {
	b, err := s.importGeneral(r.Context(), u, parsed, source)
	if err != nil {
		var me *methodError
		if errors.As(err, &me) {
			fail(me.Code)
			return
		}
		fail("error-json-schema")
		return
	}
	http.Redirect(w, r, "/b/"+b.ID+"/"+b.Slug, http.StatusSeeOther)
}
