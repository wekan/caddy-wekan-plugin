package web

import (
	"context"
	"errors"
	"testing"
)

func TestIPBlocked(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.1.2.3", "169.254.169.254", "100.64.0.1", "192.168.1.1", "172.16.0.1", "0.0.0.0",
		"224.0.0.1", "::1", "::ffff:127.0.0.1", "::127.0.0.1", "2002:7f00:1::", "64:ff9b::a9fe:a9fe", "fd00::1", "fe80::1",
		"2001:db8::1", "ff02::1", "2001:0:0a00:0001::", "fe80::1%eth0", "not-an-ip"} {
		if !ipBlocked(ip) {
			t.Errorf("negative: %s must be blocked", ip)
		}
	}
	for _, ip := range []string{"93.184.216.34", "2606:2800:220:1:248:1893:25c8:1946", "8.8.8.8"} {
		if ipBlocked(ip) {
			t.Errorf("%s is a public address", ip)
		}
	}
}

func TestValidateOutboundURL(t *testing.T) {
	real := lookupHost
	defer func() { lookupHost = real }()
	lookupHost = func(_ context.Context, host string) ([]string, error) {
		switch host {
		case "example.com":
			return []string{"93.184.216.34"}, nil
		case "rebind.example":
			return []string{"93.184.216.34", "127.0.0.1"}, nil
		}
		return nil, errors.New("no such host")
	}
	ctx := context.Background()
	if r := validateOutboundURL(ctx, "https://example.com/hook"); r != "" {
		t.Fatalf("a public host: %s", r)
	}
	for _, bad := range []string{"", "ftp://example.com/", "file:///etc/passwd", "http://localhost:3000/", "http://a.localhost/",
		"http://127.0.0.1/", "http://[::1]/", "http://169.254.169.254/latest/meta-data", "http://rebind.example/",
		"http://nosuchhost.invalid/", "http://0/", "javascript:alert(1)"} {
		if r := validateOutboundURL(ctx, bad); r == "" {
			t.Errorf("negative: %q must be refused", bad)
		}
	}
}

func TestRESTIntegrations(t *testing.T) {
	real := lookupHost
	defer func() { lookupHost = real }()
	lookupHost = func(_ context.Context, host string) ([]string, error) {
		return []string{"93.184.216.34"}, nil
	}
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	a, bt := login(t, w, "ann"), login(t, w, "bob")
	b := newRESTBoard(t, w, a, "Hooks")
	base := w.srv.URL + "/api/boards/" + b.ID + "/integrations"
	var created struct {
		ID string `json:"_id"`
	}
	if code := call(t, "POST", base, a, `{"url":"https://chat.example/hooks/SECRET"}`, &created); code != 200 || created.ID == "" {
		t.Fatalf("create: %d", code)
	}
	var bad map[string]any
	if code := call(t, "POST", base, a, `{"url":"http://169.254.169.254/"}`, &bad); code != 400 || bad["error"] != "invalid-webhook-url" {
		t.Fatalf("negative: an internal address: %d %v", code, bad)
	}
	one := base + "/" + created.ID
	call(t, "PUT", one, a, `{"title":"Chat","token":"tok","activities":["createCard"]}`, nil)
	var got map[string]any
	call(t, "GET", one, a, "", &got)
	if got["url"] != "https://chat.example/hooks/SECRET" || got["title"] != "Chat" || got["token"] != nil {
		t.Fatalf("a board admin reads everything but the token: %v", got)
	}
	if code := call(t, "PUT", one, a, `{"url":"http://10.0.0.1/"}`, nil); code != 400 {
		t.Fatalf("negative: changing the url to an internal one: %d", code)
	}
	setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: true, IsReadOnly: true})
	var list []map[string]any
	if code := call(t, "GET", base, bt, "", &list); code != 200 || len(list) != 1 || list[0]["url"] != nil || list[0]["title"] != nil {
		t.Fatalf("negative: a member who is not an admin never gets the URL (HookUrlBleed): %d %v", code, list)
	}
	if code := call(t, "PUT", one, bt, `{"enabled":false}`, nil); code != 403 {
		t.Fatalf("negative: only a board admin changes it: %d", code)
	}
	var acts map[string]any
	call(t, "POST", one+"/activities", a, `{"activities":["addComment","createCard"]}`, &acts)
	if l, _ := acts["activities"].([]any); len(l) != 2 {
		t.Fatalf("activities added, without doubles: %v", acts)
	}
	call(t, "DELETE", one+"/activities", a, `{"activities":["createCard"]}`, &acts)
	if l, _ := acts["activities"].([]any); len(l) != 1 || l[0] != "addComment" {
		t.Fatalf("activities removed: %v", acts)
	}
	if code := call(t, "GET", base+"/nosuchhook000001", a, "", nil); code != 404 {
		t.Fatalf("negative: no such integration: %d", code)
	}
	if code := call(t, "DELETE", one, a, "", nil); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	call(t, "GET", base, a, "", &list)
	if len(list) != 0 {
		t.Fatal("deleted")
	}
}
