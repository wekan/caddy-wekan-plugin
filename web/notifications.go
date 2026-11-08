package web

import (
	"context"
	"net/http"
	"regexp"
	"slices"

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
	for _, uid := range s.notificationRecipients(ctx, actorID, activity) {
		_, _ = s.db.Collection("users").UpdateOne(ctx, bson.M{"_id": eq(uid), "profile.notifications.activity": bson.M{"$ne": id}},
			bson.M{"$push": bson.M{"profile.notifications": bson.M{"activity": id, "read": nil}}})
	}
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
