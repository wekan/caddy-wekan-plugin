package web

import (
	"net/http"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Editing a custom field and its dropdown items, as Node.js WeKan
// (server/models/customFields.js): write access to the board, and only a field
// of that board.
func (s *Server) apiFieldRoutes(m *http.ServeMux) {
	field := "/api/boards/{board}/custom-fields/{field}"
	m.HandleFunc("PUT "+field, s.api(s.apiEditCustomField))
	m.HandleFunc("POST "+field+"/dropdown-items", s.api(s.apiAddDropdownItems))
	m.HandleFunc("PUT "+field+"/dropdown-items/{item}", s.api(s.apiEditDropdownItem))
	m.HandleFunc("DELETE "+field+"/dropdown-items/{item}", s.api(s.apiDeleteDropdownItem))
}

// fieldOf is the board's custom field of the request, or 404.
func (s *Server) fieldOf(w http.ResponseWriter, r *http.Request, b *Board) bson.M {
	id := r.PathValue("field")
	var f bson.M
	if !validID(id) || s.db.Collection("customFields").FindOne(r.Context(), bson.M{"_id": eq(id), "boardIds": b.ID}).Decode(&f) != nil {
		apiError(w, http.StatusNotFound, "Custom field not found")
		return nil
	}
	return f
}

var customFieldFlags = []string{"showOnCard", "automaticallyOnCard", "alwaysOnCard", "showLabelOnMiniCard", "showSumAtTopOfList"}

func (s *Server) apiEditCustomField(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, true)
	if b == nil {
		return
	}
	f := s.fieldOf(w, r, b)
	if f == nil {
		return
	}
	var req map[string]any
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	set := bson.M{}
	if v, ok := req["name"]; ok {
		name, _ := v.(string)
		if name = strings.TrimSpace(name); name == "" || len(name) > 200 {
			apiError(w, http.StatusBadRequest, "a custom field needs a name")
			return
		}
		set["name"] = name
	}
	if v, ok := req["type"]; ok {
		typ, _ := v.(string)
		if !customFieldTypes[typ] {
			apiError(w, http.StatusBadRequest, "one of WeKan's custom field types")
			return
		}
		set["type"] = typ
	}
	if v, ok := req["settings"]; ok {
		settings, isMap := v.(map[string]any)
		if !isMap {
			apiError(w, http.StatusBadRequest, "settings is an object")
			return
		}
		set["settings"] = cleanImported(settings, "")
	}
	for _, k := range customFieldFlags {
		if v, ok := req[k]; ok {
			set[k] = isTrue(v)
		}
	}
	if len(set) > 0 {
		if _, err := s.db.Collection("customFields").UpdateOne(r.Context(), bson.M{"_id": f["_id"]}, bson.M{"$set": set}); err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.hub.publish(b.ID, "board-changed")
	}
	writeJSON(w, http.StatusOK, map[string]any{"_id": f["_id"]})
}

func dropdownName(v any) (string, bool) {
	name, ok := v.(string)
	return name, ok && len(name) <= 1000
}

func (s *Server) apiAddDropdownItems(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, true)
	if b == nil {
		return
	}
	f := s.fieldOf(w, r, b)
	if f == nil {
		return
	}
	var req struct {
		Items []any `json:"items"`
	}
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	items := bson.A{}
	for _, v := range req.Items {
		if name, ok := dropdownName(v); ok { // WeKan keeps the names, skips anything else
			items = append(items, bson.M{"_id": randomString(idAlphabet, 6), "name": name}) // inserted, not a filter
		}
	}
	if len(items) > 0 {
		if _, err := s.db.Collection("customFields").UpdateOne(r.Context(), bson.M{"_id": f["_id"]},
			bson.M{"$push": bson.M{"settings.dropdownItems": bson.M{"$each": items}}}); err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.hub.publish(b.ID, "board-changed")
	}
	writeJSON(w, http.StatusOK, map[string]any{"_id": f["_id"]})
}

func (s *Server) apiEditDropdownItem(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, true)
	if b == nil {
		return
	}
	f := s.fieldOf(w, r, b)
	if f == nil {
		return
	}
	var req map[string]any
	if err := decodeJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	item := r.PathValue("item")
	if v, ok := req["name"]; ok && validID(item) {
		name, ok := dropdownName(v)
		if !ok {
			apiError(w, http.StatusBadRequest, "a dropdown item's name is text")
			return
		}
		// The list as a whole, not the positional $ operator, which FerretDB has not.
		settings, _ := f["settings"].(bson.D)
		items, _ := docMap(settings)["dropdownItems"].(bson.A)
		changed := false
		for i, e := range items {
			d, _ := e.(bson.D)
			if docMap(d)["_id"] == item {
				items[i], changed = bson.M{"_id": item, "name": name}, true // inserted, not a filter
			}
		}
		if changed {
			if _, err := s.db.Collection("customFields").UpdateOne(r.Context(), bson.M{"_id": f["_id"]},
				bson.M{"$set": bson.M{"settings.dropdownItems": items}}); err != nil {
				apiError(w, http.StatusInternalServerError, err.Error())
				return
			}
			s.hub.publish(b.ID, "board-changed")
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"_id": item})
}

func (s *Server) apiDeleteDropdownItem(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, true)
	if b == nil {
		return
	}
	f := s.fieldOf(w, r, b)
	if f == nil {
		return
	}
	if item := r.PathValue("item"); validID(item) {
		if _, err := s.db.Collection("customFields").UpdateOne(r.Context(), bson.M{"_id": f["_id"]},
			bson.M{"$pull": bson.M{"settings.dropdownItems": bson.M{"_id": eq(item)}}}); err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.hub.publish(b.ID, "board-changed")
	}
	writeJSON(w, http.StatusOK, map[string]any{"_id": f["_id"]})
}
