package web

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/url"
	"os"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Outgoing e-mail, as Node.js WeKan sends it: through MAIL_URL
// (smtp://user:password@host:port, or smtps:// for TLS from the start), else
// through Admin Panel's mail server (settings.mailServer). The sender is
// MAIL_FROM, else Admin Panel's, else WeKan's "Boards Support <support@host>".

// mailConfig is where and as whom mail goes.
type mailConfig struct {
	Host, Port, User, Password string
	ImplicitTLS                bool
	From                       string
}

// errNoMail: neither MAIL_URL nor Admin Panel names a mail server.
var errNoMail = errors.New("no mail server is set: MAIL_URL, or Admin Panel -> Email")

// mailSettings is where mail goes now; domain names the default sender.
func (s *Server) mailSettings(ctx context.Context, domain string) (*mailConfig, error) {
	cfg := &mailConfig{}
	if raw := os.Getenv("MAIL_URL"); raw != "" {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "smtp" && u.Scheme != "smtps") || u.Hostname() == "" {
			return nil, fmt.Errorf("MAIL_URL is not an smtp:// or smtps:// address")
		}
		cfg.Host, cfg.Port, cfg.ImplicitTLS = u.Hostname(), u.Port(), u.Scheme == "smtps"
		if u.User != nil {
			cfg.User = u.User.Username()
			cfg.Password, _ = u.User.Password()
		}
	} else {
		var set struct {
			MailServer struct {
				Host      string `bson:"host"`
				Port      any    `bson:"port"`
				Username  string `bson:"username"`
				Password  string `bson:"password"`
				EnableTLS bool   `bson:"enableTLS"`
				From      string `bson:"from"`
			} `bson:"mailServer"`
		}
		_ = s.db.Collection("settings").FindOne(ctx, bson.M{}).Decode(&set)
		ms := set.MailServer
		if ms.Host == "" {
			return nil, errNoMail
		}
		cfg.Host, cfg.User, cfg.Password, cfg.ImplicitTLS, cfg.From = ms.Host, ms.Username, ms.Password, ms.EnableTLS, ms.From
		if ms.Port != nil && fmt.Sprint(ms.Port) != "" {
			cfg.Port = fmt.Sprint(ms.Port)
		}
	}
	if cfg.Port == "" {
		cfg.Port = "25"
		if cfg.ImplicitTLS {
			cfg.Port = "465"
		}
	}
	if from := os.Getenv("MAIL_FROM"); from != "" {
		cfg.From = from
	}
	if cfg.From == "" {
		host := domain
		if h, _, err := net.SplitHostPort(domain); err == nil {
			host = h
		}
		cfg.From = "Boards Support <support@" + strings.TrimPrefix(host, "www.") + ">"
	}
	return cfg, nil
}

// mailMessage is one e-mail: plain text, to one or more addresses.
type mailMessage struct {
	To      []string
	Subject string
	Text    string
}

// headerSafe: no line break, so a value can never start another header.
func headerSafe(v string) bool { return !strings.ContainsAny(v, "\r\n") }

// buildMail is the message as it goes on the wire: its headers - the subject
// encoded as RFC 2047 when it is not ASCII - and the text quoted-printable.
func buildMail(from string, m mailMessage, now time.Time) ([]byte, error) {
	if !headerSafe(from) || !headerSafe(m.Subject) {
		return nil, errors.New("a mail header has a line break")
	}
	if len(m.To) == 0 {
		return nil, errors.New("a mail needs an address to go to")
	}
	for _, to := range m.To {
		if a, err := mail.ParseAddress(to); err != nil || !headerSafe(to) || a.Address == "" {
			return nil, fmt.Errorf("not an e-mail address: %q", to)
		}
	}
	sender, err := mail.ParseAddress(from)
	if err != nil {
		return nil, fmt.Errorf("not a sender address: %q", from)
	}
	var b bytes.Buffer
	domain := sender.Address[strings.LastIndex(sender.Address, "@")+1:]
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s@%s>\r\n",
		sender.String(), strings.Join(m.To, ", "), mime.QEncoding.Encode("utf-8", m.Subject), now.Format(time.RFC1123Z), randomID(), domain)
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
	qp := quotedprintable.NewWriter(&b)
	_, _ = qp.Write([]byte(strings.ReplaceAll(strings.ReplaceAll(m.Text, "\r\n", "\n"), "\n", "\r\n")))
	_ = qp.Close()
	return b.Bytes(), nil
}

// sendMail sends m; domain names the default sender.
func (s *Server) sendMail(ctx context.Context, domain string, m mailMessage) error {
	cfg, err := s.mailSettings(ctx, domain)
	if err != nil {
		return err
	}
	msg, err := buildMail(cfg.From, m, time.Now())
	if err != nil {
		return err
	}
	sender, _ := mail.ParseAddress(cfg.From)
	addr := net.JoinHostPort(cfg.Host, cfg.Port)
	dialer := &net.Dialer{Timeout: 20 * time.Second}
	var conn net.Conn
	if cfg.ImplicitTLS {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	c, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if ok, _ := c.Extension("STARTTLS"); ok && !cfg.ImplicitTLS {
		if err := c.StartTLS(&tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return err
		}
	}
	if cfg.User != "" {
		// PlainAuth itself refuses to send the password unencrypted, but to localhost.
		if err := c.Auth(smtp.PlainAuth("", cfg.User, cfg.Password, cfg.Host)); err != nil {
			return err
		}
	}
	if err := c.Mail(sender.Address); err != nil {
		return err
	}
	for _, to := range m.To {
		a, _ := mail.ParseAddress(to)
		if err := c.Rcpt(a.Address); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// fillParams puts WeKan's __name__ parameters into a translated text.
func fillParams(text string, params map[string]string) string {
	for k, v := range params {
		text = strings.ReplaceAll(text, "__"+k+"__", v)
	}
	return text
}
