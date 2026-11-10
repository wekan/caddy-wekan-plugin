package web

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// A board in other tools' formats, as Node.js WeKan's models/lib/
// externalExporters.js collects it and its formatters write it: Markdown
// (markdown-kanban task lists), OPML (Workflowy's outline) and todo.txt -
// /api/boards/{board}/export/{format}, and in the export of all boards. The
// same access as every export, and the people in it anonymized when Admin
// Panel says so.

// externalFormats are the formats Weddy writes: extension and content type.
// Each is checked byte for byte against WeKan's own formatter
// (TestExternalFormatsAreWeKans, testdata/external).
var externalFormats = map[string][2]string{
	"markdown":    {"md", "text/markdown"},
	"opml":        {"opml", "text/x-opml"},
	"todotxt":     {"txt", "text/plain"},
	"taskwarrior": {"json", "application/json"},
	"zenkit":      {"json", "application/json"},
	"nullboard":   {"nbx", "application/json"},
	"obsidian":    {"md", "text/markdown"},
	"orgmode":     {"org", "text/x-org"},
	"kanri":       {"json", "application/json"},
}

type extChecklistItem struct {
	Title string `json:"title"`
	Done  bool   `json:"done"`
}

type extChecklist struct {
	Title string             `json:"title"`
	Items []extChecklistItem `json:"items"`
}

type extComment struct {
	Text   string `json:"text"`
	Author string `json:"author,omitempty"`
	Date   string `json:"date,omitempty"`
}

// extItem is a card as WeKan's collect() gathers it and the formatters read
// it - the same JSON shape, so testdata/external/collected.json is given to
// WeKan's formatters and to these alike. Dates are WeKan's toISOString().
// Weddy has no export selection: everything WeKan's default selection
// includes is here, but custom fields (WeKan lets them out only after its
// admin-only check) and Scrum fields (Weddy has no sprints yet).
type extItem struct {
	CardID        string         `json:"cardId"`
	ListID        string         `json:"listId"`
	Title         string         `json:"title"`
	Description   string         `json:"description"`
	ListTitle     string         `json:"listTitle"`
	SwimlaneTitle string         `json:"swimlaneTitle"`
	DueAt         string         `json:"dueAt,omitempty"`
	DueComplete   bool           `json:"dueComplete,omitempty"`
	Color         string         `json:"color,omitempty"`
	LabelIDs      []string       `json:"labelIds"`
	Labels        []string       `json:"labels"`
	StartAt       string         `json:"startAt,omitempty"`
	EndAt         string         `json:"endAt,omitempty"`
	CreatedAt     string         `json:"createdAt,omitempty"`
	Owner         string         `json:"owner,omitempty"`
	Assignees     []string       `json:"assignees,omitempty"`
	Creator       string         `json:"creator,omitempty"`
	RequestedBy   string         `json:"requestedBy,omitempty"`
	ParentCardID  string         `json:"parentCardId,omitempty"`
	Comments      []extComment   `json:"comments,omitempty"`
	Checklists    []extChecklist `json:"checklists,omitempty"`
}

type extWipLimit struct {
	Enabled bool    `json:"enabled"`
	Value   float64 `json:"value"`
}

type extNamed struct {
	ID       string       `json:"_id"`
	Title    string       `json:"title"`
	WipLimit *extWipLimit `json:"wipLimit,omitempty"`
}

type extBoard struct {
	Board struct {
		ID         string  `json:"_id"`
		Title      string  `json:"title"`
		Labels     []Label `json:"labels"`
		CreatedAt  string  `json:"createdAt,omitempty"`
		ModifiedAt string  `json:"modifiedAt,omitempty"`
	} `json:"board"`
	Lists     []extNamed `json:"lists"`
	Swimlanes []extNamed `json:"swimlanes"`
	Items     []extItem  `json:"items"`
}

// listTitles are the board's list titles, in order.
func (d extBoard) listTitles() []string {
	out := []string{}
	for _, l := range d.Lists {
		out = append(out, l.Title)
	}
	return out
}

// isoDate is WeKan's iso(): toISOString, "" for none.
func isoDate(v any) string {
	var t time.Time
	switch x := v.(type) {
	case bson.DateTime:
		t = x.Time()
	case time.Time:
		t = x
	default:
		return ""
	}
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// parseISO reads one of isoDate's back.
func parseISO(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// collectExternal is WeKan's collect(): the unarchived lists, swimlanes and
// cards in their order, each card's labels by name, its people by username,
// its comments and its checklists - Admin Panel's Anonymize export users
// applied to all of it.
func (s *Server) collectExternal(ctx context.Context, b *Board) extBoard {
	var out extBoard
	out.Board.ID, out.Board.Title, out.Board.Labels = b.ID, b.Title, b.Labels
	var bdoc bson.M
	if s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(b.ID)}).Decode(&bdoc) == nil {
		out.Board.CreatedAt, out.Board.ModifiedAt = isoDate(bdoc["createdAt"]), isoDate(bdoc["modifiedAt"])
	}
	out.Lists, out.Swimlanes = []extNamed{}, []extNamed{}
	sorted := options.Find().SetSort(bson.D{{Key: "sort", Value: 1}})
	listTitle, laneTitle := map[string]string{}, map[string]string{}
	for coll, dst := range map[string]*[]extNamed{"lists": &out.Lists, "swimlanes": &out.Swimlanes} {
		if cur, err := s.db.Collection(coll).Find(ctx, bson.M{"boardId": b.ID, "archived": false}, sorted); err == nil {
			var docs []bson.M
			_ = cur.All(ctx, &docs)
			for _, d := range docs {
				x := extNamed{ID: strOf(d["_id"]), Title: strOf(d["title"])}
				if w, ok := d["wipLimit"].(bson.M); ok {
					v, _ := plain(w["value"]).(float64)
					if n, ok := plain(w["value"]).(int32); ok {
						v = float64(n)
					}
					if n, ok := plain(w["value"]).(int64); ok {
						v = float64(n)
					}
					x.WipLimit = &extWipLimit{Enabled: w["enabled"] == true, Value: v}
				}
				*dst = append(*dst, x)
			}
		}
	}
	for _, l := range out.Lists {
		listTitle[l.ID] = l.Title
	}
	for _, l := range out.Swimlanes {
		laneTitle[l.ID] = l.Title
	}
	labels := map[string]string{}
	for _, l := range b.Labels {
		labels[l.ID] = l.Name
	}
	var cards []bson.M
	if cur, err := s.db.Collection("cards").Find(ctx, bson.M{"boardId": b.ID, "archived": false}, sorted); err == nil {
		_ = cur.All(ctx, &cards)
	}
	ids := bson.A{}
	for _, c := range cards {
		ids = append(ids, strOf(c["_id"]))
	}
	var comments, checklists, items []bson.M
	if len(ids) > 0 {
		if cur, err := s.db.Collection("card_comments").Find(ctx, bson.M{"cardId": dbIn(ids)}, options.Find().SetSort(bson.D{{Key: "createdAt", Value: 1}})); err == nil {
			_ = cur.All(ctx, &comments)
		}
		if cur, err := s.db.Collection("checklists").Find(ctx, bson.M{"cardId": dbIn(ids)}, sorted); err == nil {
			_ = cur.All(ctx, &checklists)
		}
		clIDs := bson.A{}
		for _, cl := range checklists {
			clIDs = append(clIDs, strOf(cl["_id"]))
		}
		if len(clIDs) > 0 {
			if cur, err := s.db.Collection("checklistItems").Find(ctx, bson.M{"checklistId": dbIn(clIDs)}, sorted); err == nil {
				_ = cur.All(ctx, &items)
			}
		}
	}
	// The people the cards and comments name, by username.
	userIDs := bson.A{}
	seen := map[string]bool{}
	addUser := func(v any) {
		if id := strOf(v); id != "" && !seen[id] {
			seen[id] = true
			userIDs = append(userIDs, id)
		}
	}
	for _, c := range cards {
		addUser(c["userId"])
		for _, f := range []string{"assignees", "members"} {
			list, _ := c[f].(bson.A)
			for _, id := range list {
				addUser(id)
			}
		}
	}
	for _, cm := range comments {
		addUser(cm["userId"])
	}
	var users []bson.M
	if len(userIDs) > 0 {
		if cur, err := s.db.Collection("users").Find(ctx, bson.M{"_id": dbIn(userIDs)}); err == nil {
			_ = cur.All(ctx, &users)
		}
	}
	// Admin Panel -> Anonymize export users: the names and what the cards
	// and comments say about people.
	anon := s.exportAnonymizer(ctx, b, users)
	anon.cards(cards)
	anon.comments(comments)
	username := map[string]string{}
	for _, u := range users {
		anon.user(u)
		username[strOf(u["_id"])] = strOf(u["username"])
	}
	for _, c := range cards {
		id := strOf(c["_id"])
		it := extItem{CardID: id, ListID: strOf(c["listId"]), Title: strOf(c["title"]), Description: strOf(c["description"]),
			ListTitle: listTitle[strOf(c["listId"])], SwimlaneTitle: laneTitle[strOf(c["swimlaneId"])],
			DueAt: isoDate(c["dueAt"]), Color: strOf(c["color"]), LabelIDs: []string{}, Labels: []string{},
			StartAt: isoDate(c["startAt"]), EndAt: isoDate(c["endAt"]), CreatedAt: isoDate(c["createdAt"]),
			Creator: username[strOf(c["userId"])], RequestedBy: strOf(c["requestedBy"]), ParentCardID: strOf(c["parentId"])}
		if it.SwimlaneTitle == "" {
			it.SwimlaneTitle = "Default"
		}
		it.DueComplete = it.DueAt != "" && c["dueComplete"] == true
		if list, ok := c["labelIds"].(bson.A); ok {
			for _, lid := range list {
				it.LabelIDs = append(it.LabelIDs, strOf(lid))
				if n := labels[strOf(lid)]; n != "" {
					it.Labels = append(it.Labels, n)
				}
			}
		}
		// The owner is the first of the assignees and members, as WeKan's.
		var people []string
		for _, f := range []string{"assignees", "members"} {
			list, _ := c[f].(bson.A)
			for _, uid := range list {
				if n := username[strOf(uid)]; n != "" {
					people = append(people, n)
				}
			}
		}
		if len(people) > 0 {
			it.Owner, it.Assignees = people[0], people[1:]
		}
		for _, cm := range comments {
			if strOf(cm["cardId"]) == id {
				it.Comments = append(it.Comments, extComment{Text: strOf(cm["text"]), Author: username[strOf(cm["userId"])], Date: isoDate(cm["createdAt"])})
			}
		}
		for _, cl := range checklists {
			if strOf(cl["cardId"]) != id {
				continue
			}
			x := extChecklist{Title: strOf(cl["title"]), Items: []extChecklistItem{}}
			for _, i := range items {
				if strOf(i["checklistId"]) == strOf(cl["_id"]) {
					x.Items = append(x.Items, extChecklistItem{Title: strOf(i["title"]), Done: i["isFinished"] == true})
				}
			}
			it.Checklists = append(it.Checklists, x)
		}
		out.Items = append(out.Items, it)
	}
	return out
}

// isClosedList is WeKan's terminal-list heuristic.
var closedList = regexp.MustCompile(`(?i)done|closed|complete|archiv|finished`)

func isClosedList(title string) bool { return closedList.MatchString(title) }

// formatMarkdownKanban is markdownKanbanFormat.js.
func formatMarkdownKanban(d extBoard) string {
	title := d.Board.Title
	if title == "" {
		title = "Imported board"
	}
	lines := []string{"# " + title, ""}
	for _, l := range d.listTitles() {
		var in []extItem
		for _, i := range d.Items {
			if i.ListTitle == l {
				in = append(in, i)
			}
		}
		if len(in) == 0 {
			continue
		}
		lines = append(lines, "## "+l, "")
		for _, i := range in {
			box := " "
			if isClosedList(i.ListTitle) {
				box = "x"
			}
			lines = append(lines, "- ["+box+"] "+i.Title)
			for _, dl := range strings.Split(i.Description, "\n") {
				if dl != "" {
					lines = append(lines, "  "+dl)
				}
			}
		}
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

// opmlAttr is opmlOutlineFormat.js's escapeAttr.
var opmlAttr = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "\r\n", "&#10;", "\r", "&#10;", "\n", "&#10;", "\t", "&#9;")

// formatOpml is opmlOutlineFormat.js: the lists, their cards with their
// descriptions as notes, and checklists below them.
func formatOpml(d extBoard) string {
	var outline func(text, note string, done bool, kids []func(string) string, indent string) string
	outline = func(text, note string, done bool, kids []func(string) string, indent string) string {
		attrs := `text="` + opmlAttr.Replace(text) + `"`
		if note != "" {
			attrs += ` _note="` + opmlAttr.Replace(note) + `"`
		}
		if done {
			attrs += ` _complete="true"`
		}
		if len(kids) == 0 {
			return indent + "<outline " + attrs + "/>"
		}
		parts := make([]string, len(kids))
		for i, k := range kids {
			parts[i] = k(indent + "  ")
		}
		return indent + "<outline " + attrs + ">\n" + strings.Join(parts, "\n") + "\n" + indent + "</outline>"
	}
	titles := d.listTitles()
	for _, i := range d.Items {
		if i.ListTitle != "" && !contains(titles, i.ListTitle) {
			titles = append(titles, i.ListTitle)
		}
	}
	body := []string{}
	for _, t := range titles {
		var kids []func(string) string
		for _, i := range d.Items {
			if i.ListTitle != t {
				continue
			}
			var cks []func(string) string
			for _, c := range i.Checklists {
				var its []func(string) string
				for _, x := range c.Items {
					its = append(its, func(ind string) string { return outline(x.Title, "", x.Done, nil, ind) })
				}
				cks = append(cks, func(ind string) string { return outline(c.Title, "", false, its, ind) })
			}
			kids = append(kids, func(ind string) string { return outline(i.Title, i.Description, isClosedList(i.ListTitle), cks, ind) })
		}
		body = append(body, outline(t, "", false, kids, "    "))
	}
	return strings.Join(append(append([]string{`<?xml version="1.0" encoding="UTF-8"?>`, `<opml version="2.0">`, "  <head>",
		"    <title>" + opmlAttr.Replace(d.Board.Title) + "</title>", "  </head>", "  <body>"}, body...), "  </body>", "</opml>", ""), "\n")
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

var priorityLabel = regexp.MustCompile(`^priority:([A-Z])$`)

// formatTodoTxt is todoTxtFormat.js: a card a line.
func formatTodoTxt(d extBoard) string {
	day := func(v string) string {
		if t := parseISO(v); !t.IsZero() {
			return t.Format("2006-01-02")
		}
		return ""
	}
	// JavaScript's trim() and \s, Unicode's spaces with them (jsTrim, jsCollapse).
	respace := func(v string) string { return jsCollapse(jsTrim(v), "_") }
	var lines []string
	for _, i := range d.Items {
		done := isClosedList(i.ListTitle)
		var pri string
		for _, l := range i.Labels {
			if m := priorityLabel.FindStringSubmatch(l); m != nil {
				pri = m[1]
				break
			}
		}
		var head []string
		if done {
			head = append(head, "x")
			if day(i.EndAt) != "" && day(i.CreatedAt) != "" {
				head = append(head, day(i.EndAt))
			}
		} else if pri != "" {
			head = append(head, "("+pri+")")
		}
		if day(i.CreatedAt) != "" {
			head = append(head, day(i.CreatedAt))
		}
		title := jsTrim(jsCollapse(i.Title, " "))
		if title == "" {
			title = "Untitled"
		}
		var tail []string
		for _, l := range i.Labels {
			if priorityLabel.MatchString(l) {
				continue
			}
			if strings.HasPrefix(l, "@") {
				tail = append(tail, "@"+respace(l[1:]))
			} else {
				tail = append(tail, "+"+respace(l))
			}
		}
		if done && pri != "" {
			tail = append(tail, "pri:"+pri)
		}
		if day(i.DueAt) != "" {
			tail = append(tail, "due:"+day(i.DueAt))
		}
		if day(i.StartAt) != "" {
			tail = append(tail, "t:"+day(i.StartAt))
		}
		if i.ListTitle != "" {
			tail = append(tail, "list:"+respace(i.ListTitle))
		}
		lines = append(lines, strings.Join(append(append(head, title), tail...), " "))
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// externalExport is a board in one of the formats.
func (s *Server) externalExport(ctx context.Context, b *Board, format string) []byte {
	d := s.collectExternal(ctx, b)
	switch format {
	case "markdown":
		return []byte(formatMarkdownKanban(d))
	case "opml":
		return []byte(formatOpml(d))
	case "todotxt":
		return []byte(formatTodoTxt(d))
	case "taskwarrior":
		return []byte(formatTaskwarrior(d))
	case "zenkit":
		return formatZenkit(d)
	case "nullboard":
		return []byte(formatNullboard(d, time.Now()))
	case "obsidian":
		return []byte(formatObsidianKanban(d))
	case "orgmode":
		return []byte(formatOrgMode(d))
	case "kanri":
		return formatKanri(d, time.Now())
	}
	return nil
}

func (s *Server) exportFormatRoutes(m *http.ServeMux) {
	for f := range externalFormats {
		m.HandleFunc("GET /api/boards/{board}/export/"+f, s.exportable(s.apiExportFormat(f)))
	}
}

func (s *Server) apiExportFormat(format string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _, ok := s.exportReader(w, r, false)
		if !ok {
			return
		}
		ext := externalFormats[format]
		w.Header().Set("Content-Type", ext[1]+"; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="export-board-`+b.ID+`.`+ext[0]+`"`)
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(s.externalExport(r.Context(), b, format))
	}
}
