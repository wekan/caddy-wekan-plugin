package web

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// The attachment files an import names, downloaded as Node.js WeKan's
// models/lib/importAttachmentDownload.js and importAttachmentAuth.js do it:
// the address validated and every redirect validated again before anything
// is sent to it (FollowBleed, GHSA-j9p2-jm73-p549), through the outbound
// guard that checks each dialled address; and, for a Jira, Nextcloud Deck,
// OpenProject, Asana or GitLab import, the sign-in the member gave on the
// import page sent only to the address they typed, and dropped on a redirect
// to another origin.

const maxImportRedirects = 5

// importAttachmentSources are SOURCES: which sign-in each service asks for.
var importAttachmentSources = map[string]struct {
	url, username bool
	origin        string
}{
	"jira":        {url: true, username: true},
	"deck":        {url: true, username: true},
	"openproject": {url: true},
	"asana":       {origin: "https://app.asana.com"},
	"gitlab":      {url: true},
}

// importAttachmentAuth is data.attachmentAuth: the address, the user name
// and the token of one import.
type importAttachmentAuth struct{ URL, Username, Token string }

// importAttachmentAuthOf is isAttachmentAuthShape and its value: nil and
// true for none; false for any other shape.
func importAttachmentAuthOf(v any) (*importAttachmentAuth, bool) {
	if v == nil || isUndef(v) {
		return nil, true
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	a := &importAttachmentAuth{}
	for k, x := range m {
		s, ok := x.(string)
		if !ok || utf16Len(s) > 2048 {
			return nil, false
		}
		switch k {
		case "url":
			a.URL = s
		case "username":
			a.Username = s
		case "token":
			a.Token = s
		default:
			return nil, false
		}
	}
	return a, true
}

// originOf is originOf: an http(s) URL's origin, never one with credentials.
func originOf(raw string) string {
	u, err := url.Parse(jsTrim(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
}

type importSignIn struct {
	origin, base string
	headers      map[string]string
}

// attachmentSignIn is attachmentSignIn: one import's sign-in, or nil.
func attachmentSignIn(source string, a *importAttachmentAuth) *importSignIn {
	spec, ok := importAttachmentSources[source]
	if !ok || a == nil {
		return nil
	}
	token, user := jsTrim(a.Token), jsTrim(a.Username)
	if token == "" || (spec.username && user == "") {
		return nil
	}
	origin, base := spec.origin, spec.origin
	if spec.url {
		if origin = originOf(a.URL); origin == "" {
			return nil
		}
		base = strings.TrimRight(jsTrim(a.URL), "/")
	}
	basic := func(u, p string) string { return "Basic " + base64.StdEncoding.EncodeToString([]byte(u+":"+p)) }
	h := map[string]string{}
	switch source {
	case "jira":
		h["Authorization"] = basic(user, token)
	case "deck":
		h["Authorization"], h["OCS-APIRequest"] = basic(user, token), "true"
	case "openproject":
		h["Authorization"] = basic("apikey", token)
	case "asana", "gitlab":
		h["Authorization"] = "Bearer " + token
	}
	return &importSignIn{origin, base, h}
}

var gitlabUploadPath = regexp.MustCompile(`(?i)^(?:-/project/(\d+)|([^/?#]+(?:/[^/?#]+)*?))/uploads/([0-9a-f]{32})/([^/?#]+)$`)

// gitlabAPIUpload is gitlabApiUpload: a private project's upload link as
// its API address, on the GitLab the member typed only.
func gitlabAPIUpload(raw string, si *importSignIn) string {
	if si == nil || originOf(raw) != si.origin || !strings.HasPrefix(raw, si.base+"/") {
		return ""
	}
	m := gitlabUploadPath.FindStringSubmatch(raw[len(si.base)+1:])
	if m == nil {
		return ""
	}
	project := m[1]
	if project == "" {
		project = encodeURIComponent(m[2])
	}
	return si.base + "/api/v4/projects/" + project + "/uploads/" + m[3] + "/" + m[4]
}

// importAttachmentRequest is attachmentRequest: where one attachment is
// fetched, with which headers - or why it cannot be.
type importAttachmentRequest struct {
	url     string
	headers map[string]string
	asana   bool
	lost    string
}

func attachmentRequest(source string, att any, a *importAttachmentAuth) importAttachmentRequest {
	si := attachmentSignIn(source, a)
	with := func(u string) map[string]string {
		if si != nil && originOf(u) == si.origin {
			return si.headers
		}
		return nil
	}
	str := func(k string) string { return cdOr(attGet(att, k)) }
	if source == "asana" && str("asanaGid") != "" {
		if si != nil && regexp.MustCompile(`^\d+$`).MatchString(str("asanaGid")) {
			return importAttachmentRequest{url: si.base + "/api/1.0/attachments/" + str("asanaGid") + "?opt_fields=download_url,name", headers: si.headers, asana: true}
		}
		if str("url") != "" {
			return importAttachmentRequest{url: str("url")}
		}
		return importAttachmentRequest{lost: "not downloaded: give an Asana access token on the import page"}
	}
	if p := str("path"); p != "" {
		if si == nil {
			return importAttachmentRequest{lost: "not downloaded: give the service address and sign-in on the import page"}
		}
		if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") {
			return importAttachmentRequest{lost: "not downloaded: not a path on the service"}
		}
		u := si.origin + p
		return importAttachmentRequest{url: u, headers: with(u)}
	}
	if deck := attGet(att, "deck"); cdTruthy(deck) {
		if si == nil {
			return importAttachmentRequest{lost: "not downloaded: give the Nextcloud address and app password on the import page"}
		}
		ids := []string{cdString(attGet(deck, "boardId")), cdString(attGet(deck, "stackId")), cdString(attGet(deck, "cardId")), cdString(attGet(deck, "id"))}
		for _, id := range ids {
			if !regexp.MustCompile(`^\d+$`).MatchString(id) {
				return importAttachmentRequest{lost: "not downloaded: incomplete Deck ids"}
			}
		}
		version := "v1.0"
		if cdString(attGet(deck, "type")) == "file" {
			version = "v1.1"
		}
		u := si.base + "/index.php/apps/deck/api/" + version + "/boards/" + ids[0] + "/stacks/" + ids[1] + "/cards/" + ids[2] + "/attachments/" + ids[3]
		return importAttachmentRequest{url: u, headers: with(u)}
	}
	u := str("url")
	if u == "" {
		return importAttachmentRequest{lost: "not downloaded: no address"}
	}
	if source == "jira" && si == nil {
		return importAttachmentRequest{lost: "not downloaded: give the Jira address, email and API token on the import page"}
	}
	if source == "gitlab" {
		if api := gitlabAPIUpload(u, si); api != "" {
			return importAttachmentRequest{url: api, headers: with(api)}
		}
	}
	return importAttachmentRequest{url: u, headers: with(u)}
}

// errAttachmentBlocked is a download the guard refused.
type errAttachmentBlocked struct{ reason string }

func (e errAttachmentBlocked) Error() string { return e.reason }

// fetchImportedAttachment is fetchImportedAttachment: the file, every hop
// validated, the headers kept on the same origin only.
func fetchImportedAttachment(ctx context.Context, raw string, headers map[string]string) ([]byte, string, error) {
	cur, err := url.Parse(raw)
	if err != nil {
		return nil, "", errAttachmentBlocked{"Invalid URL format"}
	}
	origin := originOf(raw)
	for hop := 0; ; hop++ {
		if !webhookAllowLoopbackForTests {
			if why := validateOutboundURL(ctx, cur.String()); why != "" {
				return nil, "", errAttachmentBlocked{why}
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cur.String(), nil)
		if err != nil {
			return nil, "", errAttachmentBlocked{"Invalid URL format"}
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		client, err := webhookRequestClient(ctx, req)
		if err != nil {
			return nil, "", errAttachmentBlocked{err.Error()}
		}
		res, err := client.Do(req)
		if err != nil {
			if errors.Is(err, errSSRFBlocked) || strings.Contains(err.Error(), "SSRF_GUARD") {
				return nil, "", errAttachmentBlocked{errSSRFBlocked.Error()}
			}
			return nil, "", err
		}
		if res.StatusCode >= 300 && res.StatusCode < 400 && res.Header.Get("Location") != "" {
			res.Body.Close()
			if hop >= maxImportRedirects {
				return nil, "", errAttachmentBlocked{"SSRF_GUARD: too many redirects"}
			}
			next, err := cur.Parse(res.Header.Get("Location"))
			if err != nil {
				return nil, "", errAttachmentBlocked{"Invalid URL format"}
			}
			// The sign-in goes to the address the member typed, never on.
			if originOf(next.String()) != origin {
				headers = nil
			}
			cur = next
			continue
		}
		defer res.Body.Close()
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			return nil, "", errors.New("HTTP " + strconv.Itoa(res.StatusCode))
		}
		data, err := io.ReadAll(io.LimitReader(res.Body, MaxAttachmentBytes+1))
		if err != nil {
			return nil, "", err
		}
		if len(data) > MaxAttachmentBytes {
			return nil, "", errors.New("file too large")
		}
		typ := strings.TrimSpace(strings.SplitN(res.Header.Get("Content-Type"), ";", 2)[0])
		return data, typ, nil
	}
}

// recordAttachmentBlocked records an import's download the guard refused
// in Admin Panel -> Problems, as WeKan's trelloApiImport.js does with
// ssrf.attachment: an address nothing legitimate names in an export.
func (s *Server) recordAttachmentBlocked(ctx context.Context, source string, err error) {
	var blocked errAttachmentBlocked
	if s == nil || !errors.As(err, &blocked) {
		return
	}
	defer func() { _ = recover() }() // logging must never break the guard
	why := blocked.reason
	if len(why) > 160 {
		why = why[:160]
	}
	s.recordSecurityCtx(ctx, securityEvent{Key: "ssrf.attachment", Action: "blocked", Source: "import." + source + ".attachment", Detail: why})
}

// downloadImportAttachment is downloadImportAttachment: the file, or why it
// is lost.
func (s *Server) downloadImportAttachment(ctx context.Context, source string, att any, a *importAttachmentAuth) ([]byte, string, string) {
	if f := cdOr(attGet(att, "file")); f != "" {
		if data, err := base64.StdEncoding.DecodeString(f); err == nil && len(data) > 0 {
			return data, "", ""
		}
		return nil, "", "empty file in the export"
	}
	req := attachmentRequest(source, att, a)
	if req.lost != "" {
		return nil, "", req.lost
	}
	data, typ, err := fetchImportedAttachment(ctx, req.url, req.headers)
	s.recordAttachmentBlocked(ctx, source, err)
	var blocked errAttachmentBlocked
	if errors.As(err, &blocked) {
		return nil, "", "not downloaded (" + blocked.reason + ")"
	}
	if err != nil {
		return nil, "", "download failed (" + err.Error() + ")"
	}
	if !req.asana {
		return data, typ, ""
	}
	// Asana: the record names a signed, short-lived address, fetched bare.
	rec, _ := decodeJS(data)
	dl := cdOr(jsGet(jsGet(rec, "data"), "download_url"))
	if dl == "" {
		return nil, "", "not downloaded: Asana gives no download address for it (a file hosted elsewhere)"
	}
	data, typ, err = fetchImportedAttachment(ctx, dl, nil)
	s.recordAttachmentBlocked(ctx, source, err)
	if errors.As(err, &blocked) {
		return nil, "", "not downloaded (" + blocked.reason + ")"
	}
	if err != nil {
		return nil, "", "download failed (" + err.Error() + ")"
	}
	return data, typ, ""
}

// attGet is att[k] of a parsed task's jsObject or a document's map.
func attGet(v any, k string) any {
	if o, ok := v.(jsObject); ok {
		if x, ok := jsLookup(o, k); ok {
			return x
		}
		return undefinedValue{}
	}
	return cdGet(v, k)
}
