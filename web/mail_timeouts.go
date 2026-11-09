package web

import (
	"net"
	"os"
	"strconv"
	"sync"
	"time"
)

// WeKan's SMTP time limits (server/lib/mailTransport.js smtpTimeouts and
// server/lib/smtpDeadline.js smtpTotalTimeout), with its names, defaults and
// bounds: a connection, the server's greeting, a silent socket, and the whole
// message - an absolute deadline, so a server that keeps sending bytes cannot
// hold a send slot for ever. A value out of bounds keeps the default.
type smtpLimits struct{ Connection, Greeting, Socket, Total time.Duration }

func envMillis(name string, def, lo, hi int) time.Duration {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v >= lo && v <= hi {
		return time.Duration(v) * time.Millisecond
	}
	return time.Duration(def) * time.Millisecond
}

func mailLimits() smtpLimits {
	return smtpLimits{
		Connection: envMillis("MAIL_CONNECTION_TIMEOUT_MS", 30000, 1000, 900000),
		Greeting:   envMillis("MAIL_GREETING_TIMEOUT_MS", 30000, 1000, 900000),
		Socket:     envMillis("MAIL_SOCKET_TIMEOUT_MS", 120000, 1000, 900000),
		Total:      envMillis("MAIL_TOTAL_TIMEOUT_MS", 120000, 1000, 300000),
	}
}

// deadlineConn is an SMTP connection under those limits: until the greeting
// has come, its own deadline; after it, the socket's idle limit, renewed by
// each read and write - and never past the message's total deadline.
type deadlineConn struct {
	net.Conn
	mu       sync.Mutex
	limits   smtpLimits
	total    time.Time
	greeting time.Time // zero once the greeting has come
}

func newDeadlineConn(c net.Conn, l smtpLimits, start time.Time) *deadlineConn {
	d := &deadlineConn{Conn: c, limits: l, total: start.Add(l.Total), greeting: time.Now().Add(l.Greeting)}
	d.arm()
	return d
}

func (d *deadlineConn) arm() {
	d.mu.Lock()
	next := time.Now().Add(d.limits.Socket)
	if !d.greeting.IsZero() {
		next = d.greeting
	}
	if next.After(d.total) {
		next = d.total
	}
	d.mu.Unlock()
	_ = d.Conn.SetDeadline(next)
}

// greeted ends the greeting phase.
func (d *deadlineConn) greeted() {
	d.mu.Lock()
	d.greeting = time.Time{}
	d.mu.Unlock()
	d.arm()
}

func (d *deadlineConn) Read(b []byte) (int, error) {
	d.arm()
	return d.Conn.Read(b)
}

func (d *deadlineConn) Write(b []byte) (int, error) {
	d.arm()
	return d.Conn.Write(b)
}
