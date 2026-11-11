package web

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"regexp"
	"strings"
)

// An SVG attachment shown as an image, as Node.js WeKan's
// models/lib/svgSanitizer.js and server/lib/svgResponse.js do it: the file
// is served without what could run - script, foreignObject, the animation
// and frame elements and their content, any on* handler, a script URL
// (javascript:, vbscript:, livescript:, mocha:) however it is spelled with
// character references or spaces, a data: URL of a document type, a
// dangerous style, a DOCTYPE and its entities - and under a policy that lets
// nothing run or load. Parsed as XML token by token; a file that is not
// well-formed is not shown at all, but downloaded.

const maxSVGBytes = 32 << 20

var svgDropElements = map[string]bool{
	"script": true, "foreignobject": true, "iframe": true, "object": true, "embed": true, "handler": true, "set": true,
	"animate": true, "animatetransform": true, "animatemotion": true, "animatecolor": true, "discard": true,
	"meta": true, "link": true, "base": true, "frame": true, "frameset": true, "applet": true, "listener": true,
}

var svgURLAttributes = map[string]bool{"href": true, "xlink:href": true, "src": true, "action": true, "formaction": true,
	"xml:base": true, "data": true, "background": true, "poster": true}

var (
	svgScriptSchemes = regexp.MustCompile(`javascript:|vbscript:|livescript:|mocha:`)
	svgDangerousData = regexp.MustCompile(`^data:(?:text/html|application/|text/xml|image/svg\+xml;[^,]*charset)`)
	svgCSSDanger     = regexp.MustCompile(`javascript:|vbscript:|expression\(|-moz-binding|behavior:|@import|url\((?:'|")?(?:javascript|vbscript):`)
	svgInvisible     = regexp.MustCompile(`[\x00-\x20\x7f-\xa0\xad\x{200b}-\x{200f}\x{2028}\x{2029}\x{feff}]`)
)

// svgNormalized is normalizedValue: the value as a browser reads it, the
// characters it ignores in a scheme taken out (references are decoded
// already by the XML reader).
func svgNormalized(v string) string {
	return strings.ToLower(svgInvisible.ReplaceAllString(v, ""))
}

func svgQName(n xml.Name) string {
	if n.Space != "" {
		return n.Space + ":" + n.Local
	}
	return n.Local
}

// svgAttributeDangerous is attributeIsDangerous.
func svgAttributeDangerous(a xml.Attr) bool {
	name := strings.ToLower(svgQName(a.Name))
	local := strings.ToLower(a.Name.Local)
	if strings.HasPrefix(local, "on") {
		return true
	}
	v := svgNormalized(a.Value)
	if svgScriptSchemes.MatchString(v) {
		return true
	}
	if (svgURLAttributes[name] || local == "href") && svgDangerousData.MatchString(v) {
		return true
	}
	return local == "style" && svgCSSDanger.MatchString(v)
}

var errSVGNotWellFormed = errors.New("not a well-formed SVG")

// sanitizeSVG is sanitizeSvgString: the file without what could run, whether
// anything was taken out, and a sample of what was.
func sanitizeSVG(in []byte) ([]byte, bool, string, error) {
	if len(in) > maxSVGBytes {
		return nil, false, "", errSVGNotWellFormed
	}
	d := xml.NewDecoder(bytes.NewReader(in))
	d.Strict = true
	var out bytes.Buffer
	changed := false
	var removed []string
	note := func(s string) {
		changed = true
		if len(removed) < 8 {
			removed = append(removed, s)
		}
	}
	drop := 0 // inside a dropped element: its depth
	var style *bytes.Buffer
	var styleOpen string
	for {
		tok, err := d.RawToken()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, false, "", errSVGNotWellFormed
		}
		switch t := tok.(type) {
		case xml.StartElement:
			local := strings.ToLower(t.Name.Local)
			if drop > 0 {
				drop++
				continue
			}
			if svgDropElements[local] {
				note("<" + svgQName(t.Name) + ">")
				drop = 1
				continue
			}
			var b strings.Builder
			b.WriteString("<" + svgQName(t.Name))
			for _, a := range t.Attr {
				if svgAttributeDangerous(a) {
					note(svgQName(a.Name) + `="` + a.Value + `"`)
					continue
				}
				b.WriteString(" " + svgQName(a.Name) + `="`)
				_ = xml.EscapeText(&b, []byte(a.Value))
				b.WriteString(`"`)
			}
			b.WriteString(">")
			if local == "style" && style == nil {
				style, styleOpen = &bytes.Buffer{}, b.String()
				continue
			}
			if style != nil {
				style.WriteString(b.String())
				continue
			}
			out.WriteString(b.String())
		case xml.EndElement:
			if drop > 0 {
				drop--
				continue
			}
			end := "</" + svgQName(t.Name) + ">"
			if strings.EqualFold(t.Name.Local, "style") && style != nil {
				// A sheet that could run or load something goes whole.
				if svgCSSDanger.MatchString(svgNormalized(style.String())) {
					note("<style>")
				} else {
					out.WriteString(styleOpen)
					out.Write(style.Bytes())
					out.WriteString(end)
				}
				style = nil
				continue
			}
			if style != nil {
				style.WriteString(end)
				continue
			}
			out.WriteString(end)
		case xml.CharData:
			if drop > 0 {
				continue
			}
			var b bytes.Buffer
			_ = xml.EscapeText(&b, t)
			if style != nil {
				style.Write(b.Bytes())
				continue
			}
			out.Write(b.Bytes())
		case xml.Comment:
			// Not carried: nothing in a comment draws.
		case xml.ProcInst:
			if drop == 0 && t.Target == "xml" {
				out.WriteString("<?xml " + string(t.Inst) + "?>")
			} else if drop == 0 {
				note("<?" + t.Target + "?>")
			}
		case xml.Directive:
			note("<!" + strings.SplitN(string(t), " ", 2)[0])
		}
	}
	if drop > 0 || style != nil {
		return nil, false, "", errSVGNotWellFormed
	}
	return out.Bytes(), changed, strings.Join(removed, " "), nil
}

// isSVG is svgResponse.js isSvg: by its type, or its name.
func isSVG(typ, name string) bool {
	t := strings.ToLower(strings.TrimSpace(strings.SplitN(typ, ";", 2)[0]))
	n := strings.ToLower(strings.SplitN(strings.SplitN(name, "?", 2)[0], "#", 2)[0])
	return t == "image/svg+xml" || strings.HasSuffix(n, ".svg")
}
