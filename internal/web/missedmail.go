// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// missedMailWindow is how far back a missed call is still mailed: a call that went unnoticed while the mail server
// was down is reported when it is back, but not a day later.
const missedMailWindow = 6 * time.Hour

// missedMailSettle keeps a call that has just ended out of the mail, so that two calls in a row go out together.
const missedMailSettle = 20 * time.Second

// RunMissedCallMails e-mails the users who asked for it about their missed calls: one mail for the calls missed since
// the last one, within a minute of the call. A call is mailed once (call_log.missed_notified_at). It stops with ctx.
func (s *Server) RunMissedCallMails(ctx context.Context) {
	var backoffUntil time.Time
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if now.Before(backoffUntil) || !s.mailOn() {
				continue
			}
			if err := s.sendMissedCallMails(ctx, now); err != nil {
				s.Log.Error("missed-call mail", "err", err)
				backoffUntil = now.Add(5 * time.Minute) // a mail server that is down is not asked every 30 s
			}
		}
	}
}

type missedCall struct {
	id      int64
	started time.Time
	number  string
	line    string
	handset string
}

// sendMissedCallMails is one look: the missed calls of the users who opted in, one mail each.
func (s *Server) sendMissedCallMails(ctx context.Context, now time.Time) error {
	rows, err := s.Pool.Query(ctx, `SELECT u.id, u.email, c.id, c.started_at, c.remote_party, coalesce(a.label, ''), coalesce(nullif(h.display_name, ''), '')
		FROM call_log c
		JOIN app_user u ON u.id = c.owner_user_id
		LEFT JOIN sip_account a ON a.id = c.sip_account_id
		LEFT JOIN handset h ON h.id = c.handset_id
		WHERE u.notify_missed_calls AND NOT u.disabled AND coalesce(u.email, '') <> ''
		  AND c.direction = 'in' AND c.answered_at IS NULL AND c.ended_at IS NOT NULL AND c.missed_notified_at IS NULL
		  AND c.started_at > $1 AND c.ended_at < $2
		ORDER BY u.id, c.started_at`, now.Add(-missedMailWindow), now.Add(-missedMailSettle))
	if err != nil {
		return err
	}
	type batch struct {
		email string
		calls []missedCall
	}
	var order []uuid.UUID
	byUser := map[uuid.UUID]*batch{}
	for rows.Next() {
		var uid uuid.UUID
		var email string
		var m missedCall
		if err := rows.Scan(&uid, &email, &m.id, &m.started, &m.number, &m.line, &m.handset); err != nil {
			rows.Close()
			return err
		}
		b := byUser[uid]
		if b == nil {
			b = &batch{email: email}
			byUser[uid] = b
			order = append(order, uid)
		}
		b.calls = append(b.calls, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, uid := range order {
		b := byUser[uid]
		subject, body := missedCallMail(b.calls, s.callsURL())
		if err := s.Mailer.Send(ctx, []string{b.email}, subject, body); err != nil {
			return fmt.Errorf("to a user: %w", err)
		}
		ids := make([]int64, len(b.calls))
		for i, c := range b.calls {
			ids[i] = c.id
		}
		if _, err := s.Pool.Exec(ctx, "UPDATE call_log SET missed_notified_at = $2 WHERE id = ANY($1)", ids, now); err != nil {
			return err
		}
	}
	return nil
}

// callsURL is the address of the Calls page for the mail's link, "" without a base URL.
func (s *Server) callsURL() string {
	if s.BaseURL == nil {
		return ""
	}
	return strings.TrimRight(s.BaseURL.String(), "/") + "/calls?dir=missed"
}

// missedCallMail is the subject and the text for the calls missed since the last mail.
func missedCallMail(calls []missedCall, link string) (subject, body string) {
	if len(calls) == 1 {
		subject = "Missed call from " + calls[0].number
	} else {
		subject = fmt.Sprintf("%d missed calls", len(calls))
	}
	var b strings.Builder
	for _, c := range calls {
		fmt.Fprintf(&b, "%s  %s", c.started.Local().Format("2006-01-02 15:04"), orUnknown(c.number))
		var where []string
		if c.line != "" {
			where = append(where, "line "+c.line)
		}
		if c.handset != "" {
			where = append(where, "handset "+c.handset)
		}
		if len(where) > 0 {
			fmt.Fprintf(&b, " (%s)", strings.Join(where, ", "))
		}
		b.WriteString("\n")
	}
	if link != "" {
		fmt.Fprintf(&b, "\nAll calls: %s\n", link)
	}
	b.WriteString("\nYou get this because you asked for missed-call mails on your account page.\n")
	return subject, b.String()
}

func orUnknown(n string) string {
	if n == "" {
		return "unknown number"
	}
	return n
}
