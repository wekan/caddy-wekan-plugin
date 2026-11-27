package web

import (
	"context"
	"net/url"
	"regexp"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The Filter sidebar's card text and label filters, as WeKan's
// client/lib/filter.js - applied on the server, by the board (and its
// Calendar view) drawn again with only the cards they match, like the Advanced
// Filter beside them. They are a display filter of the board the viewer
// already reads, never an authorization selector.
//
//	?text=     the card text filter (#3361): title, description, comments,
//	           checklist and item titles, case-insensitive (boardTextSearch.js)
//	?labels=   labels a card must carry, comma separated; __none__ is "no label"
//	?xlabels=  labels a card must not carry (#2886: exclusion wins)
//	?labelMode=and  every chosen label instead of any (#2044)

const (
	maxFilterText = 512 // sidebarFilters.jade maxlength
	noLabel       = "__none__"
)

type boardFilter struct {
	Text     string
	Labels   []string // chosen labels, without noLabel
	NoLabel  bool
	Excluded []string
	And      bool
}

func parseBoardFilter(q url.Values, b *Board) boardFilter {
	f := boardFilter{Text: strings.TrimSpace(q.Get("text")), And: q.Get("labelMode") == "and"}
	if len([]rune(f.Text)) > maxFilterText {
		f.Text = string([]rune(f.Text)[:maxFilterText])
	}
	// Only the board's own labels: anything else in the URL is not a filter,
	// and what is put in the filter is the board's own id of the label, not
	// the text of the URL.
	known := map[string]string{}
	for _, l := range b.Labels {
		known[l.ID] = l.ID
	}
	ids := func(raw string, allowNone bool) []string {
		var out []string
		for _, id := range strings.Split(raw, ",") {
			switch {
			case id == noLabel && allowNone:
				f.NoLabel = true
			case known[id] != "" && !contains(out, known[id]):
				out = append(out, known[id])
			}
		}
		return out
	}
	f.Labels = ids(q.Get("labels"), true)
	f.Excluded = ids(q.Get("xlabels"), false)
	return f
}

func (f boardFilter) active() bool {
	return f.Text != "" || len(f.Labels) > 0 || f.NoLabel || len(f.Excluded) > 0
}

// labelSelector is WeKan's: the chosen labels - any of them, or all of them in
// AND mode - or no label at all as its own alternative, and never an excluded
// one.
func (f boardFilter) labelSelector() bson.M {
	var alts bson.A
	if len(f.Labels) > 0 {
		op := "$in"
		if f.And {
			op = "$all"
		}
		alts = append(alts, bson.M{"labelIds": bson.M{op: f.Labels}})
	}
	if f.NoLabel {
		// No label: labelIds missing, null or [].
		alts = append(alts, bson.M{"$or": bson.A{bson.M{"labelIds": nil}, bson.M{"labelIds": bson.M{"$size": 0}}}})
	}
	var all bson.A
	if len(alts) > 0 {
		all = append(all, bson.M{"$or": alts})
	}
	if len(f.Excluded) > 0 {
		all = append(all, bson.M{"labelIds": bson.M{"$nin": f.Excluded}})
	}
	if len(all) == 0 {
		return bson.M{}
	}
	return bson.M{"$and": all}
}

// boardFilterSelector matches a board's cards by labels and searchable text.
func (s *Server) boardFilterSelector(ctx context.Context, boardID string, f boardFilter) bson.M {
	sel := bson.A{bson.M{"boardId": eq(boardID)}, f.labelSelector()}
	if f.Text != "" {
		matched := s.boardTextMatches(ctx, boardID, f.Text)
		sel = append(sel, bson.M{"$or": matched})
	}
	return bson.M{"$and": sel}
}

// boardTextMatches builds the alternatives shared by the filter and search.
func (s *Server) boardTextMatches(ctx context.Context, boardID, text string) bson.A {
	rx := bson.Regex{Pattern: queryText(regexp.QuoteMeta(text)), Options: "i"}
	matched := bson.A{bson.M{"title": rx}, bson.M{"description": rx}}
	var hits bson.A // card ids read from the database
	for coll, field := range map[string]string{"card_comments": "text", "checklists": "title", "checklistItems": "title"} {
		cur, err := s.db.Collection(coll).Find(ctx, bson.M{"boardId": eq(boardID), field: rx})
		if err != nil {
			continue
		}
		var rows []struct {
			CardID string `bson:"cardId"`
		}
		_ = cur.All(ctx, &rows)
		for _, r := range rows {
			hits = append(hits, r.CardID)
		}
	}
	if len(hits) > 0 {
		matched = append(matched, bson.M{"_id": dbIn(hits)})
	}
	return matched
}

// boardFilterCardIDs returns the board's cards the filter matches.
func (s *Server) boardFilterCardIDs(ctx context.Context, boardID string, f boardFilter) map[string]bool {
	out := map[string]bool{}
	cur, err := s.db.Collection("cards").Find(ctx, s.boardFilterSelector(ctx, boardID, f))
	if err != nil {
		return out
	}
	var rows []struct {
		ID string `bson:"_id"`
	}
	_ = cur.All(ctx, &rows)
	for _, r := range rows {
		out[r.ID] = true
	}
	return out
}

// filterLabel is one of the board's labels in the sidebar, with its state.
type filterLabel struct {
	Label
	Chosen, Excluded bool
}

func (f boardFilter) sidebarLabels(b *Board) []filterLabel {
	out := make([]filterLabel, 0, len(b.Labels))
	for _, l := range b.Labels {
		out = append(out, filterLabel{Label: l, Chosen: contains(f.Labels, l.ID), Excluded: contains(f.Excluded, l.ID)})
	}
	return out
}
