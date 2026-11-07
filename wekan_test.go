package caddywekan

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	_ "github.com/caddyserver/caddy/v2/modules/standard"
)

// The test binary is also the stand-in WeKan: WEKAN_TEST_HELPER=serve listens on
// BIND_IP:PORT, as Meteor does, and answers with ROOT_URL and the path.
func TestMain(m *testing.M) {
	if os.Getenv("WEKAN_TEST_HELPER") == "serve" {
		time.Sleep(500 * time.Millisecond)
		l, err := net.Listen("tcp", net.JoinHostPort(os.Getenv("BIND_IP"), os.Getenv("PORT")))
		if err != nil {
			os.Exit(3)
		}
		_ = http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, os.Getenv("ROOT_URL")+" "+r.URL.Path)
		}))
		return
	}
	os.Exit(m.Run())
}

func freeAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func TestEnvironment(t *testing.T) {
	a := App{RootURL: "https://wekan.example.org", Dial: "127.0.0.1:8081",
		Env: map[string]string{"WEKAN_DB": "ferretdb", "PORT": "9999", "UNIX_SOCKET_PATH": "/x"}}
	env, err := a.Environment()
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(env, " ")
	if got != "BIND_IP=127.0.0.1 PORT=8081 ROOT_URL=https://wekan.example.org WEKAN_DB=ferretdb" {
		t.Fatalf("negative included: env cannot move WeKan away from dial: %s", got)
	}
	a.Dial = "unix//run/wekan.sock"
	env, _ = a.Environment()
	if got := strings.Join(env, " "); !strings.Contains(got, "UNIX_SOCKET_PATH=/run/wekan.sock") || strings.Contains(got, "PORT=") {
		t.Fatalf("unix socket: %s", got)
	}
}

func TestValidate(t *testing.T) {
	dir := t.TempDir()
	ok := App{Bundle: dir, Command: filepath.Join(dir, "start-wekan.sh"), RootURL: "https://wekan.example.org"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, a := range map[string]App{
		"no root_url":       {Bundle: dir, Command: "x"},
		"root_url no host":  {Bundle: dir, Command: "x", RootURL: "https://"},
		"root_url not http": {Bundle: dir, Command: "x", RootURL: "ftp://wekan.example.org"},
		"no bundle":         {RootURL: "https://wekan.example.org"},
		"bundle not a dir":  {Bundle: filepath.Join(dir, "nope"), Command: "x", RootURL: "https://wekan.example.org"},
	} {
		if err := a.Validate(); err == nil {
			t.Errorf("negative: %s must be refused", name)
		}
	}
}

func TestCaddyfile(t *testing.T) {
	adapter := caddyfile.Adapter{ServerType: httpcaddyfile.ServerType{}}
	out, _, err := adapter.Adapt([]byte("{\n\twekan {\n\t\tbundle /opt/wekan\n\t\troot_url https://wekan.example.org\n\t\tenv WEKAN_DB ferretdb\n\t}\n}\nwekan.example.org {\n\twekan\n}\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct{ Apps map[string]json.RawMessage }
	_ = json.Unmarshal(out, &cfg)
	var app App
	_ = json.Unmarshal(cfg.Apps["wekan"], &app)
	if app.Bundle != "/opt/wekan" || app.RootURL != "https://wekan.example.org" || app.Env["WEKAN_DB"] != "ferretdb" {
		t.Fatalf("the wekan option: %+v", app)
	}
	if !strings.Contains(string(cfg.Apps["http"]), `"handler":"wekan"`) {
		t.Fatal("the site has no wekan handler")
	}
	if _, _, err := adapter.Adapt([]byte("{\n\twekan {\n\t\tport 1\n\t}\n}\n"), nil); err == nil {
		t.Fatal("negative: an unknown option must be an error")
	}
}
