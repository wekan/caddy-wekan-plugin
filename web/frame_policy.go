package web

// FrameBleed: WeKan's pages refuse to be framed by another site, so a page
// elsewhere cannot load WeKan in an invisible frame and trick a signed-in user
// into clicking through it. Node.js WeKan does this with BROWSER_POLICY_ENABLED
// (server/policy.js, models/lib/framePolicy.js); Weddy's pages carried no
// framing header at all. Here the policy is on unless BROWSER_POLICY_ENABLED is
// "false" - Weddy is what is installed, as WeKan's Docker image and Snap are,
// and they turn it on - and TRUSTED_URL names the other origins that may frame
// it, as in WeKan.
//
// There is nothing to record in Admin Panel -> Problems: a page that frames
// WeKan never reaches it - the browser refuses the frame from the header, so
// there is no attempt for the server to see.

import (
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
)

// trustedOrigins: the http(s) origins TRUSTED_URL names, space or comma
// separated. Anything else is ignored rather than widening the policy.
func trustedOrigins(value string) []string {
	var out []string
	for _, entry := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
		u, err := url.Parse(entry)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			continue
		}
		origin := u.Scheme + "://" + strings.ToLower(u.Host)
		if !slices.Contains(out, origin) {
			out = append(out, origin)
		}
	}
	return out
}

// framingHeaders: the headers every response gets, or none when the policy
// is off.
func framingHeaders(enabled bool, trustedURL string) map[string]string {
	if !enabled {
		return nil
	}
	trusted := trustedOrigins(trustedURL)
	h := map[string]string{"Content-Security-Policy": "frame-ancestors 'self'"}
	for _, o := range trusted {
		h["Content-Security-Policy"] += " " + o
	}
	// X-Frame-Options cannot name another origin; without one it backs the CSP
	// directive up for older browsers.
	if len(trusted) == 0 {
		h["X-Frame-Options"] = "SAMEORIGIN"
	}
	return h
}

// framingHeadersFromEnv: framingHeaders from BROWSER_POLICY_ENABLED and
// TRUSTED_URL.
func framingHeadersFromEnv() map[string]string {
	return framingHeaders(!strings.EqualFold(strings.TrimSpace(os.Getenv("BROWSER_POLICY_ENABLED")), "false"), os.Getenv("TRUSTED_URL"))
}

// setFramingHeaders puts them on a response before its route runs. A route
// that sets a Content-Security-Policy of its own - a file: an attachment, an
// avatar, a thumbnail - replaces this one with its stricter one, on its 304s
// too, since it sets it before answering either (WeKan's 304 problem, a
// browser putting the app-wide policy in place of a stored file's, cannot
// happen here).
func setFramingHeaders(w http.ResponseWriter, h map[string]string) {
	for name, value := range h {
		w.Header().Set(name, value)
	}
}
