// SPDX-License-Identifier: AGPL-3.0-or-later

package cell_test

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"testing"

	"github.com/openDECTcell/opendectcell-controller/internal/auth"
	"github.com/openDECTcell/opendectcell-controller/internal/cell"
)

// The RFPI of figure 5.8 (EN 300 175-6), built bit by bit as a string so
// the test does not share the shifts of the code it checks.
func TestRFPILayout(t *testing.T) {
	id := cell.SystemIdentity{EIC: 0x1234, FPN: 0x56, FPS: 0x7, Source: cell.IdentityRandom}
	bits := "0" + "001" + fmt.Sprintf("%016b", 0x1234) + fmt.Sprintf("%08b", 0x56) +
		fmt.Sprintf("%04b", 0x7) + fmt.Sprintf("%08b", 0x89)
	if len(bits) != 40 {
		t.Fatalf("test vector has %d bits", len(bits))
	}
	var want [5]byte
	for i := range want {
		v, _ := strconv.ParseUint(bits[i*8:i*8+8], 2, 8)
		want[i] = byte(v)
	}
	if got := id.RFPI(0x89); got != want {
		t.Fatalf("RFPI = % x, want % x", got, want)
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
	for i := 0; i < 50; i++ {
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
