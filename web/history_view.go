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
	lang := languageOf(r, u).Tag
	b, err := s.boardFor(r.Context(), r.PathValue("board"), u)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	req := historyPageRequest{Scope: "board", ScopeID: b.ID, Search: q.Get("q"), UserID: q.Get("user")}
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
	page, _ := strconv.ParseFloat(q.Get("page"), 64)
	req.Page = page
	res, err := s.historyPage(r, u, req)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	link := func(mod func(url.Values)) string {
		v := url.Values{}
		for _, k := range []string{"card", "q", "user"} {
			if q.Get(k) != "" {
				v.Set(k, q.Get(k))
			}
		}
		mod(v)
		if len(v) == 0 {
			return "/b/" + b.ID + "/history"
		}
		return "/b/" + b.ID + "/history?" + v.Encode()
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
		id := c["userId"].(string)
		name := id
		var cu User
		if s.db.Collection("users").FindOne(r.Context(), bson.M{"_id": eq(id)}).Decode(&cu) == nil {
			name = cu.Name()
		}
		cs = append(cs, contributor{UserID: id, Name: name, Count: c["count"].(int64), Active: id == req.UserID,
			Link: link(func(v url.Values) { v.Set("user", id); v.Del("page") })})
	}
	data := map[string]any{"Title": tr(lang, "history"), "User": u, "Board": b, "Rows": rows, "Search": req.Search, "CardID": cardID,
		"ActiveUser": req.UserID, "Contributors": cs, "AllLink": link(func(v url.Values) { v.Del("user"); v.Del("page") }),
		"Page": pageN, "Pages": pages, "Action": "/b/" + b.ID + "/history", "RestoreAction": "/b/" + b.ID + "/history/restore",
		"Self": link(func(url.Values) {}), "Back": back, "BackTitle": backTitle}
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

func (s *Server) historyViewRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /b/{board}/history", s.page(s.historyViewPage))
	m.HandleFunc("POST /b/{board}/history/restore", s.page(s.historyRestorePage))
}

// jsonMarshalArgs is a method's arguments as JSON: here the one id list.
func jsonMarshalArgs(ids []string) ([]json.RawMessage, error) {
	b, err := json.Marshal(ids)
	return []json.RawMessage{b}, err
}
