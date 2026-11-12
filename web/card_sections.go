package web

import (
	"reflect"
	"slices"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// WeKan's one section model (cardDetails.jade, 2026-10-28): the opened card
// draws Card Settings' sections in the board's cardSettingsSectionOrder, the
// order Card Settings' Card pane shows and drags, each headed with that
// section's icon and title and its fields in the order of its rows there
// (cardFieldOrder) - only the sections this card has something for, and
// none Card Settings hides on the card.

// cardSettingsOrderField is where a side keeps its sections' order
// (cardSettingsSections.js SECTION_ORDER_FIELD).
var cardSettingsOrderField = map[string]string{"card": "cardSettingsSectionOrder", "minicard": "minicardSettingsSectionOrder"}

// sectionOrderOf is WeKan's sectionOrder(stored, side): the stored ids that
// are known, once each, then the rest in the side's default order.
func sectionOrderOf(stored any, side string) []string {
	known := cardSettings.Order[side]
	var out []string
	if list, ok := plain(stored).([]any); ok {
		for _, v := range list {
			if id, ok := v.(string); ok && slices.Contains(known, id) && !slices.Contains(out, id) {
				out = append(out, id)
			}
		}
	}
	for _, id := range known {
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// rowsForSide is WeKan's (cardSettingsRows.js): the rows of one side in
// order - a row with a place in it at its place, a row that modifies another
// (`after`) under it, anything left last.
func rowsForSide(side string, order []string) []*cardSettingsRow {
	var with []*cardSettingsRow
	for i := range cardSettings.Rows {
		if cardSettings.Rows[i].side(side) != nil {
			with = append(with, &cardSettings.Rows[i])
		}
	}
	var placed []*cardSettingsRow
	for _, k := range order {
		for _, r := range with {
			if r.Key == k && !slices.Contains(placed, r) {
				placed = append(placed, r)
			}
		}
	}
	var attach func(anchor string)
	attach = func(anchor string) {
		for _, r := range with {
			if r.side(side).After != anchor || slices.Contains(placed, r) {
				continue
			}
			at := slices.IndexFunc(placed, func(p *cardSettingsRow) bool { return p.Key == anchor })
			if at < 0 {
				placed = append(placed, r)
			} else {
				placed = slices.Insert(placed, at+1, r)
			}
			attach(r.Key)
		}
	}
	for _, k := range order {
		attach(k)
	}
	for _, r := range with {
		if !slices.Contains(placed, r) {
			placed = append(placed, r)
		}
	}
	return placed
}

// sideLayout is the side's layout.
func sideLayout(side string) *fieldLayout {
	if side == "minicard" {
		return &minicardLayout
	}
	return &cardLayout
}

// sideFieldOrder is a side's flat field order: its stored cardFieldOrder or
// minicardFieldOrder, canonical in its layout.
func sideFieldOrder(board bson.M, side string) []string {
	field := "cardFieldOrder"
	if side == "minicard" {
		field = "minicardFieldOrder"
	}
	var stored []string
	if list, ok := plain(board[field]).([]any); ok {
		for _, v := range list {
			if s, ok := v.(string); ok {
				stored = append(stored, s)
			}
		}
	}
	return sideLayout(side).apply(stored)
}

type cardSectionView struct {
	ID, Icon, Title string
	// Header: the section is headed here; the ones whose own heading folds
	// them (custom fields, vote, poker, attachments) keep theirs.
	Header bool
}

// present says whether a page datum holds something.
func present(v any) bool {
	if v == nil {
		return false
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		return !rv.IsNil()
	case reflect.Slice, reflect.Map, reflect.String:
		return rv.Len() > 0
	case reflect.Bool:
		return rv.Bool()
	}
	return true
}

// cardSectionsOf are the opened card's sections, from the page's data.
func cardSectionsOf(data map[string]any, board bson.M, lang string) []cardSectionView {
	has := map[string]bool{
		"labels": true, "members": true, "card": true, "subtasks": true, "attachments": true, "comments": true,
		"date":         present(data["CardDates"]),
		"customFields": present(data["CardFieldsShown"]),
		"vote":         present(data["Vote"]),
		"poker":        present(data["Poker"]),
		"description":  present(data["DescriptionShown"]),
		"scrum":        present(data["CardScrum"]),
		"checklists":   present(data["Checklists"]) || present(data["CanModifyCard"]),
	}
	ownHeading := map[string]bool{"customFields": true, "vote": true, "poker": true, "attachments": true}
	hidden, _ := plain(board[cardSettingsHiddenField["card"]]).([]any)
	var out []cardSectionView
	for _, id := range sectionOrderOf(board[cardSettingsOrderField["card"]], "card") {
		if !has[id] || slices.Contains(hidden, any(id)) {
			continue
		}
		for _, sec := range cardSettings.Sections {
			if sec.ID == id {
				out = append(out, cardSectionView{ID: id, Icon: sec.Icon, Title: tr(lang, sec.LabelKey), Header: !ownHeading[id]})
			}
		}
	}
	return out
}

// dateFieldKeys are the card dates' keys in cardFieldOrder.
var dateFieldKeys = map[string]string{"received": "receivedDate", "start": "startDate", "due": "dueDate", "end": "endDate"}

// orderCardDates puts the card's dates in the board's row order.
func orderCardDates(rows []cardDateRow, board bson.M) []cardDateRow {
	order := sideFieldOrder(board, "card")
	slices.SortStableFunc(rows, func(a, b cardDateRow) int {
		return slices.Index(order, dateFieldKeys[a.Kind]) - slices.Index(order, dateFieldKeys[b.Kind])
	})
	return rows
}
