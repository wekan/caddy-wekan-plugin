package web

import (
	"archive/zip"
	"net/http"
	"strings"
)

// WeKan's ZIP preview (client/lib/zipAttachmentViewer.js): an archive
// attachment's entry names, read from its central directory - nothing
// inflated, so neither an encrypted nor a huge entry is touched - at most
// 2048 of them from an archive of at most 32 MiB (attachmentPreviewSource.js),
// as inert text the viewer lists. Whoever may download the attachment may
// list it.

const (
	zipPreviewMaxEntries = 2048
	zipPreviewMaxBytes   = 32 << 20
)

// isZipAttachment is attachmentKind's isZIP.
func isZipAttachment(typ, name string) bool {
	typ = strings.ToLower(typ)
	if typ == "application/zip" || typ == "application/x-zip-compressed" {
		return true
	}
	return strings.HasSuffix(strings.ToLower(name), ".zip") && (typ == "" || typ == "application/octet-stream")
}

func (s *Server) serveZipEntries(w http.ResponseWriter, r *http.Request) {
	a := s.downloadableAttachment(w, r)
	if a == nil {
		return
	}
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "private, no-cache")
	if !isZipAttachment(a.Type, a.Name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not a ZIP archive"})
		return
	}
	f, err := s.openAttachment(r.Context(), a)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Attachment not found"})
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "unreadable"})
		return
	}
	if st.Size() > zipPreviewMaxBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "too large to preview"})
		return
	}
	zr, err := zip.NewReader(f, st.Size())
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "Invalid ZIP archive"})
		return
	}
	if len(zr.File) > zipPreviewMaxEntries {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "Too many ZIP entries"})
		return
	}
	names := make([]string, 0, len(zr.File))
	for _, e := range zr.File {
		names = append(names, e.Name)
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": names})
}
