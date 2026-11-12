package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// A board changed by something other than Weddy - another program on the
// database, an administrator's mongosh, WeKan's own tests - redraws on the
// pages that have it open, as WeKan's reactive pages do. Weddy's own writes
// announce themselves (hub.publish), keeping the board's stamp as they left
// it; for the rest, each board some page has open is read again every
// boardWatchEvery, one query per board however many pages show it, and
// announced when its document is not what it was.
// The page carries the board's stamp as it was drawn (boardStamp), and its
// live connection is told at once when the board is no longer that, so a
// change between drawing the page and connecting is not lost either.

const boardWatchEvery = 2 * time.Second

// boardStamp is the board document's stamp, "" when there is none.
func (s *Server) boardStamp(ctx context.Context, id string) string {
	raw, err := s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(id)}).Raw()
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:12])
}

// openBoards are the boards some page has open; a board nobody has open is
// forgotten, there being nothing to compare with.
func (h *hub) openBoards() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for b, chans := range h.subs {
		if len(chans) > 0 {
			out = append(out, b)
		}
	}
	for b := range h.seen {
		if len(h.subs[b]) == 0 {
			delete(h.seen, b)
		}
	}
	return out
}

// sawBoard keeps the board's stamp, saying what it was before.
func (h *hub) sawBoard(board, stamp string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	before, had := h.seen[board]
	h.seen[board] = stamp
	return before, had
}

func (s *Server) runBoardWatch(stop chan struct{}) {
	t := time.NewTicker(boardWatchEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if !s.boardWatchOff.Load() {
				s.watchBoardsOnce(context.Background())
			}
		}
	}
}

// watchBoardsOnce looks at each open board once and announces each that is
// not what the last look, or Weddy's own last announcement, left it.
func (s *Server) watchBoardsOnce(ctx context.Context) {
	for _, id := range s.hub.openBoards() {
		stamp := s.boardStamp(ctx, id)
		if stamp == "" {
			continue
		}
		if before, had := s.hub.sawBoard(id, stamp); had && before != stamp {
			s.hub.publish(id, "board-changed")
		}
	}
}

// liveSince starts a page's live connection: the board as it is now is the
// watch's to compare with, unless another page's look already is - that one
// may not have been announced yet - and when the page was drawn from another
// board than this (its drawn stamp), the page is told at once.
func (s *Server) liveSince(ctx context.Context, board, drawn string, ch chan string) {
	now := s.boardStamp(ctx, board)
	if now == "" {
		return
	}
	s.hub.mu.Lock()
	if _, had := s.hub.seen[board]; !had {
		s.hub.seen[board] = now
	}
	s.hub.mu.Unlock()
	if drawn != "" && drawn != now {
		select {
		case ch <- "board-changed":
		default:
		}
	}
}
