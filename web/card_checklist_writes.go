package web

import (
	"context"
	"net/http"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// The card details' checklist writes, as WeKan's checklists.js makes them
// through Checklists and ChecklistItems and their server hooks
// (server/models/checklists.js, checklistItems.js): a checklist added after
// the last one, an item after the last of its checklist, an item checked
// or unchecked - by whoever may change the card, or a Worker, who may tick
// a box and nothing more (#3307, workerChecklistItemToggle.js) - and a
// checklist deleted with its items (#6816). Each writes WeKan's activity:
// addChecklist, addChecklistItem, checkedItem / uncheckedItem with
// completeChecklist or uncompleteChecklist, and removeChecklist.

func (s *Server) registerChecklistWrites(m *http.ServeMux) {
	m.HandleFunc("POST /b/{board}/cards/{card}/checklists", s.page(s.addChecklistPage))
	m.HandleFunc("POST /b/{board}/cards/{card}/subtasks", s.page(s.addSubtaskPage))
	m.HandleFunc("POST /b/{board}/cards/{card}/checklists/{checklist}/items", s.page(s.addChecklistItemPage))
	m.HandleFunc("POST /b/{board}/cards/{card}/checklists/{checklist}/items/{item}/toggle", s.page(s.toggleChecklistItemPage))
	m.HandleFunc("POST /b/{board}/cards/{card}/checklists/{checklist}/delete", s.page(s.deleteChecklistPage))
	m.HandleFunc("POST /b/{board}/cards/{card}/checklists/{checklist}/title", s.page(s.renameChecklistPage))
	m.HandleFunc("POST /b/{board}/cards/{card}/checklists/{checklist}/items/{item}/title", s.page(s.checklistItemTitlePage))
	m.HandleFunc("POST /b/{board}/cards/{card}/checklists/{checklist}/items/{item}/delete", s.page(s.deleteChecklistItemPage))
	m.HandleFunc("POST /b/{board}/cards/{card}/checklists/{checklist}/items/{item}/assignee", s.page(s.checklistItemAssigneePage))
	m.HandleFunc("POST /b/{board}/cards/{card}/checklists/{checklist}/items/sort", s.page(s.sortChecklistItemsPage))
	m.HandleFunc("POST /b/{board}/cards/{card}/checklists/{checklist}/due", s.page(s.checklistDuePage))
	m.HandleFunc("POST /b/{board}/cards/{card}/checklists/{checklist}/items/{item}/due", s.page(s.checklistDuePage))
	m.HandleFunc("POST /b/{board}/cards/{card}/checklists/{checklist}/move-to", s.page(s.moveChecklistPage))
	m.HandleFunc("POST /b/{board}/cards/{card}/checklists/{checklist}/copy-to", s.page(s.copyChecklistPage))
}

// canCheckChecklistItem is Utils.canCheckChecklistItem: canModifyCard, or a
// Worker of the board.
func (s *Server) canCheckChecklistItem(ctx context.Context, b *Board, u *User, c *Card) bool {
	if b.WritableIn(ctx, s, u, "write", c) {
		return true
	}
	m := b.member(u.ID)
	return m != nil && m.IsActive && m.IsWorker
}

// checklistTarget is the path's board, card and - when named - checklist,
// or the answer already written.
func (s *Server) checklistTarget(w http.ResponseWriter, r *http.Request, u *User, withChecklist bool) (*Board, *Card, bson.M, bool) {
	b, err := s.boardFor(r.Context(), r.PathValue("board"), u)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return nil, nil, nil, false
	}
	id := r.PathValue("card")
	var c Card
	if !validID(id) || s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": eq(id), "boardId": b.ID}).Decode(&c) != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return nil, nil, nil, false
	}
	var cl bson.M
	if withChecklist {
		cid := r.PathValue("checklist")
		if !validID(cid) || s.db.Collection("checklists").FindOne(r.Context(), bson.M{"_id": eq(cid), "cardId": eq(c.ID)}).Decode(&cl) != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return nil, nil, nil, false
		}
	}
	return b, &c, cl, true
}

func (s *Server) backToCard(w http.ResponseWriter, r *http.Request, b *Board, c *Card) {
	s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	http.Redirect(w, r, "/b/"+b.ID+"/"+b.Slug+"/"+c.ID, http.StatusSeeOther)
}

func checklistTitleOf(w http.ResponseWriter, r *http.Request) (string, bool) {
	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" || len(title) > 10000 {
		http.Error(w, "a title is needed", http.StatusBadRequest)
		return "", false
	}
	return title, true
}

func (s *Server) addChecklistPage(w http.ResponseWriter, r *http.Request, u *User) {
	b, c, _, ok := s.checklistTarget(w, r, u, false)
	if !ok {
		return
	}
	if !b.WritableIn(r.Context(), s, u, "write", c) {
		http.Error(w, "your role on this board cannot change it", http.StatusForbidden)
		return
	}
	title, ok := checklistTitleOf(w, r)
	if !ok {
		return
	}
	id := randomID()
	now := time.Now().UTC()
	if _, err := s.insertRecorded(r.Context(), u.ID, "checklist", bson.M{"_id": id, "cardId": c.ID, "boardId": b.ID, "title": title,
		"sort": s.nextSort(r.Context(), "checklists", bson.M{"cardId": eq(c.ID)}), "createdAt": now, "modifiedAt": now}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.activity(r.Context(), u.ID, "addChecklist", bson.M{"cardId": c.ID, "boardId": b.ID, "checklistId": id, "checklistName": title,
		"listId": c.ListID, "swimlaneId": c.SwimlaneID})
	s.backToCard(w, r, b, c)
}

func (s *Server) addChecklistItemPage(w http.ResponseWriter, r *http.Request, u *User) {
	b, c, cl, ok := s.checklistTarget(w, r, u, true)
	if !ok {
		return
	}
	if !b.WritableIn(r.Context(), s, u, "write", c) {
		http.Error(w, "your role on this board cannot change it", http.StatusForbidden)
		return
	}
	title, ok := checklistTitleOf(w, r)
	if !ok {
		return
	}
	clID := docString(cl, "_id")
	id := randomID()
	now := time.Now().UTC()
	if _, err := s.insertRecorded(r.Context(), u.ID, "checklistItem", bson.M{"_id": id, "cardId": c.ID, "boardId": b.ID, "checklistId": clID,
		"title": title, "sort": s.nextSort(r.Context(), "checklistItems", bson.M{"checklistId": eq(clID)}), "isFinished": false,
		"createdAt": now, "modifiedAt": now}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.activity(r.Context(), u.ID, "addChecklistItem", bson.M{"cardId": c.ID, "boardId": b.ID, "checklistId": clID, "checklistItemId": id,
		"checklistItemName": title, "listId": c.ListID, "swimlaneId": c.SwimlaneID})
	s.backToCard(w, r, b, c)
}

// checklistFinished is publishChekListCompleted's isChecklistFinished.
func (s *Server) checklistFinished(ctx context.Context, cl bson.M) bool {
	if cl["hideAllChecklistItems"] == true {
		return true
	}
	id := docString(cl, "_id")
	all, _ := s.db.Collection("checklistItems").CountDocuments(ctx, bson.M{"checklistId": eq(id)})
	done, _ := s.db.Collection("checklistItems").CountDocuments(ctx, bson.M{"checklistId": eq(id), "isFinished": true})
	return all > 0 && all == done
}

func (s *Server) toggleChecklistItemPage(w http.ResponseWriter, r *http.Request, u *User) {
	b, c, cl, ok := s.checklistTarget(w, r, u, true)
	if !ok {
		return
	}
	if !s.canCheckChecklistItem(r.Context(), b, u, c) {
		http.Error(w, "your role on this board cannot change it", http.StatusForbidden)
		return
	}
	clID := docString(cl, "_id")
	it, ok := s.checklistItemOf(w, r, c, cl)
	if !ok {
		return
	}
	itemID := docString(it, "_id")
	finished, _ := it["isFinished"].(bool)
	place := bson.M{"cardId": c.ID, "boardId": b.ID, "checklistId": clID, "listId": c.ListID, "swimlaneId": c.SwimlaneID}
	with := func(extra bson.M) bson.M {
		out := bson.M{}
		for k, v := range place {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	// ChecklistItems.before.update: a finished checklist is uncompleted.
	wasFinished := s.checklistFinished(r.Context(), cl)
	if _, err := s.entityUpdateOne(r.Context(), u.ID, "checklistItem", bson.M{"_id": eq(itemID)},
		bson.M{"$set": bson.M{"isFinished": !finished, "modifiedAt": time.Now().UTC()}}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if wasFinished {
		s.activity(r.Context(), u.ID, "uncompleteChecklist", with(bson.M{"checklistName": docString(cl, "title")}))
	}
	kind := "checkedItem"
	if finished {
		kind = "uncheckedItem"
	}
	s.activity(r.Context(), u.ID, kind, with(bson.M{"checklistItemId": itemID, "checklistItemName": docString(it, "title")}))
	if s.checklistFinished(r.Context(), cl) {
		s.activity(r.Context(), u.ID, "completeChecklist", with(bson.M{"checklistName": docString(cl, "title")}))
	}
	s.backToCard(w, r, b, c)
}

func (s *Server) deleteChecklistPage(w http.ResponseWriter, r *http.Request, u *User) {
	b, c, cl, ok := s.checklistTarget(w, r, u, true)
	if !ok {
		return
	}
	if !b.WritableIn(r.Context(), s, u, "write", c) {
		http.Error(w, "your role on this board cannot change it", http.StatusForbidden)
		return
	}
	clID := docString(cl, "_id")
	s.activity(r.Context(), u.ID, "removeChecklist", bson.M{"cardId": c.ID, "boardId": b.ID, "checklistId": clID,
		"checklistName": docString(cl, "title"), "listId": c.ListID, "swimlaneId": c.SwimlaneID})
	if _, err := s.deleteOneRecorded(r.Context(), u.ID, "checklist", bson.M{"_id": eq(clID), "cardId": eq(c.ID)}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = s.db.Collection("checklistItems").DeleteMany(r.Context(), bson.M{"checklistId": eq(clID)})
	s.backToCard(w, r, b, c)
}

// renameChecklistPage is editChecklistForm's checklist.setTitle.
func (s *Server) renameChecklistPage(w http.ResponseWriter, r *http.Request, u *User) {
	b, c, cl, ok := s.checklistTarget(w, r, u, true)
	if !ok {
		return
	}
	if !b.WritableIn(r.Context(), s, u, "write", c) {
		http.Error(w, "your role on this board cannot change it", http.StatusForbidden)
		return
	}
	title, ok := checklistTitleOf(w, r)
	if !ok {
		return
	}
	if _, err := s.entityUpdateOne(r.Context(), u.ID, "checklist", bson.M{"_id": eq(docString(cl, "_id")), "cardId": eq(c.ID)},
		bson.M{"$set": bson.M{"title": title, "modifiedAt": time.Now().UTC()}}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.backToCard(w, r, b, c)
}

// checklistItemOf is the path's item, of the path's checklist and card.
func (s *Server) checklistItemOf(w http.ResponseWriter, r *http.Request, c *Card, cl bson.M) (bson.M, bool) {
	id := r.PathValue("item")
	var it bson.M
	if !validID(id) || s.db.Collection("checklistItems").FindOne(r.Context(), bson.M{"_id": eq(id), "checklistId": eq(docString(cl, "_id")), "cardId": eq(c.ID)}).Decode(&it) != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return nil, false
	}
	return it, true
}

// checklistItemTitlePage is editChecklistItemForm's item.setTitle. WeKan's
// after.update hook writes a checkedItem/uncheckedItem activity on every
// update, a title edit too; an edit is not a check, so none is written.
func (s *Server) checklistItemTitlePage(w http.ResponseWriter, r *http.Request, u *User) {
	b, c, cl, ok := s.checklistTarget(w, r, u, true)
	if !ok {
		return
	}
	if !b.WritableIn(r.Context(), s, u, "write", c) {
		http.Error(w, "your role on this board cannot change it", http.StatusForbidden)
		return
	}
	it, ok := s.checklistItemOf(w, r, c, cl)
	if !ok {
		return
	}
	title, ok := checklistTitleOf(w, r)
	if !ok {
		return
	}
	if _, err := s.entityUpdateOne(r.Context(), u.ID, "checklistItem", bson.M{"_id": eq(docString(it, "_id"))},
		bson.M{"$set": bson.M{"title": title, "modifiedAt": time.Now().UTC()}}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.backToCard(w, r, b, c)
}

// deleteChecklistItemPage is checklistItemDeletePopup's ChecklistItems.remove:
// no activity (itemRemover, #1598 keeps the item's checked trail).
func (s *Server) deleteChecklistItemPage(w http.ResponseWriter, r *http.Request, u *User) {
	b, c, cl, ok := s.checklistTarget(w, r, u, true)
	if !ok {
		return
	}
	if !b.WritableIn(r.Context(), s, u, "write", c) {
		http.Error(w, "your role on this board cannot change it", http.StatusForbidden)
		return
	}
	it, ok := s.checklistItemOf(w, r, c, cl)
	if !ok {
		return
	}
	if _, err := s.deleteOneRecorded(r.Context(), u.ID, "checklistItem", bson.M{"_id": eq(docString(it, "_id"))}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.backToCard(w, r, b, c)
}

// checklistDestination is copyAndMoveChecklist's choice: a card the user may
// change, on a board they belong to - or the answer already written.
func (s *Server) checklistDestination(w http.ResponseWriter, r *http.Request, u *User) (*Board, *Card, bool) {
	tb, err := s.boardFor(r.Context(), r.FormValue("boardId"), u)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return nil, nil, false
	}
	id := r.FormValue("cardId")
	var tc Card
	if !validID(id) || s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": eq(id), "boardId": tb.ID}).Decode(&tc) != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return nil, nil, false
	}
	// Both cards change: the caller may edit each (GHSA-r4gm-mjj9-g3x8).
	if !tb.WritableIn(r.Context(), s, u, "write", &tc) {
		http.Error(w, "your role on that board cannot change it", http.StatusForbidden)
		return nil, nil, false
	}
	return tb, &tc, true
}

// moveChecklistPage is WeKan's moveChecklist: the checklist, its items and
// its activities to another card.
func (s *Server) moveChecklistPage(w http.ResponseWriter, r *http.Request, u *User) {
	b, c, cl, ok := s.checklistTarget(w, r, u, true)
	if !ok {
		return
	}
	if !b.WritableIn(r.Context(), s, u, "write", c) {
		http.Error(w, "your role on this board cannot change it", http.StatusForbidden)
		return
	}
	tb, tc, ok := s.checklistDestination(w, r, u)
	if !ok {
		return
	}
	clID := docString(cl, "_id")
	now := time.Now().UTC()
	_, _ = s.db.Collection("activities").UpdateMany(r.Context(), bson.M{"checklistId": eq(clID)}, bson.M{"$set": bson.M{"cardId": tc.ID}})
	_, _ = s.db.Collection("checklistItems").UpdateMany(r.Context(), bson.M{"checklistId": eq(clID)},
		bson.M{"$set": bson.M{"cardId": tc.ID, "boardId": tb.ID, "modifiedAt": now}})
	if _, err := s.entityUpdateOne(r.Context(), u.ID, "checklist", bson.M{"_id": eq(clID), "cardId": eq(c.ID)},
		bson.M{"$set": bson.M{"cardId": tc.ID, "boardId": tb.ID, "modifiedAt": now}}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if tb.ID != b.ID {
		s.hub.publishFrom(tb.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	}
	s.backToCard(w, r, b, c)
}

// copyChecklistPage is checklist.copy: the checklist and its items, as they
// are, on another card - without an activity per item (#5688).
func (s *Server) copyChecklistPage(w http.ResponseWriter, r *http.Request, u *User) {
	b, c, cl, ok := s.checklistTarget(w, r, u, true)
	if !ok {
		return
	}
	if !b.WritableIn(r.Context(), s, u, "write", c) {
		http.Error(w, "your role on this board cannot change it", http.StatusForbidden)
		return
	}
	tb, tc, ok := s.checklistDestination(w, r, u)
	if !ok {
		return
	}
	clID := docString(cl, "_id")
	cur, err := s.db.Collection("checklistItems").Find(r.Context(), bson.M{"checklistId": eq(clID)})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var items []bson.M
	_ = cur.All(r.Context(), &items)
	now := time.Now().UTC()
	copyID := randomID()
	doc := bson.M{}
	for k, v := range cl {
		doc[k] = v
	}
	doc["_id"], doc["cardId"], doc["boardId"], doc["createdAt"], doc["modifiedAt"] = copyID, tc.ID, tb.ID, now, now
	if _, err := s.insertRecorded(r.Context(), u.ID, "checklist", doc); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, it := range items {
		it["_id"], it["checklistId"], it["cardId"], it["boardId"] = randomID(), copyID, tc.ID, tb.ID
		_, _ = s.insertRecorded(r.Context(), u.ID, "checklistItem", it)
	}
	if tb.ID != b.ID {
		s.hub.publishFrom(tb.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	}
	s.backToCard(w, r, b, c)
}

// checklistItemAssigneePage is checklistItemAssigneePopup's setAssignee: an
// active member of the board responsible for the item, or nobody (#1618).
// WeKan's popup offers only the board's active members; the server holds
// to the same.
func (s *Server) checklistItemAssigneePage(w http.ResponseWriter, r *http.Request, u *User) {
	b, c, cl, ok := s.checklistTarget(w, r, u, true)
	if !ok {
		return
	}
	if !b.WritableIn(r.Context(), s, u, "write", c) {
		http.Error(w, "your role on this board cannot change it", http.StatusForbidden)
		return
	}
	it, ok := s.checklistItemOf(w, r, c, cl)
	if !ok {
		return
	}
	who := r.FormValue("userId")
	update := bson.M{"$unset": bson.M{"assigneeId": ""}, "$set": bson.M{"modifiedAt": time.Now().UTC()}}
	if who != "" {
		m := b.member(who)
		if !validID(who) || m == nil || !m.IsActive {
			http.Error(w, "not an active member of this board", http.StatusBadRequest)
			return
		}
		update = bson.M{"$set": bson.M{"assigneeId": who, "modifiedAt": time.Now().UTC()}}
	}
	if _, err := s.entityUpdateOne(r.Context(), u.ID, "checklistItem", bson.M{"_id": eq(docString(it, "_id"))}, update); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.backToCard(w, r, b, c)
}

// sortChecklistItemsPage is checklistSortableItems' drop: the checklist's
// items in the order given, which must be exactly its items.
func (s *Server) sortChecklistItemsPage(w http.ResponseWriter, r *http.Request, u *User) {
	b, c, cl, ok := s.checklistTarget(w, r, u, true)
	if !ok {
		return
	}
	if !b.WritableIn(r.Context(), s, u, "write", c) {
		http.Error(w, "your role on this board cannot change it", http.StatusForbidden)
		return
	}
	clID := docString(cl, "_id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	order := r.PostForm["item"]
	cur, err := s.db.Collection("checklistItems").Find(r.Context(), bson.M{"checklistId": eq(clID), "cardId": eq(c.ID)})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var items []bson.M
	_ = cur.All(r.Context(), &items)
	have := map[string]bool{}
	for _, it := range items {
		have[docString(it, "_id")] = true
	}
	seen := map[string]bool{}
	for _, id := range order {
		if !have[id] || seen[id] {
			http.Error(w, "not this checklist's items", http.StatusBadRequest)
			return
		}
		seen[id] = true
	}
	if len(seen) != len(have) {
		http.Error(w, "not this checklist's items", http.StatusBadRequest)
		return
	}
	now := time.Now().UTC()
	for i, id := range order {
		_, _ = s.entityUpdateOne(r.Context(), u.ID, "checklistItem", bson.M{"_id": eq(id), "checklistId": eq(clID)},
			bson.M{"$set": bson.M{"sort": float64(i), "modifiedAt": now}})
	}
	// The page that dropped the item shows the order already: other pages
	// refresh, it does not - a refresh mid-interaction replaced the items.
	s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	w.WriteHeader(http.StatusNoContent)
}

// datePickerValue is editDateForm's submit: the time the browser made of the
// date and time picked (RFC 3339), or a delete. A year outside
// 1000-9999 is refused, as the date picker refuses a two-digit one.
func datePickerValue(r *http.Request) (at *time.Time, del bool, why string) {
	if r.FormValue("delete") == "1" {
		return nil, true, ""
	}
	t, err := time.Parse(time.RFC3339, r.FormValue("at"))
	if err != nil {
		return nil, false, "invalid-date"
	}
	if y := t.Year(); y < 1000 || y > 9999 {
		return nil, false, "invalid-year"
	}
	t = t.UTC()
	return &t, false, ""
}

// checklistDuePage is editChecklistDueDatePopup and
// editChecklistItemDueDatePopup (#4755): setDue or unsetDue of the
// checklist, or of its item, by whoever may change the card - unless the
// board has turned checklist due dates off (allowsChecklistDueDate).
func (s *Server) checklistDuePage(w http.ResponseWriter, r *http.Request, u *User) {
	b, c, cl, ok := s.checklistTarget(w, r, u, true)
	if !ok {
		return
	}
	if !b.WritableIn(r.Context(), s, u, "write", c) {
		http.Error(w, "your role on this board cannot change it", http.StatusForbidden)
		return
	}
	if !s.boardAllows(r.Context(), b.ID, "allowsChecklistDueDate") {
		http.Error(w, "this board does not use checklist due dates", http.StatusForbidden)
		return
	}
	coll, filter := "checklists", bson.M{"_id": eq(docString(cl, "_id")), "cardId": eq(c.ID)}
	if r.PathValue("item") != "" {
		it, ok := s.checklistItemOf(w, r, c, cl)
		if !ok {
			return
		}
		coll, filter = "checklistItems", bson.M{"_id": eq(docString(it, "_id"))}
	}
	at, del, why := datePickerValue(r)
	if why != "" {
		http.Error(w, why, http.StatusBadRequest)
		return
	}
	update := bson.M{"$unset": bson.M{"dueAt": ""}}
	if !del {
		update = bson.M{"$set": bson.M{"dueAt": *at}}
	}
	if _, err := s.db.Collection(coll).UpdateOne(r.Context(), filter, update); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.backToCard(w, r, b, c)
}

// boardAllows is a board's allows* setting: on unless it is false.
func (s *Server) boardAllows(ctx context.Context, boardID, field string) bool {
	var d bson.M
	if s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(boardID)}, options.FindOne().SetProjection(bson.M{field: 1})).Decode(&d) != nil {
		return true
	}
	return d[field] != false
}
