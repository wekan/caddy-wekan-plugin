package web

import (
	"net/http"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type boardWebhook struct {
	ID, URL, Title string
	Enabled        bool
}

func (s *Server) boardWebhooksPage(w http.ResponseWriter, r *http.Request, u *User) {
	ctx := r.Context()
	b, err := s.boardFor(ctx, r.PathValue("board"), u)
	if err != nil || !b.AdminOf(u.ID) || s.webhooksOff(ctx) {
		http.NotFound(w, r)
		return
	}
	fields := bson.M{"url": 1, "title": 1, "enabled": 1}
	query := bson.M{"boardId": b.ID, "bot": bson.M{"$ne": true}}
	cur, err := s.db.Collection("integrations").Find(ctx, query, options.Find().SetProjection(fields).SetSort(bson.D{{Key: "createdAt", Value: -1}, {Key: "_id", Value: 1}}))
	if err != nil {
		http.Error(w, http.StatusText(500), 500)
		return
	}
	var hooks []bson.M
	if err := cur.All(ctx, &hooks); err != nil {
		http.Error(w, http.StatusText(500), 500)
		return
	}
	rows := make([]boardWebhook, 0, len(hooks))
	for _, hook := range hooks {
		rows = append(rows, boardWebhook{ID: docString(hook, "_id"), URL: docString(hook, "url"), Title: docString(hook, "title"), Enabled: hook["enabled"] != false})
	}
	w.Header().Set("Cache-Control", "private, no-store")
	s.render(w, r, "boardwebhooks", http.StatusOK, map[string]any{"Title": tr(languageOf(r, u).Tag, "outgoing-webhooks"), "User": u, "Board": b, "Hooks": rows})
}
