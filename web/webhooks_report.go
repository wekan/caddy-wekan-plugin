package web

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func reportWebhookURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	if u.User != nil {
		u.User = url.User(u.User.Username())
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		u.RawQuery = ""
	} else {
		for key := range q {
			name := strings.ToLower(key)
			for _, secret := range []string{"token", "key", "secret", "sig", "password", "passwd", "pass", "auth", "code", "access"} {
				if strings.Contains(name, secret) {
					q.Set(key, "***")
					break
				}
			}
		}
		u.RawQuery = q.Encode()
	}
	return u.String()
}

func (s *Server) webhooksReport(w http.ResponseWriter, r *http.Request, u *User) {
	lang := languageOf(r, u).Tag
	if !u.IsAdmin {
		s.render(w, r, "notfound", http.StatusNotFound, map[string]any{"Title": tr(lang, "error-notAuthorized"), "User": u})
		return
	}
	ctx := r.Context()
	q := r.URL.Query()
	filter, search := q.Get("filter"), strings.TrimSpace(q.Get("q"))
	query := bson.M{}
	switch filter {
	case "on":
		query["enabled"] = bson.M{"$ne": false}
	case "off":
		query["enabled"] = false
	default:
		filter = "all"
	}
	page, _ := strconv.Atoi(q.Get("page"))
	page = max(1, page)
	fields := bson.M{"boardId": 1, "userId": 1, "title": 1, "url": 1, "type": 1, "enabled": 1,
		"activities": 1, "token": 1, "bot": 1, "mattermostDirectMessages": 1, "createdAt": 1}
	opts := options.Find().SetProjection(fields).SetSort(bson.D{{Key: "createdAt", Value: -1}, {Key: "_id", Value: 1}})
	cur, err := s.db.Collection("integrations").Find(ctx, query, opts)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	var hooks []bson.M
	if err := cur.All(ctx, &hooks); err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	lookup := func(collection, field string, projection bson.M) map[string]bson.M {
		ids := bson.A{}
		seen := map[string]bool{}
		for _, hook := range hooks {
			id := docString(hook, field)
			if id != "" && !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		out := map[string]bson.M{}
		if len(ids) == 0 {
			return out
		}
		cur, err := s.db.Collection(collection).Find(ctx, bson.M{"_id": bson.M{"$in": ids}}, options.Find().SetProjection(projection))
		var docs []bson.M
		if err == nil && cur.All(ctx, &docs) == nil {
			for _, doc := range docs {
				out[docString(doc, "_id")] = doc
			}
		}
		return out
	}
	boards := lookup("boards", "boardId", bson.M{"title": 1, "slug": 1})
	owners := lookup("users", "userId", bson.M{"username": 1, "profile.fullname": 1})
	type reportRow struct {
		ID, Board, BoardURL, Owner, FullName, Title, URL, Type, Status, Activities, Created string
		TokenSet, Mattermost                                                                bool
	}
	term := strings.ToLower(search)
	var rows []reportRow
	for _, hook := range hooks {
		boardID, ownerID := docString(hook, "boardId"), docString(hook, "userId")
		board, owner := boards[boardID], owners[ownerID]
		profile, _ := owner["profile"].(bson.M)
		row := reportRow{ID: docString(hook, "_id"), Board: boardID, Owner: orDefault(docString(owner, "username"), ownerID),
			FullName: docString(profile, "fullname"), Title: docString(hook, "title"), URL: reportWebhookURL(docString(hook, "url")),
			Type: "outgoing-webhooks", Status: "webhooks-report-on", Mattermost: hook["mattermostDirectMessages"] == true}
		if boardID == "_global" {
			row.Board = tr(lang, "webhooks-report-global")
		} else if board != nil {
			row.Board = docString(board, "title")
			row.BoardURL = "/b/" + url.PathEscape(boardID) + "/" + url.PathEscape(docString(board, "slug"))
		}
		if hook["enabled"] == false {
			row.Status = "webhooks-report-off"
		}
		if hook["bot"] == true {
			row.Type = "bots"
		} else if hook["type"] == "bidirectional-webhooks" {
			row.Type = "bidirectional-webhooks"
		}
		var activities []string
		if values, ok := hook["activities"].(bson.A); ok {
			for _, v := range values {
				if activity, ok := v.(string); ok {
					activities = append(activities, activity)
				}
			}
		}
		if len(activities) == 0 || slices.Contains(activities, "all") {
			row.Activities = tr(lang, "webhook-activities-all")
		} else {
			row.Activities = strings.Join(activities, ", ")
		}
		row.TokenSet = hook["token"] != nil && hook["token"] != ""
		if date, ok := hook["createdAt"].(bson.DateTime); ok {
			row.Created = date.Time().UTC().Format("2006-01-02 15:04")
		}
		for _, value := range []string{row.Title, row.URL, row.Board, row.Owner, row.FullName} {
			if strings.Contains(strings.ToLower(value), term) {
				rows = append(rows, row)
				break
			}
		}
	}
	const per = 25
	pages := max(1, (len(rows)+per-1)/per)
	page = min(page, pages)
	link := func(n int) string {
		return "/admin/problems/webhooks?" + (url.Values{"filter": {filter}, "q": {search}, "page": {strconv.Itoa(n)}}).Encode()
	}
	data := map[string]any{"User": u, "Title": tr(lang, "webhooks-report-title"), "Off": s.webhooksOff(ctx),
		"Rows": rows[(page-1)*per : min(page*per, len(rows))], "Total": len(rows), "Search": search, "Filter": filter}
	data["Filters"] = []struct{ Value, Key string }{{"all", "webhooks-report-title"}, {"on", "webhooks-report-on"}, {"off", "webhooks-report-off"}}
	if page > 1 {
		data["Prev"] = link(page - 1)
	}
	if page < pages {
		data["Next"] = link(page + 1)
	}
	s.render(w, r, "webhooksreport", http.StatusOK, data)
}
