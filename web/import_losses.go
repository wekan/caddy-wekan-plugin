package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// What an import could not bring over, as Node.js WeKan records it
// (models/lib/importLossReport.js, models/lib/importedCardChildren.js
// recordImportLosses): one "import-completed-with-warnings" row in the
// recoveryEvents collection Admin Panel -> Problems -> Recovery reads, its
// detail the count and the first 20 paths and reasons - locations and
// counts, never the file's values.

const (
	importWarningsEvent = "import-completed-with-warnings"
	importLossListed    = 20
	importLossText      = 200
)

var importLossSpaces = jsRegexp(`\s+`)

func importLossClip(v any) string {
	text := ""
	if v != nil && !isUndef(v) {
		text = jsStr(v)
	}
	text = jsTrim(importLossSpaces.ReplaceAllString(text, " "))
	if utf16Len(text) > importLossText {
		return jsSlice16(text, importLossText-1) + "…"
	}
	return text
}

// importLossReport is WeKan's: the detail line, and whether there is one.
func importLossReport(source string, warnings, unsupported []any) (string, bool) {
	type entry struct {
		kind       string
		path, text any
	}
	var entries []entry
	add := func(kind string, list []any) {
		for _, e := range list {
			if e == nil {
				continue
			}
			path, reason := jsGetOrUndef(e, "path"), jsGetOrUndef(e, "reason")
			if jsTruthy(path) || jsTruthy(reason) {
				entries = append(entries, entry{kind, path, reason})
			}
		}
	}
	add("not imported", unsupported)
	add("warning", warnings)
	if len(entries) == 0 {
		return "", false
	}
	notImported := 0
	for _, e := range entries {
		if e.kind == "not imported" {
			notImported++
		}
	}
	var listed []string
	for i, e := range entries {
		if i == importLossListed {
			break
		}
		listed = append(listed, jsTrim(importLossClip(e.path)+" "+importLossClip(e.text)))
	}
	detail := importLossClip(jsOr(source, "import")) + ": " + strconv.Itoa(notImported) + " not imported, " +
		strconv.Itoa(len(entries)-notImported) + " warning(s). " + strings.Join(listed, "; ")
	if more := len(entries) - len(listed); more > 0 {
		detail += "; and " + strconv.Itoa(more) + " more"
	}
	return detail, true
}

// recordImportLosses writes the import's report row, best effort: a report
// that cannot be written never fails the import it describes.
func (s *Server) recordImportLosses(ctx context.Context, source string, warnings, unsupported []any, boardID, boardTitle, userID string) {
	detail, ok := importLossReport(source, warnings, unsupported)
	if !ok {
		return
	}
	row := bson.M{"_id": randomID(), "type": importWarningsEvent, "detail": detail, "severity": "warning", // inserted, not a filter
		"source": "import:" + source, "done": true, "deletedData": false, "userId": userID, "createdAt": time.Now().UTC()}
	if boardID != "" {
		row["boardIds"] = bson.A{boardID}
	}
	if boardTitle != "" {
		row["boardTitles"] = bson.A{boardTitle}
	}
	_, _ = s.db.Collection("recoveryEvents").InsertOne(context.WithoutCancel(ctx), row)
}

// importReportWindow: the page asks right after its import finishes.
const importReportWindow = time.Hour

// importReportRows is WeKan's importReportForBoard: the loss reports this
// user's own import recorded for this board in the last hour, newest first,
// five at most - their detail and time, never another user's.
func (s *Server) importReportRows(ctx context.Context, u *User, boardID string) []jsObject {
	if u == nil || !validID(boardID) {
		return nil
	}
	var rows []bson.M
	opts := options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}}).SetLimit(5)
	cur, err := s.db.Collection("recoveryEvents").Find(ctx, bson.M{"type": importWarningsEvent, "userId": u.ID, "boardIds": eq(boardID),
		"createdAt": bson.M{"$gte": time.Now().Add(-importReportWindow)}}, opts)
	if err != nil {
		return nil
	}
	_ = cur.All(ctx, &rows)
	var out []jsObject
	for _, r := range rows {
		o := jsObject{}
		o.set("detail", strOf(r["detail"]))
		if t, ok := r["createdAt"].(bson.DateTime); ok {
			o.set("createdAt", jsDate(t.Time()))
		}
		out = append(out, o)
	}
	return out
}

// methodImportReportForBoard is WeKan's importReportForBoard method.
func (s *Server) methodImportReportForBoard(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	var boardID string
	if len(args) < 1 || json.Unmarshal(args[0], &boardID) != nil {
		return nil, &methodError{"Match.Error", "Expected string"}
	}
	rows := []any{}
	for _, row := range s.importReportRows(r.Context(), u, boardID) {
		rows = append(rows, row)
	}
	return rows, nil
}

var _ = []string{msg("import-report-heading"), msg("import-report-description"), msg("import-report-open-board")}

// importReportPage is WeKan's import page loss report: what the import
// could not bring over, and the board to open. With nothing to report, the
// board opens.
func (s *Server) importReportPage(w http.ResponseWriter, r *http.Request, u *User) {
	boardID := r.URL.Query().Get("board")
	var b Board
	// The rows are the user's own import's only (importReportRows); the
	// board is read for its address.
	if !validID(boardID) || s.db.Collection("boards").FindOne(r.Context(), bson.M{"_id": eq(boardID)}).Decode(&b) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	rows := s.importReportRows(r.Context(), u, boardID)
	if len(rows) == 0 {
		http.Redirect(w, r, "/b/"+b.ID+"/"+b.Slug, http.StatusSeeOther)
		return
	}
	var details []string
	for _, row := range rows {
		details = append(details, jsGet(row, "detail").(string))
	}
	data := s.importData(r, u, r.PathValue("source"))
	data["Report"] = map[string]any{"Rows": details, "BoardURL": "/b/" + b.ID + "/" + b.Slug}
	s.render(w, r, "import", http.StatusOK, data)
}
