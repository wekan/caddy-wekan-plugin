package web

// Scrum's daily observations, as WeKan's server/scrumDailySnapshots.js,
// server/lib/scrumDailyCapture.js and models/lib/scrumDailyHistory.js: the
// first observation of each UTC day of an active sprint - its cards'
// estimates and completion under the policy the sprint started with - kept
// as it was measured, never backfilled, never replaced. They are taken every
// 15 minutes and whenever a sprint's daily history is read;
// scrum.getDailyHistory answers the newest 366, the reader's cards only.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// dailyObservationID is WeKan's: sha256 of JSON [sprintId, start, day].
func dailyObservationID(sprintID string, start time.Time, day string) string {
	raw, _ := json.Marshal([]string{sprintID, start.UTC().Format("2006-01-02T15:04:05.000Z"), day})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// captureScrumDay is captureOneSprint: today's observation of an active
// sprint, unless it has one or its Scrum import is unfinished.
func (s *Server) captureScrumDay(ctx context.Context, sprint map[string]any) {
	start, _ := sprint["startSnapshot"].(map[string]any)
	if sprint["scrumImportPending"] == true || sprint["state"] != "active" || start == nil {
		return
	}
	boardID, sprintID := docString(sprint, "boardId"), docString(sprint, "_id")
	if s.scrumImportPending(ctx, boardID) != nil {
		return
	}
	startedAt, ok := plain(start["at"]).(time.Time)
	at := time.Now().UTC().Truncate(time.Millisecond)
	if !ok || at.Before(startedAt) {
		return
	}
	src, pol, unit := docString(start, "estimateSource"), docString(start, "completionPolicy"), docString(start, "unit")
	if src != "poker" && src != "customField" || pol != "dueComplete" && pol != "doneLists" || unit == "" {
		return
	}
	day := at.Format("2006-01-02")
	if s.db.Collection("scrumDailySnapshots").FindOne(ctx, bson.M{"sprintId": eq(sprintID), "boardId": eq(boardID), "startedAt": startedAt, "day": day}).Err() == nil {
		return
	}
	cards, lists := s.scrumSnapshotInputs(ctx, boardID, sprintID, true)
	current := s.findDoc(ctx, "scrumSprints", bson.M{"_id": eq(sprintID)})
	if current == nil || current["state"] != "active" || canonical(current["startSnapshot"]) != canonical(start) {
		return
	}
	settings := defaultScrumSettings()
	settings["estimateSource"], settings["estimateCustomFieldId"], settings["estimateUnit"], settings["completionPolicy"] = src, start["estimateCustomFieldId"], unit, pol
	if days, ok := start["workingDays"].([]any); ok {
		settings["workingDays"] = days
	}
	snapshot := sprintSnapshot(cards, settings, lists, at)
	if start["partial"] == true {
		snapshot["partial"] = true
	}
	header, err := s.storeScrumSnapshot(ctx, boardID, sprintID, "daily", snapshot)
	if err != nil {
		return
	}
	doc := bson.M{"_id": dailyObservationID(sprintID, startedAt, day), "boardId": boardID, "sprintId": sprintID, "startedAt": startedAt, // inserted, not a filter
		"day": day, "capturedAt": at, "consistency": "observed", "snapshot": header}
	if _, err := s.db.Collection("scrumDailySnapshots").InsertOne(ctx, doc); mongo.IsDuplicateKeyError(err) {
		// Another capture of the day won: its rows are the day's.
		_, _ = s.db.Collection("scrumSnapshotRows").DeleteMany(ctx, bson.M{"sprintId": eq(sprintID), "kind": "daily", "at": at})
	}
}

// runScrumDaily is scanScrumDailySnapshots, every 15 minutes.
func (s *Server) runScrumDaily(stop <-chan struct{}) {
	t := time.NewTicker(15 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			for _, sp := range s.findDocs(ctx, "scrumSprints", bson.M{"state": "active"}) {
				// A failure leaves a visible missing day; nothing is made up.
				func() {
					defer func() { _ = recover() }()
					s.captureScrumDay(ctx, sp)
				}()
			}
			cancel()
		}
	}
}

// dailyRows is dailyHistoryRows: one row per observation, its totals of the
// cards the reader may see (visible nil: all).
func dailyRows(obs map[string]any, cards []map[string]any, visible map[string]bool) map[string]any {
	snap, _ := obs["snapshot"].(map[string]any)
	kept := []map[string]any{}
	for _, c := range cards {
		if visible == nil || visible[docString(c, "cardId")] {
			kept = append(kept, c)
		}
	}
	var open, done []map[string]any
	for _, c := range kept {
		if c["done"] == true {
			done = append(done, c)
		} else {
			open = append(open, c)
		}
	}
	return map[string]any{"day": obs["day"], "capturedAt": obs["capturedAt"], "unit": snap["unit"], "scope": scrumTotal(kept),
		"estimateSource": snap["estimateSource"], "estimateCustomFieldId": snap["estimateCustomFieldId"], "completionPolicy": snap["completionPolicy"],
		"remaining": scrumTotal(open), "completed": scrumTotal(done), "partial": visible != nil || snap["partial"] == true, "consistency": "observed"}
}

// scrumDailyHistory is getScrumDailyHistory.
func (s *Server) scrumDailyHistory(ctx context.Context, u *User, boardID, sprintID string) (map[string]any, error) {
	if sprintID == "" || len(sprintID) > 200 {
		return nil, scrumInvalidErr("Invalid sprint identifier")
	}
	b, _, err := s.scrumBoard(ctx, u, boardID, false)
	if err != nil {
		return nil, err
	}
	if err := s.scrumImportPending(ctx, boardID); err != nil {
		return nil, err
	}
	sprint := s.findDoc(ctx, "scrumSprints", bson.M{"_id": eq(sprintID), "boardId": eq(boardID)})
	if sprint == nil {
		return nil, &methodError{"not-found", "not-found"}
	}
	partial := assignedOnlyOn(b, u)
	start, _ := sprint["startSnapshot"].(map[string]any)
	if start == nil {
		return map[string]any{"rows": []any{}, "partial": partial, "truncated": false, "sprintName": sprint["name"]}, nil
	}
	var visible map[string]bool
	if partial {
		visible = map[string]bool{}
		for _, c := range s.findDocs(ctx, "cards", scrumCardFilter(b, u), options.Find().SetProjection(bson.M{"_id": 1})) {
			visible[docString(c, "_id")] = true
		}
	}
	// Reading takes today's first observation too - of the whole sprint;
	// only the answer is the reader's.
	s.captureScrumDay(ctx, sprint)
	at, _ := plain(start["at"]).(time.Time)
	obs := s.findDocs(ctx, "scrumDailySnapshots", bson.M{"boardId": eq(boardID), "sprintId": eq(sprintID), "startedAt": at},
		options.Find().SetSort(bson.D{{Key: "capturedAt", Value: -1}}).SetLimit(367))
	truncated := len(obs) > 366
	if truncated {
		obs = obs[:366]
	}
	rows := []any{}
	for i := len(obs) - 1; i >= 0; i-- {
		o := obs[i]
		snap, _ := o["snapshot"].(map[string]any)
		cards := snapshotCards(snap)
		if snap != nil && snap["stored"] == "rows" {
			cards, err = s.scrumSnapshotRows(ctx, map[string]any{"_id": sprintID, "daily": snap}, "daily")
			if err != nil {
				return nil, err
			}
		}
		rows = append(rows, dailyRows(o, cards, visible))
	}
	return map[string]any{"rows": rows, "partial": partial, "truncated": truncated, "sprintName": sprint["name"]}, nil
}

func (s *Server) methodScrumDailyHistory(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	boardID, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	sprintID, err := stringArg(args, 1)
	if err != nil {
		return nil, err
	}
	return s.scrumDailyHistory(r.Context(), u, boardID, sprintID)
}

// scrumDailyChartRows is chartExportRows' scrumDaily: an observation a row,
// and the notices WeKan's export adds.
func (s *Server) scrumDailyChartRows(ctx context.Context, u *User, b *Board, lang, sprintID string) ([]any, [][]any, []string, error) {
	if u == nil {
		u = &User{}
	}
	data, err := s.scrumDailyHistory(ctx, u, b.ID, sprintID)
	if err != nil {
		return nil, nil, nil, err
	}
	headers := []any{tr(lang, "scrum-sprint"), tr(lang, "date"), "UTC", tr(lang, "scrum-estimate-unit")}
	for _, k := range []string{msg("scrum-observed-scope"), msg("scrum-incomplete"), msg("scrum-completed")} {
		label := tr(lang, k)
		headers = append(headers, label+": "+tr(lang, "cards"), label+": "+tr(lang, "scrum-estimate"), label+": "+tr(lang, "scrum-unknown-estimate"))
	}
	headers = append(headers, tr(lang, "scrum-estimate-source"), "Custom field", tr(lang, "scrum-completion-policy"))
	notices := []string{tr(lang, "scrum-daily-observations-export-help")}
	rows := [][]any{}
	anyPartial := false
	for _, x := range data["rows"].([]any) {
		row := x.(map[string]any)
		anyPartial = anyPartial || row["partial"] == true
		at, _ := plain(row["capturedAt"]).(time.Time)
		out := []any{docString(data, "sprintName"), docString(row, "day"), at.UTC().Format("2006-01-02T15:04:05.000Z"), docString(row, "unit")}
		for _, k := range []string{"scope", "remaining", "completed"} {
			t := row[k].(map[string]any)
			out = append(out, numberOf(t["count"]), numberOf(t["estimate"]), numberOf(t["unknown"]))
		}
		out = append(out, docString(row, "estimateSource"), docString(row, "estimateCustomFieldId"), docString(row, "completionPolicy"))
		rows = append(rows, out)
	}
	if data["partial"] == true {
		notices = append(notices, tr(lang, "scrum-partial-report"))
	}
	if anyPartial {
		notices = append(notices, tr(lang, "scrum-partial-snapshot"))
	}
	if data["truncated"] == true {
		notices = append(notices, tr(lang, "scrum-daily-truncated"))
	}
	return headers, rows, notices, nil
}

// A download link's token (exportDownloadToken): a page cannot read its
// session's HttpOnly cookie, and a chart's export link is opened outside
// the page, so the page asks for a token of its own - valid for the export
// routes only, for 15 minutes, stored as its hash.
const exportTokenTTL = 15 * time.Minute

func (s *Server) methodExportDownloadToken(r *http.Request, u *User, _ []json.RawMessage) (any, error) {
	token := randomString(tokenAlphabet, 43)
	if _, err := s.db.Collection("exportDownloadTokens").InsertOne(r.Context(), bson.M{"_id": hashToken(token), "userId": u.ID,
		"expiresAt": time.Now().UTC().Add(exportTokenTTL)}); err != nil {
		return nil, err
	}
	return token, nil
}

// exportTokenUser is the user of a live download token, or nil.
func (s *Server) exportTokenUser(ctx context.Context, token string) *User {
	var t struct {
		UserID    string    `bson:"userId"`
		ExpiresAt time.Time `bson:"expiresAt"`
	}
	if s.db.Collection("exportDownloadTokens").FindOne(ctx, bson.M{"_id": hashToken(token)}).Decode(&t) != nil || time.Now().After(t.ExpiresAt) {
		return nil
	}
	var u User
	if s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(t.UserID)}).Decode(&u) != nil {
		return nil
	}
	return &u
}
