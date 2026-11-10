package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
)

// The other tools' formats that are JSON documents, as Node.js WeKan's
// formatters write them - key for key, in their order - so the tools'
// importers, and WeKan's own, read Weddy's export as WeKan's.

// jsSpace is JavaScript's \s, which trim() and /\s+/ use: Unicode's white
// space and the byte order mark.
func jsSpace(r rune) bool { return unicode.IsSpace(r) || r == '\ufeff' }

// jsTrim is String.prototype.trim.
func jsTrim(s string) string { return strings.TrimFunc(s, jsSpace) }

// jsCollapse is s.replace(/\s+/g, rep).
func jsCollapse(s, rep string) string {
	var b strings.Builder
	inSpace := false
	for _, r := range s {
		if jsSpace(r) {
			if !inSpace {
				b.WriteString(rep)
			}
			inSpace = true
			continue
		}
		inSpace = false
		b.WriteRune(r)
	}
	return b.String()
}

// jsJSON is JSON.stringify(v, null, indent): no HTML escaping, as JavaScript.
func jsJSON(v any, indent string) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", indent)
	_ = enc.Encode(v)
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

// compactDate is taskwarriorFormat.js's: 20261010T000000Z.
func compactDate(iso string) string {
	t := parseISO(iso)
	if t.IsZero() {
		return ""
	}
	return t.Format("20060102T150405Z")
}

// taskwarriorUUID is WeKan's taskwarriorUuid: the card id spread over an RFC
// 4122 UUID, FNV-1a over its UTF-16 code units, so a card always exports with
// the same uuid.
func taskwarriorUUID(cardID string) string {
	units := utf16.Encode([]rune(cardID))
	hex := ""
	for round := uint32(0); len(hex) < 32; round++ {
		h := uint32(0x811c9dc5) ^ round
		for _, u := range units {
			h ^= uint32(u)
			h *= 0x01000193
		}
		hex += fmt.Sprintf("%08x", h)
	}
	b := []byte(hex[:32])
	b[12] = '4'
	n, _ := strconv.ParseInt(string(b[16]), 16, 64)
	b[16] = "89ab"[n%4]
	h := string(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

var (
	twPriority = regexp.MustCompile(`^priority:([HML])$`)
	twProject  = regexp.MustCompile(`^project:(.+)$`)
)

type twAnnotation struct {
	Entry       string `json:"entry,omitempty"`
	Description string `json:"description"`
}

type twTask struct {
	UUID             string         `json:"uuid"`
	Status           string         `json:"status"`
	Description      string         `json:"description"`
	Entry            string         `json:"entry,omitempty"`
	End              string         `json:"end,omitempty"`
	Due              string         `json:"due,omitempty"`
	Scheduled        string         `json:"scheduled,omitempty"`
	Project          string         `json:"project,omitempty"`
	Priority         string         `json:"priority,omitempty"`
	Tags             []string       `json:"tags,omitempty"`
	Annotations      []twAnnotation `json:"annotations,omitempty"`
	WekanList        string         `json:"wekanlist,omitempty"`
	WekanDescription string         `json:"wekandescription,omitempty"`
}

// formatTaskwarrior is taskwarriorFormat.js: `task import` JSON.
func formatTaskwarrior(d extBoard) string {
	tasks := []twTask{}
	for _, i := range d.Items {
		done := isClosedList(i.ListTitle)
		var priority, project string
		tags := []string{}
		for _, l := range i.Labels {
			if m := twPriority.FindStringSubmatch(l); m != nil && priority == "" {
				priority = m[1]
			}
			if m := twProject.FindStringSubmatch(l); m != nil && project == "" {
				project = m[1]
			}
		}
		for _, l := range i.Labels {
			if twPriority.MatchString(l) || twProject.MatchString(l) {
				continue
			}
			if t := jsCollapse(jsTrim(l), "_"); t != "" {
				tags = append(tags, t)
			}
		}
		source := i.CardID
		if source == "" {
			source = i.Title
		}
		desc := jsTrim(jsCollapse(i.Title, " "))
		if desc == "" {
			desc = "Untitled"
		}
		t := twTask{UUID: taskwarriorUUID(source), Status: map[bool]string{true: "completed", false: "pending"}[done], Description: desc,
			Entry: compactDate(i.CreatedAt), Due: compactDate(i.DueAt), Scheduled: compactDate(i.StartAt),
			Project: project, Priority: priority, Tags: tags, WekanList: i.ListTitle, WekanDescription: i.Description}
		if done {
			t.End = compactDate(i.EndAt)
		}
		for _, c := range i.Comments {
			if jsTrim(c.Text) == "" {
				continue
			}
			text := c.Text
			if c.Author != "" {
				text = c.Author + ": " + c.Text
			}
			t.Annotations = append(t.Annotations, twAnnotation{Entry: compactDate(c.Date), Description: text})
		}
		tasks = append(tasks, t)
	}
	return string(jsJSON(tasks, "  ")) + "\n"
}

type zkChecklistItem struct {
	Text    string `json:"text"`
	Checked bool   `json:"checked"`
}

type zkChecklist struct {
	Name  string            `json:"name"`
	Items []zkChecklistItem `json:"items"`
}

type zkItem struct {
	ID          string        `json:"id"`
	Title       string        `json:"title"`
	Description string        `json:"description"`
	StageName   string        `json:"stage_name"`
	Due         string        `json:"due,omitempty"`
	Tags        []string      `json:"tags"`
	Start       string        `json:"start,omitempty"`
	CreatedAt   string        `json:"created_at,omitempty"`
	ParentID    string        `json:"parent_id,omitempty"`
	Assignees   []string      `json:"assignees,omitempty"`
	Checklists  []zkChecklist `json:"checklists,omitempty"`
	Comments    []extComment  `json:"comments,omitempty"`
}

type zkStage struct {
	Name string `json:"name"`
}

type zkBoard struct {
	Title  string    `json:"title"`
	Stages []zkStage `json:"stages"`
	Items  []zkItem  `json:"items"`
}

// formatZenkit is WeKan's zenkit formatter: stages and items, the shape its
// importer reads. One JSON document, sent as WeKan's sendJsonResult sends it.
func formatZenkit(d extBoard) []byte {
	out := zkBoard{Title: d.Board.Title, Stages: []zkStage{}, Items: []zkItem{}}
	for _, l := range d.Lists {
		out.Stages = append(out.Stages, zkStage{Name: l.Title})
	}
	for _, i := range d.Items {
		it := zkItem{ID: i.CardID, Title: i.Title, Description: i.Description, StageName: i.ListTitle, Due: i.DueAt,
			Tags: append([]string{}, i.Labels...), Start: i.StartAt, CreatedAt: i.CreatedAt, ParentID: i.ParentCardID}
		for _, p := range append([]string{i.Owner}, i.Assignees...) {
			if p != "" {
				it.Assignees = append(it.Assignees, p)
			}
		}
		for _, c := range i.Checklists {
			zc := zkChecklist{Name: c.Title, Items: []zkChecklistItem{}}
			for _, x := range c.Items {
				zc.Items = append(zc.Items, zkChecklistItem{Text: x.Title, Checked: x.Done})
			}
			it.Checklists = append(it.Checklists, zc)
		}
		it.Comments = i.Comments
		out.Items = append(out.Items, it)
	}
	return jsJSON(out, "")
}
