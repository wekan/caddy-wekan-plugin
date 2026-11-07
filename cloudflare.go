package caddywekan

// CloudFlare: with an API token in <root>/cloud/cloudflare.json, every served domain
// whose zone is in that CloudFlare account gets
//
//   - a proxied (orange cloud) DNS record: a CNAME to the Cloud Panel Domain - or the
//     primary domain - which points at this server;
//   - a CloudFlare Origin certificate, for CloudFlare's "Full (strict)" mode: a key
//     and CSR made here, signed by CloudFlare's Origin CA, stored in
//     <root>/cloud/certs/<domain>.crt and .key, and renewed 30 days before it ends.
//
// A domain whose zone is not in the account is served with a Let's Encrypt
// certificate that Caddy gets on demand. The token needs the permissions Zone:Read,
// DNS:Edit and SSL and Certificates:Edit. It is read here, on the server, only.

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// cloudflareAPI is CloudFlare's API; the tests point it at a stand-in.
var cloudflareAPI = "https://api.cloudflare.com/client/v4"

var cfMu sync.Mutex // one sync at a time

// DomainStatus is what this side did for one domain, in <root>/cloud/status.json.
type DomainStatus struct {
	Domain      string `json:"domain"`
	TLS         string `json:"tls"` // "cloudflare-origin" or "letsencrypt"
	Zone        string `json:"zone,omitempty"`
	DNS         string `json:"dns,omitempty"`
	CertExpires string `json:"certExpires,omitempty"`
	Error       string `json:"error,omitempty"`
}

type cfClient struct {
	token string
	http  *http.Client
}

type cfResponse struct {
	Success bool              `json:"success"`
	Errors  []json.RawMessage `json:"errors"`
	Result  json.RawMessage   `json:"result"`
}

func (c cfClient) call(method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, cloudflareAPI+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	var r cfResponse
	if err := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&r); err != nil {
		return fmt.Errorf("CloudFlare %s %s: %d, %w", method, path, res.StatusCode, err)
	}
	if !r.Success {
		return fmt.Errorf("CloudFlare %s %s: %d %s", method, path, res.StatusCode, joinRaw(r.Errors))
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

func joinRaw(list []json.RawMessage) string {
	parts := make([]string, len(list))
	for i, m := range list {
		parts[i] = string(m)
	}
	return strings.Join(parts, "; ")
}

// zone finds the CloudFlare zone of domain: the domain itself, or the nearest
// parent that is a zone in the account. "" when there is none.
func (c cfClient) zone(domain string) (id, name string, err error) {
	labels := strings.Split(domain, ".")
	for i := 0; i < len(labels)-1; i++ {
		candidate := strings.Join(labels[i:], ".")
		var zones []struct{ ID, Name string }
		if err := c.call("GET", "/zones?name="+url.QueryEscape(candidate), nil, &zones); err != nil {
			return "", "", err
		}
		if len(zones) > 0 {
			return zones[0].ID, zones[0].Name, nil
		}
	}
	return "", "", nil
}

// ensureRecord adds a proxied CNAME to target, unless the domain has a record.
func (c cfClient) ensureRecord(zoneID, domain, target string) (string, error) {
	var records []struct{ ID, Type, Content string }
	if err := c.call("GET", "/zones/"+zoneID+"/dns_records?name="+url.QueryEscape(domain), nil, &records); err != nil {
		return "", err
	}
	if len(records) > 0 {
		return fmt.Sprintf("exists (%s %s)", records[0].Type, records[0].Content), nil
	}
	if target == "" || target == domain {
		return "", fmt.Errorf("no CNAME target: set the Cloud Panel Domain")
	}
	body := map[string]any{"type": "CNAME", "name": domain, "content": target, "proxied": true, "ttl": 1,
		"comment": "WeKan Cloud Panel (Weddy)"}
	if err := c.call("POST", "/zones/"+zoneID+"/dns_records", body, nil); err != nil {
		return "", err
	}
	return "created (CNAME " + target + ", proxied)", nil
}

// originCert gets a CloudFlare Origin certificate for domain into dir, unless a
// valid one with more than 30 days left is there.
func (c cfClient) originCert(dir, domain string, now time.Time) (time.Time, error) {
	certFile, keyFile := filepath.Join(dir, domain+".crt"), filepath.Join(dir, domain+".key")
	if b, err := os.ReadFile(certFile); err == nil {
		if blk, _ := pem.Decode(b); blk != nil {
			if cert, err := x509.ParseCertificate(blk.Bytes); err == nil && cert.NotAfter.Sub(now) > 30*24*time.Hour {
				return cert.NotAfter, nil
			}
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return time.Time{}, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: domain}, DNSNames: []string{domain},
	}, key)
	if err != nil {
		return time.Time{}, err
	}
	var result struct {
		Certificate string `json:"certificate"`
		ExpiresOn   string `json:"expires_on"`
	}
	body := map[string]any{"hostnames": []string{domain}, "request_type": "origin-ecc", "requested_validity": 5475,
		"csr": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}))}
	if err := c.call("POST", "/certificates", body, &result); err != nil {
		return time.Time{}, err
	}
	blk, _ := pem.Decode([]byte(result.Certificate))
	if blk == nil {
		return time.Time{}, fmt.Errorf("CloudFlare returned no certificate")
	}
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return time.Time{}, err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return time.Time{}, err
	}
	// The key first, so a certificate is never on disk without it.
	if err := writeFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})); err != nil {
		return time.Time{}, err
	}
	return cert.NotAfter, writeFile(certFile, []byte(result.Certificate))
}

func writeFile(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (c *Cloud) cloudflareToken() string {
	b, err := os.ReadFile(filepath.Join(c.app.cloudDir(), "cloudflare.json"))
	if err != nil {
		return ""
	}
	var f struct {
		APIToken string `json:"apiToken"`
	}
	if json.Unmarshal(b, &f) != nil {
		return ""
	}
	return strings.TrimSpace(f.APIToken)
}

// syncCloudflare brings every served customer domain's DNS record and Origin
// certificate up to date, and writes status.json.
func (c *Cloud) syncCloudflare() {
	cfMu.Lock()
	defer cfMu.Unlock()
	c.mu.RLock()
	panel := c.panel
	var names []string
	for name := range c.active {
		if name != c.app.Primary && name != panel {
			names = append(names, name)
		}
	}
	c.mu.RUnlock()
	sort.Strings(names)
	target := panel
	if target == "" {
		target = c.app.Primary
	}
	token := c.cloudflareToken()
	client := cfClient{token: token, http: &http.Client{Timeout: 60 * time.Second}}
	certs := filepath.Join(c.app.cloudDir(), "certs")
	out := make([]DomainStatus, 0, len(names))
	for _, name := range names {
		st := DomainStatus{Domain: name, TLS: "letsencrypt"}
		if token != "" {
			zoneID, zoneName, err := client.zone(name)
			switch {
			case err != nil:
				st.Error = err.Error()
			case zoneID != "":
				st.Zone = zoneName
				if st.DNS, err = client.ensureRecord(zoneID, name, target); err != nil {
					st.Error = err.Error()
				} else if exp, err := client.originCert(certs, name, time.Now()); err != nil {
					st.Error = err.Error()
				} else {
					st.TLS, st.CertExpires = "cloudflare-origin", exp.UTC().Format(time.RFC3339)
				}
			}
		}
		if st.Error != "" {
			c.log.Warn("CloudFlare", zap.String("domain", name), zap.String("error", st.Error))
		}
		out = append(out, st)
	}
	b, _ := json.MarshalIndent(map[string]any{"updated": time.Now().UTC().Format(time.RFC3339), "domains": out}, "", "  ")
	if err := writeFile(filepath.Join(c.app.cloudDir(), "status.json"), append(b, '\n')); err != nil {
		c.log.Error("writing status.json", zap.Error(err))
	}
}
