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

func TestCustomFieldMethods(t *testing.T) {
	w := newTestWeKan(t)
	ann := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	w.seedUser(t, "eve", "pw")
	a := login(t, w, "ann")
	b := newRESTBoard(t, w, a, "Fields")
	setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: true})
	ctx := context.Background()
	for _, f := range []bson.M{{"_id": "zerofield0000001", "name": "ZeroField", "type": "currency"}, {"_id": "falsefield000001", "name": "FalseField", "type": "checkbox"},
		{"_id": "adminfield000001", "name": "Secret", "type": "checkbox", "adminOnly": true}} {
		f["boardIds"] = bson.A{b.ID} // inserted, not a filter
		_, _ = w.db.Collection("customFields").InsertOne(ctx, f)
	}
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": b.Card}, bson.M{"$set": bson.M{"customFields": bson.A{
		bson.M{"_id": "zerofield0000001", "value": 1}, bson.M{"_id": "falsefield000001", "value": true}, bson.M{"_id": "adminfield000001", "value": true}}}})
	client := func(name string) func(method, args string) map[string]any {
		c := w.client(t)
		body(t, w.signIn(t, c, name, "pw"))
		return func(method, args string) map[string]any {
			req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/"+method, strings.NewReader(args))
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
	asBob, asEve := client("bob"), client("eve")
	value := func(id string) any {
		var c bson.M
		_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": b.Card}).Decode(&c)
		for _, f := range c["customFields"].(bson.A) {
			if nestedField(f, "_id") == id {
				return nestedField(f, "value")
			}
		}
		return "absent"
	}
	if out := asBob("setCardCustomFieldCurrency", `["`+b.Card+`","zerofield0000001",0]`); out["error"] != nil || value("zerofield0000001") != 0.0 {
		t.Fatalf("currency 0: %v %v", out, value("zerofield0000001"))
	}
	if out := asBob("setCardCustomFieldCheckbox", `["`+b.Card+`","falsefield000001",false]`); out["error"] != nil || value("falsefield000001") != false {
		t.Fatalf("checkbox false: %v", out)
	}
	// The activity's sentence keeps 0 and false, for the drawer and the mail.
	var act bson.M
	_ = w.db.Collection("activities").FindOne(ctx, bson.M{"activityType": "setCustomField", "customFieldId": "zerofield0000001"}).Decode(&act)
	if html, ok := w.s.notificationHTML(ctx, ann, act, "en"); !ok || !strings.Contains(string(html), "&#34;ZeroField&#34;</b>: <b>&#34;0&#34;") {
		t.Errorf("the sentence says 0: %s", html)
	}
	_ = w.db.Collection("activities").FindOne(ctx, bson.M{"activityType": "setCustomField", "customFieldId": "falsefield000001"}).Decode(&act)
	if html, _ := w.s.notificationHTML(ctx, ann, act, "en"); !strings.Contains(string(html), "&#34;false&#34;") {
		t.Errorf("the sentence says false: %s", html)
	}
	if out := asBob("setCardCustomFieldAssigned", `["`+b.Card+`","zerofield0000001",false]`); out["error"] != nil || value("zerofield0000001") != "absent" {
		t.Errorf("unassigned: %v", out)
	}
	for name, c := range map[string]struct {
		call  func(string, string) map[string]any
		m, a  string
		error string
	}{
		"a non-member":    {asEve, "setCardCustomFieldCheckbox", `["` + b.Card + `","falsefield000001",true]`, "not-authorized"},
		"the wrong type":  {asBob, "setCardCustomFieldCurrency", `["` + b.Card + `","falsefield000001",1]`, "custom-field-not-found"},
		"not on the card": {asBob, "setCardCustomFieldCurrency", `["` + b.Card + `","zerofield0000001",1]`, "custom-field-not-on-card"},
		"not a number":    {asBob, "setCardCustomFieldCurrency", `["` + b.Card + `","zerofield0000001","1"]`, "Match.Error"},
		"an unknown card": {asBob, "setCardCustomFieldCheckbox", `["nosuchcard000001","falsefield000001",false]`, "not-found"},
	} {
		if out := c.call(c.m, c.a); out["error"] != c.error {
			t.Errorf("negative: %s: %v", name, out)
		}
	}
	// Last: AdminFieldBleed is high, and - as in WeKan - disables the account.
	if out := asBob("setCardCustomFieldCheckbox", `["`+b.Card+`","adminfield000001",false]`); out["error"] != "not-authorized" {
		t.Errorf("negative: an admin-only field: %v", out)
	}
	if value("adminfield000001") != true {
		t.Error("negative: the admin-only value is unchanged")
	}
	if n, _ := w.db.Collection("eventlog").CountDocuments(ctx, bson.M{"bleed": "AdminFieldBleed"}); n != 1 {
		t.Errorf("the refusal is in Problems: %d", n)
	}
	var u User
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"_id": bob.ID}).Decode(&u)
	if !u.LoginDisabled {
		t.Error("an AdminFieldBleed attempt disables the account, as WeKan's catalog says")
	}
}

// clients cannot create or alter private email jobs: a collection write
// Weddy has no method for is Meteor's numeric 403, whatever the collection.
func TestCollectionWritesAreDenied(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	c := w.client(t)
	body(t, w.signIn(t, c, "ann", "pw"))
	ctx := context.Background()
	for _, coll := range []string{"notificationEmailJobs", "notificationEmailLeases", "activityNotificationIntents", "nosuchcollection"} {
		for _, op := range []string{"insert", "update", "remove", "upsert"} {
			req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/"+url.PathEscape("/"+coll+"/"+op), strings.NewReader(`[{"_id":"privatejob000001","state":"sent"}]`))
			req.Header.Set("Content-Type", "application/json")
			res, err := c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			var out map[string]any
			_ = json.NewDecoder(res.Body).Decode(&out)
			res.Body.Close()
			if out["error"] != 403.0 {
				t.Errorf("negative: /%s/%s: %v", coll, op, out)
			}
		}
		if n, _ := w.db.Collection(coll).CountDocuments(ctx, bson.M{"_id": "privatejob000001"}); n != 0 {
			t.Errorf("negative: nothing is written to %s", coll)
		}
	}
}
