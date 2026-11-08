package web

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestComputeSortForIndex(t *testing.T) {
	for _, c := range []struct {
		s    []float64
		pos  int
		want float64
	}{{nil, 3, 0}, {[]float64{1, 2, 3}, 0, 0}, {[]float64{1, 2, 3}, 1, 1.5}, {[]float64{1, 2, 3}, 9, 4}, {[]float64{1, 2}, -2, 0}} {
		if got := computeSortForIndex(c.s, c.pos); got != c.want {
			t.Errorf("computeSortForIndex(%v, %d) = %v, want %v", c.s, c.pos, got, c.want)
		}
	}
}

func TestCrossBoardLabels(t *testing.T) {
	from := &Board{Labels: []Label{{ID: "a", Name: "Bug", Color: "red"}, {ID: "b", Name: "Idea", Color: "blue"}, {ID: "c", Color: "green"}}}
	to := &Board{Labels: []Label{{ID: "x", Name: "Bug", Color: "orange"}}}
	got, create := crossBoardLabels(from, to, []string{"a", "b", "c"}, false)
	if len(got) != 1 || got[0] != "x" || len(create) != 0 {
		t.Fatalf("by name; without the right to create, a missing one is dropped: %v %v", got, create)
	}
	got, create = crossBoardLabels(from, to, []string{"a", "b"}, true)
	if len(got) != 2 || len(create) != 1 || create[0].Name != "Idea" || create[0].Color != "blue" {
		t.Fatalf("a board admin's move creates the missing label: %v %v", got, create)
	}
}

func TestRESTListsSwimlanesAndBoardMoves(t *testing.T) {
	w := newTestWeKan(t)
	ann := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	a, bt := login(t, w, "ann"), login(t, w, "bob")
	src, dest := newRESTBoard(t, w, a, "Src"), newRESTBoard(t, w, a, "Dest")
	ctx := context.Background()
	count := func(coll string, f bson.M) int64 {
		n, _ := w.db.Collection(coll).CountDocuments(ctx, f)
		return n
	}
	list := w.srv.URL + "/api/boards/" + src.ID + "/lists/" + src.List

	// Archive and restore a list.
	var res map[string]any
	if code := call(t, "POST", list+"/archive", a, "", &res); code != 200 || res["archived"] != true || count("lists", bson.M{"_id": src.List, "archived": true}) != 1 {
		t.Fatalf("archive: %d %v", code, res)
	}
	if code := call(t, "POST", list+"/archive", a, "", nil); code != 404 {
		t.Fatalf("negative: an archived list again: %d", code)
	}
	call(t, "POST", list+"/unarchive", a, "", nil)
	if count("lists", bson.M{"_id": src.List, "archived": false}) != 1 {
		t.Fatal("restored")
	}

	// A label on the card, a comment, and a checklist travel along.
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": src.ID}, bson.M{"$push": bson.M{"labels": bson.M{"_id": "lbl001", "name": "Bug", "color": "red"}}})
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": src.Card}, bson.M{"$set": bson.M{"labelIds": bson.A{"lbl001"}, "members": bson.A{bob.ID}}})
	call(t, "POST", w.srv.URL+"/api/boards/"+src.ID+"/cards/"+src.Card+"/comments", a, `{"comment":"travels"}`, nil)
	call(t, "POST", w.srv.URL+"/api/boards/"+src.ID+"/cards/"+src.Card+"/checklists", a, `{"title":"CL","items":["x"]}`, nil)

	// The destination already has a list "L" (a copy would go into it, as
	// WeKan's planListCopy): this one is renamed so the copy makes a new list.
	_, _ = w.db.Collection("lists").UpdateOne(ctx, bson.M{"_id": src.List}, bson.M{"$set": bson.M{"title": "Source list"}})
	// Copy the list to the other board: a new list there, the card copied with
	// its label (created by the board's admin), checklist and comment.
	var cp struct {
		ID string `json:"_id"`
	}
	if code := call(t, "POST", list+"/copy", a, `{"toBoardId":"`+dest.ID+`","toSwimlaneId":"`+dest.Swimlane+`","position":0}`, &cp); code != 200 || cp.ID == "" || cp.ID == dest.List {
		t.Fatalf("copy list: %d", code)
	}
	var copied bson.M
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"boardId": dest.ID, "listId": cp.ID}).Decode(&copied)
	var db Board
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": dest.ID}).Decode(&db)
	bug := ""
	for _, l := range db.Labels {
		if l.Name == "Bug" {
			bug = l.ID
		}
	}
	if copied["title"] != "C" || bug == "" || len(stringsOf(copied["labelIds"])) != 1 || stringsOf(copied["labelIds"])[0] != bug {
		t.Fatalf("the copied card, its label by name: %v %v", copied, db.Labels)
	}
	if len(stringsOf(copied["members"])) != 0 {
		t.Fatal("negative: a member who is not on the destination board is dropped")
	}
	if count("card_comments", bson.M{"cardId": copied["_id"], "boardId": dest.ID}) != 1 || count("checklists", bson.M{"cardId": copied["_id"]}) != 1 {
		t.Fatal("the comment and the checklist are copied")
	}
	if count("cards", bson.M{"_id": src.Card, "boardId": src.ID}) != 1 {
		t.Fatal("negative: a copy leaves the original")
	}
	var lists []List
	cur, _ := w.db.Collection("lists").Find(ctx, bson.M{"boardId": dest.ID, "archived": false})
	_ = cur.All(ctx, &lists)
	var cpl List
	_ = w.db.Collection("lists").FindOne(ctx, bson.M{"_id": cp.ID}).Decode(&cpl)
	for _, l := range lists {
		if l.ID != cp.ID && l.Sort <= cpl.Sort {
			t.Fatalf("position 0 puts the copy first: %v vs %v", cpl.Sort, l.Sort)
		}
	}

	// Negative: no copy or move into a board the caller cannot write.
	hidden := newRESTBoard(t, w, bt, "Hidden")
	if code := call(t, "POST", list+"/copy", a, `{"toBoardId":"`+hidden.ID+`"}`, nil); code != 403 {
		t.Fatalf("negative: copy into a board the caller cannot write: %d", code)
	}
	if code := call(t, "POST", list+"/move", a, `{"toBoardId":"`+hidden.ID+`"}`, nil); code != 403 {
		t.Fatalf("negative: move into a board the caller cannot write: %d", code)
	}
	if code := call(t, "POST", list+"/copy", a, `{"toBoardId":"`+dest.ID+`","toSwimlaneId":"`+hidden.Swimlane+`"}`, nil); code != 400 {
		t.Fatalf("negative: a swimlane of another board: %d", code)
	}

	// A card moved to another board by its PUT, with its comment.
	card := w.srv.URL + "/api/boards/" + src.ID + "/lists/" + src.List + "/cards/" + src.Card
	if code := call(t, "PUT", card, a, `{"newBoardId":"`+dest.ID+`","newSwimlaneId":"`+dest.Swimlane+`","newListId":"`+hidden.List+`"}`, nil); code != 404 {
		t.Fatalf("negative: a list of a third board: %d", code)
	}
	if code := call(t, "PUT", card, a, `{"title":"Moved","newBoardId":"`+dest.ID+`","newSwimlaneId":"`+dest.Swimlane+`","newListId":"`+dest.List+`"}`, nil); code != 200 {
		t.Fatalf("move a card to another board: %d", code)
	}
	var moved bson.M
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": src.Card}).Decode(&moved)
	if moved["boardId"] != dest.ID || moved["listId"] != dest.List || moved["title"] != "Moved" ||
		count("card_comments", bson.M{"cardId": src.Card, "boardId": dest.ID}) != 1 {
		t.Fatalf("moved with its comment: %v", moved)
	}

	// A list moved to another board: into its list of the same title there.
	var l2 struct {
		ID string `json:"_id"`
	}
	call(t, "POST", w.srv.URL+"/api/boards/"+src.ID+"/lists", a, `{"title":"L"}`, &l2)
	call(t, "POST", w.srv.URL+"/api/boards/"+src.ID+"/lists/"+l2.ID+"/cards", a, `{"title":"in L","swimlaneId":"`+src.Swimlane+`"}`, nil)
	if code := call(t, "POST", w.srv.URL+"/api/boards/"+src.ID+"/lists/"+l2.ID+"/move", a, `{"toBoardId":"`+dest.ID+`"}`, nil); code != 200 {
		t.Fatalf("move list: %d", code)
	}
	var inL Card
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"title": "in L"}).Decode(&inL)
	if inL.BoardID != dest.ID || inL.ListID != dest.List {
		t.Fatalf("its cards go into the destination's list of the same title: %+v", inL)
	}

	// Swimlanes: copy on the same board, move to the other one.
	sw := w.srv.URL + "/api/boards/" + src.ID + "/swimlanes/" + src.Swimlane
	var sc struct {
		ID string `json:"_id"`
	}
	if code := call(t, "POST", sw+"/copy", a, `{"title":"Second"}`, &sc); code != 200 || count("swimlanes", bson.M{"_id": sc.ID, "title": "Second", "boardId": src.ID}) != 1 {
		t.Fatalf("copy swimlane: %d", code)
	}
	if code := call(t, "POST", sw+"/move", a, `{"toBoardId":"`+dest.ID+`"}`, nil); code != 200 || count("swimlanes", bson.M{"_id": src.Swimlane, "boardId": dest.ID}) != 1 {
		t.Fatalf("move swimlane: %d", code)
	}
	setMember(t, w, src.ID, Member{UserID: bob.ID, IsActive: true, IsReadOnly: true})
	if code := call(t, "POST", w.srv.URL+"/api/boards/"+src.ID+"/swimlanes/"+sc.ID+"/copy", bt, `{}`, nil); code != 403 {
		t.Fatalf("negative: a read-only member copies nothing: %d", code)
	}
	_ = ann
}
