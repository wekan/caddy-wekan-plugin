package web

import (
	"net/http"
	"slices"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

type shopOption struct {
	ID, Name string
	Selected bool
}

type shopFieldPicker struct {
	Key, Label string
	Types      []string
	Options    []shopOption
}

func (s *Server) shopSettingsPage(w http.ResponseWriter, r *http.Request, u *User) {
	b, err := s.boardFor(r.Context(), r.PathValue("board"), u)
	if err != nil || !(u.IsAdmin || b.AdminOf(u.ID)) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	doc := s.boardDoc(r, b)
	settings := asM(doc["shopSettings"])
	if settings == nil {
		settings = bson.M{}
	}
	pickers := []shopFieldPicker{
		{Key: "priceFieldId", Label: msg("shop-price-field"), Types: []string{"number", "currency", "text"}},
		{Key: "skuFieldId", Label: msg("shop-sku-field"), Types: []string{"number", "text", "stringtemplate"}},
		{Key: "stockFieldId", Label: msg("shop-stock-field"), Types: []string{"number"}},
	}
	for _, def := range s.findDocs(r.Context(), "customFields", bson.M{"boardIds": eq(b.ID)}) {
		id, name, kind := strOf(def["_id"]), strOf(def["name"]), strOf(def["type"])
		if !validID(id) || def["adminOnly"] == true {
			continue
		}
		for i := range pickers {
			if slices.Contains(pickers[i].Types, kind) {
				pickers[i].Options = append(pickers[i].Options, shopOption{id, name, settings[pickers[i].Key] == id})
			}
		}
	}
	lists, err := s.chartLists(r.Context(), b.ID)
	if err != nil {
		http.Error(w, "Could not load lists", http.StatusInternalServerError)
		return
	}
	var listOptions, labelOptions []shopOption
	saleLists := stringsOf(settings["saleListIds"])
	for _, list := range lists {
		if list.ID != settings["ordersListId"] {
			listOptions = append(listOptions, shopOption{list.ID, list.Title, slices.Contains(saleLists, list.ID)})
		}
	}
	categories := stringsOf(settings["categoryLabelIds"])
	for _, label := range b.Labels {
		name := label.Name
		if name == "" {
			name = label.Color
		}
		labelOptions = append(labelOptions, shopOption{label.ID, name, slices.Contains(categories, label.ID)})
	}
	st := boardViewStateOf(doc)
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Invalid Shop settings", http.StatusBadRequest)
			return
		}
		title := strings.TrimSpace(r.PostForm.Get("title"))
		intro := strings.TrimSpace(r.PostForm.Get("intro"))
		currency := strings.ToUpper(strings.TrimSpace(r.PostForm.Get("currency")))
		if utf16Len(title) > 200 || utf16Len(intro) > 5000 || !shopCurrency.MatchString(currency) {
			http.Error(w, "Invalid Shop title, introduction or currency", http.StatusBadRequest)
			return
		}
		set := bson.M{"shopSettings.title": title, "shopSettings.intro": intro,
			"shopSettings.currency": currency, "shopSettings.enabled": r.PostForm.Get("enabled") == "1"}
		valid := func(options []shopOption, id string) bool {
			return slices.ContainsFunc(options, func(option shopOption) bool { return option.ID == id })
		}
		for _, picker := range pickers {
			id := r.PostForm.Get(picker.Key)
			if id != "" && !valid(picker.Options, id) {
				http.Error(w, "Invalid Shop custom field", http.StatusBadRequest)
				return
			}
			set["shopSettings."+picker.Key] = id
		}
		for key, options := range map[string][]shopOption{"saleListIds": listOptions, "categoryLabelIds": labelOptions} {
			ids := make([]string, 0)
			for _, id := range r.PostForm[key] {
				if !valid(options, id) {
					http.Error(w, "Invalid Shop list or category", http.StatusBadRequest)
					return
				}
				if !slices.Contains(ids, id) {
					ids = append(ids, id)
				}
			}
			set["shopSettings."+key] = ids
		}
		filter := bson.M{"_id": eq(b.ID)}
		if r.PostForm.Get("publicDefault") == "1" {
			set["defaultPublicBoardView"] = "board-view-shop"
			set["boardViewSettings.board-view-shop.showOnPublic"] = true
		} else if st.defaultView("public") == "board-view-shop" {
			filter["defaultPublicBoardView"] = "board-view-shop"
			set["defaultPublicBoardView"] = "board-view-swimlanes"
			set["boardViewSettings.board-view-swimlanes.showOnPublic"] = true
		}
		result, err := s.db.Collection("boards").UpdateOne(r.Context(), filter, bson.M{"$set": set})
		if err != nil {
			http.Error(w, "Could not save Shop settings", http.StatusInternalServerError)
			return
		}
		if result.MatchedCount != 1 {
			http.Error(w, "Board settings changed", http.StatusConflict)
			return
		}
		s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
		http.Redirect(w, r, shopLocation(b, "", ""), http.StatusSeeOther)
		return
	}
	currency := strOf(settings["currency"])
	if currency == "" {
		currency = "EUR"
	}
	s.render(w, r, "shop_settings", http.StatusOK, map[string]any{
		"Title": b.Title, "Board": b, "User": u, "Settings": settings, "Currency": currency,
		"Pickers": pickers, "Lists": listOptions, "Labels": labelOptions,
		"PublicDefault": st.defaultView("public") == "board-view-shop", "Shop": shopLocation(b, "", ""),
	})
}
