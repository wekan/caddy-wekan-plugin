// Package i18nkeys discovers translation keys used by the Go web application.
package i18nkeys

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var templateKey = regexp.MustCompile(`\btv? \$?\.L ["\x60]([A-Za-z0-9_.-]+)["\x60]`)

func goKeys(name string, source []byte, keys map[string]bool) error {
	file, err := parser.ParseFile(token.NewFileSet(), name, source, parser.SkipObjectResolution)
	if err != nil {
		return err
	}
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		fn, ok := call.Fun.(*ast.Ident)
		if !ok {
			return true
		}
		var index int
		switch fn.Name {
		case "tr", "trVars":
			index = 1
		case "msg":
			index = 0
		default:
			return true
		}
		if len(call.Args) > index {
			if literal, ok := call.Args[index].(*ast.BasicLit); ok && literal.Kind == token.STRING {
				if key, err := strconv.Unquote(literal.Value); err == nil {
					keys[key] = true
				}
			}
		}
		return true
	})
	return nil
}

// UsedKeys are the translation keys the files of dir/templates and dir/*.go use.
func UsedKeys(dir string) ([]string, error) {
	var files []string
	for _, pat := range []string{"templates/*.pug", "*.go"} {
		m, err := filepath.Glob(filepath.Join(dir, pat))
		if err != nil {
			return nil, err
		}
		files = append(files, m...)
	}
	set := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		if filepath.Ext(f) == ".go" {
			if err := goKeys(f, b, set); err != nil {
				return nil, err
			}
			continue
		}
		for _, m := range templateKey.FindAllSubmatch(b, -1) {
			set[string(m[1])] = true
		}
	}
	// The Rules Blocks view's Blockly messages: WeKan's own translations of
	// them, named by the mapping the page uses (blockly_message_keys.json).
	if b, err := os.ReadFile(filepath.Join(dir, "blockly_message_keys.json")); err == nil {
		var mapping map[string]string
		if err := json.Unmarshal(b, &mapping); err != nil {
			return nil, fmt.Errorf("blockly_message_keys.json: %w", err)
		}
		for _, k := range mapping {
			set[k] = true
		}
	}
	// Board Settings / Card's rows and sections, and Board Settings / Board
	// View's views: their names, from WeKan's tables (card_settings.json,
	// board_views.json, made by cmd/card-settings and cmd/board-views).
	for _, table := range []string{"card_settings.json", "board_views.json"} {
		if b, err := os.ReadFile(filepath.Join(dir, table)); err == nil {
			name := table
			var keys struct {
				I18nKeys []string `json:"i18nKeys"`
			}
			if err := json.Unmarshal(b, &keys); err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			for _, k := range keys.I18nKeys {
				set[k] = true
			}
		}
	}
	// History translates stored color names by adding the catalog prefix.
	for _, color := range strings.Fields("black blue crimson darkgreen gold gray green indigo lime magenta mistyrose navy orange paleturquoise peachpuff pink plum purple red saddlebrown silver sky slateblue white yellow") {
		set["color-"+color] = true
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, nil
}
