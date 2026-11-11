package web

// Scrum's pure half, as WeKan's models/lib/scrum.js and
// models/lib/scrumReports.js: the canonical settings, card, list and swimlane
// metadata and sprint, release and event records a method may write, the
// estimate and completion of a card, a sprint's snapshot and the report made
// from it. Weddy shares WeKan's database, so every field, default and stored
// shape is WeKan's own, and a sprint WeKan started closes here.

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

const scrumTextLimit = 10000

// scrumInvalid is a value that fails WeKan's validation: the method answers
// invalid-scrum with its message.
type scrumInvalid struct{ msg string }

func sfail(msg string) { panic(scrumInvalid{msg}) }

// scrumValidate runs fn, a validation that fails by sfail, as an error.
func scrumValidate(fn func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			v, ok := r.(scrumInvalid)
			if !ok {
				panic(r)
			}
			err = &methodError{"invalid-scrum", v.msg}
		}
	}()
	fn()
	return nil
}

// storedNum is a number as Node's driver stores it: an integer that fits as an
// int32, anything else as a double.
func storedNum(f float64) any {
	if f == math.Trunc(f) && f >= math.MinInt32 && f <= math.MaxInt32 {
		return int32(f)
	}
	return f
}

type scrumDef func(any) any

func scrumObject(v any, keys []string) map[string]any {
	m, ok := v.(map[string]any)
	if !ok || m == nil {
		sfail("Expected a plain object")
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if !slices.Contains(keys, k) {
			sfail("Unsupported field: " + k)
		}
	}
	return m
}

func scrumNormalize(v any, defs map[string]scrumDef) map[string]any {
	keys := make([]string, 0, len(defs))
	for k := range defs {
		keys = append(keys, k)
	}
	m := scrumObject(v, keys)
	out := map[string]any{}
	for k, item := range m {
		out[k] = defs[k](item)
	}
	return out
}

func scrumTextMax(max int) scrumDef {
	return func(v any) any {
		s, ok := v.(string)
		// String length as JavaScript counts it: UTF-16 code units.
		if !ok || utf16Len(s) > max || strings.ContainsRune(s, 0) {
			sfail("Invalid text")
		}
		return s
	}
}

var scrumText = scrumTextMax(scrumTextLimit)

func scrumID(v any) any {
	if v == nil {
		return nil
	}
	s := scrumTextMax(200)(v).(string)
	if s == "" {
		sfail("Empty identifier")
	}
	return s
}

func scrumNumber(v any) any {
	f, ok := v.(float64)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > 1e12 {
		sfail("Invalid nonnegative number")
	}
	return storedNum(f)
}

func scrumNullableNumber(v any) any {
	if v == nil {
		return nil
	}
	return scrumNumber(v)
}

func scrumIDs(v any) any {
	list, ok := v.([]any)
	if !ok || len(list) > 1000 {
		sfail("Invalid reference list")
	}
	out := []any{}
	seen := map[string]bool{}
	for _, x := range list {
		if x == nil {
			sfail("Null reference")
		}
		id := scrumID(x).(string)
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func scrumChoice(options ...string) scrumDef {
	return func(v any) any {
		s, ok := v.(string)
		if !ok || !slices.Contains(options, s) {
			sfail("Invalid option")
		}
		return s
	}
}

var scrumDecimal = regexp.MustCompile(`^\d+(?:\.\d+)?$`)

var scrumDatePattern = regexp.MustCompile(`^\d{4}-\d\d-\d\d(?:T\d\d:\d\d:\d\d(?:\.\d{1,3})?Z)?$`)

// scrumDate is an ISO UTC date or date and time, or a stored date.
func scrumDate(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case time.Time:
		return x.UTC().Truncate(time.Millisecond)
	case map[string]any:
		// EJSON's {$date: ms} or {$date: ISO}.
		if d, ok := x["$date"]; ok && len(x) == 1 {
			if f, ok := d.(float64); ok && !math.IsNaN(f) && !math.IsInf(f, 0) {
				return time.UnixMilli(int64(f)).UTC()
			}
			if s, ok := d.(string); ok {
				if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
					return t.UTC().Truncate(time.Millisecond)
				}
			}
		}
	case string:
		if !scrumDatePattern.MatchString(x) {
			break
		}
		layout := "2006-01-02"
		if len(x) > 10 {
			layout = time.RFC3339Nano
		}
		t, err := time.Parse(layout, x)
		if err != nil {
			sfail("Invalid calendar date")
		}
		return t.UTC().Truncate(time.Millisecond)
	}
	sfail("Use an ISO UTC date")
	return nil
}

func scrumBool(msg string) scrumDef {
	return func(v any) any {
		b, ok := v.(bool)
		if !ok {
			sfail(msg)
		}
		return b
	}
}

func scrumProvenance(v any) any {
	return scrumNormalize(v, map[string]scrumDef{"system": scrumTextMax(100), "projectId": scrumTextMax(500),
		"recordId": scrumTextMax(500), "sourceTimestamp": scrumDate, "unit": scrumTextMax(80)})
}

// scrumVisibilityKeys are Card Settings' Scrum rows (DEFAULT visibility {}).
var scrumVisibilityKeys = []string{"cardSprint", "cardPastSprints", "cardRelease", "cardIssueType", "cardAcceptanceCriteria",
	"cardBacklogRank", "minicardSprint", "minicardPastSprints", "minicardRelease", "minicardIssueType",
	"minicardAcceptanceCriteria", "minicardBacklogRank", "listCategory", "swimlaneSprint", "swimlaneRelease", "swimlanePurpose"}

// defaultScrumSettings is DEFAULT_SCRUM_SETTINGS, a fresh copy each time.
func defaultScrumSettings() map[string]any {
	return map[string]any{"enabled": false, "productGoal": "", "definitionOfDone": "", "estimateSource": "poker",
		"estimateCustomFieldId": nil, "estimateUnit": "points", "completionPolicy": "dueComplete",
		"productOwnerId": nil, "scrumMasterId": nil, "developerIds": []any{}, "workingDays": []any{int32(1), int32(2), int32(3), int32(4), int32(5)},
		"visibility": map[string]any{}}
}

// scrumSettingsOf is {...DEFAULT_SCRUM_SETTINGS, ...board.scrum}.
func scrumSettingsOf(stored any) map[string]any {
	out := defaultScrumSettings()
	if m, ok := plain(stored).(map[string]any); ok {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

func normalizeScrumSettings(v any) map[string]any {
	vis := map[string]scrumDef{}
	for _, k := range scrumVisibilityKeys {
		vis[k] = scrumBool("Invalid visibility flag")
	}
	return scrumNormalize(v, map[string]scrumDef{
		"visibility":  func(v any) any { return scrumNormalize(v, vis) },
		"enabled":     scrumBool("Invalid enabled flag"),
		"productGoal": scrumText, "definitionOfDone": scrumText,
		"estimateSource":        scrumChoice("poker", "customField"),
		"estimateCustomFieldId": scrumID,
		"estimateUnit": func(v any) any {
			s := jsTrim(scrumTextMax(80)(v).(string))
			if s == "" {
				sfail("Estimate unit is required")
			}
			return s
		},
		"completionPolicy": scrumChoice("dueComplete", "doneLists"),
		"productOwnerId":   scrumID, "scrumMasterId": scrumID, "developerIds": scrumIDs,
		"workingDays": func(v any) any {
			list, ok := v.([]any)
			if !ok || len(list) == 0 || len(list) > 7 {
				sfail("Invalid working days")
			}
			out := []any{}
			seen := map[float64]bool{}
			for _, d := range list {
				f, ok := d.(float64)
				if !ok || f != math.Trunc(f) || f < 1 || f > 7 {
					sfail("Invalid option")
				}
				if !seen[f] {
					seen[f] = true
					out = append(out, int32(f))
				}
			}
			return out
		},
	})
}

const maxCardReleases = 100

func normalizeScrumMetadata(kind string, v any) map[string]any {
	defs := map[string]map[string]scrumDef{
		"card": {"sprintId": scrumID, "pastSprintIds": scrumIDs, "backlogRank": scrumNullableNumber, "releaseId": scrumID,
			"releaseIds": func(v any) any {
				list := scrumIDs(v).([]any)
				if len(list) > maxCardReleases {
					sfail("Too many releases")
				}
				return list
			},
			"issueType": scrumTextMax(100), "acceptanceCriteria": scrumText},
		"list":     {"category": scrumChoice("backlog", "todo", "doing", "done")},
		"swimlane": {"sprintId": scrumID, "releaseId": scrumID, "purpose": scrumTextMax(1000)},
	}
	d, ok := defs[kind]
	if !ok {
		sfail("Invalid metadata kind")
	}
	return scrumNormalize(v, d)
}

// cardReleaseIds is WeKan's: the list, with a legacy releaseId first.
func cardReleaseIds(scrum any) []string {
	m, ok := plain(scrum).(map[string]any)
	if !ok {
		return nil
	}
	out := []string{}
	add := func(v any) {
		if s, ok := v.(string); ok && s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	add(m["releaseId"])
	if list, ok := m["releaseIds"].([]any); ok {
		for _, x := range list {
			add(x)
		}
	}
	return out
}

func hasReleaseFields(m map[string]any) bool {
	_, a := m["releaseId"]
	_, b := m["releaseIds"]
	return a || b
}

func withCardReleaseIds(scrum map[string]any, ids []string) map[string]any {
	out := map[string]any{}
	for k, v := range scrum {
		out[k] = v
	}
	list := []any{}
	seen := map[string]bool{}
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			list = append(list, id)
		}
	}
	if len(list) > 0 {
		out["releaseId"] = list[0]
	} else {
		out["releaseId"] = nil
	}
	out["releaseIds"] = list
	return out
}

// applyCardReleaseChange is WeKan's: what one metadata write does to a
// card's releases.
func applyCardReleaseChange(before, changes map[string]any) map[string]any {
	merged := map[string]any{}
	for k, v := range before {
		merged[k] = v
	}
	for k, v := range changes {
		merged[k] = v
	}
	var list []string
	if ids, ok := changes["releaseIds"]; ok {
		list = stringsOf(ids)
		if rid, has := changes["releaseId"]; has {
			var first any
			if len(list) > 0 {
				first = list[0]
			}
			if rid != first {
				sfail("releaseId must be the first of releaseIds")
			}
		}
	} else if rid, ok := changes["releaseId"]; ok {
		if rid == nil {
			list = []string{}
		} else {
			list = append([]string{rid.(string)}, tailOf(cardReleaseIds(before))...)
		}
	} else {
		if !hasReleaseFields(before) {
			return merged
		}
		list = cardReleaseIds(before)
	}
	return withCardReleaseIds(merged, list)
}

func tailOf(s []string) []string {
	if len(s) < 2 {
		return nil
	}
	return s[1:]
}

func normalizeScrumRecord(kind string, v any) map[string]any {
	name := func(v any) any {
		s := jsTrim(scrumTextMax(200)(v).(string))
		if s == "" {
			sfail("Name is required")
		}
		return s
	}
	common := map[string]scrumDef{"name": name, "goal": scrumText, "plannedStart": scrumDate, "plannedEnd": scrumDate, "provenance": scrumProvenance}
	with := func(extra map[string]scrumDef) map[string]scrumDef {
		out := map[string]scrumDef{}
		for k, d := range common {
			out[k] = d
		}
		for k, d := range extra {
			out[k] = d
		}
		return out
	}
	defs := map[string]map[string]scrumDef{
		"sprint":  with(map[string]scrumDef{"capacity": scrumNullableNumber, "capacityUnit": scrumTextMax(80)}),
		"release": with(map[string]scrumDef{"notes": scrumText, "releasedAt": scrumDate, "state": scrumChoice("planned", "released", "cancelled")}),
		"event": {"sprintId": scrumID, "kind": scrumChoice("planning", "daily", "review", "retrospective"), "name": name, "startsAt": scrumDate,
			"timeboxMinutes": func(v any) any {
				n := scrumNumber(v)
				if numberOf(n) > 10080 {
					sfail("Timebox too long")
				}
				return n
			},
			"notes": scrumText, "followUpCardIds": scrumIDs, "provenance": scrumProvenance},
	}
	d, ok := defs[kind]
	if !ok {
		sfail("Invalid record kind")
	}
	out := scrumNormalize(v, d)
	if startsAfterEnd(out["plannedStart"], out["plannedEnd"]) {
		sfail("Start must precede end")
	}
	return out
}

func startsAfterEnd(start, end any) bool {
	s, ok1 := plain(start).(time.Time)
	e, ok2 := plain(end).(time.Time)
	return ok1 && ok2 && s.After(e)
}

// scrumCardEstimate is getCardEstimate: the card's poker estimation or the
// board's numeric custom field, nil when it has none.
func scrumCardEstimate(card map[string]any, settings map[string]any) any {
	var v any
	if settings["estimateSource"] == "customField" {
		fields, _ := card["customFields"].([]any)
		for _, f := range fields {
			if m, ok := f.(map[string]any); ok && m["_id"] == settings["estimateCustomFieldId"] {
				v = m["value"]
				break
			}
		}
	} else if p, ok := card["poker"].(map[string]any); ok {
		v = p["estimation"]
	}
	if s, ok := v.(string); ok {
		if scrumDecimal.MatchString(s) {
			var f float64
			if _, err := fmt.Sscan(s, &f); err == nil && !math.IsInf(f, 0) {
				return storedNum(f)
			}
		}
		return nil
	}
	f := numberOf(v)
	if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return nil
	}
	return storedNum(f)
}

// doneListIDs are the lists whose Scrum category is done.
func doneListIDs(lists []map[string]any) map[string]bool {
	out := map[string]bool{}
	for _, l := range lists {
		if m, ok := l["scrum"].(map[string]any); ok && m["category"] == "done" {
			out[docString(l, "_id")] = true
		}
	}
	return out
}

func scrumCardDone(card map[string]any, settings map[string]any, done map[string]bool) bool {
	if settings["completionPolicy"] == "doneLists" {
		return done[docString(card, "listId")]
	}
	return card["dueComplete"] == true
}

// sprintSnapshot is WeKan's: every card's estimate and completion at a moment.
func sprintSnapshot(cards []map[string]any, settings map[string]any, lists []map[string]any, at time.Time) map[string]any {
	done := doneListIDs(lists)
	rows := []any{}
	missing, sum := 0, 0.0
	for _, c := range cards {
		est := scrumCardEstimate(c, settings)
		if est == nil {
			missing++
		} else {
			sum += numberOf(est)
		}
		rows = append(rows, map[string]any{"cardId": docString(c, "_id"), "estimate": est, "done": scrumCardDone(c, settings, done),
			"archived": c["archived"] == true, "listId": c["listId"]})
	}
	days := settings["workingDays"]
	if days == nil {
		days = defaultScrumSettings()["workingDays"]
	}
	return map[string]any{"at": at, "unit": settings["estimateUnit"], "estimateSource": settings["estimateSource"],
		"workingDays": append([]any{}, days.([]any)...), "estimateCustomFieldId": settings["estimateCustomFieldId"],
		"completionPolicy": settings["completionPolicy"], "cards": rows, "missingEstimates": int32(missing), "totalEstimate": storedNum(sum)}
}

// scrumTotal is total(): how many cards, their estimate, and how many have none.
func scrumTotal(rows []map[string]any) map[string]any {
	count, unknown, est := 0, 0, 0.0
	for _, r := range rows {
		count++
		if f := numberOf(r["estimate"]); !math.IsNaN(f) && !math.IsInf(f, 0) && f >= 0 {
			est += f
		} else {
			unknown++
		}
	}
	return map[string]any{"count": int32(count), "estimate": storedNum(est), "unknown": int32(unknown)}
}

func snapshotCards(snapshot any) []map[string]any {
	m, _ := plain(snapshot).(map[string]any)
	list, _ := m["cards"].([]any)
	out := []map[string]any{}
	for _, x := range list {
		if r, ok := x.(map[string]any); ok {
			out = append(out, r)
		}
	}
	return out
}

// scrumReportTotals is reportTotals(sprint), from the snapshots' rows.
func scrumReportTotals(start, end []map[string]any) map[string]any {
	committed, final, doneIDs := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, r := range start {
		committed[docString(r, "cardId")] = true
	}
	var done, added, incomplete []map[string]any
	for _, r := range end {
		final[docString(r, "cardId")] = true
		if r["done"] == true {
			done = append(done, r)
			doneIDs[docString(r, "cardId")] = true
		} else {
			incomplete = append(incomplete, r)
		}
		if !committed[docString(r, "cardId")] {
			added = append(added, r)
		}
	}
	var completedCommitment, removed []map[string]any
	for _, r := range start {
		if doneIDs[docString(r, "cardId")] {
			completedCommitment = append(completedCommitment, r)
		}
		if !final[docString(r, "cardId")] {
			removed = append(removed, r)
		}
	}
	return map[string]any{"committed": scrumTotal(start), "completed": scrumTotal(done), "completedCommitment": scrumTotal(completedCommitment),
		"added": scrumTotal(added), "removed": scrumTotal(removed), "incomplete": scrumTotal(incomplete)}
}

// plannedWorkingDays is WeKan's: the UTC calendar days from start to end,
// inclusive, that are working days; nil when it cannot be said.
func plannedWorkingDays(start, end, workingDays any) any {
	s, ok1 := plain(start).(time.Time)
	e, ok2 := plain(end).(time.Time)
	days, ok3 := plain(workingDays).([]any)
	if !ok1 || !ok2 || !ok3 || len(days) == 0 {
		return nil
	}
	selected := map[int]bool{}
	for _, d := range days {
		f := numberOf(d)
		if f != math.Trunc(f) || f < 1 || f > 7 {
			return nil
		}
		selected[int(f)] = true
	}
	dayNumber := func(t time.Time) int64 { return int64(math.Floor(float64(t.UnixMilli()) / 86400000)) }
	n := dayNumber(e) - dayNumber(s) + 1
	if n <= 0 {
		return nil
	}
	count := int(n/7) * len(selected)
	for i := 0; i < int(n%7); i++ {
		if selected[(int(s.UTC().Weekday())+i+6)%7+1] {
			count++
		}
	}
	return int32(count)
}

func snapshotField(sprint map[string]any, key string) any {
	for _, k := range []string{"closeSnapshot", "startSnapshot"} {
		if m, ok := sprint[k].(map[string]any); ok {
			if v, ok := m[key].(string); ok && v != "" {
				return v
			}
		}
	}
	return ""
}

// scrumSprintReport is sprintReport(sprint), its totals given.
func scrumSprintReport(sprint map[string]any, totals map[string]any) map[string]any {
	start, _ := sprint["startSnapshot"].(map[string]any)
	end, _ := sprint["closeSnapshot"].(map[string]any)
	partial := start != nil && start["partial"] == true || end != nil && end["partial"] == true
	var days any
	if start != nil {
		days = start["workingDays"]
	}
	out := map[string]any{"sprintId": sprint["_id"], "name": sprint["name"], "state": sprint["state"],
		"plannedWorkingDays": plannedWorkingDays(sprint["plannedStart"], sprint["plannedEnd"], days),
		"unit":               snapshotField(sprint, "unit"), "estimateSource": snapshotField(sprint, "estimateSource"),
		"estimateCustomFieldId": snapshotField(sprint, "estimateCustomFieldId"), "completionPolicy": snapshotField(sprint, "completionPolicy"),
		"hasStart": start != nil, "hasClose": end != nil, "partial": partial}
	for k, v := range totals {
		out[k] = v
	}
	return out
}
