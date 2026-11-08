package web

import (
	"context"
	"encoding/base64"
	"net/http"
	"os"

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
	out, err := s.exportBoard(r.Context(), &b, r.URL.Query().Get("attachments") == "false")
	if err != nil {
		apiError(w, http.StatusInternalServerError, "Export failed")
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="export-board-`+b.ID+`.json"`)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

// exportBoard is WeKan's Exporter.build: the board and everything of it, each
// without its boardId, the users it names with only their public fields.
func (s *Server) exportBoard(ctx context.Context, b *Board, withoutFiles bool) (any, error) {
	raw, err := s.boardDocs(ctx, b, withoutFiles)
	if err != nil {
		return nil, err
	}
	return sanitizeTransfer(raw, true)
}

// boardDocs is the board and everything of it, as the export has them.
func (s *Server) boardDocs(ctx context.Context, b *Board, withoutFiles bool) (bson.M, error) {
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

	atts, err := all("attachments", bson.M{"meta.boardId": b.ID, "deletedAt": nil})
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
				if p, err := s.attachmentFile(&at); err == nil {
					if data, err := os.ReadFile(p); err == nil {
						e["file"] = base64.StdEncoding.EncodeToString(data)
					}
				}
			}
		}
		exported = append(exported, e)
	}
	result["attachments"] = exported

	byBoard := bson.M{"boardId": b.ID}
	lists, err := all("lists", byBoard, "boardId")
	if err != nil {
		return nil, err
	}
	cards, err := all("cards", bson.M{"boardId": b.ID, "linkedId": bson.M{"$in": bson.A{"", nil}}}, "boardId")
	if err != nil {
		return nil, err
	}
	swimlanes, err := all("swimlanes", byBoard, "boardId")
	if err != nil {
		return nil, err
	}
	customFields, err := all("customFields", bson.M{"boardIds": b.ID}, "boardIds")
	if err != nil {
		return nil, err
	}
	for _, row := range append(append(append([]bson.M{result}, cards...), lists...), swimlanes...) {
		delete(row, "scrum")
		delete(row, "scrumRevision")
	}
	cardIDs := bson.A{}
	for _, c := range cards {
		cardIDs = append(cardIDs, c["_id"])
	}
	inCards := bson.M{"$in": cardIDs}
	comments, err := all("card_comments", bson.M{"cardId": inCards}, "boardId")
	if err != nil {
		return nil, err
	}
	activities, err := all("activities", bson.M{"$or": bson.A{byBoard, bson.M{"cardId": inCards}}}, "boardId")
	if err != nil {
		return nil, err
	}
	rules, err := all("rules", byBoard, "boardId")
	if err != nil {
		return nil, err
	}
	checklists, err := all("checklists", bson.M{"cardId": inCards})
	if err != nil {
		return nil, err
	}
	items, err := all("checklistItems", bson.M{"cardId": inCards})
	if err != nil {
		return nil, err
	}
	subtasks, err := all("cards", bson.M{"boardId": b.ID, "parentId": inCards})
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
