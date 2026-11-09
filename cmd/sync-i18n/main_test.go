package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A language's file is the one WeKan's loader table names - or-IN is
// or_IN.i18n.json - else <tag>.i18n.json; a language with no file is not offered.
func TestLanguageFilesFromWeKansLoaders(t *testing.T) {
	wekan, dir := t.TempDir(), t.TempDir()
	data := filepath.Join(wekan, "imports", "i18n", "data")
	write := func(path, body string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(wekan, "imports", "i18n", "languages.js"), `[
  ["fi", "fi", "fi", "Suomi", false],
  ["or-IN", "or", "or-IN", "ଓଡିଆ (ଭାରତ)", false],
  ["ca-valencia", "ca", "ca-valencia", "Valencià", false],
  ["xx", "xx", "xx", "Nobody", false],
];
  "or-IN": () => import("./data/or_IN.i18n.json"),
  "ca-valencia": () => import("./data/ca@valencia.i18n.json"),
`)
	write(filepath.Join(data, "en.i18n.json"), `{"save":"Save"}`)
	write(filepath.Join(data, "fi.i18n.json"), `{"save":"Tallenna"}`)
	write(filepath.Join(data, "or_IN.i18n.json"), `{"save":"ସଞ୍ଚୟ"}`)
	write(filepath.Join(data, "ca@valencia.i18n.json"), `{"save":"Guarda"}`)
	write(filepath.Join(dir, "templates", "x.pug"), `p {{t $.L "save"}}`)
	if err := run(wekan, dir); err != nil {
		t.Fatal(err)
	}
	for tag, want := range map[string]string{"fi": "Tallenna", "or-IN": "ସଞ୍ଚୟ", "ca-valencia": "Guarda"} {
		b, err := os.ReadFile(filepath.Join(dir, "i18n", tag+".json"))
		if err != nil || len(b) == 0 || !contains(string(b), want) {
			t.Errorf("%s is offered from its file: %s %v", tag, b, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "i18n", "xx.json")); err == nil {
		t.Error("negative: a language with no file is not offered")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
