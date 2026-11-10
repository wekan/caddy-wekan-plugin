package web

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// finishedPercent's Math.round, half up.
func TestChecklistPercent(t *testing.T) {
	for _, c := range []struct{ done, count, want int }{{0, 0, 0}, {0, 3, 0}, {1, 3, 33}, {2, 3, 67}, {1, 8, 13}, {3, 3, 100}} {
		if got := checklistPercent(c.done, c.count); got != c.want {
			t.Errorf("%d/%d: %d, want %d", c.done, c.count, got, c.want)
		}
	}
}

// The card details' checklists, sorted, with their progress and items; the
// menu that opens their Export only for whoever may change the card, and
// never another card's checklist.
func TestCardDetailsChecklists(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	ro := w.seedUser(t, "rob", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Lists")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$push": bson.M{"members": bson.M{"userId": ro.ID, "isActive": true, "isReadOnly": true}}})
	l, _ := w.s.AddList(ctx, b, "L")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	c, _ := w.s.AddCard(ctx, b, u, l.ID, sw.ID, "C")
	other, _ := w.s.AddCard(ctx, b, u, l.ID, sw.ID, "Other")
	_, _ = w.db.Collection("checklists").InsertMany(ctx, []any{
		bson.M{"_id": "clB", "cardId": c.ID, "title": "Second list", "sort": 2},
		bson.M{"_id": "clA", "cardId": c.ID, "title": "First list", "sort": 1},
		bson.M{"_id": "clX", "cardId": other.ID, "title": "Not this card", "sort": 0},
	})
	_, _ = w.db.Collection("checklistItems").InsertMany(ctx, []any{
		bson.M{"_id": "i2", "checklistId": "clA", "cardId": c.ID, "title": "Item two", "sort": 2, "isFinished": false},
		bson.M{"_id": "i1", "checklistId": "clA", "cardId": c.ID, "title": "Item one", "sort": 1, "isFinished": true},
		bson.M{"_id": "i3", "checklistId": "clA", "cardId": c.ID, "title": "Item three", "sort": 3, "isFinished": false},
	})
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	_, html := get(t, ann, w.srv.URL+"/b/"+b.ID+"/"+b.Slug+"/"+c.ID)
	first, second := strings.Index(html, "First list"), strings.Index(html, "Second list")
	if first < 0 || second < first {
		t.Fatalf("both checklists, in their sort order: %d %d", first, second)
	}
	if one, two, three := strings.Index(html, "Item one"), strings.Index(html, "Item two"), strings.Index(html, "Item three"); !(one < two && two < three && one > 0) {
		t.Errorf("the items in their sort order: %d %d %d", one, two, three)
	}
	if !strings.Contains(html, `aria-valuenow="33"`) || !strings.Contains(html, ">33%<") {
		t.Error("one of three finished is 33%")
	}
	if !regexp.MustCompile(`data-item="i1"[^>]*is-checked[^>]*aria-checked="true"|is-checked[^>]*data-item="i1"[^>]*aria-checked="true"`).MatchString(html) {
		t.Error("a finished item is checked")
	}
	if !strings.Contains(html, `js-open-checklist-details-menu" href="#" data-checklist="clA"`) || !strings.Contains(html, "js-export-checklist") {
		t.Error("the checklist's menu and its Export, for the card's editor")
	}
	if strings.Contains(html, "Not this card") {
		t.Error("negative: another card's checklist is not drawn")
	}
	if !strings.Contains(html, `data-title-checklist="Export checklist"`) {
		t.Error("the checklist's Export pop-over is titled as WeKan's")
	}
	rob := w.client(t)
	body(t, w.signIn(t, rob, "rob", "pw"))
	_, html = get(t, rob, w.srv.URL+"/b/"+b.ID+"/"+b.Slug+"/"+c.ID)
	if !strings.Contains(html, "First list") || strings.Contains(html, "js-open-checklist-details-menu") {
		t.Error("negative: a read-only member sees the checklists, without their menu")
	}
}

// The card details' checklist writes, as WeKan's: added after the last,
// items after theirs, checked and unchecked with WeKan's activities and the
// checklist's completion, deleted with its items - by whoever may change the
// card; a Worker ticks a box and nothing more; and never through another
// card's path.
func TestCardDetailsChecklistWrites(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	ro := w.seedUser(t, "rob", "pw")
	wk := w.seedUser(t, "wes", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Writes")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$push": bson.M{"members": bson.M{"$each": []any{
		bson.M{"userId": ro.ID, "isActive": true, "isReadOnly": true},
		bson.M{"userId": wk.ID, "isActive": true, "isWorker": true}}}}})
	l, _ := w.s.AddList(ctx, b, "L")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	c, _ := w.s.AddCard(ctx, b, u, l.ID, sw.ID, "C")
	other, _ := w.s.AddCard(ctx, b, u, l.ID, sw.ID, "Other")
	base := w.srv.URL + "/b/" + b.ID + "/cards/" + c.ID + "/checklists"
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	post := func(cl *http.Client, url string, v url.Values) int {
		r, err := cl.PostForm(url, v)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	acts := func(kind string) int64 {
		n, _ := w.db.Collection("activities").CountDocuments(ctx, bson.M{"boardId": b.ID, "cardId": c.ID, "activityType": kind})
		return n
	}
	if code := post(ann, base, url.Values{"title": {"First"}}); code != http.StatusSeeOther {
		t.Fatalf("added: %d", code)
	}
	_ = post(ann, base, url.Values{"title": {"Second"}})
	var cls []bson.M
	cur, _ := w.db.Collection("checklists").Find(ctx, bson.M{"cardId": c.ID}, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}}))
	_ = cur.All(ctx, &cls)
	if len(cls) != 2 || cls[0]["title"] != "First" || cls[1]["title"] != "Second" || cls[0]["boardId"] != b.ID {
		t.Fatalf("two checklists, the second after the first: %v", cls)
	}
	if acts("addChecklist") != 2 {
		t.Error("addChecklist for each")
	}
	first := docString(cls[0], "_id")
	_ = post(ann, base+"/"+first+"/items", url.Values{"title": {"One"}})
	_ = post(ann, base+"/"+first+"/items", url.Values{"title": {"Two"}})
	var items []bson.M
	cur, _ = w.db.Collection("checklistItems").Find(ctx, bson.M{"checklistId": first}, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}}))
	_ = cur.All(ctx, &items)
	if len(items) != 2 || items[0]["title"] != "One" || items[1]["title"] != "Two" || items[0]["isFinished"] != false {
		t.Fatalf("two items in order, unfinished: %v", items)
	}
	if acts("addChecklistItem") != 2 {
		t.Error("addChecklistItem for each")
	}
	one, two := docString(items[0], "_id"), docString(items[1], "_id")
	// A Worker ticks a box.
	wes := w.client(t)
	body(t, w.signIn(t, wes, "wes", "pw"))
	if code := post(wes, base+"/"+first+"/items/"+one+"/toggle", nil); code != http.StatusSeeOther {
		t.Fatalf("a Worker checks an item: %d", code)
	}
	if acts("checkedItem") != 1 || acts("completeChecklist") != 0 {
		t.Error("checkedItem, the checklist not yet complete")
	}
	_ = post(ann, base+"/"+first+"/items/"+two+"/toggle", nil)
	if acts("completeChecklist") != 1 {
		t.Error("completeChecklist when the last item is checked")
	}
	_ = post(ann, base+"/"+first+"/items/"+two+"/toggle", nil)
	if acts("uncheckedItem") != 1 || acts("uncompleteChecklist") != 1 {
		t.Error("uncheckedItem and uncompleteChecklist when a complete checklist is unchecked")
	}
	// Negative: a Worker adds and deletes nothing; a read-only member changes nothing.
	if code := post(wes, base+"/"+first+"/items", url.Values{"title": {"Worker's"}}); code != http.StatusForbidden {
		t.Errorf("negative: a Worker adds no item: %d", code)
	}
	if code := post(wes, base+"/"+first+"/delete", nil); code != http.StatusForbidden {
		t.Errorf("negative: a Worker deletes no checklist: %d", code)
	}
	rob := w.client(t)
	body(t, w.signIn(t, rob, "rob", "pw"))
	for _, u := range []string{base, base + "/" + first + "/items", base + "/" + first + "/items/" + one + "/toggle", base + "/" + first + "/delete"} {
		if code := post(rob, u, url.Values{"title": {"x"}}); code != http.StatusForbidden {
			t.Errorf("negative: a read-only member: %s %d", u, code)
		}
	}
	// Negative: another card's checklist is not reached through this card.
	_, _ = w.db.Collection("checklists").InsertOne(ctx, bson.M{"_id": "elsewhere", "cardId": other.ID, "title": "Theirs"})
	if code := post(ann, base+"/elsewhere/delete", nil); code != http.StatusNotFound {
		t.Errorf("negative: another card's checklist: %d", code)
	}
	if n, _ := w.db.Collection("checklists").CountDocuments(ctx, bson.M{"_id": "elsewhere"}); n != 1 {
		t.Error("negative: it is still there")
	}
	// Deleted with its items (#6816), and removeChecklist.
	if code := post(ann, base+"/"+first+"/delete", nil); code != http.StatusSeeOther {
		t.Fatalf("deleted: %d", code)
	}
	if n, _ := w.db.Collection("checklistItems").CountDocuments(ctx, bson.M{"checklistId": first}); n != 0 {
		t.Errorf("its items go with it: %d", n)
	}
	if acts("removeChecklist") != 1 {
		t.Error("removeChecklist")
	}
	// The page: the editor's forms, the Worker's checkbox, the reader's neither.
	_, html := get(t, ann, w.srv.URL+"/b/"+b.ID+"/"+b.Slug+"/"+c.ID)
	if !strings.Contains(html, "form.js-add-checklist") && !strings.Contains(html, `class="js-add-checklist js-inlined-form is-hidden"`) {
		t.Error("the editor's add-checklist form")
	}
	_ = post(ann, base+"/"+docString(cls[1], "_id")+"/items", url.Values{"title": {"Tick me"}})
	_, html = get(t, wes, w.srv.URL+"/b/"+b.ID+"/"+b.Slug+"/"+c.ID)
	if !strings.Contains(html, "js-toggle-checklist-item") || strings.Contains(html, "js-add-checklist-item js-inlined-form") {
		t.Error("a Worker's checkbox, and no add form")
	}
	_, html = get(t, rob, w.srv.URL+"/b/"+b.ID+"/"+b.Slug+"/"+c.ID)
	if strings.Contains(html, "js-toggle-checklist-item") || strings.Contains(html, "js-add-checklist") {
		t.Error("negative: a read-only member gets neither")
	}
}

// A checklist renamed, an item edited and deleted - by whoever may change
// the card only, and only that checklist's own item.
func TestCardDetailsChecklistEdits(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	wk := w.seedUser(t, "wes", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Edits")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$push": bson.M{"members": bson.M{"userId": wk.ID, "isActive": true, "isWorker": true}}})
	l, _ := w.s.AddList(ctx, b, "L")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	c, _ := w.s.AddCard(ctx, b, u, l.ID, sw.ID, "C")
	_, _ = w.db.Collection("checklists").InsertMany(ctx, []any{
		bson.M{"_id": "clA", "cardId": c.ID, "title": "Old name", "sort": 0},
		bson.M{"_id": "clB", "cardId": c.ID, "title": "Other", "sort": 1}})
	_, _ = w.db.Collection("checklistItems").InsertMany(ctx, []any{
		bson.M{"_id": "itA", "checklistId": "clA", "cardId": c.ID, "title": "Old item", "sort": 0},
		bson.M{"_id": "itB", "checklistId": "clB", "cardId": c.ID, "title": "B's item", "sort": 0}})
	base := w.srv.URL + "/b/" + b.ID + "/cards/" + c.ID + "/checklists/"
	post := func(cl *http.Client, u string, v url.Values) int {
		r, err := cl.PostForm(u, v)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	title := func(coll, id string) string {
		var d bson.M
		_ = w.db.Collection(coll).FindOne(ctx, bson.M{"_id": id}).Decode(&d)
		return docString(d, "title")
	}
	wes := w.client(t)
	body(t, w.signIn(t, wes, "wes", "pw"))
	for _, p := range []string{"clA/title", "clA/items/itA/title", "clA/items/itA/delete"} {
		if code := post(wes, base+p, url.Values{"title": {"Worker's"}}); code != http.StatusForbidden {
			t.Errorf("negative: a Worker edits nothing: %s %d", p, code)
		}
	}
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	if code := post(ann, base+"clA/title", url.Values{"title": {"  New name "}}); code != http.StatusSeeOther || title("checklists", "clA") != "New name" {
		t.Errorf("renamed: %d %q", code, title("checklists", "clA"))
	}
	if code := post(ann, base+"clA/title", url.Values{"title": {"  "}}); code != http.StatusBadRequest || title("checklists", "clA") != "New name" {
		t.Errorf("negative: an empty title is refused: %d", code)
	}
	if code := post(ann, base+"clA/items/itA/title", url.Values{"title": {"New item"}}); code != http.StatusSeeOther || title("checklistItems", "itA") != "New item" {
		t.Errorf("item edited: %d", code)
	}
	// Negative: B's item is not reached through checklist A.
	if code := post(ann, base+"clA/items/itB/delete", nil); code != http.StatusNotFound || title("checklistItems", "itB") != "B's item" {
		t.Errorf("negative: another checklist's item: %d", code)
	}
	if n, _ := w.db.Collection("activities").CountDocuments(ctx, bson.M{"cardId": c.ID, "activityType": bson.M{"$in": bson.A{"checkedItem", "uncheckedItem"}}}); n != 0 {
		t.Errorf("an edit is not a check: %d", n)
	}
	if code := post(ann, base+"clA/items/itA/delete", nil); code != http.StatusSeeOther || title("checklistItems", "itA") != "" {
		t.Errorf("item deleted: %d", code)
	}
	_, html := get(t, ann, w.srv.URL+"/b/"+b.ID+"/"+b.Slug+"/"+c.ID)
	if !strings.Contains(html, "js-edit-checklist-title") || !strings.Contains(html, "js-edit-checklist-item") {
		t.Error("the editor's edit forms")
	}
	_, html = get(t, wes, w.srv.URL+"/b/"+b.ID+"/"+b.Slug+"/"+c.ID)
	if strings.Contains(html, "js-edit-checklist-title") || strings.Contains(html, "js-delete-checklist-item") {
		t.Error("negative: no edit forms for a Worker")
	}
}

// moveChecklist and checklist.copy: to a card the user may change, the
// items and (on a move) the activities going along; refused for a card on
// a board the user cannot change, and never from another card's path.
func TestCardDetailsChecklistMoveAndCopy(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	other := w.seedUser(t, "olga", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "From")
	tb, _ := w.s.CreateBoard(ctx, u, "To")
	foreign, _ := w.s.CreateBoard(ctx, other, "Not ann's")
	lane := func(bb *Board) string {
		var sw Swimlane
		_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": bb.ID}).Decode(&sw)
		return sw.ID
	}
	l, _ := w.s.AddList(ctx, b, "L")
	tl, _ := w.s.AddList(ctx, tb, "TL")
	fl, _ := w.s.AddList(ctx, foreign, "FL")
	c, _ := w.s.AddCard(ctx, b, u, l.ID, lane(b), "Source")
	tc, _ := w.s.AddCard(ctx, tb, u, tl.ID, lane(tb), "Target")
	fc, _ := w.s.AddCard(ctx, foreign, other, fl.ID, lane(foreign), "Foreign")
	_, _ = w.db.Collection("checklists").InsertMany(ctx, []any{
		bson.M{"_id": "mv", "cardId": c.ID, "boardId": b.ID, "title": "Moved", "sort": 3},
		bson.M{"_id": "cp", "cardId": c.ID, "boardId": b.ID, "title": "Copied", "sort": 4}})
	_, _ = w.db.Collection("checklistItems").InsertMany(ctx, []any{
		bson.M{"_id": "mv1", "checklistId": "mv", "cardId": c.ID, "boardId": b.ID, "title": "M1", "sort": 0, "isFinished": true},
		bson.M{"_id": "cp1", "checklistId": "cp", "cardId": c.ID, "boardId": b.ID, "title": "C1", "sort": 0, "isFinished": true}})
	_, _ = w.db.Collection("activities").InsertOne(ctx, bson.M{"_id": "act", "checklistId": "mv", "cardId": c.ID, "activityType": "addChecklist"})
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	base := w.srv.URL + "/b/" + b.ID + "/cards/" + c.ID + "/checklists/"
	post := func(p string, v url.Values) int {
		r, err := ann.PostForm(base+p, v)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	// The places carry the cards the user may change, for the card select.
	_, js := get(t, ann, w.srv.URL+"/b/"+tb.ID+"/places?cards=1")
	if !strings.Contains(js, tc.ID) {
		t.Error("the target board's cards in its places")
	}
	// Negative: a card on a board ann cannot change, and a card that is not on the named board.
	if code := post("mv/move-to", url.Values{"boardId": {foreign.ID}, "cardId": {fc.ID}}); code != http.StatusNotFound && code != http.StatusForbidden {
		t.Errorf("negative: a foreign board's card: %d", code)
	}
	if code := post("mv/move-to", url.Values{"boardId": {tb.ID}, "cardId": {fc.ID}}); code != http.StatusNotFound {
		t.Errorf("negative: a card not on the named board: %d", code)
	}
	var cl bson.M
	_ = w.db.Collection("checklists").FindOne(ctx, bson.M{"_id": "mv"}).Decode(&cl)
	if cl["cardId"] != c.ID {
		t.Fatal("negative: nothing moved")
	}
	if code := post("mv/move-to", url.Values{"boardId": {tb.ID}, "cardId": {tc.ID}}); code != http.StatusSeeOther {
		t.Fatalf("moved: %d", code)
	}
	_ = w.db.Collection("checklists").FindOne(ctx, bson.M{"_id": "mv"}).Decode(&cl)
	var it, act bson.M
	_ = w.db.Collection("checklistItems").FindOne(ctx, bson.M{"_id": "mv1"}).Decode(&it)
	_ = w.db.Collection("activities").FindOne(ctx, bson.M{"_id": "act"}).Decode(&act)
	if cl["cardId"] != tc.ID || cl["boardId"] != tb.ID || it["cardId"] != tc.ID || it["boardId"] != tb.ID || act["cardId"] != tc.ID {
		t.Errorf("the checklist, its item and its activity on the target: %v %v %v", cl, it, act)
	}
	if code := post("cp/copy-to", url.Values{"boardId": {tb.ID}, "cardId": {tc.ID}}); code != http.StatusSeeOther {
		t.Fatalf("copied: %d", code)
	}
	var copied bson.M
	if w.db.Collection("checklists").FindOne(ctx, bson.M{"cardId": tc.ID, "title": "Copied"}).Decode(&copied) != nil || copied["boardId"] != tb.ID {
		t.Fatal("a copy on the target card")
	}
	var ci bson.M
	if w.db.Collection("checklistItems").FindOne(ctx, bson.M{"checklistId": copied["_id"]}).Decode(&ci) != nil || ci["title"] != "C1" || ci["isFinished"] != true || ci["cardId"] != tc.ID {
		t.Errorf("its item, as it was: %v", ci)
	}
	if n, _ := w.db.Collection("checklists").CountDocuments(ctx, bson.M{"_id": "cp", "cardId": c.ID}); n != 1 {
		t.Error("the original stays")
	}
	// Negative: the moved checklist is no longer reached through the source card.
	if code := post("mv/copy-to", url.Values{"boardId": {tb.ID}, "cardId": {tc.ID}}); code != http.StatusNotFound {
		t.Errorf("negative: a checklist no longer on this card: %d", code)
	}
}

// #1618: an item's assignee - an active member of the board, or nobody -
// set by whoever may change the card, and shown by name.
func TestChecklistItemAssignee(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	gone := w.seedUser(t, "gone", "pw")
	out := w.seedUser(t, "out", "pw")
	wk := w.seedUser(t, "wes", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Assign")
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": bob.ID}, bson.M{"$set": bson.M{"profile.fullname": "Bob Builder"}})
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$push": bson.M{"members": bson.M{"$each": []any{
		bson.M{"userId": bob.ID, "isActive": true}, bson.M{"userId": gone.ID, "isActive": false},
		bson.M{"userId": wk.ID, "isActive": true, "isWorker": true}}}}})
	l, _ := w.s.AddList(ctx, b, "L")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	c, _ := w.s.AddCard(ctx, b, u, l.ID, sw.ID, "C")
	_, _ = w.db.Collection("checklists").InsertOne(ctx, bson.M{"_id": "cl", "cardId": c.ID, "title": "L", "sort": 0})
	_, _ = w.db.Collection("checklistItems").InsertOne(ctx, bson.M{"_id": "it", "checklistId": "cl", "cardId": c.ID, "title": "Do it", "sort": 0})
	path := w.srv.URL + "/b/" + b.ID + "/cards/" + c.ID + "/checklists/cl/items/it/assignee"
	assignee := func() any {
		var d bson.M
		_ = w.db.Collection("checklistItems").FindOne(ctx, bson.M{"_id": "it"}).Decode(&d)
		return d["assigneeId"]
	}
	post := func(cl *http.Client, who string) int {
		r, err := cl.PostForm(path, url.Values{"userId": {who}})
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	if code := post(ann, bob.ID); code != http.StatusSeeOther || assignee() != bob.ID {
		t.Fatalf("assigned: %d %v", code, assignee())
	}
	if _, html := get(t, ann, w.srv.URL+"/b/"+b.ID+"/"+b.Slug+"/"+c.ID); !strings.Contains(html, ">Bob Builder</span>") {
		t.Error("shown by full name")
	}
	for _, who := range []string{out.ID, gone.ID, "nobody-at-all"} {
		if code := post(ann, who); code != http.StatusBadRequest || assignee() != bob.ID {
			t.Errorf("negative: %s is not an active member: %d", who, code)
		}
	}
	wes := w.client(t)
	body(t, w.signIn(t, wes, "wes", "pw"))
	if code := post(wes, wk.ID); code != http.StatusForbidden || assignee() != bob.ID {
		t.Errorf("negative: a Worker assigns nobody: %d", code)
	}
	if code := post(ann, ""); code != http.StatusSeeOther || assignee() != nil {
		t.Errorf("nobody: %d %v", code, assignee())
	}
}

// checklistSortableItems' drop: the items in the order given - exactly
// that checklist's items, by whoever may change the card.
func TestChecklistItemsSort(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	wk := w.seedUser(t, "wes", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Sort")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$push": bson.M{"members": bson.M{"userId": wk.ID, "isActive": true, "isWorker": true}}})
	l, _ := w.s.AddList(ctx, b, "L")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	c, _ := w.s.AddCard(ctx, b, u, l.ID, sw.ID, "C")
	_, _ = w.db.Collection("checklists").InsertMany(ctx, []any{bson.M{"_id": "cl", "cardId": c.ID, "title": "L"}, bson.M{"_id": "cl2", "cardId": c.ID, "title": "Other"}})
	for i, id := range []string{"a", "b", "c"} {
		_, _ = w.db.Collection("checklistItems").InsertOne(ctx, bson.M{"_id": id, "checklistId": "cl", "cardId": c.ID, "title": id, "sort": i})
	}
	_, _ = w.db.Collection("checklistItems").InsertOne(ctx, bson.M{"_id": "x", "checklistId": "cl2", "cardId": c.ID, "title": "x", "sort": 0})
	order := func() string {
		cur, _ := w.db.Collection("checklistItems").Find(ctx, bson.M{"checklistId": "cl"}, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}}))
		var items []bson.M
		_ = cur.All(ctx, &items)
		out := ""
		for _, it := range items {
			out += docString(it, "_id")
		}
		return out
	}
	path := w.srv.URL + "/b/" + b.ID + "/cards/" + c.ID + "/checklists/cl/items/sort"
	post := func(cl *http.Client, ids ...string) int {
		r, err := cl.PostForm(path, url.Values{"item": ids})
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	if code := post(ann, "c", "a", "b"); code != http.StatusNoContent || order() != "cab" {
		t.Fatalf("sorted: %d %s", code, order())
	}
	for _, bad := range [][]string{{"a", "b"}, {"a", "b", "c", "x"}, {"a", "a", "b"}, {"a", "b", "x"}} {
		if code := post(ann, bad...); code != http.StatusBadRequest || order() != "cab" {
			t.Errorf("negative: %v is not exactly this checklist's items: %d", bad, code)
		}
	}
	wes := w.client(t)
	body(t, w.signIn(t, wes, "wes", "pw"))
	if code := post(wes, "a", "b", "c"); code != http.StatusForbidden || order() != "cab" {
		t.Errorf("negative: a Worker reorders nothing: %d", code)
	}
	// The page: sortable for the editor, with handles when they show them.
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": u.ID}, bson.M{"$set": bson.M{"profile.showDesktopDragHandles": true}})
	if _, html := get(t, ann, w.srv.URL+"/b/"+b.ID+"/"+b.Slug+"/"+c.ID); !strings.Contains(html, `data-sort-url="/b/`+b.ID) || !strings.Contains(html, "checklistitem-handle") {
		t.Error("the editor's sortable items, with handles")
	}
	if _, html := get(t, wes, w.srv.URL+"/b/"+b.ID+"/"+b.Slug+"/"+c.ID); strings.Contains(html, `data-sort-url="/b/`) {
		t.Error("negative: not sortable for a Worker")
	}
}
