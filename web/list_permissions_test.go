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

// Negative, for the whole tree (ListLockBleed's lesson): every write route
// that names a card - REST, the pages, the methods' card writes - asks the
// card's list, through WritableIn, CommentableIn, refuseInList or a shared
// path that does (AddComment, MoveCards). The exceptions follow a rule of
// their own, each named here with why.
func TestEveryCardWriteAsksTheList(t *testing.T) {
	exempt := map[string]string{
		"apiAddDependency":    "dependency lines follow the board-wide dependency rule, as WeKan's",
		"apiEditDependency":   "dependency lines follow the board-wide dependency rule, as WeKan's",
		"apiDeleteDependency": "dependency lines follow the board-wide dependency rule, as WeKan's",
		"apiBotRunReport":     "a bot run's report changes the run, not the card, as WeKan's",
		"votePage":            "a vote is the member's own say, asked of the board as WeKan's vote methods",
		"pokerPage":           "a poker estimate is the member's own say, asked of the board as WeKan's",
		"watchCardPage":       "watching changes only the member's own subscription",
		"apiEditComment":      "a comment's edit follows the comment's own rule, as WeKan's",
		"apiDeleteComment":    "a comment's deletion follows the comment's own rule, as WeKan's",
		// The board's look, by a board admin: it changes nothing of the card
		// its image is attached to (WeKan's setBackgroundImageURL).
		"attachmentBackgroundPage": "the board's background is a board admin's choice, not a change to the card",
	}
	files, _ := filepath.Glob("*.go")
	src := map[string]string{}
	var all strings.Builder
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, _ := os.ReadFile(f)
		src[f] = string(data)
		all.Write(data)
	}
	// A route's pattern, with the file's own `name := "..."` prefixes put in:
	// "POST "+card+"/archive" is a card write as much as a literal one.
	route := regexp.MustCompile(`HandleFunc\("(POST|PUT|DELETE|PATCH) ((?:[^"]|"\+\w+\+")*)"(?:\+(\w+)(?:\+"([^"]*)")?)?, s\.(?:api|page)\(s\.(\w+)(?:\([^)]*\))?\)\)`)
	prefixDef := regexp.MustCompile(`(\w+) := "(/[^"]*)"`)
	cardWrite := regexp.MustCompile(`\{card\}|/cards$|/cards/bulk$|/cards/labels$|^/attachments/|\{list\}/ics$`)
	asks := regexp.MustCompile(`WritableIn\(|CommentableIn\(|refuseInList\(|\.AddComment\(|\.MoveCards\(`)
	body := func(name string) string {
		code := all.String()
		i := strings.Index(code, ") "+name+"(")
		if i < 0 {
			return ""
		}
		j := strings.Index(code[i:], "\n}\n")
		if j < 0 {
			return code[i:]
		}
		return code[i : i+j]
	}
	seen := 0
	for f, code := range src {
		prefixes := map[string]string{}
		for _, d := range prefixDef.FindAllStringSubmatch(code, -1) {
			prefixes[d[1]] = d[2]
		}
		for _, m := range route.FindAllStringSubmatch(code, -1) {
			pattern := m[2]
			if m[3] != "" {
				pattern += prefixes[m[3]] + m[4]
			}
			if !cardWrite.MatchString(pattern) {
				continue
			}
			m[2] = pattern
			seen++
			handler := m[5]
			if _, ok := exempt[handler]; ok {
				continue
			}
			b := body(handler)
			// Or one call down: a handler whose own helper asks (boardCard).
			ok := asks.MatchString(b)
			for _, call := range regexp.MustCompile(`s\.(\w+)\(`).FindAllStringSubmatch(b, -1) {
				if !ok && call[1] != handler && asks.MatchString(body(call[1])) {
					ok = true
				}
			}
			// A route that creates cards in a list asks THAT list itself - a
			// check of some other card (a link's source) is not it.
			if strings.HasSuffix(pattern, "/cards") || strings.HasSuffix(pattern, "/cards/bulk") || strings.HasSuffix(pattern, "/ics") {
				ok = strings.Contains(b, "refuseInList(")
			}
			if !ok {
				t.Errorf("negative: %s %s (%s, %s) does not ask the card's list", m[1], m[2], handler, f)
			}
		}
	}
	if seen < 30 {
		t.Fatalf("the route scan found only %d card write routes: the pattern is stale", seen)
	}
}

// A list that refuses a role (#4527) - set in WeKan, on the shared database -
// refuses that role every write in it, over REST and the methods: edit,
// delete, checklists, members, comments, a new card, a move in or out.
// Nothing changes, the attempt is ListLockBleed in Problems, the same member
// still writes in an open list, and the board admin in the locked one. A
// list's grant does not open the API to a role the board does not let write.
func TestListLock(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	ctx := context.Background()
	at, bt := login(t, w, "ann"), login(t, w, "bob")
	rb := newRESTBoard(t, w, at, "Locks")
	setMember(t, w, rb.ID, Member{UserID: bob.ID, IsActive: true})
	var locked struct {
		ID string `json:"_id"`
	}
	call(t, "POST", w.srv.URL+"/api/boards/"+rb.ID+"/lists", at, `{"title":"Locked"}`, &locked)
	var lc struct {
		ID string `json:"_id"`
	}
	call(t, "POST", w.srv.URL+"/api/boards/"+rb.ID+"/lists/"+locked.ID+"/cards", at, `{"title":"Guarded","swimlaneId":"`+rb.Swimlane+`"}`, &lc)
	_, _ = w.db.Collection("lists").UpdateOne(ctx, bson.M{"_id": locked.ID}, bson.M{"$set": bson.M{"roleExceptions": bson.M{"normal": bson.M{"write": false, "comment": false}}}})
	api := w.srv.URL + "/api/boards/" + rb.ID
	inLocked := api + "/lists/" + locked.ID + "/cards/" + lc.ID
	for _, tc := range []struct{ method, url, body string }{
		{"PUT", inLocked, `{"title":"Hacked"}`},
		{"DELETE", inLocked, ""},
		{"POST", api + "/cards/" + lc.ID + "/checklists", `{"title":"Sneaky"}`},
		{"POST", inLocked + "/members/" + bob.ID, ""},
		{"POST", api + "/cards/" + lc.ID + "/comments", `{"comment":"sneaky"}`},
		{"POST", api + "/lists/" + locked.ID + "/cards", `{"title":"Planted","swimlaneId":"` + rb.Swimlane + `"}`},
		{"POST", api + "/lists/" + locked.ID + "/cards/bulk", `{"swimlaneId":"` + rb.Swimlane + `","cards":[{"title":"Planted"}]}`},
		{"PUT", api + "/lists/" + rb.List + "/cards/" + rb.Card, `{"listId":"` + locked.ID + `"}`},
		{"PUT", inLocked, `{"listId":"` + rb.List + `"}`},
	} {
		if code := call(t, tc.method, tc.url, bt, tc.body, nil); code != 403 {
			t.Errorf("negative: %s %s answers %d, not 403", tc.method, strings.TrimPrefix(tc.url, api), code)
		}
	}
	var c Card
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": lc.ID}).Decode(&c)
	moved := Card{}
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": rb.Card}).Decode(&moved)
	nChecklists, _ := w.db.Collection("checklists").CountDocuments(ctx, bson.M{"cardId": lc.ID})
	nComments, _ := w.db.Collection("card_comments").CountDocuments(ctx, bson.M{"cardId": lc.ID})
	nPlanted, _ := w.db.Collection("cards").CountDocuments(ctx, bson.M{"title": "Planted"})
	if c.Title != "Guarded" || c.ListID != locked.ID || len(c.Members) != 0 || nChecklists+nComments+nPlanted != 0 || moved.ListID != rb.List {
		t.Errorf("negative: nothing changed: %+v checklists %d comments %d planted %d moved to %s", c, nChecklists, nComments, nPlanted, moved.ListID)
	}
	var row bson.M
	if err := w.db.Collection("eventlog").FindOne(ctx, bson.M{"bleed": "ListLockBleed"}).Decode(&row); err != nil || row["action"] != "blocked" {
		t.Errorf("the attempts are in Problems: %v %v", err, row)
	}
	var u User
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"_id": bob.ID}).Decode(&u)
	if u.LoginDisabled {
		t.Error("negative: medium severity never disables the account")
	}
	// The methods too.
	cl := w.client(t)
	body(t, w.signIn(t, cl, "bob", "pw"))
	method := func(name, args string) map[string]any {
		req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/"+url.PathEscape(name), strings.NewReader(args))
		req.Header.Set("Content-Type", "application/json")
		res, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		return out
	}
	method("/cards/update", `[{"_id":"`+lc.ID+`"},{"$set":{"title":"Hacked"}}]`)
	method("/card_comments/insert", `[{"cardId":"`+lc.ID+`","boardId":"`+rb.ID+`","text":"sneaky"}]`)
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": lc.ID}).Decode(&c)
	nComments, _ = w.db.Collection("card_comments").CountDocuments(ctx, bson.M{"cardId": lc.ID})
	if c.Title != "Guarded" || nComments != 0 {
		t.Errorf("negative: the methods refuse too: %q, %d comments", c.Title, nComments)
	}
	// Still allowed: bob in an open list, ann in the locked one.
	if code := call(t, "PUT", api+"/lists/"+rb.List+"/cards/"+rb.Card, bt, `{"title":"Open edit"}`, nil); code != 200 {
		t.Errorf("an open list: %d", code)
	}
	if code := call(t, "PUT", inLocked, at, `{"title":"Admin edit"}`, nil); code != 200 {
		t.Errorf("the board admin in the locked list: %d", code)
	}
	// A checklist is renamed over REST (#1037) - not in a locked list.
	_, _ = w.db.Collection("checklists").InsertMany(ctx, []any{bson.M{"_id": "clLocked", "cardId": lc.ID, "title": "Locked", "sort": 0.0},
		bson.M{"_id": "clOpen", "cardId": rb.Card, "title": "Open", "sort": 0.0}})
	if code := call(t, "PUT", api+"/cards/"+lc.ID+"/checklists/clLocked", bt, `{"title":"Renamed"}`, nil); code != 403 {
		t.Errorf("negative: renaming a checklist in a locked list: %d", code)
	}
	if code := call(t, "PUT", api+"/cards/"+rb.Card+"/checklists/clOpen", bt, `{"title":"Renamed","dueAt":"2027-01-02T03:04:05.000Z"}`, nil); code != 200 {
		t.Errorf("renaming a checklist: %d", code)
	}
	if code := call(t, "PUT", api+"/cards/"+rb.Card+"/checklists/clOpen", bt, `{}`, nil); code != 400 {
		t.Errorf("negative: nothing to change: %d", code)
	}
	var chk bson.M
	_ = w.db.Collection("checklists").FindOne(ctx, bson.M{"_id": "clOpen"}).Decode(&chk)
	if chk["title"] != "Renamed" || chk["dueAt"] == nil {
		t.Errorf("the rename and its due date: %v", chk)
	}
	call(t, "PUT", api+"/cards/"+rb.Card+"/checklists/clOpen", bt, `{"dueAt":null}`, nil)
	chk = nil
	_ = w.db.Collection("checklists").FindOne(ctx, bson.M{"_id": "clOpen"}).Decode(&chk)
	if _, has := chk["dueAt"]; has {
		t.Error("null clears the due date")
	}
	_ = w.db.Collection("checklists").FindOne(ctx, bson.M{"_id": "clLocked"}).Decode(&chk)
	if chk["title"] != "Locked" {
		t.Error("negative: the locked checklist kept its title")
	}
	// A grant does not open the API to a read-only member.
	setMember(t, w, rb.ID, Member{UserID: bob.ID, IsActive: true, IsReadOnly: true})
	_, _ = w.db.Collection("lists").UpdateOne(ctx, bson.M{"_id": rb.List}, bson.M{"$set": bson.M{"roleExceptions": bson.M{"read-only": bson.M{"write": true}}}})
	if code := call(t, "PUT", api+"/lists/"+rb.List+"/cards/"+rb.Card, bt, `{"title":"Granted?"}`, nil); code == 200 {
		t.Error("negative: a list's grant does not open the API")
	}
}
