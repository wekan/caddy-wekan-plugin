package web

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// The board as CSV or TSV, as Node.js WeKan (models/export.js export/csv,
// models/exporter.js buildCsvStream, models/lib/exporterCsvRow.js): one row a
// card, every cell quoted, a formula never left to run in a spreadsheet.
func (s *Server) exportCSVRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/boards/{board}/export/csv", s.exportable(s.apiExportCSV))
}

// csvFormula is Papa Parse's escapeFormulae: a cell a spreadsheet would run.
var csvFormula = regexp.MustCompile(`^[=+\-@\t\r]`)

// csvCell is Papa.unparse's cell: null empty, text formula-escaped, numbers and
// booleans as JavaScript writes them, a date as ISO with milliseconds.
func csvCell(v any) string {
	var str string
	switch x := v.(type) {
	case nil:
		return `""`
	case string:
		str = x
		if csvFormula.MatchString(str) {
			str = "'" + str
		}
	case bool:
		str = strconv.FormatBool(x)
	case float64:
		str = strconv.FormatFloat(x, 'f', -1, 64)
	case int32, int64, int:
		str = fmt.Sprint(x)
	case time.Time:
		str = isoMillis(x)
	case bson.DateTime:
		str = isoMillis(x.Time())
	default:
		str = fmt.Sprint(x)
	}
	return `"` + strings.ReplaceAll(str, `"`, `""`) + `"`
}

func isoMillis(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// isoOrBlank is a date cell: ISO, or the blank " " WeKan writes for none.
func isoOrBlank(v any) any {
	switch x := v.(type) {
	case time.Time:
		return isoMillis(x)
	case bson.DateTime:
		return isoMillis(x.Time())
	}
	return " "
}

func lookupProp(docs []bson.M, id any, prop string) string {
	for _, d := range docs {
		if d["_id"] == id {
			if s, ok := d[prop].(string); ok {
				return s
			}
			return ""
		}
	}
	return ""
}

// csvRow is WeKan's buildCsvCardRow: a missing list, user or label is a blank
// cell, never a failed export (#5604).
func csvRow(card bson.M, lists, swimlanes, users, labels, fields []bson.M, position map[string]int) []any {
	str := func(v any) string { s, _ := v.(string); return s }
	names := func(v any) []string {
		out := []string{}
		for _, id := range stringsOf(v) {
			if n := lookupProp(users, id, "username"); n != "" {
				out = append(out, n)
			}
		}
		return out
	}
	identity := func(ids any, text any) string {
		parts := names(ids)
		if t := str(text); t != "" {
			parts = append(parts, t)
		}
		if len(parts) == 0 {
			return " "
		}
		return strings.Join(parts, ", ")
	}
	row := []any{card["title"], card["description"], lookupProp(lists, card["listId"], "title"),
		lookupProp(swimlanes, card["swimlaneId"], "title"), lookupProp(users, card["userId"], "username"),
		identity(card["requesters"], card["requestedBy"]), identity(card["assigners"], card["assignedBy"]),
		strings.Join(names(card["members"]), " "), strings.Join(names(card["assignees"]), " ")}
	ls := []string{}
	for _, id := range stringsOf(card["labelIds"]) {
		for _, l := range labels {
			if l["_id"] == id {
				ls = append(ls, str(l["name"])+"-"+str(l["color"]))
			}
		}
	}
	row = append(row, strings.Join(ls, " "), isoOrBlank(card["startAt"]), isoOrBlank(card["dueAt"]), isoOrBlank(card["endAt"]),
		strconv.FormatBool(card["isOvertime"] == true), card["spentTime"], isoOrBlank(card["createdAt"]),
		isoOrBlank(card["modifiedAt"]), isoOrBlank(card["dateLastActivity"]))
	vote := " "
	if v, ok := card["vote"].(bson.D); ok {
		m := docMap(v)
		if q := str(m["question"]); q != "" {
			pos, neg := names(m["positive"]), names(m["negative"])
			np, nn := len(stringsOf(m["positive"])), len(stringsOf(m["negative"]))
			if m["public"] == true {
				vote = fmt.Sprintf("%s-yes-%d-%s-no-%d-%s", q, np, strings.Join(pos, " "), nn, strings.Join(neg, " "))
			} else {
				vote = fmt.Sprintf("%s-yes-%d-no-%d", q, np, nn)
			}
		}
	}
	row = append(row, vote, strconv.FormatBool(card["archived"] == true))
	values := make([]any, len(fields))
	set := make([]bool, len(fields))
	if arr, ok := card["customFields"].(bson.A); ok {
		for _, e := range arr {
			d, _ := e.(bson.D)
			f := docMap(d)
			id, _ := f["_id"].(string)
			at, known := position[id]
			if f["value"] == nil || !known {
				continue
			}
			field := fields[at]
			set[at] = true
			switch field["type"] {
			case "date":
				values[at] = isoOrBlank(f["value"])
				if s, ok := f["value"].(string); ok {
					if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
						values[at] = isoMillis(t)
					}
				}
			case "dropdown", "dropdownMultiSelect":
				opts := dropdownItems(field)
				ids := stringsOf(f["value"])
				if s, ok := f["value"].(string); ok {
					ids = []string{s}
				}
				got := []string{}
				for _, id := range ids {
					name := opts[id]
					if name == "" && field["type"] == "dropdownMultiSelect" {
						name = id
					}
					if name != "" {
						got = append(got, name)
					}
				}
				if len(got) > 0 {
					values[at] = strings.Join(got, ", ")
				}
			default:
				values[at] = f["value"]
			}
		}
	}
	for i := range values {
		if set[i] {
			row = append(row, values[i])
		} else {
			row = append(row, " ")
		}
	}
	return row
}

func dropdownItems(field bson.M) map[string]string {
	out := map[string]string{}
	settings, _ := field["settings"].(bson.D)
	items, _ := docMap(settings)["dropdownItems"].(bson.A)
	for _, e := range items {
		d, _ := e.(bson.D)
		m := docMap(d)
		id, _ := m["_id"].(string)
		name, _ := m["name"].(string)
		out[id] = name
	}
	return out
}

func (s *Server) apiExportCSV(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("board")
	var b Board
	if !validID(id) || s.db.Collection("boards").FindOne(r.Context(), bson.M{"_id": eq(id)}).Decode(&b) != nil {
		http.Error(w, "Board not found", http.StatusNotFound)
		return
	}
	u, ok := s.exportUser(r)
	if !ok {
		http.Error(w, "Invalid token", http.StatusUnauthorized)
		return
	}
	if b.Permission != "public" {
		if u == nil {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		if m := b.member(u.ID); m == nil || m.AssignedOnly() {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
	}
	// Papa Parse's delimiter: one character, never a quote or a line break.
	delim := r.URL.Query().Get("delimiter")
	if utf8.RuneCountInString(delim) != 1 || strings.ContainsAny(delim, "\r\n\"\uFEFF") {
		delim = ","
	}
	if delim == "\t" {
		w.Header().Set("Content-Type", "text/tsv")
	} else {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if delim != "\t" {
		_, _ = w.Write([]byte{0xEF, 0xBB, 0xBF}) // a BOM, so a spreadsheet reads UTF-8
	}
	_, _ = w.Write(s.boardCSV(r.Context(), &b, delim, exportWanted(r.URL.Query().Get("fields"))))
}

// csvColumnKeys are the untranslated keys of the CSV header, in its order.
var csvColumnKeys = []string{"title", "description", "list", "swimlane", "owner", "requested-by", "assigned-by",
	"members", "assignee", "labels", "card-start", "card-due", "card-end", "overtime-hours", "spent-time-hours",
	"createdAt", "last-modified-at", "last-activity", "voting", "archived"}

// csvColumnParts is CSV_COLUMN_PARTS (models/lib/exportFields.js): the
// section of the Export pop-over each column belongs to. A column not named
// - the title, archived - is what a row is, and always exported.
var csvColumnParts = map[string]string{"description": "description", "list": "board-info", "swimlane": "board-info",
	"owner": "people", "requested-by": "people", "assigned-by": "people", "members": "people", "assignee": "people",
	"labels": "labels", "card-start": "dates", "card-due": "dates", "card-end": "dates", "overtime-hours": "dates",
	"spent-time-hours": "dates", "createdAt": "dates", "last-modified-at": "dates", "last-activity": "dates",
	"voting": "voting"}

// csvColumnMask is csvColumnMask: the columns that stay, the custom fields'
// after the fixed ones.
func csvColumnMask(columns int, want exportWant) []bool {
	mask := make([]bool, columns)
	for i := range mask {
		if i < len(csvColumnKeys) {
			part, named := csvColumnParts[csvColumnKeys[i]]
			mask[i] = !named || want.has(part)
		} else {
			mask[i] = want.has("custom-fields")
		}
	}
	return mask
}

// boardCSV is the board's rows - the header, then a card a row - with
// delim, the columns of the sections want leaves out dropped.
func (s *Server) boardCSV(ctx context.Context, b *Board, delim string, want exportWant) []byte {
	var out strings.Builder
	var mask []bool
	for n, cells := range s.boardRows(ctx, b) {
		if n == 0 {
			mask = csvColumnMask(len(cells), want)
		}
		row := []string{}
		for i, c := range cells {
			if i >= len(mask) || mask[i] {
				row = append(row, csvCell(c))
			}
		}
		out.WriteString(strings.Join(row, delim) + "\r\n")
	}
	return []byte(out.String())
}

// boardRows are the board's export rows - WeKan's header, then a card a row -
// for CSV and Excel alike.
func (s *Server) boardRows(ctx context.Context, b *Board, only ...string) [][]any {
	all := func(coll string, filter bson.M) []bson.M {
		cur, err := s.db.Collection(coll).Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}}))
		docs := []bson.M{}
		if err == nil {
			_ = cur.All(ctx, &docs)
		}
		return docs
	}
	lists, swimlanes := all("lists", bson.M{"boardId": b.ID}), all("swimlanes", bson.M{"boardId": b.ID})
	fields := all("customFields", bson.M{"boardIds": b.ID})
	filter := bson.M{"boardId": b.ID, "linkedId": bson.M{"$in": bson.A{"", nil}}}
	if len(only) == 1 {
		filter["_id"] = eq(only[0])
	}
	cards := all("cards", filter)
	var raw bson.M
	_ = s.db.Collection("boards").FindOne(ctx, bson.M{"_id": b.ID}).Decode(&raw)
	labels := []bson.M{}
	if ls, ok := raw["labels"].(bson.A); ok {
		for _, e := range ls {
			d, _ := e.(bson.D)
			labels = append(labels, bson.M(docMap(d)))
		}
	}
	ids := bson.A{}
	for _, c := range cards {
		ids = append(ids, c["userId"])
		for _, k := range []string{"members", "assignees", "requesters", "assigners"} {
			for _, x := range stringsOf(c[k]) {
				ids = append(ids, x)
			}
		}
		if v, ok := c["vote"].(bson.D); ok {
			m := docMap(v)
			for _, x := range append(stringsOf(m["positive"]), stringsOf(m["negative"])...) {
				ids = append(ids, x)
			}
		}
	}
	users := []bson.M{}
	if cur, err := s.db.Collection("users").Find(ctx, bson.M{"_id": dbIn(ids)}, options.Find().SetProjection(bson.M{"username": 1})); err == nil {
		_ = cur.All(ctx, &users)
	}

	lang := "en" // WeKan writes the CSV's headers in English
	header := []any{tr(lang, "title"), tr(lang, "description"), tr(lang, "list"), tr(lang, "swimlane"), tr(lang, "owner"),
		tr(lang, "requested-by"), tr(lang, "assigned-by"), tr(lang, "members"), tr(lang, "assignee"), tr(lang, "labels"),
		tr(lang, "card-start"), tr(lang, "card-due"), tr(lang, "card-end"), tr(lang, "overtime-hours"), tr(lang, "spent-time-hours"),
		tr(lang, "createdAt"), tr(lang, "last-modified-at"), tr(lang, "last-activity"), tr(lang, "voting"), tr(lang, "archived")}
	position := map[string]int{}
	for i, f := range fields {
		id, _ := f["_id"].(string)
		position[id] = i
		name, _ := f["name"].(string)
		typ, _ := f["type"].(string)
		switch typ {
		case "dropdown", "dropdownMultiSelect":
			opts := []string{}
			settings, _ := f["settings"].(bson.D)
			items, _ := docMap(settings)["dropdownItems"].(bson.A)
			for _, e := range items {
				d, _ := e.(bson.D)
				n, _ := docMap(d)["name"].(string)
				opts = append(opts, n)
			}
			header = append(header, "CustomField-"+name+"-"+typ+"-"+strings.Join(opts, "/"))
		case "currency":
			settings, _ := f["settings"].(bson.D)
			code, _ := docMap(settings)["currencyCode"].(string)
			header = append(header, "CustomField-"+name+"-"+typ+"-"+code)
		default:
			header = append(header, "CustomField-"+name+"-"+typ)
		}
	}
	// Admin Panel -> Anonymize export users: the names in the columns, and
	// what the cards say about them.
	if a := s.exportAnonymizer(ctx, b, users); a != nil {
		for _, u := range users {
			a.user(u)
		}
		a.cards(cards)
	}
	rows := [][]any{header}
	for _, c := range cards {
		rows = append(rows, csvRow(c, lists, swimlanes, users, labels, fields, position))
	}
	return rows
}
