// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/db"
	"github.com/openDECTcell/opendectcell-controller/internal/dbtest"
	"github.com/openDECTcell/opendectcell-controller/internal/dect/stack"
	"github.com/openDECTcell/opendectcell-controller/internal/secret"
)

func newStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Migrated(t)
	ks, _ := secret.GenerateMasterKey()
	mk, _ := secret.ParseMasterKey(ks)
	kr, err := db.LoadKeyring(ctx, pool, mk, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &Store{Pool: pool, Keyring: kr}, ctx
}

func (s *Store) newCell(t *testing.T, ctx context.Context, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := s.Pool.QueryRow(ctx, "INSERT INTO cell (name, state) VALUES ($1, 'active') RETURNING id", name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (s *Store) newUser(t *testing.T, ctx context.Context, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := s.Pool.QueryRow(ctx, "INSERT INTO app_user (username, role, auth_source) VALUES ($1, 'user', 'local') RETURNING id", name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// save is SaveRecord for the tests that want only the handset's id.
func (s *Store) save(ctx context.Context, cell, id, owner uuid.UUID, r stack.Record) (uuid.UUID, error) {
	sv, err := s.SaveRecord(ctx, cell, id, owner, r)
	return sv.ID, err
}

func rec(slot uint, id byte) stack.Record {
	r := stack.Record{Slot: slot, PortID: []byte{0x80, 0xa8, 0x03, id}, CCPortID: []byte{0x80, 0xa8, 0x03, id},
		CCFixedID: []byte{0x00, 0x01, 0x23, 0x45, 0x60}}
	for k := range r.UAK {
		r.UAK[k] = id + byte(k)
	}
	return r
}

func TestStoreEnrolAndLoad(t *testing.T) {
	s, ctx := newStore(t)
	a, b := s.newCell(t, ctx, "a"), s.newCell(t, ctx, "b")
	anna, ben := s.newUser(t, ctx, "anna"), s.newUser(t, ctx, "ben")

	// No enrolment without a window's owner (the maintainers' notes point 2).
	if _, err := s.save(ctx, a, uuid.Nil, uuid.Nil, rec(2, 0x11)); err == nil {
		t.Fatal("an enrolment without an owner was stored")
	}

	// A handset enrols on cell a in chip slot 2 through anna's window: a
	// new row she owns, a home cell, the key sealed.
	id, err := s.save(ctx, a, uuid.Nil, anna, rec(2, 0x11))
	if err != nil {
		t.Fatal(err)
	}
	var enc []byte
	var tpui int32
	var own uuid.UUID
	if err := s.Pool.QueryRow(ctx, "SELECT uak_enc, tpui, owner_user_id FROM handset WHERE id = $1", id).Scan(&enc, &tpui, &own); err != nil {
		t.Fatal(err)
	}
	if own != anna {
		t.Errorf("owner %v, want anna", own)
	}
	if uak := rec(2, 0x11).UAK; bytes.Contains(enc, uak[:]) {
		t.Fatal("the UAK is stored in the clear")
	}
	if tpui < 0 || tpui > 0xFFFFF {
		t.Errorf("TPUI %#x outside the pool", tpui)
	}

	// Loaded back for cell a, in its slot, with its key and identities.
	hs, unplaced, err := s.CellRecords(ctx, a, 6)
	if err != nil || len(hs) != 1 || len(unplaced) != 0 {
		t.Fatalf("cell a: %v %v %v", hs, unplaced, err)
	}
	if h := hs[0]; h.ID != id || h.Owner != anna || h.Rec.Slot != 2 || h.Rec.UAK != rec(2, 0x11).UAK ||
		!bytes.Equal(h.Rec.PortID, rec(2, 0x11).PortID) || !bytes.Equal(h.Rec.CCFixedID, rec(2, 0x11).CCFixedID) {
		t.Errorf("loaded %+v", h)
	}

	// Cell b holds nothing yet; with the handset in its home set it gets
	// the lowest free slot there.
	if hs, _, _ := s.CellRecords(ctx, b, 6); len(hs) != 0 {
		t.Fatalf("cell b: %v", hs)
	}
	if _, err := s.Pool.Exec(ctx, "INSERT INTO handset_home_cell VALUES ($1, $2)", id, b); err != nil {
		t.Fatal(err)
	}
	if hs, _, _ := s.CellRecords(ctx, b, 6); len(hs) != 1 || hs[0].Rec.Slot != 1 {
		t.Fatalf("cell b after the home set: %v", hs)
	}

	// The same handset enrolling again through ben's window is refused:
	// a change of owner is not built.
	again := rec(3, 0x11)
	again.UAK[0] ^= 0xFF
	if _, err := s.save(ctx, a, uuid.Nil, ben, again); err == nil {
		t.Fatal("re-enrolment under another owner was stored")
	}

	// Through anna's it keeps its row; the key changes.
	id2, err := s.save(ctx, a, uuid.Nil, anna, again)
	if err != nil || id2 != id {
		t.Fatalf("re-enrolment: %v %v", id2, err)
	}
	hs, _, _ = s.CellRecords(ctx, a, 6)
	if len(hs) != 1 || hs[0].Rec.Slot != 3 || hs[0].Rec.UAK != again.UAK {
		t.Errorf("after re-enrolment: %+v", hs)
	}

	// Installed: the record's slot present, every other one empty.
	if err := s.Installed(ctx, a, []uint{3}); err != nil {
		t.Fatal(err)
	}
	var st string
	s.Pool.QueryRow(ctx, "SELECT state FROM cell_slot WHERE cell_id = $1 AND slot = 3", a).Scan(&st)
	if st != "present" {
		t.Errorf("slot 3 is %s", st)
	}
	if err := s.SlotUnknown(ctx, a, 3); err != nil {
		t.Fatal(err)
	}
	s.Pool.QueryRow(ctx, "SELECT state FROM cell_slot WHERE cell_id = $1 AND slot = 3", a).Scan(&st)
	if st != "unknown" {
		t.Errorf("slot 3 after a reject is %s", st)
	}

	// Deregistered on a: its slot there is free and it is not placed there
	// again; the handset stays, still at home on b.
	if _, err := s.Forget(ctx, a, id); err != nil {
		t.Fatal(err)
	}
	if hs, _, _ := s.CellRecords(ctx, a, 6); len(hs) != 0 {
		t.Errorf("cell a after forget: %v", hs)
	}
	if hs, _, _ := s.CellRecords(ctx, b, 6); len(hs) != 1 || hs[0].ID != id {
		t.Errorf("cell b after forget on a: %v", hs)
	}
}

func TestStoreCellFull(t *testing.T) {
	s, ctx := newStore(t)
	c := s.newCell(t, ctx, "c")
	anna := s.newUser(t, ctx, "anna")
	for i := range 3 {
		if _, err := s.save(ctx, c, uuid.Nil, anna, rec(uint(i+1), byte(0x20+i))); err != nil {
			t.Fatal(err)
		}
	}
	// One more handset in the home set than the cell has slots for.
	var extra uuid.UUID
	if err := s.Pool.QueryRow(ctx, `INSERT INTO handset (owner_user_id, ipui, tpui, uak_enc) VALUES ($1, '\x99', 77, '\x') RETURNING id`, anna).Scan(&extra); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, "INSERT INTO handset_home_cell VALUES ($1, $2)", extra, c); err != nil {
		t.Fatal(err)
	}
	hs, unplaced, err := s.CellRecords(ctx, c, 3)
	if err != nil || len(hs) != 3 || len(unplaced) != 1 || unplaced[0] != extra {
		t.Fatalf("full cell: %d records, unplaced %v, %v", len(hs), unplaced, err)
	}
}

// Seen follows the handset from cell to cell: it returns where it was before,
// and an older report does not undo a newer one.
func TestStoreSeen(t *testing.T) {
	s, ctx := newStore(t)
	a, b := s.newCell(t, ctx, "a"), s.newCell(t, ctx, "b")
	owner := s.newUser(t, ctx, "owner")
	id, err := s.save(ctx, a, uuid.Nil, owner, rec(1, 1))
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().Truncate(time.Millisecond)
	if prev, err := s.Seen(ctx, a, id, t0); err != nil || prev != uuid.Nil {
		t.Fatalf("first sighting: prev %v, %v", prev, err)
	}
	if prev, err := s.Seen(ctx, b, id, t0.Add(time.Minute)); err != nil || prev != a {
		t.Fatalf("moved to b: prev %v, %v (want a)", prev, err)
	}
	// a late report from a: the newer sighting on b stays
	if _, err := s.Seen(ctx, a, id, t0.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	var on uuid.UUID
	var at time.Time
	if err := s.Pool.QueryRow(ctx, "SELECT last_seen_cell_id, last_seen_at FROM handset WHERE id = $1", id).Scan(&on, &at); err != nil {
		t.Fatal(err)
	}
	if on != b || !at.Equal(t0.Add(time.Minute)) {
		t.Fatalf("stored %v at %v, want b at %v", on, at, t0.Add(time.Minute))
	}
	// an unknown handset is not an error
	if _, err := s.Seen(ctx, a, uuid.New(), t0); err != nil {
		t.Fatalf("unknown handset: %v", err)
	}
}

// A pooled handset is held by every cell, host-only, under its pool TPUI; a
// handset of the chip kind keeps its slot and its own kind of TPUI.
func TestStorePooledHandset(t *testing.T) {
	s, ctx := newStore(t)
	a, b := s.newCell(t, ctx, "a"), s.newCell(t, ctx, "b")
	anna := s.newUser(t, ctx, "anna")

	// A chip-slot handset on a first, and another that is pooled.
	chip, err := s.save(ctx, a, uuid.Nil, anna, rec(1, 0x31))
	if err != nil {
		t.Fatal(err)
	}
	var lt int32
	if err := s.Pool.QueryRow(ctx, "SELECT tpui FROM handset WHERE id = $1", chip).Scan(&lt); err != nil {
		t.Fatal(err)
	}
	if lt&0xFF00 == 0x1100 {
		t.Errorf("a chip handset got TPUI %#x, in the pool's range", lt)
	}
	tp, err := s.ReserveTPUI(ctx)
	if err != nil || tp[0] != 0 || tp[1] != 0x11 || tp[2] < poolFirst {
		t.Fatalf("offer %x, %v", tp, err)
	}
	if again, _ := s.ReserveTPUI(ctx); again == tp {
		t.Errorf("one TPUI offered twice: %x", tp)
	}
	r := rec(2, 0x32)
	r.TPUI = tp
	saved, err := s.SaveRecord(ctx, a, uuid.Nil, anna, r)
	if err != nil || !saved.Enrolled || !saved.Pooled || saved.Again {
		t.Fatalf("pooled enrolment: %+v, %v", saved, err)
	}
	var pt int32
	if err := s.Pool.QueryRow(ctx, "SELECT tpui FROM handset WHERE id = $1", saved.ID).Scan(&pt); err != nil || pt != 0x1100|int32(tp[2]) {
		t.Fatalf("stored TPUI %#x, want %#x (%v)", pt, 0x1100|int32(tp[2]), err)
	}
	if s.reserved[tp[2]] {
		t.Error("the TPUI is still on offer after it was given away")
	}

	// Loaded on a: the chip handset in its slot, the pooled one moved out of the
	// chip to the first record number above it. On b, which never saw either of
	// them as home, only the pooled one, the same way.
	hs, unplaced, err := s.CellRecords(ctx, a, 6)
	if err != nil || len(hs) != 2 || len(unplaced) != 0 {
		t.Fatalf("a: %v %v %v", hs, unplaced, err)
	}
	if hs[0].ID != chip || hs[0].Rec.Slot != 1 || hs[0].Rec.TPUI != ([3]byte{}) {
		t.Errorf("a's chip handset: %+v", hs[0])
	}
	if hs[1].ID != saved.ID || hs[1].Rec.Slot != poolFirst || hs[1].Rec.TPUI != tp || hs[1].Rec.UAK != r.UAK {
		t.Errorf("a's pooled handset: %+v", hs[1])
	}
	hs, _, err = s.CellRecords(ctx, b, 6)
	if err != nil || len(hs) != 1 || hs[0].ID != saved.ID || hs[0].Rec.Slot != poolFirst || hs[0].Rec.TPUI != tp {
		t.Fatalf("b: %+v, %v", hs, err)
	}
	var state string
	if err := s.Pool.QueryRow(ctx, "SELECT state FROM cell_slot WHERE cell_id = $1 AND handset_id = $2", b, saved.ID).Scan(&state); err != nil || state != "empty" {
		t.Errorf("a host-only record is %q in the chip's table (%v), want empty", state, err)
	}

	// A cell that appears later gets it by PlaceHandset; a second time, or a
	// chip handset, is not placed.
	c := s.newCell(t, ctx, "c")
	h, ok, err := s.PlaceHandset(ctx, c, saved.ID)
	if err != nil || !ok || h.Rec.Slot != poolFirst || h.Rec.TPUI != tp {
		t.Fatalf("place: %+v %v %v", h, ok, err)
	}
	if _, ok, _ := s.PlaceHandset(ctx, c, saved.ID); ok {
		t.Error("placed twice")
	}
	if _, ok, _ := s.PlaceHandset(ctx, c, chip); ok {
		t.Error("a chip handset was placed on a cell that is not its home")
	}

	// It deregistered on b: gone from every cell, the row stays.
	pooled, err := s.Forget(ctx, b, saved.ID)
	if err != nil || !pooled {
		t.Fatalf("forget: %v %v", pooled, err)
	}
	for _, cell := range []uuid.UUID{a, b, c} {
		hs, _, _ := s.CellRecords(ctx, cell, 6)
		for _, h := range hs {
			if h.ID == saved.ID {
				t.Errorf("cell %v still holds the deregistered handset", cell)
			}
		}
	}
	var n int
	if err := s.Pool.QueryRow(ctx, "SELECT count(*) FROM handset WHERE id = $1", saved.ID).Scan(&n); err != nil || n != 1 {
		t.Errorf("the handset's row: %d, %v", n, err)
	}
}

// The pool has 249 TPUIs; the 250th offer says so, and a TPUI given back is
// offered again.
func TestStoreTPUIPool(t *testing.T) {
	s, ctx := newStore(t)
	seen := map[byte]bool{}
	var last [3]byte
	for range 255 - poolFirst + 1 {
		tp, err := s.ReserveTPUI(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if seen[tp[2]] {
			t.Fatalf("%x offered twice", tp)
		}
		seen[tp[2]] = true
		last = tp
	}
	if _, err := s.ReserveTPUI(ctx); !errors.Is(err, ErrPoolFull) {
		t.Errorf("the 250th offer: %v", err)
	}
	s.ReleaseTPUI(last)
	if tp, err := s.ReserveTPUI(ctx); err != nil || tp != last {
		t.Errorf("after a release: %x %v, want %x", tp, err, last)
	}
	off := &Store{Pool: s.Pool, Keyring: s.Keyring, NoPool: true}
	if tp, err := off.ReserveTPUI(ctx); err != nil || tp != ([3]byte{}) {
		t.Errorf("pool off: %x %v", tp, err)
	}
}
