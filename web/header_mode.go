package web

import (
	"encoding/json"
	"net/http"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// methodSetMobileMode is WeKan's setMobileMode: the user's own
// profile.mobileMode, true or false, as the header's mobile/desktop toggle
// chooses it (client/lib/utils.js setMobileMode). The browser keeps the same
// choice in localStorage's wekan-mobile-mode, which it reads first.
func (s *Server) methodSetMobileMode(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	var on bool
	if len(args) != 1 || string(args[0]) == "null" || json.Unmarshal(args[0], &on) != nil {
		return nil, &methodError{"Match.Error", "Expected boolean"}
	}
	_, err := s.db.Collection("users").UpdateOne(r.Context(), bson.M{"_id": eq(u.ID)},
		bson.M{"$set": bson.M{"profile.mobileMode": on}})
	return nil, err
}

// MobileModeChoice is the user's profile.mobileMode as the header's toggle
// reads it: "true", "false", or "" when never chosen.
func (u *User) MobileModeChoice() string {
	if u == nil || u.Profile.MobileMode == nil {
		return ""
	}
	if *u.Profile.MobileMode {
		return "true"
	}
	return "false"
}
