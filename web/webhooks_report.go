package web

import (
	"context"
	"encoding/json"
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

type webhookReportRow struct {
	ID            string           `json:"_id"`
	IsGlobal      bool             `json:"isGlobal"`
	BoardID       string           `json:"boardId"`
	BoardTitle    string           `json:"boardTitle"`
	BoardSlug     string           `json:"boardSlug"`
	BoardMissing  bool             `json:"boardMissing"`
	OwnerID       string           `json:"ownerId"`
	OwnerUsername string           `json:"ownerUsername"`
	OwnerFullName string           `json:"ownerFullname"`
	Title         string           `json:"title"`
	URL           string           `json:"url"`
	TypeKey       string           `json:"typeKey"`
	Type          string           `json:"type"`
	Enabled       bool             `json:"enabled"`
	Activities    []string         `json:"activities"`
	AllActivities bool             `json:"allActivities"`
	TokenSet      bool             `json:"tokenSet"`
	Mattermost    bool             `json:"mattermostDirectMessages"`
	Bot           bool             `json:"bot"`
	OwnDelivery   bool             `json:"ownDelivery"`
	CreatedAt     map[string]int64 `json:"createdAt"`
	ModifiedAt    map[string]int64 `json:"modifiedAt"`
}

func (s *Server) webhookReportRows(ctx context.Context, filter, search string) ([]webhookReportRow, error) {
	query := bson.M{}
	switch filter {
	case "on":
		query["enabled"] = bson.M{"$ne": false}
	case "off":
		query["enabled"] = false
	}
	fields := bson.M{"boardId": 1, "userId": 1, "title": 1, "url": 1, "type": 1, "enabled": 1,
		"activities": 1, "token": 1, "bot": 1, "mattermostDirectMessages": 1, "createdAt": 1, "modifiedAt": 1, "notificationDelivery": 1}
	opts := options.Find().SetProjection(fields).SetSort(bson.D{{Key: "createdAt", Value: -1}, {Key: "_id", Value: 1}})
	cur, err := s.db.Collection("integrations").Find(ctx, query, opts)
	if err != nil {
		return nil, err
	}
	var hooks []bson.M
	if err := cur.All(ctx, &hooks); err != nil {
		return nil, err
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
	searchRunes := []rune(search)
	term := strings.ToLower(strings.TrimSpace(string(searchRunes[:min(200, len(searchRunes))])))
	rows := []webhookReportRow{}
	reportDate := func(v any) map[string]int64 {
		if date, ok := v.(bson.DateTime); ok {
			return map[string]int64{"$date": int64(date)}
		}
		return nil
	}
	for _, hook := range hooks {
		boardID, ownerID := docString(hook, "boardId"), docString(hook, "userId")
		board, owner := boards[boardID], owners[ownerID]
		profile, _ := owner["profile"].(bson.M)
		delivery, _ := hook["notificationDelivery"].(bson.M)
		token, tokenIsString := hook["token"].(string)
		row := webhookReportRow{
			ID: docString(hook, "_id"), IsGlobal: boardID == "_global", BoardID: boardID,
			BoardTitle: docString(board, "title"), BoardSlug: docString(board, "slug"),
			OwnerID: ownerID, OwnerUsername: docString(owner, "username"), OwnerFullName: docString(profile, "fullname"),
			Title: docString(hook, "title"), URL: reportWebhookURL(docString(hook, "url")),
			Type: orDefault(docString(hook, "type"), "outgoing-webhooks"), TypeKey: "outgoing-webhooks",
			Enabled: hook["enabled"] != false, Mattermost: hook["mattermostDirectMessages"] == true,
			Bot: hook["bot"] == true, OwnDelivery: len(delivery) > 0,
			TokenSet:  token != "" || (!tokenIsString && hook["token"] != nil),
			CreatedAt: reportDate(hook["createdAt"]), ModifiedAt: reportDate(hook["modifiedAt"]),
			Activities: []string{},
		}
		if row.IsGlobal {
			row.BoardID, row.BoardTitle, row.BoardSlug = "", "", ""
		} else {
			row.BoardMissing = board == nil
		}
		if row.Bot {
			row.TypeKey = "bots"
		} else if row.Type == "bidirectional-webhooks" {
			row.TypeKey = row.Type
		}
		if values, ok := hook["activities"].(bson.A); ok {
			for _, v := range values {
				if activity, ok := v.(string); ok {
					row.Activities = append(row.Activities, activity)
				}
			}
		}
		row.AllActivities = len(row.Activities) == 0 || slices.Contains(row.Activities, "all")
		for _, value := range []string{row.Title, row.URL, row.BoardTitle, row.OwnerUsername, row.OwnerFullName} {
			if strings.Contains(strings.ToLower(value), term) {
				rows = append(rows, row)
				break
			}
		}
	}
	return rows, nil
}

func (s *Server) methodWebhooksReportPage(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	var input map[string]any
	if len(args) > 1 || (len(args) == 1 && json.Unmarshal(args[0], &input) != nil) {
		return nil, &methodError{"Match.Error", "Match failed"}
	}
	if u == nil || !u.IsAdmin {
		return nil, &methodError{"not-authorized", "Not authorized"}
	}
	filter, _ := input["filter"].(string)
	search, _ := input["search"].(string)
	limit, _ := safeInt(input["limit"])
	skip, _ := safeInt(input["skip"])
	if limit <= 0 {
		limit = 25
	}
	ctx := r.Context()
	rows, err := s.webhookReportRows(ctx, filter, search)
	if err != nil {
		return nil, err
	}
	start := int(min(max(skip, 0), int64(len(rows))))
	end := start + min(int(min(limit, 100)), len(rows)-start)
	return map[string]any{"rows": rows[start:end], "total": len(rows), "webhooksOff": s.webhooksOff(ctx)}, nil
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
	if filter != "on" && filter != "off" {
		filter = "all"
	}
	page, _ := strconv.Atoi(q.Get("page"))
	page = max(1, page)
	rows, err := s.webhookReportRows(ctx, filter, search)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	const per = 25
	pages := max(1, (len(rows)+per-1)/per)
	page = min(page, pages)
	type displayRow struct {
		webhookReportRow
		Board, BoardURL, Owner, ActivityText, Created, Status string
	}
	shown := []displayRow{}
	for _, row := range rows[(page-1)*per : min(page*per, len(rows))] {
		display := displayRow{webhookReportRow: row, Board: row.BoardID, Owner: orDefault(row.OwnerUsername, row.OwnerID),
			ActivityText: strings.Join(row.Activities, ", "), Status: "webhooks-report-on"}
		if row.IsGlobal {
			display.Board = tr(lang, "webhooks-report-global")
		} else if !row.BoardMissing {
			display.Board = row.BoardTitle
			display.BoardURL = "/b/" + url.PathEscape(row.BoardID) + "/" + url.PathEscape(row.BoardSlug)
		}
		if row.AllActivities {
			display.ActivityText = tr(lang, "webhook-activities-all")
		}
		if !row.Enabled {
			display.Status = "webhooks-report-off"
		}
		if row.CreatedAt != nil {
			display.Created = bson.DateTime(row.CreatedAt["$date"]).Time().UTC().Format("2006-01-02 15:04")
		}
		shown = append(shown, display)
	}
	link := func(n int) string {
		return "/admin/problems/webhooks?" + (url.Values{"filter": {filter}, "q": {search}, "page": {strconv.Itoa(n)}}).Encode()
	}
	data := map[string]any{"User": u, "Title": tr(lang, "webhooks-report-title"), "Off": s.webhooksOff(ctx),
		"Rows": shown, "Total": len(rows), "Search": search, "Filter": filter}
	data["Filters"] = []struct{ Value, Key string }{{"all", "webhooks-report-title"}, {"on", "webhooks-report-on"}, {"off", "webhooks-report-off"}}
	if page > 1 {
		data["Prev"] = link(page - 1)
	}
	if page < pages {
		data["Next"] = link(page + 1)
	}
	s.render(w, r, "webhooksreport", http.StatusOK, data)
}
