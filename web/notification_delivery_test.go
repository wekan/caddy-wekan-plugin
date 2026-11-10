package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// WeKan's tests/notificationDelivery.test.cjs: what one level may hold.
func TestNormalizeChannelSettings(t *testing.T) {
	parse := func(s string) any {
		var v any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	got, err := normalizeChannelSettings("email", parse(`{"layout":"clear","parts":["link","board"],"grouping":"card","schedule":"interval","intervalMinutes":30,"quietStart":"22:00","quietEnd":"07:00","timezone":"Europe/Helsinki"}`))
	if err != nil {
		t.Fatal(err)
	}
	if p := got["parts"].([]string); len(p) != 2 || p[0] != "board" || p[1] != "link" {
		t.Errorf("parts in WeKan's order: %v", p)
	}
	if got["intervalMinutes"] != int32(30) || got["grouping"] != "card" || got["timezone"] != "Europe/Helsinki" {
		t.Errorf("the values kept: %v", got)
	}
	if n, err := normalizeChannelSettings("tray", parse(`{}`)); err != nil || n != nil {
		t.Errorf("nothing set is inherit: %v %v", n, err)
	}
	if n, _ := normalizeChannelSettings("webhook", parse(`{"text":false,"fields":["ids","standard"]}`)); n["text"] != false || len(n["fields"].([]string)) != 2 {
		t.Errorf("a webhook's content: %v", n)
	}
	for name, bad := range map[string][2]string{
		"an unknown key":                 {"email", `{"colour":"red"}`},
		"a webhook key on e-mail":        {"email", `{"text":true}`},
		"a grouping the tray has not":    {"tray", `{"grouping":"all"}`},
		"an interval not offered":        {"email", `{"intervalMinutes":7}`},
		"a fractional interval":          {"email", `{"intervalMinutes":30.5}`},
		"a time that is not HH:MM":       {"email", `{"dailyTime":"8am"}`},
		"half of the quiet hours":        {"email", `{"quietStart":"22:00"}`},
		"an unknown time zone":           {"email", `{"timezone":"Mars/Olympus"}`},
		"an unknown part":                {"tray", `{"parts":["board","password"]}`},
		"a list of more than it has":     {"tray", `{"parts":["board","board","board","board","board","board","board","board","board","board","board","board","board","board","board","board","board"]}`},
		"a field group that is not one":  {"webhook", `{"fields":["secrets"]}`},
		"a text that is not true/false":  {"webhook", `{"text":"yes"}`},
		"a settings value not an object": {"email", `"all"`},
		"an operator as a value":         {"email", `{"grouping":{"$ne":"x"}}`},
	} {
		if _, err := normalizeChannelSettings(bad[0], parse(bad[1])); err == nil {
			t.Errorf("negative: %s is refused", name)
		}
	}
	if _, err := normalizeChannelSettings("sms", nil); err == nil {
		t.Error("negative: an unknown channel")
	}
	if _, err := normalizeDeliverySettings(parse(`{"email":{"grouping":"all"}}`), []string{"webhook"}); err == nil {
		t.Error("negative: a webhook's settings hold its own channel only")
	}
}

// setNotificationDelivery as WeKan's server/lib/tests/notificationDelivery.tests.js:
// each scope by who may set it; a change merges, null clears a key, no settings
// clear the level, and nothing invalid is written.
func TestSetNotificationDelivery(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	ann := w.seedUser(t, "ann", "pw") // the board's admin
	bob := w.seedUser(t, "bob", "pw") // a normal member
	site := w.seedUser(t, "site", "pw")
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": site.ID}, bson.M{"$set": bson.M{"isAdmin": true}})
	if n, _ := w.db.Collection("settings").CountDocuments(ctx, bson.M{}); n == 0 {
		_, _ = w.db.Collection("settings").InsertOne(ctx, bson.M{"_id": randomID()}) // inserted, not a filter
	}
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Delivery")
	setMember(t, w, rb.ID, Member{UserID: bob.ID, IsActive: true})
	hookID, globalID := randomID(), randomID()
	_, _ = w.db.Collection("integrations").InsertOne(ctx, bson.M{"_id": hookID, "boardId": rb.ID, "url": "https://hooks.example/a", "enabled": true})       // inserted, not a filter
	_, _ = w.db.Collection("integrations").InsertOne(ctx, bson.M{"_id": globalID, "boardId": "_global", "url": "https://hooks.example/g", "enabled": true}) // inserted, not a filter
	caller := func(user string) func(args string) map[string]any {
		c := w.client(t)
		body(t, w.signIn(t, c, user, "pw"))
		return func(args string) map[string]any {
			req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/setNotificationDelivery", strings.NewReader(args))
			req.Header.Set("Content-Type", "application/json")
			res, err := c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			var out map[string]any
			_ = json.NewDecoder(res.Body).Decode(&out)
			return out
		}
	}
	annCall, bobCall, siteCall := caller("ann"), caller("bob"), caller("site")
	stored := func(coll, id string, keys ...string) any {
		var doc bson.M
		_ = w.db.Collection(coll).FindOne(ctx, bson.M{"_id": id}).Decode(&doc)
		return plain(nestedField(doc, keys...))
	}
	refused := func(out map[string]any) bool { _, ok := out["error"]; return ok && out["result"] == nil }

	// Member: the caller's own e-mail, merged key by key.
	if out := bobCall(`["member",null,"email",{"grouping":"card"}]`); refused(out) {
		t.Fatalf("a member sets their own e-mail: %v", out)
	}
	bobCall(`["member",null,"email",{"schedule":"daily","dailyTime":"07:30"}]`)
	if m, _ := stored("users", bob.ID, "profile", "notificationDelivery", "email").(map[string]any); m["grouping"] != "card" || m["schedule"] != "daily" {
		t.Errorf("a second change keeps the first: %v", m)
	}
	if d := w.s.emailDeliveryFor(ctx, bob.ID, rb.ID); d.Grouping != "card" || d.Schedule != "daily" || d.DailyTime != "07:30" {
		t.Errorf("the e-mail is delivered as the member set it: %+v", d)
	}
	bobCall(`["member",null,"email",{"grouping":null}]`)
	if m, _ := stored("users", bob.ID, "profile", "notificationDelivery", "email").(map[string]any); m["grouping"] != nil || m["schedule"] != "daily" {
		t.Errorf("null clears that key alone: %v", m)
	}
	if out := bobCall(`["member",null,"email",{"grouping":"card","colour":"red"}]`); !refused(out) || out["error"] != "invalid-notification-delivery" {
		t.Errorf("negative: an unknown key is refused: %v", out)
	}
	if m, _ := stored("users", bob.ID, "profile", "notificationDelivery", "email").(map[string]any); m["grouping"] != nil {
		t.Errorf("negative: a refused change writes nothing: %v", m)
	}
	bobCall(`["member",null,"email",null]`)
	if v := stored("users", bob.ID, "profile", "notificationDelivery", "email"); v != nil {
		t.Errorf("no settings clear the level: %v", v)
	}
	if out := bobCall(`["member","` + ann.ID + `","email",{"grouping":"card"}]`); !refused(out) || out["error"] != "not-authorized" {
		t.Errorf("negative: a member sets nobody else's: %v", out)
	}
	if v := stored("users", ann.ID, "profile", "notificationDelivery"); v != nil {
		t.Errorf("negative: ann's profile untouched: %v", v)
	}
	// Member webhooks: only leaving their name out.
	if out := bobCall(`["member",null,"webhook",{"hideIdentity":true}]`); refused(out) || stored("users", bob.ID, "profile", "webhookHideIdentity") != true {
		t.Errorf("a member leaves their name out: %v", out)
	}
	if out := bobCall(`["member",null,"webhook",{"grouping":"all"}]`); !refused(out) {
		t.Errorf("negative: a member changes no board's webhook delivery: %v", out)
	}
	bobCall(`["member",null,"webhook",{"hideIdentity":false}]`)
	if v := stored("users", bob.ID, "profile", "webhookHideIdentity"); v != nil {
		t.Errorf("false unsets it: %v", v)
	}
	// Board: its admins, or a site admin.
	if out := bobCall(`["board","` + rb.ID + `","email",{"grouping":"board"}]`); !refused(out) || stored("boards", rb.ID, "notificationDelivery") != nil {
		t.Errorf("negative: a normal member sets no board's delivery: %v", out)
	}
	if out := annCall(`["board","` + rb.ID + `","tray",{"grouping":"card"}]`); refused(out) || stored("boards", rb.ID, "notificationDelivery", "tray", "grouping") != "card" {
		t.Errorf("the board's admin sets it: %v", out)
	}
	if out := siteCall(`["board","` + rb.ID + `","email",{"grouping":"board"}]`); refused(out) {
		t.Errorf("a site admin sets it: %v", out)
	}
	// A webhook: its board's admins; a global one only a site admin.
	if out := bobCall(`["integration","` + hookID + `","webhook",{"grouping":"all"}]`); !refused(out) || stored("integrations", hookID, "notificationDelivery") != nil {
		t.Errorf("negative: a normal member sets no webhook's delivery: %v", out)
	}
	if out := annCall(`["integration","` + hookID + `","webhook",{"text":false,"fields":["ids"]}]`); refused(out) || stored("integrations", hookID, "notificationDelivery", "webhook", "text") != false {
		t.Errorf("the board's admin sets their webhook: %v", out)
	}
	if out := annCall(`["integration","` + hookID + `","email",{"grouping":"all"}]`); !refused(out) {
		t.Errorf("negative: a webhook has the webhook channel only: %v", out)
	}
	if out := annCall(`["integration","` + globalID + `","webhook",{"grouping":"all"}]`); !refused(out) || stored("integrations", globalID, "notificationDelivery") != nil {
		t.Errorf("negative: a board admin sets no global webhook: %v", out)
	}
	if out := siteCall(`["integration","` + globalID + `","webhook",{"grouping":"all"}]`); refused(out) {
		t.Errorf("a site admin sets a global webhook: %v", out)
	}
	// Admin Panel: site admins only.
	if out := annCall(`["admin",null,"email",{"grouping":"none"}]`); !refused(out) {
		t.Errorf("negative: a board admin sets no instance default: %v", out)
	}
	if out := siteCall(`["admin",null,"email",{"grouping":"none"}]`); refused(out) {
		t.Errorf("a site admin sets the instance default: %v", out)
	}
	for name, args := range map[string]string{"an unknown scope": `["everyone",null,"email",{}]`, "an unknown channel": `["member",null,"sms",{}]`,
		"a settings value not an object": `["member",null,"email","all"]`, "an object as the target": `["board",{"$ne":""},"email",{}]`} {
		if out := siteCall(args); !refused(out) {
			t.Errorf("negative: %s is refused: %v", name, out)
		}
	}
	// The REST route of a webhook takes its delivery whole, validated first.
	at := login(t, w, "ann")
	path := w.srv.URL + "/api/boards/" + rb.ID + "/integrations/" + hookID
	if code := call(t, "PUT", path, at, `{"title":"Renamed","notificationDelivery":{"webhook":{"grouping":"nonsense"}}}`, nil); code != http.StatusBadRequest {
		t.Errorf("negative: an invalid delivery is 400: %d", code)
	}
	if stored("integrations", hookID, "title") == "Renamed" {
		t.Error("negative: a refused request changes no field at all")
	}
	if code := call(t, "PUT", path, at, `{"notificationDelivery":{"webhook":{"grouping":"board","schedule":"interval","intervalMinutes":15}}}`, nil); code != http.StatusOK ||
		stored("integrations", hookID, "notificationDelivery", "webhook", "grouping") != "board" {
		t.Errorf("a valid delivery is kept: %d", code)
	}
	if d := w.s.webhookDeliveryFor(ctx, bson.M{"_id": hookID, "boardId": rb.ID, "notificationDelivery": stored("integrations", hookID, "notificationDelivery")}); d.Grouping != "board" || d.IntervalMinutes != 15 {
		t.Errorf("the webhook is sent as set: %+v", d)
	}
	if code := call(t, "PUT", path, at, `{"notificationDelivery":null}`, nil); code != http.StatusOK || stored("integrations", hookID, "notificationDelivery") != nil {
		t.Errorf("null inherits again: %d", code)
	}
}

// Member Settings' notification part on the profile page: what it shows and
// what its forms save; "Default" leaves a setting to the board and Admin Panel.
func TestMemberDeliveryPage(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	bob := w.seedUser(t, "bob", "pw")
	c := w.client(t)
	body(t, w.signIn(t, c, "bob", "pw"))
	page := getBody(t, c, w.srv.URL+"/profile")
	for _, want := range []string{`id="notification-settings"`, `action="/profile/notifications"`, `name="grouping"`, `name="hideIdentity"`, `class="js-browser-timezone"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the profile page has %s", want)
		}
	}
	res, err := c.PostForm(w.srv.URL+"/profile/notifications", url.Values{"channel": {"email"}, "layout": {"classic"}, "grouping": {"board"},
		"schedule": {"interval"}, "intervalMinutes": {"30"}, "quietStart": {"22:00"}, "quietEnd": {"06:00"}, "parts": {"card", "link"},
		"browserTimezone": {"Europe/Helsinki"}})
	if err != nil || res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(res.Header.Get("Location"), "/profile?saved=notifications") {
		t.Fatalf("the e-mail form saves: %v %v", err, res.Header.Get("Location"))
	}
	var doc bson.M
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"_id": bob.ID}).Decode(&doc)
	m, _ := plain(nestedField(doc, "profile", "notificationDelivery", "email")).(map[string]any)
	if m["grouping"] != "board" || m["layout"] != "classic" || m["timezone"] != "Europe/Helsinki" || m["quietEnd"] != "06:00" {
		t.Errorf("what the form chose, in the browser's time zone: %v", m)
	}
	if parts, _ := m["parts"].([]any); len(parts) != 2 {
		t.Errorf("the parts ticked: %v", m["parts"])
	}
	page = getBody(t, c, w.srv.URL+"/profile")
	if !strings.Contains(page, `<option value="board" selected`) || !strings.Contains(page, "Europe/Helsinki") {
		t.Error("the page shows what is set")
	}
	// Back to "Default": the level is cleared.
	_, _ = c.PostForm(w.srv.URL+"/profile/notifications", url.Values{"channel": {"email"}, "partsDefault": {"true"}})
	doc = nil
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"_id": bob.ID}).Decode(&doc)
	if m, _ := plain(nestedField(doc, "profile", "notificationDelivery", "email")).(map[string]any); m["grouping"] != nil || m["parts"] != nil {
		t.Errorf("Default clears each setting: %v", m)
	}
	// Negative: a value the form never offers is refused, nothing written.
	res, _ = c.PostForm(w.srv.URL+"/profile/notifications", url.Values{"channel": {"tray"}, "grouping": {"all"}, "partsDefault": {"true"}})
	if loc := res.Header.Get("Location"); loc != "/profile?error=notification-delivery-invalid" {
		t.Errorf("negative: the tray has no 'all' grouping: %s", loc)
	}
	doc = nil
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"_id": bob.ID}).Decode(&doc)
	if v := nestedField(doc, "profile", "notificationDelivery", "tray"); v != nil {
		t.Errorf("negative: nothing written: %v", v)
	}
	res, _ = c.PostForm(w.srv.URL+"/profile/notifications", url.Values{"channel": {"webhook"}, "hideIdentity": {"true"}})
	doc = nil
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"_id": bob.ID}).Decode(&doc)
	if res.StatusCode != http.StatusSeeOther || nestedField(doc, "profile", "webhookHideIdentity") != true {
		t.Error("the webhook form leaves the member's name out")
	}
	// Negative: signed out, nothing is saved.
	anon := w.client(t)
	res, _ = anon.PostForm(w.srv.URL+"/profile/notifications", url.Values{"channel": {"webhook"}, "hideIdentity": {"false"}})
	doc = nil
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"_id": bob.ID}).Decode(&doc)
	if nestedField(doc, "profile", "webhookHideIdentity") != true {
		t.Errorf("negative: a signed-out post changes nobody: %d", res.StatusCode)
	}
}

// A board's Notifications page, WeKan's Board Settings / Notifications: its
// admins set the board's e-mail, tray and webhooks; a normal member neither
// sees the page nor saves a setting.
func TestBoardDeliveryPage(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Notify")
	setMember(t, w, rb.ID, Member{UserID: bob.ID, IsActive: true})
	var b Board
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": rb.ID}).Decode(&b)
	ac, bc := w.client(t), w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	body(t, w.signIn(t, bc, "bob", "pw"))
	pagePath := w.srv.URL + "/b/" + rb.ID + "/" + b.Slug + "/notifications"
	page := getBody(t, ac, pagePath)
	for _, want := range []string{`data-channel="email"`, `data-channel="tray"`, `data-channel="webhook"`, `name="fields"`, `name="text"`, `js-board-delivery`} {
		if !strings.Contains(page, want) {
			t.Errorf("the board's page has %s", want)
		}
	}
	if !strings.Contains(getBody(t, ac, w.srv.URL+"/b/"+rb.ID+"/"+b.Slug), "js-open-board-notifications") {
		t.Error("the board menu links it for the board's admin")
	}
	stored := func(keys ...string) any {
		var doc bson.M
		_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": rb.ID}).Decode(&doc)
		return plain(nestedField(doc, keys...))
	}
	post := func(c *http.Client, form url.Values) *http.Response {
		res, err := c.PostForm(w.srv.URL+"/b/"+rb.ID+"/notifications", form)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res
	}
	res := post(ac, url.Values{"channel": {"webhook"}, "text": {"false"}, "fields": {"ids", "links"}, "grouping": {"card"}})
	if !strings.HasSuffix(res.Header.Get("Location"), "/notifications?saved=notifications") {
		t.Errorf("the webhook form saves: %s", res.Header.Get("Location"))
	}
	if m, _ := stored("notificationDelivery", "webhook").(map[string]any); m["text"] != false || m["grouping"] != "card" || len(m["fields"].([]any)) != 2 {
		t.Errorf("the board's webhooks as chosen: %v", m)
	}
	post(ac, url.Values{"channel": {"email"}, "grouping": {"card"}, "partsDefault": {"true"}})
	if d := w.s.emailDeliveryFor(ctx, bob.ID, rb.ID); d.Grouping != "card" {
		t.Errorf("a member without their own setting gets the board's: %+v", d)
	}
	page = getBody(t, ac, pagePath)
	if !strings.Contains(page, `<option value="card" selected`) {
		t.Error("the page shows what is set")
	}
	// Negative: a normal member.
	if res, _ := get(t, bc, pagePath); res.StatusCode != http.StatusNotFound {
		t.Errorf("negative: a normal member does not get the page: %d", res.StatusCode)
	}
	if strings.Contains(getBody(t, bc, w.srv.URL+"/b/"+rb.ID+"/"+b.Slug), "js-open-board-notifications") {
		t.Error("negative: nor the menu's link")
	}
	if res := post(bc, url.Values{"channel": {"email"}, "grouping": {"none"}, "partsDefault": {"true"}}); res.StatusCode != http.StatusForbidden {
		t.Errorf("negative: a normal member's post is 403: %d", res.StatusCode)
	}
	if stored("notificationDelivery", "email", "grouping") != "card" {
		t.Error("negative: and changes nothing")
	}
	// Negative: what the form never offers.
	if res := post(ac, url.Values{"channel": {"webhook"}, "text": {"maybe"}, "fieldsDefault": {"true"}}); !strings.Contains(res.Header.Get("Location"), "error=notification-delivery-invalid") {
		t.Errorf("negative: a text that is not yes or no: %s", res.Header.Get("Location"))
	}
	if res := post(ac, url.Values{"channel": {"webhook"}, "fields": {"secrets"}}); !strings.Contains(res.Header.Get("Location"), "error=notification-delivery-invalid") {
		t.Errorf("negative: a field group that is not one: %s", res.Header.Get("Location"))
	}
	if m, _ := stored("notificationDelivery", "webhook").(map[string]any); m["text"] != false {
		t.Errorf("negative: a refused form writes nothing: %v", m)
	}
}

func getBody(t *testing.T, c *http.Client, u string) string {
	t.Helper()
	res, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	return body(t, res)
}
