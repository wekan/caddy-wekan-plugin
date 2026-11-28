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
	_, err = s.db.Collection("cards").InsertOne(ctx, card)
	created := err == nil
	if mongo.IsDuplicateKeyError(err) {
		var existing Card
		err = s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(id), "boardId": eq(boardID)}).Decode(&existing)
		if err == nil && existing.Description != card.Description {
			http.Error(w, "Order already submitted", http.StatusConflict)
			return
		}
	}
	if err != nil {
		http.Error(w, "Could not save order", http.StatusServiceUnavailable)
		return
	}
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
		if err := s.sendMail(ctx, r.Host, mailMessage{To: []string{email}, Subject: subject, Text: receipt.String()}); err != nil && !errors.Is(err, errNoMail) {
			s.log.Warn("Shop confirmation delivery failed", "order", id)
		}
	}
	s.hub.publishFrom(boardID, "board-changed", "")
	http.SetCookie(w, &http.Cookie{Name: shopCartCookie(shop.ID), Path: "/b/" + shop.ID + "/", MaxAge: -1, HttpOnly: true})
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
