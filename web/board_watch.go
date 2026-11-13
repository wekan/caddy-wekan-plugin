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

// openBoards are the boards some page has open, with their pages' channels.
func (h *hub) openBoards() map[string][]chan string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string][]chan string{}
	for b, chans := range h.subs {
		for ch := range chans {
			out[b] = append(out[b], ch)
		}
	}
	return out
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

// openUsers are the signed-in users of the open pages, with their channels.
func (h *hub) openUsers() map[string][]chan string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string][]chan string{}
	for ch, u := range h.owner {
		out[u] = append(out[u], ch)
	}
	return out
}

// liveAs follows the page's user. Each page is compared with the profile it
// was drawn from (drawn, its stamp on the page), else as it is now: one
// baseline per page, not per user - a page drawn after a change already
// shows it, while another of the user's pages, drawn before, does not.
func (s *Server) liveAs(ctx context.Context, userID, drawn string, ch chan string) {
	st := drawn
	if st == "" {
		st = s.userStamp(ctx, userID)
	}
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	s.hub.owner[ch] = userID
	s.hub.seenUsers[ch] = st
}

// watchUsersOnce looks at each open page's user once, and tells each of that
// user's pages whose profile is not the one it shows.
func (s *Server) watchUsersOnce(ctx context.Context) {
	for id, chans := range s.hub.openUsers() {
		st := s.userStamp(ctx, id)
		if st == "" {
			continue
		}
		s.hub.mu.Lock()
		for _, ch := range chans {
			before, open := s.hub.seenUsers[ch]
			if _, still := s.hub.owner[ch]; !still || !open || before == st {
				continue // closed since, or up to date
			}
			s.hub.seenUsers[ch] = st
			select {
			case ch <- "board-changed":
			default:
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

// watchBoardsOnce looks at each open board once and tells each of its pages
// that does not show the board as it is: one baseline per page, so a page
// drawn after a change is never told of it again.
func (s *Server) watchBoardsOnce(ctx context.Context) {
	for id, chans := range s.hub.openBoards() {
		stamp := s.boardStamp(ctx, id)
		if stamp == "" {
			continue
		}
		s.hub.mu.Lock()
		for _, ch := range chans {
			before, ok := s.hub.seen[ch]
			if !ok || !s.hub.subs[id][ch] || before == stamp {
				continue
			}
			s.hub.seen[ch] = stamp
			select {
			case ch <- "board-changed":
			default:
			}
		}
		s.hub.mu.Unlock()
	}
}

// liveSince starts a page's live connection: the page is compared with the
// board it was drawn from (drawn, its stamp on the page), and told at once
// when the board has changed since.
func (s *Server) liveSince(ctx context.Context, board, drawn string, ch chan string) {
	now := s.boardStamp(ctx, board)
	if now == "" {
		return
	}
	s.hub.mu.Lock()
	s.hub.seen[ch] = now
	s.hub.mu.Unlock()
	if drawn != "" && drawn != now {
		select {
		case ch <- "board-changed":
		default:
		}
	}
}
