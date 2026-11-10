package web

import (
	"context"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
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
