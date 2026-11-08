package web

import (
	"context"
	"net/url"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// #4178: a membership that came through an org or team keeps its grantedBy -
// an empty one too - and any other field WeKan writes, whenever Weddy
// rewrites the board's members. Lost, a group-derived member became direct,
// and leaving the group no longer took the board away.
func TestMembersKeepWeKansFields(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	viaOrg := w.seedUser(t, "viaorg", "pw")
	lapsed := w.seedUser(t, "lapsed", "pw")
	gone := w.seedUser(t, "gone", "pw")
	a := login(t, w, "ann")
	b := newRESTBoard(t, w, a, "Groups")
	ctx := context.Background()
	var board bson.M
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": b.ID}).Decode(&board)
	members := board["members"].(bson.A)
	members = append(members,
		bson.M{"userId": viaOrg.ID, "isActive": true, "isAdmin": false, "grantedBy": bson.A{"org:ORG1"}, "futureField": "kept"},
		bson.M{"userId": lapsed.ID, "isActive": false, "isAdmin": false, "grantedBy": bson.A{}},
		bson.M{"userId": gone.ID, "isActive": true, "isAdmin": false})
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"members": members}})

	// The struct, round trip.
	var bd Board
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": b.ID}).Decode(&bd)
	if g, ok := bd.member(viaOrg.ID).GrantedBy(); !ok || len(g) != 1 || g[0] != "org:ORG1" {
		t.Fatalf("read: %v %v", g, ok)
	}
	// A real rewrite: a board admin removes another member from the sidebar.
	c := w.client(t)
	body(t, w.signIn(t, c, "ann", "pw"))
	res, err := c.PostForm(w.srv.URL+"/b/"+b.ID+"/members/"+gone.ID+"/remove", url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": b.ID}).Decode(&board)
	byUser := map[string]bson.M{}
	for _, m := range board["members"].(bson.A) {
		d := bson.M(docMap(m.(bson.D)))
		byUser[d["userId"].(string)] = d
	}
	if g, ok := byUser[viaOrg.ID]["grantedBy"].(bson.A); !ok || len(g) != 1 || g[0] != "org:ORG1" || byUser[viaOrg.ID]["futureField"] != "kept" {
		t.Errorf("negative: the org's grant and WeKan's other fields survive: %v", byUser[viaOrg.ID])
	}
	if g, ok := byUser[lapsed.ID]["grantedBy"].(bson.A); !ok || len(g) != 0 {
		t.Errorf("negative: an empty grantedBy stays - still group-derived: %v", byUser[lapsed.ID])
	}
}
