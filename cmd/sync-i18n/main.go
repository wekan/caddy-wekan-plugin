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

	"github.com/wekan/caddy-wekan-plugin/internal/i18nkeys"
)

var (
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
	if err := sync(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func sync(wekan string) error {
	if err := run(wekan, "web"); err != nil {
		return err
	}
	for _, name := range []string{"languageFlags.js", "browserLanguage.js"} {
		src, err := os.ReadFile(filepath.Join(wekan, "imports", "i18n", name))
		if err != nil {
			return err
		}
		// browserLanguage.js is CommonJS in WeKan and loaded as a module here.
		src = bytes.Replace(src, []byte("module.exports = {"), []byte("export {"), 1)
		if err := os.WriteFile(filepath.Join("web", "static", name), src, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func run(wekan, dir string) error {
	keys, err := i18nkeys.UsedKeys(dir)
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
			if _, err := os.Stat(filepath.Join(data, file)); os.IsNotExist(err) {
				file = l.Code + ".i18n.json"
			}
		}
		if err := readJSON(filepath.Join(data, file), &all); err != nil {
			continue // a language whose file is missing is not offered
		}
		sub := map[string]string{}
		for _, k := range keys {
			// English also records intentionally empty source strings.
			if v, ok := all[k]; ok && (v != "" || l.Tag == "en") {
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
