package web

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Admin Panel / Attachments over REST: a site administrator's; WeKan's
// defaults made on the first read; cloud secrets never returned, a blank one
// keeping the stored; only the keys and types WeKan's schema names.
func TestAdminAttachmentSettings(t *testing.T) {
	t.Setenv("S3_SECRET_KEY", "from-env-secret")
	w := newTestWeKan(t)
	admin := w.seedUser(t, "root", "pw")
	w.seedUser(t, "bob", "pw")
	ctx := context.Background()
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": admin.ID}, bson.M{"$set": bson.M{"isAdmin": true}})
	at, bt := login(t, w, "root"), login(t, w, "bob")
	url := w.srv.URL + "/api/admin/attachment-settings"
	var doc map[string]any
	if code := call(t, "GET", url, at, "", &doc); code != 200 {
		t.Fatalf("GET: %d", code)
	}
	s3 := doc["storageConfig"].(map[string]any)["s3"].(map[string]any)
	if s3["secretAccessKey"] != "" || s3["secretAccessKeySet"] != true || doc["defaultStorage"] != "filesystem" {
		t.Errorf("the defaults, the secret masked: %v", s3)
	}
	if code := call(t, "PUT", url, at, `{"limitSettings":{"avatarsUploadBlocked":true},"storageConfig":{"s3":{"bucket":"b1","secretAccessKey":"new-secret"}}}`, &doc); code != 200 {
		t.Fatalf("PUT: %d", code)
	}
	var stored bson.M
	_ = w.db.Collection("attachmentStorageSettings").FindOne(ctx, bson.M{}).Decode(&stored)
	sd, _ := plain(stored).(map[string]any)
	ss3 := sd["storageConfig"].(map[string]any)["s3"].(map[string]any)
	if ss3["secretAccessKey"] != "new-secret" || ss3["bucket"] != "b1" || sd["limitSettings"].(map[string]any)["avatarsUploadBlocked"] != true {
		t.Errorf("stored: %v %v", ss3, sd["limitSettings"])
	}
	if s3 := doc["storageConfig"].(map[string]any)["s3"].(map[string]any); s3["secretAccessKey"] != "" {
		t.Error("negative: the answer never carries the secret")
	}
	// A blank secret keeps the stored one.
	call(t, "PUT", url, at, `{"storageConfig":{"s3":{"secretAccessKey":"","region":"eu"}}}`, nil)
	_ = w.db.Collection("attachmentStorageSettings").FindOne(ctx, bson.M{}).Decode(&stored)
	sd, _ = plain(stored).(map[string]any)
	if ss3 := sd["storageConfig"].(map[string]any)["s3"].(map[string]any); ss3["secretAccessKey"] != "new-secret" || ss3["region"] != "eu" {
		t.Errorf("a blank secret keeps the stored one: %v", ss3)
	}
	for _, bad := range []string{`{"$where":"1"}`, `{"limitSettings":{"avatarsUploadBlocked":"yes"}}`, `{"defaultStorage":"ftp"}`,
		`{"storageConfig":{"dropbox":{}}}`, `{"limitSettings":{"apiUploadMaxBytes":-1}}`, `{"storageConfig":{"s3":{"a.b":"x"}}}`} {
		if code := call(t, "PUT", url, at, bad, nil); code != 400 {
			t.Errorf("negative: %s answers %d", bad, code)
		}
	}
	if code := call(t, "GET", url, bt, "", nil); code != 403 {
		t.Errorf("negative: a member reads nothing: %d", code)
	}
	if code := call(t, "PUT", url, bt, `{"limitSettings":{"avatarsUploadBlocked":false}}`, nil); code != 403 {
		t.Errorf("negative: a member changes nothing: %d", code)
	}
}

// WeKan's GET /api/admin/domains: users by their primary address's domain,
// most first, then by name; a site administrator's only.
func TestAdminDomains(t *testing.T) {
	w := newTestWeKan(t)
	admin := w.seedUser(t, "root", "pw")
	ctx := context.Background()
	_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": admin.ID}, bson.M{"$set": bson.M{"isAdmin": true, "emails": bson.A{bson.M{"address": "root@B.example"}}}})
	for i, addr := range []string{"a@a.example", "b@b.example", "c@A.example", "no-at-sign", "d@a.example"} {
		u := w.seedUser(t, "u"+string(rune('0'+i)), "pw")
		_, _ = w.db.Collection("users").UpdateOne(ctx, bson.M{"_id": u.ID}, bson.M{"$set": bson.M{"emails": bson.A{bson.M{"address": addr}, bson.M{"address": "second@z.example"}}}})
	}
	w.seedUser(t, "member", "pw")
	at, mt := login(t, w, "root"), login(t, w, "member")
	var rows []struct {
		Domain string `json:"domain"`
		Count  int    `json:"count"`
	}
	if code := call(t, "GET", w.srv.URL+"/api/admin/domains", at, "", &rows); code != 200 {
		t.Fatalf("GET: %d", code)
	}
	got := ""
	for _, r := range rows {
		got += r.Domain + ":" + itoa(r.Count) + " "
	}
	if got != "a.example:3 b.example:2 example.invalid:1 " { // the member, seeded with an example.invalid address
		t.Errorf("by primary domain, most first, lower-cased, never the second address: %q", got)
	}
	if code := call(t, "GET", w.srv.URL+"/api/admin/domains", mt, "", nil); code != 403 {
		t.Errorf("negative: a member reads nothing: %d", code)
	}
}
