package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// WeKan's tests/botRuns.test.cjs: what a report may change.
func TestBotRunUpdates(t *testing.T) {
	if _, err := cleanBotRunUpdate(map[string]any{"status": "hacked"}); err == nil {
		t.Error("negative: an unknown status")
	}
	if _, err := cleanBotRunUpdate(map[string]any{"tokens": -1.0}); err == nil {
		t.Error("negative: a negative count")
	}
	if _, err := cleanBotRunUpdate(map[string]any{"options": "a"}); err == nil {
		t.Error("negative: options that are not a list")
	}
	opts := []any{}
	for i := 0; i < 12; i++ {
		opts = append(opts, map[string]any{"value": strings.Repeat("v", 300), "label": "L"})
	}
	u, err := cleanBotRunUpdate(map[string]any{"status": "awaiting_input", "question": "Which?", "options": opts, "text": strings.Repeat("x", 5000)})
	if err != nil {
		t.Fatal(err)
	}
	if o := u.set["options"].(bson.A); len(o) != 8 || len([]rune(o[0].(bson.M)["value"].(string))) != 200 {
		t.Errorf("options bounded to 8 of 200 characters: %v", len(o))
	}
	if len([]rune(u.event["text"].(string))) != 4000 {
		t.Error("a text bounded to 4000")
	}
	if u, _ = cleanBotRunUpdate(map[string]any{"status": "running"}); u.set["question"] != nil || len(u.set["options"].(bson.A)) != 0 {
		t.Error("a status other than awaiting_input clears the question")
	}
	for _, f := range botFinal {
		if botCanMove(f, "running") {
			t.Errorf("negative: %s stays finished", f)
		}
	}
	if !botCanMove("queued", "running") || !botCanMove("done", "done") {
		t.Error("an active run moves, a finished one may repeat itself")
	}
}

// Bots: a board admin adds one and hands it a card; it is sent the card -
// with its token and where to report - and never the board's activity; its
// runner reports with write access; the admin stops, replies and answers. A
// normal member can do none of the admin's part, and a bot to this machine is
// refused.
func TestBots(t *testing.T) {
	realLookup := lookupHost
	t.Cleanup(func() { lookupHost = realLookup })
	lookupHost = func(_ context.Context, host string) ([]string, error) { return []string{"93.184.216.34"}, nil }
	sink := newWebhookSink(t)
	t.Setenv("ROOT_URL", "https://wekan.example")
	w := newTestWeKan(t)
	ann := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	ctx := context.Background()
	at := login(t, w, "ann")
	bt := login(t, w, "bob")
	rb := newRESTBoard(t, w, at, "Bots")
	setMember(t, w, rb.ID, Member{UserID: bob.ID, IsActive: true})
	methods := func(user string) func(method, args string) map[string]any {
		c := w.client(t)
		body(t, w.signIn(t, c, user, "pw"))
		return func(method, args string) map[string]any {
			req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/"+url.PathEscape(method), strings.NewReader(args))
			req.Header.Set("Content-Type", "application/json")
			res, err := c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			var out map[string]any
			_ = json.NewDecoder(res.Body).Decode(&out)
			return out
		}
	}
	annCall, bobCall := methods("ann"), methods("bob")
	// Adding: an admin's bot listens to the dispatch only; not a member's, not to this machine.
	added := annCall("/integrations/insert", `[{"boardId":"`+rb.ID+`","title":"Coder","url":"https://bots.example/run","token":"sig","bot":true,"activities":["all"]}]`)
	botID, _ := added["result"].(string)
	var stored bson.M
	if botID == "" || w.db.Collection("integrations").FindOne(ctx, bson.M{"_id": botID}).Decode(&stored) != nil {
		t.Fatalf("a board admin adds a bot: %v", added)
	}
	if acts, _ := stored["activities"].(bson.A); len(acts) != 1 || acts[0] != "bot-dispatch" || stored["bot"] != true {
		t.Errorf("negative: a bot listens to the dispatch only: %v", stored["activities"])
	}
	if out := bobCall("/integrations/insert", `[{"boardId":"`+rb.ID+`","url":"https://bots.example/x","bot":true}]`); out["result"] != nil {
		t.Errorf("negative: a normal member adds no bot: %v", out)
	}
	lookupHost = func(_ context.Context, host string) ([]string, error) { return []string{"127.0.0.1"}, nil }
	if out := annCall("/integrations/insert", `[{"boardId":"`+rb.ID+`","url":"http://internal.example/","bot":true}]`); out["result"] != nil {
		t.Errorf("negative: a bot on this machine is refused: %v", out)
	}
	lookupHost = func(_ context.Context, host string) ([]string, error) { return []string{"93.184.216.34"}, nil }
	// Point the stored bot at the test's receiver, as if it resolved there.
	webhookAllowLoopbackForTests = true
	t.Cleanup(func() { webhookAllowLoopbackForTests = false })
	_, _ = w.db.Collection("integrations").UpdateOne(ctx, bson.M{"_id": botID}, bson.M{"$set": bson.M{"url": sink.srv.URL + "/bot"}})
	// Dispatch.
	if out := bobCall("botDispatch", `["`+rb.Card+`","`+botID+`"]`); out["result"] != nil {
		t.Errorf("negative: a normal member hands no card over: %v", out)
	}
	out := annCall("botDispatch", `["`+rb.Card+`","`+botID+`"]`)
	runID, _ := out["result"].(string)
	if runID == "" {
		t.Fatalf("dispatch: %v", out)
	}
	if again := annCall("botDispatch", `["`+rb.Card+`","`+botID+`"]`); again["result"] != runID {
		t.Errorf("a card with that bot keeps its run: %v", again)
	}
	wait := func(event string) map[string]any {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			for _, b := range sink.bodies("/bot") {
				if b["event"] == event {
					return b
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("the bot was sent %s: %v", event, sink.bodies("/bot"))
		return nil
	}
	d := wait("bot.dispatch")
	card, _ := d["card"].(map[string]any)
	if d["runId"] != runID || card["title"] != "C" || d["report"] != "https://wekan.example/api/boards/"+rb.ID+"/cards/"+rb.Card+"/bot-runs/"+runID {
		t.Errorf("the dispatch: %v", d)
	}
	sink.mu.Lock()
	tok := sink.toks["/bot"]
	sink.mu.Unlock()
	if tok != "sig" {
		t.Errorf("signed with its token: %q", tok)
	}
	// The board's activity never reaches a bot.
	call(t, "POST", w.srv.URL+"/api/boards/"+rb.ID+"/lists/"+rb.List+"/cards", at, `{"title":"More","swimlaneId":"`+rb.Swimlane+`"}`, nil)
	time.Sleep(200 * time.Millisecond)
	for _, b := range sink.bodies("/bot") {
		if ev, _ := b["event"].(string); !strings.HasPrefix(ev, "bot.") {
			t.Errorf("negative: a bot is sent the board's activity: %v", b)
		}
	}
	// The runner reports: with write access, cleaned.
	report := w.srv.URL + "/api/boards/" + rb.ID + "/cards/" + rb.Card + "/bot-runs/" + runID
	var res map[string]any
	if code := call(t, "POST", report, at, `{"status":"awaiting_input","question":"Which?","options":["a.js","b.js"],"text":"asking","costUsd":0.12}`, &res); code != 200 || res["status"] != "awaiting_input" {
		t.Errorf("a report: %d %v", code, res)
	}
	if code := call(t, "POST", report, at, `{"status":"hacked"}`, nil); code != 400 {
		t.Errorf("negative: an unknown status: %d", code)
	}
	if code := call(t, "POST", report, "", `{"status":"running"}`, nil); code == 200 {
		t.Error("negative: no token, no report")
	}
	setMember(t, w, rb.ID, Member{UserID: bob.ID, IsActive: true, IsReadOnly: true})
	if code := call(t, "POST", report, bt, `{"status":"running"}`, nil); code == 200 {
		t.Error("negative: a read-only member reports nothing")
	}
	setMember(t, w, rb.ID, Member{UserID: bob.ID, IsActive: true})
	// The admin answers: a comment, the bot told, the run running again.
	if out := annCall("botAnswer", `["`+runID+`","a.js"]`); out["result"] == nil {
		t.Errorf("answer: %v", out)
	}
	if a := wait("bot.answer"); a["value"] != "a.js" || a["text"] != "↳ a.js" {
		t.Errorf("the answer told: %v", a)
	}
	var run bson.M
	_ = w.db.Collection("botRuns").FindOne(ctx, bson.M{"_id": runID}).Decode(&run)
	if run["status"] != "running" || run["costUsd"] != 0.12 {
		t.Errorf("after the answer: %v", run)
	}
	if out := bobCall("botReply", `["`+runID+`","from bob"]`); out["result"] != nil {
		t.Errorf("negative: a normal member replies nothing: %v", out)
	}
	if out := annCall("botReply", `["`+runID+`","please also test"]`); out["result"] == nil {
		t.Errorf("reply: %v", out)
	}
	if n, _ := w.db.Collection("card_comments").CountDocuments(ctx, bson.M{"cardId": rb.Card, "userId": ann.ID, "text": "please also test"}); n != 1 {
		t.Error("a reply is the admin's own comment")
	}
	// Stop, and a finished run stays finished.
	if out := bobCall("botStop", `["`+runID+`"]`); out["result"] != nil {
		t.Errorf("negative: a normal member stops nothing: %v", out)
	}
	if out := annCall("botStop", `["`+runID+`"]`); out["result"] != "stopped" {
		t.Errorf("stop: %v", out)
	}
	wait("bot.stop")
	if code := call(t, "POST", report, at, `{"status":"running"}`, nil); code != 400 {
		t.Errorf("negative: a stopped run stays stopped: %d", code)
	}
}

// A Rule's "Hand the card to bot" (dispatchBot): a run from the rule, and a
// bot of another board is never handed the card.
func TestBotRuleAction(t *testing.T) {
	sink := newWebhookSink(t)
	webhookAllowLoopbackForTests = true
	t.Cleanup(func() { webhookAllowLoopbackForTests = false })
	w := newTestWeKan(t)
	ann := w.seedUser(t, "ann", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, ann, "Rules")
	other, _ := w.s.CreateBoard(ctx, ann, "Elsewhere")
	for id, boardID := range map[string]string{"botHere": b.ID, "botThere": other.ID} {
		_, _ = w.db.Collection("integrations").InsertOne(ctx, bson.M{"_id": id, "boardId": boardID, "url": sink.srv.URL + "/" + id,
			"type": "outgoing-webhooks", "enabled": true, "bot": true, "activities": bson.A{"bot-dispatch"}, "userId": ann.ID})
	}
	addRule(t, w, b.ID, "rbot", bson.M{"activityType": "createCard", "cardTitle": "*"}, bson.M{"actionType": "dispatchBot", "integrationId": "botHere"}, true)
	addRule(t, w, b.ID, "rbot2", bson.M{"activityType": "createCard", "cardTitle": "*"}, bson.M{"actionType": "dispatchBot", "integrationId": "botThere"}, true)
	l, _ := w.s.AddList(ctx, b, "Todo")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	c, _ := w.s.AddCard(ctx, b, ann, l.ID, sw.ID, "Automate me")
	var run bson.M
	if err := w.db.Collection("botRuns").FindOne(ctx, bson.M{"cardId": c.ID, "integrationId": "botHere"}).Decode(&run); err != nil || run["source"] != "rule" {
		t.Fatalf("the rule's run: %v %v", err, run)
	}
	if n, _ := w.db.Collection("botRuns").CountDocuments(ctx, bson.M{"integrationId": "botThere"}); n != 0 {
		t.Error("negative: another board's bot is never handed the card")
	}
	time.Sleep(200 * time.Millisecond)
	if len(sink.bodies("/botThere")) != 0 || len(sink.bodies("/botHere")) != 1 {
		t.Errorf("only the board's own bot is sent the card: %d %d", len(sink.bodies("/botHere")), len(sink.bodies("/botThere")))
	}
}

// The Rules page's Bots view: for a board admin, the bots, the lists as
// columns with each card's run - its words, its question and answers - and a
// card's drawer with its comments and the run's events; for anybody else,
// none of it.
func TestRulesBotsView(t *testing.T) {
	w := newTestWeKan(t)
	ann := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, ann, "Botboard")
	setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: true})
	l, _ := w.s.AddList(ctx, b, "In progress")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	c, _ := w.s.AddCard(ctx, b, ann, l.ID, sw.ID, "Fix login")
	_, _ = w.db.Collection("integrations").InsertOne(ctx, bson.M{"_id": "botv", "boardId": b.ID, "title": "Coder", "url": "https://bots.example/run",
		"type": "outgoing-webhooks", "enabled": true, "bot": true, "activities": bson.A{"bot-dispatch"}})
	now := time.Now().UTC()
	_, _ = w.db.Collection("botRuns").InsertOne(ctx, bson.M{"_id": "runv", "boardId": b.ID, "cardId": c.ID, "integrationId": "botv",
		"status": "awaiting_input", "question": "Which file?", "options": bson.A{bson.M{"value": "a.js", "label": "a.js"}},
		"createdAt": now, "updatedAt": now, "costUsd": 0.12,
		"events": bson.A{bson.M{"text": "dispatched", "status": "queued", "at": now.Add(-time.Minute)}, bson.M{"tool": "Bash", "text": "npm test", "status": "running", "at": now}}})
	_, _ = w.db.Collection("card_comments").InsertOne(ctx, bson.M{"_id": "cmv", "cardId": c.ID, "boardId": b.ID, "userId": ann.ID, "text": "please fix", "createdAt": now.Add(-2 * time.Minute)})
	page := func(user string) string {
		cl := w.client(t)
		body(t, w.signIn(t, cl, user, "pw"))
		res, err := cl.Get(w.srv.URL + "/b/" + b.ID + "/" + b.Slug + "/rules?view=bots&card=" + c.ID)
		if err != nil {
			t.Fatal(err)
		}
		return body(t, res)
	}
	html := page("ann")
	for _, want := range []string{`data-pane="bots"`, "Coder", "bots.example", "In progress", "Fix login", "Which file?",
		`data-value="a.js"`, "please fix", "npm test", "$0.12", "js-bots-reply", "js-bots-dispatch", `data-swimlane-id="` + sw.ID + `"`} {
		if !strings.Contains(html, want) {
			t.Errorf("the Bots view has %q", want)
		}
	}
	if !strings.Contains(html, "Waiting for you") && !strings.Contains(html, "bots-status-awaiting_input") {
		t.Error("the run's state in words")
	}
	if strings.Contains(html, "bots-status-awaiting_input<") {
		t.Error("the state's key is translated, not shown")
	}
	if m := page("bob"); strings.Contains(m, "Coder") || strings.Contains(m, "bots.example") || strings.Contains(m, "Which file?") {
		t.Error("negative: a normal member sees no bot, no address, no run")
	}
}
