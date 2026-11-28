package web

import (
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

var shopDecimal = regexp.MustCompile(`^[0-9]{1,10}([.,][0-9]{1,8})?$`)
var shopCurrency = regexp.MustCompile(`^[A-Z]{3}$`)

type shopProduct struct {
	Card
	Price, SKU, URL, Image string
	SoldOut                bool
	listSort               float64
}

func shopLocation(b *Board, category, product string) string {
	params := url.Values{"view": {"shop"}}
	if category != "" {
		params.Set("category", category)
	}
	if product != "" {
		params.Set("product", product)
	}
	return "/b/" + b.ID + "/" + b.Slug + "?" + params.Encode()
}

func shopNumber(value any) (float64, bool) {
	n, ok := toFloat(value)
	if text, isText := value.(string); isText {
		text = strings.TrimSpace(text)
		if !shopDecimal.MatchString(text) {
			return 0, false
		}
		var err error
		n, err = strconv.ParseFloat(strings.ReplaceAll(text, ",", "."), 64)
		ok = err == nil
	}
	return n, ok && !math.IsNaN(n) && !math.IsInf(n, 0)
}

func shopPrice(value any, currency string) string {
	price, ok := shopNumber(value)
	if !ok || price < 0 || price > 1e9 {
		return ""
	}
	digits := 2
	switch currency {
	case "BIF", "CLP", "DJF", "GNF", "ISK", "JPY", "KMF", "KRW", "PYG", "RWF", "UGX", "UYI", "VND", "VUV", "XAF", "XOF", "XPF":
		digits = 0
	case "BHD", "IQD", "JOD", "KWD", "LYD", "OMR", "TND":
		digits = 3
	case "CLF", "UYW":
		digits = 4
	case "BTC", "LTC", "ETH":
		digits = 8
	}
	factor := math.Pow10(digits)
	minor := math.Round(price * factor)
	if minor > 9007199254740991 {
		return ""
	}
	amount := strconv.FormatInt(int64(minor), 10)
	if digits > 0 {
		if padding := digits + 1 - len(amount); padding > 0 {
			amount = strings.Repeat("0", padding) + amount
		}
		at := len(amount) - digits
		amount = amount[:at] + "." + amount[at:]
	}
	return amount + " " + currency
}

func (s *Server) boardShop(w http.ResponseWriter, r *http.Request, b *Board, u *User, lanes []lane) {
	ctx := r.Context()
	lang := languageOf(r, u).Tag
	var raw bson.M
	_ = s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(b.ID)},
		options.FindOne().SetProjection(bson.M{"shopSettings": 1})).Decode(&raw)
	settings := asM(raw["shopSettings"])
	title := strOf(settings["title"])
	if title == "" {
		title = b.Title
	}
	data := map[string]any{"Title": title, "Board": b, "User": u, "Home": shopLocation(b, "", ""),
		"Views": s.boardViewEntries(b, lang, "shop"), "Enabled": settings["enabled"] == true}
	if settings["enabled"] != true {
		s.render(w, r, "shop", http.StatusOK, data)
		return
	}
	policy := s.textPolicy(ctx)
	data["Intro"] = viewer(strOf(settings["intro"]), policy)
	currency := strings.ToUpper(strOf(settings["currency"]))
	if !shopCurrency.MatchString(currency) {
		currency = "EUR"
	}
	allowedFields := make(map[string]bool)
	for _, def := range s.findDocs(ctx, "customFields", bson.M{"boardIds": eq(b.ID)}) {
		if def["adminOnly"] != true {
			allowedFields[strOf(def["_id"])] = true
		}
	}
	priceID, skuID, stockID := strOf(settings["priceFieldId"]), strOf(settings["skuFieldId"]), strOf(settings["stockFieldId"])
	if !allowedFields[priceID] {
		s.render(w, r, "shop", http.StatusOK, data)
		return
	}
	rawCards := make(map[string]map[string]any)
	for _, doc := range s.findDocs(ctx, "cards", bson.M{"boardId": eq(b.ID), "archived": false, "deletedAt": nil},
		options.Find().SetProjection(bson.M{"customFields": 1})) {
		rawCards[strOf(doc["_id"])] = doc
	}
	saleLists := stringsOf(settings["saleListIds"])
	orders := strOf(settings["ordersListId"])
	category := r.URL.Query().Get("category")
	var products []shopProduct
	usedLabels := make(map[string]bool)
	for _, lane := range lanes {
		for _, list := range lane.Lists {
			if list.ID == orders || (len(saleLists) > 0 && !slices.Contains(saleLists, list.ID)) {
				continue
			}
			for _, card := range list.Cards {
				if (card.Type != "" && card.Type != "cardType-card") ||
					(u != nil && assignedOnlyOn(b, u) && !slices.Contains(card.Assignees, u.ID)) {
					continue
				}
				fields := make(map[string]any)
				for _, field := range fieldValues(rawCards[card.ID]) {
					id := strOf(field["_id"])
					if allowedFields[id] {
						fields[id] = field["value"]
					}
				}
				price := shopPrice(fields[priceID], currency)
				if price == "" {
					continue
				}
				stock, tracked := shopNumber(fields[stockID])
				sku := jsSlice16(strOf(fields[skuID]), 100)
				if n, ok := toFloat(fields[skuID]); ok {
					sku = strconv.FormatFloat(n, 'f', -1, 64)
				}
				products = append(products, shopProduct{Card: card, Price: price, SKU: sku,
					SoldOut: tracked && stock < 1, listSort: list.Sort})
				for _, id := range card.LabelIDs {
					usedLabels[id] = true
				}
			}
		}
	}
	sort.SliceStable(products, func(i, j int) bool {
		a, b := products[i], products[j]
		if a.listSort != b.listSort {
			return a.listSort < b.listSort
		}
		if a.Sort != b.Sort {
			return a.Sort < b.Sort
		}
		return a.Title < b.Title
	})
	var categories []map[string]string
	ticked := stringsOf(settings["categoryLabelIds"])
	validCategory := category == ""
	for _, label := range b.Labels {
		if usedLabels[label.ID] && (len(ticked) == 0 || slices.Contains(ticked, label.ID)) {
			name := label.Name
			if name == "" {
				name = label.Color
			}
			categories = append(categories, map[string]string{"Name": name, "URL": shopLocation(b, label.ID, "")})
			validCategory = validCategory || category == label.ID
		}
	}
	if !validCategory {
		category = ""
	}
	pick := r.URL.Query().Get("product")
	var visible []shopProduct
	for _, product := range products {
		if category != "" && !slices.Contains(product.LabelIDs, category) {
			continue
		}
		product.URL = shopLocation(b, category, product.ID)
		for _, attachment := range s.cardAttachments(r, b, product.ID) {
			if attachment.IsImage && strings.HasPrefix(attachment.Type, "image/") && inline[attachment.Type] {
				if product.Image == "" || attachment.ID == product.CoverID {
					product.Image = attachmentURL(attachment.ID)
				}
			}
		}
		visible = append(visible, product)
		if pick == product.ID {
			data["Product"] = product
			data["Description"] = viewer(product.Description, policy)
		}
	}
	data["Categories"], data["Products"] = categories, visible
	s.render(w, r, "shop", http.StatusOK, data)
}
