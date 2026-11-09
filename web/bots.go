package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Bots, as Node.js WeKan's server/bots.js and models/lib/botRuns.js
// (docs/Features/Rules/Bots.md): a bot is an outgoing webhook marked `bot`,
// subscribed to "bot-dispatch" - an activity no board has - so it is sent a
// card a board admin hands it and nothing else. A bot has no rights in WeKan
// of its own: it is sent a dispatch through the SSRF-guarded webhook client
// (webhooks_out.go), and its runner works on the board through the REST API
// as its own WeKan user, reporting at the one route here, which needs write
// access to the board like every card write.

var (
	botStatuses = []string{"queued", "running", "awaiting_input", "review", "done", "failed", "stopped"}
	botActive   = []string{"queued", "running", "awaiting_input", "review"}
	botFinal    = []string{"done", "failed", "stopped"}
)

const (
	botActivity   = "bot-dispatch"
	botMaxEvents  = 50
	botMaxText    = 4000
	botMaxOptions = 8
)

// isBot is WeKan's: an integration marked bot and not turned off.
func isBot(hook bson.M) bool { return hook != nil && hook["bot"] == true && hook["enabled"] != false }

func botText(v any, max int) (string, bool) {
	if v == nil {
		return "", false
	}
	s := jsString(v)
	if r := []rune(s); len(r) > max {
		s = string(r[:max])
	}
	return s, true
}

// botRunUpdate is a cleaned report.
type botRunUpdate struct {
	set   bson.M
	event bson.M
}

// cleanBotRunUpdate is WeKan's cleanRunUpdate: the known fields, bounded.
func cleanBotRunUpdate(in map[string]any) (*botRunUpdate, error) {
	u := &botRunUpdate{set: bson.M{}}
	if st, ok := in["status"]; ok {
		s, _ := st.(string)
		if !slices.Contains(botStatuses, s) {
			return nil, fmt.Errorf("status must be one of %s", strings.Join(botStatuses, ", "))
		}
		u.set["status"] = s
	}
	ev := bson.M{}
	if t, ok := botText(in["text"], botMaxText); ok {
		ev["text"] = t
	}
	if t, ok := botText(in["tool"], 200); ok {
		ev["tool"] = t
	}
	if len(ev) > 0 {
		u.event = ev
	}
	if _, has := in["question"]; has {
		q, _ := botText(in["question"], 1000)
		u.set["question"] = q
	}
	if raw, has := in["options"]; has {
		list, ok := raw.([]any)
		if !ok {
			return nil, errors.New("options must be a list")
		}
		opts := bson.A{}
		for _, o := range list {
			if len(opts) >= botMaxOptions {
				break
			}
			var value, label string
			if m, ok := o.(map[string]any); ok {
				value, _ = botText(m["value"], 200)
				label = value
				if l, ok := botText(m["label"], 200); ok {
					label = l
				}
			} else {
				value, _ = botText(o, 200)
				label = value
			}
			if value != "" {
				opts = append(opts, bson.M{"value": value, "label": label})
			}
		}
		u.set["options"] = opts
	}
	for _, k := range []string{"tokens", "costUsd"} {
		v, has := in[k]
		if !has {
			continue
		}
		n, ok := v.(float64)
		if !ok || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > 1e12 {
			return nil, fmt.Errorf("%s must be a number", k)
		}
		u.set[k] = n
	}
	if st, ok := u.set["status"].(string); ok && st != "awaiting_input" {
		u.set["question"], u.set["options"] = nil, bson.A{}
	}
	return u, nil
}

// botCanMove is WeKan's canMove: a finished run stays finished.
func botCanMove(from, to string) bool {
	return to == "" || to == from || !slices.Contains(botFinal, from)
}

// botDispatchBody is WeKan's dispatchBody.
func botDispatchBody(runID string, bot bson.M, b *Board, c *Card, listTitle string, comments []bson.M) map[string]any {
	base := strings.TrimRight(os.Getenv("ROOT_URL"), "/")
	labels := []any{}
	for _, l := range b.Labels {
		if slices.Contains(c.LabelIDs, l.ID) {
			name := l.Name
			if name == "" {
				name = l.Color
			}
			labels = append(labels, name)
		}
	}
	var list any
	if c.ListID != "" {
		list = map[string]any{"id": c.ListID, "title": listTitle}
	}
	var number any
	if c.CardNumber != 0 {
		number = c.CardNumber
	}
	slug := b.Slug
	if slug == "" {
		slug = "board"
	}
	cs := []any{}
	if len(comments) > 20 {
		comments = comments[len(comments)-20:]
	}
	for _, cm := range comments {
		cs = append(cs, map[string]any{"id": docString(cm, "_id"), "text": docString(cm, "text"), "at": plain(cm["createdAt"])})
	}
	return map[string]any{
		"event": "bot.dispatch", "runId": runID,
		"bot":   map[string]any{"id": docString(bot, "_id"), "title": docString(bot, "title")},
		"board": map[string]any{"id": b.ID, "title": b.Title, "key": ""},
		"card": map[string]any{"id": c.ID, "number": number, "title": c.Title, "description": c.Description, "list": list,
			"labels": labels, "url": base + "/b/" + b.ID + "/" + slug + "/" + c.ID},
		"comments": cs,
		"report":   base + "/api/boards/" + b.ID + "/cards/" + c.ID + "/bot-runs/" + runID,
		"api":      base + "/api",
	}
}

// sendToBot posts a body to a bot in the background, through the guard.
func (s *Server) sendToBot(bot bson.M, body map[string]any) {
	data, err := json.Marshal(body)
	if err != nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := s.deliverWebhook(ctx, bot, data); err != nil {
			s.log.Warn("bot not reached", "bot", docString(bot, "_id"), "error", err)
		}
	}()
}

// dispatchToBot is WeKan's: a run, and a dispatch to the bot of the card's own
// board; a card already with that bot keeps its run.
func (s *Server) dispatchToBot(ctx context.Context, integrationID, cardID, userID, source string) (string, error) {
	var c Card
	if !validID(cardID) || s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(cardID)}).Decode(&c) != nil {
		return "", &methodError{"not-found", "not-found"}
	}
	var bot bson.M
	if !validID(integrationID) || s.db.Collection("integrations").FindOne(ctx, bson.M{"_id": eq(integrationID), "boardId": eq(c.BoardID)}).Decode(&bot) != nil || !isBot(bot) {
		return "", &methodError{"bot-not-found", "bot-not-found"}
	}
	var active struct {
		ID string `bson:"_id"`
	}
	if s.db.Collection("botRuns").FindOne(ctx, bson.M{"cardId": eq(cardID), "integrationId": eq(integrationID), "status": bson.M{"$in": botActive}}).Decode(&active) == nil {
		return active.ID, nil
	}
	var b Board
	if s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(c.BoardID)}).Decode(&b) != nil {
		return "", &methodError{"not-found", "not-found"}
	}
	var comments []bson.M
	if cur, err := s.db.Collection("card_comments").Find(ctx, bson.M{"cardId": eq(cardID)}, options.Find().SetSort(bson.D{{Key: "createdAt", Value: 1}}).SetLimit(20)); err == nil {
		_ = cur.All(ctx, &comments)
	}
	if source == "" {
		source = "button"
	}
	var createdBy any
	if userID != "" {
		createdBy = userID
	}
	now := time.Now().UTC()
	run := bson.M{"_id": randomID(), "boardId": c.BoardID, "cardId": cardID, "integrationId": integrationID, "status": "queued", // inserted, not a filter
		"source": source, "createdBy": createdBy, "createdAt": now, "updatedAt": now,
		"events": bson.A{bson.M{"text": "dispatched", "status": "queued", "at": now}}, "options": bson.A{}, "question": nil}
	if _, err := s.db.Collection("botRuns").InsertOne(ctx, run); err != nil {
		return "", err
	}
	runID := run["_id"].(string)
	s.sendToBot(bot, botDispatchBody(runID, bot, &b, &c, s.titleOfID(ctx, "lists", c.ListID), comments))
	s.hub.publish(c.BoardID, "board-changed")
	return runID, nil
}

// botRunForAdmin is WeKan's runForAdmin.
func (s *Server) botRunForAdmin(ctx context.Context, u *User, runID string) (bson.M, *Board, bson.M, error) {
	var run bson.M
	if !validID(runID) || s.db.Collection("botRuns").FindOne(ctx, bson.M{"_id": eq(runID)}).Decode(&run) != nil {
		return nil, nil, nil, &methodError{"not-found", "not-found"}
	}
	var b Board
	if s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(docString(run, "boardId"))}).Decode(&b) != nil || !b.AdminOf(u.ID) {
		return nil, nil, nil, &methodError{"not-authorized", "not-authorized"}
	}
	var bot bson.M
	_ = s.db.Collection("integrations").FindOne(ctx, bson.M{"_id": eq(docString(run, "integrationId")), "boardId": eq(b.ID)}).Decode(&bot)
	return run, &b, bot, nil
}

// botCommentAndTell is WeKan's commentAndTell: the admin's own comment, under
// their right to comment, and the bot is told.
func (s *Server) botCommentAndTell(ctx context.Context, u *User, runID, kind, text string, payload map[string]any) (string, error) {
	run, b, bot, err := s.botRunForAdmin(ctx, u, runID)
	if err != nil {
		return "", err
	}
	var c Card
	if s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(docString(run, "cardId"))}).Decode(&c) != nil || !b.CommentableIn(ctx, s, u, &c) {
		return "", &methodError{"not-authorized", "not-authorized"}
	}
	commentID, err := s.AddComment(ctx, b, u, &c, text)
	if err != nil {
		return "", err
	}
	if isBot(bot) {
		body := map[string]any{"event": "bot." + kind, "runId": runID, "cardId": c.ID, "boardId": b.ID, "text": text, "commentId": commentID}
		for k, v := range payload {
			body[k] = v
		}
		s.sendToBot(bot, body)
	}
	if docString(run, "status") == "awaiting_input" {
		_, _ = s.db.Collection("botRuns").UpdateOne(ctx, bson.M{"_id": eq(runID)}, bson.M{"$set": bson.M{"status": "running", "question": nil, "options": bson.A{}, "updatedAt": time.Now().UTC()}})
	}
	return commentID, nil
}

func (s *Server) methodBotDispatch(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	cardID, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	integrationID, err := stringArg(args, 1)
	if err != nil {
		return nil, err
	}
	ctx := r.Context()
	var c Card
	var b Board
	if !validID(cardID) || s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(cardID)}).Decode(&c) != nil ||
		s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(c.BoardID)}).Decode(&b) != nil || !b.AdminOf(u.ID) {
		return nil, &methodError{"not-authorized", "not-authorized"}
	}
	return s.dispatchToBot(ctx, integrationID, cardID, u.ID, "button")
}

func (s *Server) methodBotAnswer(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	runID, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	value, err := stringArg(args, 1)
	if err != nil {
		return nil, err
	}
	var run struct {
		Options []struct {
			Value string `bson:"value"`
			Label string `bson:"label"`
		} `bson:"options"`
	}
	if !validID(runID) || s.db.Collection("botRuns").FindOne(r.Context(), bson.M{"_id": eq(runID)}).Decode(&run) != nil {
		return nil, &methodError{"bot-option-not-found", "bot-option-not-found"}
	}
	for _, o := range run.Options {
		if o.Value == value {
			return s.botCommentAndTell(r.Context(), u, runID, "answer", "↳ "+o.Label, map[string]any{"value": value})
		}
	}
	return nil, &methodError{"bot-option-not-found", "bot-option-not-found"}
}

func (s *Server) methodBotReply(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	runID, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	text, err := stringArg(args, 1)
	if err != nil {
		return nil, err
	}
	body := strings.TrimSpace(text)
	if len(body) > 65536 {
		body = body[:65536]
	}
	if body == "" {
		return nil, &methodError{"bot-reply-empty", "bot-reply-empty"}
	}
	return s.botCommentAndTell(r.Context(), u, runID, "reply", body, nil)
}

func (s *Server) methodBotStop(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	runID, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	ctx := r.Context()
	run, _, bot, err := s.botRunForAdmin(ctx, u, runID)
	if err != nil {
		return nil, err
	}
	if st := docString(run, "status"); slices.Contains(botFinal, st) {
		return st, nil
	}
	now := time.Now().UTC()
	_, _ = s.db.Collection("botRuns").UpdateOne(ctx, bson.M{"_id": eq(runID)}, bson.M{
		"$set":  bson.M{"status": "stopped", "finishedAt": now, "updatedAt": now},
		"$push": bson.M{"events": bson.M{"$each": bson.A{bson.M{"text": "stopped", "status": "stopped", "at": now}}, "$slice": -botMaxEvents}}})
	if isBot(bot) {
		s.sendToBot(bot, map[string]any{"event": "bot.stop", "runId": runID, "cardId": docString(run, "cardId"), "boardId": docString(run, "boardId")})
	}
	return "stopped", nil
}

// botRoutes: the runner's report.
func (s *Server) botRoutes(m *http.ServeMux) {
	m.HandleFunc("POST /api/boards/{board}/cards/{card}/bot-runs/{run}", s.api(s.apiBotRunReport))
}

// apiBotRunReport is WeKan's botRunReport: with write access to the board.
func (s *Server) apiBotRunReport(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, true)
	if b == nil {
		return
	}
	ctx := r.Context()
	cardID, runID := r.PathValue("card"), r.PathValue("run")
	var run bson.M
	if !validID(runID) || !validID(cardID) || s.db.Collection("botRuns").FindOne(ctx, bson.M{"_id": eq(runID), "boardId": eq(b.ID), "cardId": eq(cardID)}).Decode(&run) != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Run not found"})
		return
	}
	var in map[string]any
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil && err.Error() != "EOF" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if in == nil {
		in = map[string]any{}
	}
	upd, err := cleanBotRunUpdate(in)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	from := docString(run, "status")
	to, _ := upd.set["status"].(string)
	if !botCanMove(from, to) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the run is " + from})
		return
	}
	now := time.Now().UTC()
	set := bson.M{"updatedAt": now, "reportedBy": u.ID}
	for k, v := range upd.set {
		set[k] = v
	}
	if slices.Contains(botFinal, to) {
		set["finishedAt"] = now
	}
	update := bson.M{"$set": set}
	if upd.event != nil {
		status := to
		if status == "" {
			status = from
		}
		ev := bson.M{"at": now, "status": status}
		for k, v := range upd.event {
			ev[k] = v
		}
		update["$push"] = bson.M{"events": bson.M{"$each": bson.A{ev}, "$slice": -botMaxEvents}}
	}
	// Only while it is still what was checked: a finished run stays finished.
	if _, err := s.db.Collection("botRuns").UpdateOne(ctx, bson.M{"_id": eq(runID), "status": eq(from)}, update); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	status := from
	if to != "" {
		status = to
	}
	s.hub.publish(b.ID, "board-changed")
	writeJSON(w, http.StatusOK, map[string]string{"_id": runID, "status": status})
}

// methodInsertIntegration is the client's Integrations.insert under WeKan's
// allow rule (server/permissions/integrations.js): a board admin, or a site
// admin, adds a webhook - here a bot too - to that board; its address must
// pass the SSRF guard's input check, stricter than WeKan's collection write.
func (s *Server) methodInsertIntegration(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	var in map[string]any
	if len(args) < 1 || json.Unmarshal(args[0], &in) != nil || in == nil {
		return nil, &methodError{"Match.Error", "Expected an object"}
	}
	ctx := r.Context()
	str := func(k string) string { v, _ := in[k].(string); return v }
	var b Board
	if !validID(str("boardId")) || s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(str("boardId"))}).Decode(&b) != nil ||
		!(b.AdminOf(u.ID) || u.IsAdmin) {
		return nil, &methodError{"403", "Access denied"}
	}
	if why := validateOutboundURL(ctx, str("url")); why != "" {
		return nil, &methodError{"invalid-webhook-url", why}
	}
	doc := bson.M{"_id": randomID(), "boardId": b.ID, "url": str("url"), "title": str("title"), "type": "outgoing-webhooks", // inserted, not a filter
		"enabled": in["enabled"] != false, "userId": u.ID, "createdAt": time.Now().UTC(), "modifiedAt": time.Now().UTC()}
	if t := str("type"); t == "bidirectional-webhooks" {
		doc["type"] = t
	}
	if tok := str("token"); tok != "" {
		doc["token"] = tok
	}
	if in["bot"] == true {
		// A bot is subscribed to the dispatch only, never the board's activity.
		doc["bot"], doc["activities"] = true, bson.A{botActivity}
	} else {
		acts := bson.A{}
		list, _ := in["activities"].([]any)
		for _, a := range list {
			if s, ok := a.(string); ok && len(s) <= 100 {
				acts = append(acts, s)
			}
		}
		if len(acts) == 0 {
			acts = bson.A{"all"}
		}
		doc["activities"] = acts
	}
	if _, err := s.db.Collection("integrations").InsertOne(ctx, doc); err != nil {
		return nil, err
	}
	s.hub.publish(b.ID, "board-changed")
	return doc["_id"], nil
}

// methodRemoveIntegration is Integrations.remove under the same rule.
func (s *Server) methodRemoveIntegration(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	var sel struct {
		ID string `json:"_id"`
	}
	if len(args) < 1 || (json.Unmarshal(args[0], &sel) != nil && json.Unmarshal(args[0], &sel.ID) != nil) {
		return nil, &methodError{"Match.Error", "Expected an id"}
	}
	ctx := r.Context()
	var hook struct {
		BoardID string `bson:"boardId"`
	}
	var b Board
	if !validID(sel.ID) || s.db.Collection("integrations").FindOne(ctx, bson.M{"_id": eq(sel.ID)}).Decode(&hook) != nil ||
		s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(hook.BoardID)}).Decode(&b) != nil || !(b.AdminOf(u.ID) || u.IsAdmin) {
		return nil, &methodError{"403", "Access denied"}
	}
	res, err := s.db.Collection("integrations").DeleteOne(ctx, bson.M{"_id": eq(sel.ID), "boardId": eq(b.ID)})
	if err != nil {
		return nil, err
	}
	s.hub.publish(b.ID, "board-changed")
	return res.DeletedCount, nil
}
