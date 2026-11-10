package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"sort"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// A WeKan export imported INTO a board, beside the thing whose menu it was
// started from, as Node.js WeKan's models/server/scopedImporter.js does it
// (#1173): a swimlane below that swimlane, a list after that list, a card
// below that card - or, with none, every swimlane of the document at the top
// of the board. Lists are matched by title unless it is a list's import;
// custom fields by name. Everything is new, with new ids; the selection says
// what comes in. The list, swimlane and card beside it must be the board's
// own (ImportPlacementBleed).

// sortsAfter is models/lib/insertPosition.js sortsAfter: the sorts of count
// new items after targetSort (nil: before everything), evenly in the gap
// before whatever comes next.
func sortsAfter(existing []float64, target *float64, count int) []float64 {
	if count < 1 {
		return nil
	}
	var sorts []float64
	for _, v := range existing {
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			sorts = append(sorts, v)
		}
	}
	sort.Float64s(sorts)
	out := make([]float64, count)
	if len(sorts) == 0 {
		from := 0.0
		if target != nil {
			from = *target + 1
		}
		for i := range out {
			out[i] = from + float64(i)
		}
		return out
	}
	if target == nil {
		for i := range out {
			out[i] = sorts[0] - float64(count-i)
		}
		return out
	}
	next, found := 0.0, false
	for _, v := range sorts {
		if v > *target {
			next, found = v, true
			break
		}
	}
	if !found {
		for i := range out {
			out[i] = *target + 1 + float64(i)
		}
		return out
	}
	step := (next - *target) / float64(count+1)
	for i := range out {
		out[i] = *target + step*float64(i+1)
	}
	return out
}

// scopedTarget is importScoped's target.
type scopedTarget struct {
	BoardID, SwimlaneID, ListID, CardID string
}

// errImportPlacement is error-import-placement: which id is not the board's.
type errImportPlacement struct{ key string }

func (e errImportPlacement) Error() string { return e.key + " does not belong to this board." }

// scopedImporter is ScopedImporter.
type scopedImporter struct {
	s       *Server
	r       *http.Request
	u       *User
	target  scopedTarget
	doc     map[string]any
	want    exportWant
	source  string
	files   func(a bson.M) io.ReadCloser
	board   *Board
	card    bson.M // the card whose menu was used
	cardIDs map[string]string
	cardAt  map[string][2]string // new card -> list, swimlane
	listIDs map[string]string
	laneIDs map[string]string
	counts  map[string]int
}

func (si *scopedImporter) rows(key string) []map[string]any {
	var out []map[string]any
	if arr, ok := si.doc[key].([]any); ok {
		for _, x := range arr {
			if m, ok := x.(map[string]any); ok {
				out = append(out, m)
			}
		}
	}
	return out
}

func rowStr(m map[string]any, k string) string { s, _ := m[k].(string); return s }

func (si *scopedImporter) docs(ctx context.Context, coll string, filter bson.M, sorted bool) []bson.M {
	opts := options.Find()
	if sorted {
		opts.SetSort(bson.D{{Key: "sort", Value: 1}})
	}
	var out []bson.M
	if cur, err := si.s.db.Collection(coll).Find(ctx, filter, opts); err == nil {
		_ = cur.All(ctx, &out)
	}
	return out
}

func sortOf(d bson.M) (float64, bool) {
	f, ok := toFloat(plain(d["sort"]))
	return f, ok
}

func sortsOfDocs(docs []bson.M) []float64 {
	var out []float64
	for _, d := range docs {
		if f, ok := sortOf(d); ok {
			out = append(out, f)
		}
	}
	return out
}

// checkPlacement is _checkPlacement: each id beside the import is looked up
// ON the board; one that is not there is refused and recorded.
func (si *scopedImporter) checkPlacement(ctx context.Context) error {
	t := si.target
	found := func(coll, id string) bson.M {
		if id == "" {
			return nil
		}
		var d bson.M
		if !validID(id) || si.s.db.Collection(coll).FindOne(ctx, bson.M{"_id": eq(id), "boardId": si.board.ID}).Decode(&d) != nil {
			return nil
		}
		return d
	}
	list, lane, card := found("lists", t.ListID), found("swimlanes", t.SwimlaneID), found("cards", t.CardID)
	for _, c := range []struct {
		key, id string
		doc     bson.M
	}{{"listId", t.ListID, list}, {"swimlaneId", t.SwimlaneID, lane}, {"cardId", t.CardID, card}} {
		if c.id != "" && c.doc == nil {
			func() {
				defer func() { _ = recover() }() // logging must never break the guard
				si.s.recordSecurity(si.r, securityEvent{Key: "authz.import-placement", Action: "blocked", Source: si.source, UserID: si.u.ID,
					Detail: "Import into board " + si.board.ID + " refused: " + c.key + " is not on that board"})
			}()
			return errImportPlacement{c.key}
		}
	}
	si.card = card
	return nil
}

func (si *scopedImporter) run(ctx context.Context) (map[string]int, error) {
	if err := si.checkPlacement(ctx); err != nil {
		return nil, err
	}
	var err error
	switch {
	case si.target.CardID != "":
		err = si.importCards(ctx)
	case si.target.ListID != "":
		err = si.importLists(ctx)
	default:
		err = si.importSwimlanes(ctx)
	}
	return si.counts, err
}

func (si *scopedImporter) importSwimlanes(ctx context.Context) error {
	lanes := si.rows("swimlanes")
	if len(lanes) == 0 {
		return si.importLists(ctx)
	}
	existing := si.docs(ctx, "swimlanes", bson.M{"boardId": si.board.ID}, false)
	var target *float64
	if si.target.SwimlaneID != "" {
		for _, d := range existing {
			if strOf(d["_id"]) == si.target.SwimlaneID {
				if f, ok := sortOf(d); ok {
					target = &f
				}
			}
		}
	}
	sorts := sortsAfter(sortsOfDocs(existing), target, len(lanes))
	now := time.Now().UTC()
	for i, l := range lanes {
		id := randomID()
		title := rowStr(l, "title")
		if title == "" {
			title = "Swimlane"
		}
		doc := bson.M{"_id": id, "archived": false, "boardId": si.board.ID, "createdAt": now, "updatedAt": now, "title": title, "sort": sorts[i], "type": "swimlane"} // inserted, not a filter
		if c := rowStr(l, "color"); c != "" {
			doc["color"] = c
		}
		if t := rowStr(l, "type"); t != "" {
			doc["type"] = t
		}
		if _, err := si.s.db.Collection("swimlanes").InsertOne(ctx, doc); err != nil {
			return err
		}
		si.laneIDs[rowStr(l, "_id")] = id
		si.counts["swimlanes"]++
	}
	return si.importLists(ctx)
}

func (si *scopedImporter) importLists(ctx context.Context) error {
	lists := si.rows("lists")
	existing := si.docs(ctx, "lists", bson.M{"boardId": si.board.ID}, false)
	byTitle := map[string]string{}
	for _, d := range existing {
		if _, ok := byTitle[strOf(d["title"])]; !ok {
			byTitle[strOf(d["title"])] = strOf(d["_id"])
		}
	}
	var target *float64
	if si.target.ListID != "" {
		for _, d := range existing {
			if strOf(d["_id"]) == si.target.ListID {
				if f, ok := sortOf(d); ok {
					target = &f
				}
			}
		}
	}
	toCreate := 0
	for _, l := range lists {
		if _, ok := byTitle[rowStr(l, "title")]; !ok || si.target.ListID != "" {
			toCreate++
		}
	}
	sorts := sortsAfter(sortsOfDocs(existing), target, toCreate)
	created := 0
	now := time.Now().UTC()
	for _, l := range lists {
		title := rowStr(l, "title")
		if title == "" {
			title = "List"
		}
		if si.target.ListID == "" {
			if id, ok := byTitle[title]; ok {
				si.listIDs[rowStr(l, "_id")] = id
				continue
			}
		}
		id := randomID()
		doc := bson.M{"_id": id, "archived": false, "boardId": si.board.ID, "createdAt": now, "updatedAt": now, "title": title, "sort": sorts[created], // inserted, not a filter
			"type": "list", "swimlaneId": ""}
		if c := rowStr(l, "color"); c != "" {
			doc["color"] = c
		}
		if wd, ok := l["width"].(float64); ok {
			doc["width"] = wd
		}
		if col, ok := l["collapsed"].(bool); ok {
			doc["collapsed"] = col
		}
		if _, err := si.s.db.Collection("lists").InsertOne(ctx, doc); err != nil {
			return err
		}
		si.listIDs[rowStr(l, "_id")] = id
		si.counts["lists"]++
		created++
	}
	return si.importCards(ctx)
}

// importDate is new Date(v): ISO text or a time value.
func importDate(v any) (time.Time, bool) {
	switch x := v.(type) {
	case string:
		if x == "" {
			return time.Time{}, false
		}
		return jsDateTime(x)
	case float64:
		return time.UnixMilli(int64(x)).UTC(), true
	case time.Time:
		return x, true
	}
	return time.Time{}, false
}

func (si *scopedImporter) importCards(ctx context.Context) error {
	cards := si.rows("cards")
	if len(cards) == 0 {
		return nil
	}
	b := si.board
	fallbackList, fallbackLane := "", ""
	if si.card != nil {
		fallbackList, fallbackLane = strOf(si.card["listId"]), strOf(si.card["swimlaneId"])
	}
	if si.card == nil && si.target.ListID != "" {
		fallbackList = si.target.ListID
	}
	if si.card == nil && si.target.SwimlaneID != "" {
		fallbackLane = si.target.SwimlaneID
	}
	if fallbackList == "" {
		if l := si.docs(ctx, "lists", bson.M{"boardId": b.ID}, true); len(l) > 0 {
			fallbackList = strOf(l[0]["_id"])
		}
	}
	if fallbackLane == "" {
		if l := si.docs(ctx, "swimlanes", bson.M{"boardId": b.ID}, true); len(l) > 0 {
			fallbackLane = strOf(l[0]["_id"])
		}
	}
	siblings := si.docs(ctx, "cards", bson.M{"boardId": b.ID, "listId": eq(fallbackList), "swimlaneId": eq(fallbackLane), "archived": false}, false)
	var target *float64
	if si.card != nil {
		if f, ok := sortOf(si.card); ok {
			target = &f
		}
	}
	sorts := sortsAfter(sortsOfDocs(siblings), target, len(cards))
	fieldMap := si.customFieldMap(ctx)
	members := map[string]bool{}
	for _, m := range b.Members {
		members[m.UserID] = true
	}
	onBoard := func(v any) bson.A {
		out := bson.A{}
		if arr, ok := v.([]any); ok {
			for _, x := range arr {
				if id, ok := x.(string); ok && members[id] {
					out = append(out, id)
				}
			}
		}
		return out
	}
	now := time.Now().UTC()
	for i, c := range cards {
		listID := si.listIDs[rowStr(c, "listId")]
		if listID == "" {
			listID = fallbackList
		}
		laneID := si.laneIDs[rowStr(c, "swimlaneId")]
		if laneID == "" {
			laneID = fallbackLane
		}
		if listID == "" || laneID == "" {
			continue
		}
		number, err := si.s.nextCardNumber(ctx, b.ID)
		if err != nil {
			return err
		}
		id := randomID()
		doc := bson.M{"_id": id, "archived": false, "boardId": b.ID, "listId": listID, "swimlaneId": laneID, "title": rowStr(c, "title"), // inserted, not a filter
			"sort": sorts[i], "createdAt": now, "dateLastActivity": now, "userId": si.u.ID, "labelIds": bson.A{}, "members": bson.A{},
			"assignees": bson.A{}, "type": "cardType-card", "cardNumber": number}
		if si.want.has("description") && rowStr(c, "description") != "" {
			doc["description"] = rowStr(c, "description")
		}
		if si.want.has("people") {
			if v := rowStr(c, "requestedBy"); v != "" {
				doc["requestedBy"] = v
			}
			if v := rowStr(c, "assignedBy"); v != "" {
				doc["assignedBy"] = v
			}
			doc["requesters"], doc["assigners"] = onBoard(c["requesters"]), onBoard(c["assigners"])
		}
		if si.want.has("dates") {
			for _, k := range []string{"receivedAt", "startAt", "dueAt", "endAt"} {
				if cdTruthy(c[k]) {
					if t, ok := importDate(c[k]); ok {
						doc[k] = t
					}
				}
			}
			if v, ok := c["spentTime"].(float64); ok {
				doc["spentTime"] = v
			}
			if v, ok := c["isOvertime"].(bool); ok {
				doc["isOvertime"] = v
			}
		}
		if si.want.has("voting") {
			if v, ok := c["vote"].(map[string]any); ok {
				doc["vote"] = v
			}
		}
		if si.want.has("poker") {
			if v, ok := c["poker"].(map[string]any); ok {
				doc["poker"] = v
			}
		}
		if si.want.has("custom-fields") {
			if arr, ok := c["customFields"].([]any); ok {
				mapped := bson.A{}
				for _, x := range arr {
					if f, ok := x.(map[string]any); ok {
						if n := fieldMap[rowStr(f, "_id")]; n != "" {
							mapped = append(mapped, bson.M{"_id": n, "value": f["value"]}) // inserted, not a filter
						}
					}
				}
				if len(mapped) > 0 {
					doc["customFields"] = mapped
				}
			}
		}
		if _, err := si.s.db.Collection("cards").InsertOne(ctx, doc); err != nil {
			return err
		}
		si.cardIDs[rowStr(c, "_id")] = id
		si.cardAt[id] = [2]string{listID, laneID}
		si.counts["cards"]++
	}
	if err := si.importChecklists(ctx); err != nil {
		return err
	}
	if err := si.importComments(ctx); err != nil {
		return err
	}
	si.importAttachments(ctx)
	return nil
}

// customFieldMap is _customFieldMap: an imported field is the board's of
// the same name.
func (si *scopedImporter) customFieldMap(ctx context.Context) map[string]string {
	out := map[string]string{}
	if !si.want.has("custom-fields") {
		return out
	}
	incoming := si.rows("customFields")
	if len(incoming) == 0 {
		return out
	}
	byName := map[string]string{}
	for _, d := range si.docs(ctx, "customFields", bson.M{"boardIds": si.board.ID}, false) {
		byName[strOf(d["name"])] = strOf(d["_id"])
	}
	for _, f := range incoming {
		if n, ok := byName[rowStr(f, "name")]; ok {
			out[rowStr(f, "_id")] = n
		}
	}
	return out
}

func (si *scopedImporter) importChecklists(ctx context.Context) error {
	if !si.want.has("checklists") {
		return nil
	}
	ids := map[string]string{}
	now := time.Now().UTC()
	for _, cl := range si.rows("checklists") {
		cardID := si.cardIDs[rowStr(cl, "cardId")]
		if cardID == "" {
			continue
		}
		id := randomID()
		title := rowStr(cl, "title")
		if title == "" {
			title = "Checklist"
		}
		s, _ := cl["sort"].(float64)
		doc := bson.M{"_id": id, "cardId": cardID, "title": title, "sort": s, "createdAt": now} // inserted, not a filter
		if h, ok := cl["hideCheckedItems"].(bool); ok {
			doc["hideCheckedItems"] = h
		}
		if _, err := si.s.db.Collection("checklists").InsertOne(ctx, doc); err != nil {
			return err
		}
		ids[rowStr(cl, "_id")] = id
		si.counts["checklists"]++
	}
	for _, it := range si.rows("checklistItems") {
		clID, cardID := ids[rowStr(it, "checklistId")], si.cardIDs[rowStr(it, "cardId")]
		if clID == "" || cardID == "" {
			continue
		}
		s, _ := it["sort"].(float64)
		fin, _ := it["isFinished"].(bool)
		if _, err := si.s.db.Collection("checklistItems").InsertOne(ctx, bson.M{"_id": randomID(), "cardId": cardID, "checklistId": clID, // inserted, not a filter
			"title": rowStr(it, "title"), "isFinished": fin, "sort": s, "createdAt": now}); err != nil {
			return err
		}
	}
	return nil
}

func (si *scopedImporter) importComments(ctx context.Context) error {
	if !si.want.has("comments") {
		return nil
	}
	now := time.Now().UTC()
	for _, c := range si.rows("comments") {
		cardID := si.cardIDs[rowStr(c, "cardId")]
		if cardID == "" {
			continue
		}
		// The importing user, not the original author, as WeKan's.
		if _, err := si.s.db.Collection("card_comments").InsertOne(ctx, bson.M{"_id": randomID(), "boardId": si.board.ID, "cardId": cardID, // inserted, not a filter
			"text": rowStr(c, "text"), "userId": si.u.ID, "createdAt": now, "modifiedAt": now}); err != nil {
			return err
		}
		si.counts["comments"]++
	}
	return nil
}

// importAttachments is _importAttachments: each file, from the archive or
// the document's base64; one that fails never stops the rest.
func (si *scopedImporter) importAttachments(ctx context.Context) {
	if !si.want.has("attachments") {
		return
	}
	for _, a := range si.rows("attachments") {
		cardID := si.cardIDs[rowStr(a, "cardId")]
		if cardID == "" {
			continue
		}
		var body io.ReadCloser
		if si.files != nil {
			body = si.files(bson.M(a))
		}
		if body == nil {
			if data, err := base64.StdEncoding.DecodeString(rowStr(a, "file")); err == nil && len(data) > 0 {
				body = io.NopCloser(bytes.NewReader(data))
			} else if u := rowStr(a, "url"); u != "" {
				// FollowBleed: the URL and every redirect validated.
				data, _, err := fetchImportedAttachment(ctx, u, nil)
				si.s.recordAttachmentBlocked(ctx, "scoped", err)
				if err == nil && len(data) > 0 {
					body = io.NopCloser(bytes.NewReader(data))
				}
			}
		}
		if body == nil {
			continue
		}
		var c Card
		if si.s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(cardID)}).Decode(&c) != nil {
			body.Close()
			continue
		}
		name := rowStr(a, "name")
		if name == "" {
			name = "attachment"
		}
		if _, err := si.s.StoreAttachment(si.r, si.board, &c, si.u, name, rowStr(a, "type"), body); err == nil {
			si.counts["attachments"]++
		}
		body.Close()
	}
}

// pruneImportDocument is models/lib/importParts.js's: a section the
// selection leaves out is emptied, never deleted.
func pruneImportDocument(doc map[string]any, want exportWant) {
	if want == nil {
		return
	}
	parts := map[string][]string{"comments": {"comments", "cardComments", "actions"}, "checklists": {"checklists", "checklistItems"},
		"attachments": {"attachments"}, "custom-fields": {"customFields"}, "activities": {"activities"}, "subtasks": {"subtaskItems"}}
	if !want["scrum"] || !want["custom-fields"] {
		delete(doc, "wekanScrumMapping")
	}
	if !want["scrum"] {
		delete(doc, "scrumTransfer")
		delete(doc, "scrumTransferLosses")
	}
	for key, arrays := range parts {
		if want[key] {
			continue
		}
		for _, name := range arrays {
			if _, ok := doc[name].([]any); ok {
				doc[name] = []any{}
			}
		}
	}
}

// importScopedInto runs the scoped importer for u on the board.
func (s *Server) importScopedInto(r *http.Request, u *User, b *Board, t scopedTarget, doc map[string]any, want exportWant,
	source string, files func(a bson.M) io.ReadCloser) (map[string]int, error) {
	si := &scopedImporter{s: s, r: r, u: u, target: t, doc: doc, want: want, source: source, files: files, board: b,
		cardIDs: map[string]string{}, cardAt: map[string][2]string{}, listIDs: map[string]string{}, laneIDs: map[string]string{},
		counts: map[string]int{"swimlanes": 0, "lists": 0, "cards": 0, "checklists": 0, "comments": 0, "attachments": 0}}
	counts, err := si.run(r.Context())
	if err == nil {
		s.hub.publish(b.ID, "board-changed")
	}
	return counts, err
}

// methodImportScoped is the importScoped method: (target, doc, fields).
func (s *Server) methodImportScoped(r *http.Request, u *User, raw []json.RawMessage) (any, error) {
	if len(raw) < 2 {
		return nil, &methodError{"Match.Error", "Match failed"}
	}
	args := make([]any, len(raw))
	for i, a := range raw {
		if json.Unmarshal(a, &args[i]) != nil {
			return nil, &methodError{"Match.Error", "Match failed"}
		}
	}
	tg, ok := args[0].(map[string]any)
	doc, okDoc := args[1].(map[string]any)
	if !ok || !okDoc {
		return nil, &methodError{"Match.Error", "Match failed"}
	}
	t := scopedTarget{}
	for k, dst := range map[string]*string{"boardId": &t.BoardID, "swimlaneId": &t.SwimlaneID, "listId": &t.ListID, "cardId": &t.CardID} {
		v, has := tg[k]
		if !has || v == nil {
			if k == "boardId" {
				return nil, &methodError{"Match.Error", "Match failed"}
			}
			continue
		}
		str, isStr := v.(string)
		if !isStr {
			return nil, &methodError{"Match.Error", "Match failed"}
		}
		*dst = str
	}
	var want exportWant
	if len(args) > 2 && args[2] != nil {
		list, ok := args[2].([]any)
		if !ok {
			return nil, &methodError{"Match.Error", "Match failed"}
		}
		for _, x := range list {
			k, ok := x.(string)
			if !ok {
				return nil, &methodError{"Match.Error", "Match failed"}
			}
			if want == nil {
				want = exportWant{}
			}
			want[k] = true
		}
	}
	if s.importDisabled(r.Context()) {
		return nil, &methodError{"import-disabled", "Import is disabled by the administrator."}
	}
	var b Board
	if !validID(t.BoardID) || s.db.Collection("boards").FindOne(r.Context(), bson.M{"_id": eq(t.BoardID)}).Decode(&b) != nil {
		return nil, &methodError{"board-not-found", "Board not found"}
	}
	if !b.Can(u.ID, "write") {
		return nil, &methodError{"error-notAuthorized", "Not authorized"}
	}
	if f, has := doc["_format"]; has && f != nil && f != "wekan-board-1.0.0" {
		return nil, &methodError{"invalid-format", "Unknown export format: " + cdString(f)}
	}
	clean, err := s.secureImport(r, u, "wekan-scoped", doc)
	if err != nil {
		return nil, &methodError{"import-too-large", "Import/export limit exceeded"}
	}
	safe, _ := clean.(map[string]any)
	counts, err := s.importScopedInto(r, u, &b, t, safe, want, "method:importScoped", nil)
	var pe errImportPlacement
	if errors.As(err, &pe) {
		return nil, &methodError{"error-import-placement", pe.Error()}
	}
	if err != nil {
		return nil, &methodError{"import-failed", err.Error()}
	}
	return counts, nil
}
