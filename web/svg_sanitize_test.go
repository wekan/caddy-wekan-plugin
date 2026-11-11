package web

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// sanitizeSVG takes out what could run, however it is spelled, and keeps
// the drawing.
func TestSanitizeSVG(t *testing.T) {
	cases := []struct {
		name, in string
		gone     []string
		kept     []string
	}{
		{"the attack", `<svg xmlns="http://www.w3.org/2000/svg" width="40" onload="x()"><script>window.y=2</script><rect width="40" fill="#2b6cb0"/></svg>`,
			[]string{"<script", "onload", "window.y"}, []string{"<rect", `fill="#2b6cb0"`, `width="40"`}},
		{"a referenced scheme", `<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink"><a xlink:href="jav&#x61;script:alert(1)"><circle r="2"/></a></svg>`,
			[]string{"script:", "alert"}, []string{"<circle"}},
		{"a scheme split by spaces", `<svg xmlns="http://www.w3.org/2000/svg"><a href="java&#9;script:alert(1)">x</a></svg>`,
			[]string{"alert"}, []string{">x</a>"}},
		{"a document data URL", `<svg xmlns="http://www.w3.org/2000/svg"><image href="data:text/html,&lt;script&gt;"/></svg>`,
			[]string{"data:text/html"}, []string{"<image"}},
		{"an image data URL stays", `<svg xmlns="http://www.w3.org/2000/svg"><image href="data:image/png;base64,iVBOR"/></svg>`,
			nil, []string{"data:image/png;base64,iVBOR"}},
		{"a style that runs", `<svg xmlns="http://www.w3.org/2000/svg"><rect style="background:url(javascript:x)"/><style>@import url(evil.css);</style><style>rect{fill:red}</style></svg>`,
			[]string{"javascript", "@import"}, []string{"rect{fill:red}", "<rect"}},
		{"foreignObject and its content", `<svg xmlns="http://www.w3.org/2000/svg"><foreignObject><div onclick="x()">html</div></foreignObject><g/></svg>`,
			[]string{"foreignObject", "html", "onclick"}, []string{"<g>"}},
		{"animation that sets an href", `<svg xmlns="http://www.w3.org/2000/svg"><a><set attributeName="href" to="javascript:x"/>t</a></svg>`,
			[]string{"<set", "javascript"}, []string{">t</a>"}},
	}
	for _, c := range cases {
		out, _, _, err := sanitizeSVG([]byte(c.in))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		for _, g := range c.gone {
			if strings.Contains(strings.ToLower(string(out)), strings.ToLower(g)) {
				t.Errorf("%s: %q left in %s", c.name, g, out)
			}
		}
		for _, k := range c.kept {
			if !strings.Contains(string(out), k) {
				t.Errorf("%s: %q lost from %s", c.name, k, out)
			}
		}
	}
	// Negatives: a DOCTYPE's entities and a file that is not XML are not shown.
	for _, bad := range []string{
		`<!DOCTYPE svg [<!ENTITY x "&#106;avascript:a">]><svg xmlns="http://www.w3.org/2000/svg"><a href="&x;">t</a></svg>`,
		`<svg><script>`, `not svg at all <`,
	} {
		if out, _, _, err := sanitizeSVG([]byte(bad)); err == nil && strings.Contains(string(out), "avascript") {
			t.Errorf("negative: %q came out as %s", bad, out)
		}
	}
	if _, changed, _, err := sanitizeSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect/></svg>`)); err != nil || changed {
		t.Errorf("a clean drawing is unchanged: %v %v", changed, err)
	}
}

// An SVG attachment is served sanitized, inline, as an image, under a policy
// that lets nothing run; what was taken out is in Admin Panel -> Problems,
// by who uploaded it; asked for as a download, it is one.
func TestServeSVGAttachment(t *testing.T) {
	w := newTestWeKan(t)
	w.s.FilesDir = t.TempDir()
	u := w.seedUser(t, "ann", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "SVG")
	l, _ := w.s.AddList(ctx, b, "L")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	c, _ := w.s.AddCard(ctx, b, u, l.ID, sw.ID, "C")
	dir := filepath.Join(w.s.FilesDir, "attachments")
	_ = os.MkdirAll(dir, 0o755)
	evil := `<svg xmlns="http://www.w3.org/2000/svg" width="40" height="40" onload="window.__svgRan=1"><script>window.__svgRan=2</script><rect width="40" height="40" fill="#2b6cb0"/></svg>`
	_ = os.WriteFile(filepath.Join(dir, "svgfile0001"), []byte(evil), 0o644)
	_, _ = w.db.Collection("attachments").InsertOne(ctx, bson.M{"_id": "svgfile0001", "name": "evil.svg", "type": "image/svg+xml", "size": len(evil), "userId": u.ID,
		"meta":     bson.M{"boardId": b.ID, "cardId": c.ID, "listId": l.ID, "swimlaneId": sw.ID},
		"versions": bson.M{"original": bson.M{"path": filepath.Join(dir, "svgfile0001"), "storage": "fs"}}})
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	res, err := ann.Get(w.srv.URL + "/cdn/storage/attachments/svgfile0001")
	if err != nil {
		t.Fatal(err)
	}
	got := body(t, res)
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "image/svg+xml" || res.Header.Get("Content-Disposition") != "inline" {
		t.Fatalf("an inline image: %d %s %s", res.StatusCode, res.Header.Get("Content-Type"), res.Header.Get("Content-Disposition"))
	}
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "sandbox") {
		t.Errorf("the policy: %s", csp)
	}
	if regexp.MustCompile(`(?i)<script|onload`).MatchString(got) || !strings.Contains(got, "<rect") {
		t.Errorf("sanitized: %s", got)
	}
	if n, _ := w.db.Collection("eventlog").CountDocuments(ctx, bson.M{"bleed": "FileBleed", "source": "fileView"}); n != 1 {
		t.Errorf("what was taken out, in Admin Panel -> Problems: %d", n)
	}
	res, _ = ann.Get(w.srv.URL + "/cdn/storage/attachments/svgfile0001?download=true")
	_ = body(t, res)
	if res.Header.Get("Content-Type") != "application/octet-stream" || !strings.HasPrefix(res.Header.Get("Content-Disposition"), "attachment") {
		t.Errorf("a download: %s %s", res.Header.Get("Content-Type"), res.Header.Get("Content-Disposition"))
	}
	// Negative: a file that is not well-formed is downloaded, never shown.
	_ = os.WriteFile(filepath.Join(dir, "svgfile0001"), []byte(`<svg><script>x`), 0o644)
	res, _ = ann.Get(w.srv.URL + "/cdn/storage/attachments/svgfile0001")
	_ = body(t, res)
	if res.Header.Get("Content-Type") == "image/svg+xml" {
		t.Error("negative: a broken SVG is not shown as one")
	}
}

// The attachment menu: an image as the card's cover, then none, by whoever
// may change the card; the board's background by a board admin only; and
// the board's style names nothing but its own attachments.
func TestAttachmentCoverAndBackground(t *testing.T) {
	w := newTestWeKan(t)
	u := w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	ctx := context.Background()
	b, _ := w.s.CreateBoard(ctx, u, "Cover")
	_, _ = w.db.Collection("boards").UpdateOne(ctx, bson.M{"_id": b.ID}, bson.M{"$push": bson.M{"members": bson.M{"userId": bob.ID, "isActive": true}}})
	l, _ := w.s.AddList(ctx, b, "L")
	var sw Swimlane
	_ = w.db.Collection("swimlanes").FindOne(ctx, bson.M{"boardId": b.ID}).Decode(&sw)
	c, _ := w.s.AddCard(ctx, b, u, l.ID, sw.ID, "C")
	meta := bson.M{"boardId": b.ID, "cardId": c.ID, "listId": l.ID, "swimlaneId": sw.ID}
	_, _ = w.db.Collection("attachments").InsertMany(ctx, []any{
		bson.M{"_id": "image00001", "name": "p.png", "type": "image/png", "size": 1, "meta": meta},
		bson.M{"_id": "text000001", "name": "t.txt", "type": "text/plain", "size": 1, "meta": meta},
	})
	cover := func() string {
		var d Card
		_ = w.db.Collection("cards").FindOne(ctx, bson.M{"_id": c.ID}).Decode(&d)
		return d.CoverID
	}
	bg := func() string {
		var d Board
		_ = w.db.Collection("boards").FindOne(ctx, bson.M{"_id": b.ID}).Decode(&d)
		return d.BackgroundImageURL
	}
	post := func(cl *http.Client, p string) int {
		r, err := cl.PostForm(w.srv.URL+p, nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	bobc := w.client(t)
	body(t, w.signIn(t, bobc, "bob", "pw"))
	if code := post(bobc, "/attachments/image00001/cover"); code != http.StatusSeeOther || cover() != "image00001" {
		t.Errorf("a member makes it the cover: %d %q", code, cover())
	}
	if code := post(bobc, "/attachments/image00001/cover"); code != http.StatusSeeOther || cover() != "" {
		t.Errorf("and none again: %d", code)
	}
	if code := post(bobc, "/attachments/text000001/cover"); code != http.StatusBadRequest {
		t.Errorf("negative: a text file is no cover: %d", code)
	}
	if code := post(bobc, "/attachments/image00001/background"); code != http.StatusForbidden || bg() != "" {
		t.Errorf("negative: a member sets no background: %d", code)
	}
	ann := w.client(t)
	body(t, w.signIn(t, ann, "ann", "pw"))
	if code := post(ann, "/attachments/image00001/background"); code != http.StatusSeeOther || bg() != "/cdn/storage/attachments/image00001" {
		t.Errorf("the admin sets the background: %d %q", code, bg())
	}
	if !strings.Contains((Board{BackgroundImageURL: "/cdn/storage/attachments/image00001"}).BackgroundStyle(), "url(/cdn/storage/attachments/image00001)") {
		t.Error("the board's style")
	}
	for _, bad := range []string{"https://evil.example/x.png", "/cdn/storage/attachments/x);background:url(evil", "javascript:x"} {
		if s := (Board{BackgroundImageURL: bad}).BackgroundStyle(); s != "" {
			t.Errorf("negative: %q gave the style %q", bad, s)
		}
	}
}

// Negative, tree-wide: only serveSVG answers an SVG as one.
func TestOnlyServeSVGAnswersSVG(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, _ := os.ReadFile(f)
		for i, line := range strings.Split(string(src), "\n") {
			if strings.Contains(line, `"Content-Type", "image/svg+xml"`) && f != "attachments.go" {
				t.Errorf("%s:%d serves image/svg+xml outside serveSVG", f, i+1)
			}
		}
	}
	src, _ := os.ReadFile("attachments.go")
	if n := strings.Count(string(src), `"Content-Type", "image/svg+xml"`); n != 1 {
		t.Errorf("image/svg+xml set once, in serveSVG: %d", n)
	}
}
