package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestBreachLookupDigest(t *testing.T) {
	// SHA-1 of "password" and of a UTF-8 text, upper-case hex.
	if d := breachLookupDigest("password"); d != "5BAA61E4C9B93F3F0682250B6CF8331B7EE68FD8" {
		t.Errorf("ascii: %s", d)
	}
	if d := breachLookupDigest("ä"); len(d) != 40 || d != strings.ToUpper(d) {
		t.Errorf("utf-8: %s", d)
	}
}

// PASSWORD_CHECK_PWNED: a breached password is refused at sign-up, only five
// hex digits leave Weddy, and an unreachable service lets the password
// through; off, nothing is asked (negative).
func TestPwnedPasswords(t *testing.T) {
	breached := "correct-horse-1"
	digest := breachLookupDigest(breached)
	var mu sync.Mutex
	var asked []string
	up := true
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.Path)
		ok := up
		mu.Unlock()
		if !ok {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		if r.Header.Get("Add-Padding") != "true" {
			t.Error("the request asks for padding")
		}
		fmt.Fprintf(w, "0000000000000000000000000000000000A:0\r\n%s:42\r\n", digest[5:])
	}))
	defer svc.Close()
	webhookAllowLoopbackForTests = true
	t.Cleanup(func() { webhookAllowLoopbackForTests = false })
	was := pwnedRangeURL
	pwnedRangeURL = svc.URL + "/range/"
	t.Cleanup(func() { pwnedRangeURL = was })

	t.Setenv("PASSWORD_CHECK_PWNED", "")
	t.Setenv("PASSWORD_MIN_LENGTH_ENFORCE", "")
	w := newTestWeKan(t)
	w.seedUser(t, "first", "pw")
	if res := signUp(t, w, w.client(t), "off", "off@example.invalid", breached); res.StatusCode != http.StatusSeeOther {
		t.Errorf("off, a breached password is set: %d", res.StatusCode)
	}
	if len(asked) != 0 {
		t.Errorf("negative: off, nothing is asked: %v", asked)
	}
	t.Setenv("PASSWORD_CHECK_PWNED", "true")
	res := signUp(t, w, w.client(t), "on", "on@example.invalid", breached)
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(body(t, res), "appeared in a data breach") {
		t.Errorf("on, a breached password is refused: %d", res.StatusCode)
	}
	for _, p := range asked {
		if p != "/range/"+digest[:5] {
			t.Errorf("only the five-digit prefix is sent: %s", p)
		}
	}
	if res := signUp(t, w, w.client(t), "clean", "clean@example.invalid", "a-clean-password-9"); res.StatusCode != http.StatusSeeOther {
		t.Errorf("a password not in the range is set: %d", res.StatusCode)
	}
	// A padding line (count 0) is no breach; the counted one is.
	got := pwnedRange(context.Background(), digest[:5])
	if len(got) != 1 || got[0] != digest[5:] {
		t.Errorf("the range, padding left out: %v", got)
	}
	mu.Lock()
	up = false
	mu.Unlock()
	if res := signUp(t, w, w.client(t), "down", "down@example.invalid", breached); res.StatusCode != http.StatusSeeOther {
		t.Errorf("an unreachable service lets the password through: %d", res.StatusCode)
	}
	if n, _ := w.db.Collection("users").CountDocuments(context.Background(), bson.M{"username": "on"}); n != 0 {
		t.Error("negative: the refused account exists")
	}
	// The refusal says the minimum, not __min__.
	t.Setenv("PASSWORD_CHECK_PWNED", "")
	t.Setenv("PASSWORD_MIN_LENGTH_ENFORCE", "true")
	t.Setenv("PASSWORD_MIN_LENGTH", "12")
	res = signUp(t, w, w.client(t), "shorty", "shorty@example.invalid", "eleven11111")
	if text := body(t, res); !strings.Contains(text, "at least 12 characters") || strings.Contains(text, "__min__") {
		t.Errorf("the minimum in the text: %d", res.StatusCode)
	}
}
