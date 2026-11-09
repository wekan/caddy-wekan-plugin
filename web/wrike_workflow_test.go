package web

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// WeKan's Wrike workflow: import the lists and the rules a status group
// means, once; export the board back; refuse what is not a workflow.
func TestWrikeWorkflow(t *testing.T) {
	w := newTestWeKan(t)
	ann := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, ann, "Delivery board")
	setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: true})
	w.s.AddList(ctx, b, "Existing")
	call := func(name string, args string) map[string]any {
		c := w.client(t)
		body(t, w.signIn(t, c, name, "pw"))
		req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/rules.importWrikeWorkflow", strings.NewReader(args))
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
	workflow := `{"kind":"workflows","data":[{"name":"Default Workflow","standard":true,"customStatuses":[{"name":"Active","group":"Active"}]},` +
		`{"name":"Delivery","standard":false,"customStatuses":[{"name":"Queued","group":"Active","color":"Blue"},{"name":"Building","group":"Active","color":"Orange"},` +
		`{"name":"Shipped","group":"Completed","color":"Green"},{"name":"Dropped","group":"Cancelled","color":"Red"},{"name":"Retired","group":"Active","hidden":true}]}]}`
	text, _ := json.Marshal(workflow)
	args := `["` + b.ID + `",` + string(text) + `]`
	out := call("ann", args)
	res, _ := out["result"].(map[string]any)
	if res == nil || res["name"] != "Delivery" || res["listsCreated"] != 4.0 || res["rulesCreated"] != 4.0 || res["notRead"] != 2.0 {
		t.Fatalf("imported: %v", out)
	}
	cur, _ := w.db.Collection("lists").Find(ctx, bson.M{"boardId": b.ID}, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}}))
	var lists []bson.M
	_ = cur.All(ctx, &lists)
	var titles []string
	for _, l := range lists {
		titles = append(titles, docString(l, "title"))
		if docString(l, "title") == "Building" && l["color"] != "orange" {
			t.Errorf("its color: %v", l["color"])
		}
	}
	if strings.Join(titles, ",") != "Queued,Building,Shipped,Dropped,Existing" {
		t.Errorf("in the workflow's order, before the board's: %v", titles)
	}
	rules := func() []string {
		var out []string
		for _, r := range w.s.boardRuleParts(ctx, b.ID) {
			out = append(out, docString(r["trigger"].(bson.M), "listName")+" "+docString(r["action"].(bson.M), "actionType"))
		}
		sort.Strings(out)
		return out
	}
	if got := strings.Join(rules(), "; "); got != "Building markCardIncomplete; Dropped markCardComplete; Queued markCardIncomplete; Shipped markCardComplete" {
		t.Errorf("the rules: %s", got)
	}
	if out := call("ann", args); out["result"].(map[string]any)["listsCreated"] != 0.0 || len(rules()) != 4 {
		t.Errorf("negative: a second import adds nothing: %v", out)
	}
	// Negative: not JSON, not a workflow, not the board's admin.
	if out := call("ann", `["`+b.ID+`","{ not json"]`); !strings.Contains(fmtAny(out["reason"]), "not JSON") {
		t.Errorf("not JSON: %v", out)
	}
	if out := call("ann", `["`+b.ID+`","{\"kind\":\"tasks\"}"]`); out["error"] != "invalid-import-format" {
		t.Errorf("not a workflow: %v", out)
	}
	if out := call("bob", args); out["error"] != "not-authorized" {
		t.Errorf("negative: a member who is not an admin: %v", out)
	}
	// Export: the board as a workflow, its groups from the rules.
	wf := wrikeWorkflowFromBoard(b.Title, w.s.boardLists(ctx, b.ID), w.s.boardRuleParts(ctx, b.ID))
	raw, _ := json.Marshal(wf)
	for _, want := range []string{`"kind":"workflows"`, `"name":"Delivery board"`, `{"color":"Green","group":"Completed","hidden":false,"name":"Shipped"`,
		`"group":"Cancelled","hidden":false,"name":"Dropped"`, `{"color":"Orange","group":"Active","hidden":false,"name":"Building"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("export has %s: %s", want, raw)
		}
	}
	if g := wrikeGroupForName("Won't fix"); g != "Cancelled" {
		t.Errorf("by name: %s", g)
	}
}
