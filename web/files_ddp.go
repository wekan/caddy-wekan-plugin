package web

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Meteor-Files' DDP upload of WeKan's attachments collection, the one WeKan's
// client uses (ostrio:files _FilesCollectionStart_, _Write_ and _Abort_): the
// start names the file and its meta, each chunk is base64 at its place, and
// the last call, eof, stores the file - a card's attachment, or a board's own
// image when the meta names a board only - and answers its document. As WeKan's
// models/attachments.server.js onBeforeUpload, every call is authorized
// (models/lib/attachmentUploadAuth.js uploadDecision): an upload onto a card
// the caller may not edit is UploadBleed, one stamped with another board than
// its card's is StampBleed - refused at the start, and recorded once there.

const ddpUploadTTL = time.Hour

type ddpUpload struct {
	userID    string
	name, typ string
	meta      map[string]any
	chunkSize int64
	path      string
	started   time.Time
}

var ddpUploads = struct {
	sync.Mutex
	m map[string]*ddpUpload
}{m: map[string]*ddpUpload{}}

// fileIDSanitize is ostrio:files' sanitize(fileId, 20, 'a'), WeKan's
// whitelist (models/attachments.js): the token becomes no path.
var fileIDUnsafe = regexp.MustCompile(`[^a-zA-Z0-9_.\-]`)

func sanitizeFileID(id string) string {
	id = fileIDUnsafe.ReplaceAllString(id, "a")
	if len(id) > 20 {
		id = id[:20]
	}
	return id
}

// attachmentLimit is one of Admin Panel's attachment limitSettings.
func (s *Server) attachmentLimit(r *http.Request, name string) any {
	var set bson.M
	if s.db.Collection("attachmentStorageSettings").FindOne(r.Context(), bson.M{}).Decode(&set) != nil {
		return nil
	}
	return nestedField(set, "limitSettings", name)
}

// attachmentUploadBlocked is Admin Panel's limitSettings.attachmentsUploadBlocked:
// every upload from the pages - the card's form and Meteor-Files' - stops.
func (s *Server) attachmentUploadBlocked(r *http.Request) bool {
	return s.attachmentLimit(r, "attachmentsUploadBlocked") == true
}

// uploadDecision is WeKan's uploadDecision: whether u may upload with this
// meta, the board and card it goes to, and the key to record a refusal as.
func (s *Server) uploadDecision(r *http.Request, u *User, meta map[string]any) (b *Board, c *Card, reason, key string) {
	if s.attachmentUploadBlocked(r) {
		return nil, nil, "Uploads are turned off.", ""
	}
	ctx := r.Context()
	cardID, _ := meta["cardId"].(string)
	boardID, _ := meta["boardId"].(string)
	if cardID != "" {
		var card Card
		if !validID(cardID) || s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(cardID)}).Decode(&card) != nil {
			return nil, nil, "Card not found.", ""
		}
		board, err := s.boardFor(ctx, card.BoardID, u)
		if err != nil || !board.WritableIn(ctx, s, u, "write", &card) {
			return nil, nil, "You cannot add attachments to this card.", "authz.attachment-upload"
		}
		if boardID != "" && boardID != card.BoardID {
			return nil, nil, "You cannot add attachments to this card.", "authz.card-child-stamp"
		}
		return board, &card, "", ""
	}
	if boardID == "" {
		return nil, nil, "Board not found.", "authz.attachment-upload"
	}
	board, err := s.boardFor(ctx, boardID, u)
	if err != nil {
		var exists Board
		if validID(boardID) && s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(boardID)}).Decode(&exists) == nil {
			return nil, nil, "You cannot add images to this board.", "authz.attachment-upload"
		}
		return nil, nil, "Board not found.", ""
	}
	if !board.Writable(u, "write", nil) {
		return nil, nil, "You cannot add images to this board.", "authz.attachment-upload"
	}
	return board, nil, "", ""
}

func (s *Server) refuseUpload(r *http.Request, u *User, reason, key string, record bool) error {
	if record && key != "" {
		s.recordSecurity(r, securityEvent{Key: key, Action: "blocked", Source: "upload:attachments", UserID: u.ID,
			Detail: "Upload refused: " + reason})
	}
	return &methodError{"403", reason}
}

func (s *Server) methodFilesStart(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	match := &methodError{"Match.Error", "Match failed"}
	var opts struct {
		File *struct {
			Name string         `json:"name"`
			Type string         `json:"type"`
			Size *float64       `json:"size"`
			Meta map[string]any `json:"meta"`
		} `json:"file"`
		FileID     *string  `json:"fileId"`
		FSName     *string  `json:"FSName"`
		ChunkSize  *float64 `json:"chunkSize"`
		FileLength *float64 `json:"fileLength"`
	}
	if len(args) < 1 || json.Unmarshal(args[0], &opts) != nil || opts.File == nil || opts.FileID == nil || opts.ChunkSize == nil || opts.FileLength == nil {
		return nil, match
	}
	if *opts.ChunkSize < 1 || *opts.ChunkSize > MaxAttachmentBytes || (opts.File.Size != nil && *opts.File.Size > MaxAttachmentBytes) {
		return nil, &methodError{"403", "the file is too large"}
	}
	meta := opts.File.Meta
	if meta == nil {
		meta = map[string]any{}
	}
	id := sanitizeFileID(*opts.FileID)
	if _, _, reason, key := s.uploadDecision(r, u, meta); reason != "" {
		return nil, s.refuseUpload(r, u, reason, key, true)
	}
	if n, _ := s.db.Collection("attachments").CountDocuments(r.Context(), bson.M{"_id": eq(id)}); n > 0 || id == "" {
		return nil, &methodError{"400", "Can't start upload, data substitution detected!"}
	}
	dir := s.attachmentsDir()
	if s.FilesDir == "" || os.MkdirAll(dir, 0o750) != nil {
		return nil, &methodError{"500", "Can't start"}
	}
	f, err := os.CreateTemp(dir, ".ddp-upload-*")
	if err != nil {
		return nil, &methodError{"500", "Can't start"}
	}
	_ = f.Close()
	ddpUploads.Lock()
	defer ddpUploads.Unlock()
	for k, up := range ddpUploads.m { // stale uploads go, as continueUploadTTL's
		if time.Since(up.started) > ddpUploadTTL {
			_ = os.Remove(up.path)
			delete(ddpUploads.m, k)
		}
	}
	if old := ddpUploads.m[id]; old != nil {
		_ = os.Remove(old.path)
	}
	ddpUploads.m[id] = &ddpUpload{userID: u.ID, name: opts.File.Name, typ: opts.File.Type, meta: meta,
		chunkSize: int64(*opts.ChunkSize), path: f.Name(), started: time.Now()}
	return map[string]any{"status": 204}, nil
}

func (s *Server) methodFilesWrite(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	var opts struct {
		EOF     bool     `json:"eof"`
		FileID  *string  `json:"fileId"`
		BinData string   `json:"binData"`
		ChunkID *float64 `json:"chunkId"`
	}
	if len(args) < 1 || json.Unmarshal(args[0], &opts) != nil || opts.FileID == nil {
		return nil, &methodError{"Match.Error", "Match failed"}
	}
	id := sanitizeFileID(*opts.FileID)
	ddpUploads.Lock()
	up := ddpUploads.m[id]
	ddpUploads.Unlock()
	if up == nil || up.userID != u.ID || time.Since(up.started) > ddpUploadTTL {
		return nil, &methodError{"408", "Can't continue upload, session expired. Start upload again."}
	}
	// Every chunk is authorized again, as WeKan's onBeforeUpload: a membership
	// taken away during an upload stops it.
	b, c, reason, key := s.uploadDecision(r, u, up.meta)
	if reason != "" {
		s.abortDDPUpload(id)
		return nil, s.refuseUpload(r, u, reason, key, false)
	}
	if !opts.EOF {
		data, err := base64.StdEncoding.DecodeString(opts.BinData)
		chunk := int64(1)
		if opts.ChunkID != nil {
			chunk = int64(*opts.ChunkID)
		}
		if err != nil || chunk < 1 || (chunk-1)*up.chunkSize+int64(len(data)) > MaxAttachmentBytes {
			s.abortDDPUpload(id)
			return nil, &methodError{"400", "not a chunk of this upload"}
		}
		f, err := os.OpenFile(up.path, os.O_WRONLY, 0)
		if err != nil {
			return nil, &methodError{"503", "try again"}
		}
		_, err = f.WriteAt(data, (chunk-1)*up.chunkSize)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, &methodError{"503", "try again"}
		}
		return map[string]any{"status": 204}, nil
	}
	defer s.abortDDPUpload(id)
	f, err := os.Open(up.path)
	if err != nil {
		return nil, &methodError{"503", "try again"}
	}
	defer f.Close()
	a, err := s.StoreAttachment(r, b, c, u, up.name, up.typ, f)
	if err != nil {
		return nil, &methodError{"400", err.Error()}
	}
	var doc map[string]any
	if s.db.Collection("attachments").FindOne(r.Context(), bson.M{"_id": a.ID}).Decode(&doc) == nil {
		out := plain(doc).(map[string]any)
		delete(out, "path")
		delete(out, "_storagePath")
		if v, ok := out["versions"].(map[string]any); ok {
			if o, ok := v["original"].(map[string]any); ok {
				delete(o, "path")
			}
		}
		out["status"] = 200
		return out, nil
	}
	return map[string]any{"_id": a.ID, "status": 200}, nil
}

func (s *Server) methodFilesAbort(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	id, err := stringArg(args, 0)
	if err != nil {
		return nil, err
	}
	id = sanitizeFileID(id)
	ddpUploads.Lock()
	up := ddpUploads.m[id]
	ddpUploads.Unlock()
	if up != nil && up.userID == u.ID {
		s.abortDDPUpload(id)
	}
	return nil, nil
}

func (s *Server) abortDDPUpload(id string) {
	ddpUploads.Lock()
	defer ddpUploads.Unlock()
	if up := ddpUploads.m[id]; up != nil {
		if strings.HasPrefix(filepath.Base(up.path), ".ddp-upload-") {
			_ = os.Remove(up.path)
		}
		delete(ddpUploads.m, id)
	}
}

// methodInsertChecklist is the checklists collection's own /checklists/insert
// as WeKan's rules let it through (server/permissions/checklists.js): the deny
// rule first - a boardId that is not the card's is StampBleed, refused and
// recorded without an account (denyForgedCardChildInsert) - then the allow
// rule, the card's editors; the checklist is stamped with its card's board
// (Checklists.before.insert), recorded and announced as Weddy's own add.
func (s *Server) methodInsertChecklist(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	denied := &methodError{"403", "Access denied"}
	var doc map[string]any
	if len(args) < 1 || json.Unmarshal(args[0], &doc) != nil {
		return nil, denied
	}
	cardID, _ := doc["cardId"].(string)
	title, _ := doc["title"].(string)
	ctx := r.Context()
	var c Card
	if !validID(cardID) || s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(cardID)}).Decode(&c) != nil {
		return nil, denied
	}
	if stamp, _ := doc["boardId"].(string); stamp != "" && stamp != c.BoardID {
		s.recordSecurity(r, securityEvent{Key: "authz.card-child-stamp", Action: "blocked", Source: "ddp:checklist-insert",
			Detail: "Client inserted a document on its own card with a boardId that is not its card's board."})
		return nil, denied
	}
	b, err := s.boardFor(ctx, c.BoardID, u)
	if err != nil || !b.WritableIn(ctx, s, u, "write", &c) {
		return nil, denied
	}
	title = strings.TrimSpace(title)
	if title == "" || len(title) > 10000 {
		return nil, &methodError{"400", "Title is required"}
	}
	id, _ := doc["_id"].(string)
	if id == "" {
		id = randomID()
	}
	if !validID(id) {
		return nil, denied
	}
	sort, ok := doc["sort"].(float64)
	if !ok {
		sort = s.nextSort(ctx, "checklists", bson.M{"cardId": eq(c.ID)})
	}
	now := time.Now().UTC()
	if _, err := s.insertRecorded(ctx, u.ID, "checklist", bson.M{"_id": id, "cardId": c.ID, "boardId": b.ID, "title": title, // inserted, not a filter
		"sort": sort, "createdAt": now, "modifiedAt": now}); err != nil {
		return nil, &methodError{"409", "Duplicate _id"}
	}
	s.activity(ctx, u.ID, "addChecklist", bson.M{"cardId": c.ID, "boardId": b.ID, "checklistId": id, "checklistName": title,
		"listId": c.ListID, "swimlaneId": c.SwimlaneID})
	s.hub.publishFrom(b.ID, "board-changed", r.Header.Get("X-Weddy-Page"))
	return id, nil
}
