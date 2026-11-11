package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestRuleHistory(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Rules")
	setMember(t, w, rb.ID, Member{UserID: bob.ID, IsActive: true})
	caller := func(name string) func(method, args string) map[string]any {
		c := w.client(t)
		body(t, w.signIn(t, c, name, "pw"))
		return func(method, args string) map[string]any {
			req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/"+url.PathEscape(method), strings.NewReader(args))
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
	ann, bobCall := caller("ann"), caller("bob")
	out := ann("rules.createRule", `["`+rb.ID+`","First",{"activityType":"createCard"},{"actionType":"archive"}]`)
	ids, _ := out["result"].(map[string]any)
	ruleID, _ := ids["_id"].(string)
	if ruleID == "" {
		t.Fatalf("created: %v", out)
	}
	rows := func() []map[string]any {
		var out []map[string]any
		for _, r := range historyRows(t, w, rb.ID) {
			if r["entityType"] == "rule" && r["isCheckpoint"] != true {
				out = append(out, r)
			}
		}
		return out
	}
	if r := rows(); len(r) != 1 || r[0]["changeType"] != "added" {
		t.Fatalf("one added row: %d", len(r))
	}
	title := func() string {
		var d bson.M
		_ = w.db.Collection("rules").FindOne(ctx, bson.M{"_id": ruleID}).Decode(&d)
		return docString(d, "title")
	}
	ann("rules.updateRule", `["`+ruleID+`","Second",{"activityType":"createCard"},{"actionType":"unarchive"}]`)
	if r := rows(); len(r) != 2 || r[1]["changeType"] != "edited" {
		t.Fatalf("one compound edit is one row: %d", len(r))
	}
	ann("changeHistory.undoLast", `["`+rb.ID+`"]`)
	if title() != "First" {
		t.Errorf("undo: %s", title())
	}
	ann("changeHistory.redoLast", `["`+rb.ID+`"]`)
	if title() != "Second" {
		t.Errorf("redo: %s", title())
	}
	// Another admin's newer edit is not overwritten by undo.
	_, _ = w.db.Collection("rules").UpdateOne(ctx, bson.M{"_id": ruleID}, bson.M{"$set": bson.M{"title": "Someone else's"}})
	if out := ann("changeHistory.undoLast", `["`+rb.ID+`"]`); out["error"] != "history-conflict" || title() != "Someone else's" {
		t.Errorf("negative: undo over a newer edit: %v", out)
	}
	// An ordinary member cannot restore a rule through History.
	if out := bobCall("changeHistory.restore", `[["`+docString(rows()[0], "_id")+`"]]`); out["error"] != "not-authorized" {
		t.Errorf("negative: a member restores a rule: %v", out)
	}
	// Deleted, then restored from History.
	_, _ = w.db.Collection("rules").UpdateOne(ctx, bson.M{"_id": ruleID}, bson.M{"$set": bson.M{"title": "Second"}})
	ann("rules.deleteRule", `["`+ruleID+`"]`)
	r := rows()
	if r[len(r)-1]["changeType"] != "removed" || title() != "" {
		t.Fatalf("deleted, one removed row")
	}
	// A deletion's row restores "no rule" (WeKan's too): the row before it,
	// the last edit, brings the rule back.
	if out := ann("changeHistory.restore", `[["`+docString(r[len(r)-2], "_id")+`"]]`); canonical(out["result"]) != `{"restored":1,"skipped":0}` || title() != "Second" {
		t.Errorf("restored from History: %v %s", out, title())
	}
	if f := verifyWithWeKan(t, historyRows(t, w, rb.ID)); len(f) != 0 {
		t.Errorf("WeKan's audit: %v", f)
	}
}
