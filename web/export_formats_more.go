package web

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Nullboard, Obsidian Kanban, Org mode and Kanri, as Node.js WeKan's
// nullboardFormat.js, obsidianKanbanFormat.js, orgModeFormat.js and
// kanriFormat.js write them - checked byte for byte against those
// (TestExternalFormatsAreWeKans).

// jsTrimEnd is s.replace(/\s+$/, ”).
func jsTrimEnd(s string) string { return strings.TrimRightFunc(s, jsSpace) }

// jsLines is s.split(/\r\n|\r|\n/).
func jsLines(s string) []string {
	return strings.Split(strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n"), "\n")
}

// foldLineBreaks is s.replace(/\s*[\r\n]+\s*/g, ' '): every run of white
// space that holds a line break becomes one space; other runs stay.
func foldLineBreaks(s string) string {
	var b strings.Builder
	rs := []rune(s)
	for i := 0; i < len(rs); {
		if !jsSpace(rs[i]) {
			b.WriteRune(rs[i])
			i++
			continue
		}
		j, breaks := i, false
		for j < len(rs) && jsSpace(rs[j]) {
			breaks = breaks || rs[j] == '\r' || rs[j] == '\n'
			j++
		}
		if breaks {
			b.WriteString(" ")
		} else {
			b.WriteString(string(rs[i:j]))
		}
		i = j
	}
	return b.String()
}

// --- Nullboard

type nbNote struct {
	Text string `json:"text"`
	Raw  bool   `json:"raw"`
	Min  bool   `json:"min"`
}

type nbList struct {
	Title string   `json:"title"`
	Notes []nbNote `json:"notes"`
	extra bool
}

type nbBoard struct {
	Format   int      `json:"format"`
	ID       int64    `json:"id"`
	Revision int      `json:"revision"`
	Title    string   `json:"title"`
	Lists    []nbList `json:"lists"`
}

// nullboardNote is noteText: the title line, then the description and each
// checklist as blocks.
func nullboardNote(i extItem) string {
	title := jsTrim(foldLineBreaks(i.Title))
	if title == "" {
		title = "Untitled"
	}
	var blocks []string
	if d := jsTrimEnd(i.Description); d != "" {
		blocks = append(blocks, d)
	}
	for _, c := range i.Checklists {
		if len(c.Items) == 0 {
			continue
		}
		name := jsTrim(c.Title)
		if name == "" {
			name = "Checklist"
		}
		lines := []string{name}
		for _, x := range c.Items {
			box := " "
			if x.Done {
				box = "x"
			}
			lines = append(lines, "["+box+"] "+jsTrim(x.Title))
		}
		blocks = append(blocks, strings.Join(lines, "\n"))
	}
	if rest := strings.Join(blocks, "\n\n"); rest != "" {
		return title + "\n" + rest
	}
	return title
}

// formatNullboard is WeKan's: one board Nullboard's importer accepts, its id
// the board's creation time in epoch milliseconds.
func formatNullboard(d extBoard, now time.Time) string {
	id := now.UnixMilli()
	if t := parseISO(d.Board.CreatedAt); !t.IsZero() && t.UnixMilli() > 0 {
		id = t.UnixMilli()
	}
	title := d.Board.Title
	if title == "" {
		title = "WeKan board"
	}
	out := nbBoard{Format: 20190412, ID: id, Revision: 1, Title: title, Lists: []nbList{}}
	used := map[int]bool{}
	notes := func(idx []int) []nbNote {
		ns := []nbNote{}
		for _, k := range idx {
			used[k] = true
			ns = append(ns, nbNote{Text: nullboardNote(d.Items[k]), Raw: contains(d.Items[k].Labels, "raw")})
		}
		return ns
	}
	for _, l := range d.Lists {
		var idx []int
		for k, i := range d.Items {
			if (l.ID != "" && i.ListID != "" && i.ListID == l.ID) || ((l.ID == "" || i.ListID == "") && i.ListTitle == l.Title) {
				idx = append(idx, k)
			}
		}
		out.Lists = append(out.Lists, nbList{Title: l.Title, Notes: notes(idx)})
	}
	for k, i := range d.Items {
		if used[k] {
			continue
		}
		title := i.ListTitle
		if title == "" {
			title = "List"
		}
		at := -1
		for n, l := range out.Lists {
			if l.Title == title && l.extra {
				at = n
				break
			}
		}
		if at < 0 {
			out.Lists = append(out.Lists, nbList{Title: title, Notes: []nbNote{}, extra: true})
			at = len(out.Lists) - 1
		}
		out.Lists[at].Notes = append(out.Lists[at].Notes, notes([]int{k})...)
	}
	return string(jsJSON(out, "")) + "\n"
}

// --- Obsidian Kanban

func obsidianDay(iso string) string {
	if t := parseISO(iso); !t.IsZero() {
		return t.Format("2006-01-02")
	}
	return ""
}

func isoClock(iso string) string {
	t := parseISO(iso)
	text := "00:00"
	if !t.IsZero() {
		text = t.Format("15:04")
	}
	if text == "00:00" {
		return ""
	}
	return text
}

// dropLastEmpty is filter((line, i, all) => line || i < all.length - 1).
func dropLastEmpty(lines []string) []string {
	if n := len(lines); n > 0 && lines[n-1] == "" {
		return lines[:n-1]
	}
	return lines
}

// formatObsidianKanban is WeKan's: the plugin's own board file.
func formatObsidianKanban(d extBoard) string {
	titles := []string{}
	limits := map[string]float64{}
	for _, l := range d.Lists {
		if l.Title != "" {
			titles = append(titles, l.Title)
		}
		if l.WipLimit != nil && l.WipLimit.Enabled && l.WipLimit.Value > 0 {
			limits[l.Title] = l.WipLimit.Value
		}
	}
	for _, i := range d.Items {
		if i.ListTitle != "" && !contains(titles, i.ListTitle) {
			titles = append(titles, i.ListTitle)
		}
	}
	var out strings.Builder
	out.WriteString("---\n\nkanban-plugin: board\n\n---\n\n")
	for _, title := range titles {
		out.WriteString("## " + title)
		if v := limits[title]; v != 0 {
			out.WriteString(" (" + strconv.FormatFloat(v, 'f', -1, 64) + ")")
		}
		out.WriteString("\n\n")
		for _, i := range d.Items {
			if i.ListTitle != title {
				continue
			}
			var labels []string
			for _, l := range i.Labels {
				if l != "" {
					labels = append(labels, l)
				}
			}
			done := contains(labels, "done") || i.EndAt != ""
			name := jsTrim(jsCollapse(i.Title, " "))
			if name == "" {
				name = "Untitled"
			}
			parts := []string{name}
			for _, l := range labels {
				if l != "done" {
					parts = append(parts, "#"+jsCollapse(jsTrim(l), "-"))
				}
			}
			if i.DueAt != "" {
				parts = append(parts, "@{"+obsidianDay(i.DueAt)+"}")
				if c := isoClock(i.DueAt); c != "" {
					parts = append(parts, "@@{"+c+"}")
				}
			}
			if i.StartAt != "" {
				parts = append(parts, "🛫 "+obsidianDay(i.StartAt))
			}
			if i.EndAt != "" {
				parts = append(parts, "✅ "+obsidianDay(i.EndAt))
			}
			box := " "
			if done {
				box = "x"
			}
			out.WriteString("- [" + box + "] " + strings.Join(parts, " ") + "\n")
			body := dropLastEmpty(jsLines(i.Description))
			for _, c := range i.Checklists {
				for _, x := range c.Items {
					b := " "
					if x.Done {
						b = "x"
					}
					body = append(body, "- ["+b+"] "+x.Title)
				}
			}
			for _, line := range dropLastEmpty(body) {
				out.WriteString("    " + line + "\n")
			}
		}
		out.WriteString("\n\n\n")
	}
	out.WriteString("%% kanban:settings\n```\n{\"kanban-plugin\":\"board\"}\n```\n%%\n")
	return out.String()
}

// --- Org mode

var (
	orgPriority = regexp.MustCompile(`^priority:([A-Z0-9])$`)
	orgDays     = []string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
)

// orgStamp is stamp(): <2026-10-10 Sat 14:30>, [ ] when inactive.
func orgStamp(iso string, active bool) string {
	t := parseISO(iso)
	if t.IsZero() {
		return ""
	}
	inner := t.Format("2006-01-02") + " " + orgDays[t.Weekday()]
	if c := t.Format("15:04"); c != "00:00" {
		inner += " " + c
	}
	if active {
		return "<" + inner + ">"
	}
	return "[" + inner + "]"
}

func orgOneLine(s string) string { return jsTrim(jsCollapse(s, " ")) }

// orgTag is orgTag(): runs of anything but letters, numbers and _@#% become
// one "_", without one at either end.
func orgTag(s string) string {
	var b strings.Builder
	run := false
	for _, r := range jsTrim(s) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || strings.ContainsRune("_@#%", r) {
			b.WriteRune(r)
			run = false
			continue
		}
		if !run {
			b.WriteString("_")
		}
		run = true
	}
	return strings.Trim(b.String(), "_")
}

// formatOrgMode is WeKan's: a TODO outline, the lists as headings.
func formatOrgMode(d extBoard) string {
	out := []string{"#+TITLE: " + orgOneLine(d.Board.Title), "#+TODO: TODO | DONE", ""}
	titles := d.listTitles()
	for _, i := range d.Items {
		if i.ListTitle != "" && !contains(titles, i.ListTitle) {
			titles = append(titles, i.ListTitle)
		}
	}
	for _, lt := range titles {
		h := orgOneLine(lt)
		if h == "" {
			h = "List"
		}
		out = append(out, "* "+h)
		for _, i := range d.Items {
			if i.ListTitle != lt {
				continue
			}
			priority := ""
			for _, l := range i.Labels {
				if m := orgPriority.FindStringSubmatch(l); m != nil {
					priority = m[1]
					break
				}
			}
			var tags []string
			for _, l := range i.Labels {
				if orgPriority.MatchString(l) || l == "done" {
					continue
				}
				if t := orgTag(l); t != "" {
					tags = append(tags, t)
				}
			}
			done := isClosedList(i.ListTitle) || contains(i.Labels, "done")
			title := orgOneLine(i.Title)
			if title == "" {
				title = "Untitled"
			}
			line := "** " + map[bool]string{true: "DONE", false: "TODO"}[done] + " "
			if priority != "" {
				line += "[#" + priority + "] "
			}
			line += title
			if len(tags) > 0 {
				line += " :" + strings.Join(tags, ":") + ":"
			}
			out = append(out, line)
			var planning []string
			if i.StartAt != "" {
				planning = append(planning, "SCHEDULED: "+orgStamp(i.StartAt, true))
			}
			if i.DueAt != "" {
				planning = append(planning, "DEADLINE: "+orgStamp(i.DueAt, true))
			}
			if done && i.EndAt != "" {
				planning = append(planning, "CLOSED: "+orgStamp(i.EndAt, false))
			}
			if len(planning) > 0 {
				out = append(out, "   "+strings.Join(planning, " "))
			}
			if i.CreatedAt != "" {
				out = append(out, "   :PROPERTIES:", "   :CREATED:  "+orgStamp(i.CreatedAt, false), "   :END:")
			}
			if i.Description != "" {
				for _, l := range jsLines(i.Description) {
					if l != "" {
						l = "   " + l
					}
					out = append(out, l)
				}
			}
			for _, c := range i.Checklists {
				if len(c.Items) == 0 {
					continue
				}
				name := orgOneLine(c.Title)
				if name == "" {
					name = "Checklist"
				}
				out = append(out, "*** "+name)
				for _, x := range c.Items {
					item := orgOneLine(x.Title)
					if item == "" {
						item = "Item"
					}
					out = append(out, "**** "+map[bool]string{true: "DONE", false: "TODO"}[x.Done]+" "+item)
				}
			}
		}
	}
	return strings.Join(out, "\n") + "\n"
}

// --- Kanri

// wekanColorHex are WeKan's named colors as labels.css draws them.
var wekanColorHex = map[string]string{
	"white": "#ffffff", "green": "#3cb500", "yellow": "#fad900", "orange": "#ff9f19", "red": "#eb4646",
	"purple": "#a632db", "blue": "#0079bf", "sky": "#00c2e0", "lime": "#51e898", "pink": "#ff78cb",
	"black": "#4d4d4d", "silver": "#c0c0c0", "peachpuff": "#ffdab9", "crimson": "#dc143c", "plum": "#dda0dd",
	"darkgreen": "#006400", "slateblue": "#6a5acd", "magenta": "#ff00ff", "gold": "#ffd700", "navy": "#000080",
	"gray": "#808080", "saddlebrown": "#8b4513", "paleturquoise": "#afeeee", "mistyrose": "#ffe4e1", "indigo": "#4b0082",
}

// kanriCardClass is Kanri's card palette by WeKan's names.
var kanriCardClass = map[string]string{"pink": "bg-pink-600", "red": "bg-red-600", "orange": "bg-orange-600",
	"yellow": "bg-yellow-600", "green": "bg-green-600", "#0d9488": "bg-teal-600", "blue": "bg-blue-600", "purple": "bg-purple-600"}

var kanriHexPattern = regexp.MustCompile(`(?i)^#?([0-9a-f]{3}|[0-9a-f]{6})$`)

// kanriHex is WeKan's: '#rgb' or '#rrggbb' as lowercase '#rrggbb', or "".
func kanriHex(v string) string {
	m := kanriHexPattern.FindStringSubmatch(jsTrim(v))
	if m == nil {
		return ""
	}
	h := m[1]
	if len(h) == 3 {
		h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
	}
	return "#" + strings.ToLower(h)
}

func kanriCardColorOf(color string) string {
	v := strings.ToLower(jsTrim(color))
	if v == "" {
		return ""
	}
	if c := kanriCardClass[v]; c != "" {
		return c
	}
	if h := kanriHex(v); h != "" {
		return h
	}
	return wekanColorHex[v]
}

// kanriTagText is WeKan's: Kanri's dark or light text when it reads on the
// tag (WCAG 4.5:1), else plain black or white.
func kanriTagText(hex string) string {
	lin := func(c float64) float64 {
		v := c / 255
		if v <= 0.03928 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	lum := func(h string) float64 {
		sum := 0.0
		for k, i := range []int{1, 3, 5} {
			n, _ := strconv.ParseInt(h[i:i+2], 16, 64)
			sum += lin(float64(n)) * []float64{0.2126, 0.7152, 0.0722}[k]
		}
		return sum
	}
	ratio := func(a, b string) float64 {
		la, lb := lum(a), lum(b)
		return (math.Max(la, lb) + 0.05) / (math.Min(la, lb) + 0.05)
	}
	best := func(dark, light string) string {
		if ratio(hex, dark) >= ratio(hex, light) {
			return dark
		}
		return light
	}
	k := best("#1e293b", "#f4f4f5")
	if ratio(hex, k) >= 4.5 {
		return k
	}
	return best("#000000", "#ffffff")
}

type kanriTag struct {
	ID    string `json:"id"`
	Text  string `json:"text"`
	Color string `json:"color,omitempty"`
	Style string `json:"style,omitempty"`
}

type kanriTask struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Finished bool   `json:"finished"`
}

type kanriCard struct {
	ID                       string      `json:"id"`
	Name                     string      `json:"name"`
	Description              string      `json:"description"`
	Color                    string      `json:"color"`
	DueDate                  *string     `json:"dueDate"`
	IsDueDateCounterRelative bool        `json:"isDueDateCounterRelative"`
	IsDueDateCompleted       bool        `json:"isDueDateCompleted"`
	Tasks                    []kanriTask `json:"tasks"`
	Tags                     []kanriTag  `json:"tags"`
}

type kanriColumn struct {
	ID    string      `json:"id"`
	Title string      `json:"title"`
	Cards []kanriCard `json:"cards"`
}

type kanriBoard struct {
	ID         string        `json:"id"`
	Title      string        `json:"title"`
	Columns    []kanriColumn `json:"columns"`
	Background *string       `json:"background"`
	GlobalTags []kanriTag    `json:"globalTags"`
	LastEdited string        `json:"lastEdited"`
	CreatedAt  string        `json:"createdAt"`
}

// formatKanri is WeKan's: a Kanri single-board export, what its Partial
// import reads. One JSON document.
func formatKanri(d extBoard, now time.Time) []byte {
	labels := map[string]Label{}
	for _, l := range d.Board.Labels {
		if jsTrim(l.Name) != "" {
			labels[l.ID] = l
		}
	}
	tagFor := func(l Label) kanriTag {
		hex := kanriHex(l.Color)
		if hex == "" {
			hex = wekanColorHex[strings.ToLower(l.Color)]
		}
		id := l.ID
		if id == "" {
			id = l.Name
		}
		t := kanriTag{ID: id, Text: l.Name}
		if hex != "" {
			t.Color, t.Style = hex, "background-color: "+hex+"; color: "+kanriTagText(hex)
		}
		return t
	}
	var usedOrder []string
	used := map[string]kanriTag{}
	columns := []kanriColumn{}
	at := map[string]int{}
	for _, l := range d.Lists {
		at[l.ID] = len(columns)
		columns = append(columns, kanriColumn{ID: l.ID, Title: l.Title, Cards: []kanriCard{}})
	}
	for _, i := range d.Items {
		n, ok := at[i.ListID]
		if !ok {
			key := i.ListID
			if key == "" {
				key = "no-list"
			}
			title := i.ListTitle
			if title == "" {
				title = "Untitled"
			}
			n = len(columns)
			at[key] = n
			columns = append(columns, kanriColumn{ID: key, Title: title, Cards: []kanriCard{}})
		}
		tags := []kanriTag{}
		for _, id := range i.LabelIDs {
			if l, ok := labels[id]; ok {
				t := tagFor(l)
				if _, seen := used[t.ID]; !seen {
					usedOrder = append(usedOrder, t.ID)
				}
				used[t.ID] = t
				tags = append(tags, t)
			}
		}
		name := jsTrim(i.Title)
		if name == "" {
			name = "Untitled"
		}
		color := kanriCardColorOf(i.Color)
		if color == "" {
			color = "bg-elevation-2"
		}
		c := kanriCard{ID: i.CardID, Name: name, Description: i.Description, Color: color, Tasks: []kanriTask{}, Tags: tags}
		if i.DueAt != "" {
			due := i.DueAt
			c.DueDate, c.IsDueDateCompleted = &due, i.DueComplete
		}
		k := 0
		for _, cl := range i.Checklists {
			for _, x := range cl.Items {
				if jsTrim(x.Title) == "" {
					continue
				}
				k++
				c.Tasks = append(c.Tasks, kanriTask{ID: fmt.Sprintf("%s-task-%d", i.CardID, k), Name: jsTrim(x.Title), Finished: x.Done})
			}
		}
		columns[n].Cards = append(columns[n].Cards, c)
	}
	edited := d.Board.ModifiedAt
	if edited == "" {
		edited = d.Board.CreatedAt
	}
	if edited == "" {
		edited = now.UTC().Format("2006-01-02T15:04:05.000Z")
	}
	created := d.Board.CreatedAt
	if created == "" {
		created = edited
	}
	id, title := d.Board.ID, d.Board.Title
	if id == "" {
		id = "wekan-board"
	}
	if title == "" {
		title = "WeKan board"
	}
	global := []kanriTag{}
	for _, k := range usedOrder {
		global = append(global, used[k])
	}
	return jsJSON(kanriBoard{ID: id, Title: title, Columns: columns, GlobalTags: global, LastEdited: edited, CreatedAt: created}, "")
}
