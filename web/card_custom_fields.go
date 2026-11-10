package web

import (
	"context"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The card details' custom fields, as WeKan's cardFieldSectionCustomFields,
// cardCustomField-* and cardCustomFieldsPopup draw them: for whoever may
// change the card, on a board that allows custom fields
// (canShowCustomFieldsOnCard), the card's fields in their sort order - an
// admin-only one (#3141) only for a board admin, one of another card kind
// (#2012) not at all - each with its editor; and from the section's
// hamburger every field of the board, ticked when it is on this card.
// Writes go through setCardCustomField, the methods' own core.

type cardFieldItem struct {
	ID, Name string
	Selected bool
}

type cardFieldRow struct {
	ID, Name, Type, Title string
	Set                   bool
	Text                  string // the value as it is shown
	Edit                  string // the value as its editor holds it
	Checked               bool
	Date                  dueDate
	Items                 []cardFieldItem
	Editable              bool
	OnCard                bool
}

// cardFieldDefinitions are the board's fields this member may see on a card
// of this kind, in WeKan's order: sort, then those without one, by name.
func (s *Server) cardFieldDefinitions(ctx context.Context, b *Board, u *User, kind string) []bson.M {
	cur, err := s.db.Collection("customFields").Find(ctx, bson.M{"boardIds": b.ID})
	if err != nil {
		return nil
	}
	var defs []bson.M
	_ = cur.All(ctx, &defs)
	admin := b.AdminOf(u.ID)
	out := defs[:0]
	for _, d := range defs {
		if d["adminOnly"] == true && !admin {
			continue
		}
		if kinds, ok := d["cardKinds"].(bson.A); ok && len(kinds) > 0 {
			found := false
			for _, k := range kinds {
				if k == kind {
					found = true
				}
			}
			if !found {
				continue
			}
		}
		out = append(out, d)
	}
	num := func(d bson.M) (float64, bool) {
		switch v := d["sort"].(type) {
		case float64:
			return v, true
		case int32:
			return float64(v), true
		case int64:
			return float64(v), true
		}
		return 0, false
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, aok := num(out[i])
		c, cok := num(out[j])
		if aok && cok && a != c {
			return a < c
		}
		if aok != cok {
			return aok
		}
		return docString(out[i], "name") < docString(out[j], "name")
	})
	return out
}

func dropdownItemsOf(def bson.M) []cardFieldItem {
	var out []cardFieldItem
	if items, ok := nestedField(def, "settings", "dropdownItems").(bson.A); ok {
		for _, it := range items {
			out = append(out, cardFieldItem{ID: docString(asM(it), "_id"), Name: docString(asM(it), "name")})
		}
	}
	return out
}

func asM(v any) bson.M {
	if m, ok := v.(bson.M); ok {
		return m
	}
	if d, ok := v.(bson.D); ok {
		m := bson.M{}
		for _, e := range d {
			m[e.Key] = e.Value
		}
		return m
	}
	return bson.M{}
}

func formatFieldNumber(v any) string {
	switch n := v.(type) {
	case float64:
		return strconv.FormatFloat(n, 'f', -1, 64)
	case int32:
		return strconv.Itoa(int(n))
	case int64:
		return strconv.FormatInt(n, 10)
	case string:
		return n
	}
	return ""
}

// cardCustomFields are the card's fields (customFieldsWD) and, for the
// picker, every field of the board with whether it is on the card.
func (s *Server) cardCustomFields(ctx context.Context, b *Board, u *User, raw bson.M, format string) (onCard, all []cardFieldRow) {
	defs := s.cardFieldDefinitions(ctx, b, u, docString(raw, "cardKind"))
	values := map[string]any{}
	has := map[string]bool{}
	if a, ok := raw["customFields"].(bson.A); ok {
		for _, f := range a {
			id, _ := nestedField(f, "_id").(string)
			has[id] = true
			values[id] = nestedField(f, "value")
		}
	}
	for _, d := range defs {
		id, name, typ := docString(d, "_id"), docString(d, "name"), docString(d, "type")
		row := cardFieldRow{ID: id, Name: name, Type: typ, OnCard: has[id]}
		all = append(all, row)
		if !has[id] {
			continue
		}
		v := values[id]
		row.Set = v != nil && v != ""
		switch typ {
		case "checkbox":
			row.Checked = v == true
		case "number":
			row.Text, row.Edit = formatFieldNumber(v), formatFieldNumber(v)
		case "currency":
			row.Edit = formatFieldNumber(v)
			if row.Set {
				row.Text = strings.TrimSpace(formatFieldNumber(v) + " " + docString(asM(d["settings"]), "currencyCode"))
			}
		case "date":
			row.Date = dueDateOf(v)
			row.Set = row.Date.Set()
			if row.Set {
				row.Text = row.Date.Show(format)
			}
		case "dropdown":
			row.Items = dropdownItemsOf(d)
			for i := range row.Items {
				if row.Items[i].ID == v {
					row.Items[i].Selected, row.Text = true, row.Items[i].Name
				}
			}
		case "dropdownMultiSelect":
			items := dropdownItemsOf(d)
			var names []string
			if ids, ok := v.(bson.A); ok {
				for _, id := range ids {
					for _, it := range items {
						if it.ID == id {
							names = append(names, it.Name)
						}
					}
				}
			}
			row.Text = strings.Join(names, ", ")
		default:
			if str, ok := v.(string); ok {
				row.Text, row.Edit = str, str
			} else if v != nil {
				row.Text = formatFieldNumber(v)
			}
		}
		// The types the card details edit; the others are shown.
		switch typ {
		case "text", "number", "checkbox", "currency", "date", "dropdown":
			row.Editable = true
		}
		row.Title = name
		if row.Set && row.Text != "" {
			row.Title = name + ": " + row.Text
		}
		onCard = append(onCard, row)
	}
	return onCard, all
}

func (s *Server) cardCustomFieldRoutes(m *http.ServeMux) {
	m.HandleFunc("POST /b/{board}/cards/{card}/custom-fields/{field}/toggle", s.page(s.toggleCardCustomFieldPage))
	m.HandleFunc("POST /b/{board}/cards/{card}/custom-fields/{field}", s.page(s.cardCustomFieldValuePage))
	m.HandleFunc("POST /b/{board}/custom-fields", s.page(s.createCustomFieldPage))
}

// createCustomFieldPage is createCustomFieldPopup from the card's picker: a
// field of the board, shown on cards, by whoever may change the board - as
// the REST API's - with WeKan's createCustomField activity; back to the card.
func (s *Server) createCustomFieldPage(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.writableBoard(r.Context(), r.PathValue("board"), u)
	if b == nil {
		http.Error(w, "your role on this board cannot change it", http.StatusForbidden)
		return
	}
	name, typ := strings.TrimSpace(r.FormValue("name")), r.FormValue("type")
	if name == "" || len(name) > 200 || !customFieldTypes[typ] {
		http.Error(w, "a name and a known type are needed", http.StatusBadRequest)
		return
	}
	id := randomID()
	if _, err := s.db.Collection("customFields").InsertOne(r.Context(), bson.M{"_id": id, "name": name, "type": typ, "settings": bson.M{}, // inserted, not a filter
		"showOnCard": true, "automaticallyOnCard": false, "alwaysOnCard": false, "showLabelOnMiniCard": false,
		"showSumAtTopOfList": false, "boardIds": bson.A{b.ID}, "createdAt": time.Now().UTC()}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.activity(r.Context(), u.ID, "createCustomField", bson.M{"boardId": b.ID, "customFieldId": id})
	s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	to := "/b/" + b.ID + "/" + b.Slug
	var c Card
	if card := r.FormValue("card"); validID(card) && s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": eq(card), "boardId": eq(b.ID)}).Decode(&c) == nil {
		to += "/" + c.ID
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// fieldWriteError answers a setCardCustomField refusal as a page does.
func fieldWriteError(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	if me, ok := err.(*methodError); ok {
		switch me.Code {
		case "not-authorized":
			code = http.StatusForbidden
		case "not-found", "custom-field-not-found", "custom-field-not-on-card":
			code = http.StatusNotFound
		}
	}
	http.Error(w, err.Error(), code)
}

// toggleCardCustomFieldPage is cardCustomFieldsPopup's js-select-field.
func (s *Server) toggleCardCustomFieldPage(w http.ResponseWriter, r *http.Request, u *User) {
	b, c, _, ok := s.checklistTarget(w, r, u, false)
	if !ok {
		return
	}
	var raw bson.M
	_ = s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": eq(c.ID)}).Decode(&raw)
	on := true
	if a, ok := raw["customFields"].(bson.A); ok {
		for _, f := range a {
			if nestedField(f, "_id") == r.PathValue("field") {
				on = false
			}
		}
	}
	if _, err := s.setCardCustomField(r, u, c.ID, r.PathValue("field"), "assigned", func(bson.M) (any, error) { return on, nil }); err != nil {
		fieldWriteError(w, err)
		return
	}
	s.backToCard(w, r, b, c)
}

var currencyJunk = regexp.MustCompile(`[^0-9.,+-]`)

// parseFieldValue is the value of a form for a field's type, as WeKan's
// cardCustomField-* editors make it: an empty one clears it.
func parseFieldValue(r *http.Request, def bson.M) (any, error) {
	bad := &methodError{"invalid-custom-field-value", "invalid-custom-field-value"}
	v := r.FormValue("value")
	switch docString(def, "type") {
	case "text":
		if len(v) > 100000 {
			return nil, bad
		}
		if strings.TrimSpace(v) == "" {
			return nil, nil
		}
		return v, nil
	case "number", "currency":
		t := strings.TrimSpace(v)
		if docString(def, "type") == "currency" {
			t = currencyJunk.ReplaceAllString(t, "")
			// 123,45 is 123.45 (the decimal comma); 1,234.5 keeps its point.
			if strings.Contains(t, ",") && !strings.Contains(t, ".") {
				t = strings.Replace(t, ",", ".", 1)
			}
			t = strings.ReplaceAll(t, ",", "")
		}
		if t == "" {
			return nil, nil
		}
		n, err := strconv.ParseFloat(t, 64)
		if err != nil || n != n || n > 1e300 || n < -1e300 {
			return nil, bad
		}
		return n, nil
	case "checkbox":
		return v == "true" || v == "on" || v == "1", nil
	case "date":
		at, del, why := datePickerValue(r)
		if why != "" {
			return nil, bad
		}
		if del {
			return nil, nil
		}
		return *at, nil
	case "dropdown":
		if v == "" {
			return nil, nil
		}
		for _, it := range dropdownItemsOf(def) {
			if it.ID == v {
				return v, nil
			}
		}
		return nil, bad
	}
	return nil, &methodError{"not-authorized", "not editable here"}
}

// cardCustomFieldValuePage is a cardCustomField-* editor's save.
func (s *Server) cardCustomFieldValuePage(w http.ResponseWriter, r *http.Request, u *User) {
	b, c, _, ok := s.checklistTarget(w, r, u, false)
	if !ok {
		return
	}
	if !s.boardAllows(r.Context(), b.ID, "allowsCustomFields") {
		http.Error(w, "this board does not use custom fields", http.StatusForbidden)
		return
	}
	if _, err := s.setCardCustomField(r, u, c.ID, r.PathValue("field"), "value", func(def bson.M) (any, error) { return parseFieldValue(r, def) }); err != nil {
		fieldWriteError(w, err)
		return
	}
	s.backToCard(w, r, b, c)
}
