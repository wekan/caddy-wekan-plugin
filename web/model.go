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
}

// Label is one of a board's labels.
type Label struct {
	ID    string `bson:"_id" json:"_id"`
	Name  string `bson:"name" json:"name"`
	Color string `bson:"color" json:"color"`
}

// Board is a document of the boards collection.
type Board struct {
	ID         string    `bson:"_id" json:"_id"`
	Title      string    `bson:"title" json:"title"`
	Slug       string    `bson:"slug" json:"slug"`
	Color      string    `bson:"color,omitempty" json:"color,omitempty"`
	Archived   bool      `bson:"archived" json:"archived"`
	Permission string    `bson:"permission" json:"permission"` // private | public
	Type       string    `bson:"type,omitempty" json:"type,omitempty"`
	Sort       float64   `bson:"sort,omitempty" json:"sort,omitempty"`
	Members    []Member  `bson:"members" json:"members"`
	Labels     []Label   `bson:"labels,omitempty" json:"labels,omitempty"`
	CreatedAt  time.Time `bson:"createdAt,omitempty" json:"createdAt,omitempty"`
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
}

// Card is a document of the cards collection.
type Card struct {
	ID          string    `bson:"_id" json:"_id"`
	Title       string    `bson:"title" json:"title"`
	Description string    `bson:"description,omitempty" json:"description,omitempty"`
	BoardID     string    `bson:"boardId" json:"boardId"`
	ListID      string    `bson:"listId" json:"listId"`
	SwimlaneID  string    `bson:"swimlaneId" json:"swimlaneId"`
	UserID      string    `bson:"userId,omitempty" json:"userId,omitempty"`
	Members     []string  `bson:"members" json:"members"`
	Assignees   []string  `bson:"assignees" json:"assignees"`
	LabelIDs    []string  `bson:"labelIds" json:"labelIds"`
	Sort        float64   `bson:"sort" json:"sort"`
	Archived    bool      `bson:"archived" json:"archived"`
	Type        string    `bson:"type,omitempty" json:"type,omitempty"`
	CardNumber  int64     `bson:"cardNumber,omitempty" json:"cardNumber,omitempty"`
	CreatedAt   time.Time `bson:"createdAt,omitempty" json:"createdAt,omitempty"`
}
