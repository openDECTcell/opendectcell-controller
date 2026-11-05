// SPDX-License-Identifier: AGPL-3.0-or-later

// Package retention keeps old data from piling up: after the configured time
// a call's remote number and media quality are erased (the row stays, so the
// statistics of counts and talk time still add up), and old audit entries are
// deleted. The defaults are 90 days for call details and the handset location history
// and 365 days for audit entries (settings.Default*Days); an admin sets the
// days, and 0 keeps for ever.
package retention

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/settings"
	"github.com/openDECTcell/opendectcell-controller/internal/sipevents"
	"github.com/openDECTcell/opendectcell-controller/internal/webhook"
)

// NotificationDays is how long a notification is kept.
const NotificationDays = 90

// Run applies the retention settings once a day until ctx ends.
func Run(ctx context.Context, pool *pgxpool.Pool, st *settings.Store, log *slog.Logger) {
	t := time.NewTimer(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := Apply(ctx, pool, st, time.Now(), log); err != nil && ctx.Err() == nil {
				log.Error("retention", "err", err)
			}
			t.Reset(24 * time.Hour)
		}
	}
}

// Apply erases what is older than the settings allow, as of now.
func Apply(ctx context.Context, pool *pgxpool.Pool, st *settings.Store, now time.Time, log *slog.Logger) error {
	r, err := st.Retention(ctx)
	if err != nil {
		return err
	}
	if r.CallDetailDays > 0 {
		tag, err := pool.Exec(ctx, `UPDATE call_log SET remote_party = '', quality = '{}'
			WHERE started_at < $1 AND (remote_party <> '' OR quality <> '{}')`, now.AddDate(0, 0, -r.CallDetailDays))
		if err != nil {
			return err
		}
		if n := tag.RowsAffected(); n > 0 {
			log.Info("retention: call details erased", "calls", n, "older_than_days", r.CallDetailDays)
		}
	}
	if r.LocationDays > 0 {
		// where a handset went is location data, with a retention of its own
		tag, err := pool.Exec(ctx, "DELETE FROM handset_move WHERE at < $1", now.AddDate(0, 0, -r.LocationDays))
		if err != nil {
			return err
		}
		if n := tag.RowsAffected(); n > 0 {
			log.Info("retention: handset moves erased", "moves", n, "older_than_days", r.LocationDays)
		}
	}
	// a VoIP line's registration history is not personal data: a fixed time, not a setting
	if tag, err := pool.Exec(ctx, "DELETE FROM sip_reg_event WHERE at < $1", now.AddDate(0, 0, -sipevents.KeepDays)); err != nil {
		return err
	} else if n := tag.RowsAffected(); n > 0 {
		log.Info("retention: VoIP registration history deleted", "events", n, "older_than_days", sipevents.KeepDays)
	}
	// the log of what the webhooks posted: a week
	if tag, err := pool.Exec(ctx, "DELETE FROM webhook_delivery WHERE state <> 'pending' AND done_at < $1", now.AddDate(0, 0, -webhook.KeepDays)); err != nil {
		return err
	} else if n := tag.RowsAffected(); n > 0 {
		log.Info("retention: webhook deliveries deleted", "deliveries", n, "older_than_days", webhook.KeepDays)
	}
	// notifications are read for a while and then only clutter: fixed, like the registration history
	if tag, err := pool.Exec(ctx, "DELETE FROM notification WHERE at < $1", now.AddDate(0, 0, -NotificationDays)); err != nil {
		return err
	} else if n := tag.RowsAffected(); n > 0 {
		log.Info("retention: notifications deleted", "notifications", n, "older_than_days", NotificationDays)
	}
	if r.AuditDays > 0 {
		var n int64
		// audit_purge() is the one way to delete audit entries: it runs as the table's owner (migration 0024)
		if err := pool.QueryRow(ctx, "SELECT audit_purge($1)", now.AddDate(0, 0, -r.AuditDays)).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			log.Info("retention: audit entries deleted", "entries", n, "older_than_days", r.AuditDays)
			_ = audit.RecordAs(ctx, pool, audit.System, audit.Entry{Action: "retention.audit_purge", Details: map[string]any{"deleted": n, "older_than_days": r.AuditDays}})
		}
	}
	return nil
}
