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

// A user's profile changed elsewhere redraws that user's pages, and nobody
// else's; a new notification redraws nothing.
func TestUserWatch(t *testing.T) {
	w := newTestWeKan(t)
	w.s.boardWatchOff.Store(true) // this test looks itself
	ctx := context.Background()
	ann := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	annCh, bobCh := w.s.hub.subscribe("b1"), w.s.hub.subscribe("b1")
	defer w.s.hub.unsubscribe("b1", annCh)
	defer w.s.hub.unsubscribe("b1", bobCh)
	w.s.liveAs(ctx, ann.ID, "", "PageAnnAAAAAAAAAA", annCh)
	w.s.liveAs(ctx, bob.ID, "", "", bobCh)
	got := func(ch chan string) string {
		select {
		case ev := <-ch:
			return ev
		default:
			return ""
		}
	}
	// A profile of many fields stamps the same each look (not by a map's
	// random order).
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": ann.ID}, bson.M{"$set": bson.M{"profile.a": 1, "profile.b": 2,
		"profile.c": 3, "profile.d": 4, "profile.e": 5, "profile.f": 6, "profile.g": 7, "profile.h": 8}})
	w.s.watchUsersOnce(ctx)
	_ = got(annCh)
	for i := 0; i < 20; i++ {
		w.s.watchUsersOnce(ctx)
		if got(annCh) != "" || got(bobCh) != "" {
			t.Fatal("nothing changed")
		}
	}
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": ann.ID}, bson.M{"$set": bson.M{"profile.notifications": bson.A{bson.M{"activity": "x"}}}})
	w.s.watchUsersOnce(ctx)
	if got(annCh) != "" {
		t.Fatal("a notification redraws nothing")
	}
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": ann.ID}, bson.M{"$set": bson.M{"profile.showDesktopDragHandles": true}})
	w.s.watchUsersOnce(ctx)
	if got(annCh) != "board-changed" || got(bobCh) != "" {
		t.Fatal("ann's pages, not bob's")
	}
	w.s.watchUsersOnce(ctx)
	if got(annCh) != "" {
		t.Fatal("once")
	}
	// A page drawn after a change shows it: not told again, while ann's page
	// drawn before it is (one baseline per page, not per user).
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": ann.ID}, bson.M{"$set": bson.M{"profile.submitOnEnter": true}})
	freshCh := w.s.hub.subscribe("b1")
	defer w.s.hub.unsubscribe("b1", freshCh)
	w.s.liveAs(ctx, ann.ID, w.s.userStamp(ctx, ann.ID), "", freshCh)
	w.s.watchUsersOnce(ctx)
	if got(freshCh) != "" || got(annCh) != "board-changed" {
		t.Fatal("the fresh page is up to date, the older one is told")
	}
	// A change the page made itself (a method call naming it) is not told to
	// it again; ann's other page is told.
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": ann.ID}, bson.M{"$set": bson.M{"profile.submitOnEnter": false}})
	w.s.pageSawItsUser(ctx, ann.ID, "PageAnnAAAAAAAAAA")
	w.s.watchUsersOnce(ctx)
	if got(annCh) != "" || got(freshCh) != "board-changed" {
		t.Fatal("the page that made the change is not told; the other is")
	}
	// Negative: another user's page id moves nothing of ann's.
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": ann.ID}, bson.M{"$set": bson.M{"profile.submitOnEnter": true}})
	w.s.pageSawItsUser(ctx, bob.ID, "PageAnnAAAAAAAAAA")
	w.s.watchUsersOnce(ctx)
	if got(annCh) != "board-changed" {
		t.Fatal("bob cannot move ann's page")
	}
}

// One board baseline per page: a page drawn after an outside change is not
// told of it again; a page drawn before it is (the reload race that wiped an
// open card composer).
func TestBoardWatchPerPage(t *testing.T) {
	w := newTestWeKan(t)
	w.s.boardWatchOff.Store(true) // this test looks itself
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Pages")
	old := w.s.hub.subscribe(rb.ID)
	defer w.s.hub.unsubscribe(rb.ID, old)
	w.s.liveSince(ctx, rb.ID, w.s.boardStamp(ctx, rb.ID), old)
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": rb.ID}, bson.M{"$set": bson.M{"allowsCardCollapse": false}})
	fresh := w.s.hub.subscribe(rb.ID)
	defer w.s.hub.unsubscribe(rb.ID, fresh)
	w.s.liveSince(ctx, rb.ID, w.s.boardStamp(ctx, rb.ID), fresh)
	w.s.watchBoardsOnce(ctx)
	if len(fresh) != 0 || len(old) != 1 {
		t.Fatalf("fresh %d, old %d", len(fresh), len(old))
	}
}

// A custom field added to the board changes its stamp: Card Settings and the
// card details draw them, as WeKan's reactive page does.
func TestBoardStampFollowsCustomFields(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Stamp")
	before := w.s.boardStamp(ctx, rb.ID)
	if before != w.s.boardStamp(ctx, rb.ID) {
		t.Fatal("the same board stamps the same")
	}
	_, _ = w.db.Collection("customFields").InsertOne(ctx, bson.M{"_id": "cfX", "boardIds": bson.A{rb.ID}, "name": "X", "type": "text"})
	if w.s.boardStamp(ctx, rb.ID) == before {
		t.Fatal("a new field changes it")
	}
	// Negative: another board's field does not.
	other := w.s.boardStamp(ctx, rb.ID)
	_, _ = w.db.Collection("customFields").InsertOne(ctx, bson.M{"_id": "cfY", "boardIds": bson.A{"elsewhere"}, "name": "Y", "type": "text"})
	if w.s.boardStamp(ctx, rb.ID) != other {
		t.Fatal("another board's field changes nothing here")
	}
}
