package web

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// /settings/update, WeKan's global-date-format spec's collection writes in
// Go: a site admin sets the date settings; a non-admin is refused, and so is
// an unsupported format, a wrong type, the required field unset, an object
// field (the mail server has its own methods), an unknown one and another
// operator.
func TestSettingsCollectionUpdate(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	root := w.seedUser(t, "root", "pw")
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": root.ID}, bson.M{"$set": bson.M{"isAdmin": true}})
	w.seedUser(t, "ann", "pw")
	_, _ = w.db.Collection("settings").InsertOne(ctx, bson.M{"_id": "settings01", "defaultAuthenticationMethod": "password", "globalDateFormat": "YYYY-MM-DD"})
	rc := w.client(t)
	body(t, w.signIn(t, rc, "root", "pw"))
	update := func(cl *http.Client, mod string) map[string]any {
		_, out := methodCall(t, w, cl, "/settings/update", `[{"_id":"settings01"},`+mod+`]`)
		return out
	}
	setting := func() bson.M {
		var d bson.M
		_ = w.db.Collection("settings").FindOne(ctx, bson.M{"_id": "settings01"}).Decode(&d)
		return d
	}
	if out := update(rc, `{"$set":{"hideDateFormat":false,"globalDateFormat":"DD-MM-YYYY"}}`); out["result"] != float64(1) {
		t.Fatalf("an admin's update: %v", out)
	}
	if d := setting(); d["globalDateFormat"] != "DD-MM-YYYY" || d["hideDateFormat"] != false {
		t.Errorf("written: %v", d)
	}
	for name, mod := range map[string]string{
		"an unsupported format": `{"$set":{"globalDateFormat":"invalid"}}`,
		"a wrong type":          `{"$set":{"hideLogo":"yes"}}`,
		"the required unset":    `{"$unset":{"defaultAuthenticationMethod":""}}`,
		"the mail server":       `{"$set":{"mailServer":{"host":"evil.example"}}}`,
		"a mail server field":   `{"$set":{"mailServer.host":"evil.example"}}`,
		"an unknown field":      `{"$set":{"isAdmin":true}}`,
		"another operator":      `{"$push":{"webhookInternalHosts":"10.0.0.1"}}`,
	} {
		if out := update(rc, mod); out["error"] == nil {
			t.Errorf("negative: %s: %v", name, out)
		}
	}
	if d := setting(); d["globalDateFormat"] != "DD-MM-YYYY" || d["mailServer"] != nil || d["hideLogo"] != nil {
		t.Errorf("nothing refused written: %v", d)
	}
	ac := w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	if out := update(ac, `{"$set":{"hideDateFormat":true}}`); out["reason"] != "Access denied" {
		t.Errorf("negative: not a site admin: %v", out)
	}
	if setting()["hideDateFormat"] != false {
		t.Error("the non-admin wrote nothing")
	}
}

// settingsScalarFields is WeKan's Settings schema's scalar fields: when the
// plugin is checked out beside WeKan (its .tools/w/), the table matches the
// schema field for field, kind for kind and allowed value for allowed value.
func TestSettingsFieldsMatchWeKan(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "models", "settings.js"))
	if err != nil {
		t.Skip("WeKan's models/settings.js is not beside the plugin")
	}
	s := string(src)
	s = s[strings.Index(s, "attachSchema"):]
	s = s[:strings.Index(s, "\n  }),\n")]
	field := regexp.MustCompile(`(?s)\n    '?([A-Za-z0-9_.$]+)'?: \{\n\s+type: ([A-Za-z\[\]]+),(.*?)\n    \}`)
	values := regexp.MustCompile(`(?s)allowedValues: \[([^\]]*)\]`)
	quoted := regexp.MustCompile(`'([^']*)'`)
	want := map[string][]string{}
	for _, m := range field.FindAllStringSubmatch(s, -1) {
		name, typ, extra := m[1], m[2], m[3]
		if strings.Contains(name, ".") || (typ != "Boolean" && typ != "String") {
			continue
		}
		kind := []string{"string"}
		if typ == "Boolean" {
			kind = []string{"bool"}
		}
		if v := values.FindStringSubmatch(extra); v != nil {
			for _, q := range quoted.FindAllStringSubmatch(v[1], -1) {
				kind = append(kind, q[1])
			}
		}
		want[name] = kind
	}
	if len(want) < 50 {
		t.Fatalf("the schema was not read: %d fields", len(want))
	}
	for name, kind := range want {
		if got := settingsScalarFields[name]; !slices.Equal(got, kind) {
			t.Errorf("%s: WeKan's %v, Weddy's %v", name, kind, got)
		}
	}
	for name := range settingsScalarFields {
		if _, ok := want[name]; !ok {
			t.Errorf("%s is no scalar field of WeKan's schema any more", name)
		}
	}
}
