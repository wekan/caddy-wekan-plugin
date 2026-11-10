package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// The import page for a table - CSV/TSV text or an Excel workbook - as Node.js
// WeKan's /import/csv and /import/excel: choose the file (or paste the text),
// confirm which column is which field (client/components/import/csvMapping),
// and the new board opens. The rows ride from the first step to the second
// in the form, and the server checks the mapping against them again.

const maxImportForm = 32 << 20

// importPageLimit is the largest file the import page reads for source: an
// export .zip takes its parser's limit, as WeKan's - Vikunja's, Plane's and
// Notion's 64 MB - and every other file maxImportForm.
func importPageLimit(source string) int {
	switch source {
	case "vikunja":
		return maxVikunjaZipBytes
	case "plane":
		return maxPlaneZipBytes
	case "notion":
		return maxNotionZipBytes
	}
	return maxImportForm
}

// tableSources are the sources this page reads, with the files they take.
var tableSources = map[string]string{"csv": ".csv,.tsv,.txt", "excel": ".xlsx"}

func (s *Server) importPageRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /import/{source}", s.importable(s.page(s.importPage)))
	m.HandleFunc("POST /import/{source}", s.importable(s.page(s.importMappingPage)))
	m.HandleFunc("POST /import/{source}/board", s.importable(s.page(s.importBoardPage)))
	m.HandleFunc("GET /import/{source}/report", s.importable(s.page(s.importReportPage)))
	m.HandleFunc("POST /import/{source}/members", s.importable(s.page(s.importMembersPage)))
}

type mappingOption struct {
	Index    int
	Text     string
	Selected bool
}

type mappingField struct {
	Field, Label string
	Options      []mappingOption
}

func (s *Server) importData(r *http.Request, u *User, source string) map[string]any {
	lang := languageOf(r, u).Tag
	accept, paste := tableSources[source], source == "csv"
	_, general := importParsers[source]
	if _, text := importTextParsers[source]; text {
		general = true
	}
	if general {
		accept, paste = ".json,application/json", true
		if _, text := importTextParsers[source]; text {
			accept = map[string]string{"markdown": ".md,text/markdown,text/plain", "todotxt": ".txt,text/plain",
				"taskwarrior": ".json,application/json,.ndjson,text/plain", "nullboard": ".nbx,.json,application/json",
				"opml": ".opml,.xml,text/x-opml,text/xml", "leo": ".leo,.xml,application/xml",
				"obsidian": ".md,text/markdown,text/plain", "orgmode": ".org,text/plain",
				"todoist": ".csv,text/csv", "meistertask": ".csv,text/csv", "linear": ".csv,text/csv",
				"ticktick": ".csv,text/csv", "clickup": ".csv,text/csv", "pivotal": ".csv,text/csv",
				"redmine": ".csv,text/csv", "quire": ".csv,text/csv", "focalboard": ".jsonl,.json,application/json",
				"superproductivity": ".json,application/json", "vikunja": ".zip,application/zip,.json,application/json",
				"planner": ".xlsx,application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "monday": ".xlsx,application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "wrike": ".xlsx,application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
				"teamwork": ".xlsx,application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "businessmap": ".xlsx,application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
				"notion": ".zip,application/zip,.csv,text/csv", "plane": ".zip,application/zip,.json,application/json,.csv,text/csv,.xlsx"}[source]
		}
	}
	data := map[string]any{"Title": tr(lang, "import-board-c"), "User": u, "Source": source, "Accept": accept, "Paste": paste, "General": general,
		"Instruction": importInstruction(lang, source), "MembersModes": importMembersModes}
	// The sign-in a service's attachment files need (importAttachmentAuth.js
	// attachmentAuthFields): its address, its user name, its token.
	if spec, ok := importAttachmentSources[source]; ok && general {
		data["AttachmentAuth"] = map[string]bool{"URL": spec.url, "Username": spec.username}
	}
	return data
}

func (s *Server) importPage(w http.ResponseWriter, r *http.Request, u *User) {
	source := r.PathValue("source")
	_, general := importParsers[source]
	if _, text := importTextParsers[source]; text {
		general = true
	}
	if _, ok := tableSources[source]; !ok && !general {
		http.Error(w, "This import is not in Weddy yet.", http.StatusNotFound)
		return
	}
	s.render(w, r, "import", http.StatusOK, s.importData(r, u, source))
}

// importAuthOf is the import page's sign-in for attachment files.
func importAuthOf(r *http.Request) *importAttachmentAuth {
	a := &importAttachmentAuth{URL: r.FormValue("attachmentUrl"), Username: r.FormValue("attachmentUsername"), Token: r.FormValue("attachmentToken")}
	if len(a.URL) > 2048 || len(a.Username) > 2048 || len(a.Token) > 2048 || a.Token == "" {
		return nil
	}
	return a
}

// importRows are the boards of the first step's file or text: a CSV/TSV
// text is one, a workbook every sheet with a header (excelSheetBoards), with
// the sheets that have none.
func importRows(r *http.Request, source string) ([]sheetBoard, []string, error) {
	var data []byte
	if f, _, err := r.FormFile("file"); err == nil {
		defer f.Close()
		data, err = io.ReadAll(io.LimitReader(f, maxImportForm+1))
		if err != nil || len(data) > maxImportForm {
			return nil, nil, errors.New("error-csv-schema")
		}
	}
	if source == "excel" {
		sheets, err := readXlsxValues(data)
		if err != nil {
			return nil, nil, errors.New("error-csv-schema")
		}
		boards, skipped := excelSheetBoards(sheets)
		if len(boards) == 0 || len(boards[0].Rows) == 0 {
			return nil, nil, errors.New("error-csv-schema")
		}
		return boards, skipped, nil
	}
	text := string(data)
	if strings.TrimSpace(text) == "" {
		text = r.FormValue("text")
	}
	rows, err := csvRows(text)
	if err != nil || len(rows) == 0 {
		return nil, nil, errors.New("error-csv-schema")
	}
	return []sheetBoard{{Rows: rows}}, nil, nil
}

// importMappingPage is the second step: every field with the file's columns,
// chosen as the column names say.
func (s *Server) importMappingPage(w http.ResponseWriter, r *http.Request, u *User) {
	source := r.PathValue("source")
	_, jsonSource := importParsers[source]
	_, textSource := importTextParsers[source]
	if jsonSource || textSource {
		s.importGeneralPage(w, r, u, source)
		return
	}
	if _, ok := tableSources[source]; !ok {
		http.Error(w, "This import is not in Weddy yet.", http.StatusNotFound)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxImportForm+1<<20)
	data := s.importData(r, u, source)
	boards, skipped, err := importRows(r, source)
	if err != nil {
		data["Error"] = err.Error()
		s.render(w, r, "import", http.StatusBadRequest, data)
		return
	}
	title, rows := boards[0].Title, boards[0].Rows
	// csvMapping.js boardsText and skippedText: the boards a workbook holds,
	// when it holds several, and the sheets that are none. The boards after
	// the first go with the form, to be imported with it.
	if len(boards) > 1 {
		names := make([]string, len(boards))
		for i, b := range boards {
			names[i] = jsOr(b.Title, b.Sheet).(string)
		}
		more, _ := json.Marshal(boards[1:])
		data["BoardsText"], data["Sheets"] = strings.Join(names, ", "), string(more)
	}
	data["SkippedText"] = strings.Join(skipped, ", ")
	lang := languageOf(r, u).Tag
	header := rows[0]
	guess := guessTableMapping(header)
	columnText := func(i int) string {
		name := strings.TrimSpace(header[i])
		if name == "" {
			name = strings.ReplaceAll(tr(lang, "csv-mapping-column-number"), "__number__", strconv.Itoa(i+1))
		}
		for _, row := range rows[1:min(len(rows), 4)] {
			if i < len(row) && strings.TrimSpace(row[i]) != "" {
				sample := []rune(strings.TrimSpace(row[i]))
				if len(sample) > 40 {
					sample = append(sample[:40], '…')
				}
				return name + " (" + string(sample) + ")"
			}
		}
		return name
	}
	fields := []mappingField{}
	for _, f := range tableFields {
		mf := mappingField{Field: f.Field, Label: tr(lang, f.Label)}
		chosen, has := guess.Columns[f.Field]
		for i := range header {
			mf.Options = append(mf.Options, mappingOption{i, columnText(i), has && chosen == i})
		}
		fields = append(fields, mf)
	}
	used := map[int]bool{}
	for _, i := range guess.Columns {
		used[i] = true
	}
	extra := []mappingOption{}
	for i, h := range header {
		if !used[i] && !isCustomFieldHeader(h) {
			extra = append(extra, mappingOption{Index: i, Text: columnText(i)})
		}
	}
	raw, _ := json.Marshal(rows)
	_, hasList := guess.Columns["list"]
	data["Mapping"], data["Fields"], data["Extra"], data["Rows"] = true, fields, extra, string(raw)
	data["ListMissing"], data["ListName"], data["BoardTitle"] = !hasList, tr(lang, "scrum-category-todo"), title
	s.render(w, r, "import", http.StatusOK, data)
}

// importBoardPage makes the board of the confirmed mapping and opens it.
func (s *Server) importBoardPage(w http.ResponseWriter, r *http.Request, u *User) {
	source := r.PathValue("source")
	if _, ok := tableSources[source]; !ok {
		http.Error(w, "This import is not in Weddy yet.", http.StatusNotFound)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4*maxImportForm)
	data := s.importData(r, u, source)
	var rows [][]string
	if err := json.Unmarshal([]byte(r.FormValue("rows")), &rows); err != nil || len(rows) == 0 {
		data["Error"] = "error-csv-schema"
		s.render(w, r, "import", http.StatusBadRequest, data)
		return
	}
	m := tableMapping{Columns: map[string]int{}, ListName: strings.TrimSpace(r.FormValue("listName"))}
	for _, f := range tableFields {
		if v := r.FormValue("map-" + f.Field); v != "" {
			i, err := strconv.Atoi(v)
			if err != nil {
				http.Error(w, "The column mapping is not valid", http.StatusBadRequest)
				return
			}
			m.Columns[f.Field] = i
		}
	}
	for _, v := range r.Form["custom"] {
		i, err := strconv.Atoi(v)
		if err != nil {
			http.Error(w, "The column mapping is not valid", http.StatusBadRequest)
			return
		}
		// A column chosen for a field is that field, not a custom field too.
		if !slices.ContainsFunc(tableFields, func(f tableField) bool { c, ok := m.Columns[f.Field]; return ok && c == i }) {
			m.CustomFields = append(m.CustomFields, i)
		}
	}
	lang := languageOf(r, u).Tag
	_, hasList := m.Columns["list"]
	_, hasTitle := m.Columns["title"]
	switch {
	case !hasTitle:
		data["Error"] = msg("csv-mapping-title-required")
	case !hasList && m.ListName == "":
		data["Error"] = msg("csv-mapping-list-required")
	}
	if data["Error"] != nil {
		s.render(w, r, "import", http.StatusBadRequest, data)
		return
	}
	// A workbook's other boards (models/import.js): each its own board, with
	// the confirmed mapping when its header is the first board's, else by
	// its column names; the first board opens.
	parts := []sheetBoard{{Title: r.FormValue("boardTitle"), Rows: rows}}
	if more := r.FormValue("sheets"); source == "excel" && more != "" {
		var rest []sheetBoard
		if err := json.Unmarshal([]byte(more), &rest); err != nil {
			data["Error"] = "error-csv-schema"
			s.render(w, r, "import", http.StatusBadRequest, data)
			return
		}
		parts = append(parts, rest...)
	}
	var first *Board
	for i, part := range parts {
		pm := m
		if i > 0 && !sameSheetHeader(parts, i) {
			if len(part.Rows) == 0 {
				continue
			}
			pm = guessTableMapping(part.Rows[0])
		}
		b, err := s.importTable(r.Context(), u, part.Title, part.Rows, pm, tr(lang, "scrum-category-todo"))
		if errors.Is(err, errBoardCreation) {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		if err != nil {
			data["Error"] = err.Error()
			s.render(w, r, "import", http.StatusBadRequest, data)
			return
		}
		if first == nil {
			first = b
		}
	}
	http.Redirect(w, r, "/b/"+first.ID+"/"+first.Slug, http.StatusSeeOther)
}

// importIssueSources are the issue trackers WeKan's import page names, with
// the endpoint their JSON comes from.
var importIssueSources = map[string][2]string{"github": {"GitHub", "GET /repos/OWNER/REPO/issues"},
	"gitlab": {"GitLab", "GET /projects/ID/issues"}, "gitea": {"Gitea", "GET /repos/OWNER/REPO/issues"},
	"forgejo": {"Forgejo", "GET /repos/OWNER/REPO/issues"}}

// importInstruction is WeKan's importBoardInstructions: an issue tracker's
// with its name and endpoint, else the source's own.
func importInstruction(lang, source string) string {
	if is, ok := importIssueSources[source]; ok {
		return fillParams(tr(lang, "import-board-instruction-issues"), map[string]string{"sourceName": is[0], "endpoint": is[1]})
	}
	key := map[string]string{"csv": msg("import-board-instruction-csv"), "excel": msg("import-board-instruction-excel"),
		"kanboard": msg("import-board-instruction-kanboard"), "deck": msg("import-board-instruction-deck"),
		"asana": msg("import-board-instruction-asana"), "zenkit": msg("import-board-instruction-zenkit"),
		"openproject": msg("import-board-instruction-openproject"), "markdown": msg("import-board-instruction-markdown"),
		"todotxt": msg("import-board-instruction-todotxt"), "taskwarrior": msg("import-board-instruction-taskwarrior"),
		"nullboard": msg("import-board-instruction-nullboard"), "kanri": msg("import-board-instruction-kanri"),
		"opml": msg("import-board-instruction-opml"), "leo": msg("import-board-instruction-leo"),
		"obsidian": msg("import-board-instruction-obsidian"), "orgmode": msg("import-board-instruction-orgmode"),
		"todoist": msg("import-board-instruction-todoist"), "meistertask": msg("import-board-instruction-meistertask"),
		"linear": msg("import-board-instruction-linear"), "ticktick": msg("import-board-instruction-ticktick"),
		"clickup": msg("import-board-instruction-clickup"), "pivotal": msg("import-board-instruction-pivotal"),
		"redmine": msg("import-board-instruction-redmine"), "quire": msg("import-board-instruction-quire"),
		"focalboard": msg("import-board-instruction-focalboard"), "tasksorg": msg("import-board-instruction-tasksorg"),
		"superproductivity": msg("import-board-instruction-superproductivity"), "taiga": msg("import-board-instruction-taiga"),
		"vikunja": msg("import-board-instruction-vikunja"), "planner": msg("import-board-instruction-planner"),
		"monday": msg("import-board-instruction-monday"), "wrike": msg("import-board-instruction-wrike"),
		"teamwork": msg("import-board-instruction-teamwork"), "businessmap": msg("import-board-instruction-businessmap"),
		"notion": msg("import-board-instruction-notion"), "plane": msg("import-board-instruction-plane")}[source]
	return tr(lang, key)
}

var _ = msg("import-board-instruction-issues")

// importMembersModes are WeKan's people choices, "map" first and chosen, as
// its import page shows them.
var importMembersModes = []struct {
	Mode, Label string
	Checked     bool
}{
	{"map", msg("import-members-mode-map"), true},
	{"placeholder", msg("import-members-mode-placeholder"), false},
	{"me", msg("import-members-mode-me"), false},
}

var _ = msg("import-members-mode-heading")
