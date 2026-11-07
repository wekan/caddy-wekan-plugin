package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestCloudRules(t *testing.T) {
	for _, ok := range []string{"customer.wekan.team", "wekan.customer.com", "a-b.c1.example"} {
		if !ValidDomain(ok) {
			t.Errorf("%s is a domain", ok)
		}
	}
	for _, bad := range []string{"", "localhost", "../etc", "a/b.com", "a..b.com", "-a.com", "UPPER.com", "a b.com"} {
		if ValidDomain(bad) {
			t.Errorf("negative: %q is no domain (it becomes a directory name)", bad)
		}
	}
	if NormalizeHost("https://Cloud.Wekan.Team.:443/x") != "cloud.wekan.team" {
		t.Error("NormalizeHost")
	}
	if d, bad := CleanDomain(Domain{Domain: " Customer.Wekan.Team ", CustomerEmail: "a@b.fi", StartDate: "2026-11-01", EndDate: "2027-10-31"}); bad != "" || d.Domain != "customer.wekan.team" {
		t.Errorf("a good row: %q %+v", bad, d)
	}
	for field, d := range map[string]Domain{
		"domain":        {Domain: "../evil"},
		"customerEmail": {Domain: "a.fi", CustomerEmail: "no"},
		"billingEmail":  {Domain: "a.fi", BillingEmail: "x@"},
		"startDate":     {Domain: "a.fi", StartDate: "2026-02-30"},
		"endDate":       {Domain: "a.fi", StartDate: "2026-02-02", EndDate: "2026-02-01"},
		"customerName":  {Domain: "a.fi", CustomerName: strings.Repeat("x", 201)},
	} {
		if _, bad := CleanDomain(d); bad != field {
			t.Errorf("negative: %s must be refused, got %q", field, bad)
		}
	}
	u := &User{Username: "Lauri", Emails: []Email{{Address: "lauri.ojansivu@wekan.fi"}}}
	if !IsSuperAdmin(u, "lauri") || !IsSuperAdmin(u, " Lauri.Ojansivu@WeKan.fi ") {
		t.Error("the SuperAdmin by username or e-mail, without case")
	}
	if IsSuperAdmin(u, "") || IsSuperAdmin(u, "someone") || IsSuperAdmin(nil, "lauri") {
		t.Error("negative: nobody else, and nobody when it is empty")
	}
	day := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	if !(Domain{EndDate: "2026-11-07"}).Active(day) || (Domain{EndDate: "2026-11-06"}).Active(day) || (Domain{StartDate: "2026-11-08"}).Active(day) {
		t.Error("Active: start and end both included")
	}
}

// do sends a form as a browser at host would.
func do(t *testing.T, c *http.Client, method, u, host string, form url.Values) (*http.Response, string) {
	t.Helper()
	var req *http.Request
	if form != nil {
		req, _ = http.NewRequest(method, u, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req, _ = http.NewRequest(method, u, nil)
	}
	req.Host = host
	// With another Host, the client does not take the jar's cookies itself: the
	// browser of that host would have them, so they are added here.
	nojar := *c
	nojar.Jar = nil
	for _, ck := range c.Jar.Cookies(req.URL) {
		req.AddCookie(ck)
	}
	res, err := nojar.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res, body(t, res)
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

func TestCloudPanel(t *testing.T) {
	w := newTestWeKan(t)
	dir := filepath.Join(t.TempDir(), "cloud")
	w.s.CloudDir = dir
	admin := w.seedUser(t, "siteadmin", "pw")
	_, _ = w.db.Collection("users").UpdateOne(context.Background(), bson.M{"_id": admin.ID}, bson.M{"$set": bson.M{"isAdmin": true}})
	w.seedUser(t, "boss", "pw")
	w.seedUser(t, "member", "pw")
	const primary, panel = "wekan.team", "cloud.wekan.team"
	srv := w.srv.URL

	ac := w.client(t)
	if res := w.signIn(t, ac, "siteadmin", "pw"); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign in: %d %s", res.StatusCode, body(t, res))
	}
	if res, page := do(t, ac, "GET", srv+"/", primary, nil); res.StatusCode != 200 || !strings.Contains(page, "js-cloud-settings") {
		t.Fatalf("a site admin of the primary has Admin Panel / People / Cloud in the header: %d %s", res.StatusCode, page[:min(len(page), 600)])
	}
	res, _ := do(t, ac, "POST", srv+"/admin/people/cloud", primary, url.Values{"cloudPanelDomain": {"Cloud.Wekan.Team"}, "superAdminUsername": {"boss@example.invalid"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("saving the Cloud settings: %d", res.StatusCode)
	}
	var file DomainsFile
	readJSON(t, filepath.Join(dir, "domains.json"), &file)
	if file.CloudPanelDomain != panel {
		t.Fatalf("domains.json: %+v", file)
	}
	if st, _ := os.Stat(filepath.Join(dir, "settings.json")); st == nil || st.Mode().Perm() != 0o600 {
		t.Fatal("settings.json, mode 0600")
	}
	if res, _ := do(t, ac, "POST", srv+"/admin/people/cloud", primary, url.Values{"cloudPanelDomain": {"../evil"}}); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("negative: a Cloud Panel Domain that is no domain: %d", res.StatusCode)
	}

	mc := w.client(t)
	body(t, w.signIn(t, mc, "member", "pw"))
	if res, _ := do(t, mc, "GET", srv+"/admin/people/cloud", primary, nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("negative: a non-admin has no Cloud settings: %d", res.StatusCode)
	}
	if res, _ := do(t, mc, "POST", srv+"/admin/people/cloud", primary, url.Values{"superAdminUsername": {"member"}}); res.StatusCode != http.StatusNotFound {
		t.Fatalf("negative: a non-admin cannot make themself SuperAdmin: %d", res.StatusCode)
	}

	// The SuperAdmin, at the Cloud Panel Domain.
	bc := w.client(t)
	body(t, w.signIn(t, bc, "boss@example.invalid", "pw"))
	if res, page := do(t, bc, "GET", srv+"/cloud", panel, nil); res.StatusCode != 200 || !strings.Contains(page, "js-cloud-domain-form") {
		t.Fatalf("the Cloud Panel: %d", res.StatusCode)
	}
	add := url.Values{"domain": {"Customer.Wekan.Team"}, "customerName": {"Customer Oy"}, "customerEmail": {"it@customer.fi"},
		"billingEmail": {"bill@customer.fi"}, "startDate": {"2026-11-01"}, "endDate": {"2027-10-31"}}
	if res, _ := do(t, bc, "POST", srv+"/cloud/domains", panel, add); res.Header.Get("Location") != "/cloud?saved" {
		t.Fatalf("adding a domain: %s", res.Header.Get("Location"))
	}
	readJSON(t, filepath.Join(dir, "domains.json"), &file)
	if len(file.Domains) != 1 || file.Domains[0].Domain != "customer.wekan.team" || file.Domains[0].EndDate != "2027-10-31" || file.CloudPanelDomain != panel {
		t.Fatalf("domains.json: %+v", file)
	}
	_ = os.WriteFile(filepath.Join(dir, "status.json"), []byte(`{"domains":[{"domain":"customer.wekan.team","tls":"cloudflare-origin"}]}`), 0o600)
	if _, page := do(t, bc, "GET", srv+"/cloud", panel, nil); !strings.Contains(page, "Customer Oy") || !strings.Contains(page, "cloudflare-origin") {
		t.Fatal("the table shows the row and what Weddy did")
	}
	edit := url.Values{"original": {"customer.wekan.team"}, "domain": {"wekan.customer.fi"}, "customerName": {"Customer Oy"}}
	do(t, bc, "POST", srv+"/cloud/domains", panel, edit)
	readJSON(t, filepath.Join(dir, "domains.json"), &file)
	if len(file.Domains) != 1 || file.Domains[0].Domain != "wekan.customer.fi" {
		t.Fatalf("editing renames the row: %+v", file.Domains)
	}
	if res, _ := do(t, bc, "POST", srv+"/cloud/domains", panel, url.Values{"domain": {"x.fi"}, "customerEmail": {"nope"}}); res.Header.Get("Location") != "/cloud?error=customerEmail" {
		t.Fatalf("negative: a bad e-mail address: %s", res.Header.Get("Location"))
	}
	do(t, bc, "POST", srv+"/cloud/domains/delete", panel, url.Values{"domain": {"wekan.customer.fi"}})
	readJSON(t, filepath.Join(dir, "domains.json"), &file)
	if len(file.Domains) != 0 {
		t.Fatalf("deleting: %+v", file.Domains)
	}

	// CloudFlare: written, never shown.
	const token = "cf-secret-token-123"
	do(t, bc, "POST", srv+"/cloud/cloudflare", panel, url.Values{"apiToken": {token}})
	st, err := os.Stat(filepath.Join(dir, "cloudflare.json"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("cloudflare.json, mode 0600: %v", err)
	}
	for _, path := range []string{"/cloud", "/cloud?pane=cloudflare", "/"} {
		if _, page := do(t, bc, "GET", srv+path, panel, nil); strings.Contains(page, token) {
			t.Fatalf("negative: the CloudFlare key was sent to a browser at %s", path)
		}
	}
	if _, page := do(t, bc, "GET", srv+"/cloud?pane=cloudflare", panel, nil); !strings.Contains(page, "js-cloudflare-set") {
		t.Fatal("the pane says a key is saved")
	}
	do(t, bc, "POST", srv+"/cloud/cloudflare", panel, url.Values{"apiToken": {""}})
	if _, err := os.Stat(filepath.Join(dir, "cloudflare.json")); err != nil {
		t.Fatal("an empty field keeps the saved key")
	}
	do(t, bc, "POST", srv+"/cloud/cloudflare", panel, url.Values{"clear": {"1"}})
	if _, err := os.Stat(filepath.Join(dir, "cloudflare.json")); err == nil {
		t.Fatal("delete removes the key")
	}

	// Negative: everybody else, and the SuperAdmin anywhere else.
	for name, c := range map[string]struct {
		client *http.Client
		host   string
	}{"the SuperAdmin at the primary": {bc, primary}, "a site admin at the panel": {ac, panel}, "a member at the panel": {mc, panel}} {
		if res, page := do(t, c.client, "GET", srv+"/cloud", c.host, nil); res.StatusCode != http.StatusForbidden || !strings.Contains(page, "js-cloud-not-allowed") {
			t.Errorf("negative: %s sees the Cloud Panel: %d", name, res.StatusCode)
		}
		for _, p := range []string{"/cloud/domains", "/cloud/domains/delete", "/cloud/cloudflare"} {
			if res, _ := do(t, c.client, "POST", srv+p, c.host, url.Values{"domain": {"evil.fi"}, "apiToken": {"x"}}); res.StatusCode != http.StatusForbidden {
				t.Errorf("negative: %s POST %s: %d", name, p, res.StatusCode)
			}
		}
	}
	readJSON(t, filepath.Join(dir, "domains.json"), &file)
	if len(file.Domains) != 0 {
		t.Fatal("negative: a refused request changed the table")
	}
	if _, err := os.Stat(filepath.Join(dir, "cloudflare.json")); err == nil {
		t.Fatal("negative: a refused request saved a key")
	}
	req, _ := http.NewRequest("POST", srv+"/cloud/domains", strings.NewReader("domain=evil.fi"))
	req.Host = panel
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	nojar := *bc
	nojar.Jar = nil
	for _, ck := range bc.Jar.Cookies(req.URL) {
		req.AddCookie(ck)
	}
	if res, err := nojar.Do(req); err != nil || res.StatusCode != http.StatusForbidden {
		t.Fatal("negative: the SuperAdmin's browser sent from another site")
	}

	// A customer's WeKan has none of it, even with a Cloud directory.
	w.s.Tenant = true
	if res, _ := do(t, ac, "GET", srv+"/admin/people/cloud", primary, nil); res.StatusCode != http.StatusNotFound {
		t.Fatal("negative: a tenant has no Cloud settings")
	}
	if res, _ := do(t, bc, "GET", srv+"/cloud", panel, nil); res.StatusCode != http.StatusForbidden {
		t.Fatal("negative: a tenant has no Cloud Panel")
	}
}
