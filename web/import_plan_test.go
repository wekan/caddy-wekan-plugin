package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The same tasks WeKan's importedTaskPlan.js was given (testdata/import/
// plan-input.json; generate.mjs writes its output): Weddy's plan, byte for byte.
func TestImportPlanIsWeKans(t *testing.T) {
	raw, err := os.ReadFile("testdata/import/plan-input.json")
	if err != nil {
		t.Fatal(err)
	}
	input, err := decodeJS(raw)
	if err != nil {
		t.Fatal(err)
	}
	members := map[string]string{}
	for _, kv := range jsGet(input, "members").(jsObject) {
		members[kv.Key] = kv.Value.(string)
	}
	strs := func(v any) []string {
		var out []string
		for _, s := range v.([]any) {
			out = append(out, s.(string))
		}
		return out
	}
	boardMembers, allowed := strs(jsGet(input, "boardMemberIds")), strs(jsGet(input, "allowedColors"))
	tasks := jsGet(input, "tasks").([]any)
	var plans, values []any
	for _, task := range tasks {
		plans = append(plans, planImportedTask(task, members, allowed, boardMembers))
	}
	fields := planImportedCustomFields(tasks)
	for _, task := range tasks {
		values = append(values, importedCustomFieldValues(task, fields.Fields))
	}
	out := jsObject{}
	out.set("tasks", plans)
	out.set("labels", planImportedLabels(tasks, allowed))
	out.set("customFields", fields)
	out.set("customFieldValues", values)
	out.set("links", planImportedLinks(tasks))
	got := string(jsJSON(out, "  ")) + "\n"
	want, err := os.ReadFile("testdata/import/plan.golden")
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("Weddy's plan:\n%s\nWeKan's:\n%s", got, want)
	}
}

// decodeJS keeps an object's keys in JavaScript's order: array indexes
// ascending first, the rest as written.
func TestDecodeJSKeyOrder(t *testing.T) {
	v, err := decodeJS([]byte(`{"b":1,"10":2,"a":3,"2":4,"01":5}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(jsJSON(v, "")); got != `{"2":4,"10":2,"b":1,"a":3,"01":5}` {
		t.Errorf("JavaScript's order: %s", got)
	}
}

// Each source's parser given the inputs WeKan's was (testdata/import/
// parser-cases.json; generate-parsers.mjs writes WeKan's output): Weddy's
// document, byte for byte.
func TestImportParsersAreWeKans(t *testing.T) {
	raw, err := os.ReadFile("testdata/import/parser-cases.json")
	if err != nil {
		t.Fatal(err)
	}
	cases, err := decodeJS(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range cases.(jsObject) {
		parse, ok := importParsers[kv.Key]
		textParse, isText := importTextParsers[kv.Key]
		if !ok && !isText {
			t.Errorf("%s: Weddy has no parser", kv.Key)
			continue
		}
		for _, in := range kv.Value.([]any) {
			name := in.(string)
			text, err := os.ReadFile("testdata/import/" + name)
			if err != nil {
				t.Fatal(err)
			}
			var got jsObject
			if isText {
				got, err = textParse(string(text))
			} else {
				doc, derr := decodeJS(text)
				if derr != nil {
					t.Fatal(derr)
				}
				got, err = parse(doc)
			}
			if err != nil {
				t.Fatalf("%s %s: %v", kv.Key, name, err)
			}
			base := filepath.Base(name)
			base = strings.TrimSuffix(base, filepath.Ext(base))
			want, err := os.ReadFile("testdata/import/" + kv.Key + "-" + base + ".parsed.golden")
			if err != nil {
				t.Fatal(err)
			}
			// Weddy has no sprints yet: WeKan's Scrum planning of a source
			// (scrumTransfer, scrumLosses) is not read, and not compared.
			if w, err := decodeJS(want); err == nil {
				if obj, ok := w.(jsObject); ok {
					kept := jsObject{}
					for _, kv := range obj {
						if kv.Key != "scrumTransfer" && kv.Key != "scrumLosses" {
							kept = append(kept, kv)
						}
					}
					want = append(jsJSON(kept, "  "), '\n')
				}
			}
			if g := string(jsJSON(got, "  ")) + "\n"; g != string(want) {
				t.Errorf("%s %s:\n%s\nWeKan's:\n%s", kv.Key, name, g, want)
			}
		}
	}
}
