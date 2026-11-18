package web

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The driver decodes a stored nested document as bson.D, so a bson.M
// assertion on it finds nothing, silently: the board exports lost every
// list's WIP limit that way. asM takes a document however it was decoded.
func TestAsMTakesEveryDecoding(t *testing.T) {
	for name, v := range map[string]any{
		"bson.M": bson.M{"a": 1}, "bson.D": bson.D{{Key: "a", Value: 1}}, "JSON": map[string]any{"a": 1},
	} {
		if asM(v)["a"] != 1 {
			t.Errorf("%s: %v", name, asM(v))
		}
	}
	for _, v := range []any{nil, "x", 5, bson.A{1}} {
		if m := asM(v); m == nil || len(m) != 0 {
			t.Errorf("negative: %v gives %v, want an empty document", v, m)
		}
	}
}

func TestExportKeepsStoredWipLimit(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	u := w.seedUser(t, "ann", "pw")
	b, _ := w.s.CreateBoard(ctx, u, "WIP")
	l, _ := w.s.AddList(ctx, b, "Doing")
	_, _ = w.db.Collection("lists").UpdateOne(ctx, bson.M{"_id": l.ID}, bson.M{"$set": bson.M{"wipLimit": bson.M{"enabled": true, "value": 3}}})
	out := w.s.collectExternal(ctx, b, "markdown", exportWant{})
	for _, x := range out.Lists {
		if x.ID == l.ID {
			if x.WipLimit == nil || !x.WipLimit.Enabled || x.WipLimit.Value != 3 {
				t.Fatalf("the stored WIP limit is in the export: %+v", x.WipLimit)
			}
			return
		}
	}
	t.Fatal("the list is not in the export")
}

// Negative, over the package: a field of a document asserted as bson.M is a
// map the code built itself - a modifier, a document being written - listed
// here; anything read from the database or JSON goes through asM.
func TestNoNestedBsonMAssertionOnStoredDocuments(t *testing.T) {
	built := map[string]string{
		"api_rules.go":    "[\"$set\"] of a modifier built by ruleButtonMetadata",
		"rule_copy.go":    "[\"$set\"] of the update this file builds",
		"rules_engine.go": "[\"$set\"] of the update the rule action builds",
		"attachments.go":  "[\"meta\"] of the attachment document being inserted",
	}
	re := regexp.MustCompile(`\[[^\]]+\]\.\(bson\.M\)`)
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		for i, line := range strings.Split(string(b), "\n") {
			if re.MatchString(line) {
				if _, ok := built[f]; !ok {
					t.Errorf("%s:%d asserts a nested field as bson.M - use asM: %s", f, i+1, strings.TrimSpace(line))
				}
			}
		}
	}
}
