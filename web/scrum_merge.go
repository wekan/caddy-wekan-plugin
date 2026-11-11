package web

// A native Scrum transfer imported INTO an existing board, as WeKan's
// models/lib/scrumTransferMerge.js and server/lib/scrumTransferMerge.js
// (scrum.importIntoBoard): sprints and releases matched - by id on this
// board, then provenance, then a name unique on both sides - and never
// duplicated; events by id or provenance; cards only matched, by id or by a
// board export's card number and title, never created and never another
// board's. A matched record is the board's own and never overwritten; the
// rest is created with the source's provenance. Matched cards get the
// file's sprint, releases, rank, issue type and acceptance criteria, never a
// move into a finished sprint. A dry run answers what would change; the same
// file twice changes nothing; the import is one Scrum History row.

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

var linkedCardTypes = map[string]bool{"cardType-linkedCard": true, "cardType-linkedBoard": true}

func mergeRows(v any, pick func(map[string]any) map[string]any) []map[string]any {
	if v == nil {
		return nil
	}
	// A file's list, or one this reduction made already (it runs twice, as
	// WeKan's runs in the browser and again on the server).
	list, ok := v.([]any)
	if done, isDone := v.([]map[string]any); isDone {
		for _, m := range done {
			list = append(list, m)
		}
		ok = true
	}
	if !ok {
		tfail("expected a list of rows")
	}
	out := []map[string]any{}
	for _, x := range list {
		m, _ := x.(map[string]any)
		if id := docString(m, "_id"); m != nil && id != "" && len(id) <= 200 {
			out = append(out, pick(m))
		}
	}
	return out
}

func truncText(v any, n int) (string, bool) {
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	r := []rune(s)
	if len(r) > n {
		r = r[:n]
	}
	return string(r), true
}

// scrumSafeInt is Number.isSafeInteger, of a file's number or a stored one.
func scrumSafeInt(v any) (float64, bool) {
	f := numberOf(v)
	return f, f == math.Trunc(f) && math.Abs(f) <= 9007199254740991
}

// scrumTransferFileFields is WeKan's: only what the import reads, of a board
// export or a bare transfer.
func scrumTransferFileFields(file any) map[string]any {
	f, ok := file.(map[string]any)
	if !ok {
		tfail("expected a JSON object")
	}
	if _, ok := f["format"].(string); ok {
		return map[string]any{"scrumTransfer": f, "scrumTransferLosses": []any{}, "cards": []map[string]any{}, "lists": []map[string]any{}, "customFields": []map[string]any{}}
	}
	if _, ok := f["scrumTransfer"].(map[string]any); !ok {
		tfail("this file has no Scrum planning")
	}
	out := map[string]any{"scrumTransfer": f["scrumTransfer"], "scrumTransferLosses": []any{}}
	if l, ok := f["scrumTransferLosses"].([]any); ok {
		out["scrumTransferLosses"] = l
	}
	if id, ok := f["_id"].(string); ok && len(id) <= 500 {
		out["_id"] = id
	}
	out["cards"] = mergeRows(f["cards"], func(r map[string]any) map[string]any {
		m := map[string]any{"_id": r["_id"]}
		if t, ok := truncText(r["title"], 10000); ok {
			m["title"] = t
		}
		if n, ok := scrumSafeInt(r["cardNumber"]); ok {
			m["cardNumber"] = n
		}
		return m
	})
	out["lists"] = mergeRows(f["lists"], func(r map[string]any) map[string]any {
		m := map[string]any{"_id": r["_id"]}
		if t, ok := truncText(r["title"], 10000); ok {
			m["title"] = t
		}
		return m
	})
	out["customFields"] = mergeRows(f["customFields"], func(r map[string]any) map[string]any {
		m := map[string]any{"_id": r["_id"]}
		if t, ok := truncText(r["name"], 10000); ok {
			m["name"] = t
		}
		if t, ok := truncText(r["type"], 100); ok {
			m["type"] = t
		}
		return m
	})
	return out
}

func sourceProvenance(rec map[string]any, sourceBoard string) map[string]any {
	if p, ok := rec["provenance"].(map[string]any); ok {
		return p
	}
	p := map[string]any{"system": "wekan", "recordId": rec["_id"]}
	if sourceBoard != "" && len(sourceBoard) <= 500 {
		p["projectId"] = sourceBoard
	}
	return p
}

func sameProvenance(a, b map[string]any) bool {
	if a == nil || b == nil || docString(a, "system") == "" || docString(a, "recordId") == "" {
		return false
	}
	return a["system"] == b["system"] && a["recordId"] == b["recordId"] &&
		(docString(a, "projectId") == "" || docString(b, "projectId") == "" || a["projectId"] == b["projectId"])
}

type mergeDecision struct {
	Action, TargetID, By, Name, Reason, State string
	Provenance                                map[string]any
}

// resolveRecords is WeKan's, in the file's order.
func resolveRecords(kind string, incoming, destination []map[string]any, sourceBoard string) (map[string]*mergeDecision, []string) {
	decisions := map[string]*mergeDecision{}
	order := []string{}
	claimed := map[string]bool{}
	names := map[string]int{}
	for _, r := range incoming {
		names[jsTrim(docString(r, "name"))]++
		order = append(order, docString(r, "_id"))
	}
	for _, r := range incoming {
		id := docString(r, "_id")
		var byID map[string]any
		for _, d := range destination {
			if docString(d, "_id") == id {
				byID = d
			}
		}
		prov := sourceProvenance(r, sourceBoard)
		var byProv []map[string]any
		if byID == nil {
			for _, d := range destination {
				if p, _ := plain(d["provenance"]).(map[string]any); sameProvenance(p, prov) {
					byProv = append(byProv, d)
				}
			}
		}
		target := byID
		if target == nil && len(byProv) == 1 {
			target = byProv[0]
		}
		switch {
		case len(byProv) > 1, target != nil && claimed[docString(target, "_id")]:
			decisions[id] = &mergeDecision{Action: "skip", Reason: "record-ambiguous", Name: docString(r, "name")}
		case target != nil:
			claimed[docString(target, "_id")] = true
			by := "provenance"
			if byID != nil {
				by = "id"
			}
			decisions[id] = &mergeDecision{Action: "match", TargetID: docString(target, "_id"), By: by, Name: docString(target, "name"), State: docString(target, "state")}
		}
	}
	for _, r := range incoming {
		id := docString(r, "_id")
		if decisions[id] != nil {
			continue
		}
		name := jsTrim(docString(r, "name"))
		var byName []map[string]any
		if kind != "event" && name != "" {
			for _, d := range destination {
				if jsTrim(docString(d, "name")) == name {
					byName = append(byName, d)
				}
			}
		}
		switch {
		case len(byName) > 1 || len(byName) == 1 && (names[name] > 1 || claimed[docString(byName[0], "_id")]):
			decisions[id] = &mergeDecision{Action: "skip", Reason: "record-ambiguous", Name: docString(r, "name")}
		case len(byName) == 1:
			claimed[docString(byName[0], "_id")] = true
			decisions[id] = &mergeDecision{Action: "match", TargetID: docString(byName[0], "_id"), By: "name", Name: docString(byName[0], "name"), State: docString(byName[0], "state")}
		default:
			decisions[id] = &mergeDecision{Action: "create", TargetID: randomID(), Name: docString(r, "name"), State: docString(r, "state"), Provenance: sourceProvenance(r, sourceBoard)}
		}
	}
	return decisions, order
}

type cardDecision struct{ TargetID, By, Reason string }

// resolveCards is WeKan's.
func resolveCards(sourceIDs []string, sourceCards, destCards []map[string]any, foreign map[string]bool) map[string]*cardDecision {
	key := func(r map[string]any) string {
		n, ok := scrumSafeInt(r["cardNumber"])
		t := jsTrim(docString(r, "title"))
		if !ok || n <= 0 || t == "" {
			return ""
		}
		return strconv.FormatFloat(n, 'f', -1, 64) + "\x00" + t
	}
	byID, byKey := map[string]bool{}, map[string][]string{}
	for _, c := range destCards {
		if linkedCardTypes[docString(c, "type")] {
			continue
		}
		byID[docString(c, "_id")] = true
		if k := key(c); k != "" {
			byKey[k] = append(byKey[k], docString(c, "_id"))
		}
	}
	source := map[string]map[string]any{}
	for _, c := range sourceCards {
		source[docString(c, "_id")] = c
	}
	out := map[string]*cardDecision{}
	for _, id := range sourceIDs {
		if byID[id] {
			out[id] = &cardDecision{TargetID: id, By: "id"}
			continue
		}
		var cands []string
		if c, ok := source[id]; ok {
			if k := key(c); k != "" {
				cands = byKey[k]
			}
		}
		switch {
		case len(cands) == 1:
			out[id] = &cardDecision{TargetID: cands[0], By: "cardNumber"}
		case len(cands) > 1:
			out[id] = &cardDecision{Reason: "card-ambiguous"}
		case foreign[id]:
			out[id] = &cardDecision{Reason: "card-on-another-board"}
		default:
			out[id] = &cardDecision{Reason: "card-not-matched"}
		}
	}
	claims := map[string][]string{}
	for _, id := range sourceIDs {
		if d := out[id]; d.TargetID != "" {
			claims[d.TargetID] = append(claims[d.TargetID], id)
		}
	}
	for _, ids := range claims {
		if len(ids) < 2 {
			continue
		}
		for _, id := range ids {
			if out[id].By != "id" {
				out[id] = &cardDecision{Reason: "card-ambiguous"}
			}
		}
	}
	return out
}

func uniqueByTitle(src, dst []map[string]any, field string) map[string]string {
	count := func(list []map[string]any) map[string]int {
		m := map[string]int{}
		for _, r := range list {
			m[jsTrim(docString(r, field))]++
		}
		return m
	}
	sc, dc := count(src), count(dst)
	out := map[string]string{}
	for _, r := range src {
		name := jsTrim(docString(r, field))
		if name == "" || sc[name] != 1 || dc[name] != 1 {
			continue
		}
		for _, d := range dst {
			if jsTrim(docString(d, field)) == name {
				out[docString(r, "_id")] = docString(d, "_id")
			}
		}
	}
	return out
}

type mergePlan struct {
	sourceBoard               string
	prepared                  map[string]any
	maps                      map[string]map[string]string
	sprints, releases, events map[string]*mergeDecision
	sprintOrder, releaseOrder []string
	eventOrder                []string
	sprintStates              map[string]string
	losses                    []any
}

// planScrumTransferMerge is WeKan's: the transfer reduced to what is written
// or referenced, in the file's ids, and the maps onto this board's.
func planScrumTransferMerge(file map[string]any, dest map[string]any) *mergePlan {
	source := scrumTransferFileFields(file)
	t := normalizeScrumTransfer(jsonValue(source["scrumTransfer"]))
	losses := append([]any{}, normalizeScrumTransferLosses(jsonValue(source["scrumTransferLosses"]))...)
	loss := func(path, id, reason string) {
		losses = append(losses, map[string]any{"path": path, "sourceId": id, "reason": reason})
	}
	sb, _ := source["_id"].(string)
	docs := func(k string) []map[string]any { return asDocs(dest[k]) }
	sprints, sOrder := resolveRecords("sprint", t["sprints"].([]map[string]any), docs("sprints"), sb)
	releases, rOrder := resolveRecords("release", t["releases"].([]map[string]any), docs("releases"), sb)
	events, eOrder := resolveRecords("event", t["events"].([]map[string]any), docs("events"), sb)
	lists := map[string]string{}
	for _, l := range docs("lists") {
		lists[docString(l, "_id")] = docString(l, "_id")
	}
	for from, to := range uniqueByTitle(asDocs(source["lists"]), docs("lists"), "title") {
		if _, ok := lists[from]; !ok {
			lists[from] = to
		}
	}
	var numberFields, srcNumber []map[string]any
	for _, f := range docs("customFields") {
		if f["type"] == "number" {
			numberFields = append(numberFields, f)
		}
	}
	for _, f := range asDocs(source["customFields"]) {
		if f["type"] == "number" {
			srcNumber = append(srcNumber, f)
		}
	}
	fields := map[string]string{}
	for _, f := range numberFields {
		fields[docString(f, "_id")] = docString(f, "_id")
	}
	for from, to := range uniqueByTitle(srcNumber, numberFields, "name") {
		if _, ok := fields[from]; !ok {
			fields[from] = to
		}
	}
	snapshotsOf := func(sp map[string]any) []map[string]any {
		out := []map[string]any{}
		for _, k := range []string{"startSnapshot", "closeSnapshot"} {
			if s, _ := sp[k].(map[string]any); s != nil {
				out = append(out, s)
			}
		}
		return out
	}
	cardIDs, seen := []string{}, map[string]bool{}
	addCard := func(id string) {
		if !seen[id] {
			seen[id] = true
			cardIDs = append(cardIDs, id)
		}
	}
	for _, r := range t["cards"].([]map[string]any) {
		addCard(docString(r, "_id"))
	}
	for _, sp := range t["sprints"].([]map[string]any) {
		for _, snap := range snapshotsOf(sp) {
			for _, r := range snapshotCards(snap) {
				addCard(docString(r, "cardId"))
			}
		}
	}
	for _, o := range t["dailyObservations"].([]map[string]any) {
		for _, r := range snapshotCards(o["snapshot"]) {
			addCard(docString(r, "cardId"))
		}
	}
	for _, e := range t["events"].([]map[string]any) {
		for _, id := range stringsOf(e["followUpCardIds"]) {
			addCard(id)
		}
	}
	foreign := map[string]bool{}
	for _, id := range stringsOf(dest["foreignCardIds"]) {
		foreign[id] = true
	}
	cardDecisions := resolveCards(cardIDs, asDocs(source["cards"]), docs("cards"), foreign)
	cards := map[string]string{}
	for id, d := range cardDecisions {
		if d.TargetID != "" {
			cards[id] = d.TargetID
		}
	}
	snapshotFits := func(s map[string]any) bool {
		id := docString(s, "estimateCustomFieldId")
		_, ok := fields[id]
		return id == "" || ok || s["estimateSource"] != "customField"
	}
	prepareSnapshot := func(s map[string]any, path string) map[string]any {
		out := map[string]any{}
		for k, v := range s {
			out[k] = v
		}
		kept := []any{}
		for _, r := range snapshotCards(s) {
			_, cardHere := cards[docString(r, "cardId")]
			_, listHere := lists[docString(r, "listId")]
			if !cardHere || listHere {
				kept = append(kept, r)
				continue
			}
			loss(path, docString(r, "cardId"), "card-not-matched")
		}
		if id := docString(out, "estimateCustomFieldId"); id != "" {
			if _, ok := fields[id]; !ok {
				out["estimateCustomFieldId"] = nil
			}
		}
		if len(kept) != len(snapshotCards(s)) {
			out["partial"] = true
		}
		missing, total := 0.0, 0.0
		for _, r := range kept {
			if m := r.(map[string]any); m["estimate"] == nil {
				missing++
			} else {
				total += numberOf(m["estimate"])
			}
		}
		out["cards"], out["missingEstimates"], out["totalEstimate"] = kept, storedNum(missing), storedNum(total)
		return out
	}
	for _, sp := range t["sprints"].([]map[string]any) {
		d := sprints[docString(sp, "_id")]
		if d.Action != "create" {
			continue
		}
		for _, snap := range snapshotsOf(sp) {
			if !snapshotFits(snap) {
				sprints[docString(sp, "_id")] = &mergeDecision{Action: "skip", Reason: "record-not-imported", Name: docString(sp, "name")}
				break
			}
		}
	}
	for _, kd := range []struct {
		kind  string
		d     map[string]*mergeDecision
		order []string
	}{{"sprints", sprints, sOrder}, {"releases", releases, rOrder}} {
		for _, id := range kd.order {
			if d := kd.d[id]; d.Action == "skip" {
				loss(lossPath(kd.kind, id), id, d.Reason)
			}
		}
	}
	resolved := func(d map[string]*mergeDecision) func(string) bool {
		return func(id string) bool { x := d[id]; return id != "" && x != nil && x.Action != "skip" }
	}
	sprintOK, releaseOK := resolved(sprints), resolved(releases)
	reasonOf := func(d map[string]*mergeDecision, id string) string {
		if x := d[id]; x != nil && x.Reason != "" {
			return x.Reason
		}
		return "record-not-imported"
	}
	prepared := map[string]any{"format": t["format"], "settings": map[string]any{}, "lists": []any{}, "swimlanes": []any{}}
	pSprints := []any{}
	for _, sp := range t["sprints"].([]map[string]any) {
		id := docString(sp, "_id")
		switch sprints[id].Action {
		case "skip":
			continue
		case "match":
			pSprints = append(pSprints, map[string]any{"_id": id, "name": sp["name"], "state": "planned"})
			continue
		}
		r := map[string]any{}
		for k, v := range sp {
			r[k] = v
		}
		if ro := docString(r, "rolloverSprintId"); ro != "" && !sprintOK(ro) {
			loss(lossPath("sprints", id, "rolloverSprintId"), ro, reasonOf(sprints, ro))
			r["rolloverSprintId"] = nil
		}
		for _, k := range []string{"startSnapshot", "closeSnapshot"} {
			if snap, _ := r[k].(map[string]any); snap != nil {
				r[k] = prepareSnapshot(snap, lossPath("sprints", id, k))
			}
		}
		pSprints = append(pSprints, r)
	}
	pReleases := []any{}
	for _, rel := range t["releases"].([]map[string]any) {
		switch releases[docString(rel, "_id")].Action {
		case "skip":
		case "match":
			pReleases = append(pReleases, map[string]any{"_id": rel["_id"], "name": rel["name"]})
		default:
			pReleases = append(pReleases, rel)
		}
	}
	pEvents := []any{}
	for _, e := range t["events"].([]map[string]any) {
		id := docString(e, "_id")
		if events[id].Action != "create" {
			continue
		}
		if !sprintOK(docString(e, "sprintId")) {
			events[id] = &mergeDecision{Action: "skip", Reason: "record-not-imported"}
			loss(lossPath("events", id), id, "record-not-imported")
			continue
		}
		pEvents = append(pEvents, e)
	}
	pObs := []any{}
	for _, o := range t["dailyObservations"].([]map[string]any) {
		sid := docString(o, "sprintId")
		if sprints[sid].Action != "create" {
			continue
		}
		snap := o["snapshot"].(map[string]any)
		path := lossPath("dailyObservations", sid, docString(o, "day"))
		if !snapshotFits(snap) {
			loss(path, sid, "record-not-imported")
			continue
		}
		r := map[string]any{}
		for k, v := range o {
			r[k] = v
		}
		r["snapshot"] = prepareSnapshot(snap, path)
		pObs = append(pObs, r)
	}
	pCards := []any{}
	for _, c := range t["cards"].([]map[string]any) {
		id := docString(c, "_id")
		d := cardDecisions[id]
		if d.TargetID == "" {
			loss(lossPath("cards", id), id, d.Reason)
			continue
		}
		sc := map[string]any{}
		for k, v := range c["scrum"].(map[string]any) {
			sc[k] = v
		}
		if sid := docString(sc, "sprintId"); sid != "" && !sprintOK(sid) {
			loss(lossPath("cards", id, "scrum", "sprintId"), sid, reasonOf(sprints, sid))
			delete(sc, "sprintId")
		}
		if past, ok := sc["pastSprintIds"].([]any); ok {
			kept := []any{}
			for _, x := range past {
				if s, _ := x.(string); sprintOK(s) {
					kept = append(kept, s)
				}
			}
			sc["pastSprintIds"] = kept
		}
		if hasReleaseFields(sc) {
			ids := cardReleaseIds(sc)
			kept := []string{}
			for _, rid := range ids {
				if releaseOK(rid) {
					kept = append(kept, rid)
				} else {
					loss(lossPath("cards", id, "scrum", "releaseIds"), rid, reasonOf(releases, rid))
				}
			}
			delete(sc, "releaseId")
			delete(sc, "releaseIds")
			if len(kept) > 0 || len(ids) == 0 {
				for k, v := range withCardReleaseIds(map[string]any{}, kept) {
					sc[k] = v
				}
			}
		}
		pCards = append(pCards, map[string]any{"_id": id, "scrum": sc})
	}
	prepared["sprints"], prepared["releases"], prepared["events"], prepared["dailyObservations"], prepared["cards"] = pSprints, pReleases, pEvents, pObs, pCards
	maps := map[string]map[string]string{"sprints": {}, "releases": {}, "events": {}, "cards": cards, "lists": lists, "customFields": fields,
		"swimlanes": {}, "users": {}}
	states := map[string]string{}
	for id, d := range sprints {
		if d.Action != "skip" {
			maps["sprints"][id] = d.TargetID
			states[d.TargetID] = d.State
		}
	}
	for id, d := range releases {
		if d.Action != "skip" {
			maps["releases"][id] = d.TargetID
		}
	}
	for id, d := range events {
		if d.Action == "create" {
			maps["events"][id] = d.TargetID
		}
	}
	for _, id := range stringsOf(dest["memberIds"]) {
		maps["users"][id] = id
	}
	return &mergePlan{sourceBoard: sb, prepared: prepared, maps: maps, sprints: sprints, releases: releases, events: events,
		sprintOrder: sOrder, releaseOrder: rOrder, eventOrder: eOrder, sprintStates: states, losses: losses}
}

// mergeCardScrum is WeKan's: a card's metadata after the import, and the
// finished sprint it was not moved into.
func mergeCardScrum(current, incoming map[string]any, states map[string]string) (map[string]any, bool, string) {
	before := current
	if before == nil {
		before = map[string]any{}
	}
	out := map[string]any{}
	for k, v := range before {
		out[k] = v
	}
	for _, k := range []string{"issueType", "acceptanceCriteria", "backlogRank"} {
		if v, ok := incoming[k]; ok {
			out[k] = v
		}
	}
	if hasReleaseFields(incoming) {
		list := cardReleaseIds(incoming)
		if canonical(list) != canonical(cardReleaseIds(before)) {
			for k, v := range withCardReleaseIds(map[string]any{}, list) {
				out[k] = v
			}
		}
	}
	past := stringsOf(before["pastSprintIds"])
	for _, id := range stringsOf(incoming["pastSprintIds"]) {
		if !containsString(past, id) {
			past = append(past, id)
		}
	}
	refused := ""
	if target, ok := incoming["sprintId"]; ok {
		t, _ := target.(string)
		was := docString(before, "sprintId")
		if t != was {
			if st := states[t]; t != "" && st != "planned" && st != "active" {
				refused = t
			} else {
				if t == "" {
					out["sprintId"] = nil
				} else {
					out["sprintId"] = t
				}
				if was != "" && !containsString(past, was) {
					past = append(past, was)
				}
			}
		}
	}
	if len(past) > 0 || has(before, "pastSprintIds") {
		list := []any{}
		for _, p := range past {
			list = append(list, p)
		}
		out["pastSprintIds"] = list
	}
	return out, canonical(out) != canonical(before), refused
}

var cardsPath = regexp.MustCompile(`^cards\.[^.]+$`)

// methodScrumImportIntoBoard is scrum.importIntoBoard(boardId, file, {dryRun}).
func (s *Server) methodScrumImportIntoBoard(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	boardID, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	file, err := scrumObjectArg(args, 1)
	if err != nil {
		return nil, err
	}
	dryRun := false
	if v, given := scrumArg(args, 2); given && v != nil {
		o, ok := v.(map[string]any)
		if !ok {
			return nil, &methodError{"Match.Error", "Match failed"}
		}
		for k, x := range o {
			b, isBool := x.(bool)
			if k != "dryRun" || !isBool {
				return nil, &methodError{"Match.Error", "Match failed"}
			}
			dryRun = b
		}
	}
	return s.scrumCall(r, u, boardID, func(op *scrumOp) (any, error) {
		ctx := op.ctx
		b, board, err := s.scrumBoard(ctx, u, boardID, true)
		if err != nil {
			return nil, err
		}
		if err := s.scrumPending(ctx, boardID); err != nil {
			return nil, err
		}
		return s.importIntoBoard(op, b, plain(board).(map[string]any), file, dryRun)
	})
}

func (s *Server) importIntoBoard(op *scrumOp, b *Board, board map[string]any, file map[string]any, dryRun bool) (any, error) {
	ctx, boardID := op.ctx, b.ID
	var plan *mergePlan
	var t map[string]any
	var remapLosses []any
	destCards := map[string]map[string]any{}
	if err := scrumValidate(func() {
		fields := scrumTransferFileFields(file)
		dest := s.mergeDestination(ctx, b, fields)
		for _, c := range asDocs(dest["cards"]) {
			destCards[docString(c, "_id")] = c
		}
		plan = planScrumTransferMerge(fields, dest)
		t, remapLosses = remapScrumTransfer(plan.prepared, plan.maps)
	}); err != nil {
		return nil, &methodError{"invalid-scrum-transfer", err.(*methodError).Reason}
	}
	losses := append(append([]any{}, plan.losses...), remapLosses...)
	created := func(d map[string]*mergeDecision) map[string]bool {
		out := map[string]bool{}
		for _, x := range d {
			if x.Action == "create" {
				out[x.TargetID] = true
			}
		}
		return out
	}
	provenance := map[string]map[string]any{}
	for _, d := range []map[string]*mergeDecision{plan.sprints, plan.releases, plan.events} {
		for _, x := range d {
			if x.Action == "create" {
				provenance[x.TargetID] = x.Provenance
			}
		}
	}
	type write struct {
		coll string
		doc  bson.M
		kind string
	}
	var inserts []write
	var cardWrites []map[string]any
	for _, k := range []string{"sprints", "releases", "events"} {
		fresh := created(map[string]map[string]*mergeDecision{"sprints": plan.sprints, "releases": plan.releases, "events": plan.events}[k])
		for _, rec := range t[k].([]map[string]any) {
			if !fresh[docString(rec, "_id")] {
				continue
			}
			doc := bson.M{}
			for f, v := range rec {
				doc[f] = v
			}
			doc["provenance"], doc["boardId"], doc["revision"], doc["incarnation"] = provenance[docString(rec, "_id")], boardID, int32(1), randomID()
			inserts = append(inserts, write{map[string]string{"sprints": "scrumSprints", "releases": "scrumReleases", "events": "scrumEvents"}[k], doc, "scrum-" + k[:len(k)-1]})
		}
	}
	updated, unchanged := 0, 0
	for _, c := range t["cards"].([]map[string]any) {
		cur := destCards[docString(c, "_id")]
		curScrum, _ := cur["scrum"].(map[string]any)
		sc, changed, refused := mergeCardScrum(curScrum, c["scrum"].(map[string]any), plan.sprintStates)
		if refused != "" {
			losses = append(losses, map[string]any{"path": lossPath("cards", docString(c, "_id"), "scrum", "sprintId"), "sourceId": refused, "reason": "sprint-finished"})
		}
		if !changed {
			unchanged++
			continue
		}
		updated++
		cardWrites = append(cardWrites, map[string]any{"card": cur, "scrum": sc})
	}
	var report []any
	if len(losses) > 10000 {
		report = normalizeScrumTransferLosses(losses[:10000])
	} else {
		report = normalizeScrumTransferLosses(losses)
	}
	names := func(d map[string]*mergeDecision, order []string, action string) []any {
		out := []any{}
		for _, id := range order {
			if x := d[id]; x != nil && x.Action == action {
				out = append(out, x.Name)
			}
		}
		return out
	}
	count := func(d map[string]*mergeDecision, action string) int {
		n := 0
		for _, x := range d {
			if x.Action == action {
				n++
			}
		}
		return n
	}
	unmatched := 0
	for _, l := range losses {
		if cardsPath.MatchString(docString(l.(map[string]any), "path")) {
			unmatched++
		}
	}
	var sourceBoard any
	if plan.sourceBoard != "" {
		sourceBoard = plan.sourceBoard
	}
	obs := t["dailyObservations"].([]map[string]any)
	summary := map[string]any{"dryRun": dryRun, "sourceBoardId": sourceBoard,
		"sprints":           map[string]any{"created": names(plan.sprints, plan.sprintOrder, "create"), "matched": names(plan.sprints, plan.sprintOrder, "match"), "skipped": names(plan.sprints, plan.sprintOrder, "skip")},
		"releases":          map[string]any{"created": names(plan.releases, plan.releaseOrder, "create"), "matched": names(plan.releases, plan.releaseOrder, "match"), "skipped": names(plan.releases, plan.releaseOrder, "skip")},
		"events":            map[string]any{"created": count(plan.events, "create"), "matched": count(plan.events, "match")},
		"cards":             map[string]any{"updated": updated, "unchanged": unchanged, "unmatched": unmatched},
		"dailyObservations": len(obs), "losses": report, "truncated": len(losses) > 10000}
	if dryRun || len(inserts)+len(cardWrites)+len(obs) == 0 {
		summary["changed"] = false
		return summary, nil
	}
	// Written: the records, their snapshots' and observations' rows, the
	// cards, the board's report - its sprints an unfinished import until the
	// last write - and one History row (the call's).
	var sprintIDs bson.A
	for _, w := range inserts {
		if w.coll == "scrumSprints" {
			id := docString(w.doc, "_id")
			for _, f := range [][2]string{{"startSnapshot", "start"}, {"closeSnapshot", "close"}} {
				if snap, _ := w.doc[f[0]].(map[string]any); snap != nil {
					header, err := s.storeScrumSnapshot(ctx, boardID, id, f[1], snap)
					if err != nil {
						return nil, err
					}
					w.doc[f[0]] = header
				}
			}
			if w.doc["startSnapshot"] != nil || w.doc["closeSnapshot"] != nil {
				rec := t["sprints"].([]map[string]any)
				for _, r := range rec {
					if docString(r, "_id") == id {
						w.doc["reportTotals"] = scrumReportTotals(snapshotCards(r["startSnapshot"]), snapshotCards(r["closeSnapshot"]))
					}
				}
			}
			w.doc["scrumImportPending"] = true
			sprintIDs = append(sprintIDs, id)
		}
		if _, err := s.db.Collection(w.coll).InsertOne(ctx, w.doc); err != nil {
			return nil, err
		}
		after := plain(w.doc).(map[string]any)
		delete(after, "scrumImportPending")
		op.record(w.kind, nil, after)
	}
	for _, o := range obs {
		sid := docString(o, "sprintId")
		header, err := s.storeScrumSnapshot(ctx, boardID, sid, "daily", o["snapshot"].(map[string]any))
		if err != nil {
			return nil, err
		}
		startedAt := o["startedAt"].(time.Time)
		if _, err := s.db.Collection("scrumDailySnapshots").InsertOne(ctx, bson.M{"_id": dailyObservationID(sid, startedAt, docString(o, "day")), // inserted, not a filter
			"boardId": boardID, "sprintId": sid, "startedAt": startedAt, "day": o["day"], "capturedAt": o["capturedAt"], "snapshot": header, "consistency": "observed"}); err != nil {
			return nil, err
		}
	}
	for _, cw := range cardWrites {
		cur := cw["card"].(map[string]any)
		next := int32(revisionOf(cur["scrumRevision"]) + 1)
		filter := scrumRevisionSelector(cur)
		filter["_id"], filter["boardId"] = eq(docString(cur, "_id")), eq(boardID)
		res, err := s.db.Collection("cards").UpdateOne(ctx, filter, bson.M{"$set": bson.M{"scrum": cw["scrum"], "scrumRevision": next}})
		if err != nil {
			return nil, err
		}
		if res.MatchedCount == 0 {
			return nil, scrumConflict()
		}
		after := map[string]any{}
		for k, v := range cur {
			after[k] = v
		}
		after["scrum"], after["scrumRevision"] = cw["scrum"], next
		op.record("card", cur, after)
	}
	next := int32(revisionOf(board["scrumRevision"]) + 1)
	if _, err := s.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": eq(boardID)}, bson.M{"$set": bson.M{"scrumRevision": next, "scrumImportLosses": report}}); err != nil {
		return nil, err
	}
	if len(sprintIDs) > 0 {
		_, _ = s.db.Collection("scrumSprints").UpdateMany(ctx, bson.M{"boardId": eq(boardID), "_id": dbIn(sprintIDs)}, bson.M{"$unset": bson.M{"scrumImportPending": ""}})
	}
	summary["changed"] = true
	return summary, nil
}

// mergeDestination is loadDestination(): what the board has that the file
// may name.
func (s *Server) mergeDestination(ctx context.Context, b *Board, fields map[string]any) map[string]any {
	t, _ := fields["scrumTransfer"].(map[string]any)
	ids := bson.A{}
	add := func(v any) {
		if id, ok := v.(string); ok && len(id) <= 200 {
			ids = append(ids, id)
		}
	}
	for _, r := range asDocs(t["cards"]) {
		add(r["_id"])
	}
	for _, sp := range asDocs(t["sprints"]) {
		for _, k := range []string{"startSnapshot", "closeSnapshot"} {
			for _, r := range snapshotCards(sp[k]) {
				add(r["cardId"])
			}
		}
	}
	for _, o := range asDocs(t["dailyObservations"]) {
		for _, r := range snapshotCards(o["snapshot"]) {
			add(r["cardId"])
		}
	}
	for _, e := range asDocs(t["events"]) {
		for _, id := range stringsOf(e["followUpCardIds"]) {
			add(id)
		}
	}
	numbers := bson.A{}
	for _, c := range asDocs(fields["cards"]) {
		if n, ok := scrumSafeInt(c["cardNumber"]); ok {
			numbers = append(numbers, n)
		}
	}
	planning := options.Find().SetProjection(bson.M{"_id": 1, "name": 1, "state": 1, "provenance": 1})
	notLinked := bson.M{"$nin": bson.A{"cardType-linkedCard", "cardType-linkedBoard"}}
	cards := s.findDocs(ctx, "cards", bson.M{"boardId": eq(b.ID), "type": notLinked, "$or": bson.A{bson.M{"_id": dbIn(ids)}, bson.M{"cardNumber": bson.M{"$in": numbers}}}},
		options.Find().SetProjection(bson.M{"_id": 1, "title": 1, "cardNumber": 1, "type": 1, "boardId": 1, "listId": 1, "swimlaneId": 1, "scrum": 1, "scrumRevision": 1}))
	// Another board's card is named in the report, never written; the file's
	// own board is not "another board".
	sourceBoard, _ := fields["_id"].(string)
	foreign := []any{}
	for _, c := range s.findDocs(ctx, "cards", bson.M{"_id": dbIn(ids)}, options.Find().SetProjection(bson.M{"_id": 1, "boardId": 1})) {
		if bid := docString(c, "boardId"); bid != b.ID && bid != sourceBoard {
			foreign = append(foreign, c["_id"])
		}
	}
	members := []any{}
	for _, m := range b.Members {
		if m.IsActive {
			members = append(members, m.UserID)
		}
	}
	return map[string]any{"sprints": s.findDocs(ctx, "scrumSprints", bson.M{"boardId": eq(b.ID)}, planning),
		"releases": s.findDocs(ctx, "scrumReleases", bson.M{"boardId": eq(b.ID)}, planning),
		"events":   s.findDocs(ctx, "scrumEvents", bson.M{"boardId": eq(b.ID)}, options.Find().SetProjection(bson.M{"_id": 1, "provenance": 1, "sprintId": 1})),
		"cards":    cards, "lists": s.findDocs(ctx, "lists", bson.M{"boardId": eq(b.ID)}, options.Find().SetProjection(bson.M{"_id": 1, "title": 1})),
		"customFields":   s.findDocs(ctx, "customFields", bson.M{"boardIds": eq(b.ID)}, options.Find().SetProjection(bson.M{"_id": 1, "name": 1, "type": 1})),
		"foreignCardIds": foreign, "memberIds": members}
}
