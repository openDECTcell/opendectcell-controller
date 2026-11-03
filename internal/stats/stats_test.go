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

	// cell changes: alice's handset moved twice today, bob's once last week
	var ha, hb uuid.UUID
	for owner, id := range map[uuid.UUID]*uuid.UUID{alice: &ha, bob: &hb} {
		if err := pool.QueryRow(ctx, "INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc) VALUES ($1, $2, $3, '\\x') RETURNING id",
			owner, owner[:5], int(owner[0])+100).Scan(id); err != nil {
			t.Fatal(err)
		}
	}
	for _, mv := range []struct {
		h   uuid.UUID
		ago time.Duration
	}{{ha, time.Hour}, {ha, 5 * time.Hour}, {hb, 6 * 24 * time.Hour}} {
		if _, err := pool.Exec(ctx, "INSERT INTO handset_move (handset_id, from_cell_id, to_cell_id, at) VALUES ($1, $2, $2, $3)", mv.h, cellID, now.Add(-mv.ago)); err != nil {
			t.Fatal(err)
		}
	}

	m, err := s.Summary(ctx, Own(alice))
	if err != nil {
		t.Fatal(err)
	}
	if m.Moves24h != 2 || m.Moves7d != 2 {
		t.Errorf("alice's cell changes %d/%d, want 2/2", m.Moves24h, m.Moves7d)
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

// The days are the days of the controller's own time zone: a call just after local midnight (still the day before in
// UTC) counts for the new local day, and the bar is labelled with that date (review finding R4).
func TestDaysFollowTheControllersZone(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Migrated(t)
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip("no zone database")
	}
	now := time.Date(2026, 11, 4, 0, 30, 0, 0, berlin) // 23:30 UTC on 3 November
	s := &Store{Pool: pool, Now: func() time.Time { return now }}
	var u uuid.UUID
	if err := pool.QueryRow(ctx, "INSERT INTO app_user (username, role, auth_source, password_hash) VALUES ('z', 'user', 'local', 'x') RETURNING id").Scan(&u); err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{time.Date(2026, 11, 4, 0, 10, 0, 0, berlin), time.Date(2026, 11, 3, 23, 50, 0, 0, berlin)} {
		if _, err := pool.Exec(ctx, "INSERT INTO call_log (owner_user_id, direction, started_at) VALUES ($1, 'out', $2)", u, at); err != nil {
			t.Fatal(err)
		}
	}
	days, err := s.Days(ctx, Own(u), 3)
	if err != nil || len(days) != 3 {
		t.Fatalf("days %v %v", days, err)
	}
	want := []string{"2026-11-02", "2026-11-03", "2026-11-04"}
	for i, d := range days {
		if got := d.Day.Format("2006-01-02"); got != want[i] {
			t.Errorf("day %d is labelled %s, want %s", i, got, want[i])
		}
	}
	if days[2].Calls != 1 || days[1].Calls != 1 || days[0].Calls != 0 {
		t.Errorf("calls per day: %+v", days)
	}
}
