package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

type fieldFixture struct {
	rb               restBoard
	secret, ordinary string
	ro               string
}

// fieldSetup is WeKan's spec's setup: an admin-only field, a public one and a
// read-only one on a card of ann's board, bob a member.
func fieldSetup(t *testing.T, w *testWeKan) (fieldFixture, *User) {
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	f := fieldFixture{rb: newRESTBoard(t, w, login(t, w, "ann"), "Fields"), secret: "secretField1", ordinary: "ordinaryFld1", ro: "readOnlyFld1"}
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": f.rb.ID}, bson.M{"$push": bson.M{"members": bson.M{"userId": bob.ID, "isActive": true, "isAdmin": false}}})
	for _, d := range []bson.M{
		{"_id": f.secret, "name": "Secret budget", "adminOnly": true},
		{"_id": f.ordinary, "name": "Public note", "adminOnly": false},
		{"_id": f.ro, "name": "Fixed", "readOnly": true},
	} {
		d["boardIds"], d["type"], d["settings"] = bson.A{f.rb.ID}, "text", bson.M{}
		_, _ = w.db.Collection("customFields").InsertOne(ctx, d)
	}
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": f.rb.Card}, bson.M{"$set": bson.M{"customFields": bson.A{
		bson.M{"_id": f.secret, "value": "CLASSIFIED-9999"}, bson.M{"_id": f.ordinary, "value": "VISIBLE"}, bson.M{"_id": f.ro, "value": "FIXED"}}}})
	return f, bob
}

func cardFieldValue(t *testing.T, w *testWeKan, card, field string) any {
	var c bson.M
	_ = w.db.Collection("cards").FindOne(context.Background(), bson.M{"_id": card}).Decode(&c)
	for _, v := range fieldValues(plain(c).(map[string]any)) {
		if v["_id"] == field {
			return v["value"]
		}
	}
	return nil
}

// AdminFieldBleed, write side, WeKan's admin-only-custom-fields spec in Go:
// a member edits a public value by index, and every way of changing an
// admin-only one - insert, replace, push, unset, copy, REST, the definition
// and a new admin-only definition - is refused and recorded under their name.
func TestAdminFieldBleedWrites(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	f, bob := fieldSetup(t, w)
	bc := w.client(t)
	body(t, w.signIn(t, bc, "bob", "pw"))
	card := `{"_id":"` + f.rb.Card + `"}`
	if _, out := methodCall(t, w, bc, "/cards/update", `[`+card+`,{"$set":{"customFields.1.value":"changed"}}]`); out["result"] != float64(1) {
		t.Fatalf("a member edits a public value: %v", out)
	}
	if cardFieldValue(t, w, f.rb.Card, f.ordinary) != "changed" {
		t.Fatal("the public value is changed")
	}
	forged := `[{"_id":"` + f.secret + `","value":"FORGED"}]`
	for name, attack := range map[string]func() bool{
		"insert": func() bool {
			_, out := methodCall(t, w, bc, "/cards/insert", `[{"title":"forged","boardId":"`+f.rb.ID+`","listId":"`+f.rb.List+`","swimlaneId":"`+f.rb.Swimlane+`","customFields":`+forged+`}]`)
			return out["error"] != nil
		},
		"replace": func() bool {
			_, out := methodCall(t, w, bc, "/cards/update", `[`+card+`,{"$set":{"customFields":`+forged+`}}]`)
			return out["error"] != nil
		},
		"push": func() bool {
			_, out := methodCall(t, w, bc, "/cards/update", `[`+card+`,{"$push":{"customFields":{"_id":"`+f.secret+`","value":"FORGED"}}}]`)
			return out["error"] != nil
		},
		"unset": func() bool {
			_, out := methodCall(t, w, bc, "/cards/update", `[`+card+`,{"$unset":{"customFields":""}}]`)
			return out["error"] != nil
		},
		"by index": func() bool {
			_, out := methodCall(t, w, bc, "/cards/update", `[`+card+`,{"$set":{"customFields.0.value":"FORGED"}}]`)
			return out["error"] != nil
		},
		"copy": func() bool {
			_, out := methodCall(t, w, bc, "copyCard", `["`+f.rb.Card+`","`+f.rb.ID+`","`+f.rb.Swimlane+`","`+f.rb.List+`",false,{"customFields":`+forged+`}]`)
			return out["error"] != nil
		},
		"rest": func() bool {
			req, _ := http.NewRequest("PUT", w.srv.URL+"/api/boards/"+f.rb.ID+"/lists/"+f.rb.List+"/cards/"+f.rb.Card,
				strings.NewReader(`{"customFields":`+forged+`}`))
			req.Header.Set("Authorization", "Bearer "+login(t, w, "bob"))
			req.Header.Set("Content-Type", "application/json")
			res, err := w.srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			return res.StatusCode == http.StatusForbidden
		},
		"definition": func() bool {
			_, out := methodCall(t, w, bc, "/customFields/update", `[{"_id":"`+f.secret+`"},{"$set":{"adminOnly":false}}]`)
			return out["error"] != nil
		},
		"definition-insert": func() bool {
			_, out := methodCall(t, w, bc, "/customFields/insert", `[{"boardIds":["`+f.rb.ID+`"],"name":"Forged","type":"text","settings":{},"adminOnly":true}]`)
			return out["error"] != nil
		},
	} {
		// High severity blocks the account (as WeKan's); each attack is a
		// fresh sign-in, as each is a test of its own in WeKan's spec.
		_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": bob.ID}, bson.M{"$unset": bson.M{"loginDisabled": "", "services.securityBlock": ""}})
		_, _ = w.db.Collection("eventlog").DeleteMany(ctx, bson.M{"bleed": "AdminFieldBleed"})
		bc = w.client(t)
		body(t, w.signIn(t, bc, "bob", "pw"))
		if !attack() {
			t.Errorf("%s refused", name)
		}
		if v := cardFieldValue(t, w, f.rb.Card, f.secret); v != "CLASSIFIED-9999" {
			t.Errorf("%s: the secret value stays: %v", name, v)
		}
		var def bson.M
		_ = w.db.Collection("customFields").FindOne(ctx, bson.M{"_id": f.secret}).Decode(&def)
		if def["adminOnly"] != true {
			t.Errorf("%s: the field stays admin-only", name)
		}
		if n, _ := w.db.Collection("eventlog").CountDocuments(ctx, bson.M{"bleed": "AdminFieldBleed", "action": "blocked", "username": bob.Username}); n == 0 {
			t.Errorf("%s recorded as AdminFieldBleed by bob", name)
		}
	}
	// A read-only field's value is the admins' (#3143).
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": bob.ID}, bson.M{"$unset": bson.M{"loginDisabled": "", "services.securityBlock": ""}})
	bc = w.client(t)
	body(t, w.signIn(t, bc, "bob", "pw"))
	if _, out := methodCall(t, w, bc, "/cards/update", `[`+card+`,{"$set":{"customFields.2.value":"X"}}]`); out["error"] == nil {
		t.Error("negative: a member changes a read-only value")
	}
	ac := w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	for i, v := range []string{"0", "2"} {
		if _, out := methodCall(t, w, ac, "/cards/update", `[`+card+`,{"$set":{"customFields.`+v+`.value":"ADMIN"}}]`); out["result"] != float64(1) {
			t.Errorf("an admin writes field %d: %v", i, out)
		}
	}
	if cardFieldValue(t, w, f.rb.Card, f.secret) != "ADMIN" || cardFieldValue(t, w, f.rb.Card, f.ro) != "ADMIN" {
		t.Error("an admin writes the admin-only and the read-only value")
	}
}

// A member's copy keeps the public values and leaves the protected one out;
// an admin's copy keeps them all.
func TestAdminFieldCopyOmitsProtected(t *testing.T) {
	w := newTestWeKan(t)
	f, _ := fieldSetup(t, w)
	for _, c := range []struct {
		user   string
		secret bool
	}{{"bob", false}, {"ann", true}} {
		cl := w.client(t)
		body(t, w.signIn(t, cl, c.user, "pw"))
		_, out := methodCall(t, w, cl, "copyCard", `["`+f.rb.Card+`","`+f.rb.ID+`","`+f.rb.Swimlane+`","`+f.rb.List+`",false,{}]`)
		id, _ := out["result"].(string)
		if id == "" {
			t.Fatalf("%s copies: %v", c.user, out)
		}
		if cardFieldValue(t, w, id, f.ordinary) != "VISIBLE" {
			t.Errorf("%s's copy keeps the public value", c.user)
		}
		if got := cardFieldValue(t, w, id, f.secret) == "CLASSIFIED-9999"; got != c.secret {
			t.Errorf("%s's copy has the secret: %v", c.user, got)
		}
	}
	if cardFieldValue(t, w, f.rb.Card, f.secret) != "CLASSIFIED-9999" {
		t.Error("the original keeps its value")
	}
}
