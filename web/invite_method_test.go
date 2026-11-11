package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// inviteUserToBoard: by username, by e-mail in any letter case, an active
// member left as they are, a removed one back with a new activity, and a new
// address as a new account - and nobody else joins (negative).
func TestInviteUserToBoardMethod(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	w.seedUser(t, "eve", "pw")
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": bob.ID}, bson.M{"$set": bson.M{"emails": bson.A{bson.M{"address": "Bob.Builder@Example.COM", "verified": true}}}})
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Invites")
	caller := func(name string) func(method, args string) map[string]any {
		c := w.client(t)
		body(t, w.signIn(t, c, name, "pw"))
		return func(method, args string) map[string]any {
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
	ann, eve := caller("ann"), caller("eve")
	invite := func(call func(string, string) map[string]any, who string) map[string]any {
		return call("inviteUserToBoard", `["`+who+`","`+rb.ID+`"]`)
	}
	members := func() []Member {
		var b Board
		_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": rb.ID}).Decode(&b)
		return b.Members
	}
	active := func(id string) bool {
		for _, m := range members() {
			if m.UserID == id && m.IsActive {
				return true
			}
		}
		return false
	}
	activities := func(id string) int64 {
		n, _ := w.db.Collection("activities").CountDocuments(ctx, bson.M{"boardId": rb.ID, "memberId": id, "activityType": "addBoardMember"})
		return n
	}
	if out := invite(eve, "bob"); out["error"] != "error-board-notAMember" || active(bob.ID) {
		t.Errorf("negative: somebody not on the board: %v", out)
	}
	// A similar address is not Bob's (registration open: it is a new account).
	if out := invite(ann, "x"+"bob.builder@example.com"); active(bob.ID) {
		t.Errorf("negative: a similar address is not Bob's: %v", out)
	}
	if out := invite(ann, "bob.builder@example.com"); out["error"] != nil || !active(bob.ID) || activities(bob.ID) != 1 {
		t.Fatalf("by e-mail in another case: %v", out)
	}
	deadline := time.Now().Add(10 * time.Second)
	bell := func() int {
		var d struct {
			Profile struct {
				Notifications []bson.M `bson:"notifications"`
			} `bson:"profile"`
		}
		_ = w.db.Collection("users").FindOne(ctx, bson.M{"_id": bob.ID}).Decode(&d)
		return len(d.Profile.Notifications)
	}
	for bell() == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if bell() != 1 {
		t.Errorf("the membership activity is in Bob's bell: %d", bell())
	}
	// Negative: a new account has nothing to sign in to; it is mailed instead.
	var xb struct {
		Profile struct {
			Notifications []bson.M `bson:"notifications"`
		} `bson:"profile"`
	}
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"emails.address": "xbob.builder@example.com"}).Decode(&xb)
	if len(xb.Profile.Notifications) != 0 {
		t.Error("negative: a new account got a bell entry")
	}
	if out := invite(ann, "bob"); out["error"] != nil || activities(bob.ID) != 1 {
		t.Errorf("negative: an active member again: %v, %d activities", out, activities(bob.ID))
	}
	// Removed, then invited again: a new activity.
	ms := members()
	for i := range ms {
		if ms[i].UserID == bob.ID {
			ms[i].IsActive = false
		}
	}
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": rb.ID}, bson.M{"$set": bson.M{"members": ms}})
	if out := invite(ann, "bob"); out["error"] != nil || !active(bob.ID) || activities(bob.ID) != 2 {
		t.Errorf("re-invited: %v, %d activities", out, activities(bob.ID))
	}
	// Negative: an invitee who turned the bell off gets the membership, no entry.
	cid := w.seedUser(t, "cid", "pw")
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": cid.ID}, bson.M{"$set": bson.M{"profile.notifyOverrideTray": false}})
	if out := invite(ann, "cid"); out["error"] != nil || activities(cid.ID) != 1 {
		t.Fatalf("cid invited: %v", out)
	}
	var cd struct {
		Profile struct {
			Notifications []bson.M `bson:"notifications"`
		} `bson:"profile"`
	}
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"_id": cid.ID}).Decode(&cd)
	if len(cd.Profile.Notifications) != 0 {
		t.Error("negative: the bell was off")
	}
	// A new address: a new account, and only it joins.
	before := len(members())
	out := invite(ann, "new.person@example.invalid")
	var nu User
	if out["error"] != nil || w.db.Collection("users").FindOne(ctx, bson.M{"emails.address": "new.person@example.invalid"}).Decode(&nu) != nil || !active(nu.ID) || len(members()) != before+1 {
		t.Fatalf("a new address: %v", out)
	}
	if n, _ := w.db.Collection("users").CountDocuments(ctx, bson.M{"emails.address": bson.M{"$regex": "^new.person@", "$options": "i"}}); n != 1 {
		t.Errorf("negative: one account for the address: %d", n)
	}
	// Registration closed: no account for an unknown address.
	_, _ = w.db.Collection("settings").UpdateOne(ctx, bson.M{}, bson.M{"$set": bson.M{"disableRegistration": true}})
	if out := invite(ann, "closed@example.invalid"); out["error"] != "error-user-notCreated" {
		t.Errorf("negative: registration closed: %v", out)
	}
	if out := invite(ann, "no/slash"); out["result"] != false {
		t.Errorf("negative: a slash: %v", out)
	}
}
