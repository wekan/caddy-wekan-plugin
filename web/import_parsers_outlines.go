package web

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// OPML and Leo outlines, text sources as Node.js WeKan's opmlOutline.js and
// leoOutline.js read them with htmlparser2 in XML mode: entities decoded, no
// DTD or external entity ever resolved - checked against WeKan's own parsers.

func init() {
	importTextParsers["opml"] = parseOpml
	importTextParsers["leo"] = parseLeo
}

// xmlNode is htmlparser2's DOM, as much as the outlines read: an element with
// its attributes and children, or text.
type xmlNode struct {
	name     string
	attrs    map[string]string
	children []*xmlNode
	text     string
	isText   bool
}

// parseXMLTree reads XML leniently - an undefined entity or an unclosed tag
// does not stop it - and never resolves a DTD or an external entity.
func parseXMLTree(text string) *xmlNode {
	dec := xml.NewDecoder(strings.NewReader(keepCarriageReturns(text)))
	dec.Strict = false
	dec.AutoClose = nil
	// No entity table: as htmlparser2 in XML mode, &nbsp; stays text.
	root := &xmlNode{}
	stack := []*xmlNode{root}
	for {
		tok, err := dec.RawToken()
		if err != nil {
			if err != io.EOF {
				break
			}
			break
		}
		top := stack[len(stack)-1]
		switch t := tok.(type) {
		case xml.StartElement:
			n := &xmlNode{name: xmlName(t.Name), attrs: map[string]string{}}
			for _, a := range t.Attr {
				n.attrs[xmlName(a.Name)] = a.Value
			}
			top.children = append(top.children, n)
			stack = append(stack, n)
		case xml.EndElement:
			name := xmlName(t.Name)
			for i := len(stack) - 1; i > 0; i-- {
				if stack[i].name == name {
					stack = stack[:i]
					break
				}
			}
		case xml.CharData:
			top.children = append(top.children, &xmlNode{text: string(bytes.Clone(t)), isText: true})
		}
	}
	return root
}

// keepCarriageReturns writes each \r of the text between tags as &#13;: Go's
// decoder turns \r\n into \n, as XML says, and htmlparser2 keeps it.
func keepCarriageReturns(text string) string {
	if !strings.Contains(text, "\r") {
		return text
	}
	var b strings.Builder
	inTag := false
	for _, r := range text {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case r == '\r' && !inTag:
			b.WriteString("&#13;")
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func xmlName(n xml.Name) string {
	if n.Space != "" {
		return n.Space + ":" + n.Local
	}
	return n.Local
}

func (n *xmlNode) elements() []*xmlNode {
	var out []*xmlNode
	if n == nil {
		return out
	}
	for _, c := range n.children {
		if !c.isText {
			out = append(out, c)
		}
	}
	return out
}

func (n *xmlNode) child(name string) *xmlNode {
	for _, c := range n.elements() {
		if c.name == name {
			return c
		}
	}
	return nil
}

func (n *xmlNode) textOf() string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range n.children {
		if c.isText {
			b.WriteString(c.text)
		}
	}
	return b.String()
}

func (n *xmlNode) findTag(name string) *xmlNode {
	stack := n.elements()
	for len(stack) > 0 {
		node := stack[0]
		stack = stack[1:]
		if node.name == name {
			return node
		}
		stack = append(node.elements(), stack...)
	}
	return nil
}

type outlineNode struct {
	text, note string
	done       bool
	children   []*outlineNode
}

func outlineDescendants(n *outlineNode) []*outlineNode {
	var out []*outlineNode
	for _, c := range n.children {
		out = append(out, c)
		out = append(out, outlineDescendants(c)...)
	}
	return out
}

// outlineTasks is the list / card / checklist mapping both outlines share.
func outlineTasks(roots []*outlineNode, fallbackCard string, looseTitle func(*outlineNode) string) []any {
	tasks := []any{}
	for _, list := range roots {
		for _, card := range list.children {
			var checklists []any
			var loose []*outlineNode
			for _, c := range card.children {
				if len(c.children) == 0 {
					loose = append(loose, c)
				}
			}
			item := func(title string, done bool) jsObject {
				o := jsObject{}
				o.set("title", title)
				o.set("done", done)
				return o
			}
			if len(loose) > 0 {
				items := []any{}
				for _, c := range loose {
					items = append(items, item(c.text, c.done))
				}
				cl := jsObject{}
				cl.set("title", looseTitle(card))
				cl.set("items", items)
				checklists = append(checklists, cl)
			}
			for _, c := range card.children {
				if len(c.children) == 0 {
					continue
				}
				items := []any{}
				for _, d := range outlineDescendants(c) {
					items = append(items, item(d.text, d.done))
				}
				cl := jsObject{}
				cl.set("title", c.text)
				cl.set("items", items)
				checklists = append(checklists, cl)
			}
			t := jsObject{}
			title := card.text
			if title == "" {
				title = fallbackCard
			}
			t.set("title", title)
			t.set("description", card.note)
			col := list.text
			if col == "" {
				col = "Imported"
			}
			t.set("column_name", col)
			t.set("swimlane_name", "Default")
			tags := []any{}
			if card.done {
				tags = append(tags, "done")
			}
			t.set("tags", tags)
			if len(checklists) > 0 {
				t.set("checklists", checklists)
			}
			tasks = append(tasks, t)
		}
	}
	return tasks
}

func outlineColumns(roots []*outlineNode) []any {
	out := []any{}
	for _, r := range roots {
		o := jsObject{}
		t := r.text
		if t == "" {
			t = "Imported"
		}
		o.set("title", t)
		out = append(out, o)
	}
	return out
}

const maxOutlineDepth, maxOutlineNodes = 64, 100000

func parseOpml(text string) (jsObject, error) {
	if len(text) > 16<<20 {
		return nil, errors.New("OPML outline is too large")
	}
	doc := parseXMLTree(text)
	opml := doc.findTag("opml")
	body := opml.child("body")
	if opml == nil || body == nil {
		return nil, errors.New("Not an OPML outline: no <opml><body>")
	}
	unsupported := []any{}
	count := 0
	var read func(o *xmlNode, depth int, at string) (*outlineNode, error)
	read = func(o *xmlNode, depth int, at string) (*outlineNode, error) {
		count++
		if count > maxOutlineNodes {
			return nil, errors.New("OPML outline has too many items")
		}
		t, has := o.attrs["text"]
		if !has {
			t = o.attrs["title"]
		}
		n := &outlineNode{text: jsTrim(t), note: o.attrs["_note"]}
		for _, k := range []string{"_complete", "complete"} {
			n.done = n.done || strings.ToLower(o.attrs[k]) == "true"
		}
		for _, name := range []string{"url", "htmlUrl", "xmlUrl"} {
			if o.attrs[name] != "" {
				unsupported = append(unsupported, importUnsupportedObj(at+"/@"+name, "OPML "+name+" has no WeKan field"))
			}
		}
		var kids []*xmlNode
		for _, c := range o.elements() {
			if c.name == "outline" {
				kids = append(kids, c)
			}
		}
		if depth >= maxOutlineDepth {
			if len(kids) > 0 {
				unsupported = append(unsupported, importUnsupportedObj(at, "outline deeper than 64 levels"))
			}
			return n, nil
		}
		for i, k := range kids {
			c, err := read(k, depth+1, at+"/"+strconv.Itoa(i+1))
			if err != nil {
				return nil, err
			}
			n.children = append(n.children, c)
		}
		return n, nil
	}
	var roots []*outlineNode
	i := 0
	for _, o := range body.elements() {
		if o.name != "outline" {
			continue
		}
		i++
		r, err := read(o, 1, "/outline/"+strconv.Itoa(i))
		if err != nil {
			return nil, err
		}
		roots = append(roots, r)
	}
	for li, r := range roots {
		if jsTrim(r.note) != "" {
			unsupported = append(unsupported, importUnsupportedObj("/outline/"+strconv.Itoa(li+1)+"/@_note", "a list note has no WeKan field"))
		}
	}
	tasks := outlineTasks(roots, "Imported item", func(c *outlineNode) string {
		if c.text == "" {
			return "Checklist"
		}
		return c.text
	})
	name := "Imported OPML outline"
	if head := opml.child("head"); head != nil {
		if t := jsTrim(head.child("title").textOf()); t != "" {
			name = t
		}
	}
	return importDoc(name, outlineColumns(roots), tasks, []any{}, unsupported), nil
}

var leoMarked = regexp.MustCompile(`M`)

func parseLeo(text string) (jsObject, error) {
	if len(text) > 16<<20 {
		return nil, errors.New("Leo outline is too large")
	}
	doc := parseXMLTree(text)
	leo := doc.findTag("leo_file")
	vnodes := leo.child("vnodes")
	if leo == nil || vnodes == nil {
		return nil, errors.New("Not a Leo outline: no <leo_file><vnodes>")
	}
	bodies := map[string]string{}
	for _, t := range leo.child("tnodes").elements() {
		if t.name == "t" {
			bodies[t.attrs["tx"]] = t.textOf()
		}
	}
	childrenOf := map[string][]*xmlNode{}
	type headline struct {
		text   string
		marked bool
	}
	headlines := map[string]headline{}
	unsupported := []any{}
	count := 0
	var read func(v *xmlNode, depth int, ancestors map[string]bool) (*outlineNode, error)
	read = func(v *xmlNode, depth int, ancestors map[string]bool) (*outlineNode, error) {
		count++
		if count > maxOutlineNodes {
			return nil, errors.New("Leo outline has too many nodes")
		}
		gnx := v.attrs["t"]
		var kids []*xmlNode
		for _, c := range v.elements() {
			if c.name == "v" {
				kids = append(kids, c)
			}
		}
		if _, seen := childrenOf[gnx]; len(kids) > 0 && gnx != "" && !seen {
			childrenOf[gnx] = kids
		}
		if prev, seen := childrenOf[gnx]; len(kids) == 0 && gnx != "" && seen && !ancestors[gnx] {
			kids = prev
		}
		vh := v.child("vh")
		if _, seen := headlines[gnx]; vh != nil && gnx != "" && !seen {
			headlines[gnx] = headline{jsTrim(vh.textOf()), leoMarked.MatchString(v.attrs["a"])}
		}
		n := &outlineNode{text: jsTrim(vh.textOf()), note: bodies[gnx], done: leoMarked.MatchString(v.attrs["a"])}
		if first, ok := headlines[gnx]; vh == nil && ok {
			n.text, n.done = first.text, first.marked
		}
		if depth >= maxOutlineDepth {
			if len(kids) > 0 {
				unsupported = append(unsupported, importUnsupportedObj(gnx, "outline deeper than 64 levels"))
			}
			return n, nil
		}
		inside := map[string]bool{gnx: true}
		for k := range ancestors {
			inside[k] = true
		}
		for _, k := range kids {
			c, err := read(k, depth+1, inside)
			if err != nil {
				return nil, err
			}
			n.children = append(n.children, c)
		}
		return n, nil
	}
	var roots []*outlineNode
	for _, v := range vnodes.elements() {
		if v.name != "v" {
			continue
		}
		r, err := read(v, 1, map[string]bool{})
		if err != nil {
			return nil, err
		}
		roots = append(roots, r)
	}
	for _, r := range roots {
		if jsTrim(r.note) != "" {
			unsupported = append(unsupported, importUnsupportedObj(r.text, "list body text has no WeKan field"))
		}
	}
	tasks := outlineTasks(roots, "Imported node", func(c *outlineNode) string { return c.text })
	name := "Imported Leo outline"
	if h := leo.child("leo_header"); h != nil && h.attrs["wekan_board"] != "" {
		name = h.attrs["wekan_board"]
	}
	return importDoc(name, outlineColumns(roots), tasks, []any{}, unsupported), nil
}
