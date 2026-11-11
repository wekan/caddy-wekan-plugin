package web

// Appending a row to a board's change history, as WeKan does it - so that
// WeKan's servers and Weddy writing the same board never fork its chain:
//
//   - server/lib/historyWriterGate.js: every board has a gate. On a legacy
//     board a writer joins it with a token and a renewed lease, claims each
//     row id in the lease before inserting it (so recovery can fence a
//     writer that died mid-insert), and leaves it after; a board being
//     migrated takes no writer; a coordinated board is written through its
//     chain head instead.
//   - models/lib/historyLegacyAppend.js: the legacy chain - each row after
//     the chain's tip, the newest hashed row and then its successors; this
//     server's appends to one board take turns.
//   - server/lib/historyChainAppend.js: the coordinated chain - a head per
//     board whose single pending row is the compare-and-swap token, so no
//     two writers ever give one row two successors.
//
// Weddy never migrates a board (WeKan's administrator does): it writes the
// mode it finds. A writer that cannot get in records nothing - history is
// best effort, a change never fails for it (History.md §5).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const historyFenceBoard = "#history-writer-fence"

var errHistoryWriter = errors.New("history-writer")

func historyFail(code string) error { return fmt.Errorf("%w-%s", errHistoryWriter, code) }

// historyLeaseMs is HISTORY_WRITER_LEASE_MS (1000 to 600000, 30000).
func historyLeaseMs() time.Duration {
	if n, err := strconv.Atoi(os.Getenv("HISTORY_WRITER_LEASE_MS")); err == nil && n >= 1000 && n <= 600000 {
		return time.Duration(n) * time.Millisecond
	}
	return 30 * time.Second
}

// historyGate is a board's writer gate, as stored.
type historyGate struct {
	ID          string   `bson:"_id"`
	BoardID     string   `bson:"boardId"`
	Version     int32    `bson:"version"`
	Mode        string   `bson:"mode"`
	MigrationID any      `bson:"migrationId"`
	Writers     []string `bson:"writers"`
}

func historyGateID(boardID string) string {
	return sha256Hex(canonical([]any{"history-writer-gate", boardID}))
}

func (s *Server) readHistoryGate(ctx context.Context, boardID string) (*historyGate, error) {
	var raw bson.M
	err := s.db.Collection("historyWriterGates").FindOne(ctx, bson.M{"_id": eq(historyGateID(boardID))}).Decode(&raw)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	keys := []string{}
	for k := range raw {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var g historyGate
	b, _ := bson.Marshal(raw)
	if fmt.Sprint(keys) != "[_id boardId migrationId mode version writers]" || bson.Unmarshal(b, &g) != nil || g.Version != 1 ||
		!slices.Contains([]string{"legacy", "draining", "migrating", "coordinated"}, g.Mode) {
		return nil, historyFail("gate-invalid")
	}
	return &g, nil
}

func (s *Server) historyGate(ctx context.Context, boardID string) (*historyGate, error) {
	g, err := s.readHistoryGate(ctx, boardID)
	if err != nil || g != nil {
		return g, err
	}
	_, ierr := s.db.Collection("historyWriterGates").InsertOne(ctx, bson.D{{Key: "_id", Value: historyGateID(boardID)}, {Key: "boardId", Value: boardID},
		{Key: "version", Value: int32(1)}, {Key: "mode", Value: "legacy"}, {Key: "migrationId", Value: nil}, {Key: "writers", Value: bson.A{}}})
	if g, err = s.readHistoryGate(ctx, boardID); g == nil && err == nil {
		return nil, errors.Join(ierr, historyFail("gate-unconfirmed"))
	}
	return g, err
}

// changeHistoryGate is the gate's compare-and-swap: before as read, to after.
func (s *Server) changeHistoryGate(ctx context.Context, before *historyGate, writers []string) (*historyGate, error) {
	ws := bson.A{}
	for _, w := range before.Writers {
		ws = append(ws, w)
	}
	after := bson.A{}
	for _, w := range writers {
		after = append(after, w)
	}
	_, _ = s.db.Collection("historyWriterGates").ReplaceOne(ctx,
		bson.M{"_id": before.ID, "boardId": before.BoardID, "version": 1, "mode": before.Mode, "migrationId": before.MigrationID, "writers": bson.M{"$eq": ws}},
		bson.D{{Key: "_id", Value: before.ID}, {Key: "boardId", Value: before.BoardID}, {Key: "version", Value: int32(1)},
			{Key: "mode", Value: before.Mode}, {Key: "migrationId", Value: before.MigrationID}, {Key: "writers", Value: after}})
	g, err := s.readHistoryGate(ctx, before.BoardID)
	if err == nil && g == nil {
		err = historyFail("gate-unconfirmed")
	}
	return g, err
}

// historyLease is a writer's lease (version 1).
type historyLease struct {
	ID        string    `bson:"_id"`
	BoardID   any       `bson:"boardId"`
	Version   int32     `bson:"version"`
	State     string    `bson:"state"`
	ExpiresAt time.Time `bson:"expiresAt"`
	Pending   *string   `bson:"pending"`
}

func (s *Server) readHistoryLease(ctx context.Context, id string) (*historyLease, error) {
	var l historyLease
	err := s.db.Collection("historyWriterLeases").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&l)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil || l.Version != 1 || (l.State != "active" && l.State != "fenced") {
		return nil, historyFail("lease-invalid")
	}
	return &l, nil
}

// fenceHistoryRow makes rowID impossible to insert: a tombstone holds it,
// unless the row is there already.
func (s *Server) fenceHistoryRow(ctx context.Context, rowID, writer string) error {
	_, err := s.db.Collection("changeHistory").InsertOne(ctx, bson.M{"_id": rowID, "boardId": historyFenceBoard, "fencedWriter": writer})
	if err != nil && s.db.Collection("changeHistory").FindOne(ctx, bson.M{"_id": eq(rowID)}).Err() != nil {
		return err
	}
	return nil
}

// recoverHistoryWriter takes one abandoned writer of a legacy board: its
// lease fenced, a row it had claimed fenced, its token removed. A lease
// still alive is refused (writer-alive).
func (s *Server) recoverHistoryWriter(ctx context.Context, boardID string, writer string, grace time.Duration) (bool, error) {
	g, err := s.readHistoryGate(ctx, boardID)
	if err != nil || g == nil || (g.Mode != "legacy" && g.Mode != "draining") {
		return false, errors.Join(err, historyFail("recovery-state"))
	}
	l, err := s.readHistoryLease(ctx, writer)
	if err != nil {
		return false, err
	}
	leases := s.db.Collection("historyWriterLeases")
	alive := func(l *historyLease) bool { return l.State == "active" && l.ExpiresAt.Add(grace).After(time.Now()) }
	if !slices.Contains(g.Writers, writer) {
		if l != nil {
			if alive(l) {
				return false, historyFail("writer-alive")
			}
			if l.Pending != nil {
				if err := s.fenceHistoryRow(ctx, *l.Pending, writer); err != nil {
					return false, err
				}
			}
			_, _ = leases.DeleteOne(ctx, bson.M{"_id": eq(writer), "boardId": eq(boardID)})
		}
		return false, nil
	}
	if l == nil {
		return false, historyFail("lease-missing")
	}
	if l.State == "active" {
		if alive(l) {
			return false, historyFail("writer-alive")
		}
		_, _ = leases.UpdateOne(ctx, bson.M{"_id": eq(writer), "boardId": eq(boardID), "state": "active", "expiresAt": l.ExpiresAt, "pending": l.Pending},
			bson.M{"$set": bson.M{"state": "fenced"}})
		if l, err = s.readHistoryLease(ctx, writer); err != nil || l == nil || l.State != "fenced" {
			return false, errors.Join(err, historyFail("writer-alive"))
		}
	}
	if l.Pending != nil {
		if err := s.fenceHistoryRow(ctx, *l.Pending, writer); err != nil {
			return false, err
		}
	}
	for i := 0; i < 100; i++ {
		if g, err = s.readHistoryGate(ctx, boardID); err != nil || g == nil || (g.Mode != "legacy" && g.Mode != "draining") {
			return false, errors.Join(err, historyFail("recovery-state"))
		}
		if !slices.Contains(g.Writers, writer) {
			_, _ = leases.DeleteOne(ctx, bson.M{"_id": eq(writer), "boardId": eq(boardID), "state": "fenced"})
			return true, nil
		}
		_, _ = s.changeHistoryGate(ctx, g, slices.DeleteFunc(slices.Clone(g.Writers), func(w string) bool { return w == writer }))
	}
	return false, historyFail("contention")
}

// recoverExpiredHistoryWriters takes every token and orphaned lease of a
// board that can be taken online.
func (s *Server) recoverExpiredHistoryWriters(ctx context.Context, boardID string, grace time.Duration) {
	g, err := s.readHistoryGate(ctx, boardID)
	if err != nil || g == nil || (g.Mode != "legacy" && g.Mode != "draining") {
		return
	}
	ids := slices.Clone(g.Writers)
	// The board's orphaned leases too: those whose token is not in the gate.
	cur, err := s.db.Collection("historyWriterLeases").Find(ctx, bson.M{"boardId": eq(boardID)}, options.Find().SetLimit(2000))
	if err == nil {
		var ls []historyLease
		_ = cur.All(ctx, &ls)
		for _, l := range ls {
			if !slices.Contains(g.Writers, l.ID) {
				ids = append(ids, l.ID)
			}
		}
	}
	for _, id := range ids {
		if uuid.Validate(id) == nil {
			_, _ = s.recoverHistoryWriter(ctx, boardID, id, grace)
		}
	}
}

// historyLegacyQueue: this server's appends to one board, one at a time.
var historyLegacyQueue sync.Map

func boardHistoryLock(boardID string) *sync.Mutex {
	m, _ := historyLegacyQueue.LoadOrStore(boardID, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// historyChainTip is the legacy chain's last row: the newest hashed row,
// then its successors.
func (s *Server) historyChainTip(ctx context.Context, boardID string) (bson.M, error) {
	coll := s.db.Collection("changeHistory")
	var tip bson.M
	err := coll.FindOne(ctx, bson.M{"boardId": eq(boardID), "integrityHash": bson.M{"$nin": bson.A{nil, ""}}},
		options.FindOne().SetSort(bson.D{{Key: "createdAt", Value: -1}})).Decode(&tip)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for steps := 0; steps < 10000; steps++ {
		var next bson.M
		if coll.FindOne(ctx, bson.M{"boardId": eq(boardID), "previousHash": tip["integrityHash"]}).Decode(&next) != nil {
			break
		}
		tip = next
	}
	return tip, nil
}

// withHistoryWriter is WeKan's: a coordinated board through its head, a
// legacy one as a gate writer with a lease, a migrating one not at all.
func (s *Server) withHistoryWriter(ctx context.Context, boardID string, writeLegacy func(fencedInsert func(rowID string, insert func() error) error) error, writeCoordinated func() error) error {
	g, err := s.historyGate(ctx, boardID)
	if err != nil {
		return err
	}
	if g.Mode == "coordinated" {
		return writeCoordinated()
	}
	leaseFor := historyLeaseMs()
	writer := uuid.NewString()
	leases := s.db.Collection("historyWriterLeases")
	expiry := func() time.Time { return time.Now().UTC().Add(leaseFor).Truncate(time.Millisecond) }
	if _, err := leases.InsertOne(ctx, bson.D{{Key: "_id", Value: writer}, {Key: "boardId", Value: boardID}, {Key: "version", Value: int32(1)},
		{Key: "state", Value: "active"}, {Key: "expiresAt", Value: expiry()}, {Key: "pending", Value: nil}}); err != nil {
		return err
	}
	owned, recovered := false, false
	for i := 0; i < 100 && !owned; i++ {
		if g.Mode == "coordinated" {
			_, _ = leases.DeleteOne(ctx, bson.M{"_id": eq(writer), "boardId": eq(boardID), "state": "active"})
			return writeCoordinated()
		}
		if g.Mode != "legacy" {
			_, _ = leases.DeleteOne(ctx, bson.M{"_id": eq(writer), "boardId": eq(boardID), "state": "active", "pending": nil})
			return historyFail("migration-busy")
		}
		if len(g.Writers) >= 1000 {
			if recovered {
				_, _ = leases.DeleteOne(ctx, bson.M{"_id": eq(writer), "boardId": eq(boardID), "state": "active", "pending": nil})
				return historyFail("capacity")
			}
			recovered = true
			s.recoverExpiredHistoryWriters(ctx, boardID, leaseFor)
			if g, err = s.readHistoryGate(ctx, boardID); err != nil || g == nil {
				return errors.Join(err, historyFail("gate-invalid"))
			}
			continue
		}
		if g, err = s.changeHistoryGate(ctx, g, append(slices.Clone(g.Writers), writer)); err != nil {
			_, _ = leases.DeleteOne(ctx, bson.M{"_id": eq(writer), "boardId": eq(boardID), "state": "active", "pending": nil})
			return err
		}
		owned = slices.Contains(g.Writers, writer)
	}
	if !owned {
		_, _ = leases.DeleteOne(ctx, bson.M{"_id": eq(writer), "boardId": eq(boardID), "state": "active", "pending": nil})
		return historyFail("contention")
	}
	var lost bool
	var mu sync.Mutex
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(max(250*time.Millisecond, leaseFor/3))
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				res, err := leases.UpdateOne(context.WithoutCancel(ctx), bson.M{"_id": eq(writer), "boardId": eq(boardID), "state": "active"}, bson.M{"$set": bson.M{"expiresAt": expiry()}})
				if err == nil && res.MatchedCount == 0 {
					mu.Lock()
					lost = true
					mu.Unlock()
				}
			}
		}
	}()
	assertCurrent := func() error {
		mu.Lock()
		l := lost
		mu.Unlock()
		if l {
			return historyFail("ownership-lost")
		}
		cur, err := s.readHistoryGate(ctx, boardID)
		if err != nil || cur == nil || (cur.Mode != "legacy" && cur.Mode != "draining") || !slices.Contains(cur.Writers, writer) {
			return errors.Join(err, historyFail("ownership-lost"))
		}
		return nil
	}
	fencedInsert := func(rowID string, insert func() error) error {
		if rowID == "" || len(rowID) > 128 {
			return historyFail("invalid")
		}
		_, _ = leases.UpdateOne(ctx, bson.M{"_id": eq(writer), "boardId": eq(boardID), "state": "active", "pending": nil}, bson.M{"$set": bson.M{"pending": rowID}})
		if l, err := s.readHistoryLease(ctx, writer); err != nil || l == nil || l.State != "active" || l.Pending == nil || *l.Pending != rowID {
			return errors.Join(err, historyFail("ownership-lost"))
		}
		if err := assertCurrent(); err != nil {
			return err
		}
		// A failed insert keeps its claim: recovery fences that row id.
		if err := insert(); err != nil {
			return err
		}
		_, _ = leases.UpdateOne(ctx, bson.M{"_id": eq(writer), "boardId": eq(boardID), "state": "active", "pending": rowID}, bson.M{"$set": bson.M{"pending": nil}})
		if l, err := s.readHistoryLease(ctx, writer); err != nil || l == nil || l.State != "active" || l.Pending != nil {
			return errors.Join(err, historyFail("ownership-lost"))
		}
		return nil
	}
	werr := assertCurrent()
	if werr == nil {
		werr = writeLegacy(fencedInsert)
	}
	if werr == nil {
		werr = assertCurrent()
	}
	close(stop)
	if werr != nil {
		// The token and the lease stay; the lease expires, recovery takes it.
		return werr
	}
	for i := 0; i < 100; i++ {
		g, err := s.readHistoryGate(ctx, boardID)
		if err != nil || g == nil {
			return errors.Join(err, historyFail("ownership-lost"))
		}
		if !slices.Contains(g.Writers, writer) {
			break
		}
		g, _ = s.changeHistoryGate(ctx, g, slices.DeleteFunc(slices.Clone(g.Writers), func(w string) bool { return w == writer }))
		if g != nil && !slices.Contains(g.Writers, writer) {
			break
		}
	}
	if l, _ := s.readHistoryLease(ctx, writer); l != nil && l.Pending != nil {
		_ = s.fenceHistoryRow(ctx, *l.Pending, writer)
	}
	_, _ = leases.DeleteOne(ctx, bson.M{"_id": eq(writer), "boardId": eq(boardID)})
	return nil
}

// historyChainHeadID is WeKan's historyChainId.
func historyChainHeadID(boardID string) string {
	return sha256Hex(canonical([]any{"history-chain", boardID}))
}

// historyHead is a coordinated board's head; Pending keeps the stored order,
// the compare-and-swap's token.
type historyHead struct {
	raw     bson.D
	hash    any
	pending bson.D
}

func (s *Server) readHistoryHead(ctx context.Context, boardID string) (*historyHead, error) {
	var raw bson.D
	err := s.db.Collection("historyChainHeads").FindOne(ctx, bson.M{"_id": eq(historyChainHeadID(boardID))}).Decode(&raw)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	keys := []string{}
	for _, e := range raw {
		m[e.Key] = e.Value
		keys = append(keys, e.Key)
	}
	slices.Sort(keys)
	h := &historyHead{raw: raw, hash: m["hash"]}
	if fmt.Sprint(keys) != "[_id boardId hash pending pendingHash version]" || m["_id"] != historyChainHeadID(boardID) ||
		m["boardId"] != boardID {
		return nil, historyFail("head-invalid")
	}
	if v, _ := safeInt(m["version"]); v != 1 {
		return nil, historyFail("head-invalid")
	}
	if hs, ok := h.hash.(string); h.hash != nil && (!ok || !hashPattern.MatchString(hs)) {
		return nil, historyFail("head-invalid")
	}
	if p, ok := m["pending"].(bson.D); ok {
		h.pending = p
		pm := plain(p).(map[string]any)
		if m["pendingHash"] != sha256Hex(canonical(pm)) || !rowHashIsValid(pm) || pm["previousHash"] != h.hash {
			return nil, historyFail("head-invalid")
		}
	} else if m["pending"] != nil || m["pendingHash"] != nil {
		return nil, historyFail("head-invalid")
	}
	return h, nil
}

// appendCoordinatedHistory is appendHistoryChain on a coordinated board:
// reserve the row as the head's pending, store it, and move the head on.
func (s *Server) appendCoordinatedHistory(ctx context.Context, row bson.D) error {
	rowID := ""
	var boardID string
	for _, e := range row {
		switch e.Key {
		case "_id":
			rowID, _ = e.Value.(string)
		case "boardId":
			boardID, _ = e.Value.(string)
		}
	}
	heads, history := s.db.Collection("historyChainHeads"), s.db.Collection("changeHistory")
	candidateFor := func(previous any) bson.D {
		c := append(slices.Clone(row), bson.E{Key: "previousHash", Value: previous})
		return append(c, bson.E{Key: "integrityHash", Value: hashHistoryRow(plain(c).(map[string]any))})
	}
	stored := func(pending bson.D) (bool, error) {
		want := plain(pending).(map[string]any)
		var saved bson.M
		if err := history.FindOne(ctx, bson.M{"_id": want["_id"]}).Decode(&saved); err != nil {
			return false, nil
		}
		if saved["integrityHash"] != want["integrityHash"] || !rowHashIsValid(plain(saved).(map[string]any)) {
			return false, historyFail("row-conflict")
		}
		return true, nil
	}
	head, err := s.readHistoryHead(ctx, boardID)
	if err != nil {
		return err
	}
	if head == nil {
		return historyFail("not-initialized") // a head is installed by WeKan's migration, never here
	}
	for attempt := 0; attempt < 100; attempt++ {
		if head.pending != nil {
			ok, err := stored(head.pending)
			if err != nil {
				return err
			}
			if !ok {
				_, ierr := history.InsertOne(ctx, head.pending)
				if ok, err = stored(head.pending); err != nil || !ok {
					return errors.Join(ierr, err, historyFail("row-unconfirmed"))
				}
			}
			pm := plain(head.pending).(map[string]any)
			next := bson.D{}
			for _, e := range head.raw {
				switch e.Key {
				case "hash":
					next = append(next, bson.E{Key: "hash", Value: pm["integrityHash"]})
				case "pending", "pendingHash":
					next = append(next, bson.E{Key: e.Key, Value: nil})
				default:
					next = append(next, e)
				}
			}
			_, _ = heads.ReplaceOne(ctx, bson.M{"_id": eq(historyChainHeadID(boardID)), "version": 1, "hash": head.hash, "pending": bson.M{"$eq": head.pending}}, next)
			nh, err := s.readHistoryHead(ctx, boardID)
			if err != nil || nh == nil || canonical(plain(nh.raw)) == canonical(plain(head.raw)) {
				return errors.Join(err, historyFail("head-unconfirmed"))
			}
			head = nh
			continue
		}
		var existing bson.M
		if history.FindOne(ctx, bson.M{"_id": eq(rowID)}).Decode(&existing) == nil {
			if !rowHashIsValid(plain(existing).(map[string]any)) {
				return historyFail("row-conflict")
			}
			return nil
		}
		pending := candidateFor(head.hash)
		next := bson.D{}
		for _, e := range head.raw {
			switch e.Key {
			case "pending":
				next = append(next, bson.E{Key: "pending", Value: pending})
			case "pendingHash":
				next = append(next, bson.E{Key: "pendingHash", Value: sha256Hex(canonical(plain(pending)))})
			default:
				next = append(next, e)
			}
		}
		_, _ = heads.ReplaceOne(ctx, bson.M{"_id": eq(historyChainHeadID(boardID)), "version": 1, "hash": head.hash, "pending": nil, "pendingHash": nil}, next)
		nh, err := s.readHistoryHead(ctx, boardID)
		if err != nil || nh == nil || canonical(plain(nh.raw)) == canonical(plain(head.raw)) {
			return errors.Join(err, historyFail("reservation-unconfirmed"))
		}
		head = nh
	}
	return historyFail("contention")
}

// historyEntityTypes and historyChangeTypes are WeKan's allowed values.
var (
	historyEntityTypes = []string{"card", "list", "swimlane", "board", "checklist", "checklistItem", "comment", "attachment", "customField", "rule", "scrum"}
	historyChangeTypes = []string{"added", "removed", "edited", "moved", "restored"}
)

// historyChange is one change to record (ChangeHistory.record's options).
type historyChange struct {
	BoardID                                   string
	SwimlaneID, ListID, CardID                any
	EntityType, EntityID, Group, ChangeType   string
	PreviousContent, NewContent               any
	UserID                                    string
	BatchID, RestoredFromID, RestoredByUserID any
	IsCheckpoint                              bool
}

// recordHistory is ChangeHistory.record: one row after the board's chain's
// tip, the user's redo stack on the board cleared by a new change. Best
// effort: "" and no error to the change it describes.
func (s *Server) recordHistory(ctx context.Context, c historyChange) string {
	if c.BoardID == "" || c.UserID == "" || c.EntityID == "" || !slices.Contains(historyEntityTypes, c.EntityType) || !slices.Contains(historyChangeTypes, c.ChangeType) {
		return ""
	}
	orNil := func(v any) any {
		if v == "" {
			return nil
		}
		return v
	}
	group := any(nil)
	if c.Group != "" {
		group = c.Group
	}
	id := randomID()
	row := bson.D{{Key: "_id", Value: id}, {Key: "boardId", Value: c.BoardID}, {Key: "swimlaneId", Value: c.SwimlaneID}, {Key: "listId", Value: c.ListID},
		{Key: "cardId", Value: orNil(c.CardID)}, {Key: "entityType", Value: c.EntityType}, {Key: "entityId", Value: c.EntityID}, {Key: "group", Value: group},
		{Key: "changeType", Value: c.ChangeType}, {Key: "previousContent", Value: c.PreviousContent}, {Key: "newContent", Value: c.NewContent},
		{Key: "userId", Value: c.UserID}, {Key: "undone", Value: false}, {Key: "undoneAt", Value: nil}, {Key: "isCheckpoint", Value: c.IsCheckpoint},
		{Key: "batchId", Value: c.BatchID}, {Key: "restoredFromId", Value: c.RestoredFromID}, {Key: "restoredByUserId", Value: c.RestoredByUserID},
		{Key: "createdAt", Value: time.Now().UTC().Truncate(time.Millisecond)}, {Key: "superseded", Value: false}}
	clearRedo := func() {
		if c.ChangeType != "restored" && !c.IsCheckpoint {
			_, _ = s.db.Collection("changeHistory").UpdateMany(ctx, bson.M{"userId": eq(c.UserID), "boardId": eq(c.BoardID), "undone": true},
				bson.M{"$set": bson.M{"superseded": true}})
		}
	}
	err := s.withHistoryWriter(ctx, c.BoardID,
		func(fencedInsert func(string, func() error) error) error {
			clearRedo()
			mu := boardHistoryLock(c.BoardID)
			mu.Lock()
			defer mu.Unlock()
			tip, err := s.historyChainTip(ctx, c.BoardID)
			if err != nil {
				return err
			}
			var previous any
			if tip != nil {
				previous = tip["integrityHash"]
			}
			saved := append(slices.Clone(row), bson.E{Key: "previousHash", Value: previous})
			saved = append(saved, bson.E{Key: "integrityHash", Value: hashHistoryRow(plain(saved).(map[string]any))})
			return fencedInsert(id, func() error {
				_, err := s.db.Collection("changeHistory").InsertOne(ctx, saved)
				return err
			})
		},
		func() error {
			clearRedo()
			return s.appendCoordinatedHistory(ctx, row)
		})
	if err != nil {
		s.log.Warn("changeHistory: failed to record a change", "error", err.Error())
		return ""
	}
	return id
}
