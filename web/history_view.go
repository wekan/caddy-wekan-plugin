package web

// The History view on Weddy's pages: WeKan's historyTable, as a page of its
// own - a board's, from the board menu, or a card's, from the card menu -
// over changeHistory.page and changeHistory.restore. And Ctrl+Z and
// Ctrl+Shift+Z on the board undo and redo one's own last change (weddy.js).

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// historyGroupKeys is WeKan's GROUP_KEYS: a group by the word the card
// details already use for it; one without a word shows none.
var historyGroupKeys = map[string]string{"rules": "r-board-rules", "scrum": "scrum-settings", "title": "title", "description": "description",
	"labels": "labels", "members": "members", "assignees": "assignees", "dates": "date", "checklists": "checklists", "subtasks": "subtasks",
	"attachments": "attachments", "comments": "comments", "customFields": "custom-fields", "dependencies": "card-dependencies", "position": "sort"}

// historyTypeKey is changeTypeKey.
func historyTypeKey(t string) string {
	return map[string]string{"added": msg("added"), "edited": msg("history-change-edited"), "moved": msg("history-change-moved"),
		"removed": msg("history-change-removed"), "restored": msg("history-change-restored")}[t]
}

type historyViewRow struct {
	ID, Type, Group, Summary, When string
}

// historySummary is summarise: the row's content as one line of text.
func historySummary(lang string, row map[string]any) string {
	content, _ := row["newContent"].(map[string]any)
	if content == nil {
		content, _ = row["previousContent"].(map[string]any)
	}
	if content == nil {
		return ""
	}
	// A Scrum row: each record's name (or id), state and Scrum metadata.
	if docString(row, "entityType") == "scrum" {
		var parts []string
		recs, _ := content["records"].([]any)
		for _, x := range recs {
			e, _ := x.(map[string]any)
			doc, _ := e["document"].(map[string]any)
			if doc == nil {
				parts = append(parts, tr(lang, "history-change-removed"))
				continue
			}
			var bits []string
			for _, b := range []string{docString(doc, "name"), docString(doc, "state")} {
				if b != "" {
					bits = append(bits, b)
				}
			}
			if len(bits) == 0 || docString(doc, "name") == "" {
				bits = append([]string{docString(doc, "_id")}, bits...)
			}
			if sc, ok := doc["scrum"]; ok && sc != nil {
				if raw, err := json.Marshal(plain(sc)); err == nil {
					bits = append(bits, string(raw))
				}
			}
			parts = append(parts, strings.Join(bits, " — "))
		}
		return strings.Join(parts, "; ")
	}
	// A rule row: its title, trigger, action and whether it is on.
	if docString(row, "entityType") == "rule" {
		state := content
		if content["rule"] == nil {
			state, _ = row["previousContent"].(map[string]any)
		}
		rule, _ := state["rule"].(map[string]any)
		trig, _ := state["trigger"].(map[string]any)
		act, _ := state["action"].(map[string]any)
		key := msg("history-change-removed")
		if content["rule"] != nil {
			key = msg("r-rule-enabled")
			if r, _ := content["rule"].(map[string]any); r["enabled"] == false {
				key = msg("r-rule-disabled")
			}
		}
		var bits []string
		for _, b := range []string{docString(rule, "title"), orDefault(docString(trig, "desc"), docString(trig, "activityType")),
			orDefault(docString(act, "desc"), docString(act, "actionType")), tr(lang, key)} {
			if b != "" {
				bits = append(bits, b)
			}
		}
		return strings.Join(bits, " — ")
	}
	v, has := content["value"]
	switch {
	case has && v == nil:
		return "—"
	case content["isDate"] == true:
		if f, ok := v.(float64); ok {
			return time.UnixMilli(int64(f)).UTC().Format("2006-01-02 15:04")
		}
	}
	switch x := v.(type) {
	case string:
		return x
	case []any:
		parts := []string{}
		for _, e := range x {
			if s, ok := e.(string); ok {
				parts = append(parts, s)
			} else {
				parts = append(parts, canonical(e))
			}
		}
		return strings.Join(parts, ", ")
	case float64, bool:
		return jsonNumberOrBool(x)
	}
	if doc, ok := content["document"].(map[string]any); ok {
		for _, k := range []string{"title", "text", "name"} {
			if s, ok := doc[k].(string); ok && s != "" {
				return s
			}
		}
	}
	if d, ok := content["deleted"].(bool); ok {
		if d {
			return tr(lang, "history-change-removed")
		}
		return tr(lang, "history-change-restored")
	}
	if docString(row, "group") == "position" {
		return docString(content, "listId")
	}
	return ""
}

// historyViewPage is GET /b/{board}/history[?card=]: the table.
func (s *Server) historyViewPage(w http.ResponseWriter, r *http.Request, u *User) {
	b, err := s.boardFor(r.Context(), r.PathValue("board"), u)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	req := historyPageRequest{Scope: "board", ScopeID: b.ID, Search: q.Get("q"), UserID: q.Get("user")}
	// A group of changes alone - the Scrum views open History at "scrum".
	if g := q.Get("group"); historyGroupKeys[g] != "" {
		req.Group = g
	}
	back, backTitle := "/b/"+b.ID+"/"+b.Slug, b.Title
	cardID := q.Get("card")
	if cardID != "" {
		var c Card
		if !validID(cardID) || s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": eq(cardID), "boardId": eq(b.ID)}).Decode(&c) != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		req.Scope, req.ScopeID = "card", c.ID
		back, backTitle = back+"/"+c.ID, c.Title
	}
	s.renderHistory(w, r, u, req, "/b/"+b.ID+"/history", "/b/"+b.ID+"/history/restore", back, backTitle, cardID, b)
}

// renderHistory draws the table for a request: base is its own address,
// restore where its form posts.
func (s *Server) renderHistory(w http.ResponseWriter, r *http.Request, u *User, req historyPageRequest, base, restore, back, backTitle, cardID string, b *Board) {
	lang := languageOf(r, u).Tag
	q := r.URL.Query()
	page, _ := strconv.ParseFloat(q.Get("page"), 64)
	req.Page = page
	res, err := s.historyPage(r, u, req)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	link := func(mod func(url.Values)) string {
		v := url.Values{}
		for _, k := range []string{"card", "q", "user", "group"} {
			if q.Get(k) != "" {
				v.Set(k, q.Get(k))
			}
		}
		mod(v)
		if len(v) == 0 {
			return base
		}
		return base + "?" + v.Encode()
	}
	var rows []historyViewRow
	for _, row := range res["rows"].([]map[string]any) {
		id, changeType := docString(row, "_id"), docString(row, "changeType")
		when := ""
		if t, ok := row["createdAt"].(time.Time); ok {
			when = t.UTC().Format("2006-01-02 15:04")
		}
		group := ""
		if k := historyGroupKeys[docString(row, "group")]; k != "" {
			group = tr(lang, k)
		}
		rows = append(rows, historyViewRow{ID: id, Type: tr(lang, historyTypeKey(changeType)),
			Group: group, Summary: historySummary(lang, row), When: when})
	}
	pageN, size, total := res["page"].(int64), res["pageSize"].(int64), res["total"].(int64)
	pages := max(1, (total+size-1)/size)
	type contributor struct {
		UserID, Name, Link string
		Count              int64
		Active             bool
	}
	var cs []contributor
	for _, c := range res["contributors"].([]map[string]any) {
		if b == nil {
			break // the Member view is one person already
		}
		id := c["userId"].(string)
		name := id
		var cu User
		if s.db.Collection("users").FindOne(r.Context(), bson.M{"_id": eq(id)}).Decode(&cu) == nil {
			name = cu.Name()
		}
		cs = append(cs, contributor{UserID: id, Name: name, Count: c["count"].(int64), Active: id == req.UserID,
			Link: link(func(v url.Values) { v.Set("user", id); v.Del("page") })})
	}
	data := map[string]any{"Title": tr(lang, "history"), "User": u, "Rows": rows, "Search": req.Search, "CardID": cardID,
		"ActiveUser": req.UserID, "Contributors": cs, "AllLink": link(func(v url.Values) { v.Del("user"); v.Del("page") }),
		"Page": pageN, "Pages": pages, "Action": base, "RestoreAction": restore,
		"Self": link(func(url.Values) {}), "Back": back, "BackTitle": backTitle}
	if b != nil {
		data["Board"] = b
	}
	if pageN > 1 {
		data["Prev"] = link(func(v url.Values) { v.Set("page", strconv.FormatInt(pageN-1, 10)) })
	}
	if pageN < pages {
		data["Next"] = link(func(v url.Values) { v.Set("page", strconv.FormatInt(pageN+1, 10)) })
	}
	s.render(w, r, "history", http.StatusOK, data)
}

// historyRestorePage is the ticked rows' Restore.
func (s *Server) historyRestorePage(w http.ResponseWriter, r *http.Request, u *User) {
	b, err := s.boardFor(r.Context(), r.PathValue("board"), u)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	_ = r.ParseForm()
	ids := r.Form["id"]
	raw, _ := jsonMarshalArgs(ids)
	if _, err := s.methodHistoryRestore(r, u, raw); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	back := r.FormValue("back")
	if !strings.HasPrefix(back, "/b/"+b.ID+"/history") {
		back = "/b/" + b.ID + "/history"
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// memberHistoryPage is Member Settings' History: one's own changes, on the
// boards one sees now.
func (s *Server) memberHistoryPage(w http.ResponseWriter, r *http.Request, u *User) {
	req := historyPageRequest{UserID: u.ID, Search: r.URL.Query().Get("q")}
	s.renderHistory(w, r, u, req, "/profile/history", "/profile/history/restore", "/profile", tr(languageOf(r, u).Tag, "edit-profile"), "", nil)
}

func (s *Server) memberHistoryRestorePage(w http.ResponseWriter, r *http.Request, u *User) {
	_ = r.ParseForm()
	raw, _ := jsonMarshalArgs(r.Form["id"])
	if _, err := s.methodHistoryRestore(r, u, raw); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	http.Redirect(w, r, "/profile/history", http.StatusSeeOther)
}

func (s *Server) historyViewRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /b/{board}/history", s.page(s.historyViewPage))
	m.HandleFunc("POST /b/{board}/history/restore", s.page(s.historyRestorePage))
	m.HandleFunc("GET /profile/history", s.page(s.memberHistoryPage))
	m.HandleFunc("POST /profile/history/restore", s.page(s.memberHistoryRestorePage))
}

// jsonMarshalArgs is a method's arguments as JSON: here the one id list.
func jsonMarshalArgs(ids []string) ([]json.RawMessage, error) {
	b, err := json.Marshal(ids)
	return []json.RawMessage{b}, err
}
