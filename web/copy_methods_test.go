package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// copyBoard and copyCard: what the caller may change is the copy's, and an
// identity - an _id, or any other field outside the contract - is
// CopyIdentityBleed, refused before anything is read and recorded; a wrong
// type on a supported field is refused unrecorded (negative).
func TestCopyMethods(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "eve", "pw")
	victim := newRESTBoard(t, w, login(t, w, "eve"), "Private copy fixture")
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": victim.Card}, bson.M{"$set": bson.M{"title": "PRIVATE-COPY-FIXTURE"}})
	n := 0
	user := func() (*User, restBoard, func(method, args string) map[string]any) {
		n++
		name := "copier" + string(rune('a'+n))
		u := w.seedUser(t, name, "pw")
		rb := newRESTBoard(t, w, login(t, w, name), "Source "+name)
		c := w.client(t)
		body(t, w.signIn(t, c, name, "pw"))
		return u, rb, func(method, args string) map[string]any {
			req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/"+url.PathEscape(method), strings.NewReader(args))
			req.Header.Set("Content-Type", "application/json")
			res, err := c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			var out map[string]any
			_ = json.NewDecoder(res.Body).Decode(&out)
			return out
		}
	}
	bleeds := func() int64 {
		n, _ := w.db.Collection("eventlog").CountDocuments(ctx, bson.M{"bleed": "CopyIdentityBleed"})
		return n
	}
	boardsOf := func(u *User) int64 {
		n, _ := w.db.Collection("boards").CountDocuments(ctx, bson.M{"members.userId": u.ID})
		return n
	}
	privateCopied := func() bool {
		n, _ := w.db.Collection("cards").CountDocuments(ctx, bson.M{"title": "PRIVATE-COPY-FIXTURE", "boardId": bson.M{"$ne": victim.ID}})
		return n > 0
	}

	t.Run("copyBoard", func(t *testing.T) {
		u, rb, call := user()
		before := boardsOf(u)
		for _, props := range []string{`{"title":42}`, `{"sort":"1"}`, `{"sort":null}`, `{"type":"invalid"}`, `{"withoutCards":"false"}`, `{"title":null}`} {
			if out := call("copyBoard", `["`+rb.ID+`",`+props+`]`); out["error"] != "invalid-copy-properties" {
				t.Errorf("%s: %v", props, out)
			}
		}
		if out := call("copyBoard", `["`+rb.ID+`",{"copyOptions":{"cards":"false"}}]`); out["error"] != "invalid-copy-options" {
			t.Errorf("a string option: %v", out)
		}
		if out := call("copyBoard", `["`+rb.ID+`",{"copyOptions":{"everything":true}}]`); out["error"] != "invalid-copy-options" {
			t.Errorf("an unknown option: %v", out)
		}
		if boardsOf(u) != before || bleeds() != 0 {
			t.Fatalf("negative: malformed properties copied a board or were recorded: %d boards, %d rows", boardsOf(u)-before, bleeds())
		}
		var orig bson.M
		_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": rb.ID}).Decode(&orig)
		for _, props := range []string{`{}`, `{"title":"Renamed copy","sort":20,"type":"board","withoutCards":true}`, `{"copyOptions":{"cards":false,"scrum":false}}`} {
			out := call("copyBoard", `["`+rb.ID+`",`+props+`]`)
			id, _ := out["result"].(string)
			var made bson.M
			if id == "" || w.db.Collection("boards").FindOne(ctx, bson.M{"_id": id}).Decode(&made) != nil {
				t.Fatalf("%s: %v", props, out)
			}
			if a, b := toJSON(made["members"]), toJSON(orig["members"]); a != b {
				t.Errorf("%s: the copy's members %s, the source's %s", props, a, b)
			}
			if made["permission"] != orig["permission"] || made["withoutCards"] != nil || made["copyOptions"] != nil {
				t.Errorf("%s: the copy %v", props, made)
			}
			cards, _ := w.db.Collection("cards").CountDocuments(ctx, bson.M{"boardId": id})
			if strings.Contains(props, "Cards") || strings.Contains(props, "cards") {
				if cards != 0 {
					t.Errorf("%s copied %d cards", props, cards)
				}
			} else if cards == 0 {
				t.Errorf("%s copied no cards", props)
			}
			if strings.Contains(props, "Renamed") && (made["title"] != "Renamed copy" || made["sort"] != 20.0) {
				t.Errorf("title and sort: %v %v", made["title"], made["sort"])
			}
		}
		// The attack: the source's identity replaced by a private board's.
		before = boardsOf(u)
		if out := call("copyBoard", `["`+rb.ID+`",{"_id":"`+victim.ID+`"}]`); out["error"] != "invalid-copy-properties" {
			t.Errorf("an _id: %v", out)
		}
		if boardsOf(u) != before || privateCopied() || bleeds() == 0 {
			t.Errorf("the attack: %d boards made, private copied %v, %d rows", boardsOf(u)-before, privateCopied(), bleeds())
		}
	})

	t.Run("copyCard", func(t *testing.T) {
		u, rb, call := user()
		before := bleeds()
		dest := `"` + rb.ID + `","` + rb.Swimlane + `","` + rb.List + `",false`
		out := call("copyCard", `["`+rb.Card+`",`+dest+`,{"title":"Verified copy","description":"Independent text"}]`)
		id, _ := out["result"].(string)
		var made bson.M
		if id == "" || w.db.Collection("cards").FindOne(ctx, bson.M{"_id": id}).Decode(&made) != nil || made["title"] != "Verified copy" || made["boardId"] != rb.ID {
			t.Fatalf("the copy dialog's copy: %v %v", out, made)
		}
		if out := call("copyCard", `["`+rb.Card+`",`+dest+`,{"title":7}]`); out["error"] != "bad-request" || bleeds() != before {
			t.Errorf("negative: a number as a title: %v, recorded %v", out, bleeds() != before)
		}
		if out := call("copyCard", `["`+rb.Card+`","`+rb.ID+`","`+victim.Swimlane+`","`+victim.List+`",false,{}]`); out["error"] != "invalid-copy-destination" {
			t.Errorf("negative: another board's list as the destination: %v", out)
		}
		if out := call("copyCard", `["`+victim.Card+`",`+dest+`,{}]`); out["error"] != "not-authorized" {
			t.Errorf("negative: a card of a board the caller is not on: %v", out)
		}
		if out := call("copyCard", `["`+rb.Card+`",`+dest+`,{"_id":"`+victim.Card+`"}]`); out["error"] != "bad-request" {
			t.Errorf("an _id: %v", out)
		}
		if privateCopied() || bleeds() == before {
			t.Errorf("the attack: private copied %v, recorded %v", privateCopied(), bleeds() != before)
		}
		var d struct {
			LoginDisabled bool `bson:"loginDisabled"`
		}
		_ = w.db.Collection("users").FindOne(ctx, bson.M{"_id": u.ID}).Decode(&d)
		if !d.LoginDisabled {
			t.Error("CopyIdentityBleed is high: the sender's account is disabled")
		}
	})
}
