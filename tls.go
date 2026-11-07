package caddywekan

// The certificates of many domains, with no Caddy reload when the Domains table
// changes:
//
//	{
//		on_demand_tls {
//			permission wekan_domains     # a certificate only for a domain that is served
//		}
//	}
//	https:// {
//		tls {
//			on_demand
//			get_certificate wekan_cloudflare   # a CloudFlare Origin certificate when there is one
//		}
//		wekan
//	}
//
// For a domain with a CloudFlare Origin certificate (cloudflare.go), that certificate
// is used; for every other served domain Caddy gets one from Let's Encrypt.

import (
	"context"
	"crypto/tls"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddytls"
	"github.com/caddyserver/certmagic"
)

func init() {
	caddy.RegisterModule(Permission{})
	caddy.RegisterModule(OriginCerts{})
}

// Permission allows an on-demand certificate only for a served domain.
type Permission struct{}

// CaddyModule returns the Caddy module information.
func (Permission) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "tls.permission.wekan_domains", New: func() caddy.Module { return new(Permission) }}
}

// CertificateAllowed is nil only for the primary, the Cloud Panel Domain and an
// active domain of the Domains table.
func (Permission) CertificateAllowed(_ context.Context, name string) error {
	if c := CurrentCloud(); c != nil && c.Allowed(name) {
		return nil
	}
	return fmt.Errorf("%s is not a served WeKan domain: %w", name, caddytls.ErrPermissionDenied)
}

// UnmarshalCaddyfile: "permission wekan_domains", no arguments.
func (p *Permission) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next()
	if d.NextArg() {
		return d.ArgErr()
	}
	return nil
}

// OriginCerts gives the CloudFlare Origin certificate of a domain, when it has one.
type OriginCerts struct {
	mu    *sync.Mutex
	cache map[string]cachedCert
}

type cachedCert struct {
	mod  time.Time
	cert *tls.Certificate
}

// CaddyModule returns the Caddy module information.
func (OriginCerts) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "tls.get_certificate.wekan_cloudflare", New: func() caddy.Module { return new(OriginCerts) }}
}

// Provision makes the certificate cache.
func (o *OriginCerts) Provision(caddy.Context) error {
	o.mu, o.cache = new(sync.Mutex), map[string]cachedCert{}
	return nil
}

// GetCertificate returns the domain's Origin certificate, or nil - then Caddy's
// issuers (Let's Encrypt) get one.
func (o *OriginCerts) GetCertificate(_ context.Context, hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	c := CurrentCloud()
	name := NormalizeHost(hello.ServerName)
	if c == nil || !ValidDomain(name) || !c.Allowed(name) {
		return nil, nil
	}
	dir := filepath.Join(c.app.cloudDir(), "certs")
	certFile, keyFile := filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
	st, err := os.Stat(certFile)
	if err != nil {
		return nil, nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if cc, ok := o.cache[name]; ok && cc.mod.Equal(st.ModTime()) {
		return cc.cert, nil
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, nil // half written: Let's Encrypt, or the next handshake
	}
	o.cache[name] = cachedCert{mod: st.ModTime(), cert: &cert}
	return &cert, nil
}

// UnmarshalCaddyfile: "get_certificate wekan_cloudflare", no arguments.
func (o *OriginCerts) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next()
	if d.NextArg() {
		return d.ArgErr()
	}
	return nil
}

var (
	_ caddytls.OnDemandPermission = (*Permission)(nil)
	_ certmagic.Manager           = (*OriginCerts)(nil)
	_ caddyfile.Unmarshaler       = (*Permission)(nil)
	_ caddyfile.Unmarshaler       = (*OriginCerts)(nil)
)
