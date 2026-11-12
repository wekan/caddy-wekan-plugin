package web

import (
	"bufio"
	"context"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Through the page's own live connection: a board changed outside Weddy
// reaches the open page within a few looks of the watch.
func TestBoardWatchReachesTheLivePage(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Live")
	ac := w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	res, err := ac.Get(w.srv.URL + "/b/" + rb.ID + "/live")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	events := make(chan string, 8)
	go func() {
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "event: ") {
				events <- strings.TrimPrefix(sc.Text(), "event: ")
			}
		}
	}()
	time.Sleep(200 * time.Millisecond) // connected: the watch compares with the board as it was
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": rb.ID}, bson.M{"$set": bson.M{"allowsLocationOnMinicard": true}})
	select {
	case ev := <-events:
		if ev != "board-changed" {
			t.Fatalf("event %q", ev)
		}
	case <-time.After(3 * boardWatchEvery):
		t.Fatal("the outside change reached the page")
	}

	// A page drawn before a change, connecting after it, is told at once.
	_, page := get(t, ac, w.srv.URL+"/b/"+rb.ID+"/live-board")
	at := strings.Index(page, "/live?v=")
	if at < 0 {
		t.Fatal("the page carries its board's stamp")
	}
	drawn := page[at+len("/live?v=") : at+len("/live?v=")+24]
	if drawn != w.s.boardStamp(ctx, rb.ID) {
		t.Fatal("the stamp is the board's")
	}
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": rb.ID}, bson.M{"$set": bson.M{"allowsLocationOnMinicard": false}})
	ch := make(chan string, 1)
	w.s.liveSince(ctx, rb.ID, drawn, ch)
	if len(ch) != 1 {
		t.Fatal("a page drawn from the board before the change is told")
	}
	ch = make(chan string, 1)
	w.s.liveSince(ctx, rb.ID, w.s.boardStamp(ctx, rb.ID), ch)
	if len(ch) != 0 {
		t.Fatal("one drawn from the board as it is, not")
	}
}
