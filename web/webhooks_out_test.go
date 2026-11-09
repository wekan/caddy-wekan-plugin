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
	"sort"
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

// "Leave my name out of outgoing webhooks": an actor who asked is "A member"
// in the sentence and nowhere in the payload, and so is a member a join is
// about; somebody who did not ask is named as before.
func TestOutgoingWebhookHideIdentity(t *testing.T) {
	sink := newWebhookSink(t)
	webhookAllowLoopbackForTests = true
	t.Cleanup(func() { webhookAllowLoopbackForTests = false })
	w := newTestWeKan(t)
	ann := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	carl := w.seedUser(t, "carl", "pw")
	ctx := context.Background()
	for _, u := range []*User{ann, bob} {
		_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": u.ID}, bson.M{"$set": bson.M{"profile.webhookHideIdentity": true}})
	}
	b, _ := w.s.CreateBoard(ctx, ann, "Private hooks")
	_, _ = w.db.Collection("integrations").InsertOne(ctx, bson.M{"_id": "hp", "boardId": b.ID, "url": sink.srv.URL + "/p",
		"type": "outgoing-webhooks", "enabled": true, "activities": bson.A{"all"}, "userId": carl.ID})
	l, _ := w.s.AddList(ctx, b, "Todo")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	c, _ := w.s.AddCard(ctx, b, ann, l.ID, sw.ID, "Quiet card")
	w.s.activity(ctx, carl.ID, "joinMember", bson.M{"boardId": b.ID, "cardId": c.ID, "memberId": bob.ID})
	find := func(desc string) map[string]any {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			for _, body := range sink.bodies("/p") {
				if body["description"] == desc {
					return body
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("%s delivered: %v", desc, sink.bodies("/p"))
		return nil
	}
	created := find("act-createCard")
	for _, k := range []string{"user", "username", "userId", "userEmails"} {
		if _, has := created[k]; has {
			t.Errorf("negative: a hidden actor's %s: %v", k, created[k])
		}
	}
	if text, _ := created["text"].(string); !strings.HasPrefix(text, "A member ") || strings.Contains(text, "ann") {
		t.Errorf("the actor is A member: %q", text)
	}
	joined := find("act-joinMember")
	for _, k := range []string{"member", "memberUsername", "memberId"} {
		if _, has := joined[k]; has {
			t.Errorf("negative: a hidden member's %s: %v", k, joined[k])
		}
	}
	if joined["username"] != "carl" {
		t.Errorf("an actor who did not ask is named: %v", joined["username"])
	}
}

// A two-way webhook's answer edits the comment the event was about - on its
// own board only, an existing comment only - and a one-way webhook's answer
// changes nothing.
func TestTwoWayWebhookReply(t *testing.T) {
	var mu sync.Mutex
	reply := map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(srv.Close)
	webhookAllowLoopbackForTests = true
	t.Cleanup(func() { webhookAllowLoopbackForTests = false })
	w := newTestWeKan(t)
	ann := w.seedUser(t, "ann", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, ann, "Chat")
	other, _ := w.s.CreateBoard(ctx, ann, "Elsewhere")
	l, _ := w.s.AddList(ctx, b, "Todo")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	c, _ := w.s.AddCard(ctx, b, ann, l.ID, sw.ID, "Talk")
	// A real card and comment on another board, so only the webhook's own
	// board stands between a reply and it.
	ol, _ := w.s.AddList(ctx, other, "There")
	var osw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": other.ID}).Decode(&osw)
	oc, _ := w.s.AddCard(ctx, other, ann, ol.ID, osw.ID, "Other talk")
	_, _ = w.db.Collection("card_comments").InsertOne(ctx, bson.M{"_id": "cOther", "cardId": oc.ID, "boardId": other.ID, "text": "untouched"})
	hook := bson.M{"_id": "h2", "boardId": b.ID, "url": srv.URL + "/two", "type": "bidirectional-webhooks", "enabled": true}
	text := func(id string) string {
		var cm struct {
			Text string `bson:"text"`
		}
		_ = w.db.Collection("card_comments").FindOne(ctx, bson.M{"_id": id}).Decode(&cm)
		return cm.Text
	}
	id, err := w.s.AddComment(ctx, b, ann, c, "hello chat")
	if err != nil {
		t.Fatal(err)
	}
	send := func(r map[string]any, h bson.M) {
		mu.Lock()
		reply = r
		mu.Unlock()
		if err := w.s.deliverWebhook(ctx, h, []byte("{}")); err != nil {
			t.Fatal(err)
		}
	}
	send(map[string]any{"commentId": id, "cardId": c.ID, "boardId": b.ID, "comment": "edited by chat"}, hook)
	if got := text(id); got != "edited by chat" {
		t.Errorf("the reply edits the comment: %q", got)
	}
	for name, r := range map[string]map[string]any{
		"another board":      {"commentId": "cOther", "cardId": oc.ID, "boardId": other.ID, "comment": "hijacked"},
		"another card":       {"commentId": id, "cardId": "nocard", "boardId": b.ID, "comment": "hijacked"},
		"no such comment":    {"commentId": "nosuchcomment1", "cardId": c.ID, "boardId": b.ID, "comment": "planted"},
		"the board mismatch": {"commentId": "cOther", "cardId": oc.ID, "boardId": b.ID, "comment": "hijacked"},
	} {
		send(r, hook)
		if text("cOther") != "untouched" || text(id) != "edited by chat" {
			t.Errorf("negative: %s changes nothing", name)
		}
	}
	if n, _ := w.db.Collection("card_comments").CountDocuments(ctx, bson.M{"text": "planted"}); n != 0 {
		t.Error("negative: a reply makes no new comment")
	}
	oneWay := bson.M{"_id": "h1", "boardId": b.ID, "url": srv.URL + "/one", "type": "outgoing-webhooks", "enabled": true}
	send(map[string]any{"commentId": id, "cardId": c.ID, "boardId": b.ID, "comment": "from a one-way hook"}, oneWay)
	if text(id) != "edited by chat" {
		t.Error("negative: a one-way webhook's answer changes nothing")
	}
}

// #1297: a webhook marked for it also sends the card's members and assignees
// a Mattermost direct message each - never the actor, never a name
// Mattermost could not have - and a webhook not marked sends none.
func TestMattermostDirectMessages(t *testing.T) {
	sink := newWebhookSink(t)
	webhookAllowLoopbackForTests = true
	t.Cleanup(func() { webhookAllowLoopbackForTests = false })
	w := newTestWeKan(t)
	ann := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	carl := w.seedUser(t, "carl", "pw")
	ctx := context.Background()
	_, _ = w.db.Collection("users").InsertOne(ctx, bson.M{"_id": "badnameuser0001", "username": "bad name!"})
	b, _ := w.s.CreateBoard(ctx, ann, "Chatty")
	for id, dm := range map[string]bool{"mm": true, "plain": false} {
		_, _ = w.db.Collection("integrations").InsertOne(ctx, bson.M{"_id": id, "boardId": b.ID, "url": sink.srv.URL + "/" + id,
			"type": "outgoing-webhooks", "enabled": true, "activities": bson.A{"act-addComment"}, "userId": ann.ID, "mattermostDirectMessages": dm})
	}
	l, _ := w.s.AddList(ctx, b, "Todo")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	c, _ := w.s.AddCard(ctx, b, ann, l.ID, sw.ID, "Discuss")
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": c.ID}, bson.M{"$set": bson.M{
		"members": bson.A{ann.ID, bob.ID, "badnameuser0001"}, "assignees": bson.A{carl.ID, bob.ID}}})
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": c.ID}).Decode(c)
	if _, err := w.s.AddComment(ctx, b, ann, c, "Ping"); err != nil {
		t.Fatal(err)
	}
	channels := func(path string) []string {
		var out []string
		for _, body := range sink.bodies(path) {
			ch, _ := body["channel"].(string)
			out = append(out, ch)
		}
		sort.Strings(out)
		return out
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(sink.bodies("/mm")) < 3 {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if got := strings.Join(channels("/mm"), ","); got != ",@bob,@carl" {
		t.Errorf("the channel message and one each to bob and carl, not ann or a bad name: %q", got)
	}
	if got := channels("/plain"); len(got) != 1 || got[0] != "" {
		t.Errorf("negative: an unmarked webhook sends no direct message: %v", got)
	}
}
