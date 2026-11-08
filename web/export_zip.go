package web

import (
	"archive/zip"
	"encoding/json"
	"io"
	"net/http"
	"os"
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
	m.HandleFunc("GET /api/boards/{board}/exportZip", s.apiExportZip)
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
	doc, err := s.exportBoard(r.Context(), &b, true)
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
	used := map[string]bool{}
	for _, a := range s.liveAttachments(r, b.ID) {
		p, err := s.attachmentFile(&a)
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
		in, err := os.Open(p)
		if err != nil {
			continue
		}
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
