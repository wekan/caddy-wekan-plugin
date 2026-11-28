package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"unicode"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

type accountWebhook struct {
	URL        string   `bson:"url"`
	Token      string   `bson:"token"`
	OnLogin    bool     `bson:"onLogin"`
	OnRegister bool     `bson:"onRegister"`
	Fields     []string `bson:"fields"`
}

var accountWebhookFields = []struct{ Name, Label string }{
	{"email", msg("account-webhook-field-email")}, {"ip", msg("account-webhook-field-ip")},
	{"fullname", msg("account-webhook-field-fullname")}, {"authMethod", msg("account-webhook-field-authMethod")},
}

func accountWebhookForm(config accountWebhook) map[string]any {
	var fields []map[string]any
	for _, field := range accountWebhookFields {
		fields = append(fields, map[string]any{"Name": field.Name, "Label": field.Label,
			"Enabled": slices.Contains(config.Fields, field.Name)})
	}
	return map[string]any{"URL": config.URL, "HasToken": config.Token != "",
		"OnLogin": config.OnLogin, "OnRegister": config.OnRegister, "Fields": fields}
}

func (s *Server) readAccountWebhook(ctx context.Context) (accountWebhook, error) {
	var settings struct {
		Webhook accountWebhook `bson:"accountWebhook"`
	}
	err := s.db.Collection("settings").FindOne(ctx, bson.M{}).Decode(&settings)
	if errors.Is(err, mongo.ErrNoDocuments) {
		err = nil
	}
	return settings.Webhook, err
}

func (s *Server) saveAccountWebhook(w http.ResponseWriter, r *http.Request, u *User) {
	ctx := r.Context()
	if !u.IsAdmin || s.webhooksOff(ctx) {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid account webhook settings", http.StatusBadRequest)
		return
	}
	target := strings.TrimSpace(r.PostForm.Get("url"))
	if target != "" {
		parsed, err := url.Parse(target)
		if utf16Len(target) > 2000 || err != nil || parsed.Hostname() == "" || parsed.User != nil ||
			(parsed.Scheme != "http" && parsed.Scheme != "https") || strings.ContainsFunc(target, unicode.IsControl) {
			http.Error(w, "Invalid account webhook URL", http.StatusBadRequest)
			return
		}
	}
	token := strings.TrimSpace(r.PostForm.Get("token"))
	if utf16Len(token) > 500 || strings.ContainsFunc(token, unicode.IsControl) {
		http.Error(w, "Invalid account webhook token", http.StatusBadRequest)
		return
	}
	var fields = make([]string, 0, len(accountWebhookFields))
	for _, field := range accountWebhookFields {
		if slices.Contains(r.PostForm["fields"], field.Name) {
			fields = append(fields, field.Name)
		}
	}
	set := bson.M{"accountWebhook.url": target, "accountWebhook.fields": fields,
		"accountWebhook.onLogin":    r.PostForm.Get("onLogin") == "1",
		"accountWebhook.onRegister": r.PostForm.Get("onRegister") == "1"}
	if token != "" || r.PostForm.Get("clearToken") == "1" {
		set["accountWebhook.token"] = token
	}
	result, err := s.db.Collection("settings").UpdateOne(ctx, bson.M{"webhooksDisabled": bson.M{"$ne": true}}, bson.M{"$set": set})
	if err != nil || result.MatchedCount != 1 {
		http.Error(w, "Could not save account webhook settings", http.StatusConflict)
		return
	}
	http.Redirect(w, r, "/admin/settings/global-webhooks", http.StatusSeeOther)
}
