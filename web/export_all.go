package web

import (
	"archive/zip"
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// "Export all boards", as Node.js WeKan (server/routes/exportAllBoards.js):
// every board the user is an active member of - not archived, not a template -
// that they may export, in one format, as a .zip of a file a board. A board
// they may not export is named in skipped.txt.
func (s *Server) exportAllRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/export-all-boards/{format}", s.exportable(s.apiExportAll))
}

// massExportFormats lists the formats available for all-board downloads.
var massExportFormats = func() map[string]string {
	m := map[string]string{"excel": "xlsx", "wekan": "json", "csv": "csv", "scsv": "csv", "tsv": "tsv"}
	for f, ext := range externalFormats {
		if f != "trellozip" {
			m[f] = ext[0]
		}
	}
	return m
}()

var exportFormatsForMenu = func() []exportTool {
	formats := []exportTool{{"excel", "Excel (.xlsx)", "fa-table"}, {"wekan", "WeKan JSON", "fa-download"},
		{"csv", "CSV", "fa-table"}, {"scsv", "CSV (;)", "fa-table"}, {"tsv", "TSV", "fa-table"}}
	for _, tool := range exportTools {
		if _, ok := massExportFormats[tool.Key]; ok {
			formats = append(formats, tool)
		}
	}
	return formats
}()

const maxMassExportBoards = 1000

var (
	massBoardID   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	fileNameRun   = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)
	dashRun       = regexp.MustCompile(`-+`)
	edgeDashesDot = regexp.MustCompile(`^[-.]+|[-.]+$`)
)

// uniqueFileNames is WeKan's: a name of each title, the second of a name -2.
func uniqueFileNames(titles []string, ext string) []string {
	used := map[string]bool{}
	out := make([]string, len(titles))
	for i, t := range titles {
		base := edgeDashesDot.ReplaceAllString(dashRun.ReplaceAllString(fileNameRun.ReplaceAllString(t, "-"), "-"), "")
		if len(base) > 80 {
			base = base[:80]
		}
		if base == "" {
			base = "board"
		}
		name := base + "." + ext
		for n := 2; used[strings.ToLower(name)]; n++ {
			name = base + "-" + strconv.Itoa(n) + "." + ext
		}
		used[strings.ToLower(name)] = true
		out[i] = name
	}
	return out
}

func (s *Server) apiExportAll(w http.ResponseWriter, r *http.Request) {
	format := r.PathValue("format")
	ext, known := massExportFormats[format]
	if !known {
		apiError(w, http.StatusNotFound, "Unknown export format")
		return
	}
	u, ok := s.exportUser(r)
	if !ok || u == nil {
		apiError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}
	ctx := r.Context()
	filter := bson.M{"archived": false, "type": "board", "members": bson.M{"$elemMatch": bson.M{"userId": u.ID, "isActive": true}}}
	query := r.URL.Query()
	if v := query.Get("boardIds"); v != "" {
		ids := []string{}
		for _, id := range strings.Split(v, ",") {
			if id = strings.TrimSpace(id); massBoardID.MatchString(id) && !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
		in, _ := inIDs(ids)
		filter["_id"] = in
	}
	cur, err := s.db.Collection("boards").Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "title", Value: 1}}).SetLimit(maxMassExportBoards))
	if err != nil {
		apiError(w, http.StatusInternalServerError, "Export failed")
		return
	}
	var boards []Board
	_ = cur.All(ctx, &boards)
	ok2, skipped := []Board{}, []string{}
	for _, b := range boards {
		if m := b.member(u.ID); m == nil || m.AssignedOnly() {
			skipped = append(skipped, b.Title+" ("+b.ID+"): not exportable by this user")
			continue
		}
		// assertFieldExport: left out and named in skipped.txt (exportAllBoards.js).
		if !s.fieldExportAllowed(ctx, &b, u) {
			skipped = append(skipped, b.Title+" ("+b.ID+"): "+errFieldExport)
			continue
		}
		ok2 = append(ok2, b)
	}
	if len(ok2) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "No board to export", "skipped": skipped})
		return
	}
	titles := make([]string, len(ok2))
	for i, b := range ok2 {
		titles[i] = b.Title
	}
	if format == "excel" {
		sheets := make([]xlsxSpec, 0, len(ok2))
		names := massSheetNames(titles)
		for i := range ok2 {
			sheet := s.excelTableSheets(ctx, &ok2[i], exportLang(ctx), exportScope{}, false)[0]
			sheet.Name = names[i]
			sheets = append(sheets, sheet)
		}
		raw, err := xlsxBook(sheets)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "Export failed")
			return
		}
		w.Header().Set("Content-Type", xlsxType)
		w.Header().Set("Content-Disposition", `attachment; filename="wekan-boards.xlsx"`)
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(raw)
		return
	}
	names := uniqueFileNames(titles, ext)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="wekan-boards-`+format+`.zip"`)
	w.Header().Set("Cache-Control", "no-store")
	zw := zip.NewWriter(w)
	defer zw.Close()
	want := exportWanted(query.Get("fields"))
	for i := range ok2 {
		b := &ok2[i]
		var data []byte
		switch format {
		case "wekan":
			doc, err := s.exportBoardWith(ctx, b, exportDocOptions{
				WithoutFiles: query.Get("attachments") == "false",
				Want:         want,
				r:            r,
			})
			if err == nil {
				data, err = json.Marshal(jsDates(doc))
			}
			if err != nil {
				skipped = append(skipped, b.Title+" ("+b.ID+"): Export failed")
				continue
			}
		case "csv", "scsv", "tsv":
			delim := map[string]string{"csv": ",", "scsv": ";", "tsv": "\t"}[format]
			data = s.boardCSV(ctx, b, delim, want)
		default:
			data = s.externalExport(ctx, b, u, format, want)
		}
		f, err := zw.Create(names[i])
		if err != nil {
			return
		}
		if _, err = f.Write(data); err != nil {
			return
		}
	}
	if len(skipped) > 0 {
		if f, err := zw.Create("skipped.txt"); err == nil {
			_, _ = f.Write([]byte(strings.Join(skipped, "\n") + "\n"))
		}
	}
}

func massSheetNames(titles []string) []string {
	used := map[string]bool{}
	names := make([]string, len(titles))
	for i, title := range titles {
		clean := strings.Map(func(r rune) rune {
			if strings.ContainsRune(`\/?*[]:`, r) {
				return ' '
			}
			return r
		}, title)
		base := jsTrim(strings.Trim(jsTrim(mdSpace.ReplaceAllString(clean, " ")), "'"))
		base = jsTrim(jsSlice16(base, 31))
		if base == "" {
			base = "Board"
		}
		name := base
		for n := 2; used[strings.ToLower(name)]; n++ {
			suffix := " (" + strconv.Itoa(n) + ")"
			name = jsTrim(jsSlice16(base, 31-len(suffix))) + suffix
		}
		used[strings.ToLower(name)] = true
		names[i] = name
	}
	return names
}
