package web

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// WeKan's NO_PROXY matching, case for case.
func TestNoProxyMatches(t *testing.T) {
	for _, c := range []struct {
		host, port, noProxy string
		want                bool
	}{
		{"hooks.example", "443", "*", true},
		{"hooks.example", "443", "hooks.example", true},
		{"a.hooks.example", "443", "hooks.example", true},
		{"a.hooks.example", "443", ".hooks.example", true},
		{"a.hooks.example", "443", "*.hooks.example", true},
		{"hooks.example", "443", "hooks.example:8443", false},
		{"hooks.example", "8443", "hooks.example:8443", true},
		{"nothooks.example", "443", "hooks.example", false},
		{"hooks.example", "443", "other.example, hooks.example", true},
		{"hooks.example", "443", "", false},
	} {
		if got := noProxyMatches(c.host, c.port, c.noProxy); got != c.want {
			t.Errorf("%s:%s with NO_PROXY=%q: %v", c.host, c.port, c.noProxy, got)
		}
	}
}

// Through HTTP_PROXY: the proxy is asked for the checked address, never the
// name, and may itself be on this machine; a destination that resolves to a
// private address is refused before the proxy hears of it.
func TestWebhookThroughProxy(t *testing.T) {
	// A raw proxy: what reaches it, byte for byte - Go's own server would
	// take Host from an absolute request line and hide the header.
	var mu sync.Mutex
	var asked []string
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				var head []string
				for {
					line, err := r.ReadString('\n')
					if err != nil || line == "\r\n" {
						break
					}
					head = append(head, strings.TrimRight(line, "\r\n"))
				}
				mu.Lock()
				asked = append(asked, strings.Join(head, "|"))
				mu.Unlock()
				_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
			}(conn)
		}
	}()
	proxyURL := "http://" + ln.Addr().String()
	t.Setenv("HTTP_PROXY", proxyURL)
	t.Setenv("NO_PROXY", "")
	real := lookupHost
	t.Cleanup(func() { lookupHost = real })
	rebinds := 0
	lookupHost = func(_ context.Context, host string) ([]string, error) {
		switch host {
		case "inside.example":
			return []string{"10.0.0.5"}, nil
		case "rebind.example":
			// Public when the URL is checked, private when it is pinned.
			mu.Lock()
			defer mu.Unlock()
			rebinds++
			if rebinds == 1 {
				return []string{"93.184.216.34"}, nil
			}
			return []string{"10.0.0.5"}, nil
		}
		return []string{"93.184.216.34"}, nil
	}
	w := newTestWeKan(t)
	ctx := context.Background()
	if err := w.s.deliverWebhook(ctx, bson.M{"_id": "hp", "url": "http://hooks.example/in"}, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := append([]string(nil), asked...)
	mu.Unlock()
	if len(got) != 1 || !strings.HasPrefix(got[0], "POST http://93.184.216.34:80/in HTTP/1.1|") || !strings.Contains(got[0], "|Host: hooks.example") {
		t.Errorf("the proxy is asked for the checked address, the name kept as Host: %v", got)
	}
	if err := w.s.deliverWebhook(ctx, bson.M{"_id": "hq", "url": "http://inside.example/in"}, []byte("{}")); err == nil {
		t.Error("negative: a destination on a private network is refused, proxy or not")
	}
	// DNS rebinding: the address pinned is checked again, not the first one.
	if err := w.s.deliverWebhook(ctx, bson.M{"_id": "hr", "url": "http://rebind.example/in"}, []byte("{}")); err == nil {
		t.Error("negative: a name that re-resolves to a private address is refused")
	}
	mu.Lock()
	n := len(asked)
	mu.Unlock()
	if n != 1 {
		t.Errorf("negative: the proxy heard nothing of them: %d", n)
	}
	var row bson.M
	if err := w.db.Collection("eventlog").FindOne(ctx, bson.M{"bleed": "IntegrationBleed"}).Decode(&row); err != nil {
		t.Error("the refusal is in Problems")
	}
}

// WEBHOOK_TLS_CA_CERT: a webhook with a self-signed certificate is reached
// when that certificate is trusted, and refused otherwise - verification is
// never turned off.
func TestWebhookTrustedCertificate(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(srv.Close)
	webhookAllowLoopbackForTests = true
	t.Cleanup(func() { webhookAllowLoopbackForTests = false })
	t.Setenv("HTTPS_PROXY", "")
	w := newTestWeKan(t)
	ctx := context.Background()
	hook := bson.M{"_id": "ht", "url": srv.URL + "/in"}
	if err := w.s.deliverWebhook(ctx, hook, []byte("{}")); err == nil {
		t.Error("negative: an unknown issuer is refused")
	}
	certPEM := func(s *httptest.Server) string {
		return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}))
	}
	// httptest's servers share one certificate; another is made here.
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "elsewhere"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEBHOOK_TLS_CA_CERT", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
	if err := w.s.deliverWebhook(ctx, hook, []byte("{}")); err == nil {
		t.Error("negative: another certificate is no trust for this one")
	}
	t.Setenv("WEBHOOK_TLS_CA_CERT", certPEM(srv))
	if err := w.s.deliverWebhook(ctx, hook, []byte("{}")); err != nil {
		t.Errorf("the trusted certificate: %v", err)
	}
}
