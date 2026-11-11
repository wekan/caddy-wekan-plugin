package web

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func historyRows(t *testing.T, w *testWeKan, boardID string) []map[string]any {
	cur, err := w.db.Collection("changeHistory").Find(context.Background(), bson.M{"boardId": boardID}, options.Find().SetSort(bson.D{{Key: "createdAt", Value: 1}}))
	if err != nil {
		t.Fatal(err)
	}
	var rows []bson.M
	_ = cur.All(context.Background(), &rows)
	out := []map[string]any{}
	for _, r := range rows {
		out = append(out, plain(r).(map[string]any))
	}
	return out
}

// verifyWithWeKan runs WeKan's own verifyHistoryRows over the rows: the
// failures it finds, by reason. Skipped without node and WeKan's file.
func verifyWithWeKan(t *testing.T, rows []map[string]any) []string {
	t.Helper()
	module, _ := filepath.Abs("../../../../models/lib/changeHistoryIntegrity.js")
	node, err := exec.LookPath("node")
	if _, statErr := os.Stat(module); err != nil || statErr != nil {
		t.Skip("needs node and WeKan's models/lib/changeHistoryIntegrity.js")
	}
	var in []any
	for _, r := range rows {
		in = append(in, jsonForNode(r))
	}
	raw, _ := json.Marshal(in)
	script := `const m = require(process.argv[1]);
const revive = v => Array.isArray(v) ? v.map(revive) : v && typeof v === 'object'
  ? (Object.keys(v).length === 1 && v.$date ? new Date(v.$date) : Object.fromEntries(Object.entries(v).map(([k, e]) => [k, revive(e)]))) : v;
const rows = revive(JSON.parse(require('fs').readFileSync(0, 'utf8')));
process.stdout.write(JSON.stringify(m.verifyHistoryRows(rows).map(f => f.reason)));`
	cmd := exec.Command(node, "-e", script, module)
	cmd.Stdin = strings.NewReader(string(raw))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var reasons []string
	_ = json.Unmarshal(out, &reasons)
	return reasons
}

func change(boardID, entity string) historyChange {
	return historyChange{BoardID: boardID, ListID: "l1", EntityType: "card", EntityID: entity, Group: "title", ChangeType: "edited",
		PreviousContent: map[string]any{"title": "a"}, NewContent: map[string]any{"title": "b"}, UserID: "u1"}
}

// The legacy chain: each row after the tip, WeKan's audit finds nothing, and
// twenty appends at once still make one chain.
func TestHistoryLegacyChain(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if id := w.s.recordHistory(ctx, change("board1", "c"+string(rune('a'+i)))); id == "" {
			t.Fatal("recorded nothing")
		}
	}
	rows := historyRows(t, w, "board1")
	if len(rows) != 5 || rows[0]["previousHash"] != nil {
		t.Fatalf("five rows, the first a root: %d", len(rows))
	}
	for i := 1; i < len(rows); i++ {
		if rows[i]["previousHash"] != rows[i-1]["integrityHash"] || !rowHashIsValid(rows[i]) {
			t.Errorf("row %d does not follow row %d", i, i-1)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w.s.recordHistory(ctx, change("board2", "c"+string(rune('a'+i))))
		}(i)
	}
	wg.Wait()
	seen := map[any]bool{}
	for _, r := range historyRows(t, w, "board2") {
		if seen[r["previousHash"]] {
			t.Errorf("negative: a fork at %v", r["previousHash"])
		}
		seen[r["previousHash"]] = true
	}
	if len(seen) != 20 {
		t.Errorf("twenty rows: %d", len(seen))
	}
	for _, b := range []string{"board1", "board2"} {
		if f := verifyWithWeKan(t, historyRows(t, w, b)); len(f) != 0 {
			t.Errorf("WeKan's audit of %s: %v", b, f)
		}
	}
	// The gate is left empty and no lease stays.
	g, _ := w.s.readHistoryGate(ctx, "board1")
	if g == nil || g.Mode != "legacy" || len(g.Writers) != 0 {
		t.Errorf("the gate: %+v", g)
	}
	if n, _ := w.db.Collection("historyWriterLeases").CountDocuments(ctx, bson.M{}); n != 0 {
		t.Errorf("leases left: %d", n)
	}
	// A new change marks the user's undone rows of the board superseded.
	_, _ = w.db.Collection("changeHistory").UpdateOne(ctx, bson.M{"_id": rows[4]["_id"]}, bson.M{"$set": bson.M{"undone": true}})
	w.s.recordHistory(ctx, change("board1", "cz"))
	var r bson.M
	_ = w.db.Collection("changeHistory").FindOne(ctx, bson.M{"_id": rows[4]["_id"]}).Decode(&r)
	if r["superseded"] != true {
		t.Error("the redo stack is cleared")
	}
}

// A board being migrated takes no writer; a coordinated board is written
// through its head, and a pending row a writer left is finished first.
func TestHistoryGateModes(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.s.recordHistory(ctx, change("b3", "c1"))
	g, _ := w.s.readHistoryGate(ctx, "b3")
	mig := uuid.NewString()
	_, _ = w.db.Collection("historyWriterGates").UpdateOne(ctx, bson.M{"_id": g.ID}, bson.M{"$set": bson.M{"mode": "migrating", "migrationId": mig}})
	if id := w.s.recordHistory(ctx, change("b3", "c2")); id != "" {
		t.Error("negative: written while migrating")
	}
	// WeKan's migration installs the head at the chain's tip.
	rows := historyRows(t, w, "b3")
	tip := rows[len(rows)-1]["integrityHash"]
	_, _ = w.db.Collection("historyChainHeads").InsertOne(ctx, bson.D{{Key: "_id", Value: historyChainHeadID("b3")}, {Key: "boardId", Value: "b3"},
		{Key: "version", Value: int32(1)}, {Key: "hash", Value: tip}, {Key: "pending", Value: nil}, {Key: "pendingHash", Value: nil}})
	_, _ = w.db.Collection("historyWriterGates").UpdateOne(ctx, bson.M{"_id": g.ID}, bson.M{"$set": bson.M{"mode": "coordinated"}})
	if id := w.s.recordHistory(ctx, change("b3", "c3")); id == "" {
		t.Fatal("the coordinated board takes the row")
	}
	head, _ := w.s.readHistoryHead(ctx, "b3")
	rows = historyRows(t, w, "b3")
	if head == nil || head.pending != nil || head.hash != rows[len(rows)-1]["integrityHash"] || rows[len(rows)-1]["previousHash"] != tip {
		t.Fatalf("the head moved on to the new row")
	}
	// A writer that died after reserving: the pending row is finished first.
	pending := bson.D{{Key: "_id", Value: "deadwriterrow0001"}, {Key: "boardId", Value: "b3"}, {Key: "swimlaneId", Value: nil}, {Key: "listId", Value: nil},
		{Key: "cardId", Value: nil}, {Key: "entityType", Value: "card"}, {Key: "entityId", Value: "cx"}, {Key: "group", Value: nil}, {Key: "changeType", Value: "added"},
		{Key: "previousContent", Value: nil}, {Key: "newContent", Value: nil}, {Key: "userId", Value: "u9"}, {Key: "undone", Value: false}, {Key: "undoneAt", Value: nil},
		{Key: "isCheckpoint", Value: false}, {Key: "batchId", Value: nil}, {Key: "restoredFromId", Value: nil}, {Key: "restoredByUserId", Value: nil},
		{Key: "createdAt", Value: time.Now().UTC().Truncate(time.Millisecond)}, {Key: "superseded", Value: false}, {Key: "previousHash", Value: head.hash}}
	pending = append(pending, bson.E{Key: "integrityHash", Value: hashHistoryRow(plain(pending).(map[string]any))})
	_, _ = w.db.Collection("historyChainHeads").UpdateOne(ctx, bson.M{"_id": historyChainHeadID("b3")},
		bson.M{"$set": bson.M{"pending": pending, "pendingHash": sha256Hex(canonical(plain(pending)))}})
	if id := w.s.recordHistory(ctx, change("b3", "c4")); id == "" {
		t.Fatal("after a left pending row")
	}
	rows = historyRows(t, w, "b3")
	byID := map[any]map[string]any{}
	for _, r := range rows {
		byID[r["_id"]] = r
	}
	dead := byID["deadwriterrow0001"]
	last := rows[len(rows)-1]
	if dead == nil || last["previousHash"] != dead["integrityHash"] {
		t.Errorf("the pending row is stored, and the new row follows it")
	}
	if f := verifyWithWeKan(t, rows); len(f) != 0 {
		t.Errorf("WeKan's audit: %v", f)
	}
}

// Recovery: an expired writer's lease is fenced, its claimed row id can no
// longer be inserted, its token leaves; a live one is refused.
func TestHistoryWriterRecovery(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	g, _ := w.s.historyGate(ctx, "b4")
	dead, live := uuid.NewString(), uuid.NewString()
	claimed := "claimedrow000001"
	_, _ = w.db.Collection("historyWriterLeases").InsertOne(ctx, bson.D{{Key: "_id", Value: dead}, {Key: "boardId", Value: "b4"}, {Key: "version", Value: int32(1)},
		{Key: "state", Value: "active"}, {Key: "expiresAt", Value: time.Now().Add(-time.Hour)}, {Key: "pending", Value: claimed}})
	_, _ = w.db.Collection("historyWriterLeases").InsertOne(ctx, bson.D{{Key: "_id", Value: live}, {Key: "boardId", Value: "b4"}, {Key: "version", Value: int32(1)},
		{Key: "state", Value: "active"}, {Key: "expiresAt", Value: time.Now().Add(time.Hour)}, {Key: "pending", Value: nil}})
	g, _ = w.s.changeHistoryGate(ctx, g, []string{dead, live})
	if ok, err := w.s.recoverHistoryWriter(ctx, "b4", dead, time.Second); !ok || err != nil {
		t.Fatalf("the dead writer is taken: %v %v", ok, err)
	}
	if _, err := w.db.Collection("changeHistory").InsertOne(ctx, bson.M{"_id": claimed, "boardId": "b4"}); err == nil {
		t.Error("negative: its claimed row can be inserted late")
	}
	if _, err := w.s.recoverHistoryWriter(ctx, "b4", live, time.Second); err == nil || !strings.Contains(err.Error(), "writer-alive") {
		t.Errorf("negative: a live writer: %v", err)
	}
	g, _ = w.s.readHistoryGate(ctx, "b4")
	if len(g.Writers) != 1 || g.Writers[0] != live {
		t.Errorf("only the live writer stays: %v", g.Writers)
	}
}
