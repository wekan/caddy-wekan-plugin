package web

import (
	"context"
	"net/http"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The card's four dates in its details, as WeKan's cardDetails.jade and
// cardDate.js draw and write them: Received, Start, Due and End, each shown
// when the board allows it, as a badge or - for whoever may change the card,
// but not a Worker - a "+" opening the date picker (editCard*DatePopup), and
// a date set writes WeKan's a-<field> activity with its old value.

type cardDateKind struct{ Kind, Field, Allows, Icon, Label, Title, DefaultTime string }

var cardDateKinds = []cardDateKind{
	{"received", "receivedAt", "allowsReceivedDate", "fa-sign-out", msg("card-received"), msg("editCardReceivedDatePopup-title"), "now"},
	{"start", "startAt", "allowsStartDate", "fa-hourglass-start", msg("card-start"), msg("editCardStartDatePopup-title"), "now"},
	{"due", "dueAt", "allowsDueDate", "fa-clock-o", msg("card-due"), msg("editCardDueDatePopup-title"), "17:00"},
	{"end", "endAt", "allowsEndDate", "fa-hourglass-end", msg("card-end"), msg("editCardEndDatePopup-title"), "now"},
}

// cardDateRow is one of them on the opened card.
type cardDateRow struct {
	cardDateKind
	Date dueDate
}

// cardDates are the card's dates its board allows, in WeKan's order.
func (s *Server) cardDates(ctx context.Context, boardID string, card bson.M) []cardDateRow {
	var board bson.M
	_ = s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(boardID)}).Decode(&board)
	var out []cardDateRow
	for _, k := range cardDateKinds {
		if board[k.Allows] == false {
			continue
		}
		out = append(out, cardDateRow{k, dueDateOf(card[k.Field])})
	}
	return out
}

// canSetCardDates is canModifyCard, unless the member is a Worker.
func (s *Server) canSetCardDates(ctx context.Context, b *Board, u *User, c *Card) bool {
	m := b.member(u.ID)
	return b.WritableIn(ctx, s, u, "write", c) && (m == nil || !m.IsWorker)
}

func (s *Server) cardDateRoutes(m *http.ServeMux) {
	m.HandleFunc("POST /b/{board}/cards/{card}/dates/{kind}", s.page(s.cardDatePage))
	m.HandleFunc("POST /b/{board}/cards/{card}/description", s.page(s.cardDescriptionPage))
}

// rawBoardAllows is a board document's allows* setting: on unless false.
func rawBoardAllows(board bson.M, field string) bool { return board[field] != false }

// cardDescriptionPage is descriptionForm's save: the card's description, by
// whoever may change the card but a Worker, unless the board hides the
// description's text; a change writes WeKan's a-changedDescription.
func (s *Server) cardDescriptionPage(w http.ResponseWriter, r *http.Request, u *User) {
	b, c, _, ok := s.checklistTarget(w, r, u, false)
	if !ok {
		return
	}
	if !s.canSetCardDates(r.Context(), b, u, c) {
		http.Error(w, "your role on this board cannot change it", http.StatusForbidden)
		return
	}
	if !s.boardAllows(r.Context(), b.ID, "allowsDescriptionText") {
		http.Error(w, "this board does not use card descriptions", http.StatusForbidden)
		return
	}
	desc := strings.TrimRight(r.FormValue("description"), " \t\r\n")
	if len(desc) > 100000 {
		http.Error(w, "the description is too long", http.StatusBadRequest)
		return
	}
	if desc != c.Description {
		set := bson.M{"description": desc}
		if _, err := s.cardsUpdateOne(r.Context(), u.ID, bson.M{"_id": eq(c.ID), "boardId": eq(b.ID)},
			bson.M{"$set": bson.M{"description": desc, "modifiedAt": time.Now().UTC()}}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.cardTextActivities(r.Context(), u, c, set)
	}
	s.backToCard(w, r, b, c)
}

// cardDatePage is setReceived/setStart/setDue/setEnd and their unset.
func (s *Server) cardDatePage(w http.ResponseWriter, r *http.Request, u *User) {
	b, c, _, ok := s.checklistTarget(w, r, u, false)
	if !ok {
		return
	}
	var kind *cardDateKind
	for i := range cardDateKinds {
		if cardDateKinds[i].Kind == r.PathValue("kind") {
			kind = &cardDateKinds[i]
		}
	}
	if kind == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if !s.canSetCardDates(r.Context(), b, u, c) {
		http.Error(w, "your role on this board cannot change it", http.StatusForbidden)
		return
	}
	if !s.boardAllows(r.Context(), b.ID, kind.Allows) {
		http.Error(w, "this board does not use this date", http.StatusForbidden)
		return
	}
	at, del, why := datePickerValue(r)
	if why != "" {
		http.Error(w, why, http.StatusBadRequest)
		return
	}
	var before bson.M
	_ = s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": eq(c.ID)}).Decode(&before)
	update := bson.M{"$unset": bson.M{kind.Field: ""}, "$set": bson.M{"modifiedAt": time.Now().UTC()}}
	if !del {
		update = bson.M{"$set": bson.M{kind.Field: *at, "modifiedAt": time.Now().UTC()}}
	}
	if _, err := s.cardsUpdateOne(r.Context(), u.ID, bson.M{"_id": eq(c.ID), "boardId": eq(b.ID)}, update); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Cards.before.update's timing activity (server/lib/timingChangeActivity.js):
	// a date set carries timeValue, a date cleared timeOldValue alone - as a
	// rule's removeDate records it - and clearing a date the card did not
	// have records nothing.
	old := before[kind.Field]
	if !del || (old != nil && old != "") {
		if old == nil {
			old = ""
		}
		act := bson.M{"username": u.Username, "boardId": b.ID, "cardId": c.ID, "cardTitle": c.Title, // inserted, not a filter
			"timeKey": kind.Field, "timeOldValue": old, "listId": c.ListID, "swimlaneId": c.SwimlaneID}
		if !del {
			act["timeValue"] = *at
		}
		s.activity(r.Context(), u.ID, "a-"+kind.Field, act)
	}
	s.backToCard(w, r, b, c)
}

// isTimingClear is server/lib/timingChangeActivity.js isTimingClear: an
// a-<date> activity without a timeValue - a card date cleared.
func isTimingClear(a bson.M) bool {
	switch docString(a, "activityType") {
	case "a-receivedAt", "a-startAt", "a-dueAt", "a-endAt":
		v, ok := a["timeValue"]
		return !ok || v == nil || v == ""
	}
	return false
}
