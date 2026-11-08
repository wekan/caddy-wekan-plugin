package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// CreateBleed: while Admin Panel lets only admins make boards, nobody else
// makes one on any path - the page, REST, a copy or the method - but a
// template board; an admin still does. Private only: every new board is
// private.
func TestBoardCreationPolicy(t *testing.T) {
	w := newTestWeKan(t)
	admin := w.seedUser(t, "ann", "pw")
	w.seedUser(t, "bob", "pw")
	ctx := context.Background()
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": admin.ID}, bson.M{"$set": bson.M{"isAdmin": true}})
	own, _ := w.s.CreateBoard(ctx, admin, "Admin's")
	_, _ = w.db.Collection("tableVisibilityModeSettings").InsertOne(ctx, bson.M{"_id": "tableVisibilityMode-boardCreationAdminOnly", "booleanValue": true})
	bob := w.client(t)
	body(t, w.signIn(t, bob, "bob", "pw"))
	bt := login(t, w, "bob")
	if r, _ := bob.PostForm(w.srv.URL+"/boards", url.Values{"title": {"Mine"}}); r.StatusCode != http.StatusForbidden {
		t.Fatalf("negative: the page: %d", r.StatusCode)
	}
	if code := call(t, "POST", w.srv.URL+"/api/boards", bt, `{"title":"Mine"}`, nil); code != 403 {
		t.Fatalf("negative: REST: %d", code)
	}
	method := func(cl *http.Client, payload string) map[string]any {
		req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/createBoardWithInitialSwimlanes", strings.NewReader(`[`+payload+`]`))
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
	if out := method(bob, `{"title":"Mine","slug":"mine","swimlanes":[{"title":"Default"}]}`); out["error"] != "not-authorized" {
		t.Fatalf("negative: the method: %v", out)
	}
	if out := method(bob, `{"title":"A template","slug":"t","type":"template-board"}`); out["result"] == nil {
		t.Fatalf("a template board is still allowed: %v", out)
	}
	if n, _ := w.db.Collection("boards").CountDocuments(ctx, bson.M{"title": "Mine"}); n != 0 {
		t.Fatal("negative: no board made")
	}
	// One summary row per kind of event - here per source - its count each.
	cur, _ := w.db.Collection("eventlog").Find(ctx, bson.M{"bleed": "CreateBleed"})
	var rows []struct {
		Count int64 `bson:"count"`
	}
	_ = cur.All(ctx, &rows)
	total := int64(0)
	for _, r := range rows {
		total += r.Count
	}
	if total != 3 {
		t.Fatalf("each refusal recorded: %d", total)
	}
	var u User
	_ = w.db.Collection("users").FindOne(ctx, bson.M{"username": "bob"}).Decode(&u)
	if u.LoginDisabled {
		t.Fatal("negative: CreateBleed never blocks the account")
	}
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	_, _ = w.db.Collection("tableVisibilityModeSettings").InsertOne(ctx, bson.M{"_id": "tableVisibilityMode-allowPrivateOnly", "booleanValue": true})
	out := method(ann, `{"title":"Open","slug":"open","permission":"public","swimlanes":[{"title":"One"},{"title":"Two"}]}`)
	id, _ := out["result"].(string)
	var b Board
	if w.db.Collection("boards").FindOne(ctx, bson.M{"_id": id}).Decode(&b) != nil || b.Permission != "private" {
		t.Fatalf("an admin makes it - private, under the private-only policy: %v %+v", out, b)
	}
	if n, _ := w.db.Collection("swimlanes").CountDocuments(ctx, bson.M{"boardId": id}); n != 2 {
		t.Fatalf("with its swimlanes: %d", n)
	}
	at := login(t, w, "ann")
	if code := call(t, "POST", w.srv.URL+"/api/boards/"+own.ID+"/copy", at, `{}`, nil); code != 200 {
		t.Fatalf("an admin copies a board: %d", code)
	}
}

// CreateBleed, everywhere: a board is inserted only by insertBoard, after the
// check, or by an import after the same check - so no path can skip it.
func TestEveryBoardInsertIsChecked(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, _ := os.ReadFile(f)
		for i, line := range strings.Split(string(src), "\n") {
			if !strings.Contains(line, `Collection("boards").InsertOne`) {
				continue
			}
			text := string(src)
			before := strings.Join(strings.Split(text, "\n")[max(0, i-12):i], "\n")
			if !strings.Contains(before, "mayCreateBoard") && !strings.Contains(before, "func (s *Server) insertBoard") {
				t.Errorf("%s:%d inserts a board without WeKan's board creation check", f, i+1)
			}
		}
	}
}
