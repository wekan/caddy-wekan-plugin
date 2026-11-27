package web

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// quickAddLabel looks up a well-formed leading [label] and, for a board
// admin, creates a missing label using the first unused palette colour.
func (s *Server) quickAddLabel(ctx context.Context, b *Board, u *User, title string) (string, string, error) {
	if !strings.HasPrefix(title, "[") {
		return title, "", nil
	}
	name, rest, found := strings.Cut(title[1:], "]")
	first, _ := utf8.DecodeRuneInString(rest)
	if !found || strings.Contains(name, "[") || !unicode.IsSpace(first) {
		return title, "", nil
	}
	name, rest = strings.TrimSpace(name), strings.TrimSpace(rest)
	if name == "" || rest == "" {
		return title, "", nil
	}
	used := make(map[string]bool, len(b.Labels))
	for _, label := range b.Labels {
		if strings.EqualFold(label.Name, name) {
			return rest, label.ID, nil
		}
		used[label.Color] = true
	}
	if len(name) > 200 || !b.AdminOf(u.ID) {
		return title, "", nil
	}
	color := labelPalette[0]
	for _, choice := range labelPalette {
		if !used[choice] {
			color = choice
			break
		}
	}
	label := Label{ID: randomString(idAlphabet, 6), Name: name, Color: color}
	boards := s.db.Collection("boards")
	sameName := bson.Regex{Pattern: "^" + regexp.QuoteMeta(name) + "$", Options: "i"}
	result, err := boards.UpdateOne(ctx, bson.M{
		"_id": eq(b.ID), "labels": bson.M{"$not": bson.M{"$elemMatch": bson.M{"name": sameName}}},
		"members": bson.M{"$elemMatch": bson.M{"userId": u.ID, "isAdmin": true, "isActive": true}},
	}, bson.M{"$push": bson.M{"labels": label}})
	if err != nil {
		return title, "", err
	}
	if result.MatchedCount == 0 {
		// A simultaneous save can have created the same label already.
		current, err := s.boardFor(ctx, b.ID, u)
		if err != nil {
			return title, "", err
		}
		for _, existing := range current.Labels {
			if strings.EqualFold(existing.Name, name) {
				return rest, existing.ID, nil
			}
		}
		return title, "", errors.New("could not add the board label")
	}
	b.Labels = append(b.Labels, label)
	return rest, label.ID, nil
}
