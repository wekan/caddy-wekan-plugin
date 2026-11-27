package web

// WeKan's Admin Panel / Settings / Features / Turn off all webhooks
// (models/lib/webhooksSwitch.js, server/webhooksSwitch.js): while the site
// setting webhooksDisabled is true nothing is queued and nothing is sent -
// an activity's webhooks, a bot's, a two-way reply - and no webhook is added
// or changed, through REST or Meteor. A job already waiting is kept, and sent
// once webhooks are turned on again. Removing a webhook is the cleanup and is
// never refused. A refused write is recorded: no screen offers it while the
// switch is on, so it is an attempt.

import (
	"context"
	"errors"
	"net/http"

	"go.mongodb.org/mongo-driver/v2/bson"
)

var errWebhooksOff = errors.New("webhooks-disabled")

// webhooksOff: is Turn off all webhooks on?
func (s *Server) webhooksOff(ctx context.Context) bool {
	var doc struct {
		Off bool `bson:"webhooksDisabled"`
	}
	_ = s.db.Collection("settings").FindOne(ctx, bson.M{}).Decode(&doc)
	return doc.Off
}

// refuseWebhookWrite answers true, and records the attempt, when a webhook
// may not be added or changed now. Asked after the caller's own board-admin
// check, so only somebody who could otherwise write it is told.
func (s *Server) refuseWebhookWrite(r *http.Request, u *User, source, detail string) bool {
	if !s.webhooksOff(r.Context()) {
		return false
	}
	uid := ""
	if u != nil {
		uid = u.ID
	}
	s.recordSecurity(r, securityEvent{Key: "authz.webhooks-disabled", Action: "blocked", Source: source, UserID: uid, Detail: detail})
	return true
}

// webhooksOffAnswer is WeKan's WEBHOOKS_DISABLED, as REST answers it.
func webhooksOffAnswer(w http.ResponseWriter) {
	writeJSON(w, http.StatusForbidden, map[string]any{"error": "webhooks-disabled", "reason": "Webhooks are turned off"})
}

var webhooksOffError = &methodError{"webhooks-disabled", "Webhooks are turned off"}
