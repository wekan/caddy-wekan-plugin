package web

import (
	"context"
	"strings"
	"unicode/utf8"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// What WeKan's minicard shows beside its title (minicard.jade, minicard.js):
// the card's assignees as avatars, when the board shows them on minicards
// (getMinicardFlag: allowsAssigneeOnMinicard, else allowsAssignee, else on),
// and - when the board allows comments on minicards - the newest three
// comments, each cut at 140 characters, on lines of their own below them.

type minicardPerson struct{ ID, Name, Initials string }

const (
	minicardMaxComments = 3
	minicardMaxLength   = 140
)

// minicardFlag is getMinicardFlag.
func minicardFlag(board bson.M, onMinicard, legacy string, def bool) bool {
	if v, ok := board[onMinicard].(bool); ok {
		return v
	}
	if v, ok := board[legacy].(bool); ok {
		return v
	}
	return def
}

func initialsOf(name string) string {
	var out []rune
	for _, w := range strings.Fields(name) {
		r, _ := utf8.DecodeRuneInString(w)
		out = append(out, r)
		if len(out) == 2 {
			break
		}
	}
	return strings.ToUpper(string(out))
}

// annotateMinicards puts what each minicard shows on the lanes' cards: one
// query for the people, one for the comments.
func (s *Server) annotateMinicards(ctx context.Context, b *Board, lanes []lane, dateFormat string) {
	var board bson.M
	_ = s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(b.ID)}).Decode(&board)
	showAssignees := minicardFlag(board, "allowsAssigneeOnMinicard", "allowsAssignee", true)
	showComments := board["allowsCommentsOnMinicard"] == true
	showIssueType := board["allowsIssueTypeOnMinicard"] != false
	// No early return when neither shows: the checklists shown on minicards
	// are their own checklists' choice (showChecklistAtMinicard).
	cardIDs := bson.A{}
	people := map[string]bool{}
	for _, l := range lanes {
		for _, ll := range l.Lists {
			for _, c := range ll.Cards {
				cardIDs = append(cardIDs, c.ID)
				for _, a := range c.Assignees {
					people[a] = true
				}
			}
		}
	}
	names := map[string]minicardPerson{}
	if showAssignees && len(people) > 0 {
		ids := bson.A{}
		for id := range people {
			ids = append(ids, id)
		}
		cur, err := s.db.Collection("users").Find(ctx, bson.M{"_id": dbIn(ids)}, options.Find().SetProjection(bson.M{"username": 1, "profile.fullname": 1, "profile.initials": 1}))
		if err == nil {
			var us []User
			_ = cur.All(ctx, &us)
			for _, u := range us {
				name := u.Profile.Fullname
				if name == "" {
					name = u.Username
				}
				ini := u.Profile.Initials
				if ini == "" {
					ini = initialsOf(name)
				}
				names[u.ID] = minicardPerson{ID: u.ID, Name: name, Initials: ini}
			}
		}
	}
	comments := map[string][]string{}
	more := map[string]bool{}
	if showComments && len(cardIDs) > 0 {
		cur, err := s.db.Collection("card_comments").Find(ctx, bson.M{"boardId": eq(b.ID), "cardId": dbIn(cardIDs)},
			options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}}).SetProjection(bson.M{"cardId": 1, "text": 1}))
		if err == nil {
			var docs []bson.M
			_ = cur.All(ctx, &docs)
			for _, d := range docs {
				id := docString(d, "cardId")
				if len(comments[id]) >= minicardMaxComments {
					more[id] = true
					continue
				}
				text := docString(d, "text")
				if utf8.RuneCountInString(text) > minicardMaxLength {
					text = string([]rune(text)[:minicardMaxLength]) + "…"
					more[id] = true
				}
				comments[id] = append(comments[id], text)
			}
		}
	}
	checklists := s.minicardChecklists(ctx, b, cardIDs)
	for i := range lanes {
		for j := range lanes[i].Lists {
			for k := range lanes[i].Lists[j].Cards {
				c := &lanes[i].Lists[j].Cards[k]
				c.MinicardChecklists = checklists[c.ID]
				c.MinicardSides = sidesOfMinicard(board, c, dateFormat)
				if showIssueType {
					c.MinicardIssueType = issueTypeBadge(c.Scrum["issueType"])
				}
				if showAssignees {
					for _, a := range c.Assignees {
						if p, ok := names[a]; ok {
							c.MinicardAssignees = append(c.MinicardAssignees, p)
						}
					}
				}
				c.MinicardComments, c.MinicardMoreComments = comments[c.ID], more[c.ID]
			}
		}
	}
}

// minicardChecklist is one checklist on its card's minicard (WeKan's
// minicardChecklist): its title and its items, done or not.
type minicardChecklist struct {
	ID, Title string
	Items     []minicardChecklistItem
	// Collapsed: folded by the user, on the minicard and the card alike (#1591).
	Collapsed bool
}

type minicardChecklistItem struct {
	ID, Title  string
	IsFinished bool
}

// minicardChecklists are the board's checklists shown on their minicards
// (showChecklistAtMinicard), by card, each with its items, by sort.
func (s *Server) minicardChecklists(ctx context.Context, b *Board, cardIDs bson.A) map[string][]minicardChecklist {
	out := map[string][]minicardChecklist{}
	if len(cardIDs) == 0 {
		return out
	}
	var lists []struct {
		ID     string `bson:"_id"`
		CardID string `bson:"cardId"`
		Title  string `bson:"title"`
	}
	if cur, err := s.db.Collection("checklists").Find(ctx, bson.M{"cardId": dbIn(cardIDs), "showChecklistAtMinicard": true},
		options.Find().SetSort(bson.D{{Key: "sort", Value: 1}})); err == nil {
		_ = cur.All(ctx, &lists)
	}
	if len(lists) == 0 {
		return out
	}
	ids := bson.A{}
	for _, l := range lists {
		ids = append(ids, l.ID)
	}
	var items []struct {
		ID          string `bson:"_id"`
		ChecklistID string `bson:"checklistId"`
		Title       string `bson:"title"`
		IsFinished  bool   `bson:"isFinished"`
	}
	if cur, err := s.db.Collection("checklistItems").Find(ctx, bson.M{"checklistId": dbIn(ids)},
		options.Find().SetSort(bson.D{{Key: "sort", Value: 1}})); err == nil {
		_ = cur.All(ctx, &items)
	}
	byList := map[string][]minicardChecklistItem{}
	for _, it := range items {
		byList[it.ChecklistID] = append(byList[it.ChecklistID], minicardChecklistItem{it.ID, it.Title, it.IsFinished})
	}
	for _, l := range lists {
		out[l.CardID] = append(out[l.CardID], minicardChecklist{ID: l.ID, Title: l.Title, Items: byList[l.ID]})
	}
	return out
}
