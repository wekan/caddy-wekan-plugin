package web

import (
	"archive/zip"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The board as a .zip, as Node.js WeKan (models/export.js exportZip,
// models/server/ExporterZip.js): wekan.json - the JSON export without the
// attachment data - and the attachments as files beside it, so a board whose
// files are too large for one JSON document still exports.
func (s *Server) exportZipRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/boards/{board}/exportZip", s.exportable(s.apiExportZip))
}

// numberedName is WeKan's: "name-1.ext" for the second file of a name.
func numberedName(name string, n int) string {
	if n == 0 {
		return name
	}
	base, ext := name, ""
	if dot := strings.LastIndex(name, "."); dot > 0 {
		base, ext = name[:dot], name[dot:]
	}
	return base + "-" + strconv.Itoa(n) + ext
}

var zipNameUnsafe = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func (s *Server) apiExportZip(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("board")
	var b Board
	if !validID(id) || s.db.Collection("boards").FindOne(r.Context(), bson.M{"_id": eq(id)}).Decode(&b) != nil {
		apiError(w, http.StatusNotFound, "Not found")
		return
	}
	u, ok := s.exportUser(r)
	if !ok {
		apiError(w, http.StatusUnauthorized, "Invalid token")
		return
	}
	if b.Permission != "public" {
		if u == nil {
			apiError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}
		if m := b.member(u.ID); m == nil || m.AssignedOnly() {
			apiError(w, http.StatusForbidden, "Forbidden")
			return
		}
	}
	if !s.fieldExportAllowed(r.Context(), &b, u) {
		apiError(w, http.StatusForbidden, errFieldExport)
		return
	}
	// The same scope and selection as the JSON export (models/importZip.js).
	q := r.URL.Query()
	doc, err := s.exportBoardWith(r.Context(), &b, exportDocOptions{WithoutFiles: true, Want: exportWanted(q.Get("fields")), Scope: exportScopeOf(q), ExcludeArchived: q.Get("archived") == "false", r: r})
	if err != nil {
		apiError(w, http.StatusInternalServerError, "Export failed")
		return
	}
	name := strings.Trim(regexp.MustCompile(`-+`).ReplaceAllString(zipNameUnsafe.ReplaceAllString(b.Title, "-"), "-"), "-")
	if len(name) > 80 {
		name = name[:80]
	}
	if name == "" {
		name = "export-board"
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.zip"`)
	w.Header().Set("Cache-Control", "no-store")
	zw := zip.NewWriter(w)
	defer zw.Close()
	if f, err := zw.Create("wekan.json"); err == nil {
		_ = json.NewEncoder(f).Encode(doc)
	}
	// The files of the attachments the document carries, no others.
	inDoc := map[string]bool{}
	if m, ok := doc.(map[string]any); ok {
		if list, ok := m["attachments"].([]any); ok {
			for _, x := range list {
				if e, ok := x.(map[string]any); ok {
					inDoc[strOf(e["_id"])] = true
				}
			}
		}
	}
	used := map[string]bool{}
	for _, a := range s.liveAttachments(r, b.ID) {
		if !inDoc[a.ID] {
			continue
		}
		in, err := s.openAttachment(r.Context(), &a)
		if err != nil {
			continue
		}
		want := cleanFileName(a.Name)
		if want == "" {
			want = "download"
		}
		n := want
		for i := 1; used[n]; i++ {
			n = numberedName(want, i)
		}
		used[n] = true
		if f, err := zw.Create("attachments/" + n); err == nil {
			_, _ = io.Copy(f, in)
		}
		in.Close()
	}
}

// liveAttachments are the board's attachments that are not deleted.
func (s *Server) liveAttachments(r *http.Request, boardID string) []Attachment {
	cur, err := s.db.Collection("attachments").Find(r.Context(), bson.M{"meta.boardId": eq(boardID), "deletedAt": nil})
	out := []Attachment{}
	if err == nil {
		_ = cur.All(r.Context(), &out)
	}
	return out
}

// apiExportTrelloZip writes attachment entries only for cards in the exported
// document. The document carries metadata for entries successfully written.
func (s *Server) apiExportTrelloZip(w http.ResponseWriter, r *http.Request) {
	board, _, ok := s.exportReader(w, r, false)
	if !ok {
		return
	}
	user, valid := s.exportUser(r)
	if !valid {
		apiError(w, http.StatusUnauthorized, "Invalid token")
		return
	}
	ctx := r.Context()
	query := r.URL.Query()
	want := exportWanted(query.Get("fields"))
	doc := formatTrello(s.collectExternal(ctx, board, user, "trellozip", want))
	cards := jsGet(doc, "cards").([]any)
	byCard := map[string]int{}
	for index, value := range cards {
		card := value.(jsObject)
		byCard[cdString(jsGet(card, "id"))] = index
		card.set("attachments", []any{})
		cards[index] = card
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="trello.zip"`)
	w.Header().Set("Cache-Control", "private, no-store")
	archive := zip.NewWriter(w)
	defer archive.Close()
	if want.has("attachments") && query.Get("attachments") != "false" {
		for _, attachment := range s.liveAttachments(r, board.ID) {
			index, included := byCard[attachment.Meta.CardID]
			if !included || !validID(attachment.ID) {
				continue
			}
			input, err := s.openAttachment(ctx, &attachment)
			if err != nil {
				continue
			}
			name := attachment.Name
			if name == "" {
				name = attachment.ID
			}
			name = strings.NewReplacer("/", "_", "\\", "_", ":", "_", "\x00", "_").Replace(name)
			file, err := archive.Create("attachments/" + attachment.ID + "_" + name)
			if err == nil {
				_, err = io.Copy(file, input)
			}
			input.Close()
			if err != nil {
				continue
			}
			card := cards[index].(jsObject)
			refs := jsGet(card, "attachments").([]any)
			card.set("attachments", append(refs, jsObject{{"id", attachment.ID}, {"name", name},
				{"fileName", name}, {"bytes", attachment.Size}, {"mimeType", attachment.Type}, {"isUpload", true}}))
		}
	}
	if file, err := archive.Create("board.json"); err == nil {
		_, _ = file.Write(jsJSON(doc, ""))
	}
}
