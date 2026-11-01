// SPDX-License-Identifier: AGPL-3.0-or-later

// Package retention keeps old data from piling up: after the configured time
// a call's remote number and media quality are erased (the row stays, so the
// statistics of counts and talk time still add up), and old audit entries are
// deleted. Both default to keeping everything; an admin sets the days.
package retention

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/settings"
)

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
	if r.AuditDays > 0 {
		var n int64
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL opendectcell.audit_purge = 'on'"); err != nil {
				return err
			}
			tag, err := tx.Exec(ctx, "DELETE FROM audit_log WHERE at < $1", now.AddDate(0, 0, -r.AuditDays))
			n = tag.RowsAffected()
			return err
		})
		if err != nil {
			return err
		}
		if n > 0 {
			log.Info("retention: audit entries deleted", "entries", n, "older_than_days", r.AuditDays)
			_ = audit.RecordAs(ctx, pool, audit.System, audit.Entry{Action: "retention.audit_purge", Details: map[string]any{"deleted": n, "older_than_days": r.AuditDays}})
		}
	}
	return nil
}
