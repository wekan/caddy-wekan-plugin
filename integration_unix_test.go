//go:build unix

package caddywekan

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
)

// A real Caddy runs a stand-in bundle whose start-wekan.sh starts a background
// process (as the real one starts FerretDB) and then WeKan. Stopping Caddy must
// stop both.
func TestCaddyRunsWeKanAndStopsFerretDBToo(t *testing.T) {
	exe, _ := os.Executable()
	bundle := t.TempDir()
	childPID := filepath.Join(bundle, "ferretdb.pid")
	script := "#!/bin/sh\nsleep 300 &\necho $! > \"" + childPID + "\"\nWEKAN_TEST_HELPER=serve \"" + exe + "\"\n"
	if err := os.WriteFile(filepath.Join(bundle, "start-wekan.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	backend, site := freeAddr(t), freeAddr(t)
	cfg := map[string]any{
		"admin": map[string]any{"disabled": true},
		"apps": map[string]any{
			"wekan": map[string]any{"bundle": bundle, "root_url": "https://wekan.example.org", "dial": backend, "stop_timeout": "5s"},
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
	var body string
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if res, err := http.Get("http://" + site + "/b/x"); err == nil {
			b, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				body = string(b)
				break
			}
		}
	}
	if body != "https://wekan.example.org /b/x" {
		_ = caddy.Stop()
		t.Fatalf("proxied answer %q (ROOT_URL and the path must reach WeKan)", body)
	}
	pidText, _ := os.ReadFile(childPID)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(pidText)))
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		_ = caddy.Stop()
		t.Fatalf("the stand-in FerretDB did not start: %q", pidText)
	}
	if err := caddy.Stop(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("negative: the process start-wekan.sh started (FerretDB) still runs after Caddy stopped")
		}
	}
	if c, err := net.DialTimeout("tcp", backend, time.Second); err == nil {
		c.Close()
		t.Fatal("negative: WeKan still listens after Caddy stopped")
	}
}
