package web

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestRESTCustomFieldEditAndDropdownItems(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	a, bt := login(t, w, "ann"), login(t, w, "bob")
	b := newRESTBoard(t, w, a, "Fields")
	ctx := context.Background()
	var f struct {
		ID string `json:"_id"`
	}
	call(t, "POST", w.srv.URL+"/api/boards/"+b.ID+"/custom-fields", a, `{"name":"Size","type":"dropdown","settings":{"$where":"1","ok":1}}`, &f)
	field := w.srv.URL + "/api/boards/" + b.ID + "/custom-fields/" + f.ID
	get := func() bson.M {
		var m bson.M
		_ = w.db.Collection("customFields").FindOne(ctx, bson.M{"_id": f.ID}).Decode(&m)
		return m
	}
	if s, _ := get()["settings"].(bson.D); len(s) != 1 || s[0].Key != "ok" {
		t.Fatalf("negative: settings never keep an operator: %v", get()["settings"])
	}
	if code := call(t, "PUT", field, a, `{"name":"Size2","showOnCard":true,"alwaysOnCard":"true"}`, nil); code != 200 {
		t.Fatalf("edit: %d", code)
	}
	if m := get(); m["name"] != "Size2" || m["showOnCard"] != true || m["alwaysOnCard"] != true {
		t.Fatalf("edited: %v", m)
	}
	for _, bad := range []string{`{"type":"html"}`, `{"name":""}`, `{"settings":"x"}`} {
		if code := call(t, "PUT", field, a, bad, nil); code != 400 {
			t.Fatalf("negative: %s: %d", bad, code)
		}
	}
	if code := call(t, "POST", field+"/dropdown-items", a, `{"items":["S","M",{"$ne":1},"L"]}`, nil); code != 200 {
		t.Fatalf("items: %d", code)
	}
	s := docMap(get()["settings"].(bson.D))
	items, _ := s["dropdownItems"].(bson.A)
	if len(items) != 3 {
		t.Fatalf("the names are added, anything else skipped: %v", items)
	}
	first := docMap(items[0].(bson.D))
	id := first["_id"].(string)
	call(t, "PUT", field+"/dropdown-items/"+id, a, `{"name":"Small"}`, nil)
	items = docMap(get()["settings"].(bson.D))["dropdownItems"].(bson.A)
	if docMap(items[0].(bson.D))["name"] != "Small" {
		t.Fatalf("renamed: %v", items)
	}
	call(t, "DELETE", field+"/dropdown-items/"+id, a, "", nil)
	if items = docMap(get()["settings"].(bson.D))["dropdownItems"].(bson.A); len(items) != 2 {
		t.Fatalf("deleted: %v", items)
	}
	setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: true, IsCommentOnly: true})
	if code := call(t, "PUT", field, bt, `{"name":"x"}`, nil); code != 403 {
		t.Fatalf("negative: a comment-only member: %d", code)
	}
	other := newRESTBoard(t, w, a, "Other")
	if code := call(t, "PUT", w.srv.URL+"/api/boards/"+other.ID+"/custom-fields/"+f.ID, a, `{"name":"x"}`, nil); code != 404 {
		t.Fatalf("negative: a field of another board: %d", code)
	}
}

// The negative test over the whole package: no update uses the positional $
// operator ("a.$"), which the embedded FerretDB does not have - an update with
// it changes nothing there, silently.
func TestNoPositionalOperator(t *testing.T) {
	positional := regexp.MustCompile(`"[A-Za-z][A-Za-z0-9_.]*\.\$(\.|")`)
	if !positional.MatchString(`"settings.dropdownItems.$"`) || positional.MatchString(`strings.ContainsAny(k, ".$")`) {
		t.Fatal("the scan misses the shape it guards, or takes what is not one")
	}
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, _ := os.ReadFile(f)
		for i, line := range strings.Split(string(src), "\n") {
			if positional.MatchString(line) {
				t.Errorf("%s:%d uses the positional $ operator: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}
