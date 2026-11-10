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

// tableSources are the sources this page reads, with the files they take.
var tableSources = map[string]string{"csv": ".csv,.tsv,.txt", "excel": ".xlsx"}

func (s *Server) importPageRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /import/{source}", s.importable(s.page(s.importPage)))
	m.HandleFunc("POST /import/{source}", s.importable(s.page(s.importMappingPage)))
	m.HandleFunc("POST /import/{source}/board", s.importable(s.page(s.importBoardPage)))
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
				"taskwarrior": ".json,application/json,.ndjson,text/plain", "nullboard": ".nbx,.json,application/json"}[source]
		}
	}
	return map[string]any{"Title": tr(lang, "import-board-c"), "User": u, "Source": source, "Accept": accept, "Paste": paste, "General": general,
		"Instruction": importInstruction(lang, source)}
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

// importRows are the rows of the first step's file or text.
func importRows(r *http.Request, source string) (string, [][]string, error) {
	var data []byte
	if f, _, err := r.FormFile("file"); err == nil {
		defer f.Close()
		data, err = io.ReadAll(io.LimitReader(f, maxImportForm+1))
		if err != nil || len(data) > maxImportForm {
			return "", nil, errors.New("error-csv-schema")
		}
	}
	if source == "excel" {
		sheets, err := readXlsx(data)
		if err != nil {
			return "", nil, errors.New("error-csv-schema")
		}
		name, rows := firstTable(sheets)
		if len(rows) == 0 {
			return "", nil, errors.New("error-csv-schema")
		}
		if len(sheets) == 1 {
			name = ""
		}
		return name, rows, nil
	}
	text := string(data)
	if strings.TrimSpace(text) == "" {
		text = r.FormValue("text")
	}
	rows, err := csvRows(text)
	if err != nil || len(rows) == 0 {
		return "", nil, errors.New("error-csv-schema")
	}
	return "", rows, nil
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
	title, rows, err := importRows(r, source)
	if err != nil {
		data["Error"] = err.Error()
		s.render(w, r, "import", http.StatusBadRequest, data)
		return
	}
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
	b, err := s.importTable(r.Context(), u, r.FormValue("boardTitle"), rows, m, tr(lang, "scrum-category-todo"))
	if errors.Is(err, errBoardCreation) {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if err != nil {
		data["Error"] = err.Error()
		s.render(w, r, "import", http.StatusBadRequest, data)
		return
	}
	http.Redirect(w, r, "/b/"+b.ID+"/"+b.Slug, http.StatusSeeOther)
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
		"nullboard": msg("import-board-instruction-nullboard"), "kanri": msg("import-board-instruction-kanri")}[source]
	return tr(lang, key)
}

var _ = msg("import-board-instruction-issues")
