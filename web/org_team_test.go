package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestOrgTeamFeatures(t *testing.T) {
	w := newTestWeKan(t)
	admin := w.seedUser(t, "boss", "pw") // the first user is the site admin
	_, _ = w.db.Collection("users").UpdateOne(context.Background(), bson.M{"_id": admin.ID}, bson.M{"$set": bson.M{"isAdmin": true}})
	w.seedUser(t, "ann", "pw")
	inOrg := w.seedUser(t, "inorg", "pw")
	direct := w.seedUser(t, "direct", "pw")
	viaTeam := w.seedUser(t, "viateam", "pw")
	a := login(t, w, "ann")
	ctx := context.Background()
	_, _ = w.db.Collection("org").InsertMany(ctx, []any{bson.M{"_id": "orgone0000000001", "orgDisplayName": "One", "orgIsActive": true},
		bson.M{"_id": "orgtwo0000000001", "orgDisplayName": "Two", "orgIsActive": true}})
	_, _ = w.db.Collection("team").InsertOne(ctx, bson.M{"_id": "teamone000000001", "teamDisplayName": "T", "teamIsActive": true})
	for _, uid := range []string{inOrg.ID, direct.ID, viaTeam.ID} {
		_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": uid}, bson.M{"$set": bson.M{"orgs": bson.A{bson.M{"orgId": "orgone0000000001"}}}})
	}
	board := newRESTBoard(t, w, a, "Org board")
	tmpl := newRESTBoard(t, w, a, "Template")
	off := newRESTBoard(t, w, a, "Org inactive")
	listed := bson.A{bson.M{"orgId": "orgone0000000001", "orgDisplayName": "One", "isActive": true}}
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": board.ID}, bson.M{"$set": bson.M{"orgs": listed}})
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": tmpl.ID}, bson.M{"$set": bson.M{"orgs": listed, "type": "template-board"}})
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": off.ID}, bson.M{"$set": bson.M{"orgs": bson.A{bson.M{"orgId": "orgone0000000001", "isActive": false}}}})
	// direct is a member already, directly; viateam came through a team.
	setMember(t, w, board.ID, Member{UserID: direct.ID, IsActive: true})
	setMember(t, w, board.ID, Member{UserID: viaTeam.ID, IsActive: true, Rest: map[string]any{"grantedBy": bson.A{"team:teamone000000001"}}})
	client := func(name string) func(m, args string) map[string]any {
		c := w.client(t)
		body(t, w.signIn(t, c, name, "pw"))
		return func(m, args string) map[string]any {
			req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/"+m, strings.NewReader(args))
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
	asAdmin, asAnn := client("boss"), client("ann")
	flag := func(coll, id, f string) any {
		var d bson.M
		_ = w.db.Collection(coll).FindOne(ctx, bson.M{"_id": id}).Decode(&d)
		return d[f]
	}
	// Non-admin: a silent no-op, and the all-at-once call refused.
	if out := asAnn("setOrgSharedTemplates", `[{"_id":"orgone0000000001"},true]`); out["error"] != nil || flag("org", "orgone0000000001", "orgSharedTemplates") != nil {
		t.Errorf("negative: a non-admin changes nothing: %v", out)
	}
	if out := asAnn("setAllOrgsFeature", `["orgSharedTemplates",true]`); out["error"] != "not-authorized" {
		t.Errorf("negative: all at once by a non-admin: %v", out)
	}
	for _, m := range []string{"setOrgSharedTemplates", "setOrgSyncMembersFromAuth"} {
		if out := asAdmin(m, `[{"_id":"orgone0000000001"},true]`); out["error"] != nil {
			t.Errorf("%s: %v", m, out)
		}
	}
	if flag("org", "orgone0000000001", "orgSharedTemplates") != true || flag("org", "orgone0000000001", "orgSyncMembersFromAuth") != true {
		t.Error("the admin sets them")
	}
	if out := asAdmin("setAllOrgsFeature", `["orgSharedTemplates",false]`); out["error"] != nil || flag("org", "orgtwo0000000001", "orgSharedTemplates") != false {
		t.Errorf("all orgs: %v", out)
	}
	if out := asAdmin("setAllTeamsFeature", `["teamSharedTemplates",true]`); out["error"] != nil || flag("team", "teamone000000001", "teamSharedTemplates") != true {
		t.Errorf("all teams: %v", out)
	}
	if out := asAdmin("setAllOrgsFeature", `["isAdmin",true]`); out["error"] != "invalid-field" {
		t.Errorf("negative: a field that is no feature: %v", out)
	}
	if out := asAdmin("setOrgSharedTemplates", `[{"_id":{"$ne":"x"}},true]`); out["error"] != "Match.Error" {
		t.Errorf("negative: an operator for a selector: %v", out)
	}
	// Propagation.
	if out := asAdmin("setOrgPropagateMembersToBoards", `[{"_id":"orgone0000000001"},true]`); out["error"] != nil {
		t.Fatalf("propagate: %v", out)
	}
	var b, tb, ob Board
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": board.ID}).Decode(&b)
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": tmpl.ID}).Decode(&tb)
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": off.ID}).Decode(&ob)
	if m := b.member(inOrg.ID); m == nil {
		t.Error("the org's member joins the board")
	} else if g, ok := m.GrantedBy(); !ok || len(g) != 1 || g[0] != "org:orgone0000000001" || m.IsAdmin {
		t.Errorf("through the org, with no role: %v", m)
	}
	if _, derived := b.member(direct.ID).GrantedBy(); derived {
		t.Error("negative: a direct member stays direct")
	}
	if g, _ := b.member(viaTeam.ID).GrantedBy(); len(g) != 2 {
		t.Errorf("a group member gains the org's grant too: %v", g)
	}
	if tb.member(inOrg.ID) != nil || ob.member(inOrg.ID) != nil {
		t.Error("negative: never a template board, never where the org is inactive")
	}
}
