package web

import (
	"context"
	"net/url"
	"sort"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// WeKan's cardsSortPopup (boardHeader.jade): a display order of the board's
// cards, kept in the board's URL (?sort=) by weddy.js the way WeKan keeps
// its sortBy for the viewer (#5886). No card's sort changes, so turning it
// off gives back the order the cards were dragged into.
//
//	due           by due date, cards without one first (WeKan's {dueAt: 1})
//	title         by title ({title: 1})
//	created-desc  newest first ({createdAt: -1})
//	created-asc   oldest first ({createdAt: 1})
//	votes         highest vote score first, positive minus negative (#3050)
//	label-order   where a card's first label comes in the board's label
//	              order, cards without a label last (#2040)
//
// Ties keep the manual order, as WeKan's _id tiebreaker keeps one order.
var cardSorts = []struct{ Key, Icon, Label string }{
	{"due", "fa-calendar", msg("due-date")},
	{"title", "fa-sort-alpha-asc", msg("title-alphabetically")},
	{"created-desc", "fa-arrow-down", msg("created-at-newest-first")},
	{"created-asc", "fa-arrow-up", msg("created-at-oldest-first")},
	{"votes", "fa-thumbs-o-up", msg("sort-by-votes")},
	{"label-order", "fa-tags", msg("sort-by-label-order")},
}

// parseCardSort is the ?sort= of the URL when it is one of WeKan's, else "".
func parseCardSort(q url.Values) string {
	v := q.Get("sort")
	for _, s := range cardSorts {
		if s.Key == v {
			return v
		}
	}
	return ""
}

// sortKeys are what the chosen order reads of each of the board's cards.
type sortKeys struct {
	Due      *time.Time
	Title    string
	Created  time.Time
	Votes    int
	LabelIDs []string
}

func (s *Server) cardSortKeys(ctx context.Context, boardID string) map[string]sortKeys {
	out := map[string]sortKeys{}
	cur, err := s.db.Collection("cards").Find(ctx, bson.M{"boardId": eq(boardID), "archived": false})
	if err != nil {
		return out
	}
	defer cur.Close(ctx)
	for cur.Next(ctx) {
		var c struct {
			ID       string   `bson:"_id"`
			Title    string   `bson:"title"`
			LabelIDs []string `bson:"labelIds"`
			Vote     struct {
				Positive []string `bson:"positive"`
				Negative []string `bson:"negative"`
			} `bson:"vote"`
		}
		if cur.Decode(&c) != nil {
			continue
		}
		k := sortKeys{Title: c.Title, LabelIDs: c.LabelIDs, Votes: len(c.Vote.Positive) - len(c.Vote.Negative)}
		var raw bson.M
		if cur.Decode(&raw) == nil {
			if t, ok := raw["dueAt"].(bson.DateTime); ok {
				due := t.Time()
				k.Due = &due
			}
			if t, ok := raw["createdAt"].(bson.DateTime); ok {
				k.Created = t.Time()
			}
		}
		out[c.ID] = k
	}
	return out
}

// sortLaneCards puts each list's cards in the chosen order.
func (s *Server) sortLaneCards(ctx context.Context, b *Board, lanes []lane, key string) {
	if key == "" {
		return
	}
	keys := s.cardSortKeys(ctx, b.ID)
	rank := map[string]int{}
	for i, l := range b.Labels {
		rank[l.ID] = i
	}
	labelRank := func(ids []string) int {
		r := len(b.Labels) + 1
		for _, id := range ids {
			if i, ok := rank[id]; ok && i < r {
				r = i
			}
		}
		return r
	}
	less := func(a, c sortKeys) bool {
		switch key {
		case "due":
			if a.Due == nil || c.Due == nil {
				return a.Due == nil && c.Due != nil
			}
			return a.Due.Before(*c.Due)
		case "title":
			return a.Title < c.Title
		case "created-desc":
			return a.Created.After(c.Created)
		case "created-asc":
			return a.Created.Before(c.Created)
		case "votes":
			return a.Votes > c.Votes
		default:
			return labelRank(a.LabelIDs) < labelRank(c.LabelIDs)
		}
	}
	for i := range lanes {
		for j := range lanes[i].Lists {
			cards := lanes[i].Lists[j].Cards
			sort.SliceStable(cards, func(x, y int) bool { return less(keys[cards[x].ID], keys[cards[y].ID]) })
		}
	}
}
