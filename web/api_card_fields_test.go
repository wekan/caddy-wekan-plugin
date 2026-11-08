package web

import (
	"strings"
	"testing"
)

// The expected orders are what WeKan's own models/lib/cardFieldOrder.js answers
// for the same input (run with Node.js), so this Go port is held to WeKan.
func TestApplyCardFieldOrderAsWeKan(t *testing.T) {
	for in, want := range map[string]string{
		"":                                 "labels,date,members,card,time,customFields,vote,poker,description,scrum,checklists,subtasks,attachments,comments",
		"comments,labels":                  "comments,labels,date,members,card,time,customFields,vote,poker,description,scrum,checklists,subtasks,attachments",
		"dates,description,labels":         "date,description,labels,members,card,time,customFields,vote,poker,scrum,checklists,subtasks,attachments,comments",
		"members,customFields":             "members,customFields,labels,date,card,time,vote,poker,description,scrum,checklists,subtasks,attachments,comments",
		"descriptionText,checklists,bogus": "description,checklists,labels,date,members,card,time,customFields,vote,poker,scrum,subtasks,attachments,comments",
		"sort,voteAndPoker":                "card,time,vote,poker,labels,date,members,customFields,description,scrum,checklists,subtasks,attachments,comments",
		"poker,subtasks,attachments,time":  "poker,subtasks,attachments,time,labels,date,members,card,customFields,vote,description,scrum,checklists,comments",
	} {
		var stored []string
		if in != "" {
			stored = strings.Split(in, ",")
		}
		if got := strings.Join(applyCardFieldOrder(stored), ","); got != want {
			t.Errorf("%q:\n got  %s\n want %s", in, got, want)
		}
	}
	if strings.Join(cardFieldOrderKeys(), ",") != "labels,date,members,card,time,customFields,vote,poker,description,scrum,checklists,subtasks,attachments,comments" {
		t.Error("CARD_FIELD_ORDER_KEYS")
	}
}

func TestRESTCardFieldOrder(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	a, bt := login(t, w, "ann"), login(t, w, "bob")
	b := newRESTBoard(t, w, a, "Order")
	url := w.srv.URL + "/api/boards/" + b.ID + "/cardFieldOrder"
	var got struct {
		Order []string `json:"cardFieldOrder"`
		Keys  []string `json:"keys"`
	}
	if code := call(t, "GET", url, a, "", &got); code != 200 || got.Order[0] != "labels" || len(got.Keys) != 14 {
		t.Fatalf("default: %d %v", code, got)
	}
	for _, body := range []string{`{"cardFieldOrder":["comments","labels"]}`, `{"cardFieldOrder":"[\"comments\",\"labels\"]"}`, `{"cardFieldOrder":"comments, labels"}`} {
		if code := call(t, "PUT", url, a, body, &got); code != 200 || got.Order[0] != "comments" || got.Order[1] != "labels" {
			t.Fatalf("%s: %d %v", body, code, got.Order)
		}
	}
	call(t, "GET", url, a, "", &got)
	if got.Order[0] != "comments" {
		t.Fatal("kept")
	}
	if code := call(t, "PUT", url, a, `{"cardFieldOrder":5}`, nil); code != 400 {
		t.Fatalf("negative: not a list: %d", code)
	}
	setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: true})
	if code := call(t, "PUT", url, bt, `{"cardFieldOrder":["labels"]}`, nil); code != 403 {
		t.Fatalf("negative: not a board admin: %d", code)
	}
}
