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
var externalFormats = map[string][2]string{
	"markdown": {"md", "text/markdown"},
	"opml":     {"opml", "text/x-opml"},
	"todotxt":  {"txt", "text/plain"},
}

type extChecklist struct {
	Title string
	Items []struct {
		Title string
		Done  bool
	}
}

// extItem is a card as the formatters read it.
type extItem struct {
	Title, Description, ListTitle string
	Labels                        []string
	DueAt, StartAt, EndAt         time.Time
	CreatedAt                     time.Time
	Checklists                    []extChecklist
}

type extBoard struct {
	Title string
	Lists []string
	Items []extItem
}

// collectExternal is WeKan's collect(): the unarchived lists and cards in
// their order, each card's labels by name and its checklists.
func (s *Server) collectExternal(ctx context.Context, b *Board) extBoard {
	out := extBoard{Title: b.Title}
	listTitle := map[string]string{}
	if cur, err := s.db.Collection("lists").Find(ctx, bson.M{"boardId": b.ID, "archived": false}, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}})); err == nil {
		var lists []List
		_ = cur.All(ctx, &lists)
		for _, l := range lists {
			out.Lists = append(out.Lists, l.Title)
			listTitle[l.ID] = l.Title
		}
	}
	labels := map[string]string{}
	for _, l := range b.Labels {
		labels[l.ID] = l.Name
	}
	var cards []bson.M
	if cur, err := s.db.Collection("cards").Find(ctx, bson.M{"boardId": b.ID, "archived": false}, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}})); err == nil {
		_ = cur.All(ctx, &cards)
	}
	// Admin Panel -> Anonymize export users: what the cards say about people.
	s.exportAnonymizer(ctx, b, nil).cards(cards)
	when := func(v any) time.Time {
		if t, ok := v.(bson.DateTime); ok {
			return t.Time().UTC()
		}
		return time.Time{}
	}
	for _, c := range cards {
		it := extItem{Title: strOf(c["title"]), Description: strOf(c["description"]), ListTitle: listTitle[strOf(c["listId"])],
			DueAt: when(c["dueAt"]), StartAt: when(c["startAt"]), EndAt: when(c["endAt"]), CreatedAt: when(c["createdAt"])}
		if ids, ok := c["labelIds"].(bson.A); ok {
			for _, id := range ids {
				if n := labels[strOf(id)]; n != "" {
					it.Labels = append(it.Labels, n)
				}
			}
		}
		if cur, err := s.db.Collection("checklists").Find(ctx, bson.M{"cardId": eq(strOf(c["_id"]))}, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}})); err == nil {
			var cls []bson.M
			_ = cur.All(ctx, &cls)
			for _, cl := range cls {
				x := extChecklist{Title: strOf(cl["title"])}
				if ic, err := s.db.Collection("checklistItems").Find(ctx, bson.M{"checklistId": eq(strOf(cl["_id"]))}, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}})); err == nil {
					var items []bson.M
					_ = ic.All(ctx, &items)
					for _, i := range items {
						x.Items = append(x.Items, struct {
							Title string
							Done  bool
						}{strOf(i["title"]), i["isFinished"] == true})
					}
				}
				it.Checklists = append(it.Checklists, x)
			}
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
	title := d.Title
	if title == "" {
		title = "Imported board"
	}
	lines := []string{"# " + title, ""}
	for _, l := range d.Lists {
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
	titles := append([]string{}, d.Lists...)
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
		"    <title>" + opmlAttr.Replace(d.Title) + "</title>", "  </head>", "  <body>"}, body...), "  </body>", "</opml>", ""), "\n")
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

var (
	priorityLabel = regexp.MustCompile(`^priority:([A-Z])$`)
	whitespace    = regexp.MustCompile(`\s+`)
)

// formatTodoTxt is todoTxtFormat.js: a card a line.
func formatTodoTxt(d extBoard) string {
	day := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.Format("2006-01-02")
	}
	respace := func(v string) string { return whitespace.ReplaceAllString(strings.TrimSpace(v), "_") }
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
		title := strings.TrimSpace(whitespace.ReplaceAllString(i.Title, " "))
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
