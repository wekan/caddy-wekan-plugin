package web

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// Outgoing webhooks through a proxy, and a certificate to trust, as Node.js
// WeKan's server/lib/outboundProxy.js (#1455) and ssrfGuard.js (#6553):
//
//   - HTTP_PROXY / HTTPS_PROXY name the proxy, NO_PROXY the destinations that
//     go direct. The destination is resolved and checked here first, and the
//     proxy is asked for that address, never the name - it cannot resolve
//     the name to somewhere else; TLS is still checked against the real host.
//     The proxy itself is the administrator's and may be on a private
//     network; it is the only address such a connection may make.
//   - WEBHOOK_TLS_CA_CERT is a certificate - PEM, or a file of it - that
//     webhooks trust: a self-signed internal Mattermost, a company CA. As
//     Node's `ca`, it replaces the system's trust store for webhooks; it never
//     turns verification off.

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(os.Getenv(n)); v != "" {
			return v
		}
	}
	return ""
}

// noProxyMatches is WeKan's: "*", a host, ".domain" or "*.domain", with an
// optional :port.
func noProxyMatches(host, port, noProxy string) bool {
	host = strings.ToLower(strings.Trim(host, "[]"))
	for _, raw := range strings.FieldsFunc(strings.ToLower(noProxy), func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
		if raw == "*" {
			return true
		}
		entry, entryPort := raw, ""
		if i := strings.LastIndex(entry, ":"); i > 0 && strings.Index(entry, ":") == i {
			entry, entryPort = entry[:i], entry[i+1:]
		}
		entry = strings.TrimPrefix(strings.Trim(entry, "[]"), "*")
		if entryPort != "" && entryPort != port {
			continue
		}
		if strings.HasPrefix(entry, ".") {
			if host == entry[1:] || strings.HasSuffix(host, entry) {
				return true
			}
		} else if host == entry || strings.HasSuffix(host, "."+entry) {
			return true
		}
	}
	return false
}

// webhookProxyFor is WeKan's proxyFor: the proxy for a destination, or nil.
func webhookProxyFor(target *url.URL) (*url.URL, error) {
	isHTTPS := target.Scheme == "https"
	value := firstEnv("HTTP_PROXY", "http_proxy")
	if isHTTPS {
		value = firstEnv("HTTPS_PROXY", "https_proxy")
	}
	if value == "" {
		return nil, nil
	}
	port := target.Port()
	if port == "" {
		port = map[bool]string{true: "443", false: "80"}[isHTTPS]
	}
	if noProxyMatches(target.Hostname(), port, firstEnv("NO_PROXY", "no_proxy")) {
		return nil, nil
	}
	if !strings.Contains(value, "://") {
		value = "http://" + value
	}
	p, err := url.Parse(value)
	if err != nil || p.Host == "" || (p.Scheme != "http" && p.Scheme != "https") {
		return nil, errors.New("SSRF_GUARD: the proxy setting is not a URL")
	}
	return p, nil
}

// webhookTrust is WEBHOOK_TLS_CA_CERT's pool, nil for the system's.
func webhookTrust() *x509.CertPool {
	value := strings.TrimSpace(os.Getenv("WEBHOOK_TLS_CA_CERT"))
	if value == "" {
		return nil
	}
	pem := []byte(value)
	if !strings.Contains(value, "-----BEGIN CERTIFICATE-----") {
		b, err := os.ReadFile(value)
		if err != nil {
			return nil // the system's store: a private certificate is still refused
		}
		pem = b
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil
	}
	return pool
}

// guardedControl refuses a connection to a blocked address.
func guardedControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errSSRFBlocked
	}
	if webhookAllowLoopbackForTests {
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return nil
		}
	}
	if ipBlocked(host) {
		return errSSRFBlocked
	}
	return nil
}

// webhookRequestClient is the guarded client for one request: direct, every
// address checked at the socket; or through the proxy, the destination
// resolved and checked first and the request pinned to that address, the
// proxy the only other address dialled.
func webhookRequestClient(ctx context.Context, req *http.Request) (*http.Client, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: webhookTrust(), ServerName: req.URL.Hostname()}
	transport := &http.Transport{TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 20 * time.Second,
		MaxResponseHeaderBytes: 64 << 10, TLSClientConfig: tlsConfig}
	guarded := &net.Dialer{Timeout: 10 * time.Second, Control: guardedControl}
	transport.DialContext = guarded.DialContext
	proxy, err := webhookProxyFor(req.URL)
	if err != nil {
		return nil, err
	}
	if proxy != nil {
		host := req.URL.Hostname()
		ips := []string{host}
		if net.ParseIP(host) == nil {
			if ips, err = lookupHost(ctx, host); err != nil || len(ips) == 0 {
				return nil, errors.New("SSRF_GUARD: the destination does not resolve")
			}
		}
		for _, ip := range ips {
			if !webhookAllowLoopbackForTests && ipBlocked(ip) {
				return nil, errSSRFBlocked
			}
		}
		port := req.URL.Port()
		if port == "" {
			port = map[bool]string{true: "443", false: "80"}[req.URL.Scheme == "https"]
		}
		// The proxy is asked for the checked address, the Host header and
		// TLS keep the name.
		pinned := net.JoinHostPort(ips[0], port)
		req.Host = req.URL.Host
		req.URL.Host = pinned
		if req.URL.Scheme == "http" {
			// The proxy's request line is built from Host when it is set; the
			// opaque form puts the pinned address there instead.
			req.URL.Opaque = "//" + pinned + req.URL.EscapedPath()
		}
		proxyAddr := proxy.Host
		if proxy.Port() == "" {
			proxyAddr = net.JoinHostPort(proxy.Hostname(), map[bool]string{true: "443", false: "80"}[proxy.Scheme == "https"])
		}
		plain := &net.Dialer{Timeout: 10 * time.Second}
		transport.Proxy = http.ProxyURL(proxy)
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr != proxyAddr {
				return nil, errSSRFBlocked // through the proxy, nothing else is dialled
			}
			return plain.DialContext(ctx, network, addr)
		}
	}
	return &http.Client{Timeout: 30 * time.Second, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}
