package web

import (
	"errors"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"golang.org/x/net/html"
)

// The import/export boundary of Node.js WeKan (models/lib/importExportBoundary.js
// with server/lib/inputSanitizer.js): every board that leaves or enters WeKan
// passes through it, so markup cannot ride an export into another WeKan and a
// secret cannot leave in one.

var (
	transferBlockedKeys = map[string]bool{"__proto__": true, "prototype": true, "constructor": true}
	transferSecretKey   = regexp.MustCompile(`(?i)(?:password|passwd|secret|api[_-]?key|api[_-]?token|auth[_-]?token|private[_-]?key|login[_-]?token)`)
	transferLooksActive = regexp.MustCompile(`(?i)</?[a-z][^>]*>|(?:javascript|vbscript|data)\s*:|@import\b|expression\s*\(|url\s*\(`)
	cssInjection        = []*regexp.Regexp{
		regexp.MustCompile(`(?i)<style\b[^>]*>[\s\S]*?</style\b[^>]*>`),
		regexp.MustCompile(`(?i)style\s*=\s*["'][^"']*["']`),
		regexp.MustCompile(`(?i)@import\s+[^;]+;`),
		regexp.MustCompile(`(?i)url\s*\(\s*[^)]+\s*\)`),
		regexp.MustCompile(`(?i)expression\s*\(`),
		regexp.MustCompile(`(?i)javascript\s*:`),
		regexp.MustCompile(`(?i)vbscript\s*:`),
		regexp.MustCompile(`(?i)data\s*:`),
	}
	// sanitize-html's nonTextTags: their text goes with them.
	nonTextTags = map[string]bool{"script": true, "style": true, "textarea": true, "option": true, "xmp": true}
)

const (
	transferMaxDepth  = 40
	transferMaxNodes  = 4000000
	transferMaxArray  = 100000
	transferMaxString = 4 << 20
	transferMaxBinary = 512 << 20
)

// sanitizeInput is WeKan's: no tags at all (sanitize-html with allowedTags
// [], the text of script, style and the like dropped with them, the rest
// escaped), then CSS injection patterns removed.
func sanitizeInput(s string) string {
	var b strings.Builder
	z := html.NewTokenizer(strings.NewReader(s))
	skip := 0
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		tok := z.Token()
		switch tt {
		case html.StartTagToken:
			if nonTextTags[tok.Data] {
				skip++
			}
		case html.EndTagToken:
			if nonTextTags[tok.Data] && skip > 0 {
				skip--
			}
		case html.TextToken:
			if skip == 0 {
				t := strings.ReplaceAll(tok.Data, "&", "&amp;")
				t = strings.ReplaceAll(t, "<", "&lt;")
				b.WriteString(strings.ReplaceAll(t, ">", "&gt;"))
			}
		}
	}
	out := b.String()
	for _, p := range cssInjection {
		out = p.ReplaceAllString(out, "")
	}
	return strings.TrimSpace(out)
}

var errTransfer = errors.New("import/export limit exceeded")

// sanitizeTransfer walks a board export or import: documents become plain
// maps, dates times, markup in text is removed, prototype keys and - leaving
// WeKan - secret-looking keys are dropped, and the size is bounded.
func sanitizeTransfer(v any, export bool) (any, error) {
	out, _, err := transfer(v, export, true)
	return out, err
}

// plainDocs is documents as plain values, unchanged otherwise: a board copied
// within this WeKan keeps what it had.
func plainDocs(v any) (any, error) {
	out, _, err := transfer(v, false, false)
	return out, err
}

// transferWarning is one value the boundary changed, as WeKan's
// sanitizeTransferValue reports it: the JSON-pointer path and why.
type transferWarning struct{ Path, Reason string }

// transferPathKey is a key as a JSON-pointer segment, ~ and / escaped.
func transferPathKey(k string) string {
	return strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1")
}

var transferBase64 = regexp.MustCompile(`^[A-Za-z0-9+/]+={0,2}$`)

func transfer(v any, export, sanitize bool) (any, []transferWarning, error) {
	nodes := 0
	var warnings []transferWarning
	warn := func(path, reason string) { warnings = append(warnings, transferWarning{path, reason}) }
	var walk func(v any, path string, depth int) (any, error)
	walk = func(v any, path string, depth int) (any, error) {
		nodes++
		if nodes > transferMaxNodes || depth > transferMaxDepth {
			return nil, errTransfer
		}
		switch x := v.(type) {
		case nil, bool, int32, int64, int:
			return x, nil
		case float64:
			if math.IsNaN(x) || math.IsInf(x, 0) {
				warn(path, "non-finite number removed")
				return nil, nil
			}
			return x, nil
		case string:
			limit := transferMaxString
			if len(x) > limit && (strings.HasSuffix(path, "/file") || strings.HasSuffix(path, "/avatarFile") || strings.HasSuffix(path, "/excelBase64")) &&
				len(x)%4 == 0 && transferBase64.MatchString(x) {
				limit = transferMaxBinary
			}
			if len(x) > limit {
				return nil, errTransfer
			}
			if sanitize && transferLooksActive.MatchString(x) {
				clean := sanitizeInput(x)
				if clean != x {
					warn(path, "unsafe markup sanitized")
				}
				return clean, nil
			}
			return x, nil
		case time.Time:
			return x.UTC(), nil
		case bson.DateTime:
			return x.Time().UTC(), nil
		case bson.ObjectID:
			return x.Hex(), nil
		case bson.D:
			m := make(map[string]any, len(x))
			for _, e := range x {
				m[e.Key] = e.Value
			}
			return walk(m, path, depth)
		case bson.M:
			return walk(map[string]any(x), path, depth)
		case jsObject:
			// Another tool's document, in its own key order: the import
			// parsers read it in that order, as WeKan's do.
			out := jsObject{}
			for _, kv := range x {
				child := path + "/" + transferPathKey(kv.Key)
				if transferBlockedKeys[kv.Key] {
					warn(child, "prototype key removed")
					continue
				}
				if export && sanitize && transferSecretKey.MatchString(kv.Key) {
					warn(child, "secret field removed")
					continue
				}
				w, err := walk(kv.Value, child, depth+1)
				if err != nil {
					return nil, err
				}
				out = append(out, struct {
					Key   string
					Value any
				}{kv.Key, w})
			}
			return out, nil
		case map[string]any:
			out := make(map[string]any, len(x))
			for k, val := range x {
				child := path + "/" + transferPathKey(k)
				if transferBlockedKeys[k] {
					warn(child, "prototype key removed")
					continue
				}
				if export && sanitize && transferSecretKey.MatchString(k) {
					warn(child, "secret field removed")
					continue
				}
				w, err := walk(val, child, depth+1)
				if err != nil {
					return nil, err
				}
				out[k] = w
			}
			return out, nil
		case bson.A:
			return walk([]any(x), path, depth)
		case []any:
			if len(x) > transferMaxArray {
				return nil, errTransfer
			}
			out := make([]any, len(x))
			for i, e := range x {
				w, err := walk(e, path+"/"+strconv.Itoa(i), depth+1)
				if err != nil {
					return nil, err
				}
				out[i] = w
			}
			return out, nil
		case []bson.M:
			out := make([]any, len(x))
			for i, e := range x {
				out[i] = e
			}
			return walk(out, path, depth)
		case []map[string]any:
			out := make([]any, len(x))
			for i, e := range x {
				out[i] = e
			}
			return walk(out, path, depth)
		case []string:
			out := make([]any, len(x))
			for i, e := range x {
				out[i] = e
			}
			return walk(out, path, depth)
		case bson.Binary:
			return nil, nil
		}
		return nil, nil // anything else is not part of a board
	}
	out, err := walk(v, "", 0)
	return out, warnings, err
}

// secureImport is server/lib/secureTransfer.js for an import: the boundary,
// with what it removed recorded in Admin Panel -> Problems, and a file over a
// limit refused and recorded.
func (s *Server) secureImport(r *http.Request, u *User, source string, v any) (any, error) {
	out, warnings, err := transfer(v, false, true)
	uid := ""
	if u != nil {
		uid = u.ID
	}
	if err != nil {
		s.recordSecurity(r, securityEvent{Key: "transfer.limit", Action: "blocked", Source: "import:" + source, UserID: uid, Detail: err.Error()})
		return nil, err
	}
	if len(warnings) > 0 {
		var parts []string
		for i, w := range warnings {
			if i == 5 {
				break
			}
			parts = append(parts, w.Path+" "+w.Reason)
		}
		s.recordSecurity(r, securityEvent{Key: "transfer.sanitized", Action: "sanitized", Source: "import:" + source, UserID: uid,
			Detail: strconv.Itoa(len(warnings)) + " unsafe value(s): " + strings.Join(parts, "; ")})
	}
	return out, nil
}
