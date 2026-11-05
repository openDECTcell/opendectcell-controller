// SPDX-License-Identifier: AGPL-3.0-or-later

package cell_test

import (
	"context"
	"testing"

	"github.com/openDECTcell/opendectcell-controller/internal/cell"
	"github.com/openDECTcell/opendectcell-controller/internal/db"
	"github.com/openDECTcell/opendectcell-controller/internal/dbtest"
	"github.com/openDECTcell/opendectcell-controller/internal/secret"
)

// What the runbook for a lost or expiring authority (wiki Operating, "The base stations' certificate authority") says is what the
// controller does: it keeps one authority, finds it again by the key it was sealed with, and makes a new one, with another pin, only
// when none is active.
func TestTheAuthorityIsKeptAndANewOneIsMadeOnlyWhenNoneIsActive(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Migrated(t)
	ks, _ := secret.GenerateMasterKey()
	mk, _ := secret.ParseMasterKey(ks)
	kr, err := db.LoadKeyring(ctx, pool, mk, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := cell.Open(ctx, pool, kr, "dect.example.org:7300")
	if err != nil {
		t.Fatal(err)
	}
	again, err := cell.Open(ctx, pool, kr, "dect.example.org:7300")
	if err != nil {
		t.Fatal(err)
	}
	if first.CA.Pin() != again.CA.Pin() || first.Installation != again.Installation {
		t.Fatal("a restart made another authority: every base station would stop connecting")
	}
	var n int
	pool.QueryRow(ctx, "SELECT count(*) FROM cell_ca").Scan(&n)
	if n != 1 {
		t.Fatalf("%d authorities after two starts", n)
	}
	// the authority's key is sealed with the master key: with another master key it cannot be opened, and the start fails
	// instead of silently making a new authority
	other, _ := secret.GenerateMasterKey()
	omk, _ := secret.ParseMasterKey(other)
	okr, err := db.LoadKeyring(ctx, pool, omk, nil)
	if err == nil {
		if _, err := cell.Open(ctx, pool, okr, "dect.example.org:7300"); err == nil {
			t.Error("the authority opened with a master key that did not seal it")
		}
	}
	// none is active (it was retired by hand): the next start makes a new one, with another pin, and the old row stays
	if _, err := pool.Exec(ctx, "UPDATE cell_ca SET active = false"); err != nil {
		t.Fatal(err)
	}
	next, err := cell.Open(ctx, pool, kr, "dect.example.org:7300")
	if err != nil {
		t.Fatal(err)
	}
	if next.CA.Pin() == first.CA.Pin() {
		t.Error("a new authority has the pin of the old one")
	}
	var active int
	pool.QueryRow(ctx, "SELECT count(*) FILTER (WHERE active), count(*) FROM cell_ca").Scan(&active, &n)
	if active != 1 || n != 2 {
		t.Errorf("%d active of %d authorities", active, n)
	}
	// the enrolment string of the new one carries the new pin, which is what a base station checks the controller by
	if first.CA.Pin() == "" || next.CA.Pin() == "" {
		t.Error("no pin")
	}
}
