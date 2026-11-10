package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The card's dates: set and cleared by whoever may change the card - not a
// Worker - only those the board allows, a set one writing WeKan's
// a-<field> activity with the value it replaced.
func TestCardDates(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	wk := w.seedUser(t, "wes", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Dates")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$push": bson.M{"members": bson.M{"userId": wk.ID, "isActive": true, "isWorker": true}}})
	l, _ := w.s.AddList(ctx, b, "L")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	c, _ := w.s.AddCard(ctx, b, u, l.ID, sw.ID, "C")
	base := w.srv.URL + "/b/" + b.ID + "/cards/" + c.ID + "/dates/"
	field := func(f string) any {
		var d bson.M
		_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": c.ID}).Decode(&d)
		return d[f]
	}
	post := func(cl *http.Client, kind string, v url.Values) int {
		r, err := cl.PostForm(base+kind, v)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	for _, k := range []string{"received", "start", "due", "end"} {
		if code := post(ann, k, url.Values{"at": {"2099-12-31T17:00:00.000Z"}}); code != http.StatusSeeOther || field(k+"At") == nil {
			t.Errorf("%s set: %d", k, code)
		}
	}
	_ = post(ann, "due", url.Values{"at": {"2099-12-30T17:00:00Z"}})
	var act bson.M
	if w.db.Collection("activities").FindOne(ctx, bson.M{"cardId": c.ID, "activityType": "a-dueAt", "timeOldValue": bson.M{"$type": "date"}}).Decode(&act) != nil {
		t.Error("a-dueAt with the date it replaced")
	}
	_, html := get(t, ann, w.srv.URL+"/b/"+b.ID+"/"+b.Slug+"/"+c.ID)
	if n := strings.Count(html, `class="js-edit-date card-date`); n != 4 {
		t.Errorf("four badges: %d", n)
	}
	if code := post(ann, "due", url.Values{"delete": {"1"}}); code != http.StatusSeeOther || field("dueAt") != nil {
		t.Errorf("cleared: %d", code)
	}
	// A date cleared is recorded with the date it had and no new one; clearing
	// it again records nothing.
	var cleared bson.M
	if w.db.Collection("activities").FindOne(ctx, bson.M{"cardId": c.ID, "activityType": "a-dueAt", "timeValue": bson.M{"$exists": false}}).Decode(&cleared) != nil ||
		cleared["timeOldValue"] == nil || cleared["timeOldValue"] == "" {
		t.Errorf("the clear, with its old date: %v", cleared)
	}
	_ = post(ann, "due", url.Values{"delete": {"1"}})
	if n, _ := w.db.Collection("activities").CountDocuments(ctx, bson.M{"cardId": c.ID, "activityType": "a-dueAt", "timeValue": bson.M{"$exists": false}}); n != 1 {
		t.Errorf("negative: clearing nothing records nothing: %d", n)
	}
	if _, html := get(t, ann, w.srv.URL+"/b/"+b.ID+"/"+b.Slug+"/"+c.ID); !strings.Contains(html, "js-due-date") {
		t.Error("its + again")
	}
	// Negatives: a Worker, an unknown kind, a bad year, and a date the board turned off.
	wes := w.client(t)
	body(t, w.signIn(t, wes, "wes", "pw"))
	if code := post(wes, "start", url.Values{"delete": {"1"}}); code != http.StatusForbidden || field("startAt") == nil {
		t.Errorf("negative: a Worker sets no date: %d", code)
	}
	if _, html := get(t, wes, w.srv.URL+"/b/"+b.ID+"/"+b.Slug+"/"+c.ID); strings.Contains(html, "js-due-date") || strings.Contains(html, "js-edit-date card-date") {
		t.Error("negative: no date controls for a Worker")
	}
	if code := post(ann, "archived", url.Values{"at": {"2099-12-31T17:00:00Z"}}); code != http.StatusNotFound {
		t.Errorf("negative: not a date: %d", code)
	}
	if code := post(ann, "due", url.Values{"at": {"0099-12-31T17:00:00Z"}}); code != http.StatusBadRequest || field("dueAt") != nil {
		t.Errorf("negative: a two-digit year: %d", code)
	}
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"allowsReceivedDate": false}})
	if code := post(ann, "received", url.Values{"delete": {"1"}}); code != http.StatusForbidden {
		t.Errorf("negative: the board does not use Received: %d", code)
	}
	if _, html := get(t, ann, w.srv.URL+"/b/"+b.ID+"/"+b.Slug+"/"+c.ID); strings.Contains(html, "card-details-item-received") {
		t.Error("negative: Received not shown")
	}
}

// A card date cleared does not start the "date set or changed" rules.
func TestTimingClearIsNotADateSet(t *testing.T) {
	for _, c := range []struct {
		a    bson.M
		want bool
	}{
		{bson.M{"activityType": "a-dueAt"}, true},
		{bson.M{"activityType": "a-startAt", "timeValue": nil}, true},
		{bson.M{"activityType": "a-dueAt", "timeValue": "2026-01-01"}, false},
		{bson.M{"activityType": "a-changedTitle"}, false},
		{bson.M{"activityType": "addComment"}, false},
	} {
		if got := isTimingClear(c.a); got != c.want {
			t.Errorf("%v: %v", c.a, got)
		}
	}
}
