package caddywekan

import (
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	_ "github.com/caddyserver/caddy/v2/modules/standard"
)

func freeAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func TestValidate(t *testing.T) {
	for _, ok := range []App{{Data: "/var/lib/weddy"}, {Data: "/var/lib/weddy", Primary: "wekan.example.org"},
		{Data: "/var/lib/weddy", DatabaseListen: "127.0.0.1:27017"}} {
		if err := ok.Validate(); err != nil {
			t.Errorf("%+v: %v", ok, err)
		}
	}
	for name, a := range map[string]App{
		"no data":                     {},
		"relative data":               {Data: "weddy"},
		"primary not a domain":        {Data: "/x", Primary: "../etc"},
		"database_listen no port":     {Data: "/x", DatabaseListen: "127.0.0.1"},
		"database_listen and domains": {Data: "/x", Primary: "wekan.example.org", DatabaseListen: "127.0.0.1:27017"},
	} {
		if err := a.Validate(); err == nil {
			t.Errorf("negative: %s must be refused", name)
		}
	}
}

func TestCaddyfile(t *testing.T) {
	adapter := caddyfile.Adapter{ServerType: httpcaddyfile.ServerType{}}
	out, _, err := adapter.Adapt([]byte("{\n\twekan {\n\t\tdata /var/lib/weddy\n\t\tprimary wekan.team\n\t}\n}\nwekan.example.org {\n\twekan\n}\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct{ Apps map[string]json.RawMessage }
	_ = json.Unmarshal(out, &cfg)
	var app App
	_ = json.Unmarshal(cfg.Apps["wekan"], &app)
	if app.Data != "/var/lib/weddy" || app.Primary != "wekan.team" {
		t.Fatalf("the wekan option: %+v", app)
	}
	if !strings.Contains(string(cfg.Apps["http"]), `"handler":"wekan"`) {
		t.Fatal("the site has no wekan handler")
	}
	out, _, err = adapter.Adapt([]byte("{\n\twekan {\n\t\troot /srv/w\n\t}\n}\n"), nil)
	if err != nil || !strings.Contains(string(out), `"data":"/srv/w"`) {
		t.Fatalf("root is the older name of data: %v %s", err, out)
	}
	if _, _, err := adapter.Adapt([]byte("{\n\twekan {\n\t\tport 1\n\t}\n}\n"), nil); err == nil {
		t.Fatal("negative: an unknown option must be an error")
	}
	for _, old := range []string{"bundle /opt/wekan", "root_url https://x.org", "env A b", "command x"} {
		_, _, err := adapter.Adapt([]byte("{\n\twekan {\n\t\t"+old+"\n\t}\n}\n"), nil)
		if err == nil || !strings.Contains(err.Error(), "without Node.js") {
			t.Errorf("negative: the Node.js option %q must be refused with the reason: %v", old, err)
		}
	}
}
