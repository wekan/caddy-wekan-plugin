package web

import (
	"context"
	"encoding/csv"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// A board from a table - a CSV/TSV text or an Excel sheet - as Node.js WeKan's
// models/lib/csvImportMapping.js plans it and models/csvCreator.js writes it:
// rows[0] is the header, and a mapping says which column is which card field.
// The import page guesses the mapping from the column names (the English
// names other tools use, and the names WeKan's own exports write in every
// language), the person confirms it, and the server checks it again.

type tableField struct{ Field, Label, ExportKey string }

// tableFields are CSV_IMPORT_FIELDS: the card fields, in the order the
// mapping step shows them.
var tableFields = []tableField{
	{"title", msg("title"), "title"}, {"description", msg("description"), "description"}, {"list", msg("list"), "list"},
	{"swimlane", msg("swimlane"), "swimlane"}, {"owner", msg("owner"), "owner"}, {"members", msg("members"), "members"},
	{"assignees", msg("assignee"), "assignee"}, {"labels", msg("labels"), "labels"},
	{"receivedAt", msg("card-received"), "card-received"}, {"startAt", msg("card-start"), "card-start"},
	{"dueAt", msg("card-due"), "card-due"}, {"endAt", msg("card-end"), "card-end"},
	{"createdAt", msg("createdAt"), "createdAt"}, {"modifiedAt", msg("last-modified-at"), "last-modified-at"},
	{"requestedBy", msg("requested-by"), "requested-by"}, {"assignedBy", msg("assigned-by"), "assigned-by"},
	{"parentCard", msg("parent-card"), "parent-card"}, {"spentTime", msg("spent-time-hours"), "spent-time-hours"},
	{"isOvertime", msg("overtime-hours"), "overtime-hours"}, {"archived", msg("archived"), "archived"},
}

// tableAliases are the English header names of other tools, lowercase.
var tableAliases = map[string][]string{
	"title":       {"title", "name", "card", "card title", "card name", "task", "task name", "summary", "subject"},
	"description": {"description", "desc", "details", "notes", "body"},
	"list":        {"list", "list name", "stage", "status", "state", "column"},
	"swimlane":    {"swimlane", "lane"}, "owner": {"owner", "creator", "created by", "author"},
	"members": {"members", "member"}, "assignees": {"assignee", "assignees", "assigned to"},
	"labels": {"labels", "label", "tags", "tag"}, "receivedAt": {"received", "received at", "received date"},
	"startAt": {"start", "start date", "start at"}, "dueAt": {"due", "due date", "due at", "deadline"},
	"endAt":       {"end", "end date", "end at", "finish date", "finished at"},
	"createdAt":   {"created at", "creation date", "created"},
	"modifiedAt":  {"modified at", "updated at", "update date", "modified on", "last modified at"},
	"requestedBy": {"requested by", "requester"}, "assignedBy": {"assigned by", "assigner"},
	"parentCard": {"parent card", "parent"}, "spentTime": {"spent time", "spent time (hours)"},
	"isOvertime": {"overtime", "overtime (hours)"}, "archived": {"archived"},
}

const maxTableColumns = 10000

// tableMapping is a mapping: field -> column, the extra columns imported as
// text custom fields, and the one list for a file with no list column.
type tableMapping struct {
	Columns      map[string]int
	CustomFields []int
	ListName     string
}

func normalizeHeader(s string) string {
	// JavaScript's \s and trim(): Unicode's spaces too, a no-break space
	// among them.
	var b strings.Builder
	space := false
	for _, r := range s {
		if jsSpace(r) {
			if !space {
				b.WriteByte(' ')
			}
			space = true
			continue
		}
		space = false
		b.WriteRune(r)
	}
	return jsLower(jsTrim(strings.TrimSuffix(jsTrim(b.String()), ":")))
}

func isCustomFieldHeader(s string) bool { return strings.HasPrefix(normalizeHeader(s), "customfield") }

// tableCSVExportLayout and tableExcelExportLayout are CSV_EXPORT_LAYOUT and
// EXCEL_EXPORT_LAYOUT: the column order of WeKan's own CSV/TSV export
// (models/exporter.js) and Excel table (models/server/ExporterExcel.js).
var tableCSVExportLayout = []string{"title", "description", "list", "swimlane", "owner", "requested-by",
	"assigned-by", "members", "assignee", "labels", "card-start", "card-due", "card-end", "overtime-hours",
	"spent-time-hours", "createdAt", "last-modified-at", "last-activity", "voting", "archived"}
var tableExcelExportLayout = []string{"number", "title", "description", "parent-card", "owner", "createdAt",
	"last-modified-at", "card-received", "card-start", "card-due", "card-end", "list", "swimlane", "assignee",
	"members", "requested-by", "assigned-by", "labels", "overtime-hours", "spent-time-hours"}

// tableFieldByExportKey is fieldByExportKey.
func tableFieldByExportKey(key string) string {
	for _, f := range tableFields {
		if f.ExportKey == key {
			return f.Field
		}
	}
	return ""
}

// bestHeaderLanguage is bestLanguage: the language whose export headers
// match the most names, English winning a tie; -1 is none.
func bestHeaderLanguage(names []string, index []headerLanguage) int {
	best, bestCount := -1, 0
	for i, hl := range index {
		count := 0
		for _, n := range names {
			if _, ok := hl.names[n]; ok && n != "" {
				count++
			}
		}
		if count > bestCount || (count == bestCount && count > 0 && hl.tag == "en") {
			best, bestCount = i, count
		}
	}
	return best
}

// tableExportLayoutOf is exportLayoutOf: a header that is exactly one of
// WeKan's export layouts in one language is read by position - some
// languages translate two of its keys with one word ("Owner" and "Assigned
// By" in Latvian), and their places say which is which.
func tableExportLayoutOf(names []string, lang int, index []headerLanguage) map[string]int {
	if lang < 0 {
		return nil
	}
	m := index[lang].names
	for _, layout := range [][]string{tableCSVExportLayout, tableExcelExportLayout} {
		if len(names) < len(layout) {
			continue
		}
		fits := true
		for i, key := range layout {
			if !slices.Contains(m[names[i]], key) {
				fits = false
				break
			}
		}
		for _, n := range names[len(layout):] {
			if n != "" && !isCustomFieldHeader(n) {
				fits = false
			}
		}
		if !fits {
			continue
		}
		columns := map[string]int{}
		for i, key := range layout {
			if f := tableFieldByExportKey(key); f != "" {
				columns[f] = i
			}
		}
		return columns
	}
	return nil
}

// guessTableMapping is guessCsvMapping: WeKan's own export layout by
// position, else a column for each field its header names - in the file's
// own language first, then the English names, then any language - and, with
// no title column found, the first ordinary column no language knows.
func guessTableMapping(header []string) tableMapping {
	index := headerIndex()
	names := make([]string, len(header))
	for i, h := range header {
		names[i] = normalizeHeader(h)
	}
	lang := bestHeaderLanguage(names, index)
	if layout := tableExportLayoutOf(names, lang, index); layout != nil {
		return tableMapping{Columns: layout}
	}
	candidates := func(n string) []string {
		out := []string{}
		add := func(f string) {
			if f != "" && !slices.Contains(out, f) {
				out = append(out, f)
			}
		}
		if lang >= 0 {
			for _, k := range index[lang].names[n] {
				add(tableFieldByExportKey(k))
			}
		}
		add(tableAliasField(n))
		for _, hl := range index {
			for _, k := range hl.names[n] {
				add(tableFieldByExportKey(k))
			}
		}
		return out
	}
	// knownHeader: an English name, or any language's export header.
	known := func(n string) bool {
		if tableAliasField(n) != "" {
			return true
		}
		return slices.ContainsFunc(index, func(hl headerLanguage) bool { _, ok := hl.names[n]; return ok })
	}
	m := tableMapping{Columns: map[string]int{}}
	taken := map[string]bool{}
	for i, n := range names {
		if n == "" || isCustomFieldHeader(n) {
			continue
		}
		for _, f := range candidates(n) {
			if !taken[f] {
				m.Columns[f], taken[f] = i, true
				break
			}
		}
	}
	if _, ok := m.Columns["title"]; !ok {
		used := map[int]bool{}
		for _, i := range m.Columns {
			used[i] = true
		}
		for i, n := range names {
			if n != "" && !used[i] && !isCustomFieldHeader(n) && !known(n) {
				m.Columns["title"] = i
				break
			}
		}
	}
	return m
}

// checkTableMapping is validateCsvMapping: every column in the file, a
// custom field column chosen once and not a card field.
func checkTableMapping(m tableMapping, headerLen int) error {
	used := map[int]bool{}
	for f, i := range m.Columns {
		if !slices.ContainsFunc(tableFields, func(x tableField) bool { return x.Field == f }) {
			return errors.New("The column mapping is not valid")
		}
		if i < 0 || i >= headerLen || i >= maxTableColumns {
			return errors.New("The file has no column " + strconv.Itoa(i+1) + " for " + f)
		}
		used[i] = true
	}
	seen := map[int]bool{}
	for _, i := range m.CustomFields {
		if i < 0 || i >= headerLen {
			return errors.New("The file has no column " + strconv.Itoa(i+1))
		}
		if seen[i] || used[i] {
			return errors.New("Column " + strconv.Itoa(i+1) + " is chosen twice")
		}
		seen[i] = true
	}
	if len(m.ListName) > 255 {
		return errors.New("The list name is too long")
	}
	return nil
}

// csvSeparatorOf is the separator of a text, from its first line: a tab when
// it has one, a semicolon when it has more of them than commas, else a comma.
func csvSeparatorOf(text string) rune {
	first, _, _ := strings.Cut(strings.TrimPrefix(text, "\uFEFF"), "\n")
	switch {
	case strings.Contains(first, "\t"):
		return '\t'
	case strings.Count(first, ";") > strings.Count(first, ","):
		return ';'
	}
	return ','
}

// csvRows is a CSV/TSV text as rows, its empty lines skipped.
func csvRows(text string) ([][]string, error) {
	text = strings.TrimPrefix(text, "\uFEFF")
	r := csv.NewReader(strings.NewReader(text))
	r.Comma, r.FieldsPerRecord, r.LazyQuotes = csvSeparatorOf(text), -1, true
	all, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	rows := [][]string{}
	for _, row := range all {
		if slices.ContainsFunc(row, func(c string) bool { return strings.TrimSpace(c) != "" }) {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

// splitLabels: WeKan's CSV writes name-color separated by spaces, its Excel
// table names separated by commas.
func splitLabels(v string) []Label {
	out := []Label{}
	for _, tok := range regexp.MustCompile(`[\s,]+`).Split(v, -1) {
		if tok == "" {
			continue
		}
		l := Label{Name: tok, Color: "black"}
		if dash := strings.LastIndex(tok, "-"); dash > 0 && regexp.MustCompile(`^[a-z]+$`).MatchString(tok[dash+1:]) {
			l = Label{Name: tok[:dash], Color: tok[dash+1:]}
		}
		if !slices.ContainsFunc(out, func(x Label) bool { return x.Name == l.Name && x.Color == l.Color }) {
			out = append(out, l)
		}
	}
	return out
}

// cellDate is a date cell: ISO 8601, or a date and time as spreadsheets write.
func cellDate(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02", "1/2/2006", "1/2/2006 15:04"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

var truthyCell = regexp.MustCompile(`(?i)^(true|yes|1|x)$`)

// jsDateString is JavaScript's String(new Date()) on a server at UTC, as
// WeKan's runs.
func jsDateString(t time.Time) string {
	return t.UTC().Format("Mon Jan 02 2006 15:04:05") + " GMT+0000 (Coordinated Universal Time)"
}

// importTable makes the board: planCsvBoard's lists, swimlanes, labels and
// custom fields, and a card a row, for u, who may make boards. People are
// kept only when a name is the importer's own, as WeKan does with no member
// mapping: a name is never looked up among all users.
func (s *Server) importTable(ctx context.Context, u *User, title string, rows [][]string, m tableMapping, defaultList string) (*Board, error) {
	if len(rows) == 0 {
		return nil, errors.New("error-csv-schema")
	}
	header := rows[0]
	if err := checkTableMapping(m, len(header)); err != nil {
		return nil, err
	}
	// No title column is the page's to refuse (csv-mapping-title-required);
	// WeKan's planCsvBoard makes the cards untitled.
	if err := s.mayCreateBoard(ctx, u, "board", "import:csv"); err != nil {
		return nil, err
	}
	// CsvCreator's `Imported Board ${new Date()}`.
	if title == "" {
		title = "Imported Board " + jsDateString(time.Now())
	}
	b, err := s.insertBoard(ctx, u, title, "board", s.boardPermission(ctx, "private", "import:csv"))
	if err != nil {
		return nil, err
	}
	at := func(row []string, field string) string {
		if i, ok := m.Columns[field]; ok && i < len(row) {
			return row[i]
		}
		return ""
	}
	fallback := strings.TrimSpace(m.ListName)
	if fallback == "" {
		fallback = defaultList
	}
	now := time.Now().UTC()
	lists, swimlanes := map[string]string{}, map[string]string{}
	listOf := func(name string) string {
		if id, ok := lists[name]; ok {
			return id
		}
		id := randomID()
		_, _ = s.db.Collection("lists").InsertOne(ctx, bson.M{"_id": id, "title": name, "boardId": b.ID, "sort": float64(len(lists)), // inserted, not a filter
			"archived": false, "createdAt": now, "type": "list", "swimlaneId": ""}) // inserted, not a filter
		lists[name] = id
		return id
	}
	swimlaneOf := func(name string) string {
		if id, ok := swimlanes[name]; ok {
			return id
		}
		id := randomID()
		_, _ = s.db.Collection("swimlanes").InsertOne(ctx, bson.M{"_id": id, "title": name, "boardId": b.ID, "sort": float64(len(swimlanes)), // inserted, not a filter
			"archived": false, "createdAt": now, "type": "swimlane"}) // inserted, not a filter
		swimlanes[name] = id
		return id
	}
	// The custom fields: WeKan's CustomField-NAME-TYPE columns, and the
	// columns chosen as text fields.
	type cf struct {
		id, typ string
		pos     int
	}
	used := map[int]bool{}
	for _, i := range m.Columns {
		used[i] = true
	}
	fields := []cf{}
	addField := func(name, typ string, pos int) {
		id := randomID()
		_, _ = s.db.Collection("customFields").InsertOne(ctx, bson.M{"_id": id, "boardIds": bson.A{b.ID}, "name": name, "type": typ, // inserted, not a filter
			"settings": bson.M{}, "showOnCard": false, "automaticallyOnCard": false, "showLabelOnMiniCard": false, "createdAt": now}) // inserted, not a filter
		fields = append(fields, cf{id, typ, pos})
	}
	for pos, h := range header {
		if used[pos] || !isCustomFieldHeader(h) {
			continue
		}
		parts := strings.Split(strings.TrimSpace(h), "-")
		name, typ := strings.TrimSpace(h), "text"
		if len(parts) > 1 && parts[1] != "" {
			name = parts[1]
		}
		if len(parts) > 2 && slices.Contains([]string{"text", "number", "date", "checkbox", "currency", "stringtemplate"}, parts[2]) {
			typ = parts[2]
		}
		addField(name, typ, pos)
	}
	for _, pos := range m.CustomFields {
		name := strings.TrimSpace(header[pos])
		if name == "" {
			name = "#" + strconv.Itoa(pos+1)
		}
		addField(name, "text", pos)
	}
	labels := b.Labels
	labelOf := func(l Label) string {
		for _, x := range labels {
			if x.Name == l.Name && x.Color == l.Color {
				return x.ID
			}
		}
		x := Label{ID: randomString(idAlphabet, 6), Name: l.Name, Color: l.Color}
		labels = append(labels, x)
		return x.ID
	}
	self := func(v string) []string {
		out := []string{}
		for _, n := range regexp.MustCompile(`[\s,;]+`).Split(v, -1) {
			if n != "" && n == u.Username && !slices.Contains(out, u.ID) {
				out = append(out, u.ID)
			}
		}
		return out
	}
	for r, row := range rows[1:] {
		card := bson.M{"_id": randomID(), "title": at(row, "title"), "boardId": b.ID, // inserted, not a filter
			"listId": listOf(func() string {
				if v := strings.TrimSpace(at(row, "list")); v != "" {
					return v
				}
				return fallback
			}()),
			"swimlaneId": swimlaneOf(func() string {
				if v := strings.TrimSpace(at(row, "swimlane")); v != "" {
					return v
				}
				return "Default"
			}()),
			"sort": float64(r), "archived": truthyCell.MatchString(strings.TrimSpace(at(row, "archived"))),
			"userId": u.ID, "members": self(at(row, "members")), "assignees": self(at(row, "assignees")),
			"type": "cardType-card", "createdAt": now, "modifiedAt": now, "dateLastActivity": now}
		if v := at(row, "description"); strings.TrimSpace(v) != "" {
			card["description"] = v
		}
		for _, f := range []string{"receivedAt", "startAt", "dueAt", "endAt", "createdAt", "modifiedAt"} {
			if t, ok := cellDate(at(row, f)); ok {
				card[f] = t
			}
		}
		for _, f := range []string{"requestedBy", "assignedBy"} {
			if v := strings.TrimSpace(at(row, f)); v != "" {
				card[f] = v
			}
		}
		if spent, err := strconv.ParseFloat(strings.Replace(strings.TrimSpace(at(row, "spentTime")), ",", ".", 1), 64); err == nil {
			card["spentTime"] = spent
		}
		if truthyCell.MatchString(strings.TrimSpace(at(row, "isOvertime"))) {
			card["isOvertime"] = true
		}
		ids := bson.A{}
		for _, l := range splitLabels(at(row, "labels")) {
			ids = append(ids, labelOf(l))
		}
		card["labelIds"] = ids
		values := bson.A{}
		for _, f := range fields {
			if f.pos >= len(row) || strings.TrimSpace(row[f.pos]) == "" {
				continue
			}
			var v any = strings.TrimSpace(row[f.pos])
			if f.typ == "date" {
				if t, ok := cellDate(row[f.pos]); ok {
					v = t
				}
			}
			values = append(values, bson.M{"_id": f.id, "value": v}) // inserted, not a filter
		}
		card["customFields"] = values
		if _, err := s.db.Collection("cards").InsertOne(ctx, card); err != nil {
			return nil, err
		}
	}
	if len(rows) == 1 {
		listOf(fallback)
		swimlaneOf("Default")
	}
	if len(labels) != len(b.Labels) {
		_, _ = s.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$set": bson.M{"labels": labels}})
		b.Labels = labels
	}
	s.activity(ctx, u.ID, "importBoard", bson.M{"type": "board", "activityTypeId": b.ID, "boardId": b.ID, "source": "csv"})
	return b, nil
}
