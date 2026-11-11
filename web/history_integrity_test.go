package web

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// historyFixtures: rows of every shape the hash meets.
func historyFixtures() []map[string]any {
	at := time.Date(2026, 11, 11, 10, 20, 30, 123e6, time.UTC)
	return []map[string]any{
		{"boardId": "b1", "swimlaneId": nil, "listId": "l1", "cardId": "c1", "entityType": "card", "entityId": "c1", "group": "title",
			"changeType": "edited", "previousContent": map[string]any{"title": "Old"}, "newContent": map[string]any{"title": "Nëw ✓"},
			"userId": "u1", "createdAt": at, "batchId": nil, "restoredFromId": nil, "restoredByUserId": nil, "previousHash": nil},
		{"boardId": "b1", "swimlaneId": "", "listId": "l1", "cardId": nil, "entityType": "list", "entityId": "l1", "group": "lifecycle",
			"changeType": "removed", "previousContent": map[string]any{"deletedAt": nil, "sort": 1.5, "n": []any{1.0, "two", nil, map[string]any{"z": true, "a": false}}},
			"newContent": map[string]any{"deletedAt": at, "batchId": "x"}, "userId": "u1", "createdAt": at,
			"batchId": "x", "restoredFromId": nil, "restoredByUserId": nil, "previousHash": strings.Repeat("a", 64)},
		// A protected field the row has not got: undefined.
		{"boardId": "b2", "entityType": "card", "entityId": "c2", "changeType": "added", "previousContent": nil,
			"newContent": map[string]any{"title": "x", "big": 1e21, "small": 1e-7, "neg": -0.5}, "userId": "u2", "createdAt": at},
	}
}

// jsonForNode is a row as EJSON-ish JSON: dates as {"$date": iso}.
func jsonForNode(v any) any {
	switch x := v.(type) {
	case time.Time:
		return map[string]any{"$date": x.UTC().Format("2006-01-02T15:04:05.000Z")}
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			out[k] = jsonForNode(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = jsonForNode(e)
		}
		return out
	}
	return v
}

// The same hash as WeKan's own changeHistoryIntegrity.js, row by row - run
// by Node over WeKan's file when both are here.
func TestHistoryHashMatchesWeKan(t *testing.T) {
	module, _ := filepath.Abs("../../../../models/lib/changeHistoryIntegrity.js")
	if v := os.Getenv("WEKAN_SRC"); v != "" {
		module = filepath.Join(v, "models/lib/changeHistoryIntegrity.js")
	}
	node, err := exec.LookPath("node")
	if _, statErr := os.Stat(module); err != nil || statErr != nil {
		t.Skip("needs node and WeKan's models/lib/changeHistoryIntegrity.js")
	}
	rows := historyFixtures()
	var in []any
	for _, r := range rows {
		in = append(in, jsonForNode(r))
	}
	raw, _ := json.Marshal(in)
	script := `const m = require(process.argv[1]);
const revive = v => Array.isArray(v) ? v.map(revive) : v && typeof v === 'object'
  ? (Object.keys(v).length === 1 && v.$date ? new Date(v.$date) : Object.fromEntries(Object.entries(v).map(([k, e]) => [k, revive(e)]))) : v;
const rows = revive(JSON.parse(require('fs').readFileSync(0, 'utf8')));
process.stdout.write(JSON.stringify(rows.map(r => { const h = m.hashHistoryRow(r); return [h, m.rowHashIsValid({ ...r, integrityHash: h })]; })));`
	cmd := exec.Command(node, "-e", script, module)
	cmd.Stdin = strings.NewReader(string(raw))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var want [][2]any
	if err := json.Unmarshal(out, &want); err != nil {
		t.Fatalf("node said %s", out)
	}
	for i, r := range rows {
		got := hashHistoryRow(r)
		if got != want[i][0] {
			t.Errorf("row %d: Weddy %s, WeKan %v", i, got, want[i][0])
		}
		r["integrityHash"] = got
		if !rowHashIsValid(r) || want[i][1] != true {
			t.Errorf("row %d: valid in Weddy %v, in WeKan %v", i, rowHashIsValid(r), want[i][1])
		}
	}
}

func TestRowHashIsValid(t *testing.T) {
	r := historyFixtures()[0]
	r["integrityHash"] = hashHistoryRow(r)
	if !rowHashIsValid(r) {
		t.Fatal("its own hash")
	}
	// Negative: any protected field changed fails; an unprotected one does not.
	for _, k := range []string{"boardId", "userId", "previousHash", "changeType"} {
		v := map[string]any{}
		for kk, vv := range r {
			v[kk] = vv
		}
		v[k] = "tampered"
		if rowHashIsValid(v) {
			t.Errorf("negative: %s changed is valid", k)
		}
	}
	c := map[string]any{}
	for kk, vv := range r {
		c[kk] = vv
	}
	c["newContent"] = map[string]any{"title": "Other"}
	if rowHashIsValid(c) {
		t.Error("negative: the content changed is valid")
	}
	c["newContent"], c["undone"] = r["newContent"], true
	if !rowHashIsValid(c) {
		t.Error("undone is not protected")
	}
	if rowHashIsValid(map[string]any{"integrityHash": "nothex"}) {
		t.Error("negative: a malformed hash")
	}
	// #6797: '' hashed, null stored, is accepted; another value is not.
	e := historyFixtures()[1]
	e["integrityHash"] = hashHistoryRow(e)
	e["swimlaneId"] = nil
	if !rowHashIsValid(e) {
		t.Error("'' stored as null")
	}
	e["swimlaneId"] = "other"
	if rowHashIsValid(e) {
		t.Error("negative: '' became a value")
	}
	// A legacy row hashed with an undefined key the stored row lost.
	l := historyFixtures()[0]
	l["previousContent"] = map[string]any{"title": "Old", "gone": jsUndefined{}}
	l["newContent"] = map[string]any{"title": "N", "gone": "x"}
	legacy := protectedHistory(l)
	l["integrityHash"] = sha256Hex(canonical(legacy))
	l["previousContent"] = map[string]any{"title": "Old"}
	if !rowHashIsValid(l) {
		t.Error("a legacy row with a lost undefined key")
	}
}
