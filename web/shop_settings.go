package web

import (
	"math"
	"net/http"
	"net/url"
	"slices"
	"sort"
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

type shopOrderList struct {
	shopOption
	BoardID string
}

func (s *Server) shopOrderLists(r *http.Request, source *Board, u *User, settings bson.M) ([]shopOrderList, error) {
	ctx := r.Context()
	filter := bson.M{"permission": "private", "archived": bson.M{"$ne": true}, "deletedAt": nil}
	if !u.IsAdmin {
		filter["members.userId"] = eq(u.ID)
	}
	cursor, err := s.db.Collection("boards").Find(ctx, filter)
	if err != nil {
		return nil, err
	}
	var boards []Board
	if err := cursor.All(ctx, &boards); err != nil {
		return nil, err
	}
	orderBoard := strOf(settings["ordersBoardId"])
	if orderBoard == "" {
		orderBoard = source.ID
	}
	var out []shopOrderList
	for _, board := range boards {
		if !u.IsAdmin && !board.AdminOf(u.ID) {
			continue
		}
		lists, err := s.chartLists(ctx, board.ID)
		if err != nil {
			return nil, err
		}
		for _, list := range lists {
			out = append(out, shopOrderList{
				shopOption: shopOption{list.ID, board.Title + " / " + list.Title, orderBoard == board.ID && settings["ordersListId"] == list.ID},
				BoardID:    board.ID,
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func shopTermsURL(terms string) string {
	link, err := url.ParseRequestURI(terms)
	if err != nil || link.Hostname() == "" || link.User != nil ||
		(link.Scheme != "https" && link.Scheme != "http") || utf16Len(terms) > 2000 {
		return ""
	}
	return terms
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
	orderLists, err := s.shopOrderLists(r, b, u, settings)
	if err != nil {
		http.Error(w, "Could not load private order lists", http.StatusInternalServerError)
		return
	}
	linkIndex, linkMethod := shopMethod(shopPaymentMethods(settings), "link")
	var manual []map[string]string
	for _, option := range shopManualMethods {
		_, stored := shopMethod(shopPaymentMethods(settings), option.Kind)
		manual = append(manual, map[string]string{"Label": option.Label, "Field": option.Field,
			"Name": strOf(stored["name"]), "OtherLabel": strOf(stored["label"]), "Details": strOf(stored["details"])})
	}
	_, paypal := shopMethod(shopPaymentMethods(settings), "paypal")
	reference := strOf(settings["referenceType"])
	if reference == "" {
		reference = "rf"
	}
	var references []shopOption
	for _, option := range []struct{ ID, Label string }{
		{"rf", msg("billing-reference-rf")}, {"fi", msg("billing-reference-fi")}, {"be", msg("billing-reference-be")},
		{"no", msg("billing-reference-no")}, {"se", msg("billing-reference-se")}, {"none", msg("billing-reference-none")},
	} {
		references = append(references, shopOption{option.ID, option.Label, option.ID == reference})
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
		if r.PostForm.Has("ordersListId") {
			orderList, orderBoard := r.PostForm.Get("ordersListId"), ""
			if orderList != "" {
				for _, option := range orderLists {
					if option.ID == orderList {
						orderBoard = option.BoardID
						break
					}
				}
				if orderBoard == "" {
					http.Error(w, "Invalid private Shop order list", http.StatusBadRequest)
					return
				}
			}
			if orderBoard == b.ID && slices.Contains(set["shopSettings.saleListIds"].([]string), orderList) {
				http.Error(w, "The order list cannot also be on sale", http.StatusBadRequest)
				return
			}
			terms := strings.TrimSpace(r.PostForm.Get("termsUrl"))
			if terms != "" && shopTermsURL(terms) == "" {
				http.Error(w, "Invalid Shop terms URL", http.StatusBadRequest)
				return
			}
			set["shopSettings.ordersListId"], set["shopSettings.ordersBoardId"] = orderList, orderBoard
			set["shopSettings.termsUrl"] = terms
			set["shopSettings.guestCheckout"] = r.PostForm.Get("guestCheckout") == "1"
			set["shopSettings.decrementStock"] = r.PostForm.Get("decrementStock") == "1"
			set["shopSettings.confirmationEmail"] = r.PostForm.Get("confirmationEmail") == "1"
		}
		if r.PostForm.Has("vatPercent") {
			rate, ok := shopNumber(r.PostForm.Get("vatPercent"))
			if !ok || rate < 0 || rate > 100 {
				http.Error(w, "Invalid Shop VAT rate", http.StatusBadRequest)
				return
			}
			set["shopSettings.vatPercent"] = math.Round(rate*100) / 100
			set["shopSettings.pricesIncludeVat"] = r.PostForm.Get("pricesIncludeVat") == "1"
		}
		if r.PostForm.Has("paymentLink") {
			link := strings.TrimSpace(r.PostForm.Get("paymentLink"))
			if link != "" && shopPaymentURL(link, "1", "EUR", "order") == "" {
				http.Error(w, "Invalid Shop payment link", http.StatusBadRequest)
				return
			}
			methods := slices.Clone(shopPaymentMethods(settings))
			if linkIndex >= 0 {
				if link == "" {
					methods = slices.Delete(methods, linkIndex, linkIndex+1)
				} else {
					entry := asM(methods[linkIndex])
					entry["method"] = bson.M{"type": "link", "url": link, "label": strOf(linkMethod["label"])}
					methods[linkIndex] = entry
				}
			} else if link != "" {
				if len(methods) >= 20 {
					http.Error(w, "Too many Shop payment methods", http.StatusBadRequest)
					return
				}
				methods = append(methods, bson.M{"id": randomID(), "enabled": true,
					"method": bson.M{"type": "link", "url": link}})
			}
			set["shopSettings.paymentMethods"] = methods
		}
		for _, option := range shopManualMethods {
			if !r.PostForm.Has(option.Field) {
				continue
			}
			name, validName := "", true
			if option.Kind == "bank-transfer" {
				name, validName = shopContactField(r.PostForm.Get("bankName"), 200, false)
			}
			label, validLabel := "", true
			if option.Kind == "other" {
				label, validLabel = shopContactField(r.PostForm.Get("otherLabel"), 200, false)
			}
			details, validDetails := shopContactField(r.PostForm.Get(option.Field), 2000, true)
			if !validName || !validLabel || !validDetails ||
				(option.Kind == "bank-transfer" && (name == "") != (details == "")) ||
				(option.Kind == "other" && (label == "") != (details == "")) {
				http.Error(w, "Invalid Shop payment instructions", http.StatusBadRequest)
				return
			}
			methods, updated := set["shopSettings.paymentMethods"].(bson.A)
			if !updated {
				methods = slices.Clone(shopPaymentMethods(settings))
			}
			index, _ := shopMethod(methods, option.Kind)
			method := bson.M{"type": option.Kind, "details": details}
			if option.Kind == "bank-transfer" {
				method["name"] = name
			}
			if option.Kind == "other" {
				method["label"] = label
			}
			switch {
			case details == "" && index >= 0:
				methods = slices.Delete(methods, index, index+1)
			case details != "" && index >= 0:
				entry := asM(methods[index])
				entry["method"] = method
				methods[index] = entry
			case details != "":
				if len(methods) >= 20 {
					http.Error(w, "Too many Shop payment methods", http.StatusBadRequest)
					return
				}
				methods = append(methods, bson.M{"id": randomID(), "enabled": true,
					"method": method})
			}
			set["shopSettings.paymentMethods"] = methods
		}
		if r.PostForm.Has("paypalUser") {
			name := strings.TrimSpace(r.PostForm.Get("paypalUser"))
			if name != "" && !shopPayPalName.MatchString(name) {
				http.Error(w, "Invalid PayPal.Me name", http.StatusBadRequest)
				return
			}
			methods, updated := set["shopSettings.paymentMethods"].(bson.A)
			if !updated {
				methods = slices.Clone(shopPaymentMethods(settings))
			}
			index, _ := shopMethod(methods, "paypal")
			switch {
			case name == "" && index >= 0:
				methods = slices.Delete(methods, index, index+1)
			case name != "" && index >= 0:
				entry := asM(methods[index])
				entry["method"] = bson.M{"type": "paypal", "user": name}
				methods[index] = entry
			case name != "":
				if len(methods) >= 20 {
					http.Error(w, "Too many Shop payment methods", http.StatusBadRequest)
					return
				}
				methods = append(methods, bson.M{"id": randomID(), "enabled": true,
					"method": bson.M{"type": "paypal", "user": name}})
			}
			set["shopSettings.paymentMethods"] = methods
		}
		if r.PostForm.Has("referenceType") {
			id := r.PostForm.Get("referenceType")
			if !valid(references, id) {
				http.Error(w, "Invalid Shop reference type", http.StatusBadRequest)
				return
			}
			set["shopSettings.referenceType"] = id
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
		"Pickers": pickers, "Lists": listOptions, "Labels": labelOptions, "OrderLists": orderLists,
		"ConfirmationEmail": settings["confirmationEmail"] != false,
		"ManualMethods":     manual,
		"PayPalUser":        strOf(paypal["user"]),
		"References":        references, "PaymentLink": strOf(linkMethod["url"]),
		"DecrementStock":   settings["decrementStock"] != false,
		"PricesIncludeVAT": settings["pricesIncludeVat"] != false, "VATPercent": float64(shopVATRate(settings)) / 100,
		"PublicDefault": st.defaultView("public") == "board-view-shop", "Shop": shopLocation(b, "", ""),
	})
}
