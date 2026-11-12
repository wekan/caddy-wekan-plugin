package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// The security log WeKan's Admin Panel -> Problems reads: the eventlog
// collection, stream "security", in the shape Node.js WeKan writes it
// (server/lib/securityLog.js, server/lib/eventLogFold.js,
// models/lib/eventLogSummary.js) - so the same Admin Panel shows what Weddy's
// guards refused, under the same Hall of Fame names.
//
// ONE ROW PER PROBLEM, never a row per event. A guard on a path an attacker
// controls fires as fast as they can send, so the row carries count, the window
// firstAt ... at, the latest actor, and a capped per-actor tally.

// securityCategory is one key of WeKan's models/lib/securityCategories.js.
type securityCategory struct {
	Category, Bleed, Severity, CWE string
	// BlocksAccount false: the event names a victim, never block it.
	NeverBlocks bool
}

var securityCategories = map[string]securityCategory{
	"authz.list-locked": {Category: "authz", Bleed: "ListLockBleed", Severity: "medium", CWE: "CWE-863", NeverBlocks: true},
	"ssrf.webhook":      {Category: "ssrf", Bleed: "IntegrationBleed", Severity: "high", CWE: "CWE-918", NeverBlocks: true},
	"ssrf.attachment":   {Category: "ssrf", Bleed: "LiveBleed", Severity: "high", CWE: "CWE-918", NeverBlocks: true},
	// UnassignedAttachBleed (GHSA-w26g-rc43-q6cm): an assigned-only member
	// changing an attachment of a card no longer assigned to them.
	// FileBleed: active content taken out of a file as it was served.
	"file.content":                    {Category: "file", Bleed: "FileBleed", Severity: "medium", CWE: "CWE-79"},
	"authz.assigned-attachment-write": {Category: "authz", Bleed: "UnassignedAttachBleed", Severity: "medium", CWE: "CWE-863"},
	"authz.parent":                    {Category: "authz", Bleed: "ParentBleed", Severity: "high", CWE: "CWE-862"},
	"authz.assigned":                  {Category: "authz", Bleed: "AssignedBleed", Severity: "medium", CWE: "CWE-863"},
	"authz.comment":                   {Category: "authz", Bleed: "CommentBleed", Severity: "medium", CWE: "CWE-639"},
	"authz.comment-card":              {Category: "authz", Bleed: "CommentBoundaryBleed", Severity: "high", CWE: "CWE-639"},
	"authz.invitation-boards":         {Category: "authz", Bleed: "InvitationBoardBleed", Severity: "high", CWE: "CWE-639"},
	"brute.invite":                    {Category: "brute-force", Bleed: "InviteBleed", Severity: "high", CWE: "CWE-307"},
	"authz.invitation-profile":        {Category: "authz", Bleed: "InviteProfileBleed", Severity: "high", CWE: "CWE-863"},
	"authz.rule-button":               {Category: "authz", Bleed: "RuleButtonBleed", Severity: "high", CWE: "CWE-639"},
	"authz.canary":                    {Category: "authz", Bleed: "CanaryBleed", Severity: "medium", CWE: "CWE-863"},
	"dos.rule-loop":                   {Category: "dos", Bleed: "BypassBleed", Severity: "medium", CWE: "CWE-674"},
	"authz.board-creation":            {Category: "authz", Bleed: "CreateBleed", Severity: "low", CWE: "CWE-863", NeverBlocks: true},
	"authz.board-visibility":          {Category: "authz", Bleed: "VisibilityBleed", Severity: "medium", CWE: "CWE-863"},
	"authz.assigned-card-write":       {Category: "authz", Bleed: "UnassignedArchiveBleed", Severity: "medium", CWE: "CWE-863"},
	// CrossBoardExportBleed: an export while Admin Panel has turned export
	// off - recorded, never costing the account, as WeKan's key is.
	"authz.export-disabled": {Category: "authz", Bleed: "CrossBoardExportBleed", Severity: "medium", CWE: "CWE-863", NeverBlocks: true},
	// ImportPlacementBleed: an import beside another board's list, swimlane
	// or card - recorded, never costing the account, as WeKan's key is.
	"authz.import-placement": {Category: "authz", Bleed: "ImportPlacementBleed", Severity: "medium", CWE: "CWE-639", NeverBlocks: true},
	// A board export scoped to another board's checklist: no menu asks for it.
	"authz.export-checklist-scope": {Category: "authz", Bleed: "CrossBoardExportBleed", Severity: "medium", CWE: "CWE-639"},
	"authz.admin-only-field":       {Category: "authz", Bleed: "AdminFieldBleed", Severity: "high", CWE: "CWE-863"},
	"authz.legacy-avatar":          {Category: "authz", Bleed: "RelicAvatarBleed", Severity: "high", CWE: "CWE-862"},
	"auth-race.oidc":               {Category: "auth-race", Bleed: "OIDCBleed", Severity: "high", CWE: "CWE-362"},
	"authz.manage-board":           {Category: "authz", Bleed: "ManageBoardBleed", Severity: "high", CWE: "CWE-863"},
	// #6798: the same refusal reaches a read-only member working from a stale
	// screen, so it is recorded but never disables the account.
	"authz.linked-write": {Category: "authz", Bleed: "LinkedWriteBleed", Severity: "high", CWE: "CWE-863", NeverBlocks: true},
	// server/lib/secureTransfer.js and models/lib/transferFailure.cjs: markup
	// an import carried was removed, or the file was over a limit. A real
	// board reaches both, so neither costs the account.
	// security_attempts.go: attacks already refused, recorded as WeKan's are.
	"authz.repoint":       {Category: "authz", Bleed: "RepointBleed", Severity: "high", CWE: "CWE-863"},
	"injection.prototype": {Category: "injection", Bleed: "PrototypeBleed", Severity: "high", CWE: "CWE-1321"},
	"ssrf.webhook-forge":  {Category: "ssrf", Bleed: "HookBleed", Severity: "high", CWE: "CWE-345"},
	"authz.background":    {Category: "authz", Bleed: "BackgroundBleed", Severity: "high", CWE: "CWE-639"},
	// GroupLinkBleed (GHSA-v544-rmr5-38gx): a client board write naming
	// organizations, teams or domains, or adding a member past
	// inviteUserToBoard. Medium: recorded, the account kept, as WeKan's.
	"authz.group-link": {Category: "authz", Bleed: "GroupLinkBleed", Severity: "medium", CWE: "CWE-863"},
	"authz.member-add": {Category: "authz", Bleed: "GroupLinkBleed", Severity: "medium", CWE: "CWE-863"},
	// A change history row that fails its chain (changeHistoryIntegrity.js).
	"integrity.history": {Category: "integrity", Bleed: "HistoryIntegrity", Severity: "critical", CWE: "CWE-345"},
	// A mutation of a board the caller may not change (boardMutationGuard.js).
	"authz.mutation": {Category: "authz", Bleed: "MutationBleed", Severity: "high", CWE: "CWE-863"},
	// CopyIdentityBleed: a copy given another board's or card's identity.
	"authz.board-copy-overrides": {Category: "authz", Bleed: "CopyIdentityBleed", Severity: "high", CWE: "CWE-915"},
	"authz.card-copy-overrides":  {Category: "authz", Bleed: "CopyIdentityBleed", Severity: "high", CWE: "CWE-915"},
	"transfer.sanitized":         {Category: "Import/export validation", Bleed: "ImportExportSanitization", Severity: "warning", NeverBlocks: true},
	"transfer.limit":             {Category: "Import/export validation", Bleed: "ImportExportValidation", Severity: "medium", NeverBlocks: true},
}

// identityFields make two events the same problem; the actor is not one of them.
var identityFields = []string{"stream", "bleed", "category", "action", "source", "severity", "cwe", "type", "db", "kind", "api", "apiUserId"}

const (
	maxActors = 50  // models/lib/eventLogSummary.js MAX_ACTORS
	maxDetail = 500 // models/lib/securityLogFormat.js MAX_DETAIL
)

var controlRuns = regexp.MustCompile(`[\x00-\x1f\x7f]+`)

func sanitizeDetail(s string) string {
	s = strings.Join(strings.Fields(controlRuns.ReplaceAllString(s, " ")), " ")
	if r := []rune(s); len(r) > maxDetail {
		s = string(r[:maxDetail-1]) + "…"
	}
	return s
}

// classifyAddress is the address in its family; a dual-stack ::ffff: IPv4 is IPv4.
func classifyAddress(ip string) (ipv4, ipv6 string) {
	a := net.ParseIP(strings.Trim(ip, "[]"))
	if a == nil {
		return "", ""
	}
	if v4 := a.To4(); v4 != nil {
		return v4.String(), ""
	}
	return "", a.String()
}

// actorKey is models/lib/eventLogSummary.js actorKeyFor: opaque, as a field name.
func actorKey(kind, value string) string {
	h := sha256.Sum256([]byte(kind + ":" + value))
	return hex.EncodeToString(h[:])[:16]
}

// securityEvent is one refusal, as a guard reports it.
type securityEvent struct {
	Key, Action, Source, Detail string
	UserID                      string
}

// recordSecurity folds one event into its problem's row, and - as WeKan does -
// disables a logged-in account that attempted a high-severity hole. It never
// fails the caller: the refusal matters more than the record of it.
func (s *Server) recordSecurity(r *http.Request, e securityEvent) {
	defer func() { _ = recover() }() // logging must never break the guard
	ctx := context.WithoutCancel(r.Context())
	cat := securityCategories[e.Key]
	evt := bson.M{
		"stream": "security", "severity": orDefault(cat.Severity, "info"), "category": orDefault(cat.Category, "unknown"),
		"bleed": orDefault(cat.Bleed, "Generic"), "action": orDefault(e.Action, "detected"), "source": e.Source, "cwe": cat.CWE,
	}
	latest := bson.M{"detail": sanitizeDetail(e.Detail)}
	type actor struct{ kind, value, family string }
	var actors []actor
	if e.UserID != "" {
		latest["userId"] = e.UserID
		var u User
		if s.db.Collection("users").FindOne(ctx, bson.M{"_id": e.UserID}).Decode(&u) == nil && u.Username != "" {
			name := u.Username
			if len(name) > 100 {
				name = name[:100]
			}
			latest["username"] = name
			actors = append(actors, actor{kind: "user", value: name})
		}
	}
	if v4, v6 := classifyAddress(clientKey(r)); v4 != "" || v6 != "" {
		if v4 != "" {
			latest["ip"], latest["ipv4"] = v4, v4
			actors = append(actors, actor{"ip", v4, "ipv4"})
		} else {
			latest["ip"], latest["ipv6"] = v6, v6
			actors = append(actors, actor{"ip", v6, "ipv6"})
		}
	}
	identity, onInsert := bson.M{}, bson.M{"firstAt": time.Now().UTC()}
	for _, f := range identityFields {
		if v, ok := evt[f].(string); ok && v != "" {
			identity[f], onInsert[f] = v, v
		} else {
			identity[f] = bson.M{"$exists": false}
		}
	}
	now := time.Now().UTC()
	set, inc := bson.M{"at": now}, bson.M{"count": 1}
	for k, v := range latest {
		if v != "" {
			set[k] = v
		}
	}
	var row struct {
		Actors map[string]any `bson:"actors"`
	}
	_ = s.db.Collection("eventlog").FindOne(ctx, identity, options.FindOne().SetProjection(bson.M{"actors": 1})).Decode(&row)
	room := maxActors - len(row.Actors)
	for _, a := range actors {
		k := actorKey(a.kind, a.value)
		if _, known := row.Actors[k]; known {
			set["actors."+k+".at"], inc["actors."+k+".count"] = now, 1
			continue
		}
		if room <= 0 {
			inc["actorsOverflow"] = 1
			continue
		}
		room--
		set["actors."+k+".at"], set["actors."+k+".kind"], set["actors."+k+".value"] = now, a.kind, a.value
		set["actors."+k+".family"] = a.family
		if a.family == "" {
			delete(set, "actors."+k+".family")
		}
		inc["actors."+k+".count"] = 1
	}
	_, _ = s.db.Collection("eventlog").UpdateOne(ctx, identity,
		bson.M{"$set": set, "$setOnInsert": onInsert, "$inc": inc}, options.UpdateOne().SetUpsert(true))

	// server/lib/blockOnSecurityEvent.js: a blocked high or critical attempt by a
	// logged-in account disables that account, keeping the first reason.
	if e.UserID != "" && e.Action == "blocked" && !cat.NeverBlocks && (cat.Severity == "high" || cat.Severity == "critical") {
		reason := "Attempted " + orDefault(cat.Bleed, "a security guard")
		if e.Source != "" {
			reason += " at " + e.Source
		}
		ip, _ := latest["ip"].(string)
		_, _ = s.db.Collection("users").UpdateOne(ctx, bson.M{"_id": e.UserID, "loginDisabled": bson.M{"$ne": true}},
			bson.M{"$set": bson.M{"loginDisabled": true, "services.securityBlock": bson.M{
				"at": now, "reason": reason, "bleed": cat.Bleed, "source": e.Source, "ip": ip}}})
	}
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
