package web

// Attachments, as Node.js WeKan keeps them, so either reads the other's: the
// file is <files>/attachments/<id>, and the attachments collection has
// Meteor-Files' document - name, type, size, versions.original, meta with the
// board, swimlane, list and card. A deleted attachment is kept with deletedAt, as
// WeKan's soft delete does.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
			Path    string `bson:"path"`
			Storage string `bson:"storage"`
			Meta    struct {
				GridFsFileID string `bson:"gridFsFileId"`
			} `bson:"meta"`
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
	s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
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
	a := s.downloadableAttachment(w, r)
	if a == nil {
		return
	}
	f, err := s.openAttachment(r.Context(), a)
	if err != nil {
		http.Error(w, "Attachment not found", http.StatusNotFound)
		return
	}
	defer f.Close()
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
	http.ServeContent(w, r, "", f.modTime, f)
}

// boardCard is the board and the card of a request's ids, writable by u.
func (s *Server) boardCard(r *http.Request, u *User, boardID, cardID string) (*Board, *Card, int) {
	b, err := s.boardFor(r.Context(), boardID, u)
	if err != nil {
		return nil, nil, http.StatusNotFound
	}
	var c Card
	if !validID(cardID) || s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": eq(cardID), "boardId": b.ID}).Decode(&c) != nil {
		return nil, nil, http.StatusNotFound
	}
	if !b.WritableIn(r.Context(), s, u, "write", &c) {
		return nil, nil, http.StatusForbidden
	}
	return b, &c, 0
}

// uploadAttachment is the card details' upload form.
func (s *Server) uploadAttachment(w http.ResponseWriter, r *http.Request, u *User) {
	b, c, code := s.boardCard(r, u, r.PathValue("board"), r.PathValue("card"))
	if code != 0 {
		if code == http.StatusForbidden {
			s.noteAssignedAttachmentWrite(r, u, r.PathValue("board"), r.PathValue("card"), "attachment-method:upload")
		}
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
		if code == http.StatusForbidden {
			s.noteAssignedAttachmentWrite(r, u, a.Meta.BoardID, a.Meta.CardID, "attachment-method:delete")
		}
		http.Error(w, http.StatusText(code), code)
		return
	}
	if err := s.softDeleteAttachment(r, a, u); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	http.Redirect(w, r, "/b/"+b.ID+"/"+b.Slug+"/"+c.ID, http.StatusSeeOther)
}

// softDeleteAttachment is WeKan's attachments.softDelete: the attachment is
// kept, marked deleted with who and a batch id, the card's cover unset when it
// was the cover, and a deleteAttachment activity recorded. The answer is the
// batch id.
func (s *Server) softDeleteAttachment(r *http.Request, a *Attachment, u *User) error {
	_, err := s.softDeleteAttachmentBatch(r, a, u)
	return err
}

func (s *Server) softDeleteAttachmentBatch(r *http.Request, a *Attachment, u *User) (string, error) {
	now := time.Now().UTC()
	batch := "attachment-" + a.ID + "-" + strconv.FormatInt(now.UnixMilli(), 10)
	ctx := r.Context()
	if _, err := s.db.Collection("attachments").UpdateOne(ctx, bson.M{"_id": a.ID},
		bson.M{"$set": bson.M{"deletedAt": now, "deletedBy": u.ID, "deleteBatchId": batch}}); err != nil {
		return "", err
	}
	if a.Meta.CardID != "" {
		_, _ = s.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": eq(a.Meta.CardID), "coverId": a.ID}, bson.M{"$unset": bson.M{"coverId": ""}})
		s.activity(ctx, u.ID, "deleteAttachment", bson.M{"type": "card", "attachmentId": a.ID, "attachmentName": a.Name,
			"boardId": a.Meta.BoardID, "cardId": a.Meta.CardID, "listId": a.Meta.ListID, "swimlaneId": a.Meta.SwimlaneID})
	}
	return batch, nil
}

// restoreAttachment is WeKan's attachments.restore: the deletion marks go.
func (s *Server) restoreAttachment(r *http.Request, a *Attachment, u *User) error {
	ctx := r.Context()
	if _, err := s.db.Collection("attachments").UpdateOne(ctx, bson.M{"_id": a.ID},
		bson.M{"$unset": bson.M{"deletedAt": "", "deletedBy": "", "deleteBatchId": ""}}); err != nil {
		return err
	}
	if a.Meta.CardID != "" {
		s.activity(ctx, u.ID, "addAttachment", bson.M{"type": "card", "attachmentId": a.ID, "attachmentName": a.Name,
			"boardId": a.Meta.BoardID, "cardId": a.Meta.CardID, "listId": a.Meta.ListID, "swimlaneId": a.Meta.SwimlaneID})
	}
	return nil
}

func (s *Server) attachmentRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /cdn/storage/attachments/{id}", s.serveAttachment)
	m.HandleFunc("GET /cdn/storage/attachments/{id}/original/{name}", s.serveAttachment)
	m.HandleFunc("GET /cdn/storage/attachments/{id}/thumbnail", s.serveAttachmentThumbnail)
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
		if code == http.StatusForbidden {
			s.noteAssignedAttachmentWrite(r, u, boardID, cardID, "attachment-api:upload")
		}
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
	data, err := s.readAttachmentBytes(r.Context(), a)
	if err != nil {
		apiError(w, http.StatusNotFound, "Attachment file not found")
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
	var c Card
	_ = s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": a.Meta.CardID, "boardId": b.ID}).Decode(&c)
	if !b.WritableIn(r.Context(), s, u, "write", &c) {
		s.noteAssignedAttachmentWrite(r, u, b.ID, a.Meta.CardID, "attachment-api:delete")
		apiError(w, http.StatusForbidden, "Forbidden")
		return
	}
	if err := s.softDeleteAttachment(r, a, u); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
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

// copyCardAttachments is WeKan's Cards.copy for attachments: the source
// card's live attachments copied onto the copy as new files, each recording
// where it came from (meta.copyFrom), and the copy's cover pointed at the copy
// of the source's cover (#5364) - or none, when that one could not be copied,
// so a copy never names another card's attachment as its cover.
func (s *Server) copyCardAttachments(r *http.Request, u *User, from *Board, src *Card, to *Board, dst *Card) {
	ctx := r.Context()
	newOf := map[string]string{}
	for _, a := range s.cardAttachments(r, from, src.ID) {
		f, err := s.openAttachment(ctx, &a)
		if err != nil {
			continue // its bytes are gone: nothing to copy
		}
		copied, err := s.StoreAttachment(r, to, dst, u, a.Name, a.Type, f)
		f.Close()
		if err != nil || copied == nil {
			continue
		}
		newOf[a.ID] = copied.ID
		_, _ = s.db.Collection("attachments").UpdateOne(ctx, bson.M{"_id": eq(copied.ID)}, bson.M{"$set": bson.M{"meta.copyFrom": a.ID}})
	}
	var raw struct {
		CoverID string `bson:"coverId"`
	}
	_ = s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(src.ID)}).Decode(&raw)
	if id, ok := newOf[raw.CoverID]; ok && raw.CoverID != "" {
		_, _ = s.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": eq(dst.ID)}, bson.M{"$set": bson.M{"coverId": id}})
	} else {
		_, _ = s.db.Collection("cards").UpdateOne(ctx, bson.M{"_id": eq(dst.ID)}, bson.M{"$unset": bson.M{"coverId": ""}})
	}
}

// attachmentFileHandle is an attachment's bytes, open: seekable, with a time.
type attachmentFileHandle struct {
	*os.File
	modTime time.Time
	cleanup func()
}

func (h *attachmentFileHandle) Close() error {
	err := h.File.Close()
	if h.cleanup != nil {
		h.cleanup()
	}
	return err
}

// openAttachment opens an attachment's bytes wherever Node.js WeKan keeps
// them (models/lib/fileStoreStrategy.js getFileStrategy): GridFS - the
// "attachments" bucket - when the version says gridfs or names a GridFS file,
// or an imported file; otherwise the attachments directory. A GridFS file is
// copied to a temporary file, at most MaxAttachmentBytes, removed on Close.
func (s *Server) openAttachment(ctx context.Context, a *Attachment) (*attachmentFileHandle, error) {
	o := a.Versions.Original
	if o.Storage == "gridfs" || o.Meta.GridFsFileID != "" {
		return s.openGridFS(ctx, "attachments", o.Meta.GridFsFileID)
	}
	p, err := s.attachmentFile(a)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &attachmentFileHandle{File: f, modTime: st.ModTime()}, nil
}

// openGridFS opens a file of one of WeKan's GridFS buckets ("attachments",
// "avatars") by its hex ObjectId.
func (s *Server) openGridFS(ctx context.Context, bucketName, hexID string) (*attachmentFileHandle, error) {
	oid, err := bson.ObjectIDFromHex(hexID)
	if err != nil {
		return nil, os.ErrNotExist
	}
	bucket := s.db.GridFSBucket(options.GridFSBucket().SetName(bucketName))
	stream, err := bucket.OpenDownloadStream(ctx, oid)
	if err != nil {
		return nil, os.ErrNotExist
	}
	defer stream.Close()
	if stream.GetFile().Length > MaxAttachmentBytes {
		return nil, errors.New("the attachment is larger than an attachment may be")
	}
	tmp, err := os.CreateTemp("", "weddy-gridfs-*")
	if err != nil {
		return nil, err
	}
	remove := func() { _ = os.Remove(tmp.Name()) }
	if _, err := io.Copy(tmp, io.LimitReader(stream, MaxAttachmentBytes+1)); err != nil {
		tmp.Close()
		remove()
		return nil, err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		tmp.Close()
		remove()
		return nil, err
	}
	return &attachmentFileHandle{File: tmp, modTime: stream.GetFile().UploadDate, cleanup: remove}, nil
}

// readAttachmentBytes is the whole of an attachment, wherever it is kept.
func (s *Server) readAttachmentBytes(ctx context.Context, a *Attachment) ([]byte, error) {
	f, err := s.openAttachment(ctx, a)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// downloadableAttachment is every check of the attachment route - signed in,
// a reader of its board - shared by the original and its thumbnail, so a
// thumbnail is readable exactly when the original is; nil once answered.
func (s *Server) downloadableAttachment(w http.ResponseWriter, r *http.Request) *Attachment {
	// CacheBleed (GHSA-w3qg-pf27-g68r): whatever the answer, it depends on who
	// asks, and no shared cache may give it to somebody else.
	w.Header().Set("Vary", "Cookie, Authorization, X-Auth-Token")
	a, err := s.attachment(r, r.PathValue("id"))
	if err != nil {
		http.Error(w, "Attachment not found", http.StatusNotFound)
		return nil
	}
	u := s.currentUser(r)
	if u == nil {
		// As WeKan's universal file server: a public board's attachment is
		// read by anybody; any other refuses a caller who is not signed in.
		var b Board
		if s.db.Collection("boards").FindOne(r.Context(), bson.M{"_id": eq(a.Meta.BoardID)}).Decode(&b) == nil && b.Permission == "public" {
			return a
		}
		http.Error(w, "Forbidden", http.StatusForbidden)
		return nil
	}
	if _, err := s.boardFor(r.Context(), a.Meta.BoardID, u); err != nil {
		http.Error(w, "Attachment not found", http.StatusNotFound)
		return nil
	}
	return a
}

// noteAssignedAttachmentWrite is assignedOnlyAttachments.js
// mayWriteCardAttachment's record: when an assigned-only member was refused
// a change to an attachment of a card that is not assigned to them, the
// attempt goes to Admin Panel -> Problems as UnassignedAttachBleed (medium:
// it never disables the account). Any other refusal is not this one.
func (s *Server) noteAssignedAttachmentWrite(r *http.Request, u *User, boardID, cardID, source string) {
	defer func() { _ = recover() }() // logging must never break the guard
	var b Board
	if u == nil || !validID(boardID) || s.db.Collection("boards").FindOne(r.Context(), bson.M{"_id": eq(boardID)}).Decode(&b) != nil {
		return
	}
	m := b.member(u.ID)
	if m == nil || !m.AssignedOnly() || u.IsAdmin {
		return
	}
	var c Card
	if validID(cardID) && s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": eq(cardID), "boardId": eq(b.ID)}).Decode(&c) == nil &&
		slices.Contains(c.Assignees, u.ID) {
		return
	}
	s.recordSecurity(r, securityEvent{Key: "authz.assigned-attachment-write", Action: "blocked", Source: source, UserID: u.ID,
		Detail: "Assigned-only member tried to change an attachment of a card not assigned to them."})
}

// methodAttachmentDelete is WeKan's api.attachment.delete, the DDP twin of
// DELETE /api/attachment/delete/{id}: a soft delete by whoever may change
// the attachment's card - an assigned-only member only on a card assigned
// to them, the refusal recorded as UnassignedAttachBleed.
func (s *Server) methodAttachmentDelete(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	id, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	a, err := s.attachment(r, id)
	if err != nil || a == nil {
		return nil, &methodError{"attachment-not-found", "Attachment not found"}
	}
	var b Board
	if !validID(a.Meta.BoardID) || s.db.Collection("boards").FindOne(r.Context(), bson.M{"_id": eq(a.Meta.BoardID)}).Decode(&b) != nil {
		return nil, &methodError{"not-authorized", "You do not have permission to delete this attachment"}
	}
	var c Card
	_ = s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": eq(a.Meta.CardID), "boardId": eq(b.ID)}).Decode(&c)
	if !b.WritableIn(r.Context(), s, u, "write", &c) {
		s.noteAssignedAttachmentWrite(r, u, b.ID, a.Meta.CardID, "attachment-method:delete")
		return nil, &methodError{"not-authorized", "You do not have permission to delete this attachment"}
	}
	if err := s.softDeleteAttachment(r, a, u); err != nil {
		return nil, &methodError{"delete-error", err.Error()}
	}
	s.hub.publish(b.ID, "board-changed")
	return map[string]any{"success": true, "attachmentId": a.ID, "fileName": a.Name, "message": "Attachment deleted successfully"}, nil
}
