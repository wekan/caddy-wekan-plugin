// Package web is WeKan written in Go, running inside Caddy: Weddy's WeKan.
//
// It reads and writes WeKan's own MongoDB documents - users, boards, swimlanes,
// lists, cards - in the same shapes, through the FerretDB that
// caddy-ferretdb-plugin embeds, so the data of a Node.js WeKan works here as it is.
// A document is changed only with $set / $push of the fields this code knows:
// fields it does not know yet are never lost.
//
// The pages are Go Pug templates (templates/), made interactive with Unpoly,
// interact.js - with WeKan's MultiDrag: many cards at once - and Server-Sent
// Events for what other users change. The REST API answers at WeKan's addresses.
package web

import (
	"crypto/rand"
	"math/big"
	"regexp"
	"slices"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Meteor's Random.id(): 17 characters of this alphabet. WeKan's documents have
// such ids, and the ones made here look the same.
const idAlphabet = "23456789ABCDEFGHJKLMNPQRSTWXYZabcdefghijkmnopqrstuvwxyz"

func randomID() string { return randomString(idAlphabet, 17) }

// An id from a request - an address, a form, a JSON body - is used in a query
// only when it looks like one: Meteor's ids, and the letters, digits, _ and - of
// ids other tools wrote. Anything else is not found.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func validID(id string) bool { return idPattern.MatchString(id) }

// eq matches a value exactly: a value from a request is compared with $eq, never
// put in a filter where the database could read it as anything but a value.
func eq(v string) bson.M { return bson.M{"$eq": v} }

// exactFold matches exactly v in any letter case: every character of v is
// quoted, so v is matched as text, never as a pattern - an e-mail address
// as a directory spells it (#6788).
func exactFold(v string) bson.M {
	return bson.M{"$regex": "^" + regexp.QuoteMeta(v) + "$", "$options": "i"}
}

// containsFold matches text that contains v, in any letter case: v quoted,
// as exactFold's, so it is matched as text, never as a pattern.
func containsFold(v string) bson.M {
	return bson.M{"$regex": regexp.QuoteMeta(v), "$options": "i"}
}

// inIDs matches any of ids from a request, each of which must be an id; ok is
// false otherwise, and nothing is to be queried.
func inIDs(ids []string) (bson.M, bool) {
	for _, id := range ids {
		if !validID(id) {
			return nil, false
		}
	}
	return bson.M{"$in": ids}, true
}

func randomString(alphabet string, n int) string {
	b := make([]byte, n)
	max := big.NewInt(int64(len(alphabet)))
	for i := range b {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(err)
		}
		b[i] = alphabet[v.Int64()]
	}
	return string(b)
}

// Email is one of a user's addresses.
type Email struct {
	Address  string `bson:"address" json:"address"`
	Verified bool   `bson:"verified" json:"verified"`
}

// User is a document of the users collection - the fields used here.
type User struct {
	ID       string  `bson:"_id" json:"_id"`
	Username string  `bson:"username" json:"username"`
	Emails   []Email `bson:"emails,omitempty" json:"emails,omitempty"`
	IsAdmin  bool    `bson:"isAdmin,omitempty" json:"isAdmin,omitempty"`
	Profile  struct {
		Fullname string `bson:"fullname,omitempty" json:"fullname,omitempty"`
		Initials string `bson:"initials,omitempty" json:"initials,omitempty"`
		Language string `bson:"language,omitempty" json:"language,omitempty"`
		// DateFormat is WeKan's profile.dateFormat: YYYY-MM-DD and the rest.
		DateFormat string `bson:"dateFormat,omitempty" json:"dateFormat,omitempty"`
		// AvatarURL is WeKan's profile.avatarUrl: the user's uploaded picture.
		AvatarURL string `bson:"avatarUrl,omitempty" json:"avatarUrl,omitempty"`
		// StarredBoards are the user's favorites, WeKan's profile.starredBoards.
		StarredBoards []string `bson:"starredBoards,omitempty" json:"starredBoards,omitempty"`
		// InvitedBoards are the boards the user is invited to, server-written.
		InvitedBoards []string `bson:"invitedBoards,omitempty" json:"invitedBoards,omitempty"`
		// CardMaximized: the user opens a card full-screen (WeKan's profile.cardMaximized).
		CardMaximized bool `bson:"cardMaximized,omitempty" json:"cardMaximized,omitempty"`
		// CardCollapsed: the user folds the opened card's details (WeKan's profile.cardCollapsed).
		CardCollapsed bool `bson:"cardCollapsed,omitempty" json:"cardCollapsed,omitempty"`
		// CardSettingsSplit: the Minicard pane's share of Card Settings' width.
		CardSettingsSplit float64 `bson:"cardSettingsSplit,omitempty" json:"cardSettingsSplit,omitempty"`
		// SubmitOnEnter: Enter saves an editor and Shift+Enter is a new line
		// (WeKan's profile.submitOnEnter); off, Ctrl/Cmd+Enter saves.
		SubmitOnEnter bool `bson:"submitOnEnter,omitempty" json:"submitOnEnter,omitempty"`
		// ShowDesktopDragHandles: drag by a handle, not the whole item
		// (WeKan's profile.showDesktopDragHandles).
		ShowDesktopDragHandles bool `bson:"showDesktopDragHandles,omitempty" json:"showDesktopDragHandles,omitempty"`
		// CustomFieldsGrid: the card's custom fields one per row (WeKan's
		// profile.customFieldsGrid; its class says custom-fields-one-per-row).
		CustomFieldsGrid bool `bson:"customFieldsGrid,omitempty" json:"customFieldsGrid,omitempty"`
	} `bson:"profile,omitempty" json:"profile,omitempty"`
	Services struct {
		Password struct {
			Bcrypt string `bson:"bcrypt,omitempty"`
			Argon2 string `bson:"argon2,omitempty"`
		} `bson:"password,omitempty"`
	} `bson:"services,omitempty" json:"-"`
	LoginDisabled bool `bson:"loginDisabled,omitempty" json:"-"`
}

// Name is how the user is shown.
func (u *User) Name() string {
	if u.Profile.Fullname != "" {
		return u.Profile.Fullname
	}
	return u.Username
}

// Member is one entry of a board's members, with WeKan's role flags.
type Member struct {
	UserID             string `bson:"userId" json:"userId"`
	IsAdmin            bool   `bson:"isAdmin" json:"isAdmin"`
	IsActive           bool   `bson:"isActive" json:"isActive"`
	IsNoComments       bool   `bson:"isNoComments" json:"isNoComments"`
	IsCommentOnly      bool   `bson:"isCommentOnly" json:"isCommentOnly"`
	IsWorker           bool   `bson:"isWorker" json:"isWorker"`
	IsReadOnly         bool   `bson:"isReadOnly" json:"isReadOnly"`
	IsReadAssignedOnly bool   `bson:"isReadAssignedOnly" json:"isReadAssignedOnly"`
	// The two other assigned-only roles of WeKan's board schema.
	IsNormalAssignedOnly  bool `bson:"isNormalAssignedOnly,omitempty" json:"isNormalAssignedOnly,omitempty"`
	IsCommentAssignedOnly bool `bson:"isCommentAssignedOnly,omitempty" json:"isCommentAssignedOnly,omitempty"`
	// Rest are the member's other fields, kept as they are: WeKan's grantedBy
	// (#4178, the orgs and teams a membership came through - an empty list
	// still means group-derived) and whatever else WeKan writes. Weddy rewrites
	// the whole members list, so a field it did not know was lost, and a
	// group-derived member became a direct one that leaving the group no
	// longer removed.
	Rest map[string]any `bson:",inline" json:"-"`
}

// GrantedBy are the groups the membership came through, and whether it is
// group-derived at all (an empty list still is).
func (m Member) GrantedBy() ([]string, bool) {
	v, ok := m.Rest["grantedBy"]
	if !ok {
		return nil, false
	}
	var out []string
	switch a := v.(type) {
	case bson.A:
		for _, e := range a {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
	case []any:
		for _, e := range a {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
	case []string:
		out = a
	}
	return out, true
}

// What each board role may do: WeKan's models/lib/boardRoleCapabilities.js, the
// one table its server, client and documentation all read.
var roleCapabilities = map[string]map[string]bool{
	"board-admin":           {"seesAllCards": true, "comment": true, "write": true, "manageBoard": true, "moveCard": true},
	"normal":                {"seesAllCards": true, "comment": true, "write": true, "moveCard": true},
	"normal-assigned-only":  {"comment": true, "write": true, "moveCard": true},
	"no-comments":           {"seesAllCards": true, "write": true, "moveCard": true},
	"comment-only":          {"seesAllCards": true, "comment": true},
	"comment-assigned-only": {"comment": true},
	"worker":                {"seesAllCards": true, "comment": true, "moveCard": true},
	"read-only":             {"seesAllCards": true},
	"read-assigned-only":    {},
}

// Role is the member's role, in WeKan's order: the first flag set names it.
func (m Member) Role() string {
	switch {
	case !m.IsActive:
		return ""
	case m.IsAdmin:
		return "board-admin"
	case m.IsNormalAssignedOnly:
		return "normal-assigned-only"
	case m.IsNoComments:
		return "no-comments"
	case m.IsCommentOnly:
		return "comment-only"
	case m.IsCommentAssignedOnly:
		return "comment-assigned-only"
	case m.IsWorker:
		return "worker"
	case m.IsReadOnly:
		return "read-only"
	case m.IsReadAssignedOnly:
		return "read-assigned-only"
	}
	return "normal"
}

// AssignedOnly: the member sees and changes only the cards assigned to them.
func (m Member) AssignedOnly() bool {
	return m.IsNormalAssignedOnly || m.IsCommentAssignedOnly || m.IsReadAssignedOnly
}

// ReadsWholeBoard is WeKan's models/lib/boardReadAccess.js memberReadsWholeBoard:
// what its REST reads of a whole board ask (server/authentication.js
// checkBoardAccess). An assigned-only member is refused there (AssignedBleed),
// and so are the no-comments, comment-only and worker roles, as in WeKan.
func (m Member) ReadsWholeBoard() bool {
	return m.IsActive && !m.IsNoComments && !m.IsCommentOnly && !m.IsWorker && !m.AssignedOnly()
}

// member is userID's active entry of the board, or nil.
func (b *Board) member(userID string) *Member {
	for i, m := range b.Members {
		if m.UserID == userID && m.IsActive {
			return &b.Members[i]
		}
	}
	return nil
}

// Writable is WeKan's checkBoardWriteAccess, for one card or (card nil) the
// board: a site admin, or a member whose role has the capability ("write", or
// "moveCard" for a move) - an assigned-only member only on a card assigned to
// them, never on the board as a whole (GHSA-r4gm-mjj9-g3x8).
func (b *Board) Writable(u *User, capability string, card *Card) bool {
	if u == nil {
		return false
	}
	if u.IsAdmin {
		return true
	}
	m := b.member(u.ID)
	if m == nil || !roleCapabilities[m.Role()][capability] {
		return false
	}
	if m.AssignedOnly() {
		return card != nil && slices.Contains(card.Assignees, u.ID)
	}
	return true
}

// Can reports whether userID's role on the board has the capability.
func (b *Board) Can(userID, capability string) bool {
	m := b.member(userID)
	return m != nil && roleCapabilities[m.Role()][capability]
}

// Label is one of a board's labels.
type Label struct {
	ID    string `bson:"_id" json:"_id"`
	Name  string `bson:"name" json:"name"`
	Color string `bson:"color" json:"color"`
}

// Board is a document of the boards collection.
type Board struct {
	ID          string   `bson:"_id" json:"_id"`
	Title       string   `bson:"title" json:"title"`
	Description string   `bson:"description,omitempty" json:"description,omitempty"`
	Slug        string   `bson:"slug" json:"slug"`
	Color       string   `bson:"color,omitempty" json:"color,omitempty"`
	Archived    bool     `bson:"archived" json:"archived"`
	Permission  string   `bson:"permission" json:"permission"` // private | public
	Type        string   `bson:"type,omitempty" json:"type,omitempty"`
	Sort        float64  `bson:"sort,omitempty" json:"sort,omitempty"`
	Members     []Member `bson:"members" json:"members"`
	Labels      []Label  `bson:"labels,omitempty" json:"labels,omitempty"`
	// BackgroundImageURL is WeKan's: one of the board's image attachments.
	BackgroundImageURL string    `bson:"backgroundImageURL,omitempty" json:"backgroundImageURL,omitempty"`
	CreatedAt          time.Time `bson:"createdAt,omitempty" json:"createdAt,omitempty"`
	// AllowsDueCompleteOnMinicard is Card Settings' "Show at Minicard" of
	// "Mark as complete": off unless set.
	AllowsDueCompleteOnMinicard bool `bson:"allowsDueCompleteOnMinicard,omitempty" json:"allowsDueCompleteOnMinicard,omitempty"`
}

// AdminOf reports whether userID is an active board admin.
func (b *Board) AdminOf(userID string) bool {
	for _, m := range b.Members {
		if m.UserID == userID && m.IsActive && m.IsAdmin {
			return true
		}
	}
	return false
}

// MemberOf reports whether userID is an active member.
func (b *Board) MemberOf(userID string) bool {
	for _, m := range b.Members {
		if m.UserID == userID && m.IsActive {
			return true
		}
	}
	return false
}

// Swimlane is a document of the swimlanes collection.
type Swimlane struct {
	ID       string  `bson:"_id" json:"_id"`
	Title    string  `bson:"title" json:"title"`
	BoardID  string  `bson:"boardId" json:"boardId"`
	Sort     float64 `bson:"sort" json:"sort"`
	Archived bool    `bson:"archived" json:"archived"`
	Type     string  `bson:"type,omitempty" json:"type,omitempty"`
}

// List is a document of the lists collection. SwimlaneID is set for a list of
// one swimlane, and empty for a list every swimlane shows.
type List struct {
	ID         string  `bson:"_id" json:"_id"`
	Title      string  `bson:"title" json:"title"`
	BoardID    string  `bson:"boardId" json:"boardId"`
	SwimlaneID string  `bson:"swimlaneId,omitempty" json:"swimlaneId,omitempty"`
	Sort       float64 `bson:"sort" json:"sort"`
	Archived   bool    `bson:"archived" json:"archived"`
	Type       string  `bson:"type,omitempty" json:"type,omitempty"`
	// Watchers are the users who watch the list (WeKan's watchable list).
	Watchers []string `bson:"watchers,omitempty" json:"watchers,omitempty"`
}

// Card is a document of the cards collection.
type Card struct {
	ID          string   `bson:"_id" json:"_id"`
	Title       string   `bson:"title" json:"title"`
	Description string   `bson:"description,omitempty" json:"description,omitempty"`
	BoardID     string   `bson:"boardId" json:"boardId"`
	ListID      string   `bson:"listId" json:"listId"`
	SwimlaneID  string   `bson:"swimlaneId" json:"swimlaneId"`
	UserID      string   `bson:"userId,omitempty" json:"userId,omitempty"`
	Members     []string `bson:"members" json:"members"`
	Assignees   []string `bson:"assignees" json:"assignees"`
	LabelIDs    []string `bson:"labelIds" json:"labelIds"`
	Sort        float64  `bson:"sort" json:"sort"`
	Archived    bool     `bson:"archived" json:"archived"`
	// DueComplete is WeKan's dueComplete: the card is marked as complete.
	DueComplete bool      `bson:"dueComplete,omitempty" json:"dueComplete,omitempty"`
	Type        string    `bson:"type,omitempty" json:"type,omitempty"`
	CardNumber  int64     `bson:"cardNumber,omitempty" json:"cardNumber,omitempty"`
	CreatedAt   time.Time `bson:"createdAt,omitempty" json:"createdAt,omitempty"`
	// Color, RequestedBy, Locations and DateLastActivity are drawn by Card
	// Settings' sides (card_sides.go). They and the dates below are read from
	// the card's document by fillCardExtras, not decoded with the card: a
	// value of another type there (a date stored as text) must not make the
	// card itself unreadable.
	Color            string         `bson:"-" json:"-"`
	RequestedBy      string         `bson:"-" json:"-"`
	Locations        []cardLocation `bson:"-" json:"-"`
	DateLastActivity time.Time      `bson:"-" json:"-"`
	MinicardSides    minicardSides  `bson:"-" json:"-"`
	// The card's dates (WeKan's receivedAt, startAt, dueAt, endAt), and what
	// the minicard draws in the Minicard pane's order (minicard_order.go).
	ReceivedAt      *time.Time       `bson:"-" json:"-"`
	StartAt         *time.Time       `bson:"-" json:"-"`
	DueAt           *time.Time       `bson:"-" json:"-"`
	EndAt           *time.Time       `bson:"-" json:"-"`
	MinicardParts   []minicardPart   `bson:"-" json:"-"`
	MinicardDates   []minicardDate   `bson:"-" json:"-"`
	MinicardMembers []minicardPerson `bson:"-" json:"-"`
	// CoverID is WeKan's coverId: the image attachment shown on the minicard.
	CoverID string `bson:"coverId,omitempty" json:"coverId,omitempty"`
	// What the board's minicard shows of it, drawn only (minicard_badges.go).
	MinicardAssignees []minicardPerson `bson:"-" json:"-"`
	// Scrum is WeKan's Scrum metadata of the card (scrum.go), and
	// MinicardScrum its minicard's shown fields (scrum_fields.go).
	Scrum         bson.M          `bson:"scrum,omitempty" json:"-"`
	ScrumRevision float64         `bson:"scrumRevision,omitempty" json:"-"`
	MinicardScrum []scrumFieldRow `bson:"-" json:"-"`
	// MinicardIssueType is the work item type's badge on the minicard,
	// unless the board turned it off (allowsIssueTypeOnMinicard).
	MinicardIssueType *issueBadge `bson:"-" json:"-"`
	MinicardComments  []string    `bson:"-" json:"-"`
	// MinicardChecklists are its checklists shown on the minicard (WeKan's
	// showChecklistAtMinicard), with their items.
	MinicardChecklists []minicardChecklist `bson:"-" json:"-"`
	// MinicardCollapsed: the user folded this minicard (profile.collapsedCards).
	MinicardCollapsed    bool `bson:"-" json:"-"`
	MinicardMoreComments bool `bson:"-" json:"-"`
}
