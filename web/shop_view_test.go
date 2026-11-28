package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestShopPrice(t *testing.T) {
	for _, test := range []struct {
		value          any
		currency, want string
	}{
		{"12,50", "EUR", "12.50 EUR"}, {12.8, "JPY", "13 JPY"},
		{"1.2345", "KWD", "1.235 KWD"}, {0.0, "EUR", "0.00 EUR"},
		{"1e4", "EUR", ""}, {-1, "EUR", ""},
	} {
		if got := shopPrice(test.value, test.currency); got != test.want {
			t.Errorf("shopPrice(%v, %s) = %q, want %q", test.value, test.currency, got, test.want)
		}
	}
}

func TestShopBoardView(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	b := newRESTBoard(t, w, login(t, w, "ann"), "Shop fixture")
	update := func(collection, id string, fields bson.M) {
		t.Helper()
		if _, err := w.db.Collection(collection).UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": fields}); err != nil {
			t.Fatal(err)
		}
	}
	priceID, stockID, skuID, categoryID := randomID(), randomID(), randomID(), randomID()
	settings := bson.M{"enabled": true, "title": "Storefront", "priceFieldId": priceID, "stockFieldId": stockID,
		"skuFieldId": skuID, "currency": "EUR"}
	update("boards", b.ID, bson.M{"permission": "public", "defaultPublicBoardView": "board-view-shop",
		"shopSettings": settings, "labels": bson.A{bson.M{"_id": categoryID, "name": "Category fixture"}}})
	for _, field := range []bson.M{
		{"_id": priceID, "type": "number"},
		{"_id": stockID, "type": "number"},
		{"_id": skuID, "type": "text", "adminOnly": true},
	} {
		field["boardIds"] = bson.A{b.ID}
		if _, err := w.db.Collection("customFields").InsertOne(ctx, field); err != nil {
			t.Fatal(err)
		}
	}
	update("cards", b.Card, bson.M{"title": "Listed product", "description": "**Fixture description**",
		"labelIds": bson.A{categoryID}, "customFields": bson.A{
			bson.M{"_id": priceID, "value": "12,50"}, bson.M{"_id": stockID, "value": 0},
			bson.M{"_id": skuID, "value": "private-sku-fixture"},
		}})
	guest := w.client(t)
	url := w.srv.URL + "/b/" + b.ID + "/shop-fixture"
	res, page := get(t, guest, url)
	if res.StatusCode != http.StatusOK || !strings.Contains(page, "shop-products") ||
		!strings.Contains(page, "12.50 EUR") || !strings.Contains(page, "Category fixture") {
		t.Fatalf("shop landing: %d %s", res.StatusCode, page)
	}
	res, page = get(t, guest, url+"?view=shop&category="+categoryID+"&product="+b.Card)
	if res.StatusCode != http.StatusOK || !strings.Contains(page, "<strong>Fixture description</strong>") ||
		strings.Contains(page, "private-sku-fixture") || !strings.Contains(page, tr("en", "shop-sold-out")) {
		t.Fatalf("shop product: %d %s", res.StatusCode, page)
	}
	update("customFields", priceID, bson.M{"adminOnly": true})
	res, page = get(t, guest, url)
	if res.StatusCode != http.StatusOK || strings.Contains(page, "12.50 EUR") || strings.Contains(page, "Listed product") {
		t.Fatalf("restricted price field leaked: %d", res.StatusCode)
	}
}
