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
	m.HandleFunc("GET /api/export-all-boards/{format}", s.apiExportAll)
}

// massExportFormats are the formats Weddy writes so far; WeKan's others - Excel
// and the other tools' formats - are still to come.
var massExportFormats = map[string]string{"wekan": "json", "csv": "csv", "scsv": "csv", "tsv": "tsv"}

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
	if v := r.URL.Query().Get("boardIds"); v != "" {
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
	names := uniqueFileNames(titles, ext)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="wekan-boards-`+format+`.zip"`)
	w.Header().Set("Cache-Control", "no-store")
	zw := zip.NewWriter(w)
	defer zw.Close()
	for i := range ok2 {
		b := &ok2[i]
		f, err := zw.Create(names[i])
		if err != nil {
			continue
		}
		switch format {
		case "wekan":
			doc, err := s.exportBoard(ctx, b, r.URL.Query().Get("attachments") == "false")
			if err != nil {
				skipped = append(skipped, b.Title+" ("+b.ID+"): "+err.Error())
				continue
			}
			_ = json.NewEncoder(f).Encode(doc)
		default:
			delim := map[string]string{"csv": ",", "scsv": ";", "tsv": "\t"}[format]
			_, _ = f.Write(s.boardCSV(ctx, b, delim))
		}
	}
	if len(skipped) > 0 {
		if f, err := zw.Create("skipped.txt"); err == nil {
			_, _ = f.Write([]byte(strings.Join(skipped, "\n") + "\n"))
		}
	}
}
