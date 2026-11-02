// SPDX-License-Identifier: AGPL-3.0-or-later

package cell_test

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/cell"
)

// The RFPI of figure 5.8 (EN 300 175-6), built bit by bit as a string so
// the test does not share the shifts of the code it checks.
func TestRFPILayout(t *testing.T) {
	id := cell.SystemIdentity{EIC: 0x1234, FPN: 0x56, FPS: 0x7, Source: cell.IdentityRandom}
	bits := "0" + "001" + fmt.Sprintf("%016b", 0x1234) + fmt.Sprintf("%08b", 0x58) +
		fmt.Sprintf("%04b", 0x9) + fmt.Sprintf("%08b", 0)
	if len(bits) != 40 {
		t.Fatalf("test vector has %d bits", len(bits))
	}
	var want [5]byte
	for i := range want {
		v, _ := strconv.ParseUint(bits[i*8:i*8+8], 2, 8)
		want[i] = byte(v)
	}
	// cell 3: the FPN counted up by two, the FPS by two, the RPN 0
	if got := id.RFPI(3); got != want {
		t.Fatalf("RFPI = % x, want % x", got, want)
	}
	// cell 1 is the identity itself (cell 0 is taken for it), the RPN is 0
	fpn := func(r [5]byte) uint8 { return r[2]<<4 | r[3]>>4 }
	if one := id.RFPI(1); fpn(one) != 0x56 || one[4] != 0 || id.RFPI(0) != one {
		t.Fatalf("cell 1 is % x", one)
	}
	// no cell has an RPN, no two cells share an ARI, and 15 neighbours differ in
	// the FMID (the RPN and the FPS: the low 12 bits)
	fps := func(r [5]byte) uint8 { return r[3] & 0x0f }
	seen := map[[5]byte]bool{}
	fmid := map[uint16]bool{}
	for n := 1; n <= 255; n++ {
		r := id.RFPI(uint8(n))
		if r[4] != 0 || fps(r) < 1 {
			t.Fatalf("cell %d: % x", n, r)
		}
		if seen[r] {
			t.Fatalf("cell %d repeats an RFPI", n)
		}
		seen[r] = true
		if n <= 15 {
			f := uint16(fps(r))<<8 | uint16(r[4])
			if fmid[f] {
				t.Fatalf("cell %d repeats an FMID", n)
			}
			fmid[f] = true
		}
	}
	if got := id.ARI(); got>>28 != 1 || got>>31 != 0 {
		t.Fatalf("ARC of %08x is not class B", got)
	}
}

func TestIdentityRanges(t *testing.T) {
	ok := cell.SystemIdentity{EIC: 1, FPN: 0, FPS: 1, Source: cell.IdentityAllocated}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []cell.SystemIdentity{
		{EIC: 0, FPS: 1, Source: cell.IdentityRandom},
		{EIC: 1, FPS: 0, Source: cell.IdentityRandom},
		{EIC: 1, FPS: 16, Source: cell.IdentityRandom},
		{EIC: 1, FPS: 1, Source: "guessed"},
	} {
		if err := bad.Validate(); !errors.Is(err, cell.ErrBadIdentity) {
			t.Errorf("%+v: err = %v", bad, err)
		}
	}
}

func TestRandomIdentity(t *testing.T) {
	// a zero EIC is skipped; FPS lands in 1..15
	r := bytes.NewReader([]byte{0, 0, 9, 9, 0xAB, 0xCD, 0xFF, 14})
	id, err := cell.RandomIdentity(r)
	if err != nil {
		t.Fatal(err)
	}
	if id.EIC != 0xABCD || id.FPN != 0xFF || id.FPS != 15 || id.Source != cell.IdentityRandom {
		t.Fatalf("got %+v", id)
	}
	seen := map[uint16]bool{}
	for range 50 {
		id, err := cell.RandomIdentity(nil)
		if err != nil || id.Validate() != nil {
			t.Fatalf("%+v: %v", id, err)
		}
		seen[id.EIC] = true
	}
	if len(seen) < 40 {
		t.Fatalf("only %d distinct EICs in 50 draws", len(seen))
	}
}

func TestSystemIdentityStored(t *testing.T) {
	e := newEnv(t)
	if id, err := e.store.SystemIdentity(e.ctx); err != nil || id != nil {
		t.Fatalf("fresh install: %v, %v", id, err)
	}
	user, err := e.users.CreateLocalUser(e.ctx, auth.NewLocalUser{Username: "plain", Password: "plain password", Role: auth.RoleUser})
	if err != nil {
		t.Fatal(err)
	}
	want := cell.SystemIdentity{EIC: 0x4242, FPN: 7, FPS: 3, Source: cell.IdentityRandom}
	if err := e.store.SetSystemIdentity(e.ctx, user, want); !errors.Is(err, cell.ErrNotFound) {
		t.Fatalf("a user set it: %v", err)
	}
	if err := e.store.SetSystemIdentity(e.ctx, e.admin, cell.SystemIdentity{EIC: 0, FPS: 1, Source: cell.IdentityRandom}); !errors.Is(err, cell.ErrBadIdentity) {
		t.Fatalf("EIC 0 taken: %v", err)
	}
	if err := e.store.SetSystemIdentity(e.ctx, e.admin, want); err != nil {
		t.Fatal(err)
	}
	want.EIC = 0x4343
	want.Source = cell.IdentityAllocated
	if err := e.store.SetSystemIdentity(e.ctx, e.admin, want); err != nil {
		t.Fatal(err)
	}
	got, err := e.store.SystemIdentity(e.ctx)
	if err != nil || got == nil {
		t.Fatalf("%v, %v", got, err)
	}
	if got.EIC != 0x4343 || got.Source != cell.IdentityAllocated || got.SetBy != "admin" || got.SetAt.IsZero() {
		t.Fatalf("got %+v", got)
	}
	var n int
	if err := e.store.Pool.QueryRow(e.ctx,
		"SELECT count(*) FROM audit_log WHERE action = 'dect_identity.set' AND details ? 'previous'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("audit entries with the previous identity: %d, %v", n, err)
	}
}

// Class A: the donor's factory RFPI with the cell's RPN in its last 3 bits,
// nothing else changed.
func TestClassAIdentity(t *testing.T) {
	// E 0, ARC 000, EMC 0x0270, FPN 0x1abcd (17 bits), RPN 0 in the low 3 bits
	donor := [5]byte{0x02, 0x70, 0x0d, 0x5e, 0x68}
	id := cell.SystemIdentity{Class: "A", Donor: fmt.Sprintf("%x", donor), Source: cell.IdentityDonor}
	if err := id.Validate(); err != nil {
		t.Fatal(err)
	}
	if id.MaxRPN() != 7 || id.ParkPLI() != 0 || !id.IsA() {
		t.Fatalf("class A: max %d, PLI %d", id.MaxRPN(), id.ParkPLI())
	}
	got := id.RFPI(5)
	want := donor
	want[4] |= 5
	if got != want {
		t.Fatalf("RFPI(5) = % x, want % x", got, want)
	}
	if id.RFPI(0x0d) != id.RFPI(5) {
		t.Error("an RPN of more than 3 bits leaks into the ARI")
	}
	for name, bad := range map[string]cell.SystemIdentity{
		"class B donor":  {Class: "A", Donor: "1002701101", Source: cell.IdentityDonor},
		"RPN set":        {Class: "A", Donor: "0270000005", Source: cell.IdentityDonor},
		"E bit set":      {Class: "A", Donor: "8270000000", Source: cell.IdentityDonor},
		"short":          {Class: "A", Donor: "027000", Source: cell.IdentityDonor},
		"wrong source":   {Class: "A", Donor: "0270000000", Source: cell.IdentityRandom},
		"not hex":        {Class: "A", Donor: "zz70000000", Source: cell.IdentityDonor},
		"B without EIC":  {Class: "B", FPS: 1, Source: cell.IdentityRandom},
		"B with a donor": {EIC: 1, FPS: 1, Donor: "0270000000", Source: "x"},
	} {
		if err := bad.Validate(); !errors.Is(err, cell.ErrBadIdentity) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}

// RPNs are handed out from 1, stay with the cell, are freed by a revocation
// and never go beyond the class's range.
func TestAssignRPNAndWant(t *testing.T) {
	e := newEnv(t)
	ins := func(name string) uuid.UUID {
		var id uuid.UUID
		if err := e.store.Pool.QueryRow(e.ctx, "INSERT INTO cell (name, state) VALUES ($1, 'active') RETURNING id", name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	a, b := ins("A"), ins("B")
	idA := cell.SystemIdentity{Class: "A", Donor: "0270000000", Source: cell.IdentityDonor}

	// nothing set, nothing pushed: leave the cell alone
	if w, err := e.store.Want(e.ctx, a); err != nil || w.Set || w.Revert {
		t.Fatalf("no identity: %+v %v", w, err)
	}
	if err := e.store.SetIdentityApply(e.ctx, e.admin, true); !errors.Is(err, cell.ErrBadIdentity) {
		t.Fatalf("applying an identity that is not there: %v", err)
	}
	if err := e.store.SetSystemIdentity(e.ctx, e.admin, idA); err != nil {
		t.Fatal(err)
	}
	// set but not applied: still nothing
	if w, err := e.store.Want(e.ctx, a); err != nil || w.Set || w.Revert {
		t.Fatalf("identity not applied: %+v %v", w, err)
	}
	if err := e.store.SetIdentityApply(e.ctx, e.admin, true); err != nil {
		t.Fatal(err)
	}
	wa, err := e.store.Want(e.ctx, a)
	if err != nil || !wa.Set || wa.RFPI != idA.RFPI(1) || wa.ParkPLI != 0 {
		t.Fatalf("first cell: %+v %v", wa, err)
	}
	wb, _ := e.store.Want(e.ctx, b)
	if wb.RFPI != idA.RFPI(2) {
		t.Fatalf("second cell: % x", wb.RFPI)
	}
	if again, _ := e.store.Want(e.ctx, a); again.RFPI != wa.RFPI {
		t.Error("a cell's RPN changed between two looks")
	}
	// seven cells fill class A; an eighth gets no number
	for i := 3; i <= 7; i++ {
		c := ins(fmt.Sprintf("C%d", i))
		if w, err := e.store.Want(e.ctx, c); err != nil || w.RFPI != idA.RFPI(uint8(i)) {
			t.Fatalf("cell %d: %+v %v", i, w, err)
		}
	}
	eighth := ins("eighth")
	if _, err := e.store.Want(e.ctx, eighth); !errors.Is(err, cell.ErrNoRPN) {
		t.Fatalf("the eighth cell of class A: %v", err)
	}
	// revoking frees a number
	if err := e.store.Revoke(e.ctx, e.admin, b); err != nil {
		t.Fatal(err)
	}
	if w, err := e.store.Want(e.ctx, eighth); err != nil || w.RFPI != idA.RFPI(2) {
		t.Fatalf("the freed number: %+v %v", w, err)
	}

	// the way back: pushed once, then turned off
	rf := wa.RFPI
	if err := e.store.Pushed(e.ctx, a, &rf); err != nil {
		t.Fatal(err)
	}
	if err := e.store.SetIdentityApply(e.ctx, e.admin, false); err != nil {
		t.Fatal(err)
	}
	if w, _ := e.store.Want(e.ctx, a); !w.Revert || w.Set {
		t.Fatalf("turned off after a push: %+v", w)
	}
	if w, _ := e.store.Want(e.ctx, eighth); w.Revert || w.Set {
		t.Fatalf("a cell never pushed to: %+v", w)
	}
	if err := e.store.Pushed(e.ctx, a, nil); err != nil {
		t.Fatal(err)
	}
	if w, _ := e.store.Want(e.ctx, a); w.Revert || w.Set {
		t.Fatalf("after the way back: %+v", w)
	}

	// a class B identity: PLI 19, numbers up to 255, a new identity is not applied
	idB := cell.SystemIdentity{EIC: 0x0f42, FPN: 1, FPS: 1, Source: cell.IdentityRandom}
	if err := e.store.SetSystemIdentity(e.ctx, e.admin, idB); err != nil {
		t.Fatal(err)
	}
	cur, _ := e.store.SystemIdentity(e.ctx)
	if cur.Apply {
		t.Error("a new identity came already applied")
	}
	if err := e.store.SetIdentityApply(e.ctx, e.admin, true); err != nil {
		t.Fatal(err)
	}
	w, err := e.store.Want(e.ctx, a)
	if err != nil || w.ParkPLI != 19 || w.RFPI != idB.RFPI(1) {
		t.Fatalf("class B: %+v %v", w, err)
	}
	// a user cannot turn it on or off
	user, _ := e.users.CreateLocalUser(e.ctx, auth.NewLocalUser{Username: "plain", Password: "plain password", Role: auth.RoleUser})
	if err := e.store.SetIdentityApply(e.ctx, user, false); !errors.Is(err, cell.ErrNotFound) {
		t.Fatalf("a user turned it off: %v", err)
	}
}

// The donor's own identity: the RPN bits are cleared.
func TestIdentityFromCell(t *testing.T) {
	e := newEnv(t)
	if err := e.store.SystemIdentityFromCell(e.ctx, e.admin, [5]byte{0x02, 0x70, 0x0d, 0x5e, 0x6b}); err != nil {
		t.Fatal(err)
	}
	id, _ := e.store.SystemIdentity(e.ctx)
	if id == nil || !id.IsA() || id.Donor != "02700d5e68" {
		t.Fatalf("%+v", id)
	}
	if err := e.store.SystemIdentityFromCell(e.ctx, e.admin, [5]byte{0x10, 0xf4, 0x20, 0x11, 0x01}); !errors.Is(err, cell.ErrBadIdentity) {
		t.Fatalf("a class B RFPI as a donor: %v", err)
	}
}
