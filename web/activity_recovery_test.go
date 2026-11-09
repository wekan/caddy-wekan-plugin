package web

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Admin Panel / Problems / Recovery's activity notifications, as WeKan's
// activity-notification-recovery spec drives them: the report, pause and
// resume with revisions, cancellation, retry, and giving up.
func TestActivityNotificationRecoveryControls(t *testing.T) {
	w := newTestWeKan(t)
	boss := w.seedUser(t, "boss", "pw")
	bob := w.seedUser(t, "bob", "pw")
	w.seedUser(t, "eve", "pw")
	ctx := context.Background()
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": boss.ID}, bson.M{"$set": bson.M{"isAdmin": true}})
	client := func(name string) func(method, args string) map[string]any {
		c := w.client(t)
		body(t, w.signIn(t, c, name, "pw"))
		return func(method, args string) map[string]any {
			req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/"+method, strings.NewReader(args))
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
	asBoss, asEve := client("boss"), client("eve")
	now := time.Now().UTC().Truncate(time.Millisecond)
	// seed is the spec's seed(): an intent, and with recipients its activity
	// and its frozen plan.
	seed := func(id string, createdAt time.Time, recipients []any) string {
		activity := map[string]any{"_id": id, "activityType": "createCard", "createdAt": createdAt, "modifiedAt": createdAt, "privateContent": "PRIVATE BODY"}
		hash := sha256Hex(canonical(activity))
		if recipients != nil {
			_, _ = w.db.Collection("activities").InsertOne(ctx, activity)
			p := map[string]any{"version": 1, "activityId": id, "activityHash": hash, "dispatchUserId": nil, "recipients": recipients}
			_, _ = w.db.Collection("activityNotificationPlans").InsertOne(ctx, bson.M{"_id": planIDFor(id), "checksum": sha256Hex(canonical(p)), "plan": p})
		}
		_, _ = w.db.Collection("activityNotificationIntents").InsertOne(ctx, bson.M{"_id": intentIDFor(id), "version": 1, "state": "pending",
			"activity": activity, "activityHash": hash, "dispatchUserId": nil, "writerId": "test-writer"})
		return intentIDFor(id)
	}
	mail := func(id string) []any {
		return []any{map[string]any{"userId": bob.ID, "tray": false, "email": map[string]any{"userId": bob.ID, "eventId": id, "boardId": nil, "cardId": nil,
			"language": "en", "subject": "PRIVATE SUBJECT", "html": "PRIVATE BODY"}}}
	}
	jobs := func(id string) int64 {
		n, _ := w.db.Collection("notificationEmailJobs").CountDocuments(ctx, bson.M{"eventId": id})
		return n
	}
	report := func(search string, page int) map[string]any {
		out := asBoss("activityNotificationRecoveryReport", `[{"search":"`+search+`","page":`+itoa(page)+`}]`)
		r, _ := out["result"].(map[string]any)
		if r == nil {
			t.Fatalf("report: %v", out)
		}
		return r
	}
	rows := func(r map[string]any) []map[string]any {
		var out []map[string]any
		for _, x := range r["rows"].([]any) {
			out = append(out, x.(map[string]any))
		}
		return out
	}

	// The report: ten a page, an orphan "missing" with no retry, nothing private.
	for i := 0; i < 13; i++ {
		seed("orphanreport"+itoa(i), now, nil)
	}
	r := report("orphanreport", 1)
	if r["total"].(float64) != 13 || len(rows(r)) != 10 || rows(r)[0]["status"] != "missing" || rows(r)[0]["canRetry"] != false || rows(r)[0]["canControl"] != true {
		t.Fatalf("report: %v", r)
	}
	if got := len(rows(report("orphanreport", 2))); got != 3 {
		t.Errorf("page 2: %d", got)
	}
	raw, _ := json.Marshal(r)
	if regexp.MustCompile(`PRIVATE|recipients|writerId|activityHash`).Match(raw) {
		t.Errorf("negative: the report leaks: %s", raw)
	}
	if n, _ := w.db.Collection("activities").CountDocuments(ctx, bson.M{"_id": bson.M{"$regex": "^orphanreport"}}); n != 0 {
		t.Error("negative: a report never recreates an activity")
	}
	// Negative: nobody but an administrator, for every method.
	any0 := intentIDFor("orphanreport0")
	for method, args := range map[string]string{
		"activityNotificationRecoveryReport":   `[{"search":"","page":1}]`,
		"retryActivityNotification":            `[{"intentId":"` + any0 + `"}]`,
		"controlActivityNotificationRecovery":  `[{"intentId":"` + any0 + `","paused":true,"expectedRevision":0,"requestId":"` + strings.Repeat("a", 32) + `"}]`,
		"cancelActivityNotificationRecovery":   `[{"intentId":"` + any0 + `","expectedRevision":0,"requestId":"` + strings.Repeat("b", 32) + `"}]`,
		"cancelActivityNotificationsOlderThan": `[{"days":0}]`,
		"retryAllActivityNotifications":        `[]`,
	} {
		if out := asEve(method, args); out["error"] != "not-authorized" {
			t.Errorf("negative: %s by a non-admin: %v", method, out)
		}
	}
	if n, _ := w.db.Collection("activityNotificationControls").CountDocuments(ctx, bson.M{}); n != 0 {
		t.Error("negative: a refused call wrote a control")
	}
	// Negative: arguments are checked as Meteor's check() does.
	for _, args := range []string{`[{"search":"","page":"1"}]`, `[{"search":""}]`, `[{"search":"","page":1,"x":1}]`, `[]`} {
		if out := asBoss("activityNotificationRecoveryReport", args); out["error"] != "Match.Error" {
			t.Errorf("negative: %s: %v", args, out)
		}
	}

	// Pause, resume, a stale request, and a retry the recipient refuses.
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": bob.ID}, bson.M{"$set": bson.M{"loginDisabled": true, "profile.notifyOverrideEmail": true}})
	held := seed("manualretry00001", now, mail("manualretry00001"))
	control := func(paused bool, rev int, req string) map[string]any {
		return asBoss("controlActivityNotificationRecovery", `[{"intentId":"`+held+`","paused":`+map[bool]string{true: "true", false: "false"}[paused]+`,"expectedRevision":`+itoa(rev)+`,"requestId":"`+req+`"}]`)
	}
	first := strings.Repeat("p", 24)
	if out := control(true, 0, first); out["result"] == nil || out["result"].(map[string]any)["revision"].(float64) != 1 {
		t.Fatalf("pause: %v", out)
	}
	if out := control(true, 0, first); out["result"].(map[string]any)["revision"].(float64) != 1 {
		t.Errorf("the same request again answers the same: %v", out)
	}
	after := ""
	w.s.recoverActivityNotifications(ctx, &after)
	if jobs("manualretry00001") != 0 || rows(report("manualretry00001", 1))[0]["canRetry"] != false {
		t.Error("negative: a paused notification is not delivered, nor retried")
	}
	if out := asBoss("retryActivityNotification", `[{"intentId":"`+held+`"}]`); out["error"] != "activity-recovery-paused" {
		t.Errorf("retry paused: %v", out)
	}
	if out := control(false, 1, strings.Repeat("r", 24)); out["result"].(map[string]any)["revision"].(float64) != 2 {
		t.Fatalf("resume: %v", out)
	}
	if out := control(true, 0, first); out["error"] != "activity-recovery-control-conflict" {
		t.Errorf("negative: a stale pause cannot undo the resume: %v", out)
	}
	if out := asBoss("retryActivityNotification", `[{"intentId":"`+held+`"}]`); out["error"] != "activity-recovery-denied" || jobs("manualretry00001") != 0 {
		t.Errorf("negative: a disabled recipient gets nothing: %v", out)
	}
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": bob.ID}, bson.M{"$set": bson.M{"loginDisabled": false}})
	if out := asBoss("retryActivityNotification", `[{"intentId":"`+held+`"}]`); out["result"] == nil || out["result"].(map[string]any)["status"] != "completed" || jobs("manualretry00001") != 1 {
		t.Fatalf("retry: %v", out)
	}
	if out := asBoss("retryActivityNotification", `[{"intentId":"`+held+`"}]`); out["result"].(map[string]any)["status"] != "skipped" || jobs("manualretry00001") != 1 {
		t.Errorf("negative: delivered once: %v", out)
	}
	if len(rows(report("manualretry00001", 1))) != 0 {
		t.Error("completed work leaves the report")
	}

	// Cancellation: nothing more delivered, never resumed, the content gone.
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": bob.ID}, bson.M{"$set": bson.M{"loginDisabled": true}})
	cancelled := seed("cancelactivity01", now, mail("cancelactivity01"))
	req := strings.Repeat("c", 32)
	cancel := `[{"intentId":"` + cancelled + `","expectedRevision":0,"requestId":"` + req + `"}]`
	if out := asBoss("cancelActivityNotificationRecovery", cancel); canonical(out["result"]) != `{"cancelled":true,"paused":true,"revision":1}` {
		t.Fatalf("cancel: %v", out)
	}
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": bob.ID}, bson.M{"$set": bson.M{"loginDisabled": false}})
	if out := asBoss("retryActivityNotification", `[{"intentId":"`+cancelled+`"}]`); out["error"] != "activity-recovery-status-cancelled" {
		t.Errorf("negative: retry a cancelled one: %v", out)
	}
	if out := asBoss("controlActivityNotificationRecovery", `[{"intentId":"`+cancelled+`","paused":false,"expectedRevision":1,"requestId":"late-resume-request-1234"}]`); out["error"] != "activity-recovery-control-conflict" {
		t.Errorf("negative: resume a cancelled one: %v", out)
	}
	if out := asBoss("cancelActivityNotificationRecovery", cancel); canonical(out["result"]) != `{"cancelled":true,"paused":true,"revision":1}` {
		t.Errorf("the same cancellation again: %v", out)
	}
	var intent, plan bson.M
	_ = w.db.Collection("activityNotificationIntents").FindOne(ctx, bson.M{"_id": cancelled}).Decode(&intent)
	_ = w.db.Collection("activityNotificationPlans").FindOne(ctx, bson.M{"_id": planIDFor("cancelactivity01")}).Decode(&plan)
	if intent["state"] != "cancelled" || intent["activity"] != nil || intent["activityId"] != "cancelactivity01" || len(intent) != 10 {
		t.Errorf("the intent is a cancelled receipt: %v", intent)
	}
	if plan["cancelled"] != true || plan["plan"] != nil || strings.Contains(canonical(plan), "PRIVATE") {
		t.Errorf("the plan is a tombstone: %v", plan)
	}
	if jobs("cancelactivity01") != 0 {
		t.Error("negative: a cancelled one is never delivered")
	}
	if row := rows(report("cancelactivity01", 1)); len(row) != 1 || row[0]["status"] != "cancelled" || row[0]["canControl"] != false {
		t.Errorf("listed as cancelled: %v", row)
	}
	// An orphan can be cancelled too: a tombstone for a plan never made.
	orphan := seed("cancelorphan0001", now, nil)
	if out := asBoss("cancelActivityNotificationRecovery", `[{"intentId":"`+orphan+`","expectedRevision":0,"requestId":"`+strings.Repeat("d", 32)+`"}]`); out["result"] == nil {
		t.Fatalf("cancel an orphan: %v", out)
	}
	_ = w.db.Collection("activityNotificationPlans").FindOne(ctx, bson.M{"_id": planIDFor("cancelorphan0001")}).Decode(&plan)
	if plan["cancelled"] != true {
		t.Errorf("negative: a late writer cannot make its plan: %v", plan)
	}

	// Giving up: an orphan past the grace period, and anything expired.
	stuck := seed("stuckorphan00001", now.Add(-11*time.Minute), nil)
	fresh := seed("freshorphan00001", now, nil)
	old := seed("expiredactivity1", now.Add(-8*24*time.Hour), mail("expiredactivity1"))
	after = ""
	w.s.recoverActivityNotifications(ctx, &after)
	state := func(id string) any {
		var r bson.M
		_ = w.db.Collection("activityNotificationIntents").FindOne(ctx, bson.M{"_id": id}).Decode(&r)
		return r["state"]
	}
	if state(stuck) != "cancelled" || state(old) != "cancelled" || jobs("expiredactivity1") != 0 {
		t.Errorf("given up: %v %v", state(stuck), state(old))
	}
	if state(fresh) != "pending" {
		t.Errorf("negative: an activity being written right now is never given up: %v", state(fresh))
	}
	var c bson.M
	_ = w.db.Collection("activityNotificationControls").FindOne(ctx, bson.M{"_id": stuck}).Decode(&c)
	if c["actorId"] != "system:activity-notification-activity-unconfirmed" {
		t.Errorf("given up by the system, saying why: %v", c)
	}
	// Cancel older than: the whole list at once.
	seed("bulkold000000001", now.Add(-3*24*time.Hour), nil)
	if out := asBoss("cancelActivityNotificationsOlderThan", `[{"days":2}]`); canonical(out["result"]) != `{"cancelled":1,"remaining":0}` {
		t.Errorf("cancel older: %v", out)
	}
}

// The page: a site admin's, nobody else's, with the strings in the language.
func TestRecoveryPage(t *testing.T) {
	w := newTestWeKan(t)
	boss := w.seedUser(t, "boss", "pw")
	w.seedUser(t, "eve", "pw")
	_, _ = w.db.Collection("users").UpdateOne(context.Background(), bson.M{"_id": boss.ID}, bson.M{"$set": bson.M{"isAdmin": true}})
	c := w.client(t)
	body(t, w.signIn(t, c, "boss", "pw"))
	res, page := get(t, c, w.srv.URL+"/admin/problems/recovery")
	if res.StatusCode != 200 || !strings.Contains(page, "activity-notification-recovery-reports") || !strings.Contains(page, "Pending activity notifications") ||
		!strings.Contains(page, `data-k="activity-recovery-status-missing"`) {
		t.Fatalf("page: %d", res.StatusCode)
	}
	e := w.client(t)
	body(t, w.signIn(t, e, "eve", "pw"))
	if res, page := get(t, e, w.srv.URL+"/admin/problems/recovery"); res.StatusCode != 404 || strings.Contains(page, "activity-notification-recovery-reports") {
		t.Errorf("negative: not for a non-admin: %d", res.StatusCode)
	}
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}
