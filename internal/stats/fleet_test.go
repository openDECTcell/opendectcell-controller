// SPDX-License-Identifier: AGPL-3.0-or-later

package stats

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/dbtest"
)

func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"0.3.4", "0.3.4", 0}, {"0.3.4", "0.3.10", -1}, {"0.10", "0.9", 1}, {"1.0", "0.99.99", 1}, {"0.3", "0.3.1", -1}, {"v0.3.4", "0.3.4", 0},
		{"24.10.0", "24.10", 1}, {"1.2.3-rc1", "1.2.3-rc2", -1}, {"1.2.3", "1.2.3-rc1", -1}, {"2026.11.1", "2026.2.30", 1}, {"abc", "abd", -1}, {"", "0.1", -1}, {"", "", 0},
		{"0.3.4 ", "0.3.4", 0}, {"1.0a", "1.0b", -1}, {"1.2rc10", "1.2rc9", 1},
	} {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
		if got := CompareVersions(c.b, c.a); got != -c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d (the reverse)", c.b, c.a, got, -c.want)
		}
	}
}

func TestVersionSpread(t *testing.T) {
	cells := []FleetCell{{Software: "0.3.4"}, {Software: "0.3.10"}, {Software: "0.3.4"}, {Software: ""}, {Software: "0.3.10"}, {Software: "0.3.10"}, {Software: "0.2.0"}}
	got := VersionSpread(cells)
	if len(got) != 4 {
		t.Fatalf("spread: %+v", got)
	}
	want := []VersionGroup{{"0.3.10", 3, true, false}, {"0.3.4", 2, false, true}, {"0.2.0", 1, false, true}, {"", 1, false, false}}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("group %d: %+v, want %+v", i, got[i], want[i])
		}
	}
	if len(VersionSpread(nil)) != 0 {
		t.Error("a spread of nothing")
	}
}

func TestFleetCells(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Migrated(t)
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	s := &Store{Pool: pool, Now: func() time.Time { return now }}
	mkCell := func(name, state, hw, sw, stub string) uuid.UUID {
		var id uuid.UUID
		if err := pool.QueryRow(ctx, "INSERT INTO cell (name, state, hw_model, software_version, stub_version, last_seen_at) VALUES ($1, $2, $3, nullif($4, ''), nullif($5, ''), $6) RETURNING id",
			name, state, hw, sw, stub, now.Add(-time.Minute)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	busy := mkCell("Hall", "active", "AVM FRITZ!Box 7520", "0.3.10", "08.25")
	old := mkCell("Attic", "active", "AVM FRITZ!Box 7412", "0.3.4", "06.88")
	quiet := mkCell("Shed", "active", "", "", "")
	mkCell("Off", "disabled", "x", "9.9", "")
	var owner uuid.UUID
	pool.QueryRow(ctx, "INSERT INTO app_user (username, role, auth_source) VALUES ('a', 'user', 'local') RETURNING id").Scan(&owner)
	call := func(ago time.Duration, talk time.Duration, cells ...uuid.UUID) {
		start := now.Add(-ago)
		if _, err := pool.Exec(ctx, "INSERT INTO call_log (owner_user_id, direction, started_at, answered_at, ended_at, cell_ids) VALUES ($1, 'out', $2, $2, $3, $4)", owner, start, start.Add(talk), cells); err != nil {
			t.Fatal(err)
		}
	}
	call(time.Hour, 5*time.Minute, busy)
	call(2*time.Hour, 10*time.Minute, busy, old) // moved between the two
	call(24*time.Hour, 1*time.Minute, old)
	call(3*time.Hour, 1*time.Minute, busy)
	call(20*24*time.Hour, time.Minute, busy) // outside a 7-day range
	hs := func(ipui byte, seenOn *uuid.UUID, seenAgo time.Duration) uuid.UUID {
		var id uuid.UUID
		pool.QueryRow(ctx, "INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc, last_seen_cell_id, last_seen_at) VALUES ($1, $2, $3, '\\x', $4, $5) RETURNING id", owner, []byte{ipui}, int(ipui), seenOn, now.Add(-seenAgo)).Scan(&id)
		return id
	}
	h1, h2, h3 := hs(1, &busy, time.Hour), hs(2, &busy, 30*24*time.Hour), hs(3, &old, time.Hour)
	pool.Exec(ctx, "INSERT INTO handset_move (handset_id, to_cell_id, at) VALUES ($1, $2, $3), ($4, $2, $3), ($5, $2, $6)", h1, busy, now.Add(-2*time.Hour), h3, h2, now.Add(-20*24*time.Hour))
	got, err := s.FleetCells(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].ID != busy || got[1].ID != old || got[2].ID != quiet {
		t.Fatalf("order, and only the active ones: %+v", got)
	}
	h, a, q := got[0], got[1], got[2]
	if h.Calls != 3 || h.TalkSeconds != 960 || h.Hardware != "AVM FRITZ!Box 7520" || h.Software != "0.3.10" || h.Stub != "08.25" || h.Behind || h.LastSeenAt == nil {
		t.Errorf("Hall: %+v", h)
	}
	// Hall: h1 (moved there and last seen there) counted once, h3 moved there; h2 was last seen there 30 days ago and moved 20 days ago: not in 7 days
	if h.Handsets != 2 {
		t.Errorf("handsets heard on Hall in 7 days: %d, want 2 (h1 once, h3)", h.Handsets)
	}
	if a.Calls != 2 || a.TalkSeconds != 660 || !a.Behind || a.Handsets != 1 {
		t.Errorf("Attic: %+v (it carried the call that moved, and its software is behind)", a)
	}
	if q.Calls != 0 || q.TalkSeconds != 0 || q.Handsets != 0 || q.Behind || q.Software != "" {
		t.Errorf("Shed, the quiet one that has not reported: %+v", q)
	}
	// a longer range holds the old call
	if got, _ = s.FleetCells(ctx, 30); got[0].Calls != 4 {
		t.Errorf("30 days: Hall carried %d calls, want 4", got[0].Calls)
	}
}
