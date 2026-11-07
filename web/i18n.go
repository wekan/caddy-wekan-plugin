package web

import (
	"embed"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
)

// i18n/ is WeKan's own translations - only the keys used here - copied by
// `go run ./cmd/sync-i18n /path/to/wekan`. A translation is written in WeKan
// (imports/i18n/data) and nowhere else.
//
//go:embed i18n/*.json
var i18nFiles embed.FS

// Language is one language WeKan offers.
type Language struct {
	Code string `json:"code"`
	Tag  string `json:"tag"`
	Name string `json:"name"`
	RTL  bool   `json:"rtl"`
}

type catalog struct {
	once      sync.Once
	languages []Language
	byKey     map[string]*Language // lower-case code and tag, '_' as '-'
	mu        sync.Mutex
	strings   map[string]map[string]string
}

var translations catalog

func langKey(s string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "_", "-")
}

func (c *catalog) load() {
	c.once.Do(func() {
		b, err := i18nFiles.ReadFile("i18n/languages.json")
		if err == nil {
			_ = json.Unmarshal(b, &c.languages)
		}
		c.byKey = map[string]*Language{}
		for i := range c.languages {
			l := &c.languages[i]
			c.byKey[langKey(l.Tag)] = l
			if _, taken := c.byKey[langKey(l.Code)]; !taken {
				c.byKey[langKey(l.Code)] = l
			}
		}
		c.strings = map[string]map[string]string{}
	})
}

// lookup is the language of a code or tag: exactly, else its base ("pt-PT" -> "pt").
func (c *catalog) lookup(code string) *Language {
	c.load()
	k := langKey(code)
	if l := c.byKey[k]; l != nil {
		return l
	}
	if base, _, ok := strings.Cut(k, "-"); ok {
		return c.byKey[base]
	}
	return nil
}

func (c *catalog) table(tag string) map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.strings[tag]; ok {
		return t
	}
	t := map[string]string{}
	if b, err := i18nFiles.ReadFile("i18n/" + tag + ".json"); err == nil {
		_ = json.Unmarshal(b, &t)
	}
	c.strings[tag] = t
	return t
}

// tr is key in the language tag, falling back to English, then to the key.
func tr(tag, key string) string {
	translations.load()
	if v := translations.table(tag)[key]; v != "" {
		return v
	}
	if v := translations.table("en")[key]; v != "" {
		return v
	}
	return key
}

// languageOf is the language a page is shown in: the user's profile.language, as
// WeKan keeps it, else the browser's Accept-Language, else English.
func languageOf(r *http.Request, u *User) *Language {
	if u != nil && u.Profile.Language != "" {
		if l := translations.lookup(u.Profile.Language); l != nil {
			return l
		}
	}
	for _, part := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		code, _, _ := strings.Cut(part, ";")
		if l := translations.lookup(code); l != nil {
			return l
		}
	}
	if l := translations.lookup("en"); l != nil {
		return l
	}
	return &Language{Code: "en", Tag: "en", Name: "English"}
}

// Languages is every language WeKan offers, by tag.
func Languages() []Language {
	translations.load()
	return translations.languages
}
