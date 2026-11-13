package web

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"regexp"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// The board export of Node.js WeKan (models/export.js, models/exporter.js):
// GET /api/boards/{board}/export, format wekan-board-1.0.0, so a board exported
// from Weddy imports into WeKan and the other way round.
func (s *Server) exportRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/boards/{board}/export", s.exportable(s.apiExport))
}

// exportUser is who asks: the request's token or cookie, or - a download
// link - ?authToken. ok false: an authToken that is no login token.
func (s *Server) exportUser(r *http.Request) (*User, bool) {
	if t := r.URL.Query().Get("authToken"); t != "" {
		if len(t) > 10000 {
			return nil, false
		}
		u, err := s.userByToken(r.Context(), t)
		if err != nil {
			// A download link's own token (exportDownloadToken).
			if u = s.exportTokenUser(r.Context(), t); u != nil {
				return u, true
			}
		}
		return u, err == nil
	}
	return s.currentUser(r), true
}

func (s *Server) apiExport(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("board")
	var b Board
	if !validID(id) || s.db.Collection("boards").FindOne(r.Context(), bson.M{"_id": eq(id)}).Decode(&b) != nil {
		apiError(w, http.StatusNotFound, "Not found")
		return
	}
	u, ok := s.exportUser(r)
	if !ok {
		apiError(w, http.StatusUnauthorized, "Invalid token")
		return
	}
	// models/lib/exportAccess.js: a board its reader may see, and not an
	// assigned-only member's - the export holds every card.
	if b.Permission != "public" {
		if u == nil {
			apiError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}
		if m := b.member(u.ID); m == nil || m.AssignedOnly() {
			apiError(w, http.StatusForbidden, "Forbidden")
			return
		}
	}
	if !s.fieldExportAllowed(r.Context(), &b, u) {
		apiError(w, http.StatusForbidden, errFieldExport)
		return
	}
	q := r.URL.Query()
	out, err := s.exportBoardWith(r.Context(), &b, exportDocOptions{WithoutFiles: q.Get("attachments") == "false",
		Want: exportWanted(q.Get("fields")), Scope: exportScopeOf(q), r: r})
	if err != nil {
		apiError(w, http.StatusInternalServerError, "Export failed")
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="export-board-`+b.ID+`.json"`)
	w.Header().Set("Cache-Control", "no-store")
	// Its dates as WeKan's export writes them (js_dates.go).
	writeJSON(w, http.StatusOK, jsDates(out))
}

// exportBoard is WeKan's Exporter.build: the board and everything of it, each
// without its boardId, the users it names with only their public fields.
func (s *Server) exportBoard(ctx context.Context, b *Board, withoutFiles bool) (any, error) {
	return s.exportBoardWith(ctx, b, exportDocOptions{WithoutFiles: withoutFiles})
}

// exportScope is parseExportScope: which cards an export is about - a
// swimlane's, a list's, one card or the card of one checklist - each id
// only when it is one.
type exportScope struct{ SwimlaneID, ListID, CardID, ChecklistID string }

var exportScopeID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func exportScopeOf(q url.Values) exportScope {
	get := func(k string) string {
		if v := q.Get(k); exportScopeID.MatchString(v) {
			return v
		}
		return ""
	}
	return exportScope{get("swimlaneId"), get("listId"), get("cardId"), get("checklistId")}
}

func (sc exportScope) scoped() bool {
	return sc.SwimlaneID != "" || sc.ListID != "" || sc.CardID != "" || sc.ChecklistID != ""
}

// exportDocOptions are an export's: without the files, its selection (nil
// is all), its scope, and the request it answers.
type exportDocOptions struct {
	WithoutFiles bool
	Want         exportWant
	Scope        exportScope
	r            *http.Request
}

// exportBoardWith is the streaming exporter's document (models/exporter.js):
// the board, what the selection asks for of it, for the scope's cards.
func (s *Server) exportBoardWith(ctx context.Context, b *Board, o exportDocOptions) (any, error) {
	raw, err := s.boardDocs(ctx, b, o)
	if err != nil {
		return nil, err
	}
	// Admin Panel -> Anonymize export users, as WeKan's build() does it.
	users, _ := raw["users"].([]bson.M)
	if a := s.exportAnonymizer(ctx, b, users); a != nil {
		for _, u := range users {
			a.user(u)
		}
		cards, _ := raw["cards"].([]bson.M)
		comments, _ := raw["comments"].([]bson.M)
		a.cards(cards)
		a.comments(comments)
	}
	return sanitizeTransfer(raw, true)
}

// boardDocs is the board and everything of it, as the export has them.
func (s *Server) boardDocs(ctx context.Context, b *Board, o exportDocOptions) (bson.M, error) {
	withoutFiles, want, sc := o.WithoutFiles, o.Want, o.Scope
	none := bson.M{"_id": eq("__none__")}
	all := func(coll string, filter bson.M, drop ...string) ([]bson.M, error) {
		proj := bson.M{}
		for _, f := range drop {
			proj[f] = 0
		}
		opts := options.Find()
		if len(proj) > 0 {
			opts.SetProjection(proj)
		}
		cur, err := s.db.Collection(coll).Find(ctx, filter, opts)
		if err != nil {
			return nil, err
		}
		docs := []bson.M{}
		return docs, cur.All(ctx, &docs)
	}
	var board bson.M
	if err := s.db.Collection("boards").FindOne(ctx, bson.M{"_id": b.ID}, options.FindOne().SetProjection(bson.M{"stars": 0})).Decode(&board); err != nil {
		return nil, err
	}
	result := bson.M{}
	for k, v := range board {
		result[k] = v
	}
	result["_format"] = "wekan-board-1.0.0"

	// _scopedCardSelector: the cards, resolved before anything keyed off them.
	cardSel := bson.M{"boardId": b.ID, "linkedId": bson.M{"$in": bson.A{"", nil}}}
	switch {
	case sc.CardID != "":
		cardSel["_id"] = eq(sc.CardID)
	case sc.ChecklistID != "":
		var cl struct {
			CardID string `bson:"cardId"`
		}
		cardSel["_id"] = eq("__no_such_card__")
		if s.db.Collection("checklists").FindOne(ctx, bson.M{"_id": eq(sc.ChecklistID)}).Decode(&cl) == nil {
			cardSel["_id"] = eq(cl.CardID)
			// CrossBoardExportBleed (GHSA-8885-xrq4-j47h): a checklist on
			// another board's card names nothing on this one, and no menu
			// asks for it.
			if s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(cl.CardID), "boardId": b.ID}).Err() != nil && o.r != nil {
				func() {
					defer func() { _ = recover() }() // logging must never break the guard
					s.recordSecurity(o.r, securityEvent{Key: "authz.export-checklist-scope", Action: "blocked", Source: "export",
						Detail: "checklist scope from another board"})
				}()
			}
		}
	default:
		if sc.SwimlaneID != "" {
			cardSel["swimlaneId"] = eq(sc.SwimlaneID)
		}
		if sc.ListID != "" {
			cardSel["listId"] = eq(sc.ListID)
		}
	}
	scoped := sc.scoped()
	var scopedIDs bson.A
	if scoped {
		ids, err := all("cards", cardSel)
		if err != nil {
			return nil, err
		}
		for _, c := range ids {
			scopedIDs = append(scopedIDs, c["_id"])
		}
	}
	attSel := bson.M{"meta.boardId": b.ID, "deletedAt": nil}
	switch {
	case !want.has("attachments"):
		attSel = none
	case scoped:
		attSel = bson.M{"meta.cardId": dbIn(scopedIDs), "deletedAt": nil}
	}
	atts, err := all("attachments", attSel)
	if err != nil {
		return nil, err
	}
	exported := []bson.M{}
	for _, a := range atts {
		meta, _ := a["meta"].(bson.D)
		mm := docMap(meta)
		e := bson.M{"_id": a["_id"], "cardId": mm["cardId"], "source": mm["source"], "name": a["name"], "type": a["type"]}
		if !withoutFiles {
			e["file"] = nil
			var at Attachment
			doc, _ := bson.Marshal(a)
			if bson.Unmarshal(doc, &at) == nil {
				if data, err := s.readAttachmentBytes(ctx, &at); err == nil {
					e["file"] = base64.StdEncoding.EncodeToString(data)
				}
			}
		}
		exported = append(exported, e)
	}
	result["attachments"] = exported

	byBoard := bson.M{"boardId": b.ID}
	// A scoped export carries the lists and swimlanes its cards are in.
	listSel, laneSel := byBoard, byBoard
	if scoped {
		refs, err := all("cards", bson.M{"_id": dbIn(scopedIDs)})
		if err != nil {
			return nil, err
		}
		listIDs, laneIDs := bson.A{}, bson.A{}
		for _, c := range refs {
			listIDs = append(listIDs, c["listId"])
			laneIDs = append(laneIDs, c["swimlaneId"])
		}
		if sc.ListID != "" {
			listIDs = append(listIDs, sc.ListID)
		}
		if sc.SwimlaneID != "" {
			laneIDs = append(laneIDs, sc.SwimlaneID)
		}
		listSel, laneSel = bson.M{"boardId": b.ID, "_id": dbIn(listIDs)}, bson.M{"boardId": b.ID, "_id": dbIn(laneIDs)}
	}
	lists, err := all("lists", listSel, "boardId")
	if err != nil {
		return nil, err
	}
	cards, err := all("cards", cardSel, "boardId")
	if err != nil {
		return nil, err
	}
	swimlanes, err := all("swimlanes", laneSel, "boardId")
	if err != nil {
		return nil, err
	}
	cfSel := bson.M{"boardIds": b.ID}
	if !want.has("custom-fields") {
		cfSel = none
	}
	customFields, err := all("customFields", cfSel, "boardIds")
	if err != nil {
		return nil, err
	}
	for _, row := range append(append(append([]bson.M{result}, cards...), lists...), swimlanes...) {
		delete(row, "scrum")
		delete(row, "scrumRevision")
	}
	// The native Scrum transfer (scrumTransferExport.js), of the exported
	// cards, lists and swimlanes - unless the selection leaves Scrum out.
	var scrumTransfer map[string]any
	if want.has("scrum") {
		idsOf := func(docs []bson.M) []string {
			out := []string{}
			for _, d := range docs {
				out = append(out, docString(d, "_id"))
			}
			return out
		}
		t, losses, err := s.exportScrumTransfer(ctx, b.ID, idsOf(cards), idsOf(lists), idsOf(swimlanes), scoped, want.has("custom-fields"))
		if err != nil {
			return nil, err
		}
		if t != nil {
			scrumTransfer = t
			result["scrumTransfer"], result["scrumTransferLosses"] = t, losses
		}
	}
	cardIDs := bson.A{}
	for _, c := range cards {
		cardIDs = append(cardIDs, c["_id"])
	}
	inCards := bson.M{"$in": cardIDs}
	pick := func(field string, sel bson.M) bson.M {
		if !want.has(field) {
			return none
		}
		return sel
	}
	comments, err := all("card_comments", pick("comments", bson.M{"cardId": inCards}), "boardId")
	if err != nil {
		return nil, err
	}
	// A scoped export's activities are its cards', not the board's.
	actSel := bson.M{"$or": bson.A{byBoard, bson.M{"cardId": inCards}}}
	if scoped {
		actSel = bson.M{"cardId": inCards}
	}
	activities, err := all("activities", pick("activities", actSel), "boardId")
	if err != nil {
		return nil, err
	}
	ruleSel := byBoard
	if scoped {
		ruleSel = none
	}
	rules, err := all("rules", ruleSel, "boardId")
	if err != nil {
		return nil, err
	}
	// A checklist scope: that checklist, on a card of this export.
	clSel, itemSel := bson.M{"cardId": inCards}, bson.M{"cardId": inCards}
	if sc.ChecklistID != "" {
		clSel = bson.M{"_id": eq(sc.ChecklistID), "cardId": inCards}
		itemSel = bson.M{"checklistId": eq(sc.ChecklistID), "cardId": inCards}
	}
	checklists, err := all("checklists", pick("checklists", clSel))
	if err != nil {
		return nil, err
	}
	items, err := all("checklistItems", pick("checklists", itemSel))
	if err != nil {
		return nil, err
	}
	// #3626: a subtask under any of its parents.
	subtasks, err := all("cards", pick("subtasks", bson.M{"boardId": b.ID, "$or": bson.A{bson.M{"parentId": inCards}, bson.M{"parentIds": inCards}}}))
	if err != nil {
		return nil, err
	}
	triggerIDs, actionIDs := bson.A{}, bson.A{}
	for _, rule := range rules {
		triggerIDs = append(triggerIDs, rule["triggerId"])
		actionIDs = append(actionIDs, rule["actionId"])
		if x, ok := rule["extraTriggerIds"].(bson.A); ok {
			triggerIDs = append(triggerIDs, x...)
		}
		if x, ok := rule["extraActionIds"].(bson.A); ok {
			actionIDs = append(actionIDs, x...)
		}
	}
	triggers, err := all("triggers", bson.M{"_id": dbIn(triggerIDs)}, "boardId")
	if err != nil {
		return nil, err
	}
	actions, err := all("actions", bson.M{"_id": dbIn(actionIDs)}, "boardId")
	if err != nil {
		return nil, err
	}
	for k, v := range map[string][]bson.M{"lists": lists, "cards": cards, "swimlanes": swimlanes, "customFields": customFields,
		"comments": comments, "activities": activities, "rules": rules, "checklists": checklists, "checklistItems": items,
		"subtaskItems": subtasks, "triggers": triggers, "actions": actions} {
		result[k] = v
	}

	// The users it names: only these fields, never a secret.
	ids := map[string]bool{}
	add := func(v any) {
		if id, ok := v.(string); ok && id != "" {
			ids[id] = true
		}
	}
	addAll := func(v any) {
		if a, ok := v.(bson.A); ok {
			for _, e := range a {
				add(e)
			}
		}
	}
	if members, ok := board["members"].(bson.A); ok {
		for _, m := range members {
			if d, ok := m.(bson.D); ok {
				add(docMap(d)["userId"])
			}
		}
	}
	for _, l := range lists {
		add(l["userId"])
	}
	for _, c := range cards {
		add(c["userId"])
		addAll(c["members"])
		addAll(c["requesters"])
		addAll(c["assigners"])
	}
	for _, group := range [][]bson.M{comments, activities, checklists} {
		for _, d := range group {
			add(d["userId"])
		}
	}
	if scrumTransfer != nil {
		st, _ := scrumTransfer["settings"].(map[string]any)
		add(st["productOwnerId"])
		add(st["scrumMasterId"])
		for _, id := range stringsOf(st["developerIds"]) {
			add(id)
		}
		for _, k := range []string{"sprints", "releases", "events"} {
			for _, r := range asDocs(scrumTransfer[k]) {
				add(r["createdBy"])
				add(r["updatedBy"])
			}
		}
	}
	userIDs := bson.A{}
	for id := range ids {
		userIDs = append(userIDs, id)
	}
	cur, err := s.db.Collection("users").Find(ctx, bson.M{"_id": dbIn(userIDs)}, options.Find().SetProjection(bson.M{
		"_id": 1, "username": 1, "profile.fullname": 1, "profile.initials": 1, "profile.avatarUrl": 1}))
	if err != nil {
		return nil, err
	}
	users := []bson.M{}
	if err := cur.All(ctx, &users); err != nil {
		return nil, err
	}
	result["users"] = users
	return result, nil
}

// dbIn matches any of ids read from documents: only the ones that are ids, so
// a value stored by something else can never become an operator here.
func dbIn(ids bson.A) bson.M {
	ok := []string{}
	for _, v := range ids {
		if id, isString := v.(string); isString && validID(id) {
			ok = append(ok, id)
		}
	}
	return bson.M{"$in": ok}
}
