package web

import (
	"context"
	"net/http"
	"net/url"
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

func TestShopSettingsEditor(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	b := newRESTBoard(t, w, login(t, w, "ann"), "Shop config")
	setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: true})
	orders := newRESTBoard(t, w, login(t, w, "ann"), "Private orders")
	public := newRESTBoard(t, w, login(t, w, "ann"), "Public destination")
	if _, err := w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": public.ID},
		bson.M{"$set": bson.M{"permission": "public"}}); err != nil {
		t.Fatal(err)
	}
	price := randomID()
	if _, err := w.db.Collection("customFields").InsertOne(ctx, bson.M{
		"_id": price, "name": "Catalog price", "type": "number", "boardIds": bson.A{b.ID},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{
		"$set": bson.M{"shopSettings.guestCheckout": true},
	}); err != nil {
		t.Fatal(err)
	}
	annClient, bobClient := w.client(t), w.client(t)
	w.signIn(t, annClient, "ann", "pw")
	w.signIn(t, bobClient, "bob", "pw")
	address := w.srv.URL + "/b/" + b.ID + "/shop-config/shop/settings"
	form := url.Values{"title": {"New catalog"}, "currency": {"kwd"}, "priceFieldId": {price},
		"enabled": {"1"}, "publicDefault": {"1"}, "saleListIds": {b.List},
		"ordersListId": {orders.List}, "guestCheckout": {"1"}, "decrementStock": {"1"}, "termsUrl": {"https://example.test/terms"}}
	if res, _ := post(t, bobClient, address, form); res.StatusCode != http.StatusForbidden {
		t.Fatalf("member entered the editor: %d", res.StatusCode)
	}
	res, page := get(t, annClient, address)
	if res.StatusCode != http.StatusOK || !strings.Contains(page, "Catalog price") || !strings.Contains(page, `name="enabled"`) {
		t.Fatalf("shop settings form: %d %s", res.StatusCode, page)
	}
	if res, _ := post(t, annClient, address, form); res.StatusCode != http.StatusOK && res.StatusCode != http.StatusSeeOther {
		t.Fatalf("shop settings save: %d", res.StatusCode)
	}
	res, page = get(t, annClient, address)
	if res.StatusCode != http.StatusOK || !strings.Contains(page, "New catalog") ||
		!strings.Contains(page, `value="1" checked`) || !strings.Contains(page, `value="`+price+`" selected`) {
		t.Fatalf("shop settings reload: %d %s", res.StatusCode, page)
	}
	form.Set("ordersListId", public.List)
	if res, _ := post(t, annClient, address, form); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("accepted a public order destination: %d", res.StatusCode)
	}
	form.Set("ordersListId", orders.List)
	form.Set("termsUrl", "javascript:alert(1)")
	if res, _ := post(t, annClient, address, form); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("accepted unsafe terms URL: %d", res.StatusCode)
	}
	form.Set("termsUrl", "https://example.test/terms")
	form.Set("priceFieldId", "foreign")
	if res, _ := post(t, annClient, address, form); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("accepted a foreign custom field: %d", res.StatusCode)
	}
	var board bson.M
	if err := w.db.Collection("boards").FindOne(ctx, bson.M{"_id": b.ID}).Decode(&board); err != nil {
		t.Fatal(err)
	}
	settings := asM(board["shopSettings"])
	if settings["guestCheckout"] != true || settings["priceFieldId"] != price || settings["currency"] != "KWD" ||
		settings["ordersListId"] != orders.List || settings["ordersBoardId"] != orders.ID || settings["decrementStock"] != true ||
		settings["termsUrl"] != "https://example.test/terms" ||
		boardViewStateOf(board).defaultView("public") != "board-view-shop" {
		t.Fatalf("shop settings persistence: %v", board)
	}
}
