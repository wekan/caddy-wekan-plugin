package web

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestAnonMapText(t *testing.T) {
	a := buildAnonMap(map[string]string{"zzz": "bob", "aaa": "ann"}, "user")
	if a.byID["aaa"].Username != "user1" || a.byID["zzz"].Initials != "U2" {
		t.Fatalf("numbered in id order: %+v", a.byID)
	}
	for in, want := range map[string]string{"@bob hi @ann": "@user2 hi @user1", "mail bob@ann.fi": "mail bob@ann.fi",
		"(@bob)": "(@user2)", "@carol": "@carol", "@@bob": "@@bob"} {
		if got := a.text(in); got != want {
			t.Errorf("text(%q) = %q, want %q", in, got, want)
		}
	}
	var none *anonMap
	if none.text("@bob") != "@bob" {
		t.Error("off: the text as it is")
	}
	if fi := buildAnonMap(map[string]string{"a": "x"}, "käyttäjä"); fi.byID["a"].Initials != "K1" {
		t.Errorf("the word's own initial: %+v", fi.byID["a"])
	}
}

// With Admin Panel -> Anonymize export users on, no export says who anybody is.
func TestExportsAnonymizeUsers(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "annreal", "pw")
	bob := w.seedUser(t, "bobreal", "pw")
	a := login(t, w, "annreal")
	b := newRESTBoard(t, w, a, "Anon")
	ctx := context.Background()
	setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: true})
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": bob.ID}, bson.M{"$set": bson.M{"profile.fullname": "Bob Realname"}})
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": b.Card}, bson.M{"$set": bson.M{
		"title": "ask @bobreal", "description": "cc @annreal", "requestedBy": "bobreal", "members": bson.A{bob.ID},
		"dueAt": bson.DateTime(1800000000000)}})
	_, _ = w.db.Collection("card_comments").InsertOne(ctx, bson.M{"_id": "anoncomment00001", "boardId": b.ID, "cardId": b.Card, // inserted, not a filter
		"userId": bob.ID, "text": "thanks @annreal"})
	get := func(url string) string {
		req, _ := http.NewRequest("GET", w.srv.URL+url, nil)
		req.Header.Set("Authorization", "Bearer "+a)
		res, err := testClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, _ := io.ReadAll(res.Body)
		if res.StatusCode != 200 {
			t.Fatalf("%s: %d", url, res.StatusCode)
		}
		if z, err := zip.NewReader(bytes.NewReader(data), int64(len(data))); err == nil {
			var all strings.Builder
			for _, f := range z.File {
				r, _ := f.Open()
				x, _ := io.ReadAll(r)
				all.Write(x)
			}
			return all.String()
		}
		return string(data)
	}
	base := "/api/boards/" + b.ID
	routes := []string{base + "/export", base + "/exportZip", base + "/export/csv", base + "/calendar.ics", base + "/exportExcel",
		base + "/exportPDF", base + "/lists/" + b.List + "/cards/" + b.Card + "/exportPDF", "/api/export-all-boards/wekan", "/api/export-all-boards/csv"}
	if body := get(base + "/export"); !strings.Contains(body, "bobreal") || !strings.Contains(body, "Bob Realname") {
		t.Fatal("off: the export names its users")
	}
	_, _ = w.db.Collection("settings").UpdateOne(ctx, bson.M{}, bson.M{"$set": bson.M{"anonymizeExportUsers": true}})
	for _, r := range routes {
		body := get(r)
		for _, real := range []string{"bobreal", "annreal", "Bob Realname"} {
			if strings.Contains(body, real) {
				t.Errorf("negative: %s still says %q", r, real)
			}
		}
	}
	if body := get(base + "/export"); !strings.Contains(body, `"username":"user`) || !strings.Contains(body, "thanks @user") || !strings.Contains(body, "ask @user") {
		t.Errorf("the JSON export says user1, user2 ...: %.400s", body)
	}
	if body := get(base + "/export/csv"); !strings.Contains(body, `"ask @user`) {
		t.Errorf("the CSV's title: %.300s", body)
	}
}

// The shape, in every export file: a function that reads the users collection
// for what it writes also asks the anonymizer. import.go's copy of a board is
// no export and keeps its names.
func TestEveryExportOfUsersIsAnonymized(t *testing.T) {
	files, _ := filepath.Glob("export*.go")
	fn := regexp.MustCompile(`(?m)^func `)
	seen := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "export_anonymize.go" {
			continue
		}
		src, _ := os.ReadFile(f)
		bodies := fn.Split(string(src), -1)
		for _, body := range bodies {
			if !strings.Contains(body, `Collection("users")`) {
				continue
			}
			seen++
			name := strings.SplitN(body, "(", 3)
			if !strings.Contains(body, "exportAnonymizer(") && !strings.HasPrefix(body, "(s *Server) boardDocs(") {
				t.Errorf("%s: %s reads users for an export without the anonymizer", f, strings.Join(name[:2], "("))
			}
		}
	}
	if seen < 2 {
		t.Fatalf("the guard sees the exports' user reads: %d", seen)
	}
	// boardDocs is shared with the copy; its exports all go through
	// exportBoardWith, which exportBoard calls.
	src, _ := os.ReadFile("export.go")
	i := strings.Index(string(src), "func (s *Server) exportBoardWith(")
	if i < 0 || !strings.Contains(string(src)[i:i+800], "exportAnonymizer(") {
		t.Error("exportBoard anonymizes what boardDocs read")
	}
}
