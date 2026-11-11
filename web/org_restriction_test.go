package web

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// #6116: with "only from the same Organization" on, a member adds only a user
// who shares an Organization with them or with another active member - by
// inviteUserToBoard and by the Members widget - and a site admin anybody; the
// search offers only those, and never a disabled account (#1894), finding a
// colleague behind five who do not qualify (#6788).
func TestBoardMemberOrgRestrictionAndSearch(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	boss := w.seedUser(t, "boss", "pw")
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": boss.ID}, bson.M{"$set": bson.M{"isAdmin": true}})
	org := func(name, orgID string) *User {
		u := w.seedUser(t, name, "pw")
		_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": u.ID}, bson.M{"$set": bson.M{"orgs": bson.A{bson.M{"orgId": orgID, "orgDisplayName": orgID}}}})
		return u
	}
	ann, _ := w.db.Collection("users").Find(ctx, bson.M{"username": "ann"})
	_ = ann
	var annU User
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"username": "ann"}).Decode(&annU)
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": annU.ID}, bson.M{"$set": bson.M{"orgs": bson.A{bson.M{"orgId": "o1"}}}})
	same, other := org("same", "o1"), org("other", "o2")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Restricted")
	setMember(t, w, rb.ID, Member{UserID: boss.ID, IsActive: true, IsAdmin: true})
	caller := func(name string) (*http.Client, func(method, args string) map[string]any) {
		c := w.client(t)
		body(t, w.signIn(t, c, name, "pw"))
		return c, func(method, args string) map[string]any {
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
	annC, annCall := caller("ann")
	_, bossCall := caller("boss")
	active := func(id string) bool {
		var b Board
		_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": rb.ID}).Decode(&b)
		return b.MemberOf(id)
	}
	invite := func(call func(string, string) map[string]any, who string) map[string]any {
		return call("inviteUserToBoard", `["`+who+`","`+rb.ID+`"]`)
	}
	// Off: anybody.
	if out := invite(annCall, "other"); out["error"] != nil || !active(other.ID) {
		t.Fatalf("unrestricted: %v", out)
	}
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": rb.ID}, bson.M{"$pull": bson.M{"members": bson.M{"userId": other.ID}}})
	_, _ = w.db.Collection("settings").UpdateOne(ctx, bson.M{}, bson.M{"$set": bson.M{"boardMembersFromSameOrgOnly": true}})
	if out := invite(annCall, "other"); out["error"] != "error-user-notSameOrgOrTeam" || active(other.ID) {
		t.Errorf("negative: another Organization by the method: %v", out)
	}
	res, _ := post(t, annC, w.srv.URL+"/b/"+rb.ID+"/members", url.Values{"userId": {other.ID}})
	if res.StatusCode != http.StatusForbidden || active(other.ID) {
		t.Errorf("negative: another Organization by the Members widget: %d", res.StatusCode)
	}
	if out := invite(annCall, "new.person@example.invalid"); out["error"] != "error-user-notSameOrgOrTeam" {
		t.Errorf("negative: a new account has no Organization: %v", out)
	}
	if n, _ := w.db.Collection("users").CountDocuments(ctx, bson.M{"emails.address": "new.person@example.invalid"}); n != 0 {
		t.Error("negative: the refused new account was made")
	}
	// The search offers the same Organization only.
	names := func(out map[string]any) string {
		list, _ := out["result"].([]any)
		var n []string
		for _, e := range list {
			n = append(n, e.(map[string]any)["username"].(string))
		}
		return strings.Join(n, ",")
	}
	if got := names(annCall("searchUsers", `["example","`+rb.ID+`"]`)); strings.Contains(got, "other") || !strings.Contains(got, "same") {
		t.Errorf("the restricted search: %s", got)
	}
	if out := invite(annCall, "same"); out["error"] != nil || !active(same.ID) {
		t.Errorf("the same Organization: %v", out)
	}
	// A site admin adds anybody.
	if out := invite(bossCall, "other"); out["error"] != nil || !active(other.ID) {
		t.Errorf("a site admin: %v", out)
	}
	// An inviter without an Organization adds from the board's own: Bob has
	// none, but Other, an active member, is in o2.
	bob := w.seedUser(t, "bob", "pw")
	setMember(t, w, rb.ID, Member{UserID: bob.ID, IsActive: true, IsAdmin: true})
	o2 := org("o2mate", "o2")
	_, bobCall := caller("bob")
	if out := invite(bobCall, "o2mate"); out["error"] != nil || !active(o2.ID) {
		t.Errorf("through another member's Organization: %v", out)
	}

	// The search, unrestricted: no disabled account, a colleague behind five.
	_, _ = w.db.Collection("settings").UpdateOne(ctx, bson.M{}, bson.M{"$unset": bson.M{"boardMembersFromSameOrgOnly": 1}})
	for i := 0; i < 6; i++ {
		d := w.seedUser(t, "zzoff"+string(rune('a'+i)), "pw")
		_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": d.ID}, bson.M{"$set": bson.M{"loginDisabled": true}})
	}
	w.seedUser(t, "zzcolleague", "pw")
	got := names(annCall("searchUsers", `["ZZ","`+rb.ID+`"]`))
	if got != "zzcolleague" {
		t.Errorf("the search: %q", got)
	}
	if got := names(annCall("searchUsers", `["z","`+rb.ID+`"]`)); got != "" {
		t.Errorf("negative: one character finds nothing: %q", got)
	}
	if got := names(annCall("searchUsers", `[".*","`+rb.ID+`"]`)); got != "" {
		t.Errorf("negative: a pattern is text: %q", got)
	}
	_, eve := caller("zzcolleague")
	if out := eve("searchUsers", `["zz","`+rb.ID+`"]`); out["error"] != "not-authorized" {
		t.Errorf("negative: somebody not on the board: %v", out)
	}
}

// Negative, over the whole package: whatever adds a member asks the
// restriction first.
func TestEveryMemberAddAsksTheRestriction(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	callers := 0
	for _, p := range pkgs {
		for name, f := range p.Files {
			for _, d := range f.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok || fn.Body == nil || fn.Name.Name == "addMemberToBoard" {
					continue
				}
				asked, added := token.NoPos, token.NoPos
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					if c, ok := n.(*ast.CallExpr); ok {
						if sel, ok := c.Fun.(*ast.SelectorExpr); ok {
							switch sel.Sel.Name {
							case "mayAddToBoard":
								if asked == token.NoPos {
									asked = c.Pos()
								}
							case "addMemberToBoard":
								if added == token.NoPos {
									added = c.Pos()
								}
							}
						}
					}
					return true
				})
				if added != token.NoPos {
					callers++
					if asked == token.NoPos || asked > added {
						t.Errorf("%s: %s adds a member without asking mayAddToBoard first", name, fn.Name.Name)
					}
				}
			}
		}
	}
	if callers < 2 {
		t.Fatalf("only %d callers of addMemberToBoard found: the scan misses some", callers)
	}
}
