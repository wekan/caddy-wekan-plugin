package web

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"
)

// cdRevive is the generator's revive: {"$date": iso} is a date.
func cdRevive(v any) any {
	switch x := v.(type) {
	case []any:
		for i := range x {
			x[i] = cdRevive(x[i])
		}
		return x
	case map[string]any:
		if d, ok := x["$date"].(string); ok && len(x) == 1 {
			t, _ := time.Parse(time.RFC3339Nano, d)
			return t.UTC()
		}
		for k := range x {
			x[k] = cdRevive(x[k])
		}
		return x
	}
	return v
}

// The card document of every case in testdata/export/card-documents.json,
// as Node.js WeKan's buildExportCardDocument writes it
// (generate-card-documents.mjs): every section, pair and run.
func TestCardDocumentIsWeKans(t *testing.T) {
	_ = cardDocKeys
	translations.load()
	raw, _ := os.ReadFile("testdata/export/card-documents.json")
	var cases []struct {
		Name   string            `json:"name"`
		Fields []string          `json:"fields"`
		Users  map[string]string `json:"users"`
		Data   map[string]any    `json:"data"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	gold, _ := os.ReadFile("testdata/export/card-documents.golden")
	var golden map[string]json.RawMessage
	if err := json.Unmarshal(gold, &golden); err != nil || len(golden) != len(cases) {
		t.Fatalf("the golden: %v", err)
	}
	// Every key the document asks for is one Weddy carries.
	en := translations.table("en")
	for _, k := range []string{"card-number", "creator", "vote-question", "location-latitude", "export-card-subtasks"} {
		if en[k] == "" {
			t.Errorf("negative: Weddy's i18n lacks %s", k)
		}
	}
	for _, c := range cases {
		d := cdRevive(c.Data).(map[string]any)
		in := cardExportInput{Board: d["board"], List: d["list"], Card: d["card"], Swimlane: d["swimlane"],
			Checklists: cdList(d["checklists"]), ChecklistItems: cdList(d["checklistItems"]), Subtasks: cdList(d["subtasks"]),
			Comments: cdList(d["comments"]), Attachments: cdList(d["attachments"]), CustomFieldsByID: map[string]any{}}
		if m, ok := d["customFieldsById"].(map[string]any); ok {
			in.CustomFieldsByID = m
		}
		users := c.Users
		doc := buildExportCardDocument(in, cardExportOptions{
			Fields: c.Fields,
			UserName: func(id string) string {
				if n := users[id]; n != "" {
					return n
				}
				return id
			},
			FormatDate: func(v any) string {
				if tm, ok := v.(time.Time); ok {
					return tm.UTC().Format("2006-01-02T15:04:05.000Z") + " UTC"
				}
				return ""
			},
			CustomValue: func(_, v any) any { return v },
			Translate:   cardDocTranslate("en"),
		})
		g, _ := json.Marshal(doc)
		var a, b any
		_ = json.Unmarshal(g, &a)
		_ = json.Unmarshal(golden[c.Name], &b)
		if !reflect.DeepEqual(a, b) {
			w, _ := json.Marshal(b)
			t.Errorf("%s:\n%s\nnot WeKan's\n%s", c.Name, g, w)
		}
	}
}
