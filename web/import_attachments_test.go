package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// attachmentRequest is importAttachmentAuth.js's: each service's file
// address, its sign-in only on the address the member typed.
func TestAttachmentRequest(t *testing.T) {
	jira := &importAttachmentAuth{URL: "https://acme.atlassian.net/", Username: "ann@x.test", Token: "tok"}
	if r := attachmentRequest("jira", map[string]any{"url": "https://acme.atlassian.net/rest/api/3/attachment/content/1"}, jira); r.headers["Authorization"] == "" || r.lost != "" {
		t.Errorf("jira: the sign-in on its own address: %+v", r)
	}
	if r := attachmentRequest("jira", map[string]any{"url": "https://elsewhere.test/f"}, jira); r.headers != nil {
		t.Errorf("negative: jira's sign-in never to another address: %+v", r)
	}
	if r := attachmentRequest("jira", map[string]any{"url": "https://acme.atlassian.net/f"}, nil); !strings.Contains(r.lost, "Jira address") {
		t.Errorf("negative: jira without a sign-in: %+v", r)
	}
	deck := &importAttachmentAuth{URL: "https://cloud.test/nc/", Username: "ann", Token: "app"}
	if r := attachmentRequest("deck", map[string]any{"deck": map[string]any{"boardId": 9.0, "stackId": 1.0, "cardId": 11.0, "id": 3.0, "type": "file"}}, deck); r.url != "https://cloud.test/nc/index.php/apps/deck/api/v1.1/boards/9/stacks/1/cards/11/attachments/3" || r.headers["OCS-APIRequest"] != "true" {
		t.Errorf("deck: %+v", r)
	}
	if r := attachmentRequest("deck", map[string]any{"deck": map[string]any{"boardId": "x"}}, deck); r.lost == "" {
		t.Error("negative: incomplete Deck ids")
	}
	op := &importAttachmentAuth{URL: "https://op.test", Token: "k"}
	if r := attachmentRequest("openproject", map[string]any{"path": "/api/v3/attachments/5/content"}, op); r.url != "https://op.test/api/v3/attachments/5/content" {
		t.Errorf("openproject: %+v", r)
	}
	if r := attachmentRequest("openproject", map[string]any{"path": "//evil.test/x"}, op); r.lost == "" {
		t.Error("negative: a protocol-relative path is no path on the service")
	}
	gl := &importAttachmentAuth{URL: "https://gitlab.test", Token: "t"}
	if r := attachmentRequest("gitlab", map[string]any{"url": "https://gitlab.test/group/sub/proj/uploads/0123456789abcdef0123456789abcdef/a b.png"}, gl); r.url != "https://gitlab.test/api/v4/projects/group%2Fsub%2Fproj/uploads/0123456789abcdef0123456789abcdef/a b.png" {
		t.Errorf("gitlab: %+v", r)
	}
	if _, ok := importAttachmentAuthOf(map[string]any{"token": 5}); ok {
		t.Error("negative: a token that is not text")
	}
	if _, ok := importAttachmentAuthOf(map[string]any{"password": "x"}); ok {
		t.Error("negative: a field that is not one")
	}
}

// fetchImportedAttachment: every hop validated (FollowBleed), the sign-in
// kept on the same origin and dropped on a redirect to another, no more than
// five redirects; and a loopback address refused.
func TestFetchImportedAttachment(t *testing.T) {
	ctx := context.Background()
	if _, _, err := fetchImportedAttachment(ctx, "http://127.0.0.1:1/x", nil); err == nil {
		t.Fatal("negative: a loopback address is refused")
	} else if _, blocked := err.(errAttachmentBlocked); !blocked {
		t.Errorf("negative: refused by the guard, not tried: %v", err)
	}
	webhookAllowLoopbackForTests = true
	t.Cleanup(func() { webhookAllowLoopbackForTests = false })
	var gotAuth []string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = append(gotAuth, "other:"+r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "image/png; x=1")
		_, _ = w.Write([]byte("the file"))
	}))
	defer other.Close()
	var self *httptest.Server
	self = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = append(gotAuth, "self:"+r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, "/same", http.StatusFound)
		case "/same":
			http.Redirect(w, r, other.URL+"/file", http.StatusFound)
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		}
	}))
	defer self.Close()
	data, typ, err := fetchImportedAttachment(ctx, self.URL+"/start", map[string]string{"Authorization": "Bearer secret"})
	if err != nil || string(data) != "the file" || typ != "image/png" {
		t.Fatalf("followed: %q %q %v", data, typ, err)
	}
	if strings.Join(gotAuth, ",") != "self:Bearer secret,self:Bearer secret,other:" {
		t.Errorf("negative: the sign-in never goes to another origin: %v", gotAuth)
	}
	if _, _, err := fetchImportedAttachment(ctx, self.URL+"/loop", nil); err == nil || !strings.Contains(err.Error(), "too many redirects") {
		t.Errorf("negative: a redirect loop ends: %v", err)
	}
}

// An import's attachment files, as WeKan's createAttachments: downloaded and
// stored on the card; one the guard refuses is a loss of the import.
func TestImportDownloadsAttachments(t *testing.T) {
	w := newTestWeKan(t)
	w.s.FilesDir = filepath.Join(t.TempDir(), "files")
	_ = os.MkdirAll(w.s.attachmentsDir(), 0o755)
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	c := w.client(t)
	body(t, w.signIn(t, c, "ann", "pw"))
	webhookAllowLoopbackForTests = true
	t.Cleanup(func() { webhookAllowLoopbackForTests = false })
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("asset bytes")) }))
	defer files.Close()
	issues := []any{map[string]any{"id": 1.0, "title": "With a file", "state": "open", "description": "body",
		"assets": []any{map[string]any{"name": "notes.txt", "browser_download_url": files.URL + "/attachments/abcdef12-1234-1234-1234-123456789abc"},
			map[string]any{"name": "blocked.txt", "browser_download_url": "http://10.255.255.1/attachments/00000000-0000-0000-0000-000000000000"}}}}
	raw, _ := json.Marshal([]any{issues, map[string]any{"membersMode": "me"}, "gitea"})
	webhookAllowLoopbackForTests = true
	req, _ := http.NewRequest("POST", w.srv.URL+"/_methods/importBoard", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	res.Body.Close()
	id, _ := out["result"].(string)
	if id == "" {
		t.Fatalf("the import: %v", out)
	}
	var att Attachment
	if err := w.db.Collection("attachments").FindOne(ctx, bson.M{"meta.boardId": id, "name": "notes.txt"}).Decode(&att); err != nil {
		t.Fatalf("the downloaded file is the card's attachment: %v", err)
	}
	p, _ := w.s.attachmentFile(&att)
	if got, _ := os.ReadFile(p); string(got) != "asset bytes" {
		t.Errorf("its bytes: %q", got)
	}
	if n, _ := w.db.Collection("attachments").CountDocuments(ctx, bson.M{"meta.boardId": id, "name": "blocked.txt"}); n != 0 {
		t.Error("negative: a private address is not downloaded")
	}
	var ev bson.M
	_ = w.db.Collection("recoveryEvents").FindOne(ctx, bson.M{"boardIds": id}).Decode(&ev)
	if b, _ := json.Marshal(ev); !strings.Contains(string(b), "blocked.txt") {
		t.Errorf("the refused file is the import's loss: %s", b)
	}
	// Negative: a sign-in of another shape is a Match error.
	raw, _ = json.Marshal([]any{issues, map[string]any{"attachmentAuth": map[string]any{"token": 1}}, "gitea"})
	req, _ = http.NewRequest("POST", w.srv.URL+"/_methods/importBoard", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	res, _ = c.Do(req)
	data, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(data), "Match.Error") {
		t.Errorf("negative: attachmentAuth's shape: %s", data)
	}
}

// A WeKan export's attachment by `url`: through the guard, so one at a
// private address is never fetched (FollowBleed).
func TestWekanImportAttachmentURLIsGuarded(t *testing.T) {
	w := newTestWeKan(t)
	w.s.FilesDir = filepath.Join(t.TempDir(), "files")
	ctx := context.Background()
	w.seedUser(t, "ann", "pw")
	a := login(t, w, "ann")
	hit := false
	inside := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true; _, _ = w.Write([]byte("secret")) }))
	defer inside.Close()
	doc := `{"_format":"wekan-board-1.0.0","title":"Urls","lists":[{"_id":"l1","title":"L"}],"swimlanes":[{"_id":"s1","title":"S"}],` +
		`"cards":[{"_id":"c1","title":"C","listId":"l1","swimlaneId":"s1"}],"attachments":[{"_id":"a1","cardId":"c1","name":"x.txt","url":"` + inside.URL + `/x"}]}`
	var out struct {
		ID string `json:"_id"`
	}
	call(t, "POST", w.srv.URL+"/api/boards/import", a, `{"board":`+doc+`}`, &out)
	if out.ID == "" {
		t.Fatal("the board is imported")
	}
	if hit {
		t.Error("negative: a loopback URL in an export is never fetched")
	}
	if n, _ := w.db.Collection("attachments").CountDocuments(ctx, bson.M{"name": "x.txt"}); n != 0 {
		t.Errorf("negative: nothing stored from it: %d", n)
	} // The refusal is visible in Admin Panel -> Problems.
	if n, _ := w.db.Collection("eventlog").CountDocuments(ctx, bson.M{"bleed": "LiveBleed", "source": "import.wekan.attachment"}); n != 1 {
		t.Errorf("the blocked download is recorded once as ssrf.attachment: %d", n)
	}
}

// Negative, tree-wide: every download an import makes goes through
// fetchImportedAttachment and its refusal is recorded at once, and no import
// file dials on its own (http.Get, http.DefaultClient) around the guard.
func TestEveryImportDownloadIsGuardedAndRecorded(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	calls := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(src), "\n")
		for i, l := range lines {
			if strings.Contains(l, "fetchImportedAttachment(") && !strings.Contains(l, "func fetchImportedAttachment") && !strings.HasPrefix(strings.TrimSpace(l), "//") {
				calls++
				if i+1 >= len(lines) || !strings.Contains(lines[i+1], "recordAttachmentBlocked(") {
					t.Errorf("%s:%d: a download whose refusal is not recorded in Admin Panel -> Problems", f, i+1)
				}
			}
			if strings.HasPrefix(f, "import") && (strings.Contains(l, "http.Get(") || strings.Contains(l, "http.DefaultClient")) {
				t.Errorf("%s:%d: an import dials around the outbound guard", f, i+1)
			}
		}
	}
	if calls < 4 {
		t.Errorf("the scan found the downloads: %d", calls)
	}
}
