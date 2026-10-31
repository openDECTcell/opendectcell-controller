// SPDX-License-Identifier: AGPL-3.0-or-later

package retention

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/audit"
	"github.com/openDECTcell/opendectcell-controller/internal/dbtest"
	"github.com/openDECTcell/opendectcell-controller/internal/settings"
)

func TestRetention(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Migrated(t)
	st := &settings.Store{Pool: pool}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO call_log (direction, remote_party, started_at, quality) VALUES ('out', '+4930123456', $1, '{"mos":4.2}'), ('in', '+4930999999', $2, '{"mos":3.9}')`,
		now.AddDate(0, 0, -100), now.AddDate(0, 0, -5))
	exec(`ALTER TABLE audit_log DISABLE TRIGGER audit_log_append_only`)
	exec(`INSERT INTO audit_log (at, actor_name, action) VALUES ($1, 'x', 'old'), ($2, 'x', 'recent')`, now.AddDate(0, 0, -400), now.AddDate(0, 0, -10))
	exec(`ALTER TABLE audit_log ENABLE TRIGGER audit_log_append_only`)

	// defaults keep everything
	if err := Apply(ctx, pool, st, now, log); err != nil {
		t.Fatal(err)
	}
	var n int
	pool.QueryRow(ctx, "SELECT count(*) FROM call_log WHERE remote_party <> ''").Scan(&n)
	if n != 2 {
		t.Fatalf("defaults erased calls: %d left", n)
	}

	if err := st.Put(ctx, settings.KeyRetention, settings.Retention{CallDetailDays: 30, AuditDays: 365}); err != nil {
		t.Fatal(err)
	}
	if err := Apply(ctx, pool, st, now, log); err != nil {
		t.Fatal(err)
	}
	var old, recent string
	pool.QueryRow(ctx, "SELECT remote_party FROM call_log WHERE started_at < $1", now.AddDate(0, 0, -30)).Scan(&old)
	pool.QueryRow(ctx, "SELECT remote_party FROM call_log WHERE started_at >= $1", now.AddDate(0, 0, -30)).Scan(&recent)
	if old != "" || recent != "+4930999999" {
		t.Errorf("call details: old %q recent %q", old, recent)
	}
	var calls int
	pool.QueryRow(ctx, "SELECT count(*) FROM call_log").Scan(&calls)
	if calls != 2 {
		t.Errorf("the calls themselves must stay, %d left", calls)
	}
	var q string
	pool.QueryRow(ctx, "SELECT quality::text FROM call_log WHERE started_at < $1", now.AddDate(0, 0, -30)).Scan(&q)
	if q != "{}" {
		t.Errorf("quality kept: %s", q)
	}
	var actions []string
	rows, _ := pool.Query(ctx, "SELECT action FROM audit_log ORDER BY id")
	for rows.Next() {
		var a string
		rows.Scan(&a)
		actions = append(actions, a)
	}
	rows.Close()
	if len(actions) != 2 || actions[0] != "recent" || actions[1] != "retention.audit_purge" {
		t.Errorf("audit log after the purge: %v", actions)
	}
	// the audit log stays append-only for everyone else
	if _, err := pool.Exec(ctx, "DELETE FROM audit_log"); err == nil {
		t.Error("audit_log deletable without the purge switch")
	}
	_ = audit.System
}
