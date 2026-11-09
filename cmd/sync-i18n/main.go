// Command sync-i18n copies WeKan's translations into web/i18n: for every
// language, only the keys the Go WeKan uses - the pages' `t $.L "key"` and the
// code's s.tr(lang, "key") - so the binary carries kilobytes, not WeKan's 94 MB.
//
//	go run ./cmd/sync-i18n /path/to/wekan
//
// WeKan's imports/i18n/data stays the one place a translation is written; this
// copies, it never edits. web/i18n/languages.json is imports/i18n/languages.js's
// list: code, tag, the name in the language itself, and right-to-left.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	useRe  = regexp.MustCompile("(?:\\bt \\$?\\.L |\\btr\\([^;\"]*?, |\\bmsg\\()[\"`]([A-Za-z0-9_.-]+)[\"`]")
	langRe = regexp.MustCompile(`\["([^"]+)", "[^"]*", "([^"]+)", "((?:[^"\\]|\\.)*)", (true|false)\]`)
	// loadRe is languages.js's loader table: the file of each tag, which is
	// not always <tag>.i18n.json - or-IN is or_IN, ca-valencia ca@valencia.
	loadRe = regexp.MustCompile(`"([^"]+)": \(\) => import\("\./data/([^"/]+\.i18n\.json)"\)`)
)

type language struct {
	Code string `json:"code"`
	Tag  string `json:"tag"`
	Name string `json:"name"`
	RTL  bool   `json:"rtl,omitempty"`
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: go run ./cmd/sync-i18n /path/to/wekan")
		os.Exit(2)
	}
	if err := run(os.Args[1], "web"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
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
		for _, m := range useRe.FindAllSubmatch(b, -1) {
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
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, nil
}

func run(wekan, dir string) error {
	keys, err := UsedKeys(dir)
	if err != nil {
		return err
	}
	data := filepath.Join(wekan, "imports", "i18n", "data")
	en := map[string]string{}
	if err := readJSON(filepath.Join(data, "en.i18n.json"), &en); err != nil {
		return err
	}
	for _, k := range keys {
		if _, ok := en[k]; !ok {
			return fmt.Errorf("%q is used but is not in WeKan's en.i18n.json", k)
		}
	}
	src, err := os.ReadFile(filepath.Join(wekan, "imports", "i18n", "languages.js"))
	if err != nil {
		return err
	}
	langs := []language{{Code: "en", Tag: "en", Name: "English"}}
	for _, m := range langRe.FindAllStringSubmatch(string(src), -1) {
		var name string
		if err := json.Unmarshal([]byte(`"`+m[3]+`"`), &name); err != nil {
			return err
		}
		if m[2] == "en" {
			continue
		}
		langs = append(langs, language{Code: m[1], Tag: m[2], Name: name, RTL: m[4] == "true"})
	}
	files := map[string]string{}
	for _, m := range loadRe.FindAllStringSubmatch(string(src), -1) {
		files[m[1]] = m[2]
	}
	out := filepath.Join(dir, "i18n")
	if err := os.RemoveAll(out); err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	var kept []language
	for _, l := range langs {
		all := map[string]string{}
		file := files[l.Tag]
		if file == "" {
			file = l.Tag + ".i18n.json"
		}
		if err := readJSON(filepath.Join(data, file), &all); err != nil {
			continue // a language whose file is missing is not offered
		}
		sub := map[string]string{}
		for _, k := range keys {
			if v, ok := all[k]; ok && v != "" {
				sub[k] = v
			}
		}
		if err := writeJSON(filepath.Join(out, l.Tag+".json"), sub); err != nil {
			return err
		}
		kept = append(kept, l)
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].Tag < kept[j].Tag })
	if err := writeJSON(filepath.Join(out, "languages.json"), kept); err != nil {
		return err
	}
	fmt.Printf("%d keys in %d languages\n", len(keys), len(kept))
	return nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func writeJSON(path string, v any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}
