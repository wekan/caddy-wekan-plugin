package web

import (
	"context"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The people of an imported file, as Node.js WeKan's import makes them
// (models/lib/importMembersMode.js, server/lib/importPlaceholderUsers.js):
// "me" makes every one the person importing; otherwise - WeKan's default,
// and "map" when nobody was mapped - each becomes a placeholder user that
// keeps the name, cannot log in, and is an inactive member of the new board
// with no rights, for a board admin to map to a real user later.

// importMembersMode is WeKan's membersMode: placeholder unless the import
// says map or me.
func importMembersMode(mode string) string {
	switch mode {
	case "map", "me":
		return mode
	}
	return "placeholder"
}

type importPerson struct{ key, name string }

// importPeople is WeKan's importedPeople: owners, assignees, watchers and
// comment authors, each once by its trimmed key, with the best name given.
func importPeople(tasks []any) []importPerson {
	var people []importPerson
	index := map[string]int{}
	add := func(key, name any) {
		id := ""
		switch k := key.(type) {
		case string:
			id = jsTrim(k)
		case float64:
			id = jsTrim(jsStr(k))
		}
		if id == "" {
			return
		}
		given := ""
		if jsTruthy(name) {
			given = jsTrim(jsStr(name))
		}
		if i, ok := index[id]; ok {
			if given != "" && people[i].name == id {
				people[i].name = given
			}
			return
		}
		index[id] = len(people)
		people = append(people, importPerson{id, jsOr(given, id).(string)})
	}
	for _, t := range tasks {
		if _, ok := t.(jsObject); !ok {
			continue
		}
		add(jsOr(jsGet(t, "owner_id"), jsGet(t, "owner_username"), jsGet(t, "owner_name")), jsOr(jsGet(t, "owner_name"), jsGet(t, "owner_username")))
		for _, f := range []string{"assignees", "watchers"} {
			list, _ := jsGet(t, f).([]any)
			for _, k := range list {
				add(k, nil)
			}
		}
		comments, _ := jsGet(t, "comments").([]any)
		for _, c := range comments {
			if jsTruthy(c) {
				add(jsGet(c, "author"), jsGet(c, "authorName"))
			}
		}
	}
	return people
}

var importUsernameSpace = jsRegexp(`\s+`)

// importUsername is WeKan's uniqueUsername: the name with its spaces as
// dashes, at most 60 long, numbered when it is taken.
func (s *Server) importUsername(ctx context.Context, base string) string {
	name := jsSlice16(importUsernameSpace.ReplaceAllString(jsTrim(base), "-"), 60)
	if name == "" {
		name = "imported"
	}
	candidate := name
	for n := 1; s.db.Collection("users").FindOne(ctx, bson.M{"username": eq(candidate)}).Err() == nil; n++ {
		candidate = name + "-" + jsNumber(float64(n))
	}
	return candidate
}

// importPlaceholders is WeKan's createImportPlaceholders: a user for each
// person nobody is mapped to, its id in mapping, and the new users' ids.
func (s *Server) importPlaceholders(ctx context.Context, people []importPerson, mapping map[string]string, source string) []string {
	var made []string
	for _, p := range people {
		if p.key == "" || mapping[p.key] != "" {
			continue
		}
		name := jsOr(p.name, p.key).(string)
		id := randomID()
		now := time.Now().UTC()
		// What WeKan's users schema keeps of its insert: isActive, importedFrom
		// and importedAt are not in it and are not stored.
		_, err := s.db.Collection("users").InsertOne(ctx, bson.M{ // inserted, not a filter
			"_id":                  id,
			"username":             s.importUsername(ctx, name),
			"profile":              bson.M{"fullname": name, "initials": strings.ToUpper(jsSlice16(name, 2))},
			"authenticationMethod": "imported",
			"loginDisabled":        true,
			"importUsernames":      bson.A{p.key},
			"createdAt":            now,
			"services":             bson.M{},
		})
		if err != nil {
			continue // best effort: a person who cannot be a placeholder stays text
		}
		mapping[p.key] = id
		made = append(made, id)
	}
	return made
}

// addPlaceholderMembers makes the placeholders members of the board,
// inactive and with no rights, as WeKan's creators do.
func (s *Server) addPlaceholderMembers(ctx context.Context, boardID string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	var members bson.A
	for _, id := range ids {
		members = append(members, bson.M{"userId": id, "wekanId": id, "isActive": false, "isAdmin": false, // inserted, not a filter
			"isNoComments": false, "isCommentOnly": false, "swimlaneId": false})
	}
	_, err := s.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": eq(boardID)}, bson.M{"$push": bson.M{"members": bson.M{"$each": members}}})
	return err
}
