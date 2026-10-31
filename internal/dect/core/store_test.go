// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"bytes"
	"context"
	"testing"

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
	if _, err := s.SaveRecord(ctx, a, uuid.Nil, uuid.Nil, rec(2, 0x11)); err == nil {
		t.Fatal("an enrolment without an owner was stored")
	}

	// A handset enrols on cell a in chip slot 2 through anna's window: a
	// new row she owns, a home cell, the key sealed.
	id, err := s.SaveRecord(ctx, a, uuid.Nil, anna, rec(2, 0x11))
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
	if _, err := s.SaveRecord(ctx, a, uuid.Nil, ben, again); err == nil {
		t.Fatal("re-enrolment under another owner was stored")
	}

	// Through anna's it keeps its row; the key changes.
	id2, err := s.SaveRecord(ctx, a, uuid.Nil, anna, again)
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
	if err := s.Forget(ctx, a, id); err != nil {
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
		if _, err := s.SaveRecord(ctx, c, uuid.Nil, anna, rec(uint(i+1), byte(0x20+i))); err != nil {
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
