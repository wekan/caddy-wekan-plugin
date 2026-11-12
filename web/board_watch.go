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
// The signed-in user's own profile is followed the same way, for that user's
// pages only - a preference changed elsewhere (drag handles, a fold, a
// language) redraws them, as WeKan's reactive user does.
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

// userStamp is the stamp of a user's profile, its notifications left out:
// they change on their own and redraw nothing of the board.
func (s *Server) userStamp(ctx context.Context, id string) string {
	// In the stored order (bson.D): a map's order is Go's, random, and
	// would stamp the same profile differently each look.
	var doc struct {
		Profile bson.D `bson:"profile"`
	}
	if s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&doc) != nil {
		return ""
	}
	kept := bson.D{}
	for _, e := range doc.Profile {
		if e.Key != "notifications" {
			kept = append(kept, e)
		}
	}
	raw, err := bson.Marshal(kept)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:12])
}

// openUsers are the signed-in users of the open pages, with their channels;
// a user with none is forgotten.
func (h *hub) openUsers() map[string][]chan string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string][]chan string{}
	for ch, u := range h.owner {
		out[u] = append(out[u], ch)
	}
	for u := range h.seenUsers {
		if _, ok := out[u]; !ok {
			delete(h.seenUsers, u)
		}
	}
	return out
}

// liveAs follows the page's user: its profile as it is now is what the
// watch compares with, unless another of its pages' look already is.
func (s *Server) liveAs(ctx context.Context, userID string, ch chan string) {
	st := s.userStamp(ctx, userID)
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	s.hub.owner[ch] = userID
	if _, had := s.hub.seenUsers[userID]; !had && st != "" {
		s.hub.seenUsers[userID] = st
	}
}

// watchUsersOnce looks at each open page's user once, and tells that user's
// pages when the profile is not what it was.
func (s *Server) watchUsersOnce(ctx context.Context) {
	for id, chans := range s.hub.openUsers() {
		st := s.userStamp(ctx, id)
		if st == "" {
			continue
		}
		s.hub.mu.Lock()
		before, had := s.hub.seenUsers[id]
		s.hub.seenUsers[id] = st
		changed := had && before != st
		if changed {
			for _, ch := range chans {
				if _, open := s.hub.owner[ch]; !open {
					continue // closed since
				}
				select {
				case ch <- "board-changed":
				default:
				}
			}
		}
		s.hub.mu.Unlock()
	}
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
				s.watchUsersOnce(context.Background())
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
