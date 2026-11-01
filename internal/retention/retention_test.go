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

	var uid, hid, cid string
	if err := pool.QueryRow(ctx, "INSERT INTO app_user (username, role, auth_source) VALUES ('r', 'user', 'local') RETURNING id").Scan(&uid); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc) VALUES ($1, '\\x01', 5, '\\x') RETURNING id", uid).Scan(&hid); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "INSERT INTO cell (name, state) VALUES ('c', 'active') RETURNING id").Scan(&cid); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO handset_move (handset_id, from_cell_id, to_cell_id, at) VALUES ($1, $2, $2, $3), ($1, $2, $2, $4)`,
		hid, cid, now.AddDate(0, 0, -100), now.AddDate(0, 0, -5))

	// nothing saved: the defaults, 90 days and 365 days
	if err := Apply(ctx, pool, st, now, log); err != nil {
		t.Fatal(err)
	}
	var left int
	pool.QueryRow(ctx, "SELECT count(*) FROM call_log WHERE remote_party <> ''").Scan(&left)
	if left != 1 {
		t.Fatalf("defaults: %d calls keep their number, want the recent one only", left)
	}
	pool.QueryRow(ctx, "SELECT count(*) FROM handset_move").Scan(&left)
	if left != 1 {
		t.Fatalf("defaults: %d handset moves kept, want the recent one only", left)
	}
	pool.QueryRow(ctx, "SELECT count(*) FROM audit_log WHERE action = 'old'").Scan(&left)
	if left != 0 {
		t.Fatalf("defaults left the 400-day-old audit entry")
	}
	// a setting saved before the location history had its own field: 90 days for
	// it, not for ever; a field of its own then works apart from the call details
	exec(`INSERT INTO handset_move (handset_id, from_cell_id, to_cell_id, at) VALUES ($1, $2, $2, $3), ($1, $2, $2, $4)`,
		hid, cid, now.AddDate(0, 0, -95), now.AddDate(0, 0, -40))
	if err := st.Put(ctx, settings.KeyRetention, map[string]int{"call_detail_days": 90, "audit_days": 365}); err != nil {
		t.Fatal(err)
	}
	if err := Apply(ctx, pool, st, now, log); err != nil {
		t.Fatal(err)
	}
	pool.QueryRow(ctx, "SELECT count(*) FROM handset_move WHERE at < $1", now.AddDate(0, 0, -90)).Scan(&left)
	if left != 0 {
		t.Fatalf("a setting without location_days kept a 95-day-old move")
	}
	if err := st.Put(ctx, settings.KeyRetention, settings.Retention{CallDetailDays: 90, AuditDays: 365, LocationDays: 30}); err != nil {
		t.Fatal(err)
	}
	if err := Apply(ctx, pool, st, now, log); err != nil {
		t.Fatal(err)
	}
	pool.QueryRow(ctx, "SELECT count(*) FROM handset_move").Scan(&left)
	if left != 1 {
		t.Fatalf("location_days 30: %d moves kept, want the 5-day-old one only", left)
	}
	// an admin who clears both fields keeps everything for ever
	exec(`INSERT INTO call_log (direction, remote_party, started_at) VALUES ('in', '+4930777777', $1)`, now.AddDate(0, 0, -500))
	if err := st.Put(ctx, settings.KeyRetention, settings.Retention{}); err != nil {
		t.Fatal(err)
	}
	if err := Apply(ctx, pool, st, now, log); err != nil {
		t.Fatal(err)
	}
	pool.QueryRow(ctx, "SELECT count(*) FROM call_log WHERE remote_party = '+4930777777'").Scan(&left)
	if left != 1 {
		t.Fatal("zero days erased a call")
	}
	exec(`UPDATE call_log SET remote_party = '+4930123456', quality = '{"mos":4.2}' WHERE started_at < $1`, now.AddDate(0, 0, -100))
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
	if calls != 3 {
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
