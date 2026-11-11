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
func (s *Server) annotateMinicards(ctx context.Context, b *Board, lanes []lane) {
	var board bson.M
	_ = s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(b.ID)}).Decode(&board)
	showAssignees := minicardFlag(board, "allowsAssigneeOnMinicard", "allowsAssignee", true)
	showComments := board["allowsCommentsOnMinicard"] == true
	if !showAssignees && !showComments {
		return
	}
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
	for i := range lanes {
		for j := range lanes[i].Lists {
			for k := range lanes[i].Lists[j].Cards {
				c := &lanes[i].Lists[j].Cards[k]
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
