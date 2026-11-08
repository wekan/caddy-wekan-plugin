package web

import (
	"context"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Admin Panel -> Anonymize export users (anonymizeExportUsers), as Node.js
// WeKan's models/lib/importExportSecurity.js does it: every export that names
// a user - the JSON, the ZIP, all boards at once, CSV/TSV, Excel, PDF and the
// calendar feed - writes user1, user2 ... instead of usernames, full names and
// initials, drops avatars, and rewrites @mentions and the requested-by /
// assigned-by text of cards and comments. Indices follow the users' ids, so
// one board always gets the same labels. Only the JSON export did it in WeKan
// once, and the rest carried real names (CrossBoardExportBleed).

type anonMap struct {
	byID       map[string]anonUser
	byUsername map[string]string
}

type anonUser struct{ Username, Fullname, Initials string }

// exportAnonymizer is the map for a board's export, or nil when Admin Panel
// leaves export users as they are. users are the ones the export names; the
// board's members are added, so a member only mentioned in a title is
// anonymized too.
func (s *Server) exportAnonymizer(ctx context.Context, b *Board, users []bson.M) *anonMap {
	var set struct {
		AnonymizeExportUsers bool `bson:"anonymizeExportUsers"`
	}
	_ = s.db.Collection("settings").FindOne(ctx, bson.M{}).Decode(&set)
	if !set.AnonymizeExportUsers {
		return nil
	}
	known := map[string]string{}
	for _, u := range users {
		id, _ := u["_id"].(string)
		name, _ := u["username"].(string)
		if id != "" {
			known[id] = name
		}
	}
	missing := bson.A{}
	for _, m := range b.Members {
		if _, ok := known[m.UserID]; !ok && m.UserID != "" {
			missing = append(missing, m.UserID)
		}
	}
	if len(missing) > 0 {
		if cur, err := s.db.Collection("users").Find(ctx, bson.M{"_id": dbIn(missing)}, options.Find().SetProjection(bson.M{"username": 1})); err == nil {
			found := []bson.M{}
			_ = cur.All(ctx, &found)
			for _, u := range found {
				id, _ := u["_id"].(string)
				name, _ := u["username"].(string)
				known[id] = name
			}
		}
	}
	return buildAnonMap(known, anonymizedUserWord(exportLang(ctx)))
}

type exportLangKey struct{}

// exportLang is the language the export is written for: its reader's, as
// exportable found it, else English.
func exportLang(ctx context.Context) string {
	if l, ok := ctx.Value(exportLangKey{}).(string); ok && l != "" {
		return l
	}
	return "en"
}

// anonymizedUserWord is "user" in the reader's language.
func anonymizedUserWord(lang string) string {
	if w := strings.TrimSpace(tr(lang, "anonymized-user")); w != "" && w != "anonymized-user" {
		return w
	}
	return "user"
}

// buildAnonMap numbers users (id -> username) in id order.
func buildAnonMap(users map[string]string, word string) *anonMap {
	ids := make([]string, 0, len(users))
	for id := range users {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	initial, _ := utf8.DecodeRuneInString(word)
	if initial == utf8.RuneError {
		initial = 'u'
	}
	first := strings.ToUpper(string(initial))
	a := &anonMap{byID: map[string]anonUser{}, byUsername: map[string]string{}}
	for i, id := range ids {
		n := strconv.Itoa(i + 1)
		label := word + n
		a.byID[id] = anonUser{label, label, first + n}
		if name := users[id]; name != "" {
			a.byUsername[name] = label
		}
	}
	return a
}

// mention is an @username not inside a word or an e-mail address.
var mention = regexp.MustCompile(`(^|[^\w@])@([\w.-]+)`)

// text rewrites the @mentions of known users; any other @ stays.
func (a *anonMap) text(s string) string {
	if a == nil || s == "" || len(a.byUsername) == 0 {
		return s
	}
	return mention.ReplaceAllStringFunc(s, func(m string) string {
		sub := mention.FindStringSubmatch(m)
		if repl, ok := a.byUsername[sub[2]]; ok {
			return sub[1] + "@" + repl
		}
		return m
	})
}

// user anonymizes a user document as the export writes it.
func (a *anonMap) user(u bson.M) {
	if a == nil {
		return
	}
	id, _ := u["_id"].(string)
	x, ok := a.byID[id]
	if !ok {
		return
	}
	u["username"] = x.Username
	u["profile"] = bson.M{"fullname": x.Fullname, "initials": x.Initials}
}

// cards rewrites what cards say about users: title, description, and the
// requested-by / assigned-by text when it is a known username.
func (a *anonMap) cards(cards []bson.M) {
	if a == nil {
		return
	}
	for _, c := range cards {
		for _, f := range []string{"title", "description"} {
			if v, ok := c[f].(string); ok {
				c[f] = a.text(v)
			}
		}
		for _, f := range []string{"requestedBy", "assignedBy"} {
			if v, ok := c[f].(string); ok {
				if repl, ok := a.byUsername[v]; ok {
					c[f] = repl
				}
			}
		}
	}
}

// comments rewrites the @mentions of comments.
func (a *anonMap) comments(comments []bson.M) {
	if a == nil {
		return
	}
	for _, c := range comments {
		if v, ok := c["text"].(string); ok {
			c["text"] = a.text(v)
		}
	}
}
