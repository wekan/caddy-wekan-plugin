package web

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The same board WeKan's own formatters were given (.tools/tmp/mc/fmt.mjs):
// their output, byte for byte.
func TestExternalFormatsAreWeKans(t *testing.T) {
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	d := extBoard{Title: `Plan & <Go> "q"`, Lists: []string{"To Do", "Done"}, Items: []extItem{
		{Title: "Write   spec", Description: "line one\n\nline \"two\" <b>", ListTitle: "To Do", Labels: []string{"priority:A", "web dev", "@home"},
			DueAt: at("2026-11-20T10:00:00Z"), StartAt: at("2026-11-10T00:00:00Z"), CreatedAt: at("2026-11-01T08:00:00Z"),
			Checklists: []extChecklist{{Title: "Steps", Items: []struct {
				Title string
				Done  bool
			}{{"a & b", true}, {"c", false}}}}},
		{Title: "Ship", ListTitle: "Done", Labels: []string{"priority:B"}, EndAt: at("2026-11-05T00:00:00Z"), CreatedAt: at("2026-11-02T00:00:00Z")},
	}}
	for name, c := range map[string]struct{ got, want string }{
		"markdown": {formatMarkdownKanban(d), "# Plan & <Go> \"q\"\n\n## To Do\n\n- [ ] Write   spec\n  line one\n  line \"two\" <b>\n\n## Done\n\n- [x] Ship\n"},
		"opml":     {formatOpml(d), "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<opml version=\"2.0\">\n  <head>\n    <title>Plan &amp; &lt;Go&gt; &quot;q&quot;</title>\n  </head>\n  <body>\n    <outline text=\"To Do\">\n      <outline text=\"Write   spec\" _note=\"line one&#10;&#10;line &quot;two&quot; &lt;b&gt;\">\n        <outline text=\"Steps\">\n          <outline text=\"a &amp; b\" _complete=\"true\"/>\n          <outline text=\"c\"/>\n        </outline>\n      </outline>\n    </outline>\n    <outline text=\"Done\">\n      <outline text=\"Ship\" _complete=\"true\"/>\n    </outline>\n  </body>\n</opml>\n"},
		"todotxt":  {formatTodoTxt(d), "(A) 2026-11-01 Write spec +web_dev @home due:2026-11-20 t:2026-11-10 list:To_Do\nx 2026-11-05 2026-11-02 Ship pri:B list:Done\n"},
	} {
		if c.got != c.want {
			t.Errorf("%s:\n%q\nWeKan's:\n%q", name, c.got, c.want)
		}
	}
}

func TestExportFormatRoutes(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	bob := w.seedUser(t, "bob", "pw")
	a, bt := login(t, w, "ann"), login(t, w, "bob")
	b := newRESTBoard(t, w, a, "Formats")
	get := func(path, token string) (int, string, string) {
		req, _ := http.NewRequest("GET", w.srv.URL+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := testClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, _ := io.ReadAll(res.Body)
		return res.StatusCode, res.Header.Get("Content-Type"), string(data)
	}
	if code, typ, body := get("/api/boards/"+b.ID+"/export/markdown", a); code != 200 || typ != "text/markdown; charset=utf-8" || !strings.Contains(body, "## L\n\n- [ ] C") {
		t.Errorf("markdown: %d %s %q", code, typ, body)
	}
	if code, _, body := get("/api/boards/"+b.ID+"/export/opml", a); code != 200 || !strings.Contains(body, `<outline text="C"/>`) {
		t.Errorf("opml: %d %q", code, body)
	}
	if code, _, body := get("/api/boards/"+b.ID+"/export/todotxt", a); code != 200 || !strings.Contains(body, "C list:L") {
		t.Errorf("todo.txt: %d %q", code, body)
	}
	if code, _, _ := get("/api/boards/"+b.ID+"/export/markdown", ""); code != 401 {
		t.Errorf("negative: no token: %d", code)
	}
	if code, _, _ := get("/api/boards/"+b.ID+"/export/markdown", bt); code != 403 {
		t.Errorf("negative: a non-member: %d", code)
	}
	setMember(t, w, b.ID, Member{UserID: bob.ID, IsActive: true, IsNormalAssignedOnly: true})
	if code, _, _ := get("/api/boards/"+b.ID+"/export/opml", bt); code != 403 {
		t.Errorf("negative: assigned-only: %d", code)
	}
	_, _ = w.db.Collection("settings").UpdateOne(context.Background(), bson.M{}, bson.M{"$set": bson.M{"disableAllExport": true}})
	if code, _, _ := get("/api/boards/"+b.ID+"/export/todotxt", a); code != 403 {
		t.Errorf("negative: export off: %d", code)
	}
}
