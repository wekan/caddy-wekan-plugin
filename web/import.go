package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The board import of Node.js WeKan's REST API (server/models/boards.js
// POST /api/boards/import): a wekan-board-1.0.0 export - from WeKan or from
// Weddy - becomes a new board of the caller, with its lists, swimlanes, cards,
// checklists, comments, custom fields, attachments and rules.
func (s *Server) importRoutes(m *http.ServeMux) {
	m.HandleFunc("POST /api/boards/import", s.importable(s.api(s.apiImportBoard)))
	m.HandleFunc("DELETE /api/boards/{board}", s.api(s.apiDeleteBoard))
	m.HandleFunc("POST /api/boards/{board}/copy", s.api(s.apiCopyBoard))
}

// MaxImportBytes is the largest board export taken: attachments ride in it.
const MaxImportBytes = 256 << 20

var importDateKey = regexp.MustCompile(`(At|^dateLastActivity)$`)

// cleanImported is a document from an export as it may be stored: no key that
// is an operator or a path, dates as dates.
func cleanImported(v any, key string) any {
	switch x := v.(type) {
	case map[string]any:
		out := bson.M{}
		for k, val := range x {
			if k == "" || strings.HasPrefix(k, "$") || strings.Contains(k, ".") {
				continue
			}
			out[k] = cleanImported(val, k)
		}
		return out
	case []any:
		out := bson.A{}
		for _, e := range x {
			out = append(out, cleanImported(e, ""))
		}
		return out
	case string:
		if importDateKey.MatchString(key) {
			if t, err := time.Parse(time.RFC3339Nano, x); err == nil {
				return t.UTC()
			}
		}
		return x
	case time.Time:
		return x
	}
	return v
}

func docsOf(export map[string]any, key string) []bson.M {
	arr, _ := export[key].([]any)
	out := []bson.M{}
	for _, e := range arr {
		if m, ok := cleanImported(e, "").(bson.M); ok {
			out = append(out, m)
		}
	}
	return out
}

func str(m bson.M, k string) string {
	v, _ := m[k].(string)
	return v
}

// idMap gives each id of the export a new one, the same new one each time.
type idMap map[string]string

func (ids idMap) fresh(old string) string {
	if old == "" {
		return randomID()
	}
	if n, ok := ids[old]; ok {
		return n
	}
	n := randomID()
	ids[old] = n
	return n
}

func (s *Server) apiImportBoard(w http.ResponseWriter, r *http.Request, u *User) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxImportBytes)
	var req struct {
		Board          map[string]any    `json:"board"`
		MembersMapping map[string]string `json:"membersMapping"`
	}
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	if err := dec.Decode(&req); err != nil || req.Board == nil {
		apiError(w, http.StatusBadRequest, "Missing board export object")
		return
	}
	numbersToFloat(req.Board)
	clean, err := s.secureImport(r, u, "wekan", req.Board)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	export, _ := clean.(map[string]any)
	if export["_format"] != "wekan-board-1.0.0" {
		apiError(w, http.StatusBadRequest, "not a WeKan board export (_format wekan-board-1.0.0)")
		return
	}
	id, err := s.ImportBoard(r, u, export, req.MembersMapping)
	if errors.Is(err, errBoardCreation) {
		apiError(w, http.StatusForbidden, err.Error())
		return
	}
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"_id": id})
}

// numbersToFloat turns json.Number into float64, as WeKan's JSON numbers are.
func numbersToFloat(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = numbersToFloat(e)
		}
	case []any:
		for i, e := range x {
			x[i] = numbersToFloat(e)
		}
	case json.Number:
		f, _ := x.Float64()
		return f
	}
	return v
}

// ImportBoard writes an export as a new board of u and answers its id.
func (s *Server) ImportBoard(r *http.Request, u *User, export map[string]any, mapping map[string]string) (string, error) {
	return s.importBoardWith(r, u, export, mapping, importOptions{})
}

// importOptions: Into, an existing board the export goes into instead of a new
// one; Files, where an attachment's bytes are when the export does not carry
// them (a .zip import).
type importOptions struct {
	Into   *Board
	Files  func(attachment bson.M) io.ReadCloser
	Counts map[string]int // what was imported, by collection, when given
}

func (s *Server) importBoardWith(r *http.Request, u *User, export map[string]any, mapping map[string]string, opts importOptions) (string, error) {
	ctx := r.Context()
	// Who the export's people are here: a mapped user who exists, else nobody
	// (a member) or the importer (an author).
	users := map[string]string{}
	for from, to := range mapping {
		if validID(to) && s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(to)}).Err() == nil {
			users[from] = to
		}
	}
	author := func(old string) string {
		if n, ok := users[old]; ok {
			return n
		}
		return u.ID
	}
	mapped := func(v any) bson.A {
		out := bson.A{}
		arr, _ := v.(bson.A)
		for _, e := range arr {
			if id, ok := e.(string); ok {
				if n, ok := users[id]; ok {
					out = append(out, n)
				}
			}
		}
		return out
	}

	title := strings.TrimSpace(str(export, "title"))
	if title == "" {
		title = "Imported board"
	}
	if len(title) > 1000 {
		title = title[:1000]
	}
	now := time.Now().UTC()
	boardID := randomID()
	if opts.Into != nil {
		boardID = opts.Into.ID
	}
	board := bson.M{"_id": boardID, "title": title, "slug": slugify(title), "permission": "private", "type": "board", // inserted, not a filter
		"archived": false, "createdAt": now, "modifiedAt": now, "color": "belize"}
	if p := str(export, "permission"); p == "public" {
		board["permission"] = p
	}
	if c := str(export, "color"); regexp.MustCompile(`^[a-z]{1,30}$`).MatchString(c) {
		board["color"] = c
	}
	if d, ok := export["description"].(string); ok {
		board["description"] = d
	}
	for k, v := range export { // WeKan's card settings, allowsX and the like: booleans only
		if b, ok := v.(bool); ok && (strings.HasPrefix(k, "allows") || strings.HasPrefix(k, "show")) && !strings.Contains(k, ".") {
			board[k] = b
		}
	}
	labels := bson.A{}
	labelIDs := map[string]string{} // the export's label id -> the board's
	var newLabels []Label
	for _, l := range docsOf(export, "labels") {
		old, name, color := str(l, "_id"), str(l, "name"), str(l, "color")
		if opts.Into != nil { // into a board: its label of the same name and color, or a new one
			found := ""
			for _, x := range opts.Into.Labels {
				if x.Name == name && x.Color == color {
					found = x.ID
				}
			}
			if found == "" {
				found = randomString(idAlphabet, 6)
				newLabels = append(newLabels, Label{ID: found, Name: name, Color: color})
				opts.Into.Labels = append(opts.Into.Labels, Label{ID: found, Name: name, Color: color})
			}
			labelIDs[old] = found
			continue
		}
		id := old
		taken := false
		for _, v := range labelIDs {
			taken = taken || v == id
		}
		if !validID(id) || taken {
			id = randomString(idAlphabet, 6)
		}
		labelIDs[old] = id
		labels = append(labels, bson.M{"_id": id, "name": name, "color": color}) // inserted, not a filter
	}
	board["labels"] = labels
	members := bson.A{bson.M{"userId": u.ID, "isAdmin": true, "isActive": true}}
	if ms, ok := export["members"].([]any); ok {
		for _, e := range ms {
			m, _ := e.(map[string]any)
			old, _ := m["userId"].(string)
			if n, ok := users[old]; ok && n != u.ID {
				isAdmin, _ := m["isAdmin"].(bool)
				members = append(members, bson.M{"userId": n, "isActive": true, "isAdmin": isAdmin}) // inserted, not a filter
			}
		}
	}
	board["members"] = members
	if opts.Into == nil {
		// CreateBleed and the private-only policy hold for an import and a copy too.
		typ, _ := board["type"].(string)
		if err := s.mayCreateBoard(ctx, u, typ, "importBoard"); err != nil {
			return "", err
		}
		perm, _ := board["permission"].(string)
		board["permission"] = s.boardPermission(ctx, perm, "importBoard")
		if _, err := s.db.Collection("boards").InsertOne(ctx, board); err != nil {
			return "", err
		}
	} else if len(newLabels) > 0 {
		if _, err := s.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": eq(boardID)}, bson.M{"$push": bson.M{"labels": bson.M{"$each": newLabels}}}); err != nil {
			return "", err
		}
	}
	inserted := map[string][]any{} // what this import wrote, to take back when it fails
	fail := func(err error) (string, error) {
		if opts.Into == nil {
			s.deleteBoardData(context.WithoutCancel(ctx), boardID)
		} else {
			for coll, ids := range inserted {
				_, _ = s.db.Collection(coll).DeleteMany(context.WithoutCancel(ctx), bson.M{"_id": dbIn(bson.A(ids))})
			}
		}
		return "", err
	}

	swimlanes, lists, cards, checklists, items, fields := idMap{}, idMap{}, idMap{}, idMap{}, idMap{}, idMap{}
	insert := func(coll string, docs []any) error {
		if len(docs) == 0 {
			return nil
		}
		_, err := s.db.Collection(coll).InsertMany(ctx, docs)
		if err == nil {
			for _, d := range docs {
				if m, ok := d.(bson.M); ok {
					inserted[coll] = append(inserted[coll], m["_id"])
				}
			}
			if opts.Counts != nil {
				opts.Counts[coll] += len(docs)
			}
		}
		return err
	}

	var out []any
	firstSwimlane := ""
	for _, sw := range docsOf(export, "swimlanes") {
		sw["_id"], sw["boardId"] = swimlanes.fresh(str(sw, "_id")), boardID
		if firstSwimlane == "" {
			firstSwimlane = sw["_id"].(string)
		}
		out = append(out, sw)
	}
	if firstSwimlane == "" && opts.Into != nil {
		firstSwimlane = s.firstSwimlane(ctx, boardID)
	}
	if firstSwimlane == "" {
		firstSwimlane = randomID()
		out = append(out, bson.M{"_id": firstSwimlane, "title": "Default", "boardId": boardID, "type": "swimlane", "archived": false, "sort": 0}) // inserted, not a filter
	}
	if err := insert("swimlanes", out); err != nil {
		return fail(err)
	}
	out = nil
	for _, f := range docsOf(export, "customFields") {
		f["_id"], f["boardIds"] = fields.fresh(str(f, "_id")), bson.A{boardID}
		out = append(out, f)
	}
	if err := insert("customFields", out); err != nil {
		return fail(err)
	}
	out = nil
	for _, l := range docsOf(export, "lists") {
		l["_id"], l["boardId"] = lists.fresh(str(l, "_id")), boardID
		if sw := str(l, "swimlaneId"); sw != "" {
			if n, ok := swimlanes[sw]; ok {
				l["swimlaneId"] = n
			} else {
				l["swimlaneId"] = ""
			}
		}
		if uid := str(l, "userId"); uid != "" {
			l["userId"] = author(uid)
		}
		out = append(out, l)
	}
	if err := insert("lists", out); err != nil {
		return fail(err)
	}
	exportCards := docsOf(export, "cards")
	for _, c := range exportCards { // every card's new id first: parents and dependencies name each other
		cards.fresh(str(c, "_id"))
	}
	out = nil
	for _, c := range exportCards {
		listID, ok := lists[str(c, "listId")]
		if !ok {
			continue
		}
		c["_id"], c["boardId"], c["listId"] = cards[str(c, "_id")], boardID, listID
		if sw, ok := swimlanes[str(c, "swimlaneId")]; ok {
			c["swimlaneId"] = sw
		} else {
			c["swimlaneId"] = firstSwimlane
		}
		c["userId"] = author(str(c, "userId"))
		c["members"], c["assignees"] = mapped(c["members"]), mapped(c["assignees"])
		delete(c, "linkedId")
		if p, ok := cards[str(c, "parentId")]; ok {
			c["parentId"] = p
		} else {
			delete(c, "parentId")
		}
		keep := bson.A{}
		if ls, ok := c["labelIds"].(bson.A); ok {
			for _, l := range ls {
				if id, _ := l.(string); labelIDs[id] != "" {
					keep = append(keep, labelIDs[id])
				}
			}
		}
		c["labelIds"] = keep
		cfs := bson.A{}
		if arr, ok := c["customFields"].(bson.A); ok {
			for _, e := range arr {
				if m, ok := e.(bson.M); ok {
					if n, ok := fields[str(m, "_id")]; ok {
						cfs = append(cfs, bson.M{"_id": n, "value": m["value"]}) // inserted, not a filter
					}
				}
			}
		}
		c["customFields"] = cfs
		deps := bson.A{}
		for _, d := range normalizeDependencies(toBSON(c["cardDependencies"])) {
			if n, ok := cards[d.CardID]; ok {
				d.CardID = n
				deps = append(deps, d)
			}
		}
		if len(deps) > 0 {
			c["cardDependencies"] = deps
		} else {
			delete(c, "cardDependencies")
		}
		out = append(out, c)
	}
	if err := insert("cards", out); err != nil {
		return fail(err)
	}
	out = nil
	for _, cl := range docsOf(export, "checklists") {
		card, ok := cards[str(cl, "cardId")]
		if !ok {
			continue
		}
		cl["_id"], cl["cardId"] = checklists.fresh(str(cl, "_id")), card
		if _, has := cl["boardId"]; has {
			cl["boardId"] = boardID
		}
		if uid := str(cl, "userId"); uid != "" {
			cl["userId"] = author(uid)
		}
		out = append(out, cl)
	}
	if err := insert("checklists", out); err != nil {
		return fail(err)
	}
	out = nil
	for _, it := range docsOf(export, "checklistItems") {
		card, ok1 := cards[str(it, "cardId")]
		cl, ok2 := checklists[str(it, "checklistId")]
		if !ok1 || !ok2 {
			continue
		}
		it["_id"], it["cardId"], it["checklistId"] = items.fresh(str(it, "_id")), card, cl
		out = append(out, it)
	}
	if err := insert("checklistItems", out); err != nil {
		return fail(err)
	}
	out = nil
	for _, cm := range docsOf(export, "comments") {
		card, ok := cards[str(cm, "cardId")]
		if !ok {
			continue
		}
		cm["_id"], cm["cardId"], cm["boardId"], cm["userId"] = randomID(), card, boardID, author(str(cm, "userId"))
		out = append(out, cm)
	}
	if err := insert("card_comments", out); err != nil {
		return fail(err)
	}

	// Rules, with their triggers and actions.
	triggers, actions := idMap{}, idMap{}
	out = nil
	for _, t := range docsOf(export, "triggers") {
		t["_id"], t["boardId"] = triggers.fresh(str(t, "_id")), boardID
		out = append(out, t)
	}
	if err := insert("triggers", out); err != nil {
		return fail(err)
	}
	out = nil
	for _, a := range docsOf(export, "actions") {
		a["_id"], a["boardId"] = actions.fresh(str(a, "_id")), boardID
		out = append(out, a)
	}
	if err := insert("actions", out); err != nil {
		return fail(err)
	}
	out = nil
	for _, rule := range docsOf(export, "rules") {
		t, ok1 := triggers[str(rule, "triggerId")]
		a, ok2 := actions[str(rule, "actionId")]
		if !ok1 || !ok2 {
			continue
		}
		rule["_id"], rule["boardId"], rule["triggerId"], rule["actionId"] = randomID(), boardID, t, a
		for field, ids := range map[string]idMap{"extraTriggerIds": triggers, "extraActionIds": actions} {
			extra := bson.A{}
			if arr, ok := rule[field].(bson.A); ok {
				for _, e := range arr {
					if id, _ := e.(string); ids[id] != "" {
						extra = append(extra, ids[id])
					}
				}
			}
			if len(extra) > 0 {
				rule[field] = extra
			} else {
				delete(rule, field)
			}
		}
		out = append(out, rule)
	}
	if err := insert("rules", out); err != nil {
		return fail(err)
	}

	// Attachments that carry their file.
	var b Board
	_ = s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(boardID)}).Decode(&b)
	for _, a := range docsOf(export, "attachments") {
		cardID, ok := cards[str(a, "cardId")]
		if !ok {
			continue
		}
		var body io.ReadCloser
		if data, err := base64.StdEncoding.DecodeString(str(a, "file")); err == nil && len(data) > 0 {
			body = io.NopCloser(bytes.NewReader(data))
		} else if opts.Files != nil {
			body = opts.Files(a)
		}
		if body == nil {
			continue
		}
		var c Card
		if s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(cardID)}).Decode(&c) != nil {
			body.Close()
			continue
		}
		at, err := s.StoreAttachment(r, &b, &c, u, str(a, "name"), str(a, "type"), body)
		body.Close()
		if err != nil {
			return fail(err)
		}
		inserted["attachments"] = append(inserted["attachments"], at.ID)
		if opts.Counts != nil {
			opts.Counts["attachments"]++
		}
	}
	s.activity(ctx, u.ID, "importBoard", bson.M{"type": "board", "activityTypeId": boardID, "boardId": boardID}) // inserted, not a filter
	return boardID, nil
}

// toBSON is an imported value as the database driver gives it back.
func toBSON(v any) any {
	arr, ok := v.(bson.A)
	if !ok {
		return v
	}
	out := bson.A{}
	for _, e := range arr {
		if m, ok := e.(bson.M); ok {
			d := bson.D{}
			for k, val := range m {
				d = append(d, bson.E{Key: k, Value: val})
			}
			out = append(out, d)
		} else {
			out = append(out, e)
		}
	}
	return out
}

// deleteBoardData removes a board and everything of it.
func (s *Server) deleteBoardData(ctx context.Context, boardID string) {
	var cardIDs bson.A
	cur, err := s.db.Collection("cards").Find(ctx, bson.M{"boardId": eq(boardID)})
	if err == nil {
		var cards []Card
		_ = cur.All(ctx, &cards)
		for _, c := range cards {
			cardIDs = append(cardIDs, c.ID)
		}
	}
	byCard := bson.M{"cardId": bson.M{"$in": cardIDs}}
	for coll, filter := range map[string]bson.M{"checklists": byCard, "checklistItems": byCard, "card_comments": byCard,
		"cards": {"boardId": eq(boardID)}, "lists": {"boardId": eq(boardID)}, "swimlanes": {"boardId": eq(boardID)},
		"rules": {"boardId": eq(boardID)}, "triggers": {"boardId": eq(boardID)}, "actions": {"boardId": eq(boardID)},
		"customFields": {"boardIds": eq(boardID)}} {
		_, _ = s.db.Collection(coll).DeleteMany(ctx, filter)
	}
	_, _ = s.db.Collection("attachments").UpdateMany(ctx, bson.M{"meta.boardId": eq(boardID), "deletedAt": nil},
		bson.M{"$set": bson.M{"deletedAt": time.Now().UTC()}})
	_, _ = s.db.Collection("boards").DeleteOne(ctx, bson.M{"_id": eq(boardID)})
}

// apiDeleteBoard: a board admin removes the board (WeKan's DELETE /api/boards/:boardId).
func (s *Server) apiDeleteBoard(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardAdmin(w, r, u)
	if b == nil {
		return
	}
	s.deleteBoardData(r.Context(), b.ID)
	s.hub.publish(b.ID, "board-changed")
	writeJSON(w, http.StatusOK, map[string]string{"_id": b.ID})
}

var numberedTitle = regexp.MustCompile(`^(.*?)\s*(\[(\d+)\]\s*$|\s*$)`)

// uniqueTitle is WeKan's Boards.uniqueTitle: "Title", or "Title [n]" one past
// the highest number a board of that title has.
func (s *Server) uniqueTitle(ctx context.Context, title string) string {
	base := numberedTitle.FindStringSubmatch(title)[1]
	cur, err := s.db.Collection("boards").Find(ctx, bson.M{"title": bson.M{"$regex": "^" + regexp.QuoteMeta(base) + `\s*(\[\d+\]\s*$|\s*$)`}})
	if err != nil {
		return title
	}
	var boards []Board
	_ = cur.All(ctx, &boards)
	if len(boards) == 0 {
		return title
	}
	most := 0
	for _, b := range boards {
		if m := numberedTitle.FindStringSubmatch(b.Title); m != nil && m[3] != "" {
			if n := atoiOr(m[3], 0); n > most {
				most = n
			}
		}
	}
	return base + " [" + strconv.Itoa(most+1) + "]"
}

func atoiOr(s string, d int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return d
	}
	return n
}

// apiCopyBoard is WeKan's POST /api/boards/:boardId/copy: a board admin copies
// the board - its lists, cards, checklists, comments, attachments, its webhooks
// (#5592) and its rules with their triggers and actions (#5627) - and gets the
// new board's id.
func (s *Server) apiCopyBoard(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardAdmin(w, r, u)
	if b == nil {
		return
	}
	var req struct {
		Title string `json:"title"`
	}
	_ = decodeJSON(r, &req)
	ctx := r.Context()
	raw, err := s.boardDocs(ctx, b, false)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	plain, err := plainDocs(raw)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	doc, _ := plain.(map[string]any)
	doc["title"] = strings.TrimSpace(req.Title)
	if doc["title"] == "" {
		doc["title"] = s.uniqueTitle(ctx, b.Title)
	}
	same := map[string]string{} // the board's people stay themselves
	for _, m := range b.Members {
		if m.IsActive {
			same[m.UserID] = m.UserID
		}
	}
	id, err := s.ImportBoard(r, u, doc, same)
	if errors.Is(err, errBoardCreation) {
		apiError(w, http.StatusForbidden, err.Error())
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	cur, err := s.db.Collection("integrations").Find(ctx, bson.M{"boardId": b.ID})
	if err == nil {
		var hooks []bson.M
		_ = cur.All(ctx, &hooks)
		for _, h := range hooks {
			h["_id"], h["boardId"] = randomID(), id
			_, _ = s.db.Collection("integrations").InsertOne(ctx, h)
		}
	}
	writeJSON(w, http.StatusOK, id)
}
