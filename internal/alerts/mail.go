// SPDX-License-Identifier: AGPL-3.0-or-later

package alerts

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Mailer is what the mail run needs of the controller's mail sender.
type Mailer interface {
	Enabled() bool
	Send(ctx context.Context, to []string, subject, body string) error
}

// MailGiveUp is how long a notification waits for a mail that cannot be sent; after that it is marked done unsent, so a broken mail
// server does not make a mail of everything a day late.
const MailGiveUp = 24 * time.Hour

// MailRun sends what users asked to get by mail, one mail per person per run, and never inside their quiet hours (it waits for the end).
type MailRun struct {
	Pool   *pgxpool.Pool
	Mailer Mailer
	Log    *slog.Logger
	Now    func() time.Time
}

// Run sends every minute until ctx ends.
func (m *MailRun) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := m.Once(ctx); err != nil && ctx.Err() == nil && m.Log != nil {
				m.Log.Warn("notification mail", "err", err)
			}
		}
	}
}

// Once sends what is due.
func (m *MailRun) Once(ctx context.Context) error {
	if m.Mailer == nil || !m.Mailer.Enabled() {
		return nil
	}
	now := time.Now()
	if m.Now != nil {
		now = m.Now()
	}
	// stale ones first: they are not sent any more
	if _, err := m.Pool.Exec(ctx, "UPDATE notification SET mailed_at = $1 WHERE mail_wanted AND mailed_at IS NULL AND at < $2", now, now.Add(-MailGiveUp)); err != nil {
		return err
	}
	rows, err := m.Pool.Query(ctx, `SELECT DISTINCT n.user_id, u.email, u.alert_prefs FROM notification n JOIN app_user u ON u.id = n.user_id
		WHERE n.mail_wanted AND n.mailed_at IS NULL AND NOT u.disabled AND coalesce(u.email, '') <> ''`)
	if err != nil {
		return err
	}
	type due struct {
		id    uuid.UUID
		email string
		prefs Prefs
	}
	var users []due
	for rows.Next() {
		var d due
		var raw []byte
		if err := rows.Scan(&d.id, &d.email, &raw); err != nil {
			rows.Close()
			return err
		}
		d.prefs = ParsePrefs(raw)
		users = append(users, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, u := range users {
		if u.prefs.Quiet(now.Local()) {
			continue
		}
		if err := m.digest(ctx, u.id, u.email, now); err != nil {
			return err
		}
	}
	return nil
}

func (m *MailRun) digest(ctx context.Context, user uuid.UUID, email string, now time.Time) error {
	type item struct {
		id            int64
		at            time.Time
		level, sub, b string
	}
	rows, err := m.Pool.Query(ctx, "SELECT id, at, level, subject, body FROM notification WHERE user_id = $1 AND mail_wanted AND mailed_at IS NULL ORDER BY at, id LIMIT 50", user)
	if err != nil {
		return err
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (item, error) {
		var i item
		err := r.Scan(&i.id, &i.at, &i.level, &i.sub, &i.b)
		return i, err
	})
	if err != nil || len(items) == 0 {
		return err
	}
	subject := "openDECTcell: " + items[0].sub
	if len(items) > 1 {
		subject = fmt.Sprintf("openDECTcell: %d notifications", len(items))
	}
	var b strings.Builder
	ids := make([]int64, 0, len(items))
	for _, i := range items {
		ids = append(ids, i.id)
		fmt.Fprintf(&b, "%s  %s\n", i.at.Local().Format("2006-01-02 15:04"), i.sub)
		if i.b != "" {
			fmt.Fprintf(&b, "    %s\n", strings.ReplaceAll(i.b, "\n", "\n    "))
		}
		b.WriteString("\n")
	}
	b.WriteString("You get these because you asked for them on your account page (Notifications). Quiet hours and the kinds can be changed there.\n")
	if err := m.Mailer.Send(ctx, []string{email}, subject, b.String()); err != nil {
		return err
	}
	_, err = m.Pool.Exec(ctx, "UPDATE notification SET mailed_at = $2 WHERE id = ANY($1)", ids, now)
	return err
}
