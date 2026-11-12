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

func methodCall(t *testing.T, w *testWeKan, cl *http.Client, name, args string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/"+url.PathEscape(name), strings.NewReader(args))
	req.Header.Set("Content-Type", "application/json")
	res, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

// GroupLinkBleed (GHSA-v544-rmr5-38gx): setBoardTeams adds only the admin's
// own teams; a client /boards/update naming teams, or adding a member, is
// refused and recorded as GroupLinkBleed without disabling the account; a
// role change through it still works.
func TestGroupLinkBleed(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Teams")
	var ann User
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"username": "ann"}).Decode(&ann)
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": ann.ID}, bson.M{"$set": bson.M{"teams": bson.A{bson.M{"teamId": "own", "teamDisplayName": "Own"}}}})
	ac := w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	hasTeam := func(id string) bool {
		var d struct {
			Teams []boardTeam `bson:"teams"`
		}
		_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": rb.ID}).Decode(&d)
		for _, x := range d.Teams {
			if x.TeamID == id {
				return true
			}
		}
		return false
	}
	if _, out := methodCall(t, w, ac, "setBoardTeams", `[[{"teamId":"foreign","teamDisplayName":"x","isActive":true}],[],"`+rb.ID+`"]`); out["error"] != "not-authorized" {
		t.Fatalf("#5141: a team the admin is not in: %v", out)
	}
	write := func(mod string) map[string]any {
		_, out := methodCall(t, w, ac, "/boards/update", `[{"_id":"`+rb.ID+`"},`+mod+`]`)
		return out
	}
	if out := write(`{"$set":{"teams":[{"teamId":"foreign","teamDisplayName":"x","isActive":true}]}}`); out["reason"] != "Access denied" {
		t.Fatalf("the collection refuses the team: %v", out)
	}
	if hasTeam("foreign") {
		t.Fatal("no foreign team on the board")
	}
	if out := write(`{"$push":{"members":{"userId":"` + bob.ID + `","isAdmin":false,"isActive":true}}}`); out["reason"] != "Access denied" {
		t.Fatalf("the collection refuses a new member: %v", out)
	}
	var bd Board
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": rb.ID}).Decode(&bd)
	if bd.MemberOf(bob.ID) {
		t.Fatal("bob is no member")
	}
	var row bson.M
	if w.db.Collection("eventlog").FindOne(ctx, bson.M{"bleed": "GroupLinkBleed", "source": "ddp:boards.update"}).Decode(&row) != nil {
		t.Fatal("recorded as GroupLinkBleed")
	}
	if n, _ := row["count"].(int32); n < 2 {
		if n64, _ := row["count"].(int64); n64 < 2 {
			t.Errorf("both attempts counted: %v", row["count"])
		}
	}
	var me User
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"_id": ann.ID}).Decode(&me)
	if me.LoginDisabled {
		t.Fatal("medium never disables the account")
	}
	// Negative: the admin's own team is added, and a role change is written.
	if _, out := methodCall(t, w, ac, "setBoardTeams", `[[{"teamId":"own","teamDisplayName":"Own","isActive":true}],[],"`+rb.ID+`"]`); out["error"] != nil {
		t.Fatalf("own team: %v", out)
	}
	if !hasTeam("own") {
		t.Fatal("the own team is on the board")
	}
	if out := write(`{"$set":{"members.0.isWorker":true}}`); out["result"] != float64(1) {
		t.Fatalf("a role change is written: %v", out)
	}
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": rb.ID}).Decode(&bd)
	if !bd.Members[0].IsWorker {
		t.Fatal("isWorker set")
	}
	// Negatives of the allow rule.
	for name, mod := range map[string]string{
		"the last admin":   `{"$set":{"members.0.isAdmin":false}}`,
		"another field":    `{"$set":{"title":"x"}}`,
		"not a bool":       `{"$set":{"members.0.isWorker":"yes"}}`,
		"out of range":     `{"$set":{"members.9.isWorker":true}}`,
		"userId":           `{"$set":{"members.0.userId":"` + bob.ID + `"}}`,
		"a reactivation":   `{"$set":{"members.0.isActive":true}}`,
		"two operators":    `{"$set":{"members.0.isWorker":false},"$unset":{"title":""}}`,
		"a $rename":        `{"$rename":{"members":"x"}}`,
		"a domain renamed": `{"$rename":{"title":"domains"}}`,
	} {
		if out := write(mod); out["reason"] != "Access denied" {
			t.Errorf("negative: %s: %v", name, out)
		}
	}
	bc := w.client(t)
	body(t, w.signIn(t, bc, "bob", "pw"))
	if _, out := methodCall(t, w, bc, "/boards/update", `[{"_id":"`+rb.ID+`"},{"$set":{"members.0.isWorker":false}}]`); out["reason"] != "Access denied" {
		t.Errorf("negative: not a board admin: %v", out)
	}
	if _, out := methodCall(t, w, bc, "setBoardTeams", `[[],[],"`+rb.ID+`"]`); out["error"] != "not-authorized" {
		t.Errorf("negative: not a board admin's setBoardTeams: %v", out)
	}
	// An insert naming teams is recorded too.
	_, _ = methodCall(t, w, ac, "/boards/insert", `[{"title":"x","teams":[{"teamId":"foreign"}]}]`)
	if n, _ := w.db.Collection("eventlog").CountDocuments(ctx, bson.M{"bleed": "GroupLinkBleed", "source": "ddp:boards.insert"}); n != 1 {
		t.Errorf("the insert recorded: %d", n)
	}
}

// setBoardTeams reconciles the members as WeKan's (#5730): an active admin
// stays even when omitted, a listed member stays, an omitted one leaves its
// cards, and a new one comes in with its team as grantedBy (#4178) - but
// only when it is in one of the board's active teams (#6116).
func TestSetBoardTeamsMembers(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	kept := w.seedUser(t, "kept", "pw")
	gone := w.seedUser(t, "gone", "pw")
	inTeam := w.seedUser(t, "inteam", "pw")
	stranger := w.seedUser(t, "stranger", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Teams")
	var ann User
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"username": "ann"}).Decode(&ann)
	_, _ = w.db.Collection("users").UpdateMany(ctx, bson.M{"_id": bson.M{"$in": bson.A{ann.ID, inTeam.ID}}},
		bson.M{"$set": bson.M{"teams": bson.A{bson.M{"teamId": "t1", "teamDisplayName": "T1"}}}})
	for _, u := range []*User{kept, gone} {
		_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": rb.ID}, bson.M{"$push": bson.M{"members": bson.M{"userId": u.ID, "isActive": true, "isAdmin": false}}})
	}
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": rb.Card}, bson.M{"$set": bson.M{"members": bson.A{gone.ID, kept.ID}, "watchers": bson.A{gone.ID}}})
	ac := w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	members := `[{"userId":"` + kept.ID + `","isAdmin":false,"isActive":true},{"userId":"` + inTeam.ID + `","isAdmin":false,"isActive":true},{"userId":"` + stranger.ID + `","isAdmin":true,"isActive":true}]`
	if _, out := methodCall(t, w, ac, "setBoardTeams", `[[{"teamId":"t1","teamDisplayName":"T1","isActive":true}],`+members+`,"`+rb.ID+`"]`); out["error"] != nil {
		t.Fatalf("set: %v", out)
	}
	var bd Board
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": rb.ID}).Decode(&bd)
	if !bd.AdminOf(ann.ID) || !bd.MemberOf(kept.ID) {
		t.Fatal("the omitted active admin and the listed member stay")
	}
	if bd.MemberOf(gone.ID) {
		t.Fatal("the omitted member leaves")
	}
	if bd.MemberOf(stranger.ID) {
		t.Fatal("negative (#6116): a user in none of the board's teams does not come in")
	}
	found := false
	for _, m := range bd.Members {
		if m.UserID == inTeam.ID {
			found = true
			if g, ok := m.GrantedBy(); !ok || len(g) != 1 || g[0] != "team:t1" {
				t.Errorf("#4178: grantedBy team:t1: %v", m.Rest)
			}
		}
	}
	if !found {
		t.Fatal("the team's user comes in")
	}
	var card bson.M
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": rb.Card}).Decode(&card)
	if m, _ := card["members"].(bson.A); len(m) != 1 || m[0] != kept.ID {
		t.Errorf("the leaver is off the card: %v", card["members"])
	}
	if wa, _ := card["watchers"].(bson.A); len(wa) != 0 {
		t.Errorf("and off its watchers: %v", wa)
	}
	if n, _ := w.db.Collection("activities").CountDocuments(ctx, bson.M{"activityType": "removeBoardMember", "memberId": gone.ID}); n != 1 {
		t.Errorf("the removal is in the activities: %d", n)
	}
	if f := verifyWithWeKan(t, historyRows(t, w, rb.ID)); len(f) != 0 {
		t.Errorf("WeKan's audit: %v", f)
	}
	if _, out := methodCall(t, w, ac, "setBoardTeams", `[[{"teamId":1}],[],"`+rb.ID+`"]`); out["error"] != "Match.Error" {
		t.Errorf("negative: a malformed team: %v", out)
	}
}

// addsBoardMember is WeKan's boardWriteGuards.js addsBoardMember, case by case.
func TestAddsBoardMember(t *testing.T) {
	cur := []Member{{UserID: "a", IsActive: true, IsAdmin: true}, {UserID: "gone", IsActive: false}}
	for name, c := range map[string]struct {
		mod  string
		adds bool
	}{
		"a role flag":          {`{"$set":{"members.0.isWorker":true}}`, false},
		"a pull":               {`{"$pull":{"members":{"userId":"a"}}}`, false},
		"a push":               {`{"$push":{"members":{"userId":"b"}}}`, true},
		"an addToSet":          {`{"$addToSet":{"members":{"userId":"b"}}}`, true},
		"the whole list, same": {`{"$set":{"members":[{"userId":"a","isActive":true}]}}`, false},
		"the whole list, new":  {`{"$set":{"members":[{"userId":"b","isActive":true}]}}`, true},
		"a reactivation":       {`{"$set":{"members.1.isActive":true}}`, true},
		"a slot, other user":   {`{"$set":{"members.0":{"userId":"b"}}}`, true},
		"a userId":             {`{"$set":{"members.0.userId":"b"}}`, true},
		"past the end":         {`{"$set":{"members.2.isAdmin":true}}`, true},
		"not an index":         {`{"$set":{"members.x.isAdmin":true}}`, true},
		"a rename":             {`{"$rename":{"title":"members"}}`, true},
		"no members":           {`{"$set":{"title":"x"}}`, false},
	} {
		var mod map[string]any
		_ = json.Unmarshal([]byte(c.mod), &mod)
		if got := addsBoardMember(cur, mod); got != c.adds {
			t.Errorf("%s: %v", name, got)
		}
	}
	if !isGroupLinkField("teams.0.teamId") || !isGroupLinkField("domains") || isGroupLinkField("teamsX") {
		t.Error("isGroupLinkField")
	}
	if got := forbiddenNewGroupIDs([]string{"old"}, []string{"old", "own", "x"}, []string{"own"}, false); len(got) != 1 || got[0] != "x" {
		t.Errorf("forbiddenNewGroupIDs: %v", got)
	}
	if forbiddenNewGroupIDs(nil, []string{"x"}, nil, true) != nil {
		t.Error("a site admin adds any")
	}
}

// setBoardOrgs adds only the admin's own organizations (#5141), any for a
// site admin; setBoardDomains, addBoardDomain and removeBoardDomain keep
// valid domains, normalized, once (#5850); none of them is anybody's but a
// board admin's, and a collection write of either field is GroupLinkBleed.
func TestBoardOrgsAndDomains(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	w.seedUser(t, "bob", "pw")
	root := w.seedUser(t, "root", "pw")
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": root.ID}, bson.M{"$set": bson.M{"isAdmin": true}})
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Groups")
	var ann User
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"username": "ann"}).Decode(&ann)
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": ann.ID}, bson.M{"$set": bson.M{"orgs": bson.A{bson.M{"orgId": "myOrg", "orgDisplayName": "Mine"}}}})
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": rb.ID}, bson.M{"$push": bson.M{"members": bson.M{"userId": root.ID, "isActive": true, "isAdmin": true}}})
	ac := w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	org := func(id string) string { return `{"orgId":"` + id + `","orgDisplayName":"x","isActive":true}` }
	if _, out := methodCall(t, w, ac, "setBoardOrgs", `[[`+org("theirOrg")+`],"`+rb.ID+`"]`); out["error"] != "not-authorized" {
		t.Errorf("#5141: an org the admin is not in: %v", out)
	}
	if _, out := methodCall(t, w, ac, "setBoardOrgs", `[[`+org("myOrg")+`],"`+rb.ID+`"]`); out["error"] != nil {
		t.Errorf("their own org: %v", out)
	}
	rc := w.client(t)
	body(t, w.signIn(t, rc, "root", "pw"))
	if _, out := methodCall(t, w, rc, "setBoardOrgs", `[[`+org("myOrg")+`,`+org("theirOrg")+`],"`+rb.ID+`"]`); out["error"] != nil {
		t.Errorf("a site admin adds any: %v", out)
	}
	var stored struct {
		Orgs    []boardOrg    `bson:"orgs"`
		Domains []boardDomain `bson:"domains"`
	}
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": rb.ID}).Decode(&stored)
	if len(stored.Orgs) != 2 {
		t.Errorf("both orgs: %+v", stored.Orgs)
	}
	if _, out := methodCall(t, w, ac, "setBoardDomains", `[[{"domain":"  Example.COM ","isActive":true},{"domain":"example.com","isActive":false},{"domain":"b.org","isActive":false}],"`+rb.ID+`"]`); out["error"] != nil {
		t.Fatalf("set domains: %v", out)
	}
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": rb.ID}).Decode(&stored)
	if len(stored.Domains) != 2 || stored.Domains[0] != (boardDomain{"example.com", true}) || stored.Domains[1] != (boardDomain{"b.org", false}) {
		t.Errorf("normalized, once: %+v", stored.Domains)
	}
	for _, bad := range []string{"nodot", "a@b.com", "a b.com", ""} {
		if _, out := methodCall(t, w, ac, "setBoardDomains", `[[{"domain":"`+bad+`","isActive":true}],"`+rb.ID+`"]`); out["error"] != "invalid-domain" {
			t.Errorf("negative: %q: %v", bad, out)
		}
	}
	_, _ = methodCall(t, w, ac, "addBoardDomain", `[" New.Example ","`+rb.ID+`"]`)
	_, _ = methodCall(t, w, ac, "addBoardDomain", `["new.example","`+rb.ID+`"]`)
	_, _ = methodCall(t, w, ac, "removeBoardDomain", `["B.ORG","`+rb.ID+`"]`)
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": rb.ID}).Decode(&stored)
	if len(stored.Domains) != 2 || stored.Domains[1] != (boardDomain{"new.example", true}) {
		t.Errorf("added once, removed: %+v", stored.Domains)
	}
	bc := w.client(t)
	body(t, w.signIn(t, bc, "bob", "pw"))
	for _, c := range []string{`setBoardOrgs`, `setBoardDomains`} {
		if _, out := methodCall(t, w, bc, c, `[[],"`+rb.ID+`"]`); out["error"] != "not-authorized" {
			t.Errorf("negative: %s by a non-admin: %v", c, out)
		}
	}
	if _, out := methodCall(t, w, bc, "addBoardDomain", `["x.example","`+rb.ID+`"]`); out["error"] != "not-authorized" {
		t.Errorf("negative: addBoardDomain by a non-admin: %v", out)
	}
	// The collection write of either is GroupLinkBleed.
	_, _ = w.db.Collection("eventlog").DeleteMany(ctx, bson.M{"bleed": "GroupLinkBleed"})
	for _, mod := range []string{`{"$set":{"orgs":[]}}`, `{"$set":{"domains.0.isActive":false}}`} {
		if _, out := methodCall(t, w, ac, "/boards/update", `[{"_id":"`+rb.ID+`"},`+mod+`]`); out["reason"] != "Access denied" {
			t.Errorf("the collection refuses %s: %v", mod, out)
		}
	}
	if n, _ := w.db.Collection("eventlog").CountDocuments(ctx, bson.M{"bleed": "GroupLinkBleed"}); n == 0 {
		t.Error("recorded as GroupLinkBleed")
	}
}
