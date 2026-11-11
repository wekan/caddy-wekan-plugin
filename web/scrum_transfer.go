package web

// The native Scrum transfer, as WeKan's models/lib/scrumTransfer.js and
// server/lib/scrumTransferExport.js: a board export's scrumTransfer section
// (format wekan-scrum-2) - the settings, sprints with their snapshots whole,
// releases, events, daily observations and the Scrum metadata of the
// exported cards, lists and swimlanes - and scrumTransferLosses, what a
// scoped or selective export left out. normalizeScrumTransfer is the
// contract both directions check; remapScrumTransfer puts a file's records
// on a new board's ids.

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const scrumTransferFormat = "wekan-scrum-2"

func tfail(msg string) { sfail("Invalid Scrum transfer: " + msg) }

// jsonValue is a document as JSON reads it: numbers as float64, dates as
// ISO text - the shape the transfer's checks are written for.
func jsonValue(v any) any {
	raw, err := json.Marshal(plain(v))
	if err != nil {
		return nil
	}
	var out any
	_ = json.Unmarshal(raw, &out)
	return out
}

func tObject(v any, keys ...string) map[string]any {
	m, ok := v.(map[string]any)
	if !ok || m == nil {
		tfail("expected a plain object")
	}
	for k := range m {
		if !containsString(keys, k) {
			tfail("unsupported field " + k)
		}
	}
	return m
}

func tID(v any) string {
	s, ok := v.(string)
	if !ok || s == "" || len(s) > 200 || strings.ContainsRune(s, 0) {
		tfail("invalid identifier")
	}
	return s
}

func tRows(v any, max int) []any {
	list, ok := v.([]any)
	if !ok || max > 0 && len(list) > max {
		tfail("invalid record array")
	}
	return list
}

func tDate(v any) time.Time {
	t, _ := scrumDate(v).(time.Time)
	if t.IsZero() {
		tfail("missing timestamp")
	}
	return t
}

func has(m map[string]any, k string) bool { _, ok := m[k]; return ok }

// tSnapshot is snapshot(): a sprint's or an observation's rows and header.
func tSnapshot(v any) map[string]any {
	m := tObject(v, "at", "unit", "estimateSource", "estimateCustomFieldId", "completionPolicy", "workingDays", "cards", "missingEstimates", "totalEstimate", "partial")
	settings := normalizeScrumSettings(map[string]any{"estimateUnit": m["unit"], "estimateSource": m["estimateSource"],
		"estimateCustomFieldId": m["estimateCustomFieldId"], "completionPolicy": m["completionPolicy"]})
	if settings["estimateSource"] == "customField" && settings["estimateCustomFieldId"] == nil {
		tfail("snapshot estimate field is required")
	}
	seen := map[string]bool{}
	cards := []any{}
	missing, total := 0.0, 0.0
	for _, x := range tRows(m["cards"], 0) {
		r := tObject(x, "cardId", "listId", "estimate", "done", "archived")
		id := tID(r["cardId"])
		tID(r["listId"])
		if seen[id] {
			tfail("duplicate snapshot card")
		}
		seen[id] = true
		if r["estimate"] != nil {
			f, ok := r["estimate"].(float64)
			if !ok || f < 0 || f > 1e12 || f != f {
				tfail("invalid snapshot estimate")
			}
			total += f
		} else {
			missing++
		}
		_, d := r["done"].(bool)
		_, a := r["archived"].(bool)
		if !d || !a {
			tfail("invalid snapshot status")
		}
		row := map[string]any{}
		for k, x := range r {
			row[k] = x
		}
		if f, ok := row["estimate"].(float64); ok {
			row["estimate"] = storedNum(f)
		}
		cards = append(cards, row)
	}
	if numberOf(m["missingEstimates"]) != missing || numberOf(m["totalEstimate"]) != total {
		tfail("inconsistent snapshot totals")
	}
	if p, ok := m["partial"]; ok {
		if _, isBool := p.(bool); !isBool {
			tfail("invalid partial flag")
		}
	}
	out := map[string]any{"at": tDate(m["at"]), "unit": settings["estimateUnit"], "estimateSource": settings["estimateSource"],
		"estimateCustomFieldId": settings["estimateCustomFieldId"], "completionPolicy": settings["completionPolicy"],
		"cards": cards, "missingEstimates": storedNum(missing), "totalEstimate": storedNum(total)}
	if has(m, "workingDays") {
		out["workingDays"] = normalizeScrumSettings(map[string]any{"workingDays": m["workingDays"]})["workingDays"]
	}
	if m["partial"] == true {
		out["partial"] = true
	}
	return out
}

var sprintLifecycle = []string{"state", "startedAt", "completedAt", "cancelledAt", "cancellationReason", "rolloverSprintId", "startSnapshot", "closeSnapshot"}
var recordAudit = []string{"createdAt", "createdBy", "updatedAt", "updatedBy"}

// tRecord is record(kind, value).
func tRecord(kind string, v any) map[string]any {
	src, ok := v.(map[string]any)
	if !ok {
		tfail("invalid planning record")
	}
	fields := map[string]any{}
	for k, x := range src {
		if k != "_id" && k != "incarnation" {
			fields[k] = x
		}
	}
	id := tID(src["_id"])
	extras := map[string]any{}
	keys := append([]string{}, recordAudit...)
	if kind == "sprint" {
		keys = append(keys, sprintLifecycle...)
	}
	for _, k := range keys {
		if x, ok := fields[k]; ok {
			extras[k] = x
			delete(fields, k)
		}
	}
	out := normalizeScrumRecord(kind, fields)
	out["_id"] = id
	if name, _ := out["name"].(string); kind != "event" && name == "" {
		tfail("record name is required")
	}
	for _, k := range recordAudit {
		if x, ok := extras[k]; ok {
			if strings.HasSuffix(k, "At") {
				out[k] = tDate(x)
			} else {
				out[k] = tID(x)
			}
		}
	}
	if kind == "event" && (out["sprintId"] == nil || out["kind"] == nil || out["startsAt"] == nil) {
		tfail("event requires sprint, kind and timestamp")
	}
	if kind != "sprint" {
		return out
	}
	st, _ := extras["state"].(string)
	if !containsString([]string{"planned", "active", "closed", "cancelled"}, st) {
		tfail("invalid sprint state")
	}
	out["state"] = st
	for _, k := range []string{"startedAt", "completedAt", "cancelledAt"} {
		if x, ok := extras[k]; ok {
			out[k] = tDate(x)
		}
	}
	if x, ok := extras["cancellationReason"]; ok {
		out["cancellationReason"] = normalizeScrumRecord("sprint", map[string]any{"goal": x})["goal"]
	}
	if x, ok := extras["rolloverSprintId"]; ok {
		if x == nil {
			out["rolloverSprintId"] = nil
		} else {
			out["rolloverSprintId"] = tID(x)
		}
	}
	for _, k := range []string{"startSnapshot", "closeSnapshot"} {
		if x, ok := extras[k]; ok {
			out[k] = tSnapshot(x)
		}
	}
	start, _ := out["startSnapshot"].(map[string]any)
	end, _ := out["closeSnapshot"].(map[string]any)
	startedAt, hasStarted := out["startedAt"].(time.Time)
	completedAt, hasCompleted := out["completedAt"].(time.Time)
	cancelledAt, hasCancelled := out["cancelledAt"].(time.Time)
	if (st == "active" || st == "closed") && (start == nil || !hasStarted) {
		tfail("started sprint requires a start snapshot")
	}
	if st == "closed" && (end == nil || !hasCompleted) {
		tfail("closed sprint requires a close snapshot")
	}
	reason, _ := out["cancellationReason"].(string)
	if (start != nil) != hasStarted || st == "planned" && hasStarted ||
		st != "closed" && (end != nil || hasCompleted || out["rolloverSprintId"] != nil) ||
		st != "cancelled" && (hasCancelled || reason != "") {
		tfail("inconsistent sprint lifecycle")
	}
	if start != nil && !start["at"].(time.Time).Equal(startedAt) {
		tfail("inconsistent startSnapshot snapshot timestamp")
	}
	if end != nil && !end["at"].(time.Time).Equal(completedAt) {
		tfail("inconsistent closeSnapshot snapshot timestamp")
	}
	if start != nil && end != nil {
		for _, k := range []string{"unit", "estimateSource", "estimateCustomFieldId", "completionPolicy"} {
			if canonical(start[k]) != canonical(end[k]) {
				tfail("incompatible sprint snapshot " + k)
			}
		}
	}
	if st == "cancelled" && (!hasCancelled || jsTrim(reason) == "") {
		tfail("cancelled sprint requires timestamp and reason")
	}
	if hasCompleted && startedAt.After(completedAt) {
		tfail("sprint completed before it started")
	}
	if hasCancelled && startedAt.After(cancelledAt) {
		tfail("sprint cancelled before it started")
	}
	return out
}

func tUnique(list []map[string]any) []map[string]any {
	seen := map[string]bool{}
	for _, r := range list {
		id := docString(r, "_id")
		if seen[id] {
			tfail("duplicate record identifier")
		}
		seen[id] = true
	}
	return list
}

// normalizeScrumTransfer is WeKan's: v as JSON reads it (jsonValue).
func normalizeScrumTransfer(v any) map[string]any {
	m := tObject(v, "format", "settings", "sprints", "releases", "events", "cards", "lists", "swimlanes", "dailyObservations")
	format, _ := m["format"].(string)
	if format != scrumTransferFormat && format != "wekan-scrum-1" {
		tfail("unsupported version")
	}
	if format == "wekan-scrum-1" && has(m, "dailyObservations") {
		tfail("daily observations require version 2")
	}
	settings := normalizeScrumSettings(m["settings"])
	if settings["estimateSource"] == "customField" && settings["estimateCustomFieldId"] == nil {
		tfail("estimate field is required")
	}
	out := map[string]any{"format": scrumTransferFormat, "settings": settings}
	byKind := map[string][]map[string]any{}
	for _, p := range [][2]string{{"sprints", "sprint"}, {"releases", "release"}, {"events", "event"}} {
		list := []map[string]any{}
		for _, x := range tRows(m[p[0]], 10000) {
			list = append(list, tRecord(p[1], x))
		}
		byKind[p[0]] = tUnique(list)
	}
	for _, p := range [][2]string{{"cards", "card"}, {"lists", "list"}, {"swimlanes", "swimlane"}} {
		list := []map[string]any{}
		for _, x := range tRows(m[p[0]], 0) {
			r := tObject(x, "_id", "scrum")
			sc := normalizeScrumMetadata(p[1], r["scrum"])
			if p[1] == "card" {
				sc = portableCardReleases(sc)
			}
			list = append(list, map[string]any{"_id": tID(r["_id"]), "scrum": sc})
		}
		byKind[p[0]] = tUnique(list)
	}
	sprintIDs, sprintByID := map[string]bool{}, map[string]map[string]any{}
	for _, sp := range byKind["sprints"] {
		sprintIDs[docString(sp, "_id")], sprintByID[docString(sp, "_id")] = true, sp
	}
	seenObs := map[string]bool{}
	obs := []map[string]any{}
	var rawObs any = []any{}
	if has(m, "dailyObservations") {
		rawObs = m["dailyObservations"]
	}
	for _, x := range tRows(rawObs, 10000) {
		r := tObject(x, "sprintId", "startedAt", "day", "capturedAt", "snapshot", "consistency")
		sid := tID(r["sprintId"])
		if !sprintIDs[sid] {
			tfail("foreign daily observation sprint")
		}
		startedAt, capturedAt := tDate(r["startedAt"]), tDate(r["capturedAt"])
		snap := tSnapshot(r["snapshot"])
		if cur, _ := sprintByID[sid]["startSnapshot"].(map[string]any); cur != nil && cur["at"].(time.Time).Equal(startedAt) {
			for _, k := range []string{"unit", "estimateSource", "estimateCustomFieldId", "completionPolicy"} {
				if canonical(snap[k]) != canonical(cur[k]) {
					tfail("incompatible daily observation " + k)
				}
			}
		}
		day, _ := r["day"].(string)
		if r["consistency"] != "observed" || startedAt.After(capturedAt) || !snap["at"].(time.Time).Equal(capturedAt) || day != capturedAt.Format("2006-01-02") {
			tfail("inconsistent daily observation")
		}
		key := sid + "|" + startedAt.Format(time.RFC3339Nano) + "|" + day
		if seenObs[key] {
			tfail("duplicate daily observation")
		}
		seenObs[key] = true
		obs = append(obs, map[string]any{"sprintId": sid, "startedAt": startedAt, "day": day, "capturedAt": capturedAt, "snapshot": snap, "consistency": "observed"})
	}
	releaseIDs := map[string]bool{}
	for _, r := range byKind["releases"] {
		releaseIDs[docString(r, "_id")] = true
	}
	present := func(ids map[string]bool, v any) {
		if s, ok := v.(string); ok && !ids[s] {
			tfail("foreign planning reference " + s)
		}
	}
	refs := append([]map[string]any{}, byKind["events"]...)
	for _, p := range []string{"cards", "swimlanes"} {
		for _, r := range byKind[p] {
			refs = append(refs, r["scrum"].(map[string]any))
		}
	}
	for _, r := range refs {
		present(sprintIDs, r["sprintId"])
		for _, id := range cardReleaseIds(r) {
			present(releaseIDs, id)
		}
		for _, id := range stringsOf(r["pastSprintIds"]) {
			present(sprintIDs, id)
		}
	}
	for _, sp := range byKind["sprints"] {
		seen := map[string]bool{docString(sp, "_id"): true}
		for target := docString(sp, "rolloverSprintId"); target != ""; target = docString(sprintByID[target], "rolloverSprintId") {
			present(sprintIDs, target)
			if seen[target] {
				tfail("cyclic sprint rollover")
			}
			seen[target] = true
		}
	}
	for k, list := range byKind {
		out[k] = list
	}
	out["dailyObservations"] = obs
	return out
}

// portableCardReleases is WeKan's: one release as releaseId alone, several
// as both fields.
func portableCardReleases(sc map[string]any) map[string]any {
	if !hasReleaseFields(sc) {
		return sc
	}
	list := cardReleaseIds(sc)
	out := map[string]any{}
	for k, v := range sc {
		if k != "releaseId" && k != "releaseIds" {
			out[k] = v
		}
	}
	if len(list) > 0 {
		out["releaseId"] = list[0]
	}
	if len(list) > 1 {
		ids := []any{}
		for _, id := range list {
			ids = append(ids, id)
		}
		out["releaseIds"] = ids
	}
	return out
}

// exportScrumTransfer is WeKan's: called after the export route has
// authorized the board and its scope. nil when the board has nothing of
// Scrum.
func (s *Server) exportScrumTransfer(ctx context.Context, boardID string, cardIDs, listIDs, laneIDs []string, scoped, withFields bool) (map[string]any, []any, error) {
	var board bson.M
	if s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(boardID)}).Decode(&board) != nil {
		return nil, nil, &methodError{"not-found", "Board no longer exists"}
	}
	meta := options.Find().SetProjection(bson.M{"scrum": 1})
	withScrum := bson.M{"$exists": true}
	cards := s.findDocs(ctx, "cards", bson.M{"boardId": eq(boardID), "_id": dbIn(stringsA(cardIDs)), "scrum": withScrum}, meta)
	lists := s.findDocs(ctx, "lists", bson.M{"boardId": eq(boardID), "_id": dbIn(stringsA(listIDs)), "scrum": withScrum}, meta)
	lanes := s.findDocs(ctx, "swimlanes", bson.M{"boardId": eq(boardID), "_id": dbIn(stringsA(laneIDs)), "scrum": withScrum}, meta)
	sprints := s.findDocs(ctx, "scrumSprints", bson.M{"boardId": eq(boardID)})
	releases := s.findDocs(ctx, "scrumReleases", bson.M{"boardId": eq(boardID)})
	events := s.findDocs(ctx, "scrumEvents", bson.M{"boardId": eq(boardID)})
	pendingOp := &methodError{"scrum-pending", "Finish the pending Scrum operation before exporting"}
	if s.db.Collection("scrumImportPending").FindOne(ctx, bson.M{"_id": eq(boardID)}).Err() == nil {
		return nil, nil, pendingOp
	}
	if board["scrum"] == nil && len(cards)+len(lists)+len(lanes)+len(sprints)+len(releases)+len(events) == 0 {
		return nil, nil, nil
	}
	if s.db.Collection("scrumHistoryPending").FindOne(ctx, bson.M{"_id": eq(boardID)}).Err() == nil {
		return nil, nil, pendingOp
	}
	for i, sp := range sprints {
		if sp["scrumImportPending"] == true || sp["rolloverPending"] == true || len(asAnyList(sp["rolloverPending"])) > 0 {
			return nil, nil, pendingOp
		}
		for _, key := range []string{"startSnapshot", "closeSnapshot"} {
			if snap, _ := sp[key].(map[string]any); snap != nil {
				rows, err := s.scrumSnapshotRows(ctx, sp, key)
				if err != nil {
					return nil, nil, err
				}
				h := map[string]any{}
				for k, v := range snap {
					if k != "stored" && k != "chunks" && k != "rowCount" {
						h[k] = v
					}
				}
				h["cards"] = rowsAsAny(rows)
				sprints[i][key] = h
			}
		}
	}
	sprintIDs, releaseIDs := map[string]bool{}, map[string]bool{}
	for _, r := range append(append([]map[string]any{}, cards...), lanes...) {
		sc, _ := r["scrum"].(map[string]any)
		if id := docString(sc, "sprintId"); id != "" {
			sprintIDs[id] = true
		}
		for _, id := range stringsOf(sc["pastSprintIds"]) {
			sprintIDs[id] = true
		}
		for _, id := range cardReleaseIds(sc) {
			releaseIDs[id] = true
		}
	}
	// Rollover destinations stay structural references, scoped or not.
	for changed := true; changed; {
		changed = false
		for _, sp := range sprints {
			if t := docString(sp, "rolloverSprintId"); sprintIDs[docString(sp, "_id")] && t != "" && !sprintIDs[t] {
				sprintIDs[t], changed = true, true
			}
		}
	}
	visibleCards, visibleLists := map[string]bool{}, map[string]bool{}
	for _, id := range cardIDs {
		visibleCards[id] = true
	}
	for _, id := range listIDs {
		visibleLists[id] = true
	}
	losses := []any{}
	clean := func(r map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range r {
			switch k {
			case "boardId", "revision", "rolloverPending", "closedFromRevision", "scrumImportPending", "reportTotals",
				"rolloverTotal", "rolloverDone", "rolloverError", "rolloverLease":
			default:
				out[k] = v
			}
		}
		return out
	}
	metaRows := func(rows []map[string]any, card bool) []any {
		out := []any{}
		for _, r := range rows {
			sc, _ := r["scrum"].(map[string]any)
			if card {
				sc = portableCardReleases(sc)
			}
			out = append(out, map[string]any{"_id": r["_id"], "scrum": sc})
		}
		return out
	}
	settings := plain(board["scrum"])
	if settings == nil {
		settings = map[string]any{}
	}
	transfer := map[string]any{"format": scrumTransferFormat, "settings": settings, "cards": metaRows(cards, true), "lists": metaRows(lists, false), "swimlanes": metaRows(lanes, false)}
	keep := func(list []map[string]any, ok func(map[string]any) bool) []map[string]any {
		out := []map[string]any{}
		for _, r := range list {
			if !scoped || ok(r) {
				out = append(out, clean(r))
			}
		}
		return out
	}
	tSprints := keep(sprints, func(r map[string]any) bool { return sprintIDs[docString(r, "_id")] })
	tReleases := keep(releases, func(r map[string]any) bool { return releaseIDs[docString(r, "_id")] })
	tEvents := keep(events, func(r map[string]any) bool { return sprintIDs[docString(r, "sprintId")] })
	filter := func(snap map[string]any, path string) {
		kept := []any{}
		missing, total := 0, 0.0
		for _, c := range snapshotCards(snap) {
			if visibleCards[docString(c, "cardId")] && visibleLists[docString(c, "listId")] {
				kept = append(kept, c)
				if c["estimate"] == nil {
					missing++
				} else {
					total += numberOf(c["estimate"])
				}
				continue
			}
			losses = append(losses, map[string]any{"path": path, "sourceId": c["cardId"], "reason": "card-or-list-not-exported"})
			snap["partial"] = true
		}
		snap["cards"], snap["missingEstimates"], snap["totalEstimate"] = kept, storedNum(float64(missing)), storedNum(total)
	}
	selected := bson.A{}
	for _, sp := range tSprints {
		for _, key := range []string{"startSnapshot", "closeSnapshot"} {
			if snap, _ := sp[key].(map[string]any); snap != nil {
				filter(snap, lossPath("sprints", docString(sp, "_id"), key))
			}
		}
		selected = append(selected, sp["_id"])
	}
	obs := []any{}
	for _, o := range s.findDocs(ctx, "scrumDailySnapshots", bson.M{"boardId": eq(boardID), "sprintId": dbIn(selected)},
		options.Find().SetSort(bson.D{{Key: "capturedAt", Value: 1}, {Key: "_id", Value: 1}}).SetLimit(10001)) {
		if len(obs) >= 10000 {
			return nil, nil, &methodError{"scrum-export", "Daily Scrum history exceeds the native transfer limit"}
		}
		snap, _ := o["snapshot"].(map[string]any)
		h := map[string]any{}
		for k, v := range snap {
			if k != "stored" && k != "chunks" && k != "rowCount" {
				h[k] = v
			}
		}
		if snap["stored"] == "rows" {
			rows, err := s.scrumSnapshotRows(ctx, map[string]any{"_id": o["sprintId"], "daily": snap}, "daily")
			if err != nil {
				return nil, nil, err
			}
			h["cards"] = rowsAsAny(rows)
		}
		filter(h, lossPath("dailyObservations", docString(o, "sprintId"), docString(o, "day")))
		obs = append(obs, map[string]any{"sprintId": o["sprintId"], "startedAt": o["startedAt"], "day": o["day"], "capturedAt": o["capturedAt"],
			"snapshot": h, "consistency": o["consistency"]})
	}
	for _, e := range tEvents {
		kept := []any{}
		for _, id := range stringsOf(e["followUpCardIds"]) {
			if visibleCards[id] {
				kept = append(kept, id)
			} else {
				losses = append(losses, map[string]any{"path": lossPath("events", docString(e, "_id"), "followUpCardIds"), "sourceId": id, "reason": "card-not-exported"})
			}
		}
		e["followUpCardIds"] = kept
	}
	transfer["sprints"], transfer["releases"], transfer["events"], transfer["dailyObservations"] = tSprints, tReleases, tEvents, obs
	if !withFields {
		seen := map[string]bool{}
		refs := []any{docString(settings.(map[string]any), "estimateCustomFieldId")}
		for _, sp := range tSprints {
			for _, key := range []string{"startSnapshot", "closeSnapshot"} {
				if snap, _ := sp[key].(map[string]any); snap != nil {
					refs = append(refs, docString(snap, "estimateCustomFieldId"))
				}
			}
		}
		for _, o := range obs {
			refs = append(refs, docString(o.(map[string]any)["snapshot"].(map[string]any), "estimateCustomFieldId"))
		}
		for _, r := range refs {
			if id, _ := r.(string); id != "" && !seen[id] {
				seen[id] = true
				losses = append(losses, map[string]any{"path": "customFields", "sourceId": id, "reason": "estimate-field-not-exported"})
			}
		}
	}
	var out map[string]any
	if err := scrumValidate(func() { out = normalizeScrumTransfer(jsonValue(transfer)) }); err != nil {
		return nil, nil, err
	}
	return out, losses, nil
}

// lossPath is a loss's path in the file ("events.<id>.followUpCardIds"):
// text the import report shows, never a database path.
func lossPath(parts ...string) string { return strings.Join(parts, ".") }

func stringsA(ids []string) bson.A {
	a := bson.A{}
	for _, id := range ids {
		a = append(a, id)
	}
	return a
}

// remapScrumTransfer is WeKan's: the transfer's records on the destination's
// ids. maps are the importer's own, never the file's; a missing structural
// reference is an error, an omitted card or actor a loss.
func remapScrumTransfer(v map[string]any, maps map[string]map[string]string) (map[string]any, []any) {
	t := normalizeScrumTransfer(jsonValue(v))
	losses := []any{}
	ref := func(kind string, src any, path string, optional bool) any {
		s, ok := src.(string)
		if src == nil || !ok {
			return nil
		}
		m, ok := maps[kind]
		if !ok {
			tfail("missing " + kind + " ID map")
		}
		if target, ok := m[s]; ok {
			return tID(target)
		}
		if !optional {
			tfail("unmapped " + path + ": " + s)
		}
		losses = append(losses, map[string]any{"path": path, "sourceId": s, "reason": "not-transferred"})
		return nil
	}
	metadata := func(meta map[string]any, path string) {
		for _, f := range [][2]string{{"sprintId", "sprints"}, {"releaseId", "releases"}} {
			if has(meta, f[0]) {
				meta[f[0]] = ref(f[1], meta[f[0]], lossPath(path, f[0]), false)
			}
		}
		for _, f := range [][2]string{{"releaseIds", "releases"}, {"pastSprintIds", "sprints"}} {
			if list, ok := meta[f[0]].([]any); ok {
				out := []any{}
				for _, x := range list {
					out = append(out, ref(f[1], x, lossPath(path, f[0]), false))
				}
				meta[f[0]] = out
			}
		}
	}
	remapSnap := func(snap map[string]any, path string) {
		before := len(snapshotCards(snap))
		snap["estimateCustomFieldId"] = ref("customFields", snap["estimateCustomFieldId"], lossPath(path, "estimateCustomFieldId"), false)
		kept := []any{}
		missing, total := 0.0, 0.0
		for _, c := range snapshotCards(snap) {
			id := ref("cards", c["cardId"], lossPath(path, "cards"), true)
			if id == nil {
				continue
			}
			row := map[string]any{}
			for k, x := range c {
				row[k] = x
			}
			row["cardId"], row["listId"] = id, ref("lists", c["listId"], lossPath(path, "listId"), false)
			if row["estimate"] == nil {
				missing++
			} else {
				total += numberOf(row["estimate"])
			}
			kept = append(kept, row)
		}
		if len(kept) != before {
			snap["partial"] = true
		}
		snap["cards"], snap["missingEstimates"], snap["totalEstimate"] = kept, storedNum(missing), storedNum(total)
	}
	settings := t["settings"].(map[string]any)
	for _, k := range []string{"productOwnerId", "scrumMasterId"} {
		if has(settings, k) {
			settings[k] = ref("users", settings[k], lossPath("settings", k), true)
		}
	}
	if list, ok := settings["developerIds"].([]any); ok {
		out, seen := []any{}, map[string]bool{}
		for _, x := range list {
			if id, ok := ref("users", x, "settings.developerIds", true).(string); ok && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
		settings["developerIds"] = out
	}
	if has(settings, "estimateCustomFieldId") {
		settings["estimateCustomFieldId"] = ref("customFields", settings["estimateCustomFieldId"], "settings.estimateCustomFieldId", false)
	}
	for _, plural := range []string{"sprints", "releases", "events"} {
		for _, row := range t[plural].([]map[string]any) {
			path := lossPath(plural, docString(row, "_id"))
			row["_id"] = ref(plural, row["_id"], lossPath(path, "_id"), false)
			for _, k := range []string{"createdBy", "updatedBy"} {
				if has(row, k) {
					if actor := ref("users", row[k], lossPath(path, k), true); actor != nil {
						row[k] = actor
					} else {
						delete(row, k)
					}
				}
			}
			metadata(row, path)
			if has(row, "rolloverSprintId") {
				row["rolloverSprintId"] = ref("sprints", row["rolloverSprintId"], lossPath(path, "rolloverSprintId"), false)
			}
			if list, ok := row["followUpCardIds"].([]any); ok {
				out, seen := []any{}, map[string]bool{}
				for _, x := range list {
					if id, ok := ref("cards", x, lossPath(path, "followUpCardIds"), true).(string); ok && !seen[id] {
						seen[id] = true
						out = append(out, id)
					}
				}
				row["followUpCardIds"] = out
			}
			for _, k := range []string{"startSnapshot", "closeSnapshot"} {
				if snap, _ := row[k].(map[string]any); snap != nil {
					remapSnap(snap, lossPath(path, k))
				}
			}
		}
	}
	for _, o := range t["dailyObservations"].([]map[string]any) {
		path := lossPath("dailyObservations", docString(o, "sprintId"), docString(o, "day"))
		o["sprintId"] = ref("sprints", o["sprintId"], lossPath(path, "sprintId"), false)
		remapSnap(o["snapshot"].(map[string]any), lossPath(path, "snapshot"))
	}
	for _, plural := range []string{"cards", "lists", "swimlanes"} {
		out := []map[string]any{}
		for _, row := range t[plural].([]map[string]any) {
			target := ref(plural, row["_id"], lossPath(plural, docString(row, "_id")), true)
			if target == nil {
				continue
			}
			metadata(row["scrum"].(map[string]any), lossPath(plural, docString(row, "_id"), "scrum"))
			out = append(out, map[string]any{"_id": target, "scrum": row["scrum"]})
		}
		t[plural] = out
	}
	// Checked again: the importer's maps may collide even where the file's
	// ids did not.
	return normalizeScrumTransfer(jsonValue(t)), losses
}

var scrumLossReasons = []string{"not-transferred", "card-or-list-not-exported", "card-not-exported", "estimate-field-not-exported",
	"card-not-matched", "card-ambiguous", "card-on-another-board", "record-ambiguous", "record-not-imported", "sprint-finished"}

// normalizeScrumTransferLosses is WeKan's.
func normalizeScrumTransferLosses(v any) []any {
	if v == nil {
		return []any{}
	}
	out := []any{}
	for _, x := range tRows(v, 10000) {
		r := tObject(x, "path", "sourceId", "reason")
		path, _ := r["path"].(string)
		if path == "" || len(path) > 1000 {
			tfail("invalid loss path")
		}
		reason, _ := r["reason"].(string)
		if !containsString(scrumLossReasons, reason) {
			tfail("invalid loss reason")
		}
		out = append(out, map[string]any{"path": path, "sourceId": tID(r["sourceId"]), "reason": reason})
	}
	return out
}

func sourceIDs(export map[string]any, key string) map[string]string {
	out := map[string]string{}
	list, _ := jsonValue(export[key]).([]any)
	for _, x := range list {
		m, _ := x.(map[string]any)
		id := docString(m, "_id")
		if id == "" {
			id = docString(m, "id")
		}
		if id != "" {
			out[id] = id
		}
	}
	return out
}

// validateScrumImport is WeKan's: before the import writes anything, the
// file's transfer must hold together with the file itself - every record
// it names in the file, every estimate field a numeric custom field of it.
func validateScrumImport(export map[string]any) error {
	raw, ok := export["scrumTransfer"]
	if !ok || raw == nil {
		return nil
	}
	err := scrumValidate(func() {
		t := normalizeScrumTransfer(jsonValue(raw))
		normalizeScrumTransferLosses(jsonValue(export["scrumTransferLosses"]))
		maps := map[string]map[string]string{}
		for _, k := range []string{"sprints", "releases", "events"} {
			maps[k] = map[string]string{}
			for _, r := range t[k].([]map[string]any) {
				maps[k][docString(r, "_id")] = docString(r, "_id")
			}
		}
		for _, k := range []string{"cards", "lists", "swimlanes", "customFields", "users"} {
			maps[k] = sourceIDs(export, k)
		}
		remapScrumTransfer(t, maps)
		numeric := map[string]bool{}
		fields, _ := jsonValue(export["customFields"]).([]any)
		for _, x := range fields {
			if m, _ := x.(map[string]any); m != nil && m["type"] == "number" {
				numeric[docString(m, "_id")] = true
			}
		}
		refs := []string{docString(t["settings"].(map[string]any), "estimateCustomFieldId")}
		for _, sp := range t["sprints"].([]map[string]any) {
			for _, k := range []string{"startSnapshot", "closeSnapshot"} {
				if snap, _ := sp[k].(map[string]any); snap != nil {
					refs = append(refs, docString(snap, "estimateCustomFieldId"))
				}
			}
		}
		for _, o := range t["dailyObservations"].([]map[string]any) {
			refs = append(refs, docString(o["snapshot"].(map[string]any), "estimateCustomFieldId"))
		}
		for _, id := range refs {
			if id != "" && !numeric[id] {
				sfail("Scrum estimates require a numeric custom field in the imported file")
			}
		}
	})
	if err != nil {
		return &methodError{"invalid-scrum-transfer", err.(*methodError).Reason}
	}
	return nil
}

// importScrumTransfer is WeKan's: the file's Scrum on the new board - new
// ids for its records, provenance naming the source, snapshot and
// observation rows in their chunks, the cards', lists' and swimlanes'
// metadata, the settings and the losses - its sprints marked as an
// unfinished import until the last write.
func (s *Server) importScrumTransfer(ctx context.Context, export map[string]any, boardID string, maps map[string]map[string]string) error {
	raw, ok := export["scrumTransfer"]
	if !ok || raw == nil {
		return nil
	}
	var t map[string]any
	var losses []any
	err := scrumValidate(func() {
		norm := normalizeScrumTransfer(jsonValue(raw))
		for _, k := range []string{"sprints", "releases", "events"} {
			maps[k] = map[string]string{}
			for _, r := range norm[k].([]map[string]any) {
				maps[k][docString(r, "_id")] = randomID()
			}
		}
		t, losses = remapScrumTransfer(norm, maps)
		losses = append(normalizeScrumTransferLosses(jsonValue(export["scrumTransferLosses"])), losses...)
	})
	if err != nil {
		return &methodError{"invalid-scrum-transfer", err.(*methodError).Reason}
	}
	sourceBoard, _ := export["_id"].(string)
	var sprintIDs bson.A
	for _, k := range []string{"sprints", "releases", "events"} {
		source := map[string]string{}
		for from, to := range maps[k] {
			source[to] = from
		}
		coll := map[string]string{"sprints": "scrumSprints", "releases": "scrumReleases", "events": "scrumEvents"}[k]
		for _, rec := range t[k].([]map[string]any) {
			id := docString(rec, "_id")
			doc := bson.M{}
			for f, v := range rec {
				doc[f] = v
			}
			if doc["provenance"] == nil {
				p := bson.M{"system": "wekan", "recordId": source[id]}
				if sourceBoard != "" && len(sourceBoard) <= 500 {
					p["projectId"] = sourceBoard
				}
				doc["provenance"] = p
			}
			if k == "sprints" {
				for _, f := range [][2]string{{"startSnapshot", "start"}, {"closeSnapshot", "close"}} {
					if snap, _ := rec[f[0]].(map[string]any); snap != nil {
						header, err := s.storeScrumSnapshot(ctx, boardID, id, f[1], snap)
						if err != nil {
							return err
						}
						doc[f[0]] = header
					}
				}
				if rec["startSnapshot"] != nil || rec["closeSnapshot"] != nil {
					doc["reportTotals"] = scrumReportTotals(snapshotCards(rec["startSnapshot"]), snapshotCards(rec["closeSnapshot"]))
				}
				doc["scrumImportPending"] = true
				sprintIDs = append(sprintIDs, id)
			}
			doc["boardId"], doc["revision"], doc["incarnation"] = boardID, int32(1), randomID()
			if _, err := s.db.Collection(coll).InsertOne(ctx, doc); err != nil {
				return err
			}
		}
	}
	for _, p := range [][2]string{{"cards", "cards"}, {"lists", "lists"}, {"swimlanes", "swimlanes"}} {
		for _, rec := range t[p[0]].([]map[string]any) {
			res, err := s.db.Collection(p[1]).UpdateOne(ctx, bson.M{"_id": eq(docString(rec, "_id")), "boardId": eq(boardID)},
				bson.M{"$set": bson.M{"scrum": rec["scrum"], "scrumRevision": int32(1)}})
			if err != nil {
				return err
			}
			if res.MatchedCount == 0 {
				return &methodError{"invalid-scrum-transfer", "An imported item no longer belongs to the destination board"}
			}
		}
	}
	for _, o := range t["dailyObservations"].([]map[string]any) {
		sid := docString(o, "sprintId")
		snap := o["snapshot"].(map[string]any)
		header, err := s.storeScrumSnapshot(ctx, boardID, sid, "daily", snap)
		if err != nil {
			return err
		}
		startedAt := o["startedAt"].(time.Time)
		if _, err := s.db.Collection("scrumDailySnapshots").InsertOne(ctx, bson.M{"_id": dailyObservationID(sid, startedAt, docString(o, "day")), // inserted, not a filter
			"boardId": boardID, "sprintId": sid, "startedAt": startedAt, "day": o["day"], "capturedAt": o["capturedAt"], "snapshot": header,
			"consistency": "observed"}); err != nil {
			return err
		}
	}
	if _, err := s.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": eq(boardID)}, bson.M{"$set": bson.M{"scrum": t["settings"],
		"scrumRevision": int32(1), "scrumImportLosses": losses}}); err != nil {
		return err
	}
	// The import is whole: its sprints are ordinary sprints from here.
	_, err = s.db.Collection("scrumSprints").UpdateMany(ctx, bson.M{"boardId": eq(boardID), "_id": dbIn(sprintIDs)}, bson.M{"$unset": bson.M{"scrumImportPending": ""}})
	return err
}
