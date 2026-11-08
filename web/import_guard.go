package web

import (
	"context"
	"net/http"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Admin Panel can turn all import off (disableAllImport), and Node.js WeKan
// then refuses every import entry point (models/lib/importExportSecurity.js
// assertImportEnabled: "Import is disabled by the administrator."). Weddy
// shares that settings document, so its every import route asks the same
// switch first - importable wraps them all, and
// TestEveryImportRouteIsImportable keeps it that way. WeKan records no
// Problem for it, and Weddy does not invent one.

func (s *Server) importDisabled(ctx context.Context) bool {
	var set struct {
		DisableAllImport bool `bson:"disableAllImport"`
	}
	_ = s.db.Collection("settings").FindOne(ctx, bson.M{}).Decode(&set)
	return set.DisableAllImport
}

func (s *Server) importable(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.importDisabled(r.Context()) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "import-disabled",
				"reason": "Import is disabled by the administrator."})
			return
		}
		h(w, r)
	}
}
