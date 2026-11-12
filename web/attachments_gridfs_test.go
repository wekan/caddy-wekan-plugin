package web

import (
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// An upload goes to Admin Panel's default storage as WeKan's onAfterUpload
// moves it: with GridFS - enabled and writable - into the "attachments"
// bucket with WeKan's metadata, the disk copy removed, and read back from
// there; with GridFS turned off, or the filesystem the default, it stays on
// the filesystem.
func TestUploadToDefaultStorage(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.s.FilesDir = filepath.Join(t.TempDir(), "files")
	w.seedUser(t, "ann", "pw")
	rb := newRESTBoard(t, w, login(t, w, "ann"), "Files")
	ac := w.client(t)
	body(t, w.signIn(t, ac, "ann", "pw"))
	settings := func(storage string, enabled bool) {
		_, _ = w.db.Collection("attachmentStorageSettings").DeleteMany(ctx, bson.M{})
		_, _ = w.db.Collection("attachmentStorageSettings").InsertOne(ctx, bson.M{"_id": "settings1", "defaultStorage": storage,
			"storageConfig": bson.M{"gridfs": bson.M{"enabled": enabled, "read": true, "write": true}}})
	}
	upload := func(content string) Attachment {
		t.Helper()
		var buf strings.Builder
		mw := multipart.NewWriter(&buf)
		fw, _ := mw.CreateFormFile("file", "notes.txt")
		_, _ = fw.Write([]byte(content))
		_ = mw.Close()
		req, _ := http.NewRequest("POST", w.srv.URL+"/b/"+rb.ID+"/cards/"+rb.Card+"/attachments", strings.NewReader(buf.String()))
		req.Header.Set("Content-Type", mw.FormDataContentType())
		res, err := noRedirect(ac).Do(req)
		if err != nil || res.StatusCode != http.StatusSeeOther {
			t.Fatalf("upload: %v %v", err, res)
		}
		res.Body.Close()
		var a Attachment
		_ = w.db.Collection("attachments").FindOne(ctx, bson.M{"meta.cardId": rb.Card}, options.FindOne().SetSort(bson.D{{Key: "uploadedAt", Value: -1}})).Decode(&a)
		return a
	}
	download := func(a Attachment) string {
		res, err := ac.Get(w.srv.URL + "/cdn/storage/attachments/" + a.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return string(b)
	}
	onDisk := func(a Attachment) bool {
		_, err := os.Stat(filepath.Join(w.s.attachmentsDir(), a.ID))
		return err == nil
	}

	settings("gridfs", true)
	a := upload("in GridFS")
	if a.Versions.Original.Storage != "gridfs" || a.Versions.Original.Meta.GridFsFileID == "" {
		t.Fatalf("moved to GridFS: %+v", a.Versions.Original)
	}
	if onDisk(a) {
		t.Error("the disk copy is removed")
	}
	id, _ := bson.ObjectIDFromHex(a.Versions.Original.Meta.GridFsFileID)
	var file struct {
		Metadata struct {
			FileID      string `bson:"fileId"`
			VersionName string `bson:"versionName"`
			CardID      string `bson:"cardId"`
		} `bson:"metadata"`
	}
	if err := w.db.Collection("attachments.files").FindOne(ctx, bson.M{"_id": id}).Decode(&file); err != nil {
		t.Fatalf("the GridFS file: %v", err)
	}
	if m := file.Metadata; m.FileID != a.ID || m.VersionName != "original" || m.CardID != rb.Card {
		t.Errorf("WeKan's metadata: %+v", m)
	}
	if got := download(a); got != "in GridFS" {
		t.Errorf("read back from GridFS: %q", got)
	}
	settings("gridfs", false)
	if b := upload("gridfs off"); b.Versions.Original.Storage == "gridfs" || !onDisk(b) || download(b) != "gridfs off" {
		t.Errorf("negative: GridFS turned off keeps it on the filesystem: %+v", b.Versions.Original)
	}
	settings("filesystem", true)
	if c := upload("on disk"); c.Versions.Original.Storage == "gridfs" || !onDisk(c) {
		t.Errorf("negative: the filesystem as the default: %+v", c.Versions.Original)
	}
}
