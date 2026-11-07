//go:build unix

package caddywekan

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"
)

func get(t *testing.T, site, host string) (int, string) {
	req, _ := http.NewRequest("GET", "http://"+site+"/sign-in", nil)
	req.Host = host
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

// signIn reports whether username/password signs in at host.
func signIn(t *testing.T, site, host, username, password string) bool {
	req, _ := http.NewRequest("POST", "http://"+site+"/sign-in", strings.NewReader(url.Values{"username": {username}, "password": {password}}.Encode()))
	req.Host = host
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode == http.StatusSeeOther
}

func seedUser(t *testing.T, c *Cloud, host, username, password string) {
	in := c.lookup(host)
	if in == nil {
		t.Fatalf("no WeKan at %s", host)
	}
	sum := sha256.Sum256([]byte(password))
	hash, _ := bcrypt.GenerateFromPassword([]byte(hex.EncodeToString(sum[:])), 4)
	_, err := in.srv.DB().Collection("users").InsertOne(context.Background(), bson.M{"_id": username + "-id", "username": username,
		"services": bson.M{"password": bson.M{"bcrypt": string(hash)}}})
	if err != nil {
		t.Fatal(err)
	}
}

func waitOK(t *testing.T, site, host string) string {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if code, body := get(t, site, host); code == 200 {
			return body
		}
	}
	t.Fatalf("%s did not answer", host)
	return ""
}

func writeJSON(t *testing.T, path string, v any) {
	b, _ := json.Marshal(v)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDomainRules(t *testing.T) {
	for _, ok := range []string{"customer.wekan.team", "wekan.customer.com", "a-b.c1.example"} {
		if !ValidDomain(ok) {
			t.Errorf("%s should be valid", ok)
		}
	}
	for _, bad := range []string{"", "localhost", "../etc", "a/b.com", "a..b.com", "-a.com", "a.com.", "UPPER.com", "a b.com", "a.com/../x"} {
		if ValidDomain(bad) {
			t.Errorf("negative: %q must not be a domain (it becomes a directory name)", bad)
		}
	}
	if NormalizeHost("Customer.Wekan.Team.:443") != "customer.wekan.team" {
		t.Error("NormalizeHost")
	}
	day := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	for d, want := range map[Domain]bool{
		{}: true, {StartDate: "2026-11-07", EndDate: "2026-11-07"}: true, {EndDate: "2026-11-06"}: false,
		{StartDate: "2026-11-08"}: false, {StartDate: "2025-11-07", EndDate: "2026-11-07"}: true,
	} {
		if d.Active(day) != want {
			t.Errorf("%+v active = %v", d, !want)
		}
	}
}

func TestManyDomains(t *testing.T) {
	root := t.TempDir()
	cloud := filepath.Join(root, "cloud")
	if err := os.MkdirAll(cloud, 0o700); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(cloud, "domains.json"), DomainsFile{CloudPanelDomain: "cloud.test", Domains: []Domain{
		{Domain: "customer.test", CustomerName: "Customer", EndDate: "2999-12-31"},
		{Domain: "old.test", EndDate: "2000-01-01"},
		{Domain: "../evil", EndDate: "2999-12-31"},
	}})
	site := freeAddr(t)
	cfg := map[string]any{
		"admin": map[string]any{"disabled": true, "config": map[string]any{"persist": false}},
		"apps": map[string]any{
			"wekan": map[string]any{"data": root, "primary": "primary.test"},
			"http": map[string]any{"servers": map[string]any{"s": map[string]any{
				"listen": []string{site}, "automatic_https": map[string]any{"disable": true},
				"routes": []any{map[string]any{"handle": []any{map[string]any{"handler": "wekan"}}}},
			}}},
		},
	}
	raw, _ := json.Marshal(cfg)
	if err := caddy.Load(raw, true); err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = caddy.Stop()
		}
	}()

	waitOK(t, site, "primary.test")
	waitOK(t, site, "Customer.Test:443")
	waitOK(t, site, "cloud.test")
	c := CurrentCloud()
	if c.lookup("primary.test").srv.Tenant || !c.lookup("customer.test").srv.Tenant {
		t.Fatal("the primary is not a tenant; a customer is")
	}
	if c.lookup("primary.test").srv.CloudDir != cloud || c.lookup("customer.test").srv.CloudDir != "" {
		t.Fatal("only the primary's WeKan has the Cloud Panel, over <data>/cloud")
	}
	seedUser(t, c, "customer.test", "cust", "pw-cust")
	seedUser(t, c, "primary.test", "boss", "pw-boss")
	for _, d := range []string{"primary.test", "customer.test"} {
		if _, err := os.Stat(filepath.Join(root, "domains", d, "files", "db", "wekan.sqlite")); err != nil {
			t.Fatalf("%s has its own database, where Node.js WeKan keeps it: %v", d, err)
		}
	}
	if !signIn(t, site, "customer.test", "cust", "pw-cust") || !signIn(t, site, "primary.test", "boss", "pw-boss") {
		t.Fatal("each domain's own user signs in there")
	}
	if signIn(t, site, "primary.test", "cust", "pw-cust") || signIn(t, site, "customer.test", "boss", "pw-boss") {
		t.Fatal("negative: a user of one domain must not exist at another")
	}
	if !signIn(t, site, "cloud.test", "boss", "pw-boss") {
		t.Fatal("the Cloud Panel Domain is the primary's WeKan")
	}
	for _, host := range []string{"old.test", "unknown.test", "evil"} {
		if code, _ := get(t, site, host); code != http.StatusNotFound {
			t.Errorf("negative: %s must get 404, never another domain's WeKan; got %d", host, code)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "domains", "old.test")); err == nil {
		t.Error("negative: an expired domain got a directory")
	}
	if _, err := os.Stat(filepath.Join(root, "evil")); err == nil {
		t.Error("negative: ../evil reached the disk")
	}

	var p Permission
	for name, allowed := range map[string]bool{"customer.test": true, "primary.test": true, "cloud.test": true, "old.test": false, "other.test": false} {
		if err := p.CertificateAllowed(nil, name); (err == nil) != allowed {
			t.Errorf("certificate for %s allowed = %v", name, err == nil)
		}
	}

	// The customer leaves the table: its WeKan stops, within the watch interval.
	time.Sleep(1100 * time.Millisecond) // a new mtime
	writeJSON(t, filepath.Join(cloud, "domains.json"), DomainsFile{CloudPanelDomain: "cloud.test"})
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		if code, _ := get(t, site, "customer.test"); code == http.StatusNotFound {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a removed domain is still served")
		}
	}
	if err := caddy.Stop(); err != nil {
		t.Fatal(err)
	}
	stopped = true
	if code, _ := get(t, site, "primary.test"); code == 200 {
		t.Fatal("negative: still served after Caddy stopped")
	}
}

// A stand-in CloudFlare: one zone, wekan.team; it records DNS records and signs
// Origin certificate CSRs with a CA of its own.
type fakeCloudflare struct {
	mu      sync.Mutex
	records map[string]map[string]any
	certs   int
	caKey   *ecdsa.PrivateKey
	ca      *x509.Certificate
	auth    string
}

func newFakeCloudflare(t *testing.T) (*fakeCloudflare, *httptest.Server) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Fake Origin CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(20 * 365 * 24 * time.Hour), IsCA: true,
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	ca, _ := x509.ParseCertificate(der)
	f := &fakeCloudflare{records: map[string]map[string]any{}, caKey: key, ca: ca}
	reply := func(w http.ResponseWriter, result any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "errors": []any{}, "result": result})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.auth = r.Header.Get("Authorization")
		switch {
		case r.Method == "GET" && r.URL.Path == "/zones":
			if r.URL.Query().Get("name") == "wekan.team" {
				reply(w, []any{map[string]string{"id": "z1", "name": "wekan.team"}})
			} else {
				reply(w, []any{})
			}
		case r.Method == "GET" && r.URL.Path == "/zones/z1/dns_records":
			if rec, ok := f.records[r.URL.Query().Get("name")]; ok {
				reply(w, []any{rec})
			} else {
				reply(w, []any{})
			}
		case r.Method == "POST" && r.URL.Path == "/zones/z1/dns_records":
			var rec map[string]any
			_ = json.NewDecoder(r.Body).Decode(&rec)
			f.records[rec["name"].(string)] = rec
			reply(w, rec)
		case r.Method == "POST" && r.URL.Path == "/certificates":
			var body struct {
				CSR       string   `json:"csr"`
				Hostnames []string `json:"hostnames"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			blk, _ := pem.Decode([]byte(body.CSR))
			csr, err := x509.ParseCertificateRequest(blk.Bytes)
			if err != nil || csr.CheckSignature() != nil {
				w.WriteHeader(400)
				_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []any{"bad csr"}})
				return
			}
			tmpl := &x509.Certificate{SerialNumber: big.NewInt(int64(2 + f.certs)), Subject: csr.Subject, DNSNames: body.Hostnames,
				NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(15 * 365 * 24 * time.Hour),
				ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
			der, _ := x509.CreateCertificate(rand.Reader, tmpl, f.ca, csr.PublicKey, f.caKey)
			f.certs++
			reply(w, map[string]string{"certificate": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))})
		default:
			w.WriteHeader(404)
			_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []any{r.Method + " " + r.URL.Path}})
		}
	}))
	return f, srv
}

func TestCloudflare(t *testing.T) {
	f, srv := newFakeCloudflare(t)
	defer srv.Close()
	old := cloudflareAPI
	cloudflareAPI = srv.URL
	defer func() { cloudflareAPI = old }()

	root := t.TempDir()
	app := &App{Data: root, Primary: "primary.test"}
	c := newCloud(app, nil)
	c.log = zapNop()
	if err := os.MkdirAll(filepath.Join(root, "cloud", "certs"), 0o700); err != nil {
		t.Fatal(err)
	}
	c.active = map[string]bool{"primary.test": true, "cloud.wekan.team": true, "customer.wekan.team": true, "wekan.customer.com": true}
	c.panel = "cloud.wekan.team"

	// No token: nothing is sent to CloudFlare, and every domain is Let's Encrypt.
	c.syncCloudflare()
	if f.records["customer.wekan.team"] != nil {
		t.Fatal("negative: without a token nothing may reach CloudFlare")
	}
	writeJSON(t, filepath.Join(root, "cloud", "cloudflare.json"), map[string]string{"apiToken": "secret-token"})
	c.syncCloudflare()
	rec := f.records["customer.wekan.team"]
	if rec == nil || rec["type"] != "CNAME" || rec["content"] != "cloud.wekan.team" || rec["proxied"] != true {
		t.Fatalf("a proxied CNAME to the Cloud Panel Domain: %v", rec)
	}
	if f.auth != "Bearer secret-token" {
		t.Fatalf("the token is sent as a bearer token: %q", f.auth)
	}
	if _, ok := f.records["wekan.customer.com"]; ok {
		t.Fatal("negative: a domain whose zone is not in the account got a record")
	}
	keyInfo, err := os.Stat(filepath.Join(root, "cloud", "certs", "customer.wekan.team.key"))
	if err != nil || keyInfo.Mode().Perm() != 0o600 {
		t.Fatalf("the Origin key, mode 0600: %v %v", err, keyInfo)
	}
	var status struct{ Domains []DomainStatus }
	b, _ := os.ReadFile(filepath.Join(root, "cloud", "status.json"))
	_ = json.Unmarshal(b, &status)
	tlsOf := map[string]string{}
	for _, d := range status.Domains {
		tlsOf[d.Domain] = d.TLS
	}
	if tlsOf["customer.wekan.team"] != "cloudflare-origin" || tlsOf["wekan.customer.com"] != "letsencrypt" || len(status.Domains) != 2 {
		t.Fatalf("status.json: %s", b)
	}

	// Again: the record exists and the certificate is valid - nothing new.
	certs := f.certs
	c.syncCloudflare()
	if f.certs != certs {
		t.Fatal("negative: a valid Origin certificate was requested again")
	}

	// The certificate manager serves it for that domain, and nothing for others.
	current.Lock()
	current.cloud = c
	current.Unlock()
	defer func() { current.Lock(); current.cloud = nil; current.Unlock() }()
	var o OriginCerts
	_ = o.Provision(caddy.Context{})
	cert, err := o.GetCertificate(nil, &tls.ClientHelloInfo{ServerName: "customer.wekan.team"})
	if err != nil || cert == nil || cert.Leaf == nil && len(cert.Certificate) == 0 {
		t.Fatalf("the Origin certificate: %v %v", cert, err)
	}
	if leaf, _ := x509.ParseCertificate(cert.Certificate[0]); leaf.DNSNames[0] != "customer.wekan.team" {
		t.Fatalf("the certificate is for %v", leaf.DNSNames)
	}
	for _, name := range []string{"wekan.customer.com", "other.test"} {
		if cert, _ := o.GetCertificate(nil, &tls.ClientHelloInfo{ServerName: name}); cert != nil {
			t.Errorf("negative: %s must fall through to Let's Encrypt (or be refused)", name)
		}
	}
}

func zapNop() *zap.Logger { return zap.NewNop() }
