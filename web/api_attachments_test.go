package web

import (
	"context"
	"encoding/base64"
	"path/filepath"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestRESTBoardAttachments(t *testing.T) {
	w := newTestWeKan(t)
	w.s.FilesDir = filepath.Join(t.TempDir(), "files")
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	a, bt := login(t, w, "ann"), login(t, w, "bob")
	b := newRESTBoard(t, w, a, "Files")
	ctx := context.Background()
	data := base64.StdEncoding.EncodeToString([]byte("hello"))
	var up struct {
		AttachmentID string `json:"attachmentId"`
	}
	call(t, "POST", w.srv.URL+"/api/attachment/upload", a, `{"boardId":"`+b.ID+`","cardId":"`+b.Card+`","fileName":"a.txt","fileType":"text/plain","fileData":"`+data+`"}`, &up)
	if up.AttachmentID == "" {
		t.Fatal("upload")
	}
	_, _ = w.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": b.Card}, bson.M{"$set": bson.M{"coverId": up.AttachmentID}})
	base := w.srv.URL + "/api/boards/" + b.ID + "/attachments"

	var list []map[string]any
	if code := call(t, "GET", base, a, "", &list); code != 200 || len(list) != 1 || list[0]["attachmentId"] != up.AttachmentID || list[0]["cardId"] != b.Card {
		t.Fatalf("the board's attachments: %d %v", code, list)
	}
	var exp map[string]any
	if code := call(t, "GET", base+"/"+up.AttachmentID+"/export?authToken="+a, "", "", &exp); code != 200 || exp["file"] != data || exp["name"] != "a.txt" {
		t.Fatalf("one exported, with its file: %d %v", code, exp)
	}
	if code := call(t, "GET", base+"/"+up.AttachmentID+"/export", bt, "", nil); code != 403 {
		t.Fatalf("negative: a non-member exports nothing: %d", code)
	}

	// Soft delete: a read-only member may not; the board's admin may, and the
	// card loses the cover; the file and the document stay, and it is restored.
	setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: true, IsReadOnly: true})
	if code := call(t, "DELETE", base+"/"+up.AttachmentID, bt, "", nil); code != 403 {
		t.Fatalf("negative: a read-only member: %d", code)
	}
	var del map[string]any
	if code := call(t, "DELETE", base+"/"+up.AttachmentID, a, "", &del); code != 200 || del["deleted"] != true || del["batchId"] == "" {
		t.Fatalf("soft delete: %d %v", code, del)
	}
	var card bson.M
	_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": b.Card}).Decode(&card)
	if _, has := card["coverId"]; has {
		t.Fatal("the card's cover goes with it")
	}
	call(t, "GET", base, a, "", &list)
	var deleted []map[string]any
	call(t, "GET", base+"/deleted", a, "", &deleted)
	if len(list) != 0 || len(deleted) != 1 || deleted[0]["deleteBatchId"] != del["batchId"] {
		t.Fatalf("listed as deleted: %v %v", list, deleted)
	}
	var again map[string]any
	call(t, "DELETE", base+"/"+up.AttachmentID, a, "", &again)
	if again["batchId"] != del["batchId"] {
		t.Fatal("deleting again answers the batch it was deleted in")
	}
	var res map[string]any
	if code := call(t, "POST", base+"/"+up.AttachmentID+"/restore", a, "", &res); code != 200 || res["restored"] != true {
		t.Fatalf("restore: %d %v", code, res)
	}
	call(t, "POST", base+"/"+up.AttachmentID+"/restore", a, "", &res)
	if res["restored"] != false {
		t.Fatal("a live attachment is not restored again")
	}
	if code := call(t, "DELETE", w.srv.URL+"/api/boards/"+newRESTBoard(t, w, a, "Other").ID+"/attachments/"+up.AttachmentID, a, "", nil); code != 404 {
		t.Fatalf("negative: an attachment of another board: %d", code)
	}
}
