package web

import (
	"bytes"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Focalboard's archive (.boardarchive's board.jsonl), as Node.js WeKan's
// focalboardFormat.js writes it: one JSON object a line - the archive's
// version, the board, its view, then each card with its text, checkbox and
// comment blocks - checked byte for byte against it.

// jsObject is a JavaScript object: its keys in the order they were set.
type jsObject []struct {
	Key   string
	Value any
}

func (o *jsObject) set(k string, v any) {
	for n := range *o {
		if (*o)[n].Key == k {
			(*o)[n].Value = v
			return
		}
	}
	*o = append(*o, struct {
		Key   string
		Value any
	}{k, v})
}

func (o jsObject) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	n := 0
	for _, kv := range o {
		// JSON.stringify leaves out a key whose value is undefined.
		if _, undef := kv.Value.(undefinedValue); undef {
			continue
		}
		if n > 0 {
			b.WriteByte(',')
		}
		n++
		b.Write(jsJSON(kv.Key, ""))
		b.WriteByte(':')
		b.Write(jsJSON(kv.Value, ""))
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// focalboardBlockID is blockId(): a type letter and 26 base-36 characters.
// As WeKan's, an astral character counts by its first UTF-16 unit only, and
// an empty source keeps FNV's offset as a signed number.
func focalboardBlockID(prefix, source string) string {
	var units []uint16
	for _, r := range source {
		if r > 0xFFFF {
			hi, _ := utf16.EncodeRune(r)
			units = append(units, uint16(hi))
		} else {
			units = append(units, uint16(r))
		}
	}
	out := ""
	for round := int32(0); len(out) < 26; round++ {
		start := int32(-2128831035) ^ round // 0x811c9dc5 ^ round, a signed 32-bit number in JavaScript
		if len(units) == 0 {
			s := strconv.FormatInt(int64(start), 36)
			out += strings.Repeat("0", max(0, 7-len(s))) + s
			continue
		}
		h := uint32(start)
		for _, u := range units {
			h ^= uint32(u)
			h *= 0x01000193
		}
		s := strconv.FormatUint(uint64(h), 36)
		out += strings.Repeat("0", max(0, 7-len(s))) + s
	}
	return prefix + out[:26]
}

func focalboardMillis(iso string) (int64, bool) {
	if t := parseISO(iso); !t.IsZero() {
		return t.UnixMilli(), true
	}
	return 0, false
}

type fbOption struct {
	ID    string `json:"id"`
	Value string `json:"value"`
	Color string `json:"color"`
}

type fbProperty struct {
	ID      string     `json:"id"`
	Name    string     `json:"name"`
	Type    string     `json:"type"`
	Options []fbOption `json:"options"`
}

func formatFocalboard(d extBoard, now int64) string {
	src := d.Board.ID
	if src == "" {
		src = d.Board.Title
	}
	if src == "" {
		src = "board"
	}
	boardID := focalboardBlockID("b", src)
	titles := []string{}
	for _, t := range append(d.listTitles(), func() []string {
		var out []string
		for _, i := range d.Items {
			out = append(out, i.ListTitle)
		}
		return out
	}()...) {
		if t != "" && !contains(titles, t) {
			titles = append(titles, t)
		}
	}
	status := fbProperty{ID: focalboardBlockID("p", boardID+":status"), Name: "Status", Type: "select", Options: []fbOption{}}
	for _, t := range titles {
		status.Options = append(status.Options, fbOption{ID: focalboardBlockID("o", boardID+":status:"+t), Value: t, Color: "propColorDefault"})
	}
	var labelNames []string
	for _, i := range d.Items {
		for _, l := range i.Labels {
			if !contains(labelNames, l) {
				labelNames = append(labelNames, l)
			}
		}
	}
	labels := fbProperty{ID: focalboardBlockID("p", boardID+":labels"), Name: "Labels", Type: "multiSelect", Options: []fbOption{}}
	for _, n := range labelNames {
		labels.Options = append(labels.Options, fbOption{ID: focalboardBlockID("o", boardID+":label:"+n), Value: n, Color: "propColorDefault"})
	}
	due := fbProperty{ID: focalboardBlockID("p", boardID+":due"), Name: "Due", Type: "date", Options: []fbOption{}}
	line := func(typ string, data any) string {
		o := jsObject{}
		o.set("type", typ)
		o.set("data", data)
		return string(jsJSON(o, ""))
	}
	base := func(id, parentID, typ, title string, fields any, created int64) jsObject {
		if created == 0 {
			created = now
		}
		o := jsObject{}
		for _, kv := range []struct {
			k string
			v any
		}{{"id", id}, {"parentId", parentID}, {"boardId", boardID}, {"createdBy", ""}, {"modifiedBy", ""}, {"schema", 1},
			{"type", typ}, {"title", title}, {"fields", fields}, {"createAt", created}, {"updateAt", created}, {"deleteAt", 0}} {
			o.set(kv.k, kv.v)
		}
		return o
	}
	version := jsObject{}
	version.set("version", 1)
	version.set("date", now)
	out := []string{string(jsJSON(version, ""))}
	title := d.Board.Title
	if title == "" {
		title = "WeKan board"
	}
	board := jsObject{}
	for _, kv := range []struct {
		k string
		v any
	}{{"id", boardID}, {"teamId", "0"}, {"channelId", ""}, {"createdBy", ""}, {"modifiedBy", ""}, {"type", "P"},
		{"minimumRole", ""}, {"title", title}, {"description", d.Board.Description}, {"icon", ""}, {"showDescription", false},
		{"isTemplate", false}, {"templateVersion", 0}, {"properties", jsObject{}}, {"cardProperties", []fbProperty{status, labels, due}},
		{"createAt", now}, {"updateAt", now}, {"deleteAt", 0}} {
		board.set(kv.k, kv.v)
	}
	out = append(out, line("board", board))
	view := jsObject{}
	filter := jsObject{}
	filter.set("operation", "and")
	filter.set("filters", []any{})
	for _, kv := range []struct {
		k string
		v any
	}{{"viewType", "board"}, {"groupById", status.ID}, {"sortOptions", []any{}}, {"visiblePropertyIds", []string{labels.ID, due.ID}},
		{"visibleOptionIds", []any{}}, {"hiddenOptionIds", []any{}}, {"collapsedOptionIds", []any{}}, {"filter", filter},
		{"cardOrder", []any{}}, {"columnWidths", jsObject{}}, {"columnCalculations", jsObject{}}, {"kanbanCalculations", jsObject{}},
		{"defaultTemplateId", ""}} {
		view.set(kv.k, kv.v)
	}
	out = append(out, line("block", base(focalboardBlockID("v", boardID+":view"), boardID, "view", "Board view", view, now)))
	for _, i := range d.Items {
		cardSrc := i.CardID
		if cardSrc == "" {
			cardSrc = i.Title
		}
		cardID := focalboardBlockID("c", cardSrc)
		props := jsObject{}
		for _, o := range status.Options {
			if o.Value == i.ListTitle {
				props.set(status.ID, o.ID)
				break
			}
		}
		var chosen []string
		for _, n := range i.Labels {
			for _, o := range labels.Options {
				if o.Value == n {
					chosen = append(chosen, o.ID)
					break
				}
			}
		}
		if len(chosen) > 0 {
			props.set(labels.ID, chosen)
		}
		from, hasFrom := focalboardMillis(i.StartAt)
		if to, ok := focalboardMillis(i.DueAt); ok {
			r := jsObject{}
			if hasFrom && from < to {
				r.set("from", from)
				r.set("to", to)
			} else {
				r.set("from", to)
			}
			props.set(due.ID, string(jsJSON(r, "")))
		}
		created, ok := focalboardMillis(i.CreatedAt)
		if !ok || created == 0 {
			created = now
		}
		var content []string
		var order []string
		if i.Description != "" {
			id := focalboardBlockID("a", cardID+":description")
			content = append(content, line("block", base(id, cardID, "text", i.Description, jsObject{}, created)))
			order = append(order, id)
		}
		for c, cl := range i.Checklists {
			for n, x := range cl.Items {
				id := focalboardBlockID("x", cardID+":"+strconv.Itoa(c)+":"+strconv.Itoa(n))
				v := jsObject{}
				v.set("value", x.Done)
				content = append(content, line("block", base(id, cardID, "checkbox", x.Title, v, created)))
				order = append(order, id)
			}
		}
		if order == nil {
			order = []string{}
		}
		fields := jsObject{}
		fields.set("icon", "")
		fields.set("properties", props)
		fields.set("contentOrder", order)
		fields.set("isTemplate", false)
		name := i.Title
		if name == "" {
			name = "Untitled"
		}
		out = append(out, line("block", base(cardID, boardID, "card", name, fields, created)))
		out = append(out, content...)
		for n, c := range i.Comments {
			if jsTrim(c.Text) == "" {
				continue
			}
			text := c.Text
			if c.Author != "" {
				text = c.Author + ": " + c.Text
			}
			when, ok := focalboardMillis(c.Date)
			if !ok || when == 0 {
				when = created
			}
			out = append(out, line("block", base(focalboardBlockID("m", cardID+":comment:"+strconv.Itoa(n)), cardID, "comment", text, jsObject{}, when)))
		}
	}
	return strings.Join(out, "\n") + "\n"
}
