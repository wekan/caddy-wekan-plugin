package web

import (
	"context"
	"html/template"
	"net/http"
	"regexp"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The notifications drawer, as Node.js WeKan's notificationsDrawer: the user's
// notifications, newest first, each as WeKan's clear item - the activity's
// sentence (act-<activityType>) with its board, its actor and its names - and
// the menu that marks them all read or deletes them all.

// activitySentences are the act-* sentences the drawer may show.
var activitySentences = map[string]string{
	"createCard": msg("act-createCard"), "moveCard": msg("act-moveCard"), "archivedCard": msg("act-archivedCard"),
	"restoredCard": msg("act-restoredCard"), "addComment": msg("act-addComment"), "joinMember": msg("act-joinMember"),
	"unjoinMember": msg("act-unjoinMember"), "joinAssignee": msg("act-joinAssignee"), "unjoinAssignee": msg("act-unjoinAssignee"),
	"addChecklist": msg("act-addChecklist"), "addAttachment": msg("act-addAttachment"), "createList": msg("act-createList"),
	"createSwimlane": msg("act-createSwimlane"), "createBoard": msg("act-createBoard"), "addedLabel": msg("act-addedLabel"),
	"removedLabel": msg("act-removedLabel"), "addBoardMember": msg("act-addBoardMember"), "removeBoardMember": msg("act-removeBoardMember"),
	"deleteCard": msg("act-deleteCard"), "checkedItem": msg("act-checkedItem"), "uncheckedItem": msg("act-uncheckedItem"),
}

// drawerItem is one notification as the drawer shows it.
type drawerItem struct {
	ActivityID string
	Read       bool
	HTML       template.HTML
	At         time.Time
}

func (d drawerItem) Display() string { return d.At.Local().Format("2006-01-02 15:04") }

var placeholderPattern = regexp.MustCompile(`__(\w+)__`)

// clearItem is WeKan's renderNotificationItem: the board, the actor, then the
// sentence with each name in bold - the card's a link to it - and a name that
// is not there left out. Everything but the markup is escaped.
func clearItem(sentence string, params map[string]string, cardURL string) template.HTML {
	esc := template.HTMLEscapeString
	body := placeholderPattern.ReplaceAllStringFunc(esc(sentence), func(m string) string {
		key := m[2 : len(m)-2]
		v := params[key]
		if v == "" {
			return ""
		}
		q := esc(`"` + v + `"`)
		if key == "card" && cardURL != "" {
			return `<b><a href="` + esc(cardURL) + `">` + q + `</a></b>`
		}
		return `<b>` + q + `</b>`
	})
	head := ""
	if b := params["board"]; b != "" {
		head += `<b>` + esc(b) + `</b>: `
	}
	if u := params["user"]; u != "" {
		head += `<b>` + esc(u) + `</b> `
	}
	return template.HTML(`<div class="wekan-notification-item">` + head + strings.TrimSpace(body) + `</div>`)
}

// drawerItems are u's notifications whose board u may still read - a board
// the user left keeps none of its activity in their drawer.
func (s *Server) drawerItems(ctx context.Context, u *User, lang string) []drawerItem {
	var doc struct {
		Profile struct {
			Notifications []struct {
				Activity string `bson:"activity"`
				Read     any    `bson:"read"`
			} `bson:"notifications"`
		} `bson:"profile"`
	}
	_ = s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(u.ID)}).Decode(&doc)
	var out []drawerItem
	n := doc.Profile.Notifications
	for i := len(n) - 1; i >= 0 && len(out) < 100; i-- {
		var a bson.M
		if !validID(n[i].Activity) || s.db.Collection("activities").FindOne(ctx, bson.M{"_id": eq(n[i].Activity)}).Decode(&a) != nil {
			continue
		}
		item, ok := s.notificationHTML(ctx, u, a, lang)
		if !ok {
			continue
		}
		at, _ := a["createdAt"].(bson.DateTime)
		out = append(out, drawerItem{ActivityID: n[i].Activity, Read: n[i].Read != nil, HTML: item, At: at.Time()})
	}
	return out
}

func (s *Server) notificationDrawerRoutes(m *http.ServeMux) {
	m.HandleFunc("POST /notifications/read-all", s.page(func(w http.ResponseWriter, r *http.Request, u *User) {
		var doc struct {
			Profile struct {
				Notifications []bson.M `bson:"notifications"`
			} `bson:"profile"`
		}
		_ = s.db.Collection("users").FindOne(r.Context(), bson.M{"_id": eq(u.ID)}).Decode(&doc)
		now := time.Now().UTC()
		for _, n := range doc.Profile.Notifications {
			if n["read"] == nil {
				n["read"] = now
			}
		}
		// The whole list: FerretDB has no positional $ to set one entry.
		_, _ = s.db.Collection("users").UpdateOne(r.Context(), bson.M{"_id": eq(u.ID)}, bson.M{"$set": bson.M{"profile.notifications": doc.Profile.Notifications}})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))
	m.HandleFunc("POST /notifications/delete-all", s.page(func(w http.ResponseWriter, r *http.Request, u *User) {
		_, _ = s.db.Collection("users").UpdateOne(r.Context(), bson.M{"_id": eq(u.ID)}, bson.M{"$set": bson.M{"profile.notifications": bson.A{}}})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))
}

// notificationHTML is one activity as a notification of u shows it - or false
// when u may no longer read its board, which then keeps it from them.
func (s *Server) notificationHTML(ctx context.Context, u *User, a bson.M, lang string) (template.HTML, bool) {
	params := map[string]string{"user": ""}
	cardURL := ""
	if boardID := docString(a, "boardId"); boardID != "" {
		b, err := s.boardFor(ctx, boardID, u)
		if err != nil {
			return "", false
		}
		params["board"] = b.Title
		if c := docString(a, "cardId"); c != "" {
			params["card"] = docString(a, "cardTitle")
			if params["card"] == "" {
				params["card"] = s.titleOfID(ctx, "cards", c)
			}
			cardURL = "/b/" + b.ID + "/" + b.Slug + "/" + c
		}
	}
	params["list"] = s.titleOfID(ctx, "lists", docString(a, "listId"))
	params["swimlane"] = s.titleOfID(ctx, "swimlanes", docString(a, "swimlaneId"))
	var actor User
	if s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(docString(a, "userId"))}).Decode(&actor) == nil {
		params["user"] = actor.Name()
	}
	// An activity with no sentence of its own shows its type, as WeKan's.
	item := template.HTML(template.HTMLEscapeString(docString(a, "activityType")))
	if key, ok := activitySentences[docString(a, "activityType")]; ok {
		item = clearItem(tr(lang, key), params, cardURL)
	}
	return item, true
}
