package web

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// A board's order of the card details' sections, as Node.js WeKan
// (models/lib/cardFieldOrder.js, server/models/boards.js cardFieldOrder).
func (s *Server) cardFieldOrderRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/boards/{board}/cardFieldOrder", s.api(s.apiCardFieldOrder))
	m.HandleFunc("PUT /api/boards/{board}/cardFieldOrder", s.api(s.apiSetCardFieldOrder))
}

type cardSection struct {
	Key         string
	Fields      []string
	PinnedFirst bool
}

// cardLayout is WeKan's CARD_LAYOUT.
var cardLayout = struct {
	Head, Tail []string
	Sections   []cardSection
	Legacy     map[string][]string
	Aliases    map[string][]string
}{
	Head: []string{"dueComplete", "cardNumber", "cover"},
	Sections: []cardSection{
		{Key: "labels", Fields: []string{"labels", "stickers", "location"}},
		{Key: "date", Fields: []string{"receivedDate", "startDate", "dueDate", "endDate"}},
		{Key: "members", Fields: []string{"members", "assignee", "creator", "requestedBy", "assignedBy"}},
		{Key: "card", Fields: []string{"dependencies", "cardSortingByNumber", "showLists", "cardButtons", "similarCards"}},
		{Key: "time", Fields: []string{"spentTime", "flowtime", "pomodoro"}},
		{Key: "customFields", Fields: []string{"customFields"}},
		{Key: "vote", Fields: []string{"vote"}},
		{Key: "poker", Fields: []string{"poker"}},
		{Key: "description", Fields: []string{"descriptionTitle", "descriptionText"}, PinnedFirst: true},
		{Key: "scrum", Fields: []string{"scrumSprint", "scrumPastSprints", "scrumRelease", "scrumIssueType", "scrumAcceptanceCriteria", "scrumBacklogRank"}},
		{Key: "checklists", Fields: []string{"checklistCount", "checklists"}},
		{Key: "subtasks", Fields: []string{"subtasks"}},
		{Key: "attachments", Fields: []string{"attachments", "textNotes"}},
		{Key: "comments", Fields: []string{"comments"}},
	},
	Tail: []string{"attachmentCount", "activities"},
	Legacy: map[string][]string{
		"labels":       {"labels", "stickers", "location"},
		"dates":        {"receivedDate", "startDate", "dueDate", "endDate"},
		"members":      {"members", "assignee", "creator", "requestedBy", "assignedBy", "dependencies", "cardSortingByNumber", "showLists", "spentTime", "flowtime", "pomodoro"},
		"customFields": {"customFields", "vote", "poker"},
		"description":  {"descriptionTitle", "descriptionText"},
	},
	Aliases: map[string][]string{
		"dates":        {"receivedDate", "startDate", "dueDate", "endDate"},
		"sort":         {"cardSortingByNumber", "showLists", "spentTime", "flowtime", "pomodoro"},
		"voteAndPoker": {"vote", "poker"},
	},
}

func cardSectionOf(field string) *cardSection {
	for i, s := range cardLayout.Sections {
		if slices.Contains(s.Fields, field) {
			return &cardLayout.Sections[i]
		}
	}
	return nil
}

// cardFieldOrderKeys is WeKan's CARD_FIELD_ORDER_KEYS: the sections' keys.
func cardFieldOrderKeys() []string {
	out := []string{}
	for _, s := range cardLayout.Sections {
		out = append(out, s.Key)
	}
	return out
}

// applyCardLayout is WeKan's applyLayoutOrder: the canonical flat order of a
// stored value - field keys, section keys, legacy or aliased keys; unknown keys
// dropped, no field lost or doubled.
func applyCardLayout(stored []string) []string {
	all := map[string]bool{}
	for _, f := range cardLayout.Head {
		all[f] = true
	}
	for _, s := range cardLayout.Sections {
		for _, f := range s.Fields {
			all[f] = true
		}
	}
	for _, f := range cardLayout.Tail {
		all[f] = true
	}
	legacy := len(stored) > 0
	someNotField := false
	for _, k := range stored {
		if _, ok := cardLayout.Legacy[k]; !ok {
			legacy = false
		}
		if !all[k] {
			someNotField = true
		}
	}
	legacy = legacy && someNotField
	var fields []string
	for _, k := range stored {
		switch {
		case legacy:
			for _, f := range cardLayout.Legacy[k] {
				if all[f] {
					fields = append(fields, f)
				}
			}
		case all[k]:
			fields = append(fields, k)
		default:
			found := false
			for _, s := range cardLayout.Sections {
				if s.Key == k {
					fields, found = append(fields, s.Fields...), true
				}
			}
			if !found {
				for _, f := range cardLayout.Aliases[k] {
					if all[f] {
						fields = append(fields, f)
					}
				}
			}
		}
	}
	var keys []string
	for _, f := range fields {
		if s := cardSectionOf(f); s != nil && !slices.Contains(keys, s.Key) {
			keys = append(keys, s.Key)
		}
	}
	for _, s := range cardLayout.Sections {
		if !slices.Contains(keys, s.Key) {
			keys = append(keys, s.Key)
		}
	}
	out := append([]string{}, cardLayout.Head...)
	for _, k := range keys {
		var s cardSection
		for _, x := range cardLayout.Sections {
			if x.Key == k {
				s = x
			}
		}
		var in []string
		for _, f := range fields {
			if slices.Contains(s.Fields, f) && !slices.Contains(in, f) {
				in = append(in, f)
			}
		}
		for _, f := range s.Fields {
			if !slices.Contains(in, f) {
				in = append(in, f)
			}
		}
		if s.PinnedFirst {
			first := s.Fields[0]
			in = append([]string{first}, slices.DeleteFunc(in, func(x string) bool { return x == first })...)
		}
		out = append(out, in...)
	}
	return append(out, cardLayout.Tail...)
}

// applyCardFieldOrder is WeKan's: the sections in the order the stored value gives.
func applyCardFieldOrder(stored []string) []string {
	var keys []string
	for _, f := range applyCardLayout(stored) {
		if s := cardSectionOf(f); s != nil && !slices.Contains(keys, s.Key) {
			keys = append(keys, s.Key)
		}
	}
	return keys
}

func (s *Server) storedCardFieldOrder(r *http.Request, b *Board) []string {
	var doc struct {
		Order []string `bson:"cardFieldOrder"`
	}
	_ = s.db.Collection("boards").FindOne(r.Context(), bson.M{"_id": b.ID}).Decode(&doc)
	return doc.Order
}

func (s *Server) apiCardFieldOrder(w http.ResponseWriter, r *http.Request, u *User) {
	if b := s.apiBoardFor(w, r, u, false); b != nil {
		writeJSON(w, http.StatusOK, map[string]any{"cardFieldOrder": applyCardFieldOrder(s.storedCardFieldOrder(r, b)), "keys": cardFieldOrderKeys()})
	}
}

// apiSetCardFieldOrder: a board admin sets the order - a list, its JSON text, or
// comma-separated keys, as WeKan takes it.
func (s *Server) apiSetCardFieldOrder(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardAdmin(w, r, u)
	if b == nil {
		return
	}
	var req map[string]any
	_ = decodeJSON(r, &req)
	var order []string
	switch v := req["cardFieldOrder"].(type) {
	case []any:
		for _, e := range v {
			if k, ok := e.(string); ok {
				order = append(order, k)
			}
		}
	case string:
		if json.Unmarshal([]byte(v), &order) != nil {
			order = nil
			for _, k := range strings.Split(v, ",") {
				order = append(order, strings.TrimSpace(k))
			}
		}
	default:
		apiError(w, http.StatusBadRequest, "cardFieldOrder must be an array of section keys")
		return
	}
	if len(order) > 200 {
		apiError(w, http.StatusBadRequest, "cardFieldOrder must be an array of section keys")
		return
	}
	stored := applyCardFieldOrder(order)
	if _, err := s.db.Collection("boards").UpdateOne(r.Context(), bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"cardFieldOrder": stored}}); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.hub.publish(b.ID, "board-changed")
	writeJSON(w, http.StatusOK, map[string]any{"cardFieldOrder": stored, "keys": cardFieldOrderKeys()})
}
