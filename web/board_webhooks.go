package web

import (
	"net/http"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type boardWebhook struct {
	ID, URL, Title                 string
	Enabled, TwoWay, AllActivities bool
	Groups                         []webhookGroupView
}

type webhookGroupView struct {
	Label, Activities string
	Checked           bool
}

// Same groups as WeKan's models/lib/notificationActivityGroups.js.
var webhookActivityGroups = []struct{ key, names string }{
	{"labels", "addedLabel removedLabel"},
	{"members", "joinMember unjoinMember addBoardMember removeBoardMember"},
	{"assignees", "joinAssignee unjoinAssignee"},
	{"comments", "addComment editComment deleteComment"},
	{"moves", "moveCard moveCardToOtherBoard moveCardBoard"},
	{"dates", "a-dueAt a-endAt a-startAt a-receivedAt"},
	{"checklists", "addChecklist addChecklistItem removeChecklist removeChecklistItem removedChecklistItem checkedItem uncheckedItem completeChecklist uncompleteChecklist"},
	{"attachments", "addAttachment deleteAttachment"},
	{"customFields", "setCustomField"},
	{"archive", "archivedCard restoredCard archivedList archivedSwimlane deleteCard restoredList restoredSwimlane"},
	{"created", "createCard createList createSwimlane addSubtask importCard importList"},
}

func (s *Server) boardWebhooksPage(w http.ResponseWriter, r *http.Request, u *User) {
	ctx := r.Context()
	b, err := s.boardFor(ctx, r.PathValue("board"), u)
	if err != nil || !b.AdminOf(u.ID) || s.webhooksOff(ctx) {
		http.NotFound(w, r)
		return
	}
	s.renderWebhookManagement(w, r, u, b.ID, b, "/api/boards/"+b.ID+"/integrations", "outgoing-webhooks")
}

func (s *Server) globalWebhooksPage(w http.ResponseWriter, r *http.Request, u *User) {
	if !u.IsAdmin || s.webhooksOff(r.Context()) {
		http.NotFound(w, r)
		return
	}
	s.renderWebhookManagement(w, r, u, globalWebhookBoard, nil, "/api/admin/webhooks", "global-webhook")
}

func (s *Server) renderWebhookManagement(w http.ResponseWriter, r *http.Request, u *User, boardID string, b *Board, endpoint, titleKey string) {
	ctx := r.Context()
	fields := bson.M{"url": 1, "title": 1, "enabled": 1, "type": 1, "activities": 1}
	query := bson.M{"boardId": boardID, "bot": bson.M{"$ne": true}}
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
		row := boardWebhook{ID: docString(hook, "_id"), URL: docString(hook, "url"), Title: docString(hook, "title"),
			Enabled: hook["enabled"] != false, TwoWay: hook["type"] == "bidirectional-webhooks"}
		activities, hasActivities := hook["activities"].(bson.A)
		selected := map[string]bool{}
		for _, item := range activities {
			if activity, ok := item.(string); ok {
				selected[activity] = true
			}
		}
		row.AllActivities = !hasActivities || selected["all"]
		for _, group := range webhookActivityGroups {
			descriptions := strings.Fields(group.names)
			checked := !row.AllActivities
			for i, name := range descriptions {
				descriptions[i] = "act-" + name
				checked = checked && selected[descriptions[i]]
			}
			row.Groups = append(row.Groups, webhookGroupView{"notification-activity-" + group.key, strings.Join(descriptions, " "), checked})
		}
		rows = append(rows, row)
	}
	w.Header().Set("Cache-Control", "private, no-store")
	s.render(w, r, "boardwebhooks", http.StatusOK, map[string]any{"Title": tr(languageOf(r, u).Tag, titleKey), "User": u, "Board": b, "Hooks": rows, "Endpoint": endpoint})
}
