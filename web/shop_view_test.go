package web

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
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

func TestShopTaxTotals(t *testing.T) {
	net, vat, total := shopTaxTotals(1000, 2550, false)
	if net != 1000 || vat != 255 || total != 1255 {
		t.Fatalf("tax-exclusive total: %d %d %d", net, vat, total)
	}
	net, vat, total = shopTaxTotals(1255, 2550, true)
	if net != 1000 || vat != 255 || total != 1255 {
		t.Fatalf("tax-inclusive total: %d %d %d", net, vat, total)
	}
	_, vat, total = shopTaxTotals(2, 2500, false)
	if vat != 1 || total != 3 {
		t.Fatalf("minor-unit rounding: %d %d", vat, total)
	}
}

func TestShopPaymentURL(t *testing.T) {
	if got := shopPaymentURL("https://pay.example.test/invoice?a={amount}&c={currency}&n={number}", "1.25", "EUR", "x&y"); got !=
		"https://pay.example.test/invoice?a=1.25&c=EUR&n=x%26y" {
		t.Fatalf("payment link expansion: %q", got)
	}
	for _, link := range []string{"http://example.test/", "https://unsafe@example.test/", "https://", "https://example.test/\""} {
		if got := shopPaymentURL(link, "1", "EUR", "order"); got != "" {
			t.Errorf("invalid payment link accepted: %q", got)
		}
	}
}

func TestShopReference(t *testing.T) {
	// These values match WeKan's paymentReference(2026, 42, kind).
	for kind, want := range map[string]string{
		"rf": "RF242026000422", "fi": "2026000422", "be": "+++020/2600/04222+++",
		"no": "2026000428", "se": "20260004211", "none": "",
	} {
		if got := shopReference(2026, 42, kind); got != want {
			t.Errorf("%s reference: got %q, want %q", kind, got, want)
		}
	}
}

func TestShopBoardView(t *testing.T) {
	sink := newSMTPSink(t)
	t.Setenv("MAIL_URL", "smtp://"+sink.ln.Addr().String())
	t.Setenv("MAIL_FROM", "orders@example.test")
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
	update("cards", b.Card, bson.M{"customFields": bson.A{
		bson.M{"_id": priceID, "value": "12,50"}, bson.M{"_id": stockID, "value": 3},
	}})
	cartAction := url + "/shop/cart"
	cartForm := map[string][]string{"product": {b.Card}, "quantity": {"2"}, "add": {"1"}}
	res, _ = post(t, guest, cartAction, cartForm)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("cart update: %d", res.StatusCode)
	}
	res, page = get(t, guest, url+"?view=shop&cart=1")
	if res.StatusCode != http.StatusOK || !strings.Contains(page, "25.00 EUR") ||
		!strings.Contains(page, "Listed product") || res.Header.Get("Cache-Control") != "private, no-store" {
		t.Fatalf("cart from shop product: %d %s", res.StatusCode, page)
	}
	orders := newRESTBoard(t, w, login(t, w, "ann"), "Order destination")
	update("boards", b.ID, bson.M{"shopSettings.ordersBoardId": orders.ID,
		"shopSettings.ordersListId": orders.List, "shopSettings.termsUrl": "https://example.test/terms"})
	checkoutURL := url + "?view=shop&checkout=1"
	res, page = get(t, guest, checkoutURL)
	if res.StatusCode != http.StatusOK || strings.Contains(page, "shop-checkout-review") ||
		!strings.Contains(page, tr("en", "shop-login-to-order")) {
		t.Fatalf("guest checkout must be enabled: %d %s", res.StatusCode, page)
	}
	update("boards", b.ID, bson.M{"shopSettings.guestCheckout": true})
	res, page = get(t, guest, checkoutURL)
	if res.StatusCode != http.StatusOK || !strings.Contains(page, "shop-checkout-review") ||
		!strings.Contains(page, "25.00 EUR") || !strings.Contains(page, "https://example.test/terms") ||
		strings.Contains(page, "Order destination") {
		t.Fatalf("checkout review: %d %s", res.StatusCode, page)
	}
	if strings.Contains(page, `name="quote"`) {
		t.Fatal("checkout accepted a tracked product before stock updates were available")
	}
	update("cards", b.Card, bson.M{"customFields": bson.A{bson.M{"_id": priceID, "value": "12,50"}}})
	update("boards", b.ID, bson.M{
		"shopSettings.paymentMethods": bson.A{
			bson.M{"id": "link", "enabled": true,
				"method": bson.M{"type": "link", "url": "https://pay.example.test/order?number={number}&amount={amount}&currency={currency}&reference={reference}"}},
			bson.M{"id": "bank", "enabled": true,
				"method": bson.M{"type": "bank-transfer", "name": "Shop test recipient", "details": "Account fixture"}},
			bson.M{"id": "cash", "enabled": true, "method": bson.M{"type": "cash", "details": "Pay at pickup"}},
			bson.M{"id": "other", "enabled": true, "method": bson.M{"type": "other", "label": "Store credit", "details": "Present your voucher"}},
			bson.M{"id": "paypal", "enabled": true, "method": bson.M{"type": "paypal", "user": "TestSeller"}},
		},
	})
	res, page = get(t, guest, checkoutURL)
	nonce := regexp.MustCompile(`name="id" value="([A-Za-z0-9_-]+)"`).FindStringSubmatch(page)
	quote := regexp.MustCompile(`name="quote" value="([0-9a-f]+)"`).FindStringSubmatch(page)
	if res.StatusCode != http.StatusOK || len(nonce) != 2 || len(quote) != 2 {
		t.Fatalf("order form: %d %s", res.StatusCode, page)
	}
	var cookie *http.Cookie
	for _, candidate := range guest.Jar.Cookies(res.Request.URL) {
		if candidate.Name == shopCartCookie(b.ID) {
			cookie = candidate
		}
	}
	if cookie == nil {
		t.Fatal("missing cart cookie")
	}
	cookie.Path = "/b/" + b.ID + "/"
	form := map[string][]string{"id": {nonce[1]}, "quote": {quote[1]}, "name": {"Test customer"}, "email": {"test@example.test"}, "acceptTerms": {"1"}}
	form["phone"] = []string{"+358 123"}
	form["address"] = []string{"1 Delivery Lane\r\nSecond floor"}
	form["note"] = []string{"![literal](https://example.test/image.png)"}
	orderURL := url + "/shop/order"
	form["quote"] = []string{"stale"}
	if res, _ = post(t, guest, orderURL, form); res.StatusCode != http.StatusConflict {
		t.Fatalf("accepted a stale quote: %d", res.StatusCode)
	}
	form["quote"] = []string{quote[1]}
	for attempt := 0; attempt < 2; attempt++ {
		if res, page = post(t, guest, orderURL, form); res.StatusCode != http.StatusCreated || !strings.Contains(page, "https://pay.example.test/order?number=") ||
			!strings.Contains(page, "amount=25.00&amp;currency=EUR&amp;reference=RF") ||
			!strings.Contains(page, "Shop test recipient") || !strings.Contains(page, "Account fixture") || !strings.Contains(page, "Pay at pickup") || !strings.Contains(page, "Store credit") || !strings.Contains(page, "Present your voucher") || !strings.Contains(page, "https://paypal.me/TestSeller/25.00EUR") {
			t.Fatalf("order submission: %d %s", res.StatusCode, page)
		}
	}
	form["note"] = []string{"mismatched retry"}
	if res, _ = post(t, guest, orderURL, form); res.StatusCode != http.StatusConflict {
		t.Fatalf("mismatched order replay exposed a receipt: %d", res.StatusCode)
	}
	if count, err := w.db.Collection("cards").CountDocuments(ctx, bson.M{"boardId": orders.ID, "title": bson.M{"$regex": "^Order "}}); err != nil || count != 1 {
		t.Fatalf("order card count: %d %v", count, err)
	}
	messages := sink.messages()
	if len(messages) != 1 || !strings.Contains(messages[0], "test@example.test") ||
		!strings.Contains(messages[0], "Account fixture") || !strings.Contains(messages[0], "Pay at pickup") || !strings.Contains(messages[0], "Present your voucher") || !strings.Contains(messages[0], "https://paypal.me/TestSeller/25.00EUR") || !strings.Contains(messages[0], "25.00 EUR") || strings.Contains(messages[0], "Delivery Lane") {
		t.Fatalf("expected one order receipt containing the total, without the address: %d messages", len(messages))
	}
	var saved Card
	if err := w.db.Collection("cards").FindOne(ctx, bson.M{"boardId": orders.ID,
		"title": bson.M{"$regex": "^Order "}}).Decode(&saved); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"+358 123", "    1 Delivery Lane\n    Second floor", "    ![literal]"} {
		if !strings.Contains(saved.Description, text) {
			t.Fatalf("order lost contact data %q", text)
		}
	}
	if strings.Contains(string(viewer(saved.Description)), "<img") {
		t.Fatal("buyer text rendered a remote image")
	}
	guest.Jar.SetCookies(res.Request.URL, []*http.Cookie{cookie})

	update("boards", orders.ID, bson.M{"permission": "public"})
	res, page = get(t, guest, checkoutURL)
	if res.StatusCode != http.StatusOK || strings.Contains(page, "shop-checkout-review") {
		t.Fatalf("checkout to a public destination: %d", res.StatusCode)
	}

	update("cards", b.Card, bson.M{"customFields": bson.A{
		bson.M{"_id": priceID, "value": "13"}, bson.M{"_id": stockID, "value": 3},
	}})
	res, page = get(t, guest, url+"?view=shop&cart=1")
	if res.StatusCode != http.StatusOK || !strings.Contains(page, "26.00 EUR") || strings.Contains(page, "25.00 EUR") {
		t.Fatalf("cart did not use current product price: %d", res.StatusCode)
	}
	update("boards", b.ID, bson.M{"shopSettings.vatPercent": 25.5, "shopSettings.pricesIncludeVat": false})
	res, page = get(t, guest, url+"?view=shop&cart=1")
	if res.StatusCode != http.StatusOK || !strings.Contains(page, "6.63 EUR") || !strings.Contains(page, "32.63 EUR") {
		t.Fatalf("cart VAT total: %d %s", res.StatusCode, page)
	}
	res, _ = post(t, guest, cartAction, map[string][]string{"product": {b.Card}, "quantity": {"0"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("cart remove: %d", res.StatusCode)
	}
	res, page = get(t, guest, url+"?view=shop&cart=1")
	if res.StatusCode != http.StatusOK || !strings.Contains(page, tr("en", "shop-cart-empty")) {
		t.Fatalf("cart item removed: %d", res.StatusCode)
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
	form.Set("bankName", "Shop test recipient")
	form.Set("bankDetails", "Account fixture")
	form.Set("cashDetails", "Pay at pickup")
	form.Set("otherLabel", "Store credit")
	form.Set("otherDetails", "Present your voucher")
	form.Set("paypalUser", "TestSeller")
	form.Set("chequeDetails", "Mail to Shop")
	form.Set("paymentLink", "https://pay.example.test/order?n={number}")
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
	if res.StatusCode != http.StatusOK || !strings.Contains(page, "New catalog") || !strings.Contains(page, "Store credit") || !strings.Contains(page, "Present your voucher") ||
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
	_, other := shopMethod(shopPaymentMethods(settings), "other")
	if other["label"] != "Store credit" || other["details"] != "Present your voucher" {
		t.Fatalf("other payment instructions were lost: %v", other)
	}
	_, bank := shopMethod(shopPaymentMethods(settings), "bank-transfer")
	_, link := shopMethod(shopPaymentMethods(settings), "link")
	_, cash := shopMethod(shopPaymentMethods(settings), "cash")
	_, cheque := shopMethod(shopPaymentMethods(settings), "cheque")
	_, paypal := shopMethod(shopPaymentMethods(settings), "paypal")
	if paypal["user"] != "TestSeller" || cash["details"] != "Pay at pickup" || cheque["details"] != "Mail to Shop" {
		t.Fatalf("manual payment instructions were lost: %v", settings["paymentMethods"])
	}
	if bank["details"] != "Account fixture" || link["url"] != "https://pay.example.test/order?n={number}" {
		t.Fatalf("both payment methods must survive: %v", settings["paymentMethods"])
	}
	if settings["guestCheckout"] != true || settings["priceFieldId"] != price || settings["currency"] != "KWD" ||
		settings["ordersListId"] != orders.List || settings["ordersBoardId"] != orders.ID || settings["decrementStock"] != true ||
		settings["termsUrl"] != "https://example.test/terms" ||
		boardViewStateOf(board).defaultView("public") != "board-view-shop" {
		t.Fatalf("shop settings persistence: %v", board)
	}
}

func TestShopDestinationVisibility(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	token := login(t, w, "ann")
	source := newRESTBoard(t, w, token, "Shop source")
	target := newRESTBoard(t, w, token, "Shop orders")
	c := w.client(t)
	w.signIn(t, c, "ann", "pw")
	set := func(id string, settings bson.M) {
		t.Helper()
		if _, err := w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": id},
			bson.M{"$set": bson.M{"shopSettings": settings}}); err != nil {
			t.Fatal(err)
		}
	}
	address := w.srv.URL + "/b/" + target.ID + "/visibility"
	set(source.ID, bson.M{"enabled": true, "ordersBoardId": target.ID, "ordersListId": target.List})
	for _, visibility := range []string{"public", "instance"} {
		if res, _ := post(t, c, address, url.Values{"visibility": {visibility}}); res.StatusCode != http.StatusConflict {
			t.Fatalf("configured destination became %s: %d", visibility, res.StatusCode)
		}
	}
	set(source.ID, bson.M{"enabled": false, "ordersBoardId": target.ID, "ordersListId": target.List})
	set(target.ID, bson.M{"enabled": true, "ordersListId": target.List})
	if res, _ := post(t, c, address, url.Values{"visibility": {"public"}}); res.StatusCode != http.StatusConflict {
		t.Fatalf("same-board destination became public: %d", res.StatusCode)
	}
	set(target.ID, bson.M{"enabled": false, "ordersListId": target.List})
	if res, _ := post(t, c, address, url.Values{"visibility": {"public"}}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("unused destination could not change visibility: %d", res.StatusCode)
	}
	if _, err := w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": target.ID},
		bson.M{"$set": bson.M{"permission": "private", "shopOrderPrivacy": true}}); err != nil {
		t.Fatal(err)
	}
	if res, _ := post(t, c, address, url.Values{"visibility": {"instance"}}); res.StatusCode != http.StatusConflict {
		t.Fatalf("previous order destination became visible after its shop closed: %d", res.StatusCode)
	}
}
