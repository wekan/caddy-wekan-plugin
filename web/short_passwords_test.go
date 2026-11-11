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

func TestShortPasswordSignIns(t *testing.T) {
	t.Setenv("PASSWORD_MIN_LENGTH_ENFORCE", "")
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "boss", "boss-password-1")
	boss := User{}
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"username": "boss"}).Decode(&boss)
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": boss.ID}, bson.M{"$set": bson.M{"isAdmin": true}})
	ann := w.seedUser(t, "ann", "short7x")
	w.seedUser(t, "longer", "a-long-password-1")
	dir := w.seedUser(t, "dir", "short7y")
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": dir.ID}, bson.M{"$set": bson.M{"authenticationMethod": "ldap"}})
	row := func(id string) bson.M {
		var r bson.M
		_ = w.db.Collection("shortPasswordLogins").FindOne(ctx, bson.M{"userId": id}).Decode(&r)
		return r
	}
	required := func(id string) bool {
		var d struct {
			R bool `bson:"passwordChangeRequired"`
		}
		_ = w.db.Collection("users").FindOne(ctx, bson.M{"_id": id}).Decode(&d)
		return d.R
	}

	// Not enforced (the default): reported, counted, never required.
	c1 := w.client(t)
	if res := w.signIn(t, c1, "ann", "short7x"); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("a short password still signs in: %d", res.StatusCode)
	}
	body(t, w.signIn(t, w.client(t), "ann", "short7x"))
	if r := row(ann.ID); r["count"] != int32(2) || r["username"] != "ann" || r["firstAt"] == nil || r["fixedAt"] != nil {
		t.Errorf("one row, two sign-ins: %v", r)
	}
	if required(ann.ID) {
		t.Error("negative: not enforced, nothing is required")
	}
	if res, _ := get(t, c1, w.srv.URL+"/"); res.StatusCode != 200 {
		t.Error("not enforced, the app is open")
	}
	body(t, w.signIn(t, w.client(t), "longer", "a-long-password-1"))
	body(t, w.signIn(t, w.client(t), "dir", "short7y"))
	var lg User
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"username": "longer"}).Decode(&lg)
	if row(lg.ID) != nil || row(dir.ID) != nil {
		t.Error("negative: a long password, or a directory's, is not reported")
	}
	// The REST login measures the password too.
	var out map[string]any
	call(t, "POST", w.srv.URL+"/users/login", "", `{"username":"ann","password":"short7x"}`, &out)
	if row(ann.ID)["count"] != int32(3) {
		t.Errorf("the REST login: %v", row(ann.ID))
	}

	// Enforced: the change before anything else.
	t.Setenv("PASSWORD_MIN_LENGTH_ENFORCE", "true")
	c2 := w.client(t)
	body(t, w.signIn(t, c2, "ann", "short7x"))
	if !required(ann.ID) {
		t.Fatal("enforced, the change is required")
	}
	if res, _ := get(t, c2, w.srv.URL+"/"); res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/profile" {
		t.Errorf("every page leads to the change: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	if _, html := get(t, c2, w.srv.URL+"/profile"); !strings.Contains(html, "js-password-change-required") || !strings.Contains(html, "at least 8 characters") {
		t.Error("the profile says what is required")
	}
	req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/setLanguage", strings.NewReader(`["fi"]`))
	req.Header.Set("Content-Type", "application/json")
	if res, _ := c2.Do(req); res == nil || res.StatusCode != http.StatusOK {
		// methods are not pages: WeKan's requirement is the pages' too
		t.Logf("a method: %v", res)
	}
	res, _ := post(t, c2, w.srv.URL+"/profile", url.Values{"fullname": {"Ann"}})
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("negative: another action before the change: %d", res.StatusCode)
	}
	// Negative: another short one is refused while enforced.
	res, _ = post(t, c2, w.srv.URL+"/profile/password", url.Values{"oldPassword": {"short7x"}, "newPassword": {"short8z"}, "newPassword2": {"short8z"}})
	if loc := res.Header.Get("Location"); !strings.Contains(loc, "error=password-too-short") {
		t.Errorf("negative: a short new password: %s", loc)
	}
	other := w.client(t)
	body(t, w.signIn(t, other, "ann", "short7x"))
	res, _ = post(t, c2, w.srv.URL+"/profile/password", url.Values{"oldPassword": {"short7x"}, "newPassword": {"a-long-new-one-1"}, "newPassword2": {"a-long-new-one-1"}})
	if res.Header.Get("Location") != "/sign-in" {
		t.Fatalf("the change: %s", res.Header.Get("Location"))
	}
	for name, c := range map[string]*http.Client{"this": c2, "another": other} {
		if res, _ := get(t, c, w.srv.URL+"/"); res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/sign-in" {
			t.Errorf("%s session signed out: %d %s", name, res.StatusCode, res.Header.Get("Location"))
		}
	}
	if required(ann.ID) || row(ann.ID)["fixedAt"] == nil {
		t.Errorf("the requirement ends and the row is fixed: %v", row(ann.ID))
	}
	c3 := w.client(t)
	body(t, w.signIn(t, c3, "ann", "a-long-new-one-1"))
	if res, _ := get(t, c3, w.srv.URL+"/"); res.StatusCode != 200 {
		t.Error("the new password opens the app")
	}

	// The table: site administrators only, by filter.
	bc := w.client(t)
	body(t, w.signIn(t, bc, "boss", "boss-password-1"))
	page := func(c *http.Client, args string) map[string]any {
		req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/shortPasswordLoginsPage", strings.NewReader(args))
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
	if r, _ := page(bc, `[{"filter":"fixed"}]`)["result"].(map[string]any); r == nil || r["total"] != 1.0 {
		t.Errorf("the fixed rows: %v", r)
	}
	if r, _ := page(bc, `[{}]`)["result"].(map[string]any); r == nil || r["total"] != 0.0 {
		t.Errorf("negative: no open row: %v", r)
	}
	if r, _ := page(bc, `[{"filter":"all","search":"AN"}]`)["result"].(map[string]any); r == nil || r["total"] != 1.0 {
		t.Errorf("searched: %v", r)
	}
	if out := page(c3, `[{}]`); out["error"] != "not-authorized" {
		t.Errorf("negative: not a site admin: %v", out)
	}
}

// Admin Panel / Problems / Password length: the settings, saved over the
// environment, and the table - a site admin's page, nobody else's.
func TestPasswordLengthPage(t *testing.T) {
	t.Setenv("PASSWORD_MIN_LENGTH_ENFORCE", "")
	t.Setenv("PASSWORD_MIN_LENGTH", "")
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "boss", "boss-password-1")
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"username": "boss"}, bson.M{"$set": bson.M{"isAdmin": true}})
	w.seedUser(t, "ann", "short7x")
	body(t, w.signIn(t, w.client(t), "ann", "short7x"))
	bc := w.client(t)
	body(t, w.signIn(t, bc, "boss", "boss-password-1"))
	page := w.srv.URL + "/admin/problems/password-length"
	if _, html := get(t, bc, page); !strings.Contains(html, "js-password-length-row") || !strings.Contains(html, ">ann<") ||
		!strings.Contains(html, `value="8"`) || strings.Contains(html, "js-password-min-length-enforce\" type=\"checkbox\" name=\"enforce\" value=\"true\" checked") {
		t.Fatalf("the page: the row, the default minimum, not enforced")
	}
	if _, html := get(t, bc, w.srv.URL+"/admin/problems/recovery"); !strings.Contains(html, `href="/admin/problems/password-length"`) {
		t.Error("Problems' menu leads to it")
	}
	if _, html := get(t, bc, page+"?filter=fixed"); strings.Contains(html, "js-password-length-row") {
		t.Error("negative: nothing fixed yet")
	}
	if _, html := get(t, bc, page+"?filter=all&q=nobody"); strings.Contains(html, "js-password-length-row") {
		t.Error("negative: a search for nobody")
	}
	// Save: the minimum and enforcing, as Admin Panel's login settings.
	res, _ := post(t, bc, page, url.Values{"minLength": {"10"}, "enforce": {"true"}})
	if !strings.HasSuffix(res.Header.Get("Location"), "?saved=1") {
		t.Fatalf("saved: %s", res.Header.Get("Location"))
	}
	if p := w.s.passwordPolicy(ctx); p.MinLength != 10 || !p.Enforce {
		t.Errorf("the policy in force: %+v", p)
	}
	for _, bad := range []string{"0", "257", "ten"} {
		res, _ := post(t, bc, page, url.Values{"minLength": {bad}})
		if !strings.Contains(res.Header.Get("Location"), "error=") {
			t.Errorf("negative: %q saved", bad)
		}
	}
	if p := w.s.passwordPolicy(ctx); p.MinLength != 10 {
		t.Errorf("negative: a refused value changed it: %+v", p)
	}
	// Empty takes the environment's back.
	t.Setenv("PASSWORD_MIN_LENGTH", "9")
	post(t, bc, page, url.Values{"minLength": {""}})
	if p := w.s.passwordPolicy(ctx); p.MinLength != 9 || p.Enforce {
		t.Errorf("back to the environment, unenforced: %+v", p)
	}
	if _, html := get(t, bc, page); !strings.Contains(html, "set in the server") {
		t.Error("the page says the environment is in effect")
	}
	// Negative: not a site admin.
	ac := w.client(t)
	body(t, w.signIn(t, ac, "ann", "short7x"))
	if res, _ := get(t, ac, page); res.StatusCode != http.StatusNotFound {
		t.Errorf("negative: not a site admin reads it: %d", res.StatusCode)
	}
	if res, _ := post(t, ac, page, url.Values{"minLength": {"1"}}); res.StatusCode != http.StatusNotFound {
		t.Errorf("negative: not a site admin saves it: %d", res.StatusCode)
	}
}
