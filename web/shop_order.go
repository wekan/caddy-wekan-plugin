package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func shopOrderQuote(board string, lines []shopCartLine, total int64, currency string, settings bson.M) string {
	values := make([][]any, 0, len(lines))
	for _, line := range lines {
		values = append(values, []any{line.ID, line.Title, line.Quantity, line.Unit})
	}
	encoded, _ := json.Marshal([]any{board, values, total, currency, settings["ordersBoardId"], settings["ordersListId"], settings["termsUrl"], settings["vatPercent"], settings["pricesIncludeVat"]})
	return fmt.Sprintf("%x", sha256.Sum256(encoded))
}

func shopPaymentURL(template, amount, currency, number string, references ...string) string {
	if !strings.HasPrefix(template, "https://") || utf16Len(template) > 2000 ||
		strings.ContainsAny(template, " \t\r\n<>\"'") {
		return ""
	}
	reference := ""
	if len(references) > 0 {
		reference = references[0]
	}
	value := strings.NewReplacer("{amount}", url.QueryEscape(amount), "{currency}", url.QueryEscape(currency),
		"{number}", url.QueryEscape(number), "{reference}", url.QueryEscape(reference)).Replace(template)
	link, err := url.Parse(value)
	if err != nil || link.Scheme != "https" || link.Hostname() == "" || link.User != nil ||
		strings.ContainsAny(link.Host, "{}") {
		return ""
	}
	return value
}

func shopReference(year int, number int64, kind string) string {
	base := fmt.Sprintf("%04d%05d", year, number)
	mod97 := func(value string) int {
		remainder := 0
		for _, digit := range value {
			remainder = (remainder*10 + int(digit-'0')) % 97
		}
		return remainder
	}
	luhn := func(value string) string {
		sum := 0
		for i := len(value) - 1; i >= 0; i-- {
			digit := int(value[i] - '0')
			if (len(value)-1-i)%2 == 0 {
				digit *= 2
				if digit > 9 {
					digit -= 9
				}
			}
			sum += digit
		}
		return value + strconv.Itoa((10-sum%10)%10)
	}
	switch kind {
	case "none":
		return ""
	case "be":
		if len(base) > 10 {
			return ""
		}
		ten := strings.Repeat("0", 10-len(base)) + base
		check := mod97(ten)
		if check == 0 {
			check = 97
		}
		return fmt.Sprintf("+++%s/%s/%s%02d+++", ten[:3], ten[3:7], ten[7:], check)
	case "no":
		return luhn(base)
	case "se":
		return luhn(base + strconv.Itoa((len(base)+2)%10))
	}
	if len(base) > 19 {
		return ""
	}
	weights := [3]int{7, 3, 1}
	sum := 0
	for i := len(base) - 1; i >= 0; i-- {
		sum += int(base[i]-'0') * weights[(len(base)-1-i)%3]
	}
	finnish := base + strconv.Itoa((10-sum%10)%10)
	if kind == "fi" {
		return finnish
	}
	return fmt.Sprintf("RF%02d%s", 98-mod97(finnish+"271500"), finnish)
}

func shopPaymentMethods(settings bson.M) bson.A {
	methods, _ := settings["paymentMethods"].(bson.A)
	return methods
}

func shopMethod(methods bson.A, kind string) (int, bson.M) {
	for i, raw := range methods {
		entry := asM(raw)
		method := asM(entry["method"])
		if method["type"] == kind && entry["enabled"] != false {
			return i, method
		}
	}
	return -1, nil
}

var shopManualMethods = []struct{ Kind, Label, Field string }{
	{"bank-transfer", msg("billing-method-bank-transfer"), "bankDetails"},
	{"cash", msg("billing-method-cash"), "cashDetails"},
	{"cheque", msg("billing-method-cheque"), "chequeDetails"},
}

type shopInstructions struct{ Label, Name, Details string }

func shopManualInstructions(settings bson.M, lang string) []shopInstructions {
	var instructions []shopInstructions
	for _, option := range shopManualMethods {
		_, method := shopMethod(shopPaymentMethods(settings), option.Kind)
		if details := strOf(method["details"]); details != "" {
			instructions = append(instructions, shopInstructions{tr(lang, option.Label), strOf(method["name"]), details})
		}
	}
	return instructions
}

func (s *Server) submitShopOrder(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	if !sameOrigin(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	if r.ParseForm() != nil {
		http.Error(w, "Invalid order", http.StatusBadRequest)
		return
	}
	u := s.currentUser(r)
	b, err := s.boardFor(r.Context(), r.PathValue("board"), u)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	lanes, _, err := s.boardData(r.Context(), b)
	if err != nil {
		http.Error(w, "Order unavailable", http.StatusServiceUnavailable)
		return
	}
	s.boardShop(w, r, b, u, lanes)
}

func shopContactField(value string, limit int, multiline bool) (string, bool) {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\r\n", "\n"))
	if utf16Len(value) > limit {
		return "", false
	}
	for _, character := range value {
		if unicode.IsControl(character) && !(multiline && character == '\n') {
			return "", false
		}
	}
	return value, true
}

func writeShopOrderText(builder *strings.Builder, value string) {
	for _, line := range strings.Split(value, "\n") {
		fmt.Fprintln(builder, "    "+line)
	}
}

func (s *Server) saveShopOrder(w http.ResponseWriter, r *http.Request, shop *Board, u *User, settings bson.M, lines []shopCartLine, total, quote string) {
	lang := languageOf(r, u).Tag
	if r.PostForm.Get("quote") != quote {
		http.Error(w, tr(lang, msg("shop-price-changed")), http.StatusConflict)
		return
	}
	name, validName := shopContactField(r.PostForm.Get("name"), 100, false)
	email, validEmail := shopContactField(r.PostForm.Get("email"), 254, false)
	phone, validPhone := shopContactField(r.PostForm.Get("phone"), 40, false)
	delivery, validDelivery := shopContactField(r.PostForm.Get("address"), 500, true)
	note, validNote := shopContactField(r.PostForm.Get("note"), 1000, true)
	address, err := mail.ParseAddress(email)
	nonce := r.PostForm.Get("id")
	if !validID(nonce) || !validName || !validEmail || !validPhone || !validDelivery || !validNote ||
		name == "" || err != nil || address.Address != email ||
		(shopTermsURL(strOf(settings["termsUrl"])) != "" && r.PostForm.Get("acceptTerms") != "1") {
		http.Error(w, "Invalid order details", http.StatusBadRequest)
		return
	}
	boardID, listID := strOf(settings["ordersBoardId"]), strOf(settings["ordersListId"])
	if boardID == "" {
		boardID = shop.ID
	}
	ctx := r.Context()
	swimlane, err := s.shopOrderSwimlane(ctx, boardID, listID)
	if err != nil {
		http.Error(w, "Order destination unavailable", http.StatusConflict)
		return
	}
	pinned, err := s.db.Collection("boards").UpdateOne(ctx, bson.M{
		"_id": eq(boardID), "permission": "private", "archived": bson.M{"$ne": true}, "deletedAt": nil,
	}, bson.M{"$set": bson.M{"shopOrderPrivacy": true}})
	if err != nil || pinned.MatchedCount == 0 {
		http.Error(w, "Order destination unavailable", http.StatusConflict)
		return
	}

	digest := sha256.Sum256([]byte(shop.ID + ":" + nonce))
	id := hex.EncodeToString(digest[:16])
	userID := ""
	if u != nil {
		userID = u.ID
	}
	var description strings.Builder
	for _, entry := range []struct{ label, value string }{
		{msg("name"), name}, {msg("email"), email}, {msg("billing-phone"), phone},
		{msg("billing-address"), delivery}, {msg("shop-note"), note},
	} {
		if entry.value != "" {
			fmt.Fprintf(&description, "%s\n\n", tr(lang, entry.label))
			writeShopOrderText(&description, entry.value)
			description.WriteByte('\n')
		}
	}
	for _, line := range lines {
		fmt.Fprintf(&description, "- %s × %d — %s\n", line.Title, line.Quantity, line.Total)
	}
	fmt.Fprintf(&description, "\n%s", total)
	number, err := s.nextCardNumber(ctx, boardID)
	if err != nil {
		http.Error(w, "Could not save order", http.StatusServiceUnavailable)
		return
	}
	card := Card{ID: id, BoardID: boardID, ListID: listID, SwimlaneID: swimlane, UserID: userID,
		Title: "Order " + id, Description: description.String(), Type: "cardType-card",
		Members: []string{}, Assignees: []string{}, LabelIDs: []string{}, CardNumber: number, CreatedAt: time.Now().UTC(),
		Sort: s.nextSort(ctx, "cards", bson.M{"boardId": eq(boardID), "listId": eq(listID), "swimlaneId": eq(swimlane)})}
	reference := shopReference(card.CreatedAt.Year(), number, strOf(settings["referenceType"]))
	order := struct {
		Card      `bson:",inline"`
		Reference string `bson:"shopPaymentReference"`
	}{card, reference}
	_, err = s.db.Collection("cards").InsertOne(ctx, order)
	created := err == nil
	if mongo.IsDuplicateKeyError(err) {
		var existing = order
		existing.Reference = ""
		err = s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(id), "boardId": eq(boardID)}).Decode(&existing)
		if err == nil && existing.Description != card.Description {
			http.Error(w, "Order already submitted", http.StatusConflict)
			return
		}
		reference = existing.Reference
	}
	if err != nil {
		http.Error(w, "Could not save order", http.StatusServiceUnavailable)
		return
	}
	amount, currency, _ := strings.Cut(total, " ")
	_, method := shopMethod(shopPaymentMethods(settings), "link")
	instructions := shopManualInstructions(settings, lang)
	payment := shopPaymentURL(strOf(method["url"]), amount, currency, id, reference)
	if created && settings["confirmationEmail"] != false {
		shopName := strOf(settings["title"])
		if shopName == "" {
			shopName = shop.Title
		}
		shopName = strings.Join(strings.Fields(shopName), " ")
		subject := strings.NewReplacer("__number__", id, "__shop__", shopName).Replace(tr(lang, msg("shop-email-subject")))
		var receipt strings.Builder
		fmt.Fprintln(&receipt, strings.ReplaceAll(tr(lang, msg("shop-order-placed")), "__number__", id))
		for _, line := range lines {
			fmt.Fprintf(&receipt, "%s × %d — %s\n", line.Title, line.Quantity, line.Total)
		}
		fmt.Fprintln(&receipt, total)
		if reference != "" {
			fmt.Fprintln(&receipt, tr(lang, msg("billing-reference")), reference)
		}
		for _, method := range instructions {
			fmt.Fprintln(&receipt, method.Label)
			if method.Name != "" {
				fmt.Fprintln(&receipt, method.Name)
			}
			fmt.Fprintln(&receipt, method.Details)
		}
		if payment != "" {
			fmt.Fprintln(&receipt, tr(lang, msg("shop-email-pay")), payment)
		}
		if err := s.sendMail(ctx, r.Host, mailMessage{To: []string{email}, Subject: subject, Text: receipt.String()}); err != nil && !errors.Is(err, errNoMail) {
			s.log.Warn("Shop confirmation delivery failed", "order", id)
		}
	}
	s.hub.publishFrom(boardID, "board-changed", "")
	http.SetCookie(w, &http.Cookie{Name: shopCartCookie(shop.ID), Path: "/b/" + shop.ID + "/", MaxAge: -1, HttpOnly: true})
	if payment != "" || reference != "" || len(instructions) > 0 {
		s.render(w, r, "shop_order", http.StatusCreated, map[string]any{
			"Title": shop.Title, "User": u, "Total": total, "Payment": payment, "Reference": reference,
			"Instructions": instructions,
			"Message":      strings.ReplaceAll(tr(lang, msg("shop-order-placed")), "__number__", id),
			"Shop":         shopLocation(shop, "", ""),
		})
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusCreated)
	fmt.Fprintln(w, strings.ReplaceAll(tr(lang, msg("shop-order-placed")), "__number__", id))
}

func (s *Server) shopOrderSwimlane(ctx context.Context, boardID, listID string) (string, error) {
	var list List
	if err := s.db.Collection("lists").FindOne(ctx, bson.M{
		"_id": eq(listID), "boardId": eq(boardID), "archived": bson.M{"$ne": true}, "deletedAt": nil,
	}).Decode(&list); err != nil {
		return "", err
	}
	filter := bson.M{"boardId": eq(boardID), "archived": bson.M{"$ne": true}, "deletedAt": nil}
	if list.SwimlaneID != "" {
		filter["_id"] = eq(list.SwimlaneID)
	}
	var lane Swimlane
	if err := s.db.Collection("swimlanes").FindOne(ctx, filter).Decode(&lane); err != nil {
		return "", err
	}
	if lane.ID == "" {
		return "", errors.New("no order swimlane")
	}
	return lane.ID, nil
}
