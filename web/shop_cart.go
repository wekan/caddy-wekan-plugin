package web

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
)

type shopCartLine struct {
	ID, Title, Unit, Total string
	Quantity, Stock        int
	Unavailable            bool
}

func shopCartLocation(b *Board) string {
	return shopLocation(b, "", "") + "&cart=1"
}

func shopCartCookie(boardID string) string {
	return "weddy-shop-" + boardID
}

func readShopCart(r *http.Request, boardID string) map[string]int {
	items := map[string]int{}
	cookie, err := r.Cookie(shopCartCookie(boardID))
	if err == nil && len(cookie.Value) <= 3600 {
		if raw, err := base64.RawURLEncoding.DecodeString(cookie.Value); err == nil {
			_ = json.Unmarshal(raw, &items)
		}
	}
	clean := make(map[string]int)
	for id, qty := range items {
		if validID(id) && qty > 0 && qty <= 999 {
			clean[id] = qty
		}
	}
	return clean
}

func (s *Server) updateShopCart(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	u := s.currentUser(r)
	b, err := s.boardFor(r.Context(), r.PathValue("board"), u)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2048)
	if r.ParseForm() != nil {
		http.Error(w, "Invalid cart", http.StatusBadRequest)
		return
	}
	id := r.PostForm.Get("product")
	qty, err := strconv.Atoi(r.PostForm.Get("quantity"))
	if err != nil || !validID(id) || qty < 0 || qty > 999 {
		http.Error(w, "Invalid cart quantity", http.StatusBadRequest)
		return
	}
	cart := readShopCart(r, b.ID)
	if r.PostForm.Get("add") == "1" {
		qty += cart[id]
	}
	if qty > 999 {
		http.Error(w, "Invalid cart quantity", http.StatusBadRequest)
		return
	}
	if qty == 0 {
		delete(cart, id)
	} else {
		cart[id] = qty
	}
	raw, err := json.Marshal(cart)
	value := base64.RawURLEncoding.EncodeToString(raw)
	if err != nil || len(cart) > 100 || len(value) > 3600 {
		http.Error(w, "Cart is full", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: shopCartCookie(b.ID), Value: value, Path: "/b/" + b.ID + "/",
		HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode, MaxAge: 30 * 86400})
	http.Redirect(w, r, shopCartLocation(b), http.StatusSeeOther)
}

func shopCartQuote(products []shopProduct, quantities map[string]int, currency string) ([]shopCartLine, string) {
	byID := make(map[string]shopProduct, len(products))
	for _, product := range products {
		byID[product.ID] = product
	}
	ids := make([]string, 0, len(quantities))
	for id := range quantities {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	lines := make([]shopCartLine, 0, len(ids))
	var sum int64
	digits := 0
	for _, id := range ids {
		qty := quantities[id]
		product, live := byID[id]
		line := shopCartLine{ID: id, Quantity: qty, Stock: product.Stock,
			Unavailable: !live || qty > product.Stock, Title: product.Title, Unit: product.Price}
		if live {
			if qty < 1 || product.Minor > (9007199254740991-sum)/int64(qty) {
				line.Unavailable = true
			}
			digits = product.Digits
			if !line.Unavailable {
				total := int64(qty) * product.Minor
				line.Total = shopMoney(total, digits, currency)
				sum += total
			}
		}
		lines = append(lines, line)
	}
	return lines, shopMoney(sum, digits, currency)
}
