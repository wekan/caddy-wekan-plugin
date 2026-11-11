package web

// A card's or swimlane's Scrum metadata on another board, as WeKan's
// models/lib/scrumCopy.js (maintainer decision of 2026-10-02): on its own
// board a copy keeps all of it; on another board the sprint and each release
// are linked to that board's own of the same name when exactly one matches -
// a planned or active sprint, a release not cancelled - and dropped
// otherwise. Past sprints and the rank describe the board left and are
// always dropped. Issue type, acceptance criteria and a swimlane's purpose
// stay. The copy's revision starts at 1.

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

var boardScopedScrum = []string{"sprintId", "pastSprintIds", "releaseId", "releaseIds", "backlogRank"}

// scrumPlanning is a board pair's sprints and releases (scrumPlanningPair.js).
type scrumPlanning struct{ fromSprints, fromReleases, toSprints, toReleases []map[string]any }

func (s *Server) scrumPlanningPair(ctx context.Context, from, to string) *scrumPlanning {
	f := options.Find().SetProjection(bson.M{"_id": 1, "name": 1, "state": 1})
	return &scrumPlanning{s.findDocs(ctx, "scrumSprints", bson.M{"boardId": eq(from)}, f), s.findDocs(ctx, "scrumReleases", bson.M{"boardId": eq(from)}, f),
		s.findDocs(ctx, "scrumSprints", bson.M{"boardId": eq(to)}, f), s.findDocs(ctx, "scrumReleases", bson.M{"boardId": eq(to)}, f)}
}

func scrumCounterpart(id string, from, to []map[string]any, usable func(map[string]any) bool) string {
	name := ""
	for _, r := range from {
		if docString(r, "_id") == id {
			name = jsTrim(docString(r, "name"))
		}
	}
	if name == "" {
		return ""
	}
	match := ""
	n := 0
	for _, r := range to {
		if usable(r) && jsTrim(docString(r, "name")) == name {
			match = docString(r, "_id")
			n++
		}
	}
	if n != 1 {
		return ""
	}
	return match
}

// boardScrum is WeKan's: the metadata for the other board.
func boardScrum(sc map[string]any, p *scrumPlanning) map[string]any {
	out := map[string]any{}
	for k, v := range sc {
		if !containsString(boardScopedScrum, k) {
			out[k] = v
		}
	}
	if p == nil {
		return out
	}
	if sid := docString(sc, "sprintId"); sid != "" {
		if t := scrumCounterpart(sid, p.fromSprints, p.toSprints, func(r map[string]any) bool { st := r["state"]; return st == "planned" || st == "active" }); t != "" {
			out["sprintId"] = t
		}
	}
	ids := []string{}
	for _, id := range cardReleaseIds(sc) {
		if t := scrumCounterpart(id, p.fromReleases, p.toReleases, func(r map[string]any) bool { return r["state"] != "cancelled" }); t != "" && !containsString(ids, t) {
			ids = append(ids, t)
		}
	}
	if _, list := sc["releaseIds"].([]any); list {
		return withCardReleaseIds(out, ids)
	}
	if len(ids) > 0 {
		out["releaseId"] = ids[0]
	}
	return out
}

// copiedScrum is copiedScrumMetadata: the copy's scrum and scrumRevision, or
// nothing when the source has none.
func copiedScrum(doc map[string]any, toBoard string, p *scrumPlanning) bson.M {
	sc, _ := plain(doc["scrum"]).(map[string]any)
	if sc == nil {
		return nil
	}
	if docString(doc, "boardId") != toBoard {
		sc = boardScrum(sc, p)
	}
	return bson.M{"scrum": sc, "scrumRevision": int32(1)}
}
