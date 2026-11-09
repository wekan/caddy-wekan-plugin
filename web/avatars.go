package web

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Avatars, as Node.js WeKan's models/avatars.js and server/routes/
// avatarServer.js: an image a user uploads (at most 72 KB, unless Admin Panel
// blocks avatar uploads), stored in files/avatars as Meteor-Files stores it,
// in the avatars collection, and served at /cdn/storage/avatars/<id> to a
// signed-in user - or to anybody, from a public board its owner is on. A path
// from the database is never followed out of the avatars directory.

const maxAvatarBytes = 72000

// avatarPath is WeKan's AVATAR_PATH: an avatar's name within a stored URL.
var avatarPath = regexp.MustCompile(`/(?:cdn/storage|cfs/files)/avatars/[^?#]+`)
var avatarBefore = regexp.MustCompile(`^(?:https?://[^/?#]+)?(?:/[^?#]*)?$`)

// avatarDisplayURL is WeKan's avatarDisplayUrl: a stored avatar URL as a name -
// whatever host or old path prefix stood before it is dropped - and anything
// that is not a WeKan avatar is left as it is.
func avatarDisplayURL(stored string) string {
	loc := avatarPath.FindStringIndex(stored)
	if loc == nil {
		return stored
	}
	before := stored[:loc[0]]
	if strings.ContainsAny(before, "?#") || (before != "" && !avatarBefore.MatchString(before)) {
		return stored
	}
	return stored[loc[0]:loc[1]]
}

func (s *Server) avatarsDir() string { return filepath.Join(s.FilesDir, "avatars") }

func (s *Server) avatarRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /cdn/storage/avatars/{name}", s.serveAvatar)
	m.HandleFunc("GET /cdn/storage/avatars/{name}/original/{file}", s.serveAvatar)
	m.HandleFunc("GET /avatar-default/{user}", s.defaultAvatar)
	m.HandleFunc("POST /profile/avatar", s.page(s.uploadAvatar))
	m.HandleFunc("POST /profile/avatar/remove", s.page(s.removeAvatar))
}

type avatarDoc struct {
	ID       string `bson:"_id"`
	UserID   string `bson:"userId"`
	Type     string `bson:"type"`
	Size     int64  `bson:"size"`
	Versions struct {
		Original struct {
			Path    string `bson:"path"`
			Storage string `bson:"storage"`
			Meta    struct {
				GridFsFileID string `bson:"gridFsFileId"`
			} `bson:"meta"`
		} `bson:"original"`
	} `bson:"versions"`
}

// serveAvatar is WeKan's /cdn/storage/avatars/:fileName.
func (s *Server) serveAvatar(w http.ResponseWriter, r *http.Request) {
	id, _, _ := strings.Cut(r.PathValue("name"), "-original-")
	var a avatarDoc
	if !validID(id) || s.db.Collection("avatars").FindOne(r.Context(), bson.M{"_id": eq(id)}).Decode(&a) != nil {
		// No current avatar: an anonymous caller learns nothing - as WeKan's
		// legacy fallback - and the attempt is recorded.
		if s.currentUser(r) == nil {
			s.recordSecurity(r, securityEvent{Key: "authz.legacy-avatar", Action: "blocked", Source: "/cdn/storage/avatars/:fileName",
				Detail: "refused an unauthenticated legacy-avatar fallback request"})
			http.Error(w, "Authentication required", http.StatusUnauthorized)
			return
		}
		http.Error(w, "Avatar not found", http.StatusNotFound)
		return
	}
	if s.currentUser(r) == nil && !s.avatarOnPublicBoard(r, a.UserID) {
		http.Error(w, "Authentication required", http.StatusUnauthorized)
		return
	}
	// An avatar Node.js WeKan keeps in its "avatars" GridFS bucket.
	var grid *attachmentFileHandle
	if o := a.Versions.Original; o.Storage == "gridfs" || o.Meta.GridFsFileID != "" {
		f, err := s.openGridFS(r.Context(), "avatars", o.Meta.GridFsFileID)
		if err != nil {
			http.Error(w, "Avatar file not found in storage", http.StatusNotFound)
			return
		}
		defer f.Close()
		grid = f
	}
	var path string
	for _, name := range []string{filepath.Base(a.Versions.Original.Path), a.ID} {
		if grid != nil {
			break
		}
		if name == "" || name == "." || name == "/" || name == ".." {
			continue
		}
		p := filepath.Join(s.avatarsDir(), name)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			path = p
			break
		}
	}
	if path == "" && grid == nil {
		http.Error(w, "Avatar file not found in storage", http.StatusNotFound)
		return
	}
	// CacheBleed: private, after the check; and never a page of this site.
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("ETag", `"`+a.ID+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	if r.Header.Get("If-None-Match") == `"`+a.ID+`"` {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	typ := a.Type
	if !inline[typ] || !strings.HasPrefix(typ, "image/") {
		// An SVG or anything else that is not a plain picture is downloaded.
		w.Header().Set("Content-Disposition", "attachment")
		typ = "application/octet-stream"
	}
	w.Header().Set("Content-Type", typ)
	if grid != nil {
		http.ServeContent(w, r, "", grid.modTime, grid)
		return
	}
	http.ServeFile(w, r, path)
}

// avatarOnPublicBoard is WeKan's avatarIsOnAPublicBoard: ?boardId= names a
// public board, and the avatar's owner is on it.
func (s *Server) avatarOnPublicBoard(r *http.Request, owner string) bool {
	id := r.URL.Query().Get("boardId")
	var b Board
	if owner == "" || !validID(id) || s.db.Collection("boards").FindOne(r.Context(), bson.M{"_id": eq(id)}).Decode(&b) != nil {
		return false
	}
	return b.Permission == "public" && b.MemberOf(owner)
}

// defaultAvatar is WeKan's /avatar-default/:userId (#824): DEFAULT_AVATAR_URL,
// when it is an http(s) address and the user is there; else 404.
func (s *Server) defaultAvatar(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("user")
	target := os.Getenv("DEFAULT_AVATAR_URL")
	u, err := url.Parse(target)
	if !validID(id) || target == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		http.NotFound(w, r)
		return
	}
	if n, _ := s.db.Collection("users").CountDocuments(r.Context(), bson.M{"_id": eq(id)}); n == 0 {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// avatarUploadBlocked is Admin Panel's attachmentStorageSettings
// limitSettings.avatarsUploadBlocked.
func (s *Server) avatarUploadBlocked(r *http.Request) bool {
	var set bson.M
	if s.db.Collection("attachmentStorageSettings").FindOne(r.Context(), bson.M{}).Decode(&set) != nil {
		return false
	}
	return nestedField(set, "limitSettings", "avatarsUploadBlocked") == true
}

// uploadAvatar takes the user's own new avatar from the profile page.
func (s *Server) uploadAvatar(w http.ResponseWriter, r *http.Request, u *User) {
	fail := func(key string) { http.Redirect(w, r, "/profile?error="+url.QueryEscape(key), http.StatusSeeOther) }
	if s.avatarUploadBlocked(r) {
		fail(msg("avatars-upload-blocked-description"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAvatarBytes+64<<10)
	f, hdr, err := r.FormFile("avatar")
	if err != nil {
		fail(msg("avatar-too-big"))
		return
	}
	defer f.Close()
	typ, _, _ := mime.ParseMediaType(hdr.Header.Get("Content-Type"))
	if !strings.HasPrefix(typ, "image/") {
		fail(msg("avatar-too-big"))
		return
	}
	a, err := s.saveAvatar(r, u, cleanFileName(hdr.Filename), typ, f)
	if err != nil {
		fail(msg("avatar-too-big"))
		return
	}
	_, _ = s.db.Collection("users").UpdateOne(r.Context(), bson.M{"_id": eq(u.ID)}, bson.M{"$set": bson.M{"profile.avatarUrl": "/cdn/storage/avatars/" + a}})
	http.Redirect(w, r, "/profile?saved=avatar", http.StatusSeeOther)
}

// removeAvatar goes back to the initials.
func (s *Server) removeAvatar(w http.ResponseWriter, r *http.Request, u *User) {
	_, _ = s.db.Collection("users").UpdateOne(r.Context(), bson.M{"_id": eq(u.ID)}, bson.M{"$unset": bson.M{"profile.avatarUrl": ""}})
	http.Redirect(w, r, "/profile?saved=avatar", http.StatusSeeOther)
}

var safeAvatarName = regexp.MustCompile(`[^a-zA-Z0-9_.\-]`)

// saveAvatar stores the image as Meteor-Files does - <id>-original-<name>,
// the name with only safe characters - and is its id.
func (s *Server) saveAvatar(r *http.Request, u *User, name, typ string, body io.Reader) (string, error) {
	dir := s.avatarsDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(body, maxAvatarBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxAvatarBytes {
		return "", fmt.Errorf("an avatar is at most %d bytes", maxAvatarBytes)
	}
	if name == "" {
		name = "image"
	}
	id := randomID()
	ext := strings.TrimPrefix(filepath.Ext(name), ".")
	base := safeAvatarName.ReplaceAllString(strings.TrimSuffix(name, filepath.Ext(name)), "_")
	file := id + "-original-" + base
	path := filepath.Join(dir, file)
	if filepath.Dir(path) != dir {
		return "", errors.New("no such avatar name")
	}
	if err := os.WriteFile(path, data, 0o640); err != nil {
		return "", err
	}
	doc := bson.M{"_id": id, "name": name, "extension": ext, "ext": ext, "extensionWithDot": "." + ext, // inserted, not a filter
		"type": typ, "mime": typ, "mime-type": typ, "size": int64(len(data)), "userId": u.ID, "isImage": true,
		"path": path, "_storagePath": dir, "_collectionName": "avatars", "_downloadRoute": "/cdn/storage", "public": false,
		"versions": bson.M{"original": bson.M{"path": path, "size": int64(len(data)), "type": typ, "extension": ext, "storage": "fs"}},
		"meta":     bson.M{}, "uploadedAt": time.Now().UTC()}
	if _, err := s.db.Collection("avatars").InsertOne(r.Context(), doc); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return id, nil
}
