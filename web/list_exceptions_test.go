package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Negative, for the whole tree: a list's grant opens the pages and the
// methods, never the REST API (WeKan's assertListNotRefused: "A list's GRANT
// does not open the API to a role the board does not let write"). No REST
// handler file calls what grants.
func TestListGrantsNeverReachREST(t *testing.T) {
	files, _ := filepath.Glob("api*.go")
	grants := regexp.MustCompile(`\bcanAddCardIn\(|\bcanCommentIn\(|\.AddComment\(`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, _ := os.ReadFile(f)
		if loc := grants.FindIndex(data); loc != nil {
			t.Errorf("%s calls %s: a list's grant must not reach REST", f, data[loc[0]:loc[1]])
		}
	}
	// And the grant itself is only these two helpers' (list_permissions.go).
	all, _ := filepath.Glob("*.go")
	for _, f := range all {
		if strings.HasSuffix(f, "_test.go") || f == "list_permissions.go" {
			continue
		}
		data, _ := os.ReadFile(f)
		if regexp.MustCompile(`listException\([^\n]*\); e != nil && \*e`).Match(data) {
			t.Errorf("%s reads a list's grant itself: go through canAddCardIn or canCommentIn", f)
		}
	}
}

// #4527: a list may grant a role more than the board gives - a read-only
// member adds cards and comments in that list alone, from the page and the
// methods; everybody signed in on a public board where the list says so -
// but never over REST, and never in the board's other lists.
func TestListGrants(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")   // read-only on the board
	carl := w.seedUser(t, "carl", "pw") // not a member
	at, bt := login(t, w, "ann"), login(t, w, "bob")
	rb := newRESTBoard(t, w, at, "Grants")
	setMember(t, w, rb.ID, Member{UserID: bob.ID, IsActive: true, IsReadOnly: true})
	var open struct {
		ID string `json:"_id"`
	}
	call(t, "POST", w.srv.URL+"/api/boards/"+rb.ID+"/lists", at, `{"title":"Suggestions"}`, &open)
	var oc struct {
		ID string `json:"_id"`
	}
	call(t, "POST", w.srv.URL+"/api/boards/"+rb.ID+"/lists/"+open.ID+"/cards", at, `{"title":"Idea","swimlaneId":"`+rb.Swimlane+`"}`, &oc)
	_, _ = w.db.Collection("lists").UpdateOne(ctx, bson.M{"_id": open.ID}, bson.M{"$set": bson.M{"roleExceptions": bson.M{
		"read-only": bson.M{"write": true, "comment": true}, "signed-in": bson.M{"write": true}}}})
	var b Board
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": rb.ID}).Decode(&b)
	bc := w.client(t)
	body(t, w.signIn(t, bc, "bob", "pw"))
	addCard := func(c *http.Client, listID, title string) int {
		res, err := c.PostForm(w.srv.URL+"/b/"+rb.ID+"/cards", url.Values{"listId": {listID}, "swimlaneId": {rb.Swimlane}, "title": {title}})
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	count := func(filter bson.M) int64 { n, _ := w.db.Collection("cards").CountDocuments(ctx, filter); return n }

	// The page: the granted list offers its composer, the others do not.
	page := getBody(t, bc, w.srv.URL+"/b/"+rb.ID+"/"+b.Slug)
	if !strings.Contains(page, `name="listId" value="`+open.ID+`"`) {
		t.Error("the granted list offers a read-only member its card composer")
	}
	if strings.Contains(page, `name="listId" value="`+rb.List+`"`) {
		t.Error("negative: the board's other lists do not")
	}
	if code := addCard(bc, open.ID, "From bob"); code != http.StatusSeeOther || count(bson.M{"title": "From bob", "listId": open.ID}) != 1 {
		t.Errorf("a read-only member adds a card in the granted list: %d", code)
	}
	if code := addCard(bc, rb.List, "Not here"); code != http.StatusForbidden || count(bson.M{"title": "Not here"}) != 0 {
		t.Errorf("negative: nor in another list: %d", code)
	}
	// Comments, on the page and through the method.
	res, _ := bc.PostForm(w.srv.URL+"/b/"+rb.ID+"/cards/"+oc.ID+"/comments", url.Values{"comment": {"Granted comment"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Errorf("a read-only member comments in the granted list: %d", res.StatusCode)
	}
	if !strings.Contains(getBody(t, bc, w.srv.URL+"/b/"+rb.ID+"/"+b.Slug+"/"+oc.ID), "js-new-comment-form") {
		t.Error("the card in the granted list offers the comment form")
	}
	if strings.Contains(getBody(t, bc, w.srv.URL+"/b/"+rb.ID+"/"+b.Slug+"/"+rb.Card), "js-new-comment-form") {
		t.Error("negative: a card of another list does not")
	}
	res, _ = bc.PostForm(w.srv.URL+"/b/"+rb.ID+"/cards/"+rb.Card+"/comments", url.Values{"comment": {"Elsewhere"}})
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("negative: nor a comment there: %d", res.StatusCode)
	}
	req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/"+url.PathEscape("/card_comments/insert"),
		strings.NewReader(`[{"boardId":"`+rb.ID+`","cardId":"`+oc.ID+`","text":"By method"}]`))
	req.Header.Set("Content-Type", "application/json")
	if res, err := bc.Do(req); err != nil || res.StatusCode != http.StatusOK {
		t.Errorf("the comment method grants it too: %v", res)
	}
	if n, _ := w.db.Collection("card_comments").CountDocuments(ctx, bson.M{"cardId": oc.ID}); n != 2 {
		t.Errorf("both comments are on the card: %d", n)
	}
	// Negative: REST never grants.
	if code := call(t, "POST", w.srv.URL+"/api/boards/"+rb.ID+"/lists/"+open.ID+"/cards", bt, `{"title":"Over REST","swimlaneId":"`+rb.Swimlane+`"}`, nil); code != http.StatusForbidden {
		t.Errorf("negative: a grant does not open REST's cards: %d", code)
	}
	if code := call(t, "POST", w.srv.URL+"/api/boards/"+rb.ID+"/cards/"+oc.ID+"/comments", bt, `{"comment":"Over REST"}`, nil); code == http.StatusOK {
		t.Errorf("negative: nor REST's comments: %d", code)
	}
	if count(bson.M{"title": "Over REST"}) != 0 {
		t.Error("negative: no card over REST")
	}
	// Everybody signed in, on a public board, where the list says so.
	cc := w.client(t)
	body(t, w.signIn(t, cc, "carl", "pw"))
	if code := addCard(cc, open.ID, "From carl"); code == http.StatusSeeOther || count(bson.M{"title": "From carl"}) != 0 {
		t.Errorf("negative: a private board grants nobody outside it: %d", code)
	}
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": rb.ID}, bson.M{"$set": bson.M{"permission": "public"}})
	if code := addCard(cc, open.ID, "From carl"); code != http.StatusSeeOther || count(bson.M{"title": "From carl", "userId": carl.ID}) != 1 {
		t.Errorf("a public board's list grants everybody signed in: %d", code)
	}
	res, _ = cc.PostForm(w.srv.URL+"/b/"+rb.ID+"/cards/"+oc.ID+"/comments", url.Values{"comment": {"Carl comments"}})
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("negative: the list granted signed-in users cards, not comments: %d", res.StatusCode)
	}
}

// lists.setRoleExceptions, and the List permissions page: the board's admins
// set a list's exceptions, cleaned as WeKan's; nobody else can, and a direct
// write of the field is refused and seen.
func TestSetListExceptions(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Exceptions")
	setMember(t, w, rb.ID, Member{UserID: bob.ID, IsActive: true})
	var b Board
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": rb.ID}).Decode(&b)
	caller := func(user string) (*http.Client, func(method, args string) map[string]any) {
		c := w.client(t)
		body(t, w.signIn(t, c, user, "pw"))
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
	ac, annCall := caller("ann")
	bc, bobCall := caller("bob")
	stored := func() any {
		var l bson.M
		_ = w.db.Collection("lists").FindOne(ctx, bson.M{"_id": rb.List}).Decode(&l)
		return plain(l["roleExceptions"])
	}
	out := annCall("lists.setRoleExceptions", `["`+rb.List+`",{"read-only":{"write":true,"delete":true},"board-admin":{"write":false},"Bad Role!":{"write":true},"worker":{"comment":"yes"}}]`)
	if m, _ := stored().(map[string]any); out["error"] != nil || len(m) != 1 || m["read-only"].(map[string]any)["write"] != true || m["read-only"].(map[string]any)["delete"] != nil {
		t.Errorf("the board's admin sets them, cleaned: %v %v", out, m)
	}
	if out := bobCall("lists.setRoleExceptions", `["`+rb.List+`",{"normal":{"write":true}}]`); out["error"] != "error-notAuthorized" {
		t.Errorf("negative: a normal member sets none: %v", out)
	}
	if m, _ := stored().(map[string]any); m["normal"] != nil {
		t.Error("negative: and nothing changed")
	}
	annCall("lists.setRoleExceptions", `["`+rb.List+`",{}]`)
	if stored() != nil {
		t.Error("an empty set clears them")
	}
	// Negative: a direct write of the field is refused and seen.
	if out := bobCall("/lists/update", `[{"_id":"`+rb.List+`"},{"$set":{"roleExceptions":{"normal":{"write":true}}}}]`); out["error"] == nil || stored() != nil {
		t.Errorf("negative: a direct write is refused: %v", out)
	}
	if n, _ := w.db.Collection("eventlog").CountDocuments(ctx, bson.M{"source": "canary:list.role-exceptions-direct"}); n == 0 {
		t.Error("and it is in Problems")
	}
	// The page.
	pagePath := w.srv.URL + "/b/" + rb.ID + "/" + b.Slug + "/lists/" + rb.List + "/permissions"
	page := getBody(t, ac, pagePath)
	if !strings.Contains(page, `name="read-only.write"`) || !strings.Contains(page, `name="normal.comment"`) || strings.Contains(page, `name="board-admin.`) {
		t.Error("the page has a choice per role but the board's admins")
	}
	if strings.Contains(page, `name="signed-in.write"`) {
		t.Error("negative: signed-in users only on an open board")
	}
	if !strings.Contains(getBody(t, ac, w.srv.URL+"/b/"+rb.ID+"/"+b.Slug), "js-list-permissions") {
		t.Error("the list menu links it for the board's admin")
	}
	res, _ := ac.PostForm(w.srv.URL+"/b/"+rb.ID+"/lists/"+rb.List+"/permissions", url.Values{"normal.comment": {"deny"}, "worker.write": {"allow"}, "read-only.write": {""}, "x.y": {"maybe"}})
	if m, _ := stored().(map[string]any); res.StatusCode != http.StatusSeeOther || len(m) != 2 || m["normal"].(map[string]any)["comment"] != false || m["worker"].(map[string]any)["write"] != true {
		t.Errorf("the page's form saves: %d %v", res.StatusCode, m)
	}
	if !strings.Contains(getBody(t, ac, pagePath), `<option value="deny" selected`) {
		t.Error("the page shows what is set")
	}
	if res, _ := get(t, bc, pagePath); res.StatusCode != http.StatusNotFound {
		t.Errorf("negative: a normal member does not get the page: %d", res.StatusCode)
	}
	if strings.Contains(getBody(t, bc, w.srv.URL+"/b/"+rb.ID+"/"+b.Slug), "js-list-permissions") {
		t.Error("negative: nor the menu's link")
	}
	res, _ = bc.PostForm(w.srv.URL+"/b/"+rb.ID+"/lists/"+rb.List+"/permissions", url.Values{"normal.write": {"allow"}})
	if m, _ := stored().(map[string]any); res.StatusCode != http.StatusForbidden || m["normal"].(map[string]any)["write"] != nil {
		t.Errorf("negative: nor save it: %d", res.StatusCode)
	}
}
