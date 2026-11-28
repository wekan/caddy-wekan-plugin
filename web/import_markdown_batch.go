package web

import (
	"archive/zip"
	"io"
	"mime/multipart"
	"net/http"
	"path"
	"strings"
	"unicode/utf8"
)

type markdownImportResult struct {
	Name string
	URL  string
}

func markdownFile(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".md", ".markdown":
		return true
	}
	return false
}

func (s *Server) importMarkdownFiles(r *http.Request, u *User, files []*multipart.FileHeader, limit int) []markdownImportResult {
	const maxResults = 200
	results := make([]markdownImportResult, 0, len(files))
	remaining := int64(limit)
	failed := func(name string) {
		results = append(results, markdownImportResult{Name: name})
	}
	importFile := func(name string, reader io.Reader) {
		raw, err := io.ReadAll(io.LimitReader(reader, remaining+1))
		if err != nil || int64(len(raw)) > remaining {
			remaining = 0
			failed(name)
			return
		}
		remaining -= int64(len(raw))
		if !utf8.Valid(raw) || strings.TrimSpace(string(raw)) == "" {
			failed(name)
			return
		}
		board, err := s.importInput(r, u, "markdown", string(raw), importPeopleOptions{Mode: "placeholder"})
		if err != nil {
			failed(name)
			return
		}
		results = append(results, markdownImportResult{Name: name, URL: "/b/" + board.ID + "/" + board.Slug})
	}
	for _, file := range files {
		if len(results) >= maxResults {
			break
		}
		reader, err := file.Open()
		if err != nil {
			failed(file.Filename)
			continue
		}
		switch {
		case markdownFile(file.Filename):
			importFile(file.Filename, reader)
		case strings.EqualFold(path.Ext(file.Filename), ".zip"):
			archive, err := zip.NewReader(reader, file.Size)
			first := len(results)
			if err == nil {
				for _, entry := range archive.File {
					if len(results) >= maxResults {
						break
					}
					name := strings.ReplaceAll(entry.Name, "\\", "/")
					if entry.FileInfo().IsDir() || strings.HasPrefix(name, "__MACOSX/") || strings.HasPrefix(path.Base(name), "._") || !markdownFile(name) {
						continue
					}
					content, err := entry.Open()
					if err != nil {
						failed(name)
						continue
					}
					importFile(name, content)
					content.Close()
				}
			}
			if len(results) == first {
				failed(file.Filename)
			}
		default:
			failed(file.Filename)
		}
		reader.Close()
	}
	return results
}
