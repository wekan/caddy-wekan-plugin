package web

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func TestAvatarDisplayURL(t *testing.T) {
	for in, want := range map[string]string{
		"/cdn/storage/avatars/abc":                                 "/cdn/storage/avatars/abc",
		"/wekan/cdn/storage/avatars/abc":                           "/cdn/storage/avatars/abc",
		"https://old-name.example/wekan/cdn/storage/avatars/abc?x": "/cdn/storage/avatars/abc",
		"/cfs/files/avatars/old":                                   "/cfs/files/avatars/old",
		"https://pictures.example/me.png":                          "https://pictures.example/me.png",
		"https://x.example/?u=/cdn/storage/avatars/abc":            "https://x.example/?u=/cdn/storage/avatars/abc",
		"": "",
	} {
		if got := avatarDisplayURL(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestAvatars(t *testing.T) {
	w := newTestWeKan(t)
	dir := t.TempDir()
	w.s.FilesDir = dir
	ann := w.seedUser(t, "ann", "pw")
	a := login(t, w, "ann")
	ctx := context.Background()
	c := w.client(t)
	body(t, w.signIn(t, c, "ann", "pw"))
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 100)...)
	upload := func(name, typ string, data []byte) string {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="avatar"; filename="`+name+`"`)
		h.Set("Content-Type", typ)
		part, _ := mw.CreatePart(h)
		_, _ = part.Write(data)
		_ = mw.Close()
		req, _ := http.NewRequest("POST", w.srv.URL+"/profile/avatar", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		res, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.Header.Get("Location")
	}
	avatarURL := func() string {
		var u User
		_ = w.db.Collection("users").FindOne(ctx, bson.M{"_id": ann.ID}).Decode(&u)
		return u.Profile.AvatarURL
	}
	if loc := upload("../../me photo.png", "image/png", png); loc != "/profile?saved=avatar" {
		t.Fatalf("uploaded: %s", loc)
	}
	url := avatarURL()
	id := strings.TrimPrefix(url, "/cdn/storage/avatars/")
	var doc bson.M
	if !strings.HasPrefix(url, "/cdn/storage/avatars/") || w.db.Collection("avatars").FindOne(ctx, bson.M{"_id": id}).Decode(&doc) != nil {
		t.Fatalf("stored: %s", url)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "avatars", "*"))
	if len(files) != 1 || !strings.HasSuffix(files[0], id+"-original-me_photo") {
		t.Errorf("Meteor-Files' name, only safe characters, in the avatars directory: %v", files)
	}
	get := func(client *http.Client, path string) *http.Response {
		res, err := client.Get(w.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res
	}
	if res := get(c, url); res.StatusCode != 200 || res.Header.Get("Content-Type") != "image/png" ||
		res.Header.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(res.Header.Get("Cache-Control"), "private") {
		t.Errorf("served to a signed-in user: %d %v", res.StatusCode, res.Header)
	}
	anon := w.client(t)
	if res := get(anon, url); res.StatusCode != 401 {
		t.Errorf("negative: signed out: %d", res.StatusCode)
	}
	pub := newRESTBoard(t, w, a, "Public")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": pub.ID}, bson.M{"$set": bson.M{"permission": "public"}})
	priv := newRESTBoard(t, w, a, "Private")
	if res := get(anon, url+"?boardId="+pub.ID); res.StatusCode != 200 {
		t.Errorf("a public board its owner is on: %d", res.StatusCode)
	}
	if res := get(anon, url+"?boardId="+priv.ID); res.StatusCode != 401 {
		t.Errorf("negative: a private board: %d", res.StatusCode)
	}
	// The sidebar of the public board names it with its board.
	if res, html := get2(t, c, w.srv.URL+"/b/"+pub.ID+"/public"); res.StatusCode != 200 || !strings.Contains(html, `class="avatar avatar-image" src="`+url+`?boardId=`+pub.ID) {
		t.Errorf("the sidebar shows the avatar: %s", html[:min(len(html), 200)])
	}
	// Negatives: too big, not an image, a path out of the directory, a guess.
	before := avatarURL()
	if loc := upload("big.png", "image/png", bytes.Repeat([]byte{1}, maxAvatarBytes+1)); !strings.Contains(loc, "error=") || avatarURL() != before {
		t.Errorf("negative: over 72 KB: %s", loc)
	}
	if loc := upload("x.html", "text/html", []byte("<script>")); !strings.Contains(loc, "error=") {
		t.Errorf("negative: not an image: %s", loc)
	}
	_, _ = w.db.Collection("avatars").InsertOne(ctx, bson.M{"_id": "escapeavatar0001", "userId": ann.ID, "type": "image/png",
		"versions": bson.M{"original": bson.M{"path": "../../../../etc/passwd"}}})
	_ = os.WriteFile(filepath.Join(dir, "passwd"), []byte("secret"), 0o600)
	if res := get(c, "/cdn/storage/avatars/escapeavatar0001"); res.StatusCode != 404 {
		t.Errorf("negative: a path out of the avatars directory: %d", res.StatusCode)
	}
	if res := get(anon, "/cdn/storage/avatars/guessedavatar001"); res.StatusCode != 401 {
		t.Errorf("negative: a guessed id, signed out: %d", res.StatusCode)
	}
	if n, _ := w.db.Collection("eventlog").CountDocuments(ctx, bson.M{"bleed": "RelicAvatarBleed"}); n != 1 {
		t.Errorf("the guess is in Problems: %d", n)
	}
	if res := get(c, "/cdn/storage/avatars/guessedavatar001"); res.StatusCode != 404 {
		t.Errorf("signed in, no such avatar: %d", res.StatusCode)
	}
	if loc := upload("pic.svg", "image/svg+xml", []byte(`<svg onload="alert(1)"/>`)); loc != "/profile?saved=avatar" {
		t.Fatalf("an SVG is stored: %s", loc)
	}
	if res := get(c, avatarURL()); res.Header.Get("Content-Disposition") != "attachment" || res.Header.Get("Content-Type") != "application/octet-stream" {
		t.Errorf("negative: an SVG never shows as a page: %v", res.Header)
	}
	_, _ = w.db.Collection("attachmentStorageSettings").InsertOne(ctx, bson.M{"limitSettings": bson.M{"avatarsUploadBlocked": true}})
	if loc := upload("ok.png", "image/png", png); !strings.Contains(loc, "error=") {
		t.Errorf("negative: Admin Panel blocks avatar uploads: %s", loc)
	}
	// /avatar-default/:userId (#824).
	if res := get(anon, "/avatar-default/"+ann.ID); res.StatusCode != 404 {
		t.Errorf("no DEFAULT_AVATAR_URL: %d", res.StatusCode)
	}
	t.Setenv("DEFAULT_AVATAR_URL", "https://avatars.example/default.png")
	if res := get(anon, "/avatar-default/"+ann.ID); res.StatusCode != 302 || res.Header.Get("Location") != "https://avatars.example/default.png" {
		t.Errorf("redirected: %d", res.StatusCode)
	}
	if res := get(anon, "/avatar-default/nosuchuser00001"); res.StatusCode != 404 {
		t.Errorf("negative: unknown user: %d", res.StatusCode)
	}
	t.Setenv("DEFAULT_AVATAR_URL", "javascript:alert(1)")
	if res := get(anon, "/avatar-default/"+ann.ID); res.StatusCode != 404 {
		t.Errorf("negative: never anything but http(s): %d", res.StatusCode)
	}
}

func get2(t *testing.T, c *http.Client, u string) (*http.Response, string) { return get(t, c, u) }

// The minicard's "Mark as complete": shown only when Card Settings says so,
// changed only by whoever may change the card; and isAvatarUploadBlocked.
func TestMinicardCompleteAndAvatarBlock(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	a := login(t, w, "ann")
	b := newRESTBoard(t, w, a, "Done")
	ctx := context.Background()
	c := w.client(t)
	body(t, w.signIn(t, c, "ann", "pw"))
	if _, html := get(t, c, w.srv.URL+"/b/"+b.ID+"/done"); strings.Contains(html, "js-toggle-card-complete") {
		t.Error("negative: hidden by default")
	}
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"allowsDueCompleteOnMinicard": true}})
	if _, html := get(t, c, w.srv.URL+"/b/"+b.ID+"/done"); !strings.Contains(html, "js-toggle-card-complete") {
		t.Error("shown when Card Settings says so")
	}
	post := func(client *http.Client, v string) int {
		res, err := client.PostForm(w.srv.URL+"/b/"+b.ID+"/cards/"+b.Card+"/complete", map[string][]string{"complete": {v}})
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	done := func() bool {
		var card Card
		_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": b.Card}).Decode(&card)
		return card.DueComplete
	}
	if code := post(c, "true"); code != 204 || !done() {
		t.Fatalf("marked complete: %d", code)
	}
	setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: true, IsNormalAssignedOnly: true})
	bc := w.client(t)
	body(t, w.signIn(t, bc, "bob", "pw"))
	if code := post(bc, "false"); code != 403 || !done() {
		t.Errorf("negative: an assigned-only member, not their card: %d", code)
	}
	call := func() any {
		req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/isAvatarUploadBlocked", strings.NewReader(`[]`))
		req.Header.Set("Content-Type", "application/json")
		res, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		return out["result"]
	}
	if call() != false {
		t.Error("avatars are enabled by default")
	}
	_, _ = w.db.Collection("attachmentStorageSettings").InsertOne(ctx, bson.M{"limitSettings": bson.M{"avatarsUploadBlocked": true}})
	if call() != true {
		t.Error("blocked")
	}
}

// An avatar Node.js WeKan keeps in its "avatars" GridFS bucket is served to a
// signed-in user as one on disk; an anonymous one still gets nothing, and a
// GridFS id with nothing behind it is not found.
func TestGridFSAvatar(t *testing.T) {
	w := newTestWeKan(t)
	ann := w.seedUser(t, "ann", "pw")
	ctx := context.Background()
	png := "\x89PNG\r\n\x1a\nfake"
	oid, err := w.db.GridFSBucket(options.GridFSBucket().SetName("avatars")).UploadFromStream(ctx, "a.png", strings.NewReader(png))
	if err != nil {
		t.Fatal(err)
	}
	for id, grid := range map[string]string{"avGrid": oid.Hex(), "avMissing": bson.NewObjectID().Hex()} {
		_, _ = w.db.Collection("avatars").InsertOne(ctx, bson.M{"_id": id, "userId": ann.ID, "type": "image/png", "size": len(png),
			"versions": bson.M{"original": bson.M{"storage": "gridfs", "meta": bson.M{"gridFsFileId": grid}}}})
	}
	cl := w.client(t)
	body(t, w.signIn(t, cl, "ann", "pw"))
	res, err := cl.Get(w.srv.URL + "/cdn/storage/avatars/avGrid-original-a.png")
	if err != nil {
		t.Fatal(err)
	}
	if got := body(t, res); res.StatusCode != 200 || got != png {
		t.Errorf("served from GridFS: %d %q", res.StatusCode, got)
	}
	res, err = testClient.Get(w.srv.URL + "/cdn/storage/avatars/avGrid-original-a.png")
	if err != nil {
		t.Fatal(err)
	}
	if body(t, res); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("negative: an anonymous caller gets nothing: %d", res.StatusCode)
	}
	res, err = cl.Get(w.srv.URL + "/cdn/storage/avatars/avMissing-original-a.png")
	if err != nil {
		t.Fatal(err)
	}
	if body(t, res); res.StatusCode != http.StatusNotFound {
		t.Errorf("negative: a GridFS id with nothing behind it: %d", res.StatusCode)
	}
}
