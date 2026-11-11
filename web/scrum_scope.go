package web

// A started sprint's scope and burndown replayed change by change from
// History, as WeKan's models/lib/scrumScopeReplay.js and server/scrum.js
// getScrumScopeHistory: sprint membership from Scrum rows, estimates from the
// card's custom fields or planning poker, completion from dueComplete or the
// card's list, and archiving. Each card's state at the start is rebuilt
// backwards from now, then replayed forwards, a point after each row that
// changes the totals; a start, close or present that History does not
// reproduce marks the result inconsistent rather than presenting it as fact.

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const maxScopeReplayRows = 20000

type scopeChange struct {
	card, field   string
	before, after any
}

// scopeChangesOf is changesOf(row).
func scopeChangesOf(row map[string]any) []scopeChange {
	if _, ok := row["createdAt"].(time.Time); !ok {
		return nil
	}
	if row["group"] == "scrum" {
		side := func(c any) map[string]any {
			out := map[string]any{}
			m, _ := c.(map[string]any)
			recs, _ := m["records"].([]any)
			for _, x := range recs {
				r, _ := x.(map[string]any)
				if r != nil && r["type"] == "card" && docString(r, "id") != "" {
					doc, _ := r["document"].(map[string]any)
					sc, _ := doc["scrum"].(map[string]any)
					out[docString(r, "id")] = sc["sprintId"]
				}
			}
			return out
		}
		before, after := side(row["previousContent"]), side(row["newContent"])
		ids := []string{}
		for id := range before {
			ids = append(ids, id)
		}
		for id := range after {
			if _, ok := before[id]; !ok {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		out := []scopeChange{}
		for _, id := range ids {
			out = append(out, scopeChange{id, "sprintId", before[id], after[id]})
		}
		return out
	}
	id := docString(row, "entityId")
	if row["entityType"] != "card" || id == "" {
		return nil
	}
	if row["group"] == "position" {
		list := func(c any) any {
			m, _ := c.(map[string]any)
			if s, ok := m["listId"].(string); ok {
				return s
			}
			return nil
		}
		return []scopeChange{{id, "listId", list(row["previousContent"]), list(row["newContent"])}}
	}
	nc, _ := row["newContent"].(map[string]any)
	pc, _ := row["previousContent"].(map[string]any)
	field, _ := nc["field"].(string)
	if field == "" {
		field, _ = pc["field"].(string)
	}
	switch field {
	case "customFields", "poker", "dueComplete", "archived":
	default:
		return nil
	}
	read := func(c map[string]any) any {
		if c == nil {
			return nil
		}
		v, ok := historyValue(c)
		if !ok {
			return nil
		}
		return v
	}
	return []scopeChange{{id, field, read(pc), read(nc)}}
}

type scopeCard = map[string]any

func scopeStateOf(c map[string]any) scopeCard {
	sc, _ := c["scrum"].(map[string]any)
	return scopeCard{"sprintId": sc["sprintId"], "customFields": c["customFields"], "poker": c["poker"],
		"dueComplete": c["dueComplete"] == true, "listId": c["listId"], "archived": c["archived"] == true}
}

func scopeSet(sc scopeCard, field string, v any) {
	if field == "dueComplete" || field == "archived" {
		sc[field] = v == true
	} else {
		sc[field] = v
	}
}

// replaySprintScope is WeKan's, its result as the method answers it.
func replaySprintScope(sprint map[string]any, cards []map[string]any, rows []map[string]any, doneLists map[string]bool, now time.Time) map[string]any {
	snap, _ := sprint["startSnapshot"].(map[string]any)
	startedAt, ok := sprint["startedAt"].(time.Time)
	if snap == nil || !ok {
		return nil
	}
	settings := map[string]any{"estimateSource": snap["estimateSource"], "estimateCustomFieldId": snap["estimateCustomFieldId"], "completionPolicy": snap["completionPolicy"]}
	truncated := len(rows) > maxScopeReplayRows
	if truncated {
		rows = rows[:maxScopeReplayRows]
	}
	var replayed []map[string]any
	for _, r := range rows {
		if at, ok := r["createdAt"].(time.Time); ok && !at.Before(startedAt) && !at.After(now) {
			replayed = append(replayed, r)
		}
	}
	sort.SliceStable(replayed, func(i, j int) bool {
		return replayed[i]["createdAt"].(time.Time).Before(replayed[j]["createdAt"].(time.Time))
	})
	state, current := map[string]scopeCard{}, map[string]scopeCard{}
	for _, c := range cards {
		state[docString(c, "_id")], current[docString(c, "_id")] = scopeStateOf(c), scopeStateOf(c)
	}
	for i := len(replayed) - 1; i >= 0; i-- {
		for _, ch := range scopeChangesOf(replayed[i]) {
			if st := state[ch.card]; st != nil {
				scopeSet(st, ch.field, ch.before)
			}
		}
	}
	sprintID := docString(sprint, "_id")
	totals := func(of map[string]scopeCard) map[string]float64 {
		var scope, completed, unknown, count float64
		for _, c := range of {
			if c["sprintId"] != sprintID || c["archived"] == true {
				continue
			}
			count++
			est := scrumCardEstimate(c, settings)
			if est == nil {
				unknown++
				continue
			}
			e := numberOf(est)
			scope += e
			if settings["completionPolicy"] == "doneLists" && doneLists[docString(c, "listId")] || settings["completionPolicy"] != "doneLists" && c["dueComplete"] == true {
				completed += e
			}
		}
		return map[string]float64{"scope": scope, "completed": completed, "remaining": scope - completed, "unknown": unknown, "cards": count}
	}
	point := func(at time.Time, t map[string]float64, cause any) map[string]any {
		return map[string]any{"at": at, "scope": storedNum(t["scope"]), "completed": storedNum(t["completed"]), "remaining": storedNum(t["remaining"]),
			"unknown": storedNum(t["unknown"]), "cards": storedNum(t["cards"]), "cause": cause}
	}
	differs := func(a, b map[string]float64) bool {
		for _, k := range []string{"scope", "completed", "unknown", "cards"} {
			if a[k] != b[k] {
				return true
			}
		}
		return false
	}
	last := totals(state)
	points := []any{point(startedAt, last, "start")}
	consistency := []any{}
	if last["scope"] != numberOf(snap["totalEstimate"]) || last["unknown"] != numberOf(snap["missingEstimates"]) {
		consistency = append(consistency, "start")
	}
	until := now
	if t, ok := sprint["completedAt"].(time.Time); ok {
		until = t
	}
	for _, r := range replayed {
		at := r["createdAt"].(time.Time)
		if at.After(until) {
			break
		}
		touched := false
		for _, ch := range scopeChangesOf(r) {
			if st := state[ch.card]; st != nil {
				scopeSet(st, ch.field, ch.after)
				touched = true
			}
		}
		if !touched {
			continue
		}
		if next := totals(state); differs(next, last) {
			points = append(points, point(at, next, r["group"]))
			last = next
		}
	}
	if closeSnap, _ := sprint["closeSnapshot"].(map[string]any); closeSnap != nil {
		var scope, completed float64
		for _, c := range snapshotCards(closeSnap) {
			if c["archived"] == true || c["estimate"] == nil {
				continue
			}
			scope += numberOf(c["estimate"])
			if c["done"] == true {
				completed += numberOf(c["estimate"])
			}
		}
		if math.Abs(last["scope"]-scope) > 0 || last["completed"] != completed {
			consistency = append(consistency, "close")
		}
	}
	if _, closed := sprint["completedAt"].(time.Time); !closed && differs(totals(current), last) {
		consistency = append(consistency, "now")
	}
	if truncated {
		consistency = append(consistency, "truncated")
	}
	return map[string]any{"points": points, "truncated": truncated, "consistent": len(consistency) == 0, "inconsistentAt": consistency,
		"unit": snap["unit"], "from": startedAt, "until": until}
}

// scrumScopeHistory is getScrumScopeHistory.
func (s *Server) scrumScopeHistory(ctx context.Context, u *User, boardID, sprintID string) (map[string]any, error) {
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
	startedAt, ok := sprint["startedAt"].(time.Time)
	if sprint["startSnapshot"] == nil || !ok {
		return map[string]any{"points": []any{}, "partial": partial, "sprintName": sprint["name"]}, nil
	}
	// The snapshots' rows, wherever they are kept.
	for _, key := range []string{"startSnapshot", "closeSnapshot"} {
		if snap, _ := sprint[key].(map[string]any); snap != nil {
			rows, err := s.scrumSnapshotRows(ctx, sprint, key)
			if err != nil {
				return nil, err
			}
			full := map[string]any{}
			for k, v := range snap {
				full[k] = v
			}
			full["cards"] = rowsAsAny(rows)
			sprint[key] = full
		}
	}
	since := bson.M{"$gte": startedAt}
	ids := map[string]bool{}
	for _, r := range snapshotCards(sprint["startSnapshot"]) {
		ids[docString(r, "cardId")] = true
	}
	for _, c := range s.findDocs(ctx, "cards", bson.M{"boardId": eq(boardID), "scrum.sprintId": eq(sprintID)}, options.Find().SetProjection(bson.M{"_id": 1})) {
		ids[docString(c, "_id")] = true
	}
	scrumRows := s.findDocs(ctx, "changeHistory", bson.M{"boardId": eq(boardID), "group": "scrum", "createdAt": since},
		options.Find().SetSort(bson.D{{Key: "createdAt", Value: 1}}).SetLimit(maxScopeReplayRows+1))
	for _, r := range scrumRows {
		for _, ch := range scopeChangesOf(r) {
			if ch.before == sprintID || ch.after == sprintID {
				ids[ch.card] = true
			}
		}
	}
	var visible map[string]bool
	if partial {
		visible = map[string]bool{}
		for _, c := range s.findDocs(ctx, "cards", scrumCardFilter(b, u), options.Find().SetProjection(bson.M{"_id": 1})) {
			visible[docString(c, "_id")] = true
		}
	}
	cardIDs := bson.A{}
	for id := range ids {
		if visible == nil || visible[id] {
			cardIDs = append(cardIDs, id)
		}
	}
	cards := s.findDocs(ctx, "cards", bson.M{"_id": dbIn(cardIDs)}, options.Find().SetProjection(bson.M{"scrum": 1, "customFields": 1, "poker": 1,
		"dueComplete": 1, "listId": 1, "archived": 1}))
	fieldRows := s.findDocs(ctx, "changeHistory", bson.M{"boardId": eq(boardID), "entityType": "card", "entityId": dbIn(cardIDs),
		"group": bson.M{"$in": bson.A{"customFields", "dates", "position", "lifecycle"}}, "createdAt": since},
		options.Find().SetSort(bson.D{{Key: "createdAt", Value: 1}}).SetLimit(maxScopeReplayRows+1))
	done := map[string]bool{}
	for _, l := range s.findDocs(ctx, "lists", bson.M{"boardId": eq(boardID), "scrum.category": "done"}, options.Find().SetProjection(bson.M{"_id": 1})) {
		done[docString(l, "_id")] = true
	}
	result := replaySprintScope(sprint, cards, append(append([]map[string]any{}, scrumRows...), fieldRows...), done, time.Now().UTC())
	if result == nil {
		return map[string]any{"points": []any{}, "partial": partial, "sprintName": sprint["name"]}, nil
	}
	result["truncated"] = result["truncated"] == true || len(scrumRows) > maxScopeReplayRows || len(fieldRows) > maxScopeReplayRows
	result["partial"], result["sprintName"] = partial, sprint["name"]
	return result, nil
}

func (s *Server) methodScrumScopeHistory(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	boardID, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	sprintID, err := stringArg(args, 1)
	if err != nil {
		return nil, err
	}
	return s.scrumScopeHistory(r.Context(), u, boardID, sprintID)
}
