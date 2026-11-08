package web

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// With import off in Admin Panel, every import is refused before it reads
// anything, and nothing is made.
func TestImportDisabledRefusesEveryImport(t *testing.T) {
	w := newTestWeKan(t)
	w.seedUser(t, "ann", "pw")
	a := login(t, w, "ann")
	ctx := context.Background()
	_, _ = w.db.Collection("settings").UpdateOne(ctx, bson.M{}, bson.M{"$set": bson.M{"disableAllImport": true}})
	before, _ := w.db.Collection("boards").CountDocuments(ctx, bson.M{})
	for _, url := range []string{"/api/boards/import", "/api/import/zip"} {
		req, _ := http.NewRequest("POST", w.srv.URL+url, strings.NewReader(`{"_format":"wekan-board-1.0.0","title":"X"}`))
		req.Header.Set("Authorization", "Bearer "+a)
		req.Header.Set("Content-Type", "application/json")
		res, err := testClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 403 || !strings.Contains(string(body), "import-disabled") {
			t.Errorf("negative: %s with import off: %d %s", url, res.StatusCode, body)
		}
	}
	if after, _ := w.db.Collection("boards").CountDocuments(ctx, bson.M{}); after != before {
		t.Errorf("negative: no board is made: %d -> %d", before, after)
	}
}

// The shape, anywhere in the package: a route whose path imports, registered
// without the switch.
func TestEveryImportRouteIsImportable(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	route := regexp.MustCompile(`HandleFunc\("(?:GET|POST|PUT) ([^"]*)", (.+)\)$`)
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, _ := os.ReadFile(f)
		for _, line := range strings.Split(string(src), "\n") {
			m := route.FindStringSubmatch(strings.TrimSpace(line))
			if m == nil || !strings.Contains(m[1], "import") {
				continue
			}
			checked++
			if !strings.HasPrefix(m[2], "s.importable(") {
				t.Errorf("%s: %s is not behind the Admin Panel import switch", f, m[1])
			}
		}
	}
	if checked < 2 {
		t.Fatalf("the guard sees the import routes: %d", checked)
	}
}
