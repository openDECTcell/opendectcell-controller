// SPDX-License-Identifier: AGPL-3.0-or-later

package stats

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openDECTcell/opendectcell-controller/internal/dbtest"
)

type world struct {
	s          *Store
	pool       *pgxpool.Pool
	alice, bob uuid.UUID
	hall, shed uuid.UUID
	line, hs   uuid.UUID
	berlin     *time.Location
	now        time.Time
	ctx        context.Context
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{ctx: context.Background(), pool: dbtest.Migrated(t)}
	var err error
	if w.berlin, err = time.LoadLocation("Europe/Berlin"); err != nil {
		t.Skip("no time zone data")
	}
	w.now = time.Date(2026, 11, 1, 12, 0, 0, 0, w.berlin)
	w.s = &Store{Pool: w.pool, Now: func() time.Time { return w.now }}
	for name, id := range map[string]*uuid.UUID{"alice": &w.alice, "bob": &w.bob} {
		if err := w.pool.QueryRow(w.ctx, "INSERT INTO app_user (username, display_name, role, auth_source) VALUES ($1, $1 || ' A', 'user', 'local') RETURNING id", name).Scan(id); err != nil {
			t.Fatal(err)
		}
	}
	w.pool.QueryRow(w.ctx, "INSERT INTO cell (name, state, cap_audio_nb) VALUES ('Hall', 'active', 2) RETURNING id").Scan(&w.hall)
	w.pool.QueryRow(w.ctx, "INSERT INTO cell (name, state) VALUES ('Shed', 'active') RETURNING id").Scan(&w.shed)
	w.pool.QueryRow(w.ctx, "INSERT INTO sip_account (owner_user_id, label, username, domain) VALUES ($1, 'Home', 'u', 'd.example') RETURNING id", w.alice).Scan(&w.line)
	w.pool.QueryRow(w.ctx, "INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc, display_name) VALUES ($1, '\\x01', 1, '\\x', 'Hall phone') RETURNING id", w.alice).Scan(&w.hs)
	return w
}

// call makes one call of owner from start to end (end zero: not ended), answered after 2 s unless talk is false, on the cells.
func (w *world) call(owner uuid.UUID, start, end time.Time, answered bool, dir string, mos float64, cells ...uuid.UUID) {
	var ans, ended any
	if answered {
		ans = start.Add(2 * time.Second)
	}
	if !end.IsZero() {
		ended = end
	}
	q := `{}`
	if mos > 0 {
		q = `{"mos": ` + formatFloat(mos) + `}`
	}
	if cells == nil {
		cells = []uuid.UUID{}
	}
	if _, err := w.pool.Exec(w.ctx, `INSERT INTO call_log (owner_user_id, handset_id, sip_account_id, direction, started_at, answered_at, ended_at, cell_ids, quality)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb)`, owner, w.hs, w.line, dir, start, ans, ended, cells, q); err != nil {
		panic(err)
	}
}

func formatFloat(f float64) string {
	s := []byte{}
	whole := int(f)
	s = append(s, byte('0'+whole))
	s = append(s, '.')
	frac := int((f-float64(whole))*100 + 0.5)
	s = append(s, byte('0'+frac/10), byte('0'+frac%10))
	return string(s)
}

func at(w *world, day, h, m int) time.Time {
	return time.Date(2026, 10, day, h, m, 0, 0, w.berlin)
}

func TestPeaks(t *testing.T) {
	w := newWorld(t)
	s := w.s
	// Oct 30: two calls that touch (one ends when the next starts): never two at once
	w.call(w.alice, at(w, 30, 8, 0), at(w, 30, 9, 0), true, "out", 0, w.hall)
	w.call(w.alice, at(w, 30, 9, 0), at(w, 30, 10, 0), true, "out", 0, w.hall)
	// Oct 31: three overlapping calls (peak 3 between 10:15 and 10:20) and one that crosses midnight
	w.call(w.alice, at(w, 31, 10, 0), at(w, 31, 10, 30), true, "in", 0, w.hall)
	w.call(w.bob, at(w, 31, 10, 10), at(w, 31, 10, 20), true, "in", 0, w.hall)
	w.call(w.bob, at(w, 31, 10, 15), at(w, 31, 10, 25), true, "in", 0, w.hall, w.shed)
	w.call(w.alice, at(w, 31, 23, 50), time.Date(2026, 11, 1, 0, 10, 0, 0, w.berlin), true, "out", 0, w.shed)
	// Nov 1: two overlapping (11:02 to 11:05), and one that has not ended and began an hour ago; a lost one from days ago is not counted
	w.call(w.alice, time.Date(2026, 11, 1, 11, 0, 0, 0, w.berlin), time.Date(2026, 11, 1, 11, 5, 0, 0, w.berlin), true, "out", 0, w.hall)
	w.call(w.bob, time.Date(2026, 11, 1, 11, 2, 0, 0, w.berlin), time.Date(2026, 11, 1, 11, 10, 0, 0, w.berlin), true, "in", 0, w.hall)
	w.call(w.bob, w.now.Add(-time.Hour), time.Time{}, true, "in", 0, w.shed)
	w.call(w.bob, w.now.Add(-72*time.Hour), time.Time{}, true, "in", 0, w.shed)

	// three calls on a third base station that began on Oct 29 and are all still going on when the window begins (Oct 30 00:00): the
	// peak of 3 is at the window's first moment, not at any event after it
	var cave uuid.UUID
	w.pool.QueryRow(w.ctx, "INSERT INTO cell (name, state) VALUES ('Cave', 'active') RETURNING id").Scan(&cave)
	w.call(w.alice, at(w, 29, 20, 0), at(w, 30, 1, 0), true, "out", 0, cave)
	w.call(w.alice, at(w, 29, 20, 10), at(w, 30, 1, 10), true, "out", 0, cave)
	w.call(w.alice, at(w, 29, 20, 20), at(w, 30, 1, 20), true, "out", 0, cave)
	peaks, err := s.Peaks(w.ctx, Scope{}, 3)
	if err != nil {
		t.Fatal(err)
	}
	got := []int{peaks[0].Peak, peaks[1].Peak, peaks[2].Peak}
	// Nov 1: the 11:00-11:10 pair is 2, plus the open call from 11:00 (it began at 11:00 = now - 1 h): 3 at 11:02
	if got[0] != 3 || got[1] != 3 || got[2] != 3 { // Oct 30 carries the three calls of the cave
		t.Errorf("peaks per day: %v (oldest first: Oct 30, Oct 31, Nov 1)", got)
	}
	// a day where only a call from the day before is still going on has that call as its peak
	w.now = time.Date(2026, 11, 2, 0, 5, 0, 0, w.berlin)
	w.pool.Exec(w.ctx, "DELETE FROM call_log WHERE ended_at IS NULL")
	w.call(w.bob, time.Date(2026, 11, 1, 23, 0, 0, 0, w.berlin), time.Date(2026, 11, 2, 3, 0, 0, 0, w.berlin), true, "in", 0)
	peaks, _ = s.Peaks(w.ctx, Scope{}, 2)
	if peaks[1].Peak != 1 {
		t.Errorf("a call that crosses midnight is not counted on the day after: %+v", peaks)
	}
	w.now = time.Date(2026, 11, 1, 12, 0, 0, 0, w.berlin)

	// per user: alice alone never has more than one at once on Oct 31 (her 10:00 call, then the midnight one)
	own, _ := s.Peaks(w.ctx, Own(w.alice), 3)
	if own[1].Peak != 1 {
		t.Errorf("alice's own peak on Oct 31: %+v", own)
	}

	// per base station: Hall had three at once (capacity 2), Shed one at a time (the call it shared with Hall, the midnight call and the open one never overlap)
	cps, err := s.PeakPerCell(w.ctx, Scope{}, 3)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]CellPeak{}
	for _, c := range cps {
		byName[c.Name] = c
	}
	if byName["Cave"].Peak != 3 {
		t.Errorf("calls that began before the window and are going on at its start: %+v", cps)
	}
	if byName["Hall"].Peak != 3 || byName["Hall"].Capacity != 2 || byName["Shed"].Peak != 1 || byName["Shed"].Capacity != 0 {
		t.Errorf("per cell: %+v", cps)
	}
	if len(cps) != 3 || cps[2].Name != "Shed" {
		t.Errorf("highest first: %+v", cps)
	}
}

func TestHeatUsesTheLocalClock(t *testing.T) {
	w := newWorld(t)
	// 2026-10-31 23:30 UTC is Sunday 00:30 in Berlin (CET, UTC+1): weekday 6, hour 0
	w.call(w.alice, time.Date(2026, 10, 31, 23, 30, 0, 0, time.UTC), time.Date(2026, 10, 31, 23, 40, 0, 0, time.UTC), true, "out", 0)
	w.call(w.bob, time.Date(2026, 10, 31, 23, 45, 0, 0, time.UTC), time.Date(2026, 10, 31, 23, 50, 0, 0, time.UTC), true, "out", 0)
	// Wednesday 28 October 2026, 09:15 Berlin (CET)
	w.call(w.alice, time.Date(2026, 10, 28, 9, 15, 0, 0, w.berlin), time.Date(2026, 10, 28, 9, 20, 0, 0, w.berlin), true, "out", 0)
	// before the window
	w.call(w.alice, time.Date(2026, 9, 1, 9, 15, 0, 0, w.berlin), time.Date(2026, 9, 1, 9, 20, 0, 0, w.berlin), true, "out", 0)
	h, err := w.s.Heat(w.ctx, Scope{}, 14)
	if err != nil {
		t.Fatal(err)
	}
	if h.Cells[6][0] != 2 || h.Cells[2][9] != 1 || h.Total != 3 || h.Max != 2 {
		t.Errorf("heat: Sunday 0h %d, Wednesday 9h %d, total %d, max %d", h.Cells[6][0], h.Cells[2][9], h.Total, h.Max)
	}
	if h, _ = w.s.Heat(w.ctx, Own(w.bob), 14); h.Total != 1 || h.Cells[6][0] != 1 {
		t.Errorf("bob's heat: %+v", h.Total)
	}
	if h, _ = w.s.Heat(w.ctx, Scope{}, 3); h.Total != 2 {
		t.Errorf("a 3-day window holds %d, want 2", h.Total)
	}
}

func TestUsagePerLineAndHandset(t *testing.T) {
	w := newWorld(t)
	d := func(h, m int) time.Time { return time.Date(2026, 10, 31, h, m, 0, 0, w.berlin) }
	w.call(w.alice, d(9, 0), d(9, 10), true, "out", 4.3)
	w.call(w.alice, d(10, 0), d(10, 1), false, "in", 0)  // missed
	w.call(w.alice, d(11, 0), d(11, 1), false, "out", 0) // failed outgoing: not missed
	w.call(w.alice, d(12, 0), d(12, 5), true, "in", 3.0) // poor
	// a call on a line that was removed since
	w.pool.Exec(w.ctx, "UPDATE call_log SET sip_account_id = NULL WHERE started_at = $1", d(12, 0))
	lines, err := w.s.PerLine(w.ctx, Own(w.alice), 7)
	if err != nil || len(lines) != 2 {
		t.Fatalf("per line: %v %+v", err, lines)
	}
	home, gone := lines[0], lines[1]
	if home.Name != "Home" || home.ID != w.line || home.Calls != 3 || home.Answered != 1 || home.Missed != 1 || home.TalkSeconds != 598 || home.MOSCalls != 1 || home.AvgMOS < 4.29 || home.AvgMOS > 4.31 || home.Poor != 0 {
		t.Errorf("Home: %+v", home)
	}
	if gone.ID != uuid.Nil || gone.Name != "" || gone.Calls != 1 || gone.Poor != 1 || gone.AvgMOS != 3.0 {
		t.Errorf("removed line: %+v", gone)
	}
	hs, _ := w.s.PerHandset(w.ctx, Own(w.alice), 7)
	if len(hs) != 1 || hs[0].Name != "Hall phone" || hs[0].Calls != 4 || hs[0].Poor != 1 || hs[0].MOSCalls != 2 {
		t.Errorf("per handset: %+v", hs)
	}
	if other, _ := w.s.PerLine(w.ctx, Own(w.bob), 7); len(other) != 0 {
		t.Errorf("bob sees alice's lines: %+v", other)
	}
	if none, _ := w.s.PerLine(w.ctx, Own(w.alice), 0); len(none) != 0 {
		t.Errorf("a window of 0 days holds calls: %+v", none)
	}

	q, err := w.s.Quality(w.ctx, Own(w.alice), 3)
	if err != nil || len(q) != 3 {
		t.Fatalf("quality: %v %+v", err, q)
	}
	if q[1].Calls != 2 || q[1].Poor != 1 || q[1].AvgMOS < 3.64 || q[1].AvgMOS > 3.66 || q[0].Calls != 0 || q[2].Calls != 0 {
		t.Errorf("quality by day: %+v", q)
	}
	worst, _ := w.s.Worst(w.ctx, Own(w.alice), 7, 5)
	if len(worst) != 1 || worst[0].MOS != 3.0 {
		t.Errorf("worst: %+v", worst)
	}
	if worst, _ = w.s.Worst(w.ctx, Own(w.bob), 7, 5); len(worst) != 0 {
		t.Error("bob sees alice's worst call")
	}
}

func TestCompareAndUsageReports(t *testing.T) {
	w := newWorld(t)
	day := func(back int, h int) time.Time {
		return time.Date(2026, 11, 1, h, 0, 0, 0, w.berlin).AddDate(0, 0, -back)
	}
	// the last 3 days (Oct 30, 31, Nov 1): 3 calls, 2 answered, one missed, 60 s of talk
	w.call(w.alice, day(0, 9), day(0, 9).Add(time.Minute+2*time.Second), true, "out", 0)
	w.call(w.alice, day(1, 9), day(1, 9).Add(30*time.Second), false, "in", 0)
	w.call(w.bob, day(2, 9), day(2, 9).Add(2*time.Second+time.Second), true, "in", 0)
	// the 3 days before (Oct 27-29): 1 call
	w.call(w.alice, day(4, 9), day(4, 9).Add(10*time.Second), false, "out", 0)
	// older than that: in neither period
	w.call(w.alice, day(7, 9), day(7, 9).Add(10*time.Second), false, "out", 0)
	cur, prev, err := w.s.Compare(w.ctx, Scope{}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Calls != 3 || cur.Answered != 2 || cur.Missed != 1 || cur.Failed != 0 || cur.TalkSeconds != 61 || prev.Calls != 1 || prev.Failed != 1 || prev.Answered != 0 {
		t.Errorf("compare: %+v / %+v", cur, prev)
	}
	if cur, prev, _ = w.s.Compare(w.ctx, Own(w.bob), 3); cur.Calls != 1 || prev.Calls != 0 {
		t.Errorf("bob: %+v / %+v", cur, prev)
	}

	// the report for October: every call of the month by its user (alice: Oct 15, 25, 28 and 31; bob: Oct 30); alice has a handset and a line
	oct := time.Date(2026, 10, 1, 0, 0, 0, 0, w.berlin)
	w.call(w.alice, time.Date(2026, 10, 15, 12, 0, 0, 0, w.berlin), time.Date(2026, 10, 15, 12, 5, 0, 0, w.berlin), true, "out", 0)
	w.call(w.alice, time.Date(2026, 11, 1, 0, 30, 0, 0, w.berlin), time.Date(2026, 11, 1, 0, 40, 0, 0, w.berlin), true, "out", 0) // November: not in October
	w.pool.Exec(w.ctx, "INSERT INTO app_user (username, role, auth_source) VALUES ('carol', 'user', 'local')")                    // no calls, no equipment: not in the report
	users, err := w.s.MonthlyUsage(w.ctx, oct)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]UserUsage{}
	for _, u := range users {
		by[u.Username] = u
	}
	a := by["alice"]
	if len(users) != 2 {
		t.Errorf("a user with nothing to report is in the report: %+v", users)
	}
	if a.Calls != 4 || a.Handsets != 1 || a.Lines != 1 || a.DisplayName != "alice A" || by["bob"].Calls != 1 || users[0].Username != "alice" {
		t.Errorf("October: %+v", users)
	}
	// an organization sums its members; a user in two is in both
	var o1, o2 uuid.UUID
	w.pool.QueryRow(w.ctx, "INSERT INTO organization (name) VALUES ('Makers') RETURNING id").Scan(&o1)
	w.pool.QueryRow(w.ctx, "INSERT INTO organization (name) VALUES ('Club') RETURNING id").Scan(&o2)
	for _, m := range []struct{ o, u uuid.UUID }{{o1, w.alice}, {o1, w.bob}, {o2, w.bob}} {
		w.pool.Exec(w.ctx, "INSERT INTO organization_member (org_id, user_id, role) VALUES ($1, $2, 'member')", m.o, m.u)
	}
	orgs, err := w.s.OrgMonthlyUsage(w.ctx, oct)
	if err != nil || len(orgs) != 2 {
		t.Fatalf("orgs: %v %+v", err, orgs)
	}
	if orgs[0].Name != "Makers" || orgs[0].Members != 2 || orgs[0].Calls != 5 || orgs[0].Handsets != 1 || orgs[0].Lines != 1 || orgs[1].Name != "Club" || orgs[1].Calls != 1 || orgs[1].Members != 1 {
		t.Errorf("organizations: %+v", orgs)
	}
}
