package web

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestUniqueFileNames(t *testing.T) {
	got := uniqueFileNames([]string{"My Board!", "my board", "", "../x"}, "csv")
	if strings.Join(got, ",") != "My-Board.csv,my-board-2.csv,board.csv,x.csv" {
		t.Fatalf("unique, safe names: %v", got)
	}
}

func TestMassBoardIDsAndSkippedReport(t *testing.T) {
	input := []string{" board1 ", "board1", "../invalid"}
	for i := 2; i <= maxMassExportBoards+1; i++ {
		input = append(input, "board"+strconv.Itoa(i))
	}
	got := massBoardIDs(strings.Join(input, ","))
	if len(got) != maxMassExportBoards || got[0] != "board1" ||
		got[len(got)-1] != "board"+strconv.Itoa(maxMassExportBoards) {
		t.Fatalf("invalid board selection: ids=%q count=%d", got, len(got))
	}
	if got := skippedBoardLine(" Board\r\nname\u2028example ", "board1", "Export failed"); got != "Board  name example (board1): Export failed" {
		t.Fatalf("unexpected report line: %q", got)
	}
}

func TestExportAllBoards(t *testing.T) {
	w := newTestWeKan(t)
	ann := w.seedUser(t, "ann", "pw")
	w.seedUser(t, "bob", "pw")
	a, bt := login(t, w, "ann"), login(t, w, "bob")
	alpha := newRESTBoard(t, w, a, "Alpha")
	if _, err := w.db.Collection("cards").UpdateOne(context.Background(), bson.M{"_id": alpha.Card},
		bson.M{"$set": bson.M{"dueAt": time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.db.Collection("card_comments").InsertOne(context.Background(),
		bson.M{"_id": randomID(), "cardId": alpha.Card, "boardId": alpha.ID, "text": "draft comment"}); err != nil {
		t.Fatal(err)
	}
	beta := newRESTBoard(t, w, a, "Beta")
	newRESTBoard(t, w, bt, "Bob's own")
	setMember(t, w, beta.ID, Member{UserID: ann.ID, IsActive: true, IsReadAssignedOnly: true})
	get := func(url, token string) (int, []byte) {
		req, _ := http.NewRequest("GET", url, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := testClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, b
	}
	files := func(body []byte) map[string]string {
		zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, f := range zr.File {
			rc, _ := f.Open()
			b, _ := io.ReadAll(rc)
			rc.Close()
			out[f.Name] = string(b)
		}
		return out
	}
	code, body := get(w.srv.URL+"/api/export-all-boards/csv", a)
	if code != 200 {
		t.Fatalf("csv: %d %s", code, body)
	}
	got := files(body)
	names := []string{}
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "Alpha.csv,skipped.txt" || !strings.Contains(got["skipped.txt"], "Beta") {
		t.Fatalf("the user's boards; one they may not export is named in skipped.txt: %v %q", names, got["skipped.txt"])
	}
	if strings.Contains(strings.Join(names, ""), "Bob") {
		t.Fatal("negative: never another user's board")
	}
	code, body = get(w.srv.URL+"/api/export-all-boards/wekan?authToken="+a, "")
	if got = files(body); code != 200 || !strings.Contains(got["Alpha.json"], "wekan-board-1.0.0") || !strings.Contains(got["Alpha.json"], "draft comment") {
		t.Fatalf("wekan: %d %v", code, got)
	}
	code, body = get(w.srv.URL+"/api/export-all-boards/wekan?fields=dates", a)
	if code != 200 {
		t.Fatalf("selected WeKan export: %d %s", code, body)
	}
	var doc struct{ Cards, Comments []map[string]any }
	if err := json.Unmarshal([]byte(files(body)["Alpha.json"]), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Cards) != 1 || doc.Cards[0]["dueAt"] != "2026-10-10T00:00:00.000Z" {
		t.Fatalf("selected date: %+v", doc.Cards)
	}
	if doc.Comments == nil || len(doc.Comments) != 0 {
		t.Fatalf("expected an empty comments array: %+v", doc.Comments)
	}
	code, body = get(w.srv.URL+"/api/export-all-boards/scsv", a)
	if got = files(body); !strings.Contains(got["Alpha.csv"], `"Title";"Description"`) {
		t.Fatalf("scsv is semicolons: %q", got["Alpha.csv"])
	}
	if code, _ = get(w.srv.URL+"/api/export-all-boards/html", a); code != 404 {
		t.Fatalf("negative: an unknown format: %d", code)
	}
	if code, _ = get(w.srv.URL+"/api/export-all-boards/csv", ""); code != 401 {
		t.Fatalf("negative: nobody signed in: %d", code)
	}
	if code, _ = get(w.srv.URL+"/api/export-all-boards/csv?boardIds="+beta.ID, a); code != 404 {
		t.Fatalf("negative: only boards that may not be exported: %d", code)
	}
}
