// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"encoding/binary"
	"math/rand"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/dect/stack"
)

// The store at the size the project aims for: 100 cells and 500 handsets (about ten
// times what a pool of 90 TPUIs could name), handsets that enrol on a home cell and then
// move between cells. After every round no cell's table holds two handsets with one TPUI,
// every pooled TPUI is one the 06.88 chip hears, and the work per move stays small.
func TestStoreScale(t *testing.T) {
	if testing.Short() {
		t.Skip("scale")
	}
	const nCells, nHandsets, rounds = 100, 500, 4
	s, ctx := newStore(t)
	rnd := rand.New(rand.NewSource(1))
	cells := make([]uuid.UUID, nCells)
	for i := range cells {
		cells[i] = s.newCell(t, ctx, "c"+uuid.NewString()[:6])
	}
	users := make([]uuid.UUID, 20)
	for i := range users {
		users[i] = s.newUser(t, ctx, "u"+uuid.NewString()[:6])
	}
	type hs struct {
		id   uuid.UUID
		ipui []byte
		at   uuid.UUID
	}
	all := make([]hs, nHandsets)
	perCell := map[uuid.UUID]int{}
	start := time.Now()
	for i := range all {
		home := cells[rnd.Intn(nCells)]
		tp, err := s.ReserveTPUI(ctx, home)
		if err != nil {
			t.Fatalf("handset %d: %v", i, err)
		}
		var r stack.Record
		r.Slot = uint(maxSlot + 1 + perCell[home])
		perCell[home]++
		r.PortID = binary.BigEndian.AppendUint32([]byte{0x80}, uint32(i+1))
		r.TPUI = tp
		for k := range r.UAK {
			r.UAK[k] = byte(i + k)
		}
		sv, err := s.SaveRecord(ctx, home, uuid.Nil, users[i%len(users)], r)
		if err != nil || !sv.Pooled {
			t.Fatalf("enrolling handset %d: %+v %v", i, sv, err)
		}
		all[i] = hs{id: sv.ID, ipui: r.PortID, at: home}
	}
	t.Logf("enrolled %d handsets on %d cells in %v", nHandsets, nCells, time.Since(start).Round(time.Millisecond))

	check := func(when string) {
		t.Helper()
		var dup int
		if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM (SELECT cs.cell_id, h.tpui & 65535 FROM cell_slot cs
			JOIN handset h ON h.id = cs.handset_id WHERE h.pooled GROUP BY 1, 2 HAVING count(*) > 1) d`).Scan(&dup); err != nil || dup != 0 {
			t.Fatalf("%s: %d TPUIs held twice in one cell's table (%v)", when, dup, err)
		}
		var bad int
		if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM handset WHERE pooled AND NOT ((tpui & 255) >= 16 AND (tpui & 255) % 16 BETWEEN 1 AND 6 AND tpui & 65280 = 4352)`).Scan(&bad); err != nil || bad != 0 {
			t.Fatalf("%s: %d pooled TPUIs the 06.88 chip does not hear (%v)", when, bad, err)
		}
	}
	check("after enrolment")
	var distinct, shared int
	if err := s.Pool.QueryRow(ctx, `SELECT count(DISTINCT tpui), 500 - count(DISTINCT tpui) FROM handset`).Scan(&distinct, &shared); err != nil {
		t.Fatal(err)
	}
	if distinct > poolSize || shared == 0 {
		t.Errorf("%d distinct TPUIs for %d handsets: the pool should be reused across cells (%d shared)", distinct, nHandsets, shared)
	}

	moves := 0
	start = time.Now()
	for round := 1; round <= rounds; round++ {
		for i := range all {
			to := cells[rnd.Intn(nCells)]
			if to == all[i].at {
				continue
			}
			if _, ok, err := s.PlaceByIPUI(ctx, to, all[i].ipui); err != nil || !ok {
				t.Fatalf("round %d: handset %d to a cell: %v %v", round, i, ok, err)
			}
			if err := s.Unplace(ctx, all[i].at, all[i].id); err != nil {
				t.Fatal(err)
			}
			all[i].at = to
			moves++
		}
		check("after round " + string(rune('0'+round)))
	}
	per := time.Since(start) / time.Duration(moves)
	t.Logf("%d moves in %v (%v each)", moves, time.Since(start).Round(time.Millisecond), per.Round(time.Microsecond))
	if per > 50*time.Millisecond {
		t.Errorf("a move takes %v: too slow for a cell that fetches a handset while it waits", per)
	}

	// every cell loads its stack from the database in a fraction of a second
	start = time.Now()
	for _, c := range cells {
		if _, _, err := s.CellRecords(ctx, c, 6); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("%d stack loads in %v", nCells, time.Since(start).Round(time.Millisecond))
	check("after the stack loads")
}

// A call is offered to the cells in the order Records.CellOrder gives: where the handset
// was heard in the last week first, then the nearest to the cell it was last on, cells
// without a position last; the limit holds.
func TestStoreCellOrder(t *testing.T) {
	s, ctx := newStore(t)
	anna := s.newUser(t, ctx, "anna")
	at := func(lat, lon float64) uuid.UUID {
		id := s.newCell(t, ctx, "c"+uuid.NewString()[:6])
		if _, err := s.Pool.Exec(ctx, "UPDATE cell SET latitude = $2, longitude = $3 WHERE id = $1", id, lat, lon); err != nil {
			t.Fatal(err)
		}
		return id
	}
	home := at(52.00, 13.00)
	near := at(52.01, 13.00)
	mid := at(52.50, 13.00)
	far := at(48.10, 11.50)
	nowhere := s.newCell(t, ctx, "nowhere")
	moved := at(53.50, 10.00) // far away but the handset was heard there yesterday
	id, err := s.save(ctx, home, uuid.Nil, anna, rec(7, 0x33))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, "INSERT INTO handset_move (handset_id, from_cell_id, to_cell_id, at) VALUES ($1, $2, $3, now() - interval '1 day')", id, home, moved); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, "INSERT INTO handset_move (handset_id, from_cell_id, to_cell_id, at) VALUES ($1, $2, $3, now() - interval '30 days')", id, home, far); err != nil {
		t.Fatal(err)
	}
	up := []uuid.UUID{far, nowhere, mid, home, moved, near}
	got, err := s.CellOrder(ctx, id, home, up, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []uuid.UUID{moved, home, near, mid, far, nowhere}
	if len(got) != len(want) {
		t.Fatalf("got %d cells, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("position %d: %v, want %v (moved, home, near, mid, far, nowhere)", i, got[i], want[i])
		}
	}
	if got, _ := s.CellOrder(ctx, id, uuid.Nil, up, 2); len(got) != 2 || got[0] != moved {
		t.Errorf("limit 2 without an anchor: %v", got)
	}
}
