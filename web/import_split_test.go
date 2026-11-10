package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// "One board per project" of every parsed fixture and the split/*.json edge
// cases, as Node.js WeKan's splitBySwimlane (generate-split.mjs): the parts,
// their keys in WeKan's order, or the document whole.
func TestImportSplitIsWeKans(t *testing.T) {
	raw, err := os.ReadFile("testdata/import/split.golden")
	if err != nil {
		t.Fatal(err)
	}
	var golden map[string]json.RawMessage
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	if len(golden) < 50 {
		t.Fatalf("the golden covers the fixtures: %d", len(golden))
	}
	for name, want := range golden {
		data, err := os.ReadFile("testdata/import/" + name)
		if err != nil {
			t.Fatal(err)
		}
		v, err := decodeJS(data)
		parsed, ok := v.(jsObject)
		if err != nil || !ok {
			t.Fatalf("%s: %v", name, err)
		}
		parts, err := splitBySwimlane(parsed)
		var got any = parts
		if err != nil {
			got = map[string]string{"error": err.Error()}
		} else if len(parts) == 1 {
			got = "whole"
		}
		g, _ := json.Marshal(got)
		w, _ := decodeJS(want)
		wj, _ := json.Marshal(w)
		if string(g) != string(wj) {
			t.Errorf("%s:\n%s\nnot WeKan's\n%s", name, g, wj)
		}
	}
}

// Negative: more than 200 projects is refused, not 201 boards.
func TestImportSplitLimit(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"tasks":[`)
	for i := 0; i <= maxSplitBoards; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"title":"t","swimlane_name":"lane` + strings.Repeat("x", i) + `"}`)
	}
	b.WriteString(`]}`)
	v, _ := decodeJS([]byte(b.String()))
	if _, err := splitBySwimlane(v.(jsObject)); err == nil || !strings.Contains(err.Error(), "at most 200 boards") {
		t.Errorf("201 swimlanes: %v", err)
	}
}

// "One board per project" through importBoard's splitBy and the import
// page's checkbox, as WeKan's Kanri all-data case: each board of the file
// its own board; without it, or with another splitBy, one board.
func TestImportSplitByProject(t *testing.T) {
	w := newTestWeKan(t)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	c := w.client(t)
	body(t, w.signIn(t, c, "ann", "pw"))
	kanri := `{"activeTheme":"dark","colors":{},"pins":[],"boards":[` +
		`{"id":"a","title":"First Kanri board","columns":[{"id":"a-c","title":"Todo","cards":[{"name":"One"}]}]},` +
		`{"id":"b","title":"Second Kanri board","columns":[{"id":"b-c","title":"Todo","cards":[{"name":"Two"}]}]}]}`
	call := func(data string) map[string]any {
		req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/importBoard", strings.NewReader(`[`+kanri+`,`+data+`,"kanri"]`))
		req.Header.Set("Content-Type", "application/json")
		res, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		return out
	}
	count := func() int64 { n, _ := w.db.Collection("boards").CountDocuments(ctx, bson.M{}); return n }
	cardsOf := func(title string) []string {
		var b bson.M
		if err := w.db.Collection("boards").FindOne(ctx, bson.M{"title": title}).Decode(&b); err != nil {
			return nil
		}
		cur, _ := w.db.Collection("cards").Find(ctx, bson.M{"boardId": b["_id"]})
		var docs []bson.M
		_ = cur.All(ctx, &docs)
		var out []string
		for _, d := range docs {
			out = append(out, strOf(d["title"]))
		}
		return out
	}
	before := count()
	out := call(`{"splitBy":"swimlane","membersMode":"placeholder"}`)
	if count() != before+2 || fmt.Sprint(cardsOf("First Kanri board")) != "[One]" || fmt.Sprint(cardsOf("Second Kanri board")) != "[Two]" {
		t.Fatalf("a board a project: %v %d", out, count()-before)
	}
	var first bson.M
	_ = w.db.Collection("boards").FindOne(ctx, bson.M{"title": "First Kanri board"}).Decode(&first)
	if out["result"] != first["_id"] {
		t.Errorf("the first board answered: %v", out)
	}
	for _, data := range []string{`{}`, `{"splitBy":"project"}`, `{"splitBy":true}`} {
		before = count()
		if out := call(data); count() != before+1 {
			t.Errorf("negative: %s is one board: %v %d", data, out, count()-before)
		}
	}
	// The page: the checkbox's splitBy, with Import without mapping members.
	before = count()
	res, err := c.PostForm(w.srv.URL+"/import/kanri", url.Values{"text": {kanri}, "splitBy": {"swimlane"}, "skipMapping": {"1"}})
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if count() != before+2 {
		t.Errorf("the page's One board per project: %d boards", count()-before)
	}
	_, page := get(t, c, w.srv.URL+"/import/kanri")
	if !strings.Contains(page, `js-import-split-toggle`) || !strings.Contains(page, `name="splitBy" value=""`) {
		t.Errorf("the page offers One board per project, off")
	}
	// Negative: no paragraph holds a div - a browser closes the p at it,
	// and the checkbox's link came out twice.
	for rest := page; ; {
		i := strings.Index(rest, "<p")
		if i < 0 {
			break
		}
		rest = rest[i+2:]
		if end := strings.Index(rest, "</p>"); end >= 0 && strings.Contains(rest[:end], "<div") {
			t.Errorf("a p holds a div: %s", rest[:end])
		}
	}
}
