//go:build unix

package caddywekan

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
)

// A real Caddy is WeKan: the sign-in page from the Go WeKan, its database where
// Node.js WeKan keeps it (wekan.sqlite, from the first write), the database also on TCP for MongoDB
// tools when asked - and all of it gone when Caddy stops.
func TestCaddyIsWeKan(t *testing.T) {
	data := t.TempDir()
	site, dbAddr := freeAddr(t), freeAddr(t)
	cfg := map[string]any{
		"admin": map[string]any{"disabled": true, "config": map[string]any{"persist": false}},
		"apps": map[string]any{
			"wekan": map[string]any{"data": data, "database_listen": dbAddr},
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
	var page string
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if res, err := http.Get("http://" + site + "/sign-in"); err == nil {
			b, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				page = string(b)
				break
			}
		}
	}
	if !strings.Contains(page, `id="at-pwd-form"`) {
		_ = caddy.Stop()
		t.Fatalf("the sign-in page: %q", page)
	}
	if res, err := http.Get("http://" + site + "/static/vendor/unpoly.min.js"); err != nil || res.StatusCode != 200 {
		_ = caddy.Stop()
		t.Fatal("the static files are served")
	}
	if st, err := os.Stat(filepath.Join(data, "files", "db")); err != nil || !st.IsDir() {
		_ = caddy.Stop()
		t.Fatalf("the database is in <data>/files/db, as Node.js WeKan keeps it: %v", err)
	}
	if c, err := net.DialTimeout("tcp", dbAddr, time.Second); err != nil {
		_ = caddy.Stop()
		t.Fatalf("database_listen: %v", err)
	} else {
		c.Close()
	}
	if err := caddy.Stop(); err != nil {
		t.Fatal(err)
	}
	if c, err := net.DialTimeout("tcp", dbAddr, time.Second); err == nil {
		c.Close()
		t.Fatal("negative: the database still listens after Caddy stopped")
	}
	if c, err := net.DialTimeout("tcp", site, time.Second); err == nil {
		c.Close()
		t.Fatal("negative: WeKan is still served after Caddy stopped")
	}
}
