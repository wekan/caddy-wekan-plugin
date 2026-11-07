package web

// Attachments, as Node.js WeKan keeps them, so either reads the other's: the
// file is <files>/attachments/<id>, and the attachments collection has
// Meteor-Files' document - name, type, size, versions.original, meta with the
// board, swimlane, list and card. A deleted attachment is kept with deletedAt, as
// WeKan's soft delete does.

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// MaxAttachmentBytes is the largest attachment accepted: 100 MiB.
const MaxAttachmentBytes = 100 << 20

// Attachment is a document of the attachments collection - the fields used here.
type Attachment struct {
	ID       string `bson:"_id" json:"_id"`
	Name     string `bson:"name" json:"name"`
	Type     string `bson:"type" json:"type"`
	Size     int64  `bson:"size" json:"size"`
	IsImage  bool   `bson:"isImage" json:"isImage"`
	UserID   string `bson:"userId" json:"userId"`
	Versions struct {
		Original struct {
			Path string `bson:"path"`
		} `bson:"original"`
	} `bson:"versions" json:"-"`
	Meta struct {
		BoardID    string `bson:"boardId" json:"boardId"`
		SwimlaneID string `bson:"swimlaneId" json:"swimlaneId"`
		ListID     string `bson:"listId" json:"listId"`
		CardID     string `bson:"cardId" json:"cardId"`
	} `bson:"meta" json:"meta"`
	UploadedAt time.Time  `bson:"uploadedAt" json:"uploadedAt"`
	DeletedAt  *time.Time `bson:"deletedAt,omitempty" json:"-"`
}

func (s *Server) attachmentsDir() string { return filepath.Join(s.FilesDir, "attachments") }

// cleanFileName keeps a file name a name: its last path element, no control
// characters, at most 255 bytes.
func cleanFileName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || name == "/" {
		name = "file"
	}
	for len(name) > 255 { // whole characters only: never half of a UTF-8 sequence
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	return name
}

// StoreAttachment writes an attachment of a card from r (at most
// MaxAttachmentBytes) and records it.
func (s *Server) StoreAttachment(r *http.Request, b *Board, c *Card, u *User, name, typ string, body io.Reader) (*Attachment, error) {
	if s.FilesDir == "" {
		return nil, errors.New("attachments are not kept here")
	}
	dir := s.attachmentsDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	name = cleanFileName(name)
	if mt, _, err := mime.ParseMediaType(typ); err == nil {
		typ = mt
	} else {
		typ = "application/octet-stream"
	}
	id := randomID()
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, io.LimitReader(body, MaxAttachmentBytes+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	if n > MaxAttachmentBytes {
		return nil, fmt.Errorf("an attachment is at most %d MiB", MaxAttachmentBytes>>20)
	}
	path := filepath.Join(dir, id)
	if err := os.Rename(tmp.Name(), path); err != nil {
		return nil, err
	}
	ext := strings.TrimPrefix(filepath.Ext(name), ".")
	now := time.Now().UTC()
	isImage := strings.HasPrefix(typ, "image/")
	doc := bson.M{
		"_id": id, "name": name, "extension": ext, "ext": ext, "extensionWithDot": "." + ext,
		"type": typ, "mime": typ, "mime-type": typ, "size": n, "userId": u.ID,
		"isImage": isImage, "isVideo": strings.HasPrefix(typ, "video/"), "isAudio": strings.HasPrefix(typ, "audio/"),
		"isText": strings.HasPrefix(typ, "text/"), "isJSON": typ == "application/json", "isPDF": typ == "application/pdf",
		"path": path, "_storagePath": dir, "_collectionName": "attachments", "_downloadRoute": "/cdn/storage", "public": false,
		"versions": bson.M{"original": bson.M{"path": path, "size": n, "type": typ, "extension": ext, "storage": "fs"}},
		"meta": bson.M{"boardId": b.ID, "swimlaneId": c.SwimlaneID, "listId": c.ListID, "cardId": c.ID,
			"fileId": id, "source": "weddy"},
		"uploadedAt": now,
	}
	if _, err := s.db.Collection("attachments").InsertOne(r.Context(), doc); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	s.activity(r.Context(), u.ID, "addAttachment", bson.M{"attachmentId": id, "attachmentName": name, "boardId": b.ID,
		"cardId": c.ID, "listId": c.ListID, "swimlaneId": c.SwimlaneID})
	s.hub.publish(b.ID, "board-changed")
	return s.attachment(r, id)
}

func (s *Server) attachment(r *http.Request, id string) (*Attachment, error) {
	var a Attachment
	if !validID(id) {
		return nil, errors.New("no such attachment")
	}
	if err := s.db.Collection("attachments").FindOne(r.Context(), bson.M{"_id": eq(id)}).Decode(&a); err != nil {
		return nil, err
	}
	if a.DeletedAt != nil {
		return nil, errors.New("deleted")
	}
	return &a, nil
}

// attachmentFile is where the attachment's bytes are: in the attachments
// directory, whatever path the document names (a path from the database is
// never followed out of it - AvatarBleed's lesson).
func (s *Server) attachmentFile(a *Attachment) (string, error) {
	dir := s.attachmentsDir()
	for _, name := range []string{a.ID, filepath.Base(a.Versions.Original.Path)} {
		if name == "" || name == "." || name == "/" || name == ".." {
			continue
		}
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return p, nil
		}
	}
	return "", os.ErrNotExist
}

// cardAttachments are a card's attachments, oldest first.
func (s *Server) cardAttachments(r *http.Request, b *Board, cardID string) []Attachment {
	cur, err := s.db.Collection("attachments").Find(r.Context(), bson.M{"meta.boardId": b.ID, "meta.cardId": cardID, "deletedAt": nil},
		options.Find().SetSort(bson.D{{Key: "uploadedAt", Value: 1}}))
	if err != nil {
		return nil
	}
	var out []Attachment
	_ = cur.All(r.Context(), &out)
	return out
}

// inline types are shown in the browser; everything else - HTML, SVG, scripts -
// is downloaded, so a file can never run as a page of this site.
var inline = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true,
	"image/avif": true, "image/bmp": true, "application/pdf": true, "text/plain": true, "video/mp4": true,
	"video/webm": true, "audio/mpeg": true, "audio/ogg": true, "audio/wav": true}

// serveAttachment answers /cdn/storage/attachments/<id>[/original/<name>] to a
// reader of the attachment's board.
func (s *Server) serveAttachment(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	if u == nil {
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}
	a, err := s.attachment(r, r.PathValue("id"))
	if err != nil {
		http.Error(w, "Attachment not found", http.StatusNotFound)
		return
	}
	if _, err := s.boardFor(r.Context(), a.Meta.BoardID, u); err != nil {
		http.Error(w, "Attachment not found", http.StatusNotFound)
		return
	}
	path, err := s.attachmentFile(a)
	if err != nil {
		http.Error(w, "Attachment not found", http.StatusNotFound)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "Attachment not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	disposition := "attachment"
	typ := a.Type
	if inline[typ] {
		disposition = "inline"
	} else {
		typ = "application/octet-stream"
	}
	h := w.Header()
	h.Set("Content-Type", typ)
	h.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": a.Name}))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("Cache-Control", "private, no-cache")
	http.ServeContent(w, r, "", st.ModTime(), f)
}

// boardCard is the board and the card of a request's ids, writable by u.
func (s *Server) boardCard(r *http.Request, u *User, boardID, cardID string) (*Board, *Card, int) {
	b, err := s.boardFor(r.Context(), boardID, u)
	if err != nil {
		return nil, nil, http.StatusNotFound
	}
	if !b.MemberOf(u.ID) {
		return nil, nil, http.StatusForbidden
	}
	var c Card
	if !validID(cardID) || s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": eq(cardID), "boardId": b.ID}).Decode(&c) != nil {
		return nil, nil, http.StatusNotFound
	}
	return b, &c, 0
}

// uploadAttachment is the card details' upload form.
func (s *Server) uploadAttachment(w http.ResponseWriter, r *http.Request, u *User) {
	b, c, code := s.boardCard(r, u, r.PathValue("board"), r.PathValue("card"))
	if code != 0 {
		http.Error(w, http.StatusText(code), code)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxAttachmentBytes+1<<20)
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "a file is needed", http.StatusBadRequest)
		return
	}
	defer file.Close()
	if _, err := s.StoreAttachment(r, b, c, u, header.Filename, header.Header.Get("Content-Type"), file); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/b/"+b.ID+"/"+b.Slug+"/"+c.ID, http.StatusSeeOther)
}

func (s *Server) deleteAttachmentPage(w http.ResponseWriter, r *http.Request, u *User) {
	a, err := s.attachment(r, r.PathValue("id"))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	b, c, code := s.boardCard(r, u, a.Meta.BoardID, a.Meta.CardID)
	if code != 0 {
		http.Error(w, http.StatusText(code), code)
		return
	}
	if err := s.softDeleteAttachment(r, a, u); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.hub.publish(b.ID, "board-changed")
	http.Redirect(w, r, "/b/"+b.ID+"/"+b.Slug+"/"+c.ID, http.StatusSeeOther)
}

func (s *Server) softDeleteAttachment(r *http.Request, a *Attachment, u *User) error {
	_, err := s.db.Collection("attachments").UpdateOne(r.Context(), bson.M{"_id": a.ID},
		bson.M{"$set": bson.M{"deletedAt": time.Now().UTC(), "deletedBy": u.ID}})
	return err
}

func (s *Server) attachmentRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /cdn/storage/attachments/{id}", s.serveAttachment)
	m.HandleFunc("GET /cdn/storage/attachments/{id}/original/{name}", s.serveAttachment)
	m.HandleFunc("POST /b/{board}/cards/{card}/attachments", s.page(s.uploadAttachment))
	m.HandleFunc("POST /attachments/{id}/delete", s.page(s.deleteAttachmentPage))
	m.HandleFunc("POST /api/attachment/upload", s.api(s.apiUploadAttachment))
	m.HandleFunc("GET /api/attachment/download/{id}", s.api(s.apiDownloadAttachment))
	m.HandleFunc("GET /api/attachment/info/{id}", s.api(s.apiAttachmentInfo))
	m.HandleFunc("DELETE /api/attachment/delete/{id}", s.api(s.apiDeleteAttachment))
	m.HandleFunc("GET /api/attachment/list/{board}", s.api(s.apiListAttachments))
	m.HandleFunc("GET /api/attachment/list/{board}/{swimlane}", s.api(s.apiListAttachments))
	m.HandleFunc("GET /api/attachment/list/{board}/{swimlane}/{list}", s.api(s.apiListAttachments))
	m.HandleFunc("GET /api/attachment/list/{board}/{swimlane}/{list}/{card}", s.api(s.apiListAttachments))
}

// apiUploadAttachment takes WeKan's JSON body with the file in base64, or - with
// fileName and the ids in the query - the file itself as the body.
func (s *Server) apiUploadAttachment(w http.ResponseWriter, r *http.Request, u *User) {
	q := r.URL.Query()
	var boardID, cardID, name, typ string
	var body io.Reader
	if q.Get("fileName") != "" {
		boardID, cardID, name, typ = q.Get("boardId"), q.Get("cardId"), q.Get("fileName"), q.Get("fileType")
		if typ == "" {
			typ = r.Header.Get("Content-Type")
		}
		body = http.MaxBytesReader(w, r.Body, MaxAttachmentBytes+1)
	} else {
		r.Body = http.MaxBytesReader(w, r.Body, MaxAttachmentBytes*4/3+1<<20)
		var req struct {
			BoardID  string `json:"boardId"`
			CardID   string `json:"cardId"`
			FileData string `json:"fileData"`
			FileName string `json:"fileName"`
			FileType string `json:"fileType"`
		}
		if err := decodeJSON(r, &req); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		data, err := base64.StdEncoding.DecodeString(req.FileData)
		if err != nil || req.FileName == "" {
			apiError(w, http.StatusBadRequest, "fileName and fileData in base64 are needed")
			return
		}
		boardID, cardID, name, typ = req.BoardID, req.CardID, req.FileName, req.FileType
		body = strings.NewReader(string(data))
	}
	b, c, code := s.boardCard(r, u, boardID, cardID)
	if code != 0 {
		apiError(w, code, http.StatusText(code))
		return
	}
	a, err := s.StoreAttachment(r, b, c, u, name, typ, body)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "attachmentId": a.ID, "fileName": a.Name,
		"fileSize": a.Size, "storageBackend": "fs", "message": "Attachment uploaded successfully"})
}

// readableAttachment is the path's attachment, if u may read its board.
func (s *Server) readableAttachment(w http.ResponseWriter, r *http.Request, u *User) (*Attachment, *Board) {
	a, err := s.attachment(r, r.PathValue("id"))
	if err != nil {
		apiError(w, http.StatusNotFound, "Attachment not found")
		return nil, nil
	}
	b, err := s.boardFor(r.Context(), a.Meta.BoardID, u)
	if err != nil {
		apiError(w, http.StatusForbidden, "Forbidden")
		return nil, nil
	}
	return a, b
}

func attachmentJSON(a *Attachment) map[string]any {
	return map[string]any{"attachmentId": a.ID, "fileName": a.Name, "fileSize": a.Size, "fileType": a.Type,
		"storageBackend": "fs", "boardId": a.Meta.BoardID, "swimlaneId": a.Meta.SwimlaneID, "listId": a.Meta.ListID,
		"cardId": a.Meta.CardID, "createdAt": a.UploadedAt, "isImage": a.IsImage}
}

func (s *Server) apiDownloadAttachment(w http.ResponseWriter, r *http.Request, u *User) {
	a, _ := s.readableAttachment(w, r, u)
	if a == nil {
		return
	}
	path, err := s.attachmentFile(a)
	if err != nil {
		apiError(w, http.StatusNotFound, "Attachment file not found")
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := attachmentJSON(a)
	out["success"], out["base64Data"] = true, base64.StdEncoding.EncodeToString(data)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiAttachmentInfo(w http.ResponseWriter, r *http.Request, u *User) {
	if a, _ := s.readableAttachment(w, r, u); a != nil {
		out := attachmentJSON(a)
		out["success"] = true
		writeJSON(w, http.StatusOK, out)
	}
}

func (s *Server) apiDeleteAttachment(w http.ResponseWriter, r *http.Request, u *User) {
	a, b := s.readableAttachment(w, r, u)
	if a == nil {
		return
	}
	if !b.MemberOf(u.ID) {
		apiError(w, http.StatusForbidden, "Forbidden")
		return
	}
	if err := s.softDeleteAttachment(r, a, u); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.hub.publish(b.ID, "board-changed")
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "attachmentId": a.ID, "message": "Attachment deleted successfully"})
}

func (s *Server) apiListAttachments(w http.ResponseWriter, r *http.Request, u *User) {
	b := s.apiBoardFor(w, r, u, false)
	if b == nil {
		return
	}
	filter := bson.M{"meta.boardId": b.ID, "deletedAt": nil}
	for field, key := range map[string]string{"swimlane": "meta.swimlaneId", "list": "meta.listId", "card": "meta.cardId"} {
		if v := r.PathValue(field); v != "" && v != "null" {
			if !validID(v) {
				apiError(w, http.StatusNotFound, "not found")
				return
			}
			filter[key] = eq(v)
		}
	}
	cur, err := s.db.Collection("attachments").Find(r.Context(), filter, options.Find().SetSort(bson.D{{Key: "uploadedAt", Value: 1}}))
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var list []Attachment
	_ = cur.All(r.Context(), &list)
	out := make([]map[string]any, 0, len(list))
	for i := range list {
		out = append(out, attachmentJSON(&list[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "attachments": out, "count": len(out)})
}
