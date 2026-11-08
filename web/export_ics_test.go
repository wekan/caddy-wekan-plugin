package web

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestICSHelpers(t *testing.T) {
	if got := icsEscape("a,b;c\\d\ne"); got != `a\,b\;c\\d\ne` {
		t.Fatalf("escape: %s", got)
	}
	long := strings.Repeat("x", 160)
	folded := icsFold(long)
	parts := strings.Split(folded, "\r\n ")
	if len(parts) != 3 || len([]rune(parts[0])) != 75 || len([]rune(parts[1])) != 74 || strings.Join(parts, "") != long {
		t.Fatalf("folded at 75, then 74 after the space: %q", folded)
	}
	when := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	out := cardsToICS([]bson.M{{"_id": "c1", "title": "Ship", "dueAt": bson.NewDateTimeFromTime(when), "startAt": bson.NewDateTimeFromTime(when)}},
		"WeKan - B", func(bson.M) string { return "https://x/b/1/b/c1" }, when)
	for _, want := range []string{"BEGIN:VCALENDAR\r\n", "X-WR-CALNAME:WeKan - B", "UID:c1-span@wekan", "UID:c1-due@wekan",
		"SUMMARY:Ship (due)", "DTSTART:20300102T030405Z", "URL:https://x/b/1/b/c1", "END:VCALENDAR\r\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("the feed has %q", want)
		}
	}
	if strings.Contains(out, "received@wekan") {
		t.Error("negative: no received event without a received date")
	}
}

func TestExportICS(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	w.seedUser(t, "bob", "pw")
	a, bt := login(t, w, "ann"), login(t, w, "bob")
	b := newRESTBoard(t, w, a, "Cal")
	_, _ = w.db.Collection("cards").UpdateOne(context.Background(), bson.M{"_id": b.Card},
		bson.M{"$set": bson.M{"dueAt": time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)}})
	get := func(url, token string) (int, string, string) {
		req, _ := http.NewRequest("GET", url, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := testClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return res.StatusCode, res.Header.Get("Content-Type"), string(body)
	}
	url := w.srv.URL + "/api/boards/" + b.ID + "/calendar.ics"
	code, typ, body := get(url, a)
	if code != 200 || typ != "text/calendar; charset=utf-8" || !strings.Contains(body, "UID:"+b.Card+"-due@wekan") ||
		!strings.Contains(body, "/b/"+b.ID+"/") {
		t.Fatalf("ics: %d %s %s", code, typ, body)
	}
	if code, _, _ = get(url, ""); code != 401 {
		t.Fatalf("negative: no token on a private board: %d", code)
	}
	if code, _, _ = get(url, bt); code != 403 {
		t.Fatalf("negative: a non-member: %d", code)
	}
	if code, _, _ = get(url+"?authToken=wrongtoken", ""); code != 401 {
		t.Fatalf("negative: a token that is no login token: %d", code)
	}
	if code, _, _ = get(url+"?authToken="+a, ""); code != 200 {
		t.Fatalf("a calendar app's link with the token: %d", code)
	}
}
