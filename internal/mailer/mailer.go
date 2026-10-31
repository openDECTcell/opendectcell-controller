// SPDX-License-Identifier: AGPL-3.0-or-later

// Package mailer sends the controller's notification e-mails over SMTP.
package mailer

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/config"
)

// Mailer sends mail through the configured server.
type Mailer struct {
	Cfg config.SMTP
	// Name is the controller's name, put in the subject line.
	Name func() string
}

// ErrDisabled means no SMTP server is configured.
var ErrDisabled = errors.New("mailer: no SMTP server configured")

// Enabled reports whether mail can be sent.
func (m *Mailer) Enabled() bool { return m != nil && m.Cfg.Enabled() }

// Send sends one plain-text message to each recipient separately, so no
// recipient learns another's address.
func (m *Mailer) Send(ctx context.Context, to []string, subject, body string) error {
	if !m.Enabled() {
		return ErrDisabled
	}
	var errs []error
	for _, r := range to {
		if err := m.sendOne(ctx, r, subject, body); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r, err))
		}
	}
	return errors.Join(errs...)
}

func (m *Mailer) sendOne(ctx context.Context, to, subject, body string) error {
	if strings.ContainsAny(to+subject, "\r\n") {
		return errors.New("header injection")
	}
	rcpt, err := mail.ParseAddress(to)
	if err != nil {
		return err
	}
	msg := Message(m.Cfg.From, rcpt.Address, m.subject(subject), body, time.Now())
	addr := net.JoinHostPort(m.Cfg.Host, m.Cfg.Port)
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	if m.Cfg.Security == "tls" {
		conn = tls.Client(conn, &tls.Config{ServerName: m.Cfg.Host, MinVersion: tls.VersionTLS12})
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	c, err := smtp.NewClient(conn, m.Cfg.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if m.Cfg.Security == "starttls" {
		if err := c.StartTLS(&tls.Config{ServerName: m.Cfg.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return err
		}
	}
	if m.Cfg.User != "" {
		if err := c.Auth(smtp.PlainAuth("", m.Cfg.User, m.Cfg.Password, m.Cfg.Host)); err != nil {
			return err
		}
	}
	from, err := mail.ParseAddress(m.Cfg.From)
	if err != nil {
		return err
	}
	if err := c.Mail(from.Address); err != nil {
		return err
	}
	if err := c.Rcpt(rcpt.Address); err != nil {
		return err
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

func (m *Mailer) subject(s string) string {
	if m.Name != nil {
		return "[" + m.Name() + "] " + s
	}
	return s
}

// Message formats a plain-text mail with CRLF line ends and a UTF-8 body.
func Message(from, to, subject, body string, at time.Time) []byte {
	body = strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n")
	h := []string{
		"From: " + from,
		"To: " + to,
		"Subject: " + mimeWord(subject),
		"Date: " + at.Format(time.RFC1123Z),
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"Content-Transfer-Encoding: 8bit",
		"Auto-Submitted: auto-generated",
	}
	return []byte(strings.Join(h, "\r\n") + "\r\n\r\n" + body + "\r\n")
}

func mimeWord(s string) string {
	s = strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
	return mime.QEncoding.Encode("utf-8", s)
}
