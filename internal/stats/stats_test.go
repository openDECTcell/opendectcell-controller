// SPDX-License-Identifier: AGPL-3.0-or-later

package stats

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/dbtest"
)

func TestScopedStatistics(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Migrated(t)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	s := &Store{Pool: pool, Now: func() time.Time { return now }}

	var alice, bob uuid.UUID
	for name, id := range map[string]*uuid.UUID{"alice": &alice, "bob": &bob} {
		if err := pool.QueryRow(ctx, "INSERT INTO app_user (username, role, auth_source, password_hash) VALUES ($1, 'user', 'local', 'x') RETURNING id", name).Scan(id); err != nil {
			t.Fatal(err)
		}
	}
	var cellID uuid.UUID
	if err := pool.QueryRow(ctx, "INSERT INTO cell (name, state) VALUES ('Hall', 'active') RETURNING id").Scan(&cellID); err != nil {
		t.Fatal(err)
	}
	call := func(owner uuid.UUID, ago time.Duration, talk time.Duration, dir, reason string) {
		start := now.Add(-ago)
		var answered, ended any
		if talk > 0 {
			answered, ended = start.Add(3*time.Second), start.Add(3*time.Second+talk)
		} else {
			ended = start.Add(20 * time.Second)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO call_log (owner_user_id, direction, started_at, answered_at, ended_at, end_reason, cell_ids, srtp)
			VALUES ($1, $2, $3, $4, $5, $6, ARRAY[$7::uuid], true)`, owner, dir, start, answered, ended, reason, cellID); err != nil {
			t.Fatal(err)
		}
	}
	call(alice, time.Hour, 5*time.Minute, "out", "")
	call(alice, 30*time.Hour, 2*time.Minute, "in", "")
	call(alice, 3*24*time.Hour, 0, "out", "busy")
	call(bob, 2*time.Hour, 10*time.Minute, "in", "")
	call(bob, 20*24*time.Hour, 1*time.Minute, "in", "")

	m, err := s.Summary(ctx, Own(alice))
	if err != nil {
		t.Fatal(err)
	}
	if m.Calls24h != 1 || m.Calls7d != 3 || m.Answered7d != 2 || m.Failed7d != 1 || m.TalkSeconds7d != 420 || m.CallsTotal != 3 {
		t.Errorf("alice %+v", m)
	}
	all, err := s.Summary(ctx, Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if all.CallsTotal != 5 || all.Calls7d != 4 || all.TalkSeconds7d != 1020 {
		t.Errorf("all %+v", all)
	}

	days, err := s.Days(ctx, Own(alice), 7)
	if err != nil || len(days) != 7 {
		t.Fatalf("days %d %v", len(days), err)
	}
	var total int
	for _, d := range days {
		total += d.Calls
	}
	if total != 3 || days[6].Calls != 1 {
		t.Errorf("days %+v", days)
	}

	rs, err := s.EndReasons(ctx, Own(alice), 7)
	if err != nil || len(rs) != 2 {
		t.Fatalf("reasons %+v %v", rs, err)
	}
	pc, err := s.PerCell(ctx, Own(bob), 7)
	if err != nil || len(pc) != 1 || pc[0].Calls != 1 || pc[0].Name != "Hall" {
		t.Errorf("per cell %+v %v", pc, err)
	}
	f, err := s.Fleet(ctx)
	if err != nil || f.CellsActive != 1 || f.Users != 2 {
		t.Errorf("fleet %+v %v", f, err)
	}
}
