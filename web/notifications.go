package web

import (
	"context"
	"errors"
	"html"
	"net/http"
	"regexp"
	"slices"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Notifications, as Node.js WeKan makes them (server/models/activities.js
// prepareActivityNotification, models/lib/boardNotificationRecipients.js,
// models/lib/notificationInsertion.js): an activity notifies the people who
// take part in what it is about and those who watch it, each once, in the
// user's profile.notifications as {activity, read: null}.

// mentionPattern is WeKan's: @name or @"a name".
var mentionPattern = regexp.MustCompile(`(?:^|[^\w])@(?:"([\w.\s-]*)"|([\w.@-]+))`)

func addAll(set []string, ids ...string) []string {
	for _, id := range ids {
		if id != "" && !slices.Contains(set, id) {
			set = append(set, id)
		}
	}
	return set
}

// notificationRecipients is who an activity notifies.
func (s *Server) notificationRecipients(ctx context.Context, actorID string, a bson.M) []string {
	str := func(k string) string { v, _ := a[k].(string); return v }
	var board bson.M
	if s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(str("boardId"))}).Decode(&board) != nil {
		return nil
	}
	var b Board
	raw, _ := bson.Marshal(board)
	_ = bson.Unmarshal(raw, &b)
	var participants, watchers, scoped []string
	if m := str("memberId"); m != "" {
		participants = addAll(participants, m)
	}
	for _, f := range []string{"listId", "oldListId"} {
		var l bson.M
		if id := str(f); id != "" && s.db.Collection("lists").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&l) == nil {
			ws := stringsOf(l["watchers"])
			scoped, watchers = addAll(scoped, ws...), addAll(watchers, ws...)
		}
	}
	var card bson.M
	if id := str("cardId"); id != "" && s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&card) == nil {
		creator, _ := card["userId"].(string)
		participants = addAll(participants, creator)
		participants = addAll(participants, stringsOf(card["members"])...)
		participants = addAll(participants, stringsOf(card["assignees"])...)
		ws := stringsOf(card["watchers"])
		scoped, watchers = addAll(scoped, ws...), addAll(watchers, ws...)
	}
	mentioned, selfMention := false, false
	if id := str("commentId"); id != "" {
		var cm struct {
			Text string `bson:"text"`
		}
		_ = s.db.Collection("card_comments").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&cm)
		for _, m := range mentionPattern.FindAllStringSubmatch(cm.Text, -1) {
			name := m[1]
			if name == "" {
				name = m[2]
			}
			switch name {
			case "board_members":
				for _, mem := range b.Members {
					watchers = addAll(watchers, mem.UserID)
				}
			case "board_assignees":
				cur, err := s.db.Collection("cards").Find(ctx, bson.M{"boardId": b.ID})
				if err == nil {
					var cards []Card
					_ = cur.All(ctx, &cards)
					for _, c := range cards {
						for _, as := range c.Assignees {
							if b.member(as) != nil {
								watchers = addAll(watchers, as)
							}
						}
					}
				}
			case "card_members", "card_assignees":
				field := map[string]string{"card_members": "members", "card_assignees": "assignees"}[name]
				for _, id := range stringsOf(card[field]) {
					if b.member(id) != nil {
						watchers = addAll(watchers, id)
					}
				}
			default:
				var u User
				if s.db.Collection("users").FindOne(ctx, bson.M{"username": eq(name)}).Decode(&u) != nil || !slices.ContainsFunc(b.Members, func(m Member) bool { return m.UserID == u.ID }) {
					continue
				}
				watchers = addAll(watchers, u.ID)
				selfMention = selfMention || u.ID == actorID
			}
			mentioned = true
		}
	}
	var watching, tracking []string
	if ws, ok := board["watchers"].(bson.A); ok {
		for _, e := range ws {
			d, _ := e.(bson.D)
			m := docMap(d)
			uid, _ := m["userId"].(string)
			switch m["level"] {
			case "watching":
				watching = append(watching, uid)
			case "tracking":
				tracking = append(tracking, uid)
			}
		}
	}
	if !mentioned {
		watchers = addAll(watchers, watching...)
		for _, p := range participants {
			if slices.Contains(tracking, p) {
				watchers = addAll(watchers, p)
			}
		}
	}
	// #5833: the member or assignee just added is told directly.
	if t := str("activityType"); t == "joinMember" || t == "joinAssignee" {
		watchers = addAll(watchers, str("assigneeId"), str("memberId"))
	}
	// #6658: only active members who watch or track the board, or watch this
	// list or card; a muted member is never opted back in by an assignment.
	notifying := append(append(append([]string{}, watching...), tracking...), scoped...)
	var out []string
	for _, id := range watchers {
		if b.MemberOf(id) && slices.Contains(notifying, id) && (id != actorID || selfMention) {
			out = append(out, id)
		}
	}
	return out
}

// notify adds the activity to each recipient's notifications, once.
func (s *Server) notify(ctx context.Context, actorID string, activity bson.M) {
	id, _ := activity["_id"].(string)
	if id == "" {
		return
	}
	// As stored: the intent's snapshot is the activity, byte for byte.
	var stored bson.M
	if s.db.Collection("activities").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&stored) != nil {
		return
	}
	var dispatch any
	if actorID != "" {
		dispatch = actorID
	}
	s.captureIntent(ctx, plain(stored).(map[string]any), dispatch)
	if err := s.deliverIntent(ctx, intentIDFor(id)); err != nil && !errors.Is(err, errLeaseBusy) {
		s.log.Warn("activity notification left for recovery", "reason", err.Error())
	}
}

// emailNotificationsOn is WeKan's resolveNotificationSetting for e-mail: the
// member's own choice, else the board's, else Admin Panel's default, else on.
func (s *Server) emailNotificationsOn(ctx context.Context, member, boardID string) bool {
	var u struct {
		Profile struct {
			Override *bool `bson:"notifyOverrideEmail"`
		} `bson:"profile"`
	}
	_ = s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(member)}).Decode(&u)
	if u.Profile.Override != nil {
		return *u.Profile.Override
	}
	var b struct {
		Override *bool `bson:"notifyOverrideEmail"`
	}
	if validID(boardID) && s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(boardID)}).Decode(&b) == nil && b.Override != nil {
		return *b.Override
	}
	var set struct {
		Default *bool `bson:"notifyDefaultEmail"`
	}
	if s.db.Collection("settings").FindOne(ctx, bson.M{}).Decode(&set) == nil && set.Default != nil {
		return *set.Default
	}
	return true
}

var tagPattern = regexp.MustCompile(`<[^>]*>`)

// mailNotification sends the notification to the member by e-mail, as WeKan
// does when a mail server is set and e-mail notifications are on for them:
// WeKan's "Activity Notification" subject, the drawer's sentence in their
// language, without markup. It is sent in the background, so no request
// waits for the mail server.
func (s *Server) prepareEmailJob(ctx context.Context, member string, activity bson.M) map[string]any {
	var u User
	if s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(member)}).Decode(&u) != nil || len(u.Emails) == 0 ||
		!s.emailNotificationsOn(ctx, member, docString(activity, "boardId")) {
		return nil
	}
	if _, err := s.mailSettings(ctx, "localhost"); err != nil {
		return nil // no mail server: the drawer is all there is
	}
	lang := u.Profile.Language
	if lang == "" {
		lang = "en"
	}
	item, ok := s.notificationHTML(ctx, &u, activity, lang)
	if !ok {
		return nil
	}
	text := html.UnescapeString(tagPattern.ReplaceAllString(string(item), ""))
	boardID, cardID, eventID := docString(activity, "boardId"), docString(activity, "cardId"), docString(activity, "_id")
	nullable := func(v string) any {
		if v == "" {
			return nil
		}
		return v
	}
	// WeKan's prepareActivityEmail: the plan's e-mail job, grouped and
	// scheduled as the member, the board or Admin Panel says.
	job := map[string]any{"userId": member, "eventId": eventID, "subject": tr(lang, msg("act-activity-notify")), "html": string(item),
		"language": lang, "boardId": nullable(boardID), "cardId": nullable(cardID), "text": text}
	d := s.emailDeliveryFor(ctx, member, boardID)
	if !d.GroupingFromDefault && d.Grouping != "all" {
		job["groupKey"] = emailGroupKey(d.Grouping, boardID, cardID, eventID)
	}
	if d.Schedule != "immediate" || d.QuietStart != "" {
		job["deliverAt"] = float64(nextDeliveryAt(time.Now().UTC(), d, emailDelay()).UnixMilli())
	}
	return job
}

// unreadNotifications is how many of u's notifications are unread.
func (s *Server) unreadNotifications(ctx context.Context, userID string) int {
	var u struct {
		Profile struct {
			Notifications []struct {
				Read any `bson:"read"`
			} `bson:"notifications"`
		} `bson:"profile"`
	}
	_ = s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(userID)}).Decode(&u)
	n := 0
	for _, x := range u.Profile.Notifications {
		if x.Read == nil {
			n++
		}
	}
	return n
}

func (s *Server) notificationRoutes(m *http.ServeMux) {
	s.notificationDrawerRoutes(m)
	m.HandleFunc("GET /notifications/unread", s.page(func(w http.ResponseWriter, r *http.Request, u *User) {
		writeJSON(w, http.StatusOK, map[string]int{"unread": s.unreadNotifications(r.Context(), u.ID)})
	}))
}
