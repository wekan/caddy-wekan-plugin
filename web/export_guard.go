package web

import (
	"context"
	"net/http"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Admin Panel can turn all export off (CrossBoardExportBleed,
// GHSA-8885-xrq4-j47h), and Node.js WeKan then answers every export route
// 403 before anything else (models/lib/importExportSecurity.js
// refuseDisabledExport). Weddy shares that settings document, so every one of
// its export routes asks the same switch first - exportable wraps them all,
// and TestEveryExportRouteIsExportable keeps it that way.

func (s *Server) exportDisabled(ctx context.Context) bool {
	var set struct {
		DisableAllExport bool `bson:"disableAllExport"`
	}
	_ = s.db.Collection("settings").FindOne(ctx, bson.M{}).Decode(&set)
	return set.DisableAllExport
}

func (s *Server) exportable(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.exportDisabled(r.Context()) {
			// The menus are hidden then, so somebody asked by hand: recorded,
			// and never costing the account (the catalog key never blocks).
			// recordSecurity recovers itself: logging never breaks the guard.
			s.recordSecurity(r, securityEvent{Key: "authz.export-disabled", Action: "blocked", Source: "export",
				Detail: "export while all export is disabled"})
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("Export is disabled by the administrator."))
			return
		}
		h(w, r)
	}
}
