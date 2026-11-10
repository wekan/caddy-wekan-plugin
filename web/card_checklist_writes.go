package web

import (
	"context"
	"net/http"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
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
	m.HandleFunc("POST /b/{board}/cards/{card}/checklists/{checklist}/items", s.page(s.addChecklistItemPage))
	m.HandleFunc("POST /b/{board}/cards/{card}/checklists/{checklist}/items/{item}/toggle", s.page(s.toggleChecklistItemPage))
	m.HandleFunc("POST /b/{board}/cards/{card}/checklists/{checklist}/delete", s.page(s.deleteChecklistPage))
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
	s.hub.publish(b.ID, "board-changed")
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
	if _, err := s.db.Collection("checklists").InsertOne(r.Context(), bson.M{"_id": id, "cardId": c.ID, "boardId": b.ID, "title": title,
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
	if _, err := s.db.Collection("checklistItems").InsertOne(r.Context(), bson.M{"_id": id, "cardId": c.ID, "boardId": b.ID, "checklistId": clID,
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
	itemID := r.PathValue("item")
	var it bson.M
	if !validID(itemID) || s.db.Collection("checklistItems").FindOne(r.Context(), bson.M{"_id": eq(itemID), "checklistId": eq(clID), "cardId": eq(c.ID)}).Decode(&it) != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
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
	if _, err := s.db.Collection("checklistItems").UpdateOne(r.Context(), bson.M{"_id": eq(itemID)},
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
	if _, err := s.db.Collection("checklists").DeleteOne(r.Context(), bson.M{"_id": eq(clID), "cardId": eq(c.ID)}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = s.db.Collection("checklistItems").DeleteMany(r.Context(), bson.M{"checklistId": eq(clID)})
	s.backToCard(w, r, b, c)
}
