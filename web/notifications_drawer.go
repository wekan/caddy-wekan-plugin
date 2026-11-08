package web

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"strconv"
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
	// The rest of WeKan's sentences that are named by their activity type.
	"addChecklistItem": msg("act-addChecklistItem"), "addLabel": msg("act-addLabel"), "addSubtask": msg("act-addSubtask"),
	"archivedBoard": msg("act-archivedBoard"), "archivedList": msg("act-archivedList"), "archivedSwimlane": msg("act-archivedSwimlane"),
	"changedBoardTitle": msg("act-changedBoardTitle"), "changedListTitle": msg("act-changedListTitle"),
	"changedSwimlaneTitle": msg("act-changedSwimlaneTitle"), "completeChecklist": msg("act-completeChecklist"),
	"createCustomField": msg("act-createCustomField"), "deleteAttachment": msg("act-deleteAttachment"), "deleteBoard": msg("act-deleteBoard"),
	"deleteComment": msg("act-deleteComment"), "deleteCustomField": msg("act-deleteCustomField"), "editCard": msg("act-editCard"),
	"editComment": msg("act-editComment"), "importBoard": msg("act-importBoard"), "importCard": msg("act-importCard"),
	"importList": msg("act-importList"), "moveCardBoard": msg("act-moveCardBoard"), "moveCardToOtherBoard": msg("act-moveCardToOtherBoard"),
	"removeBoard": msg("act-removeBoard"), "removeChecklist": msg("act-removeChecklist"),
	"removeChecklistItem": msg("act-removeChecklistItem"), "removedChecklistItem": msg("act-removedChecklistItem"),
	"removeLabel": msg("act-removeLabel"), "removeList": msg("act-removeList"), "removeSwimlane": msg("act-removeSwimlane"),
	"restoredList": msg("act-restoredList"), "restoredSwimlane": msg("act-restoredSwimlane"), "setCustomField": msg("act-setCustomField"),
	"uncompleteChecklist": msg("act-uncompleteChecklist"),
}

// drawerItem is one notification as the drawer shows it.
type drawerItem struct {
	ActivityID       string
	Read             bool
	HTML             template.HTML
	At               time.Time
	BoardID, CardID  string
	BoardTitle, Card string
	// Group is the collapsible row it belongs to; Rest, an entry after its
	// group's first, hidden until the row is opened.
	Group string
	Rest  bool
}

// drawerGroup is one row of the drawer: an entry alone, or - #5171, when the
// tray grouping says so - one card's or one board's entries together, shown
// by the first of them until opened.
type drawerGroup struct {
	Key         string
	Label       string
	Collapsible bool
	Items       []drawerItem
}

// trayGrouping is WeKan's trayDeliveryFor(boardId).grouping: the member's
// tray setting, else the board's, else Admin Panel's, else none.
func (s *Server) trayGrouping(ctx context.Context, u *User, boardID string) string {
	valid := func(v any) string {
		if g, _ := v.(string); g == "none" || g == "card" || g == "board" {
			return g
		}
		return ""
	}
	// field is a nested value of a document as the driver decodes it.
	field := func(v any, keys ...string) any {
		for _, k := range keys {
			switch d := v.(type) {
			case bson.M:
				v = d[k]
			case bson.D:
				v = docMap(d)[k]
			default:
				return nil
			}
		}
		return v
	}
	of := func(doc bson.M) any {
		if g := field(doc, "notificationDelivery", "tray", "grouping"); g != nil {
			return g
		}
		return field(doc, "profile", "notificationDelivery", "tray", "grouping")
	}
	var user, board, set bson.M
	_ = s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(u.ID)}).Decode(&user)
	if g := valid(of(user)); g != "" {
		return g
	}
	if validID(boardID) {
		_ = s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(boardID)}).Decode(&board)
		if g := valid(of(board)); g != "" {
			return g
		}
	}
	_ = s.db.Collection("settings").FindOne(ctx, bson.M{}).Decode(&set)
	if g := valid(of(set)); g != "" {
		return g
	}
	return "none"
}

// drawerGroups is WeKan's groupEntries over the drawer's entries, in order.
func (s *Server) drawerGroups(ctx context.Context, u *User, items []drawerItem, lang string) []drawerGroup {
	groups := []drawerGroup{}
	at := map[string]int{}
	grouping := map[string]string{}
	for i, it := range items {
		g, ok := grouping[it.BoardID]
		if !ok {
			g = s.trayGrouping(ctx, u, it.BoardID)
			grouping[it.BoardID] = g
		}
		key := ""
		switch {
		case g == "card" && it.CardID != "":
			key = "card:" + it.CardID
		case g == "card" || g == "board":
			key = "board:" + it.BoardID
		}
		if key == "" {
			groups = append(groups, drawerGroup{Key: "single:" + strconv.Itoa(i), Items: []drawerItem{it}})
			continue
		}
		if j, ok := at[key]; ok {
			groups[j].Items = append(groups[j].Items, it)
			continue
		}
		at[key] = len(groups)
		groups = append(groups, drawerGroup{Key: key, Items: []drawerItem{it}})
	}
	for i := range groups {
		g := &groups[i]
		if g.Collapsible = len(g.Items) > 1; !g.Collapsible {
			continue
		}
		for j := range g.Items {
			g.Items[j].Group, g.Items[j].Rest = g.Key, j > 0
		}
		first, n := g.Items[0], strconv.Itoa(len(g.Items))
		if strings.HasPrefix(g.Key, "card:") {
			g.Label = strings.NewReplacer("__count__", n, "__card__", first.Card).Replace(tr(lang, "notification-group-card"))
		} else {
			g.Label = strings.NewReplacer("__count__", n, "__board__", first.BoardTitle).Replace(tr(lang, "notification-group-board"))
		}
	}
	return groups
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
		boardID, _ := a["boardId"].(string)
		cardID, _ := a["cardId"].(string)
		out = append(out, drawerItem{ActivityID: n[i].Activity, Read: n[i].Read != nil, HTML: item, At: at.Time(),
			BoardID: boardID, CardID: cardID, BoardTitle: s.titleOfID(ctx, "boards", boardID), Card: s.titleOfID(ctx, "cards", cardID)})
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
	// The rest of WeKan's params (server/models/activities.js).
	params["oldList"] = s.titleOfID(ctx, "lists", docString(a, "oldListId"))
	params["oldSwimlane"] = s.titleOfID(ctx, "swimlanes", docString(a, "oldSwimlaneId"))
	params["oldBoard"] = s.titleOfID(ctx, "boards", docString(a, "oldBoardId"))
	params["checklist"] = s.titleOfID(ctx, "checklists", docString(a, "checklistId"))
	params["checklistItem"] = s.titleOfID(ctx, "checklistItems", docString(a, "checklistItemId"))
	params["attachment"] = docString(a, "attachmentName")
	if id := docString(a, "commentId"); validID(id) {
		var c struct {
			Text string `bson:"text"`
		}
		if s.db.Collection("card_comments").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&c) == nil {
			params["comment"] = c.Text
		}
	}
	if id := docString(a, "customFieldId"); validID(id) {
		var f struct {
			Name      string `bson:"name"`
			AdminOnly bool   `bson:"adminOnly"`
		}
		if s.db.Collection("customFields").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&f) == nil {
			params["customField"] = f.Name
			// #3141: an admin-only field's value reaches only board admins.
			var b Board
			admin := false
			if s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(docString(a, "boardId"))}).Decode(&b) == nil {
				m := b.member(u.ID)
				admin = m != nil && m.IsAdmin
			}
			if v, ok := a["value"]; ok && v != nil && (!f.AdminOnly || admin) {
				params["customFieldValue"] = jsString(v)
			}
		}
	}
	if id := docString(a, "labelId"); id != "" {
		var b Board
		if s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(docString(a, "boardId"))}).Decode(&b) == nil {
			for _, l := range b.Labels {
				if l.ID == id {
					params["label"] = l.Name
					if l.Name == "" {
						params["label"] = l.Color
					}
				}
			}
		}
	}
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

// jsString is a value as JavaScript's String() writes it: 0 is "0", false
// is "false" - never left out as empty.
func jsString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int32:
		return strconv.Itoa(int(x))
	case int64:
		return strconv.FormatInt(x, 10)
	case bson.DateTime:
		return x.Time().UTC().Format("2006-01-02T15:04:05.000Z")
	}
	return fmt.Sprint(v)
}
