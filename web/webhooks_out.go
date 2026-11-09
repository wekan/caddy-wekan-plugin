package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Outgoing webhooks, as Node.js WeKan's server/models/activities.js
// (Activities.after.insert) and server/notifications/outgoing.js: after an
// activity is recorded, every enabled webhook of its board - and every global
// one - that asked for that activity, or for all, is sent WeKan's payload:
// the sentence in the webhook user's language, then the activity's ids, names
// and links, then its description. A bot is never sent the board's activity.
//
// Delivery is in the background and never fails what caused the activity
// (WeKan's #1402). Its client is the SSRF guard at delivery time (WeKan's
// server/lib/ssrfGuard.js): http or https only, each address the connection
// is actually made to checked against ssrf.go's blocked ranges - so a host
// name that resolves, or re-resolves, to this machine or a private network is
// refused at the socket - no redirect followed (FollowBleed), and 30 seconds
// for the whole request. A refused address is recorded in Admin Panel /
// Problems as IntegrationBleed (ssrf.webhook).
//
// Not here yet: Notification delivery's grouping and schedule for webhooks,
// "Leave my name out of outgoing webhooks", Mattermost direct messages, a
// two-way webhook's reply editing the comment, and an outbound proxy.

var errSSRFBlocked = errors.New("SSRF_GUARD: the address is a private, loopback or reserved one")

// webhookAllowLoopbackForTests lets a test's own server on 127.0.0.1 receive;
// nothing else sets it.
var webhookAllowLoopbackForTests = false

// webhookClient is the guarded client.
func webhookClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: func(network, address string, _ syscall.RawConn) error {
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
	}}
	return &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{Proxy: nil, DialContext: dialer.DialContext, TLSHandshakeTimeout: 10 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second, MaxResponseHeaderBytes: 64 << 10},
	}
}

// webhookAttributes is WeKan's WEBHOOKS_ATTRIBUTES default.
var webhookAttributes = []string{"cardId", "listId", "oldListId", "boardId", "comment", "user", "username", "card",
	"commentId", "swimlaneId", "customField", "customFieldValue", "labelId", "label", "attachmentId",
	"list", "board", "swimlane", "member", "memberUsername", "assigneeId", "assignee", "assigneeUsername", "url"}

// webhookQuoted are the params WeKan puts in quotes in the sentence.
var webhookQuoted = map[string]bool{"card": true, "list": true, "oldList": true, "board": true, "oldBoard": true,
	"comment": true, "checklist": true, "swimlane": true, "oldSwimlane": true, "labelId": true, "label": true,
	"attachment": true, "attachmentId": true}

// webhookParams are WeKan's prepareActivityNotification params of an activity.
func (s *Server) webhookParams(ctx context.Context, a bson.M) map[string]any {
	p := map[string]any{"activityId": docString(a, "_id")}
	userName := func(id string) (string, string, bool) {
		var u User
		if !validID(id) || s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&u) != nil {
			return "", "", false
		}
		return u.Name(), u.Username, true
	}
	if id := docString(a, "userId"); id != "" {
		if name, login, ok := userName(id); ok {
			p["user"], p["username"], p["userId"] = name, login, id
		}
	}
	root := strings.TrimRight(os.Getenv("ROOT_URL"), "/")
	var b Board
	boardID := docString(a, "boardId")
	if boardID != "" && s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(boardID)}).Decode(&b) == nil {
		p["board"], p["boardId"] = b.Title, boardID
		if root != "" {
			p["url"] = root + "/b/" + b.ID + "/" + b.Slug
			p["boardUrl"] = p["url"]
		}
	}
	if id := docString(a, "oldBoardId"); id != "" {
		if t := s.titleOfID(ctx, "boards", id); t != "" {
			p["oldBoard"], p["oldBoardId"] = t, id
		}
	}
	for _, f := range [][3]string{{"memberId", "member", "memberUsername"}, {"assigneeId", "assignee", "assigneeUsername"}} {
		if id := docString(a, f[0]); id != "" {
			name, login, _ := userName(id)
			p[f[0]], p[f[1]], p[f[2]] = id, name, login
		}
	}
	for _, f := range [][3]string{{"listId", "list", "lists"}, {"oldListId", "oldList", "lists"}, {"swimlaneId", "swimlane", "swimlanes"},
		{"oldSwimlaneId", "oldSwimlane", "swimlanes"}, {"checklistId", "checklist", "checklists"}, {"checklistItemId", "checklistItem", "checklistItems"}} {
		if id := docString(a, f[0]); id != "" {
			if t := s.titleOfID(ctx, f[2], id); t != "" {
				p[f[1]], p[f[0]] = t, id
			}
		}
	}
	if id := docString(a, "cardId"); id != "" {
		var c Card
		if s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&c) == nil && c.BoardID == boardID {
			p["card"], p["cardId"], p["description"] = c.Title, id, c.Description
			if root != "" && b.ID != "" {
				p["url"] = root + "/b/" + b.ID + "/" + b.Slug + "/" + c.ID
				p["cardUrl"] = p["url"]
			}
		}
	}
	if id := docString(a, "commentId"); validID(id) {
		var cm struct {
			Text string `bson:"text"`
		}
		if s.db.Collection("card_comments").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&cm) == nil {
			p["comment"], p["commentId"] = cm.Text, id
		}
	}
	if id := docString(a, "attachmentId"); id != "" {
		p["attachmentId"] = id
		if n := docString(a, "attachmentName"); n != "" {
			p["attachment"] = n
		}
	}
	if id := docString(a, "labelId"); id != "" {
		p["labelId"] = id
		for _, l := range b.Labels {
			if l.ID == id {
				p["label"] = l.Name
				if l.Name == "" {
					p["label"] = l.Color
				}
			}
		}
	}
	if id := docString(a, "customFieldId"); validID(id) {
		var f struct {
			Name      string `bson:"name"`
			AdminOnly bool   `bson:"adminOnly"`
		}
		if s.db.Collection("customFields").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&f) == nil {
			p["customField"], p["customFieldId"] = f.Name, id
			// #3141: an admin-only field's value never leaves in a webhook.
			if v, ok := a["value"]; ok && v != nil && !f.AdminOnly {
				p["customFieldValue"] = jsString(v)
			}
		}
	}
	return p
}

// webhookBody is WeKan's prepareOutgoingWebhook: nil when the webhook user is
// unknown or the sentence is "-".
func (s *Server) webhookBody(ctx context.Context, hook bson.M, description string, p map[string]any) []byte {
	userID, _ := p["userId"].(string)
	if userID == "" {
		userID = docString(hook, "userId")
	}
	var u User
	if !validID(userID) || s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(userID)}).Decode(&u) != nil {
		return nil
	}
	lang := u.Profile.Language
	if lang == "" {
		lang = "en"
	}
	sentence := tr(lang, msg(description))
	if sentence == "-" {
		return nil
	}
	for k, v := range p {
		text, ok := v.(string)
		if !ok {
			continue
		}
		if webhookQuoted[k] {
			text = `"` + text + `"`
		}
		sentence = strings.ReplaceAll(sentence, "__"+k+"__", text)
	}
	user, _ := p["user"].(string)
	link, _ := p["url"].(string)
	var body map[string]any
	if docString(hook, "type") == "bidirectional-webhooks" {
		body = map[string]any{"description": description}
		for k, v := range p {
			body[k] = v
		}
	} else {
		body = map[string]any{"text": user + " " + sentence + "\n" + link}
		for _, k := range webhookAttributes {
			if v, ok := p[k]; ok {
				body[k] = v
			}
		}
		body["description"] = description
	}
	out, err := json.Marshal(body)
	if err != nil {
		return nil
	}
	return out
}

// deliverWebhook posts one body; a blocked address is recorded.
func (s *Server) deliverWebhook(ctx context.Context, hook bson.M, body []byte) error {
	target := docString(hook, "url")
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("invalid-webhook-url")
	}
	if !webhookAllowLoopbackForTests {
		if why := validateOutboundURL(ctx, target); why != "" {
			s.recordWebhookBlocked(ctx, hook, why)
			return errSSRFBlocked
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token := docString(hook, "token"); token != "" {
		req.Header.Set("X-Wekan-Token", token)
	}
	resp, err := webhookClient().Do(req)
	if err != nil {
		if errors.Is(err, errSSRFBlocked) {
			s.recordWebhookBlocked(ctx, hook, err.Error())
		}
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errors.New("webhook answered " + resp.Status)
	}
	return nil
}

func (s *Server) recordWebhookBlocked(ctx context.Context, hook bson.M, why string) {
	defer func() { _ = recover() }() // logging must never break the guard
	if len(why) > 160 {
		why = why[:160]
	}
	s.recordSecurityCtx(ctx, securityEvent{Key: "ssrf.webhook", Action: "blocked", Source: "outgoingWebhooks",
		Detail: "webhook " + docString(hook, "_id") + ": " + why})
}

// sendActivityWebhooks is WeKan's activityWebhookIntegrations and its
// fire-and-forget delivery: in the background, each webhook on its own.
func (s *Server) sendActivityWebhooks(activity bson.M) {
	boardID := docString(activity, "boardId")
	if boardID == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		description := "act-" + docString(activity, "activityType")
		cur, err := s.db.Collection("integrations").Find(ctx, bson.M{
			"boardId": bson.M{"$in": bson.A{boardID, "_global"}}, "enabled": true, "bot": bson.M{"$ne": true},
			"activities": bson.M{"$in": bson.A{description, "all"}}})
		if err != nil {
			return
		}
		var hooks []bson.M
		if cur.All(ctx, &hooks) != nil || len(hooks) == 0 {
			return
		}
		p := s.webhookParams(ctx, activity)
		for _, hook := range hooks {
			body := s.webhookBody(ctx, hook, description, p)
			if body == nil {
				continue
			}
			if err := s.deliverWebhook(ctx, hook, body); err != nil {
				s.log.Warn("outgoing webhook not delivered", "webhook", docString(hook, "_id"), "error", err)
			}
		}
	}()
}
