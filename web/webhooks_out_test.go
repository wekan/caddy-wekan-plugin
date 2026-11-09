package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// webhookSink is a receiver that keeps what each path got.
type webhookSink struct {
	mu   sync.Mutex
	got  map[string][]map[string]any
	toks map[string]string
	srv  *httptest.Server
}

func newWebhookSink(t *testing.T) *webhookSink {
	k := &webhookSink{got: map[string][]map[string]any{}, toks: map[string]string{}}
	k.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/target", http.StatusFound)
			return
		}
		var body map[string]any
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
		k.mu.Lock()
		k.got[r.URL.Path] = append(k.got[r.URL.Path], body)
		k.toks[r.URL.Path] = r.Header.Get("X-Wekan-Token")
		k.mu.Unlock()
	}))
	t.Cleanup(k.srv.Close)
	return k
}

func (k *webhookSink) bodies(path string) []map[string]any {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]map[string]any(nil), k.got[path]...)
}

// Outgoing webhooks: an activity goes to the board's enabled webhooks that
// asked for it - with WeKan's payload and the webhook's token - never to a
// disabled one, a bot, one that asked for something else, or another board's.
func TestOutgoingWebhooks(t *testing.T) {
	sink := newWebhookSink(t)
	webhookAllowLoopbackForTests = true
	t.Cleanup(func() { webhookAllowLoopbackForTests = false })
	t.Setenv("ROOT_URL", "https://wekan.example")
	w := newTestWeKan(t)
	ann := w.seedUser(t, "ann", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, ann, "Hooks")
	other, _ := w.s.CreateBoard(ctx, ann, "Other")
	hook := func(id, boardID, path string, extra bson.M) {
		doc := bson.M{"_id": id, "boardId": boardID, "url": sink.srv.URL + path, "type": "outgoing-webhooks",
			"enabled": true, "activities": bson.A{"all"}, "userId": ann.ID, "token": "tok-" + id}
		for k, v := range extra {
			doc[k] = v
		}
		if _, err := w.db.Collection("integrations").InsertOne(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}
	hook("h1", b.ID, "/all", nil)
	hook("h2", b.ID, "/off", bson.M{"enabled": false})
	hook("h3", b.ID, "/bot", bson.M{"bot": true})
	hook("h4", b.ID, "/comments", bson.M{"activities": bson.A{"act-addComment"}})
	hook("h5", other.ID, "/other", nil)
	hook("h6", "_global", "/global", nil)
	l, _ := w.s.AddList(ctx, b, "Todo")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	c, _ := w.s.AddCard(ctx, b, ann, l.ID, sw.ID, "Hello")
	var got map[string]any
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, body := range sink.bodies("/all") {
			if body["description"] == "act-createCard" {
				got = body
			}
		}
		if got != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got == nil {
		t.Fatalf("the board's webhook got the new card: %v", sink.bodies("/all"))
	}
	text, _ := got["text"].(string)
	for k, want := range map[string]any{"card": "Hello", "cardId": c.ID, "boardId": b.ID, "board": "Hooks", "list": "Todo", "username": "ann",
		"url": "https://wekan.example/b/" + b.ID + "/" + b.Slug + "/" + c.ID} {
		if got[k] != want {
			t.Errorf("%s: %v, want %v", k, got[k], want)
		}
	}
	if !strings.Contains(text, `"Hello"`) || !strings.HasSuffix(text, "\nhttps://wekan.example/b/"+b.ID+"/"+b.Slug+"/"+c.ID) {
		t.Errorf("the sentence names the card and ends with its link: %q", text)
	}
	sink.mu.Lock()
	tok := sink.toks["/all"]
	sink.mu.Unlock()
	if tok != "tok-h1" {
		t.Errorf("X-Wekan-Token: %q", tok)
	}
	if len(sink.bodies("/global")) == 0 {
		t.Error("a global webhook gets every board's activity")
	}
	time.Sleep(200 * time.Millisecond) // anything that was going to arrive has
	for _, path := range []string{"/off", "/bot", "/comments", "/other"} {
		if n := len(sink.bodies(path)); n != 0 {
			t.Errorf("negative: %s got %d", path, n)
		}
	}
}

// The guard at delivery: a loopback address is refused at the socket, a
// redirect is not followed, and a refusal is recorded as IntegrationBleed.
func TestOutgoingWebhookSSRFGuard(t *testing.T) {
	sink := newWebhookSink(t)
	w := newTestWeKan(t)
	ctx := context.Background()
	// The socket check itself: 127.0.0.1 by number.
	req, _ := http.NewRequest(http.MethodPost, sink.srv.URL+"/direct", bytes.NewReader([]byte("{}")))
	if _, err := webhookClient().Do(req); err == nil || !errors.Is(err, errSSRFBlocked) {
		t.Errorf("negative: a loopback address is refused at the socket: %v", err)
	}
	// Through delivery, by number and by name.
	for _, target := range []string{sink.srv.URL + "/direct", strings.Replace(sink.srv.URL, "127.0.0.1", "localhost", 1) + "/named"} {
		hook := bson.M{"_id": "hx", "url": target}
		if err := w.s.deliverWebhook(ctx, hook, []byte("{}")); err == nil {
			t.Errorf("negative: %s is refused", target)
		}
	}
	if n := len(sink.bodies("/direct")) + len(sink.bodies("/named")); n != 0 {
		t.Errorf("negative: nothing arrived: %d", n)
	}
	var row bson.M
	if err := w.db.Collection("eventlog").FindOne(ctx, bson.M{"bleed": "IntegrationBleed"}).Decode(&row); err != nil || row["action"] != "blocked" {
		t.Errorf("the refusal is in Problems: %v %v", err, row)
	}
	// A redirect is not followed, even where the address may be called.
	webhookAllowLoopbackForTests = true
	t.Cleanup(func() { webhookAllowLoopbackForTests = false })
	if err := w.s.deliverWebhook(ctx, bson.M{"_id": "hr", "url": sink.srv.URL + "/redirect"}, []byte("{}")); err == nil {
		t.Error("negative: a redirect is an answer, not followed")
	}
	if len(sink.bodies("/target")) != 0 {
		t.Error("negative: the redirect's target was not called")
	}
}

// Negative, for the whole plugin: an outbound HTTP client is made only where
// its address is known not to be a user's - the guarded webhook client, the
// administrator's own OIDC provider and Cloudflare's API. A new one, or a
// bare http.Get, is a new SSRF to judge, and fails here until it is.
func TestNoUnguardedOutboundHTTP(t *testing.T) {
	allowed := map[string]string{
		"web/webhooks_out.go": "the SSRF-guarded webhook client",
		"web/oidc.go":         "OAUTH2_* settings: the administrator's identity provider",
		"cloudflare.go":       "api.cloudflare.com, with the administrator's token",
	}
	pattern := regexp.MustCompile(`http\.(Get|Post|PostForm|Head)\(|http\.DefaultClient|http\.Client\{`)
	root := ".."
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "gitea" || d.Name() == "node_modules" || strings.HasPrefix(d.Name(), ".")) && path != root {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if pattern.Match(data) {
			if _, ok := allowed[filepath.ToSlash(rel)]; !ok {
				t.Errorf("negative: %s makes an outbound HTTP request outside the SSRF guard", rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
