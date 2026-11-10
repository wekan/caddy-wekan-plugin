package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The card details' custom fields: shown to whoever may change the card,
// in their order, an admin-only one only to a board admin; put on and taken
// off from the picker; each type's value saved as WeKan's editors save it -
// and refused for the wrong item, an admin-only field, a field not on the
// card or of another board, and a board without custom fields.
func TestCardDetailsCustomFields(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	ro := w.seedUser(t, "rob", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Fields")
	other, _ := w.s.CreateBoard(ctx, u, "Other")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$push": bson.M{"members": bson.M{"$each": []any{
		bson.M{"userId": bob.ID, "isActive": true}, bson.M{"userId": ro.ID, "isActive": true, "isReadOnly": true}}}}})
	l, _ := w.s.AddList(ctx, b, "L")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	c, _ := w.s.AddCard(ctx, b, u, l.ID, sw.ID, "C")
	field := func(id, name, typ string, sort int, extra bson.M) {
		d := bson.M{"_id": id, "boardIds": bson.A{b.ID}, "name": name, "type": typ, "settings": bson.M{}, "sort": sort}
		for k, v := range extra {
			d[k] = v
		}
		_, _ = w.db.Collection("customFields").InsertOne(ctx, d)
	}
	field("ftext", "Notes", "text", 2, nil)
	field("fnum", "Points", "number", 1, nil)
	field("fbox", "Approved", "checkbox", 3, nil)
	field("fcur", "Budget", "currency", 4, bson.M{"settings": bson.M{"currencyCode": "EUR"}})
	field("fdate", "Deadline", "date", 5, nil)
	field("fdrop", "Size", "dropdown", 6, bson.M{"settings": bson.M{"dropdownItems": bson.A{bson.M{"_id": "small", "name": "Small"}, bson.M{"_id": "big", "name": "Big"}}}})
	field("fsecret", "Secret", "text", 7, bson.M{"adminOnly": true})
	_, _ = w.db.Collection("customFields").InsertOne(ctx, bson.M{"_id": "felse", "boardIds": bson.A{other.ID}, "name": "Elsewhere", "type": "text"})
	cardURL := w.srv.URL + "/b/" + b.ID + "/" + b.Slug + "/" + c.ID
	base := w.srv.URL + "/b/" + b.ID + "/cards/" + c.ID + "/custom-fields/"
	post := func(cl *http.Client, p string, v url.Values) int {
		r, err := cl.PostForm(base+p, v)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	value := func(id string) any {
		var d bson.M
		_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": c.ID}).Decode(&d)
		if a, ok := d["customFields"].(bson.A); ok {
			for _, f := range a {
				if nestedField(f, "_id") == id {
					return nestedField(f, "value")
				}
			}
		}
		return "absent"
	}
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	for _, id := range []string{"ftext", "fnum", "fbox", "fcur", "fdate", "fdrop", "fsecret"} {
		if code := post(ann, id+"/toggle", nil); code != http.StatusSeeOther || value(id) != nil {
			t.Fatalf("%s put on: %d %v", id, code, value(id))
		}
	}
	for _, c := range []struct {
		id  string
		v   url.Values
		out any
	}{
		{"ftext", url.Values{"value": {"Hello"}}, "Hello"},
		{"fnum", url.Values{"value": {"3.5"}}, 3.5},
		{"fbox", url.Values{"value": {"true"}}, true},
		{"fcur", url.Values{"value": {"123,45"}}, 123.45},
		{"fdrop", url.Values{"value": {"big"}}, "big"},
		{"fsecret", url.Values{"value": {"Admin's"}}, "Admin's"},
	} {
		if code := post(ann, c.id, c.v); code != http.StatusSeeOther || value(c.id) != c.out {
			t.Errorf("%s: %d %v", c.id, code, value(c.id))
		}
	}
	if code := post(ann, "fdate", url.Values{"at": {"2099-12-31T17:00:00Z"}}); code != http.StatusSeeOther {
		t.Errorf("date: %d", code)
	}
	if d, ok := value("fdate").(time.Time); !ok || d.Year() != 2099 {
		if bd, ok := value("fdate").(bson.DateTime); !ok || bd.Time().Year() != 2099 {
			t.Errorf("date stored: %v", value("fdate"))
		}
	}
	if n, _ := w.db.Collection("activities").CountDocuments(ctx, bson.M{"cardId": c.ID, "activityType": "setCustomField"}); n < 6 {
		t.Errorf("setCustomField for each change: %d", n)
	}
	_, html := get(t, ann, cardURL)
	points, notes := strings.Index(html, ">Points<"), strings.Index(html, ">Notes<")
	if !strings.Contains(html, "card-details-group-custom-fields") || points < 0 || notes < points {
		t.Errorf("the section, in the fields' order: %d %d", points, notes)
	}
	for _, want := range []string{"123.45 EUR", "Big", "js-open-custom-fields-settings", `title="Deadline: `} {
		if !strings.Contains(html, want) {
			t.Errorf("shown: %s", want)
		}
	}
	// Negatives.
	if code := post(ann, "fdrop", url.Values{"value": {"huge"}}); code != http.StatusBadRequest || value("fdrop") != "big" {
		t.Errorf("negative: not one of the items: %d", code)
	}
	if code := post(ann, "fnum", url.Values{"value": {"many"}}); code != http.StatusBadRequest {
		t.Errorf("negative: not a number: %d", code)
	}
	if code := post(ann, "felse", url.Values{"value": {"x"}}); code != http.StatusNotFound {
		t.Errorf("negative: another board's field: %d", code)
	}
	bobc := w.client(t)
	body(t, w.signIn(t, bobc, "bob", "pw"))
	// Seen before the attempt below: a member who attempts an admin-only
	// field is signed out, as WeKan disables an account that tries a hole.
	if _, html := get(t, bobc, cardURL); strings.Contains(html, ">Secret<") || !strings.Contains(html, ">Notes<") {
		t.Error("negative: a member does not see an admin-only field")
	}
	if code := post(bobc, "fsecret", url.Values{"value": {"Bob's"}}); code != http.StatusForbidden || value("fsecret") != "Admin's" {
		t.Errorf("negative: a member sets no admin-only field: %d", code)
	}
	if n, _ := w.db.Collection("eventlog").CountDocuments(ctx, bson.M{"bleed": "AdminFieldBleed", "source": "cardDetails:customField"}); n != 1 {
		t.Errorf("the refusal in Admin Panel -> Problems: %d", n)
	}
	rob := w.client(t)
	body(t, w.signIn(t, rob, "rob", "pw"))
	if _, html := get(t, rob, cardURL); strings.Contains(html, "card-details-group-custom-fields") {
		t.Error("negative: no custom fields section for a read-only member")
	}
	if code := post(rob, "ftext", url.Values{"value": {"Reader's"}}); code != http.StatusForbidden {
		t.Errorf("negative: a read-only member: %d", code)
	}
	if code := post(ann, "ftext/toggle", nil); code != http.StatusSeeOther || value("ftext") != "absent" {
		t.Errorf("taken off: %d", code)
	}
	if code := post(ann, "ftext", url.Values{"value": {"x"}}); code != http.StatusNotFound {
		t.Errorf("negative: a field not on the card: %d", code)
	}
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"allowsCustomFields": false}})
	if code := post(ann, "fnum", url.Values{"value": {"1"}}); code != http.StatusForbidden {
		t.Errorf("negative: the board does not use custom fields: %d", code)
	}
}

// createCustomFieldPopup from the card's picker: a field of the board by
// whoever may change it, with WeKan's createCustomField activity; refused
// for a read-only member, an unknown type and an empty name.
func TestCreateCustomFieldFromCard(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	ro := w.seedUser(t, "rob", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Create")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$push": bson.M{"members": bson.M{"userId": ro.ID, "isActive": true, "isReadOnly": true}}})
	path := w.srv.URL + "/b/" + b.ID + "/custom-fields"
	post := func(cl *http.Client, v url.Values) int {
		r, err := cl.PostForm(path, v)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	if code := post(ann, url.Values{"name": {" Points "}, "type": {"number"}}); code != http.StatusSeeOther {
		t.Fatalf("created: %d", code)
	}
	var f bson.M
	if w.db.Collection("customFields").FindOne(ctx, bson.M{"name": "Points", "boardIds": b.ID, "type": "number"}).Decode(&f) != nil || f["showOnCard"] != true {
		t.Fatalf("the field: %v", f)
	}
	if n, _ := w.db.Collection("activities").CountDocuments(ctx, bson.M{"activityType": "createCustomField", "customFieldId": f["_id"]}); n != 1 {
		t.Error("createCustomField")
	}
	for _, bad := range []url.Values{{"name": {"X"}, "type": {"script"}}, {"name": {"  "}, "type": {"text"}}} {
		if code := post(ann, bad); code != http.StatusBadRequest {
			t.Errorf("negative: %v: %d", bad, code)
		}
	}
	rob := w.client(t)
	body(t, w.signIn(t, rob, "rob", "pw"))
	if code := post(rob, url.Values{"name": {"Reader's"}, "type": {"text"}}); code != http.StatusForbidden {
		t.Errorf("negative: a read-only member: %d", code)
	}
}

// A multi-select's items and a string template's items, as WeKan's editors
// save them, a string template shown through its format; and the member's
// own layout of the fields (toggleCustomFieldsGrid).
func TestCardCustomFieldsMultiAndTemplate(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	w.seedUser(t, "bob", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "More")
	l, _ := w.s.AddList(ctx, b, "L")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	c, _ := w.s.AddCard(ctx, b, u, l.ID, sw.ID, "Card one")
	_, _ = w.db.Collection("customFields").InsertMany(ctx, []any{
		bson.M{"_id": "fmulti", "boardIds": bson.A{b.ID}, "name": "Tags", "type": "dropdownMultiSelect",
			"settings": bson.M{"dropdownItems": bson.A{bson.M{"_id": "red", "name": "Red"}, bson.M{"_id": "blue", "name": "Blue"}}}},
		bson.M{"_id": "ftpl", "boardIds": bson.A{b.ID}, "name": "Links", "type": "stringtemplate",
			"settings": bson.M{"stringtemplateFormat": "[%{value}]", "stringtemplateSeparator": " + "}},
	})
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": c.ID}, bson.M{"$set": bson.M{"customFields": bson.A{
		bson.M{"_id": "fmulti", "value": nil}, bson.M{"_id": "ftpl", "value": nil}}}})
	base := w.srv.URL + "/b/" + b.ID + "/cards/" + c.ID + "/custom-fields/"
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	post := func(p string, v url.Values) int {
		r, err := ann.PostForm(base+p, v)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	value := func(id string) any {
		var d bson.M
		_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": c.ID}).Decode(&d)
		for _, f := range d["customFields"].(bson.A) {
			if nestedField(f, "_id") == id {
				return nestedField(f, "value")
			}
		}
		return "absent"
	}
	if code := post("fmulti", url.Values{"value": {"", "blue", "red", "blue"}}); code != http.StatusSeeOther {
		t.Fatalf("multi: %d", code)
	}
	if got, ok := value("fmulti").(bson.A); !ok || len(got) != 2 || got[0] != "blue" || got[1] != "red" {
		t.Errorf("the items ticked, once each: %v", value("fmulti"))
	}
	if code := post("ftpl", url.Values{"value": {"one", "  ", "two", ""}}); code != http.StatusSeeOther {
		t.Fatalf("template: %d", code)
	}
	if got, ok := value("ftpl").(bson.A); !ok || len(got) != 2 || got[1] != "two" {
		t.Errorf("the items written, blanks dropped: %v", value("ftpl"))
	}
	_, html := get(t, ann, w.srv.URL+"/b/"+b.ID+"/"+b.Slug+"/"+c.ID)
	for _, want := range []string{"Blue, Red", "[one] &#43; [two]", "custom-fields-grid", "toggleCustomFieldsGridButton"} {
		if !strings.Contains(html, want) {
			t.Errorf("shown: %s", want)
		}
	}
	// Negatives: an item the field does not have; nothing ticked clears it.
	if code := post("fmulti", url.Values{"value": {"green"}}); code != http.StatusBadRequest {
		t.Errorf("negative: not one of the items: %d", code)
	}
	if code := post("fmulti", url.Values{"value": {""}}); code != http.StatusSeeOther || value("fmulti") != nil {
		t.Errorf("nothing ticked: %v", value("fmulti"))
	}
	// The layout: one per row after the toggle, for this member only.
	req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/toggleCustomFieldsGrid", strings.NewReader(`[]`))
	req.Header.Set("Content-Type", "application/json")
	if res, err := ann.Do(req); err != nil || res.StatusCode != 200 {
		t.Fatal("toggleCustomFieldsGrid")
	}
	if _, html := get(t, ann, w.srv.URL+"/b/"+b.ID+"/"+b.Slug+"/"+c.ID); !strings.Contains(html, "custom-fields-one-per-row") {
		t.Error("one per row after the toggle")
	}
}
