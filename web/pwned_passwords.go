package web

// PASSWORD_CHECK_PWNED (off by default: it asks a service on the Internet):
// a new password that appears in a known breach is refused, as WeKan's
// server/lib/passwordPolicy.js. Have I Been Pwned's range API is
// k-anonymous - only the first 5 hex digits of the password's SHA-1 leave
// Weddy, a range shared by hundreds of passwords - and the comparison with
// the suffixes it answers is made here. A failed lookup lets the password
// through: an unreachable service must not stop anybody setting one.
//
// The SHA-1 is NOT how a password is stored - that is bcrypt (passwords.go)
// - and it is never stored or logged: the range API is keyed by SHA-1, so a
// slow hash could not be matched by it (WeKan's
// docs/Security/Password-Breach-Check-Digest-2026-10-31.md, CodeQL #599).
// Not recorded in Problems: a password being checked is ordinary use.

import (
	"bufio"
	"context"
	"crypto/sha1" // the range API's key, not password storage (above)
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// pwnedRangeURL is the service; tests point it at their own.
var pwnedRangeURL = "https://api.pwnedpasswords.com/range/"

// breachLookupDigest is WeKan's: the 40 upper-case hex digits the range API
// compares, in memory only.
func breachLookupDigest(password string) string {
	sum := sha1.Sum([]byte(password)) // #nosec G401 -- the range API's key, never stored
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

// pwnedRange is the breached suffixes of one 5-digit range; nil when the
// service cannot answer. Padding lines (count 0) are not breaches.
func pwnedRange(ctx context.Context, prefix string) []string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pwnedRangeURL+prefix, nil)
	if err != nil {
		return nil
	}
	// Padding makes every answer about the same size, so its size says
	// nothing about which range was asked for.
	req.Header.Set("Add-Padding", "true")
	req.Header.Set("User-Agent", "WeKan")
	// Through the SSRF guard, as every request Weddy makes.
	res, err := webhookClient().Do(req)
	if err != nil {
		return nil
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil
	}
	out := []string{}
	sc := bufio.NewScanner(res.Body)
	for sc.Scan() {
		suffix, count, ok := strings.Cut(strings.TrimSpace(sc.Text()), ":")
		n, err := strconv.Atoi(strings.TrimSpace(count))
		if ok && err == nil && n > 0 && len(suffix) == 35 {
			out = append(out, strings.ToUpper(suffix))
		}
	}
	return out
}

// pwned: does the password appear in a known breach? False when the
// service cannot tell.
func pwned(ctx context.Context, password string) bool {
	digest := breachLookupDigest(password)
	for _, s := range pwnedRange(ctx, digest[:5]) {
		if s == digest[5:] {
			return true
		}
	}
	return false
}
