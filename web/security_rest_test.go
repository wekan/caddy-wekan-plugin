package web

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// restBoard is a board of a's, with one list and one card, over REST.
type restBoard struct {
	ID, Swimlane, List, Card string
}

func newRESTBoard(t *testing.T, w *testWeKan, token, title string) restBoard {
	t.Helper()
	var b struct {
		ID  string `json:"_id"`
		Swl string `json:"defaultSwimlaneId"`
	}
	call(t, "POST", w.srv.URL+"/api/boards", token, `{"title":"`+title+`"}`, &b)
	var l, c struct {
		ID string `json:"_id"`
	}
	call(t, "POST", w.srv.URL+"/api/boards/"+b.ID+"/lists", token, `{"title":"L"}`, &l)
	call(t, "POST", w.srv.URL+"/api/boards/"+b.ID+"/lists/"+l.ID+"/cards", token, `{"title":"C","swimlaneId":"`+b.Swl+`"}`, &c)
	if c.ID == "" {
		t.Fatal("seeding the board")
	}
	return restBoard{b.ID, b.Swl, l.ID, c.ID}
}

func login(t *testing.T, w *testWeKan, name string) string {
	t.Helper()
	var a struct{ Token string }
	call(t, "POST", w.srv.URL+"/users/login", "", `{"username":"`+name+`","password":"pw"}`, &a)
	return a.Token
}

func setMember(t *testing.T, w *testWeKan, boardID string, m Member) {
	t.Helper()
	ctx := context.Background()
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": boardID}, bson.M{"$pull": bson.M{"members": bson.M{"userId": m.UserID}}})
	if _, err := w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": boardID}, bson.M{"$push": bson.M{"members": m}}); err != nil {
		t.Fatal(err)
	}
}

// ParentBleed, bulk sibling (WeKan's 17-rest-api "bulk create refuses a parent
// card on a board the caller cannot see"): the entry is refused alone, the
// attempt is ONE summary row of Admin Panel -> Problems, and - a high-severity
// attempt by a logged-in account - that account is disabled, as in WeKan.
func TestParentBleed(t *testing.T) {
	w := newTestWeKan(t)
	ann := w.seedUser(t, "ann", "pw")
	w.seedUser(t, "ben", "pw")
	a, bt := login(t, w, "ann"), login(t, w, "ben")
	mine, hidden := newRESTBoard(t, w, a, "Mine"), newRESTBoard(t, w, bt, "Hidden")
	bulk := w.srv.URL + "/api/boards/" + mine.ID + "/lists/" + mine.List + "/cards/bulk"

	var res []map[string]any
	code := call(t, "POST", bulk, a, `{"swimlaneId":"`+mine.Swimlane+`","cards":[{"title":"child","parentId":"`+hidden.Card+
		`"},{"title":"visible child","parentId":"`+mine.Card+`"}]}`, &res)
	if code != 200 || len(res) != 2 || res[0]["error"] != "Forbidden" || res[0]["index"] != float64(0) || res[1]["_id"] == nil {
		t.Fatalf("the hidden parent's entry alone is refused: %d %v", code, res)
	}
	ctx := context.Background()
	if n, _ := w.db.Collection("cards").CountDocuments(ctx, bson.M{"parentId": hidden.Card}); n != 0 {
		t.Fatal("negative: no card got the hidden parent")
	}
	var raw bson.M
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": res[1]["_id"]}).Decode(&raw)
	if raw["parentId"] != mine.Card {
		t.Fatalf("a visible parent is kept: %v", raw["parentId"])
	}
	var row bson.M
	if err := w.db.Collection("eventlog").FindOne(ctx, bson.M{"bleed": "ParentBleed", "source": "parentId"}).Decode(&row); err != nil {
		t.Fatalf("the attempt is in Admin Panel -> Problems: %v", err)
	}
	if row["stream"] != "security" || row["count"] != int32(1) || row["severity"] != "high" || row["action"] != "blocked" ||
		row["username"] != "ann" || row["firstAt"] == nil || row["cwe"] != "CWE-862" {
		t.Fatalf("WeKan's summary row: %v", row)
	}
	var u bson.M
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"_id": ann.ID}).Decode(&u)
	if u["loginDisabled"] != true {
		t.Fatal("a high-severity attempt disables the account, as WeKan's blockOnSecurityEvent")
	}
	if code := call(t, "GET", w.srv.URL+"/api/user", a, "", nil); code != 401 {
		t.Fatalf("negative: the disabled account's token no longer works: %d", code)
	}

	// The single create and the edit refuse it too: 403, and an unknown parent 404.
	w.seedUser(t, "cid", "pw")
	ct := login(t, w, "cid")
	theirs := newRESTBoard(t, w, ct, "Theirs")
	cards := w.srv.URL + "/api/boards/" + theirs.ID + "/lists/" + theirs.List + "/cards"
	if code := call(t, "POST", cards, ct, `{"title":"x","swimlaneId":"`+theirs.Swimlane+`","parentId":"nosuchcard12345"}`, nil); code != 404 {
		t.Fatalf("an unknown parent: %d", code)
	}
	if code := call(t, "PUT", cards+"/"+theirs.Card, ct, `{"parentId":"`+theirs.Card+`"}`, nil); code != 200 {
		t.Fatalf("a parent on the caller's own board: %d", code)
	}
	if code := call(t, "PUT", cards+"/"+theirs.Card, ct, `{"parentId":"`+hidden.Card+`"}`, nil); code != 403 {
		t.Fatalf("negative: an edit naming a hidden parent: %d", code)
	}
	_ = w.db.Collection("eventlog").FindOne(ctx, bson.M{"bleed": "ParentBleed"}).Decode(&row)
	if row["count"] != int32(2) {
		t.Fatalf("ONE row per problem, counting: %v", row["count"])
	}
	if n, _ := w.db.Collection("eventlog").CountDocuments(ctx, bson.M{"bleed": "ParentBleed"}); n != 1 {
		t.Fatalf("negative: never a row per event: %d", n)
	}
	var tally struct {
		Actors map[string]struct {
			Kind, Value string
			Count       int
		} `bson:"actors"`
	}
	_ = w.db.Collection("eventlog").FindOne(ctx, bson.M{"bleed": "ParentBleed"}).Decode(&tally)
	got := map[string]int{}
	for _, a := range tally.Actors {
		got[a.Kind+":"+a.Value] = a.Count
	}
	if len(got) != 3 || got["user:ann"] != 1 || got["user:cid"] != 1 || got["ip:127.0.0.1"] != 2 {
		t.Fatalf("each user and the address are tallied separately: %v", got)
	}
}

// HashBleed sibling: an action other than the route's own verb answers 400 and
// never the user document.
func TestHashBleedMemberAction(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	a := login(t, w, "ann")
	b := newRESTBoard(t, w, a, "Members")
	for _, verb := range []string{"add", "remove"} {
		for _, body := range []string{`{"action":"show"}`, `{}`} {
			req, _ := http.NewRequest("POST", w.srv.URL+"/api/boards/"+b.ID+"/members/"+bob.ID+"/"+verb, strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+a)
			req.Header.Set("Content-Type", "application/json")
			res, err := testClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			text, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode != 400 || strings.Contains(string(text), "services") || strings.Contains(string(text), "bcrypt") || strings.Contains(string(text), bob.ID) {
				t.Fatalf("negative: %s %s: %d %s", verb, body, res.StatusCode, text)
			}
		}
	}
	if code := call(t, "POST", w.srv.URL+"/api/boards/"+b.ID+"/members/"+bob.ID+"/add", a, `{"action":"add"}`, nil); code != 200 {
		t.Fatalf("the route's own action works: %d", code)
	}
}

// ErrorBleed and AssignedBleed: a refused read is a real 403, and only a role
// that sees every card reads the whole board; writes follow WeKan's role table.
func TestRESTRoleAccess(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	a, bt := login(t, w, "ann"), login(t, w, "bob")
	b := newRESTBoard(t, w, a, "Roles")
	board := w.srv.URL + "/api/boards/" + b.ID
	reads := []string{board + "/lists", board + "/swimlanes", board + "/cards_count", board + "/lists/" + b.List + "/cards",
		board + "/lists/" + b.List + "/cards_count"}
	for _, u := range reads {
		if code := call(t, "GET", u, bt, "", nil); code != 403 {
			t.Fatalf("ErrorBleed: a non-member's read of %s is 403: %d", u, code)
		}
	}
	var count map[string]int
	if code := call(t, "GET", board+"/cards_count", a, "", &count); code != 200 || count["board_cards_count"] != 1 {
		t.Fatalf("cards_count, WeKan's shape: %d %v", code, count)
	}
	call(t, "GET", board+"/lists/"+b.List+"/cards_count", a, "", &count)
	if count["list_cards_count"] != 1 {
		t.Fatalf("a list's cards_count: %v", count)
	}

	write := func() int {
		return call(t, "POST", board+"/lists/"+b.List+"/cards", bt, `{"title":"w","swimlaneId":"`+b.Swimlane+`"}`, nil)
	}
	comment := func(card string) int {
		return call(t, "POST", board+"/cards/"+card+"/comments", bt, `{"comment":"hi"}`, nil)
	}
	for _, c := range []struct {
		m                       Member
		read, write, commentAny int
	}{
		{Member{IsReadAssignedOnly: true}, 403, 403, 403},
		{Member{IsNormalAssignedOnly: true}, 403, 403, 404}, // comments only on their own cards
		{Member{IsCommentAssignedOnly: true}, 403, 403, 404},
		{Member{IsReadOnly: true}, 200, 403, 403},
		{Member{IsCommentOnly: true}, 403, 403, 200}, // WeKan's memberReadsWholeBoard refuses comment-only too
		{Member{IsWorker: true}, 403, 403, 200},
		{Member{}, 200, 200, 200},
	} {
		c.m.UserID, c.m.IsActive = bob.ID, true
		setMember(t, w, b.ID, c.m)
		role := c.m.Role()
		for _, u := range reads {
			if code := call(t, "GET", u, bt, "", nil); code != c.read {
				t.Fatalf("%s reads %s: %d, want %d", role, u, code, c.read)
			}
		}
		if code := write(); code != c.write {
			t.Fatalf("%s writes: %d, want %d", role, code, c.write)
		}
		if code := comment(b.Card); code != c.commentAny {
			t.Fatalf("%s comments on a card not assigned to them: %d, want %d", role, code, c.commentAny)
		}
	}
	// An assigned-only member comments on a card assigned to them, and the refusal
	// above was recorded as AssignedBleed (medium: the account stays enabled).
	setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: true, IsNormalAssignedOnly: true})
	_, _ = w.db.Collection("cards").UpdateOne(context.Background(), bson.M{"_id": b.Card}, bson.M{"$set": bson.M{"assignees": []string{bob.ID}}})
	if code := comment(b.Card); code != 200 {
		t.Fatalf("an assigned-only member comments on their own card: %d", code)
	}
	var row bson.M
	if err := w.db.Collection("eventlog").FindOne(context.Background(), bson.M{"bleed": "AssignedBleed", "source": "rest:comment-insert"}).Decode(&row); err != nil || row["count"] != int32(2) {
		t.Fatalf("AssignedBleed is recorded: %v %v", err, row)
	}
	if code := call(t, "GET", w.srv.URL+"/api/user", bt, "", nil); code != 200 {
		t.Fatalf("negative: a medium-severity attempt does not disable the account: %d", code)
	}
}

// A write to eventlog: reading it (Admin Panel -> Problems) is anybody's.
var eventlogWrite = regexp.MustCompile(`Collection\("eventlog"\)\.(Insert|Update|Replace|Delete|FindOneAnd|BulkWrite)`)

// The negative tests over the whole package: no REST handler decides board
// access with membership alone. A write asks Board.Writable or apiBoardCan,
// which read WeKan's role table; MemberOf - "is on the board" - is never what
// lets a request change it.
func TestNoWriteDecidedByMembershipAlone(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	memberOf := regexp.MustCompile(`!\s*\w+\.MemberOf\(u\.ID\)`)
	var offenders []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, _ := os.ReadFile(f)
		for i, line := range strings.Split(string(src), "\n") {
			// boardFor's own visibility rule (member or public) is the one exception.
			if memberOf.MatchString(line) && !strings.Contains(line, `b.Permission != "public"`) {
				offenders = append(offenders, f+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("access decided by membership alone, not by the role:\n%s", strings.Join(offenders, "\n"))
	}
	if !memberOf.MatchString(`if err != nil || !to.MemberOf(u.ID) {`) {
		t.Fatal("the scan misses the shape it guards")
	}
	// Every eventlog write goes through recordSecurity's fold: one row per problem.
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "securitylog.go" {
			continue
		}
		src, _ := os.ReadFile(f)
		if !eventlogWrite.MatchString(`s.db.Collection("eventlog").UpdateOne(ctx`) || eventlogWrite.MatchString(`s.db.Collection("eventlog").Find(`) {
			t.Fatal("the eventlog scan misses a write, or takes a read")
		}
		if eventlogWrite.Match(src) {
			t.Fatalf("%s writes eventlog itself, not through the fold", f)
		}
	}
}
