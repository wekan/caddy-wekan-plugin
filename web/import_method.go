package web

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
)

// The one way another tool's document becomes a board, for the import page
// and for WeKan's importBoard method alike (models/import.js): its shape
// checked (models/lib/importSourceShape.js), what it carries cleaned at the
// import boundary, its source's parser, and WeKan's creator.

// importWorkbookSources are the sources whose file is an .xlsx workbook.
var importWorkbookSources = map[string]bool{"planner": true, "monday": true, "wrike": true, "teamwork": true, "businessmap": true}

var (
	errImportShape    = errors.New("invalid-import-format")
	errImportTooLarge = errors.New("import-too-large")
	leoShape          = regexp.MustCompile(`<leo_file[\s>]`)
	opmlShape         = regexp.MustCompile(`<opml[\s>]`)
)

// validImportShape is WeKan's validateImportSourceShape, for the sources
// Weddy parses.
func validImportShape(source string, v any) bool {
	arrayAt := func(o any, k string) bool { _, ok := jsGet(o, k).([]any); return ok }
	_, isArr := v.([]any)
	text, isText := v.(string)
	switch source {
	case "kanboard":
		return arrayAt(v, "tasks")
	case "github", "gitlab", "gitea", "forgejo":
		return isArr || arrayAt(v, "issues")
	case "deck":
		return arrayAt(v, "stacks") || arrayAt(jsGet(v, "board"), "stacks")
	case "openproject":
		return isArr || arrayAt(v, "elements") || arrayAt(jsGet(v, "_embedded"), "elements")
	case "asana":
		return isArr || arrayAt(v, "data")
	case "zenkit":
		return isArr || arrayAt(v, "items") || (arrayAt(v, "elements") && (arrayAt(v, "entries") || arrayAt(v, "listEntries")))
	case "taiga":
		return arrayAt(v, "user_stories")
	case "planner", "monday", "wrike", "teamwork", "businessmap":
		// The workbook as { excelBase64 }, or its bytes from the import page.
		x, _ := jsGet(v, "excelBase64").(string)
		return x != "" || isText && text != ""
	case "plane":
		zip, _ := jsGet(v, "zipBase64").(string)
		xlsx, _ := jsGet(v, "xlsxBase64").(string)
		return zip != "" || xlsx != "" || isText && strings.TrimSpace(text) != ""
	case "vikunja", "notion":
		// The export .zip as { zipBase64 }, or the text of its data.json.
		zip, _ := jsGet(v, "zipBase64").(string)
		return zip != "" || isText && strings.TrimSpace(text) != ""
	case "tasksorg":
		return arrayAt(jsGet(v, "data"), "tasks")
	case "kanri":
		return arrayAt(v, "columns") || arrayAt(v, "boards")
	case "markdown", "todotxt", "taskwarrior", "nullboard", "obsidian", "orgmode", "todoist", "meistertask", "linear", "ticktick", "clickup", "pivotal", "redmine", "quire", "focalboard", "superproductivity":
		return isText && strings.TrimSpace(text) != ""
	case "leo":
		return isText && leoShape.MatchString(text)
	case "opml":
		return isText && opmlShape.MatchString(text)
	}
	return false
}

// importInput writes one document - the text of a text source, the decoded
// JSON of any other - as a new board.
// importPeopleOptions are the import's people: WeKan's membersMode, and the
// file's people mapped to existing users ("map").
//
// Split is WeKan's splitBy "swimlane", "One board per project": each of the
// document's swimlanes its own board (splitBySwimlane).
type importPeopleOptions struct {
	Mode    string
	Mapping map[string]string
	Split   bool
}

// importInput writes one document - the text of a text source, the decoded
// JSON of any other - as a new board.
func (s *Server) importInput(r *http.Request, u *User, source string, input any, people importPeopleOptions) (*Board, error) {
	parsed, err := s.importParse(r, u, source, input)
	if err != nil {
		return nil, err
	}
	if !people.Split {
		return s.importGeneral(r.Context(), u, parsed, source, people)
	}
	// models/import.js: each part its own import, the first board answered.
	parts, err := splitBySwimlane(parsed)
	if err != nil {
		return nil, &methodError{"invalid-import-format", err.Error()}
	}
	var first *Board
	for _, part := range parts {
		b, err := s.importGeneral(r.Context(), u, part, source, people)
		if err != nil {
			return nil, err
		}
		if first == nil {
			first = b
		}
	}
	return first, nil
}

// importParse is the document as WeKan's importBoard reads it before its
// creator: the shape checked, what it carries cleaned, the source's parser.
func (s *Server) importParse(r *http.Request, u *User, source string, input any) (jsObject, error) {
	if !validImportShape(source, input) {
		return nil, errImportShape
	}
	secure := func(v any) (any, error) {
		clean, err := s.secureImport(r, u, source, v)
		if err != nil {
			return nil, errImportTooLarge
		}
		return clean, nil
	}
	// A workbook comes to importBoard as { excelBase64 }: its bytes.
	if z, ok := jsGet(input, "excelBase64").(string); ok && importWorkbookSources[source] {
		if len(z) > (MaxImportBytes+2)/3*4 {
			return nil, errImportTooLarge
		}
		raw, err := base64.StdEncoding.DecodeString(z)
		if err != nil {
			return nil, errImportShape
		}
		input = string(raw)
	}
	// Plane's export .zip comes as { zipBase64 } - a workbook sent so is
	// refused, as WeKan's readPlaneArchive does - and a workbook from it as
	// { xlsxBase64 }.
	if source == "plane" {
		zipB64, _ := jsGet(input, "zipBase64").(string)
		xlsxB64, _ := jsGet(input, "xlsxBase64").(string)
		if b64 := jsOr(zipB64, xlsxB64).(string); b64 != "" {
			if len(b64) > (maxPlaneZipBytes+2)/3*4+4 {
				return nil, errImportTooLarge
			}
			raw, err := base64.StdEncoding.DecodeString(b64)
			if err != nil {
				return nil, errImportShape
			}
			if zipB64 != "" {
				if _, err := readPlaneArchive(raw); err != nil {
					return nil, &methodError{"invalid-import-format", err.Error()}
				}
			}
			input = string(raw)
		}
	}
	// A Vikunja or Notion .zip comes to importBoard as { zipBase64 }: its
	// bytes, at most WeKan's 64 MB.
	if z, ok := jsGet(input, "zipBase64").(string); ok && (source == "vikunja" || source == "notion") {
		if len(z) > (maxVikunjaZipBytes+2)/3*4+4 {
			return nil, errImportTooLarge
		}
		raw, err := base64.StdEncoding.DecodeString(z)
		if err != nil {
			return nil, errImportShape
		}
		input = string(raw)
	}
	var parsed jsObject
	var err error
	if parse, ok := importTextParsers[source]; ok {
		// An outline is XML: its tags would go as markup, so it is parsed
		// first and the parsed board cleaned.
		text, _ := input.(string)
		if !importParsedFirst[source] {
			clean, err := secure(text)
			if err != nil {
				return nil, err
			}
			text, _ = clean.(string)
		}
		if parsed, err = parse(text); err != nil {
			return nil, &methodError{"invalid-import-format", err.Error()}
		}
		if importParsedFirst[source] {
			clean, err := secure(parsed)
			if err != nil {
				return nil, err
			}
			parsed, _ = clean.(jsObject)
		}
	} else {
		doc, err := secure(input)
		if err != nil {
			return nil, err
		}
		if parsed, err = importParsers[source](doc); err != nil {
			return nil, &methodError{"invalid-import-format", err.Error()}
		}
	}
	return parsed, nil
}

// methodImportBoard is WeKan's importBoard(board, data, importSource,
// currentBoard) for the sources Weddy parses: the new board's id, or
// invalid-import-format, import-too-large or import-disabled as WeKan's.
func (s *Server) methodImportBoard(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	if len(args) < 3 {
		return nil, &methodError{"Match.Error", "Match failed"}
	}
	var source string
	if json.Unmarshal(args[2], &source) != nil {
		return nil, &methodError{"Match.Error", "Expected string"}
	}
	board, err := decodeJS(args[0])
	if err != nil {
		return nil, &methodError{"Match.Error", "Match failed"}
	}
	if source == "csv" || source == "excel" {
		var data json.RawMessage
		if len(args) > 1 {
			data = args[1]
		}
		return s.methodImportTable(r, u, source, board, data)
	}
	if s.importDisabled(r.Context()) {
		return nil, &methodError{"import-disabled", "Import is disabled by the administrator."}
	}
	_, jsonSource := importParsers[source]
	_, text := importTextParsers[source]
	if !jsonSource && !text {
		return nil, &methodError{"not-implemented", "Weddy does not import " + source + " through importBoard yet"}
	}
	var data struct {
		MembersMode    string         `json:"membersMode"`
		MembersMapping map[string]any `json:"membersMapping"`
		PreviewPeople  bool           `json:"previewPeople"`
		SplitBy        any            `json:"splitBy"`
	}
	if len(args) > 1 {
		_ = json.Unmarshal(args[1], &data)
	}
	// previewPeople: the file's people for the page's map-members step, read
	// by the same parser - nothing is created (WeKan's importedPeople).
	if data.PreviewPeople {
		parsed, err := s.importParse(r, u, source, board)
		if err != nil {
			var me *methodError
			if errors.As(err, &me) {
				return nil, me
			}
			return nil, &methodError{"invalid-import-format", "Invalid " + source + " import document shape"}
		}
		out := []any{}
		for _, p := range importPeople(importTasks(parsed)) {
			o := jsObject{}
			o.set("key", p.key)
			o.set("name", p.name)
			out = append(out, o)
		}
		return out, nil
	}
	mapping := map[string]string{}
	for k, v := range data.MembersMapping {
		if id, ok := v.(string); ok && id != "" {
			mapping[k] = id
		}
	}
	b, err := s.importInput(r, u, source, board, importPeopleOptions{Mode: data.MembersMode, Mapping: mapping, Split: data.SplitBy == "swimlane"})
	var me *methodError
	switch {
	case err == nil:
		return b.ID, nil
	case errors.As(err, &me):
		return nil, me
	case errors.Is(err, errImportTooLarge):
		return nil, &methodError{"import-too-large", "Import/export limit exceeded"}
	case errors.Is(err, errBoardCreation):
		return nil, &methodError{"not-authorized", err.Error()}
	}
	return nil, &methodError{"invalid-import-format", "Invalid " + source + " import document shape"}
}
