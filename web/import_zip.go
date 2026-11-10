package web

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// A .zip of WeKan's export - wekan.json and the attachments as files - as a new
// board, or into a board the caller may change, as Node.js WeKan
// (models/importZip.js). ZipBleed and ZipBombBleed: an entry's name is never a
// path, and what is unpacked is counted as it comes, never trusted from the
// archive's own header.
func (s *Server) importZipRoutes(m *http.ServeMux) {
	m.HandleFunc("POST /api/import/zip", s.importable(s.apiImportZip))
}

// MaxImportDocumentBytes is the most wekan.json may unpack to.
var MaxImportDocumentBytes int64 = 256 << 20

func maxImportZipBytes() int64 {
	if n, err := strconv.ParseInt(os.Getenv("WEKAN_IMPORT_ZIP_MAX_BYTES"), 10, 64); err == nil && n > 0 {
		return n
	}
	return 5 << 30
}

var errTooLarge = errors.New("too large")

// readBounded reads at most max bytes of an entry, counting what it unpacks.
func readBounded(f *zip.File, max int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, errTooLarge
	}
	return data, nil
}

var (
	wekanJSONEntry  = regexp.MustCompile(`(^|/)wekan\.json$`)
	attachmentEntry = regexp.MustCompile(`(?:^|/)attachments/([^/]+)$`)
)

func (s *Server) apiImportZip(w http.ResponseWriter, r *http.Request) {
	answer := func(code int, data any) { writeJSON(w, code, data) }
	u, ok := s.exportUser(r)
	if !ok {
		answer(http.StatusUnauthorized, map[string]string{"error": "Invalid token"})
		return
	}
	if u == nil {
		answer(http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}
	q := r.URL.Query()
	newBoard := q.Get("newBoard") == "1"
	var into *Board
	if id := q.Get("boardId"); id != "" {
		b, err := s.boardFor(r.Context(), id, u)
		if err != nil {
			answer(http.StatusNotFound, map[string]string{"error": "Not found"})
			return
		}
		// Importing writes: "may you change it", not "may you see it".
		if !b.Can(u.ID, "write") {
			answer(http.StatusForbidden, map[string]string{"error": "Forbidden"})
			return
		}
		into = b
	}
	if into == nil && !newBoard {
		answer(http.StatusNotFound, map[string]string{"error": "Not found"})
		return
	}

	// The upload to a file of our own naming, its size checked as it arrives.
	tmp, err := os.CreateTemp("", "weddy-import-*.zip")
	if err != nil {
		answer(http.StatusInternalServerError, map[string]string{"error": "import-failed"})
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	limit := maxImportZipBytes()
	n, err := io.Copy(tmp, io.LimitReader(r.Body, limit+1))
	if err != nil {
		answer(http.StatusBadRequest, map[string]string{"error": "import-failed"})
		return
	}
	if n > limit {
		answer(http.StatusRequestEntityTooLarge, map[string]string{"error": "import-zip-too-large"})
		return
	}
	zr, err := zip.NewReader(tmp, n)
	if err != nil {
		answer(http.StatusBadRequest, map[string]string{"error": "import-not-wekan-export"})
		return
	}
	var docEntry *zip.File
	byID := map[string]*zip.File{}
	byName := map[string][]*zip.File{}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if docEntry == nil && wekanJSONEntry.MatchString(f.Name) {
			docEntry = f
		}
		if m := attachmentEntry.FindStringSubmatch(f.Name); m != nil {
			base := m[1]
			if dash := strings.Index(base, "-"); dash > 0 {
				byID[base[:dash]] = f
			}
			byName[base] = append(byName[base], f)
		}
	}
	if docEntry == nil {
		answer(http.StatusBadRequest, map[string]string{"error": "import-not-wekan-export"})
		return
	}
	raw, err := readBounded(docEntry, MaxImportDocumentBytes)
	if err != nil {
		answer(http.StatusRequestEntityTooLarge, map[string]string{"error": "import-document-too-large"})
		return
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var parsed map[string]any
	if dec.Decode(&parsed) != nil {
		answer(http.StatusBadRequest, map[string]string{"error": "import-not-wekan-export"})
		return
	}
	numbersToFloat(parsed)
	clean, err := s.secureImport(r, u, "wekan-zip", parsed)
	if err != nil {
		answer(http.StatusBadRequest, map[string]string{"error": "import-failed"})
		return
	}
	doc, _ := clean.(map[string]any)
	if f, has := doc["_format"]; has && f != "wekan-board-1.0.0" {
		answer(http.StatusBadRequest, map[string]string{"error": "invalid-format"})
		return
	}
	doc["_format"] = "wekan-board-1.0.0"
	// An attachment's file: the entry named attachments/<id>-..., else - WeKan's
	// own .zip export names them so - the next entry of its name.
	files := func(a bson.M) io.ReadCloser {
		f := byID[str(a, "_id")]
		if f == nil {
			name := cleanFileName(str(a, "name"))
			if list := byName[name]; len(list) > 0 {
				f, byName[name] = list[0], list[1:]
			}
		}
		if f == nil {
			return nil
		}
		data, err := readBounded(f, MaxAttachmentBytes)
		if err != nil {
			return nil
		}
		return io.NopCloser(bytes.NewReader(data))
	}
	counts := map[string]int{}
	id, err := s.importBoardWith(r, u, doc, nil, importOptions{Into: into, Files: files, Counts: counts})
	if errors.Is(err, errBoardCreation) {
		answer(http.StatusForbidden, map[string]string{"error": "not-authorized"})
		return
	}
	if err != nil {
		answer(http.StatusInternalServerError, map[string]string{"error": "import-failed"})
		return
	}
	if into == nil {
		answer(http.StatusOK, map[string]any{"ok": true, "boardId": id})
		return
	}
	s.hub.publish(into.ID, "board-changed")
	answer(http.StatusOK, map[string]any{"ok": true, "counts": counts})
}
