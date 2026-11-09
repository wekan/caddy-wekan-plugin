package web

import (
	"bufio"
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A stuck SMTP server: it never greets, goes silent, or answers DATA with an
// endless "still processing" - each ends at WeKan's limit, the socket closed.
func TestSMTPTimeoutsCloseTheSocket(t *testing.T) {
	t.Setenv("MAIL_GREETING_TIMEOUT_MS", "1000")
	t.Setenv("MAIL_SOCKET_TIMEOUT_MS", "1000")
	t.Setenv("MAIL_TOTAL_TIMEOUT_MS", "2000")
	for _, phase := range []string{"greeting", "idle", "total"} {
		t.Run(phase, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			var open atomic.Int32
			closed := make(chan struct{}, 1)
			go func() {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				open.Add(1)
				defer func() { c.Close(); open.Add(-1); closed <- struct{}{} }()
				rd := bufio.NewReader(c)
				say := func(l string) { _, _ = c.Write([]byte(l + "\r\n")) }
				if phase == "greeting" {
					_, _ = rd.ReadString('\n') // until the client hangs up
					return
				}
				say("220 stuck")
				for {
					line, err := rd.ReadString('\n')
					if err != nil {
						return
					}
					cmd := strings.ToUpper(strings.TrimSpace(line))
					switch {
					case phase == "idle" && strings.HasPrefix(cmd, "MAIL"):
						_, _ = rd.ReadString('\n') // silent from here
						return
					case strings.HasPrefix(cmd, "EHLO"):
						say("250 hi")
					case strings.HasPrefix(cmd, "DATA"):
						say("354 go")
						for {
							l, err := rd.ReadString('\n')
							if err != nil {
								return
							}
							if l == ".\r\n" {
								break
							}
						}
						for { // the total phase: bytes, but never an answer
							if _, err := c.Write([]byte("250-still processing\r\n")); err != nil {
								return
							}
							time.Sleep(10 * time.Millisecond)
						}
					default:
						say("250 ok")
					}
				}
			}()
			t.Setenv("MAIL_URL", "smtp://"+ln.Addr().String())
			w := newTestWeKan(t)
			start := time.Now()
			err = w.s.sendMail(context.Background(), "localhost", mailMessage{To: []string{"bob@example.org"}, Subject: "S", HTML: "hi"})
			took := time.Since(start)
			if err == nil {
				t.Fatal("negative: a stuck server is never a sent message")
			}
			want := map[string]time.Duration{"greeting": time.Second, "idle": time.Second, "total": 2 * time.Second}[phase]
			if took < want-100*time.Millisecond || took > want+1500*time.Millisecond {
				t.Errorf("ended after %v, the limit is %v", took, want)
			}
			select {
			case <-closed:
			case <-time.After(3 * time.Second):
				t.Error("the socket is closed, not abandoned")
			}
			if d := emailRetryDecision(err, "smtp", 1, time.Now(), 0); d.state != "pending" || d.reason != "delivery-failed" {
				t.Errorf("kept for a retry: %+v", d)
			}
		})
	}
}

// The limits are WeKan's: its names, defaults and bounds.
func TestMailLimitsAreWeKans(t *testing.T) {
	l := mailLimits()
	if l.Connection != 30*time.Second || l.Greeting != 30*time.Second || l.Socket != 120*time.Second || l.Total != 120*time.Second {
		t.Errorf("defaults: %+v", l)
	}
	t.Setenv("MAIL_TOTAL_TIMEOUT_MS", "999")
	t.Setenv("MAIL_SOCKET_TIMEOUT_MS", "900001")
	if l := mailLimits(); l.Total != 120*time.Second || l.Socket != 120*time.Second {
		t.Errorf("negative: out of bounds keeps the default: %+v", l)
	}
	t.Setenv("MAIL_TOTAL_TIMEOUT_MS", "300000")
	if mailLimits().Total != 300*time.Second {
		t.Error("the total's upper bound")
	}
}
