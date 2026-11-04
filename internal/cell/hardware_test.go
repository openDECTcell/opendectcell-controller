// SPDX-License-Identifier: AGPL-3.0-or-later

package cell_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/cell"
	"github.com/openDECTcell/opendectcell-controller/internal/cell/celltest"
	"github.com/openDECTcell/opendectcell-controller/internal/cell/wire"
)

func TestModelNameAndAutoName(t *testing.T) {
	for in, want := range map[string]string{"AVM FRITZ!Box 7520": "FRITZ!Box 7520", "avm FRITZ!Box 7412": "FRITZ!Box 7412", "FRITZ!Box 7362 SL": "FRITZ!Box 7362 SL", "AVM": "AVM", "": ""} {
		if got := cell.ModelName(in); got != want {
			t.Errorf("ModelName(%q) = %q, want %q", in, got, want)
		}
	}
	for name, want := range map[string]bool{"OpenWrt": true, "openwrt": true, "OpenWrt-2": true, " OpenWrt-17 ": true, "OpenWrt-": false, "OpenWrt lab": false, "Kitchen": false, "openDECTcell": false, "": false} {
		if got := cell.GenericName(name); got != want {
			t.Errorf("GenericName(%q) = %v, want %v", name, got, want)
		}
	}
	if got := cell.AutoName("FRITZ!Box 7520", []byte{0x10, 0xb5, 0xc7, 0x2a, 0x00}); got != "FRITZ!Box 7520 72a00" {
		t.Errorf("AutoName = %q", got)
	}
	if cell.AutoName("", []byte{1, 2, 3, 4, 5}) != "" || cell.AutoName("FRITZ!Box", []byte{1, 2}) != "" {
		t.Error("a name from a missing model or identity")
	}
}

func hello(model string, rfpi ...byte) wire.HelloMsg {
	h := wire.HelloMsg{HWModel: model, SWVersion: "openDECTcell 0.3.4"}
	if len(rfpi) == 5 {
		copy(h.RFPI[:], rfpi)
		h.HasRFPI = true
	}
	return h
}

// enrolNamed enrols a cell whose own name is hint, as a box does with its hostname.
func (e *env) enrolNamed(t *testing.T, hint string) *celltest.Cell {
	t.Helper()
	_, es, err := e.token(t, e.admin, "", time.Hour, true)
	if err != nil {
		t.Fatal(err)
	}
	c, status, err := celltest.Enroll(e.ctx, es, hint)
	if err != nil || status != wire.EnrollOK {
		t.Fatalf("enroll: %d %v", status, err)
	}
	return c
}

func (e *env) nameOf(t *testing.T, c *celltest.Cell) string {
	t.Helper()
	got, err := e.store.GetCell(e.ctx, e.admin, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got.Name
}

// A base station that keeps the OpenWrt name is named by its model and the end of its identity once both are
// known; one the owner named is not; and the model is kept.
func TestDefaultNameBecomesModelAndIdentity(t *testing.T) {
	e := newEnv(t)
	c := e.enrolNamed(t, "OpenWrt")
	if got := e.nameOf(t, c); got != "OpenWrt" {
		t.Fatalf("enrolled as %q", got)
	}
	// no identity yet: nothing to make a name of, but the model is kept
	if err := e.store.RecordHello(e.ctx, c.ID, hello("AVM FRITZ!Box 7520")); err != nil {
		t.Fatal(err)
	}
	got, _ := e.store.GetCell(e.ctx, e.admin, c.ID)
	if got.Name != "OpenWrt" || got.Detected() != "FRITZ!Box 7520" || got.Model() != "FRITZ!Box 7520" {
		t.Fatalf("model only: %q detected %q", got.Name, got.Detected())
	}
	if err := e.store.RecordHello(e.ctx, c.ID, hello("AVM FRITZ!Box 7520", 0x10, 0xb5, 0xc7, 0x2a, 0x00)); err != nil {
		t.Fatal(err)
	}
	if got := e.nameOf(t, c); got != "FRITZ!Box 7520 72a00" {
		t.Fatalf("named %q", got)
	}
	// once: a later hello changes nothing, and the rename is in the audit log
	if err := e.store.RecordHello(e.ctx, c.ID, hello("AVM FRITZ!Box 7520", 0x10, 0xb5, 0xc7, 0x2b, 0x00)); err != nil {
		t.Fatal(err)
	}
	if got := e.nameOf(t, c); got != "FRITZ!Box 7520 72a00" {
		t.Errorf("renamed again: %q", got)
	}
	var n int
	e.pool.QueryRow(e.ctx, "SELECT count(*) FROM audit_log WHERE action = 'cell.rename_auto' AND object_id = $1 AND details->>'from' = 'OpenWrt'", c.ID.String()).Scan(&n)
	if n != 1 {
		t.Errorf("%d audit entries for the rename", n)
	}
	// the owner's model beats the reported one in names and on the pages
	if _, err := e.pool.Exec(e.ctx, "UPDATE cell SET box_model = 'FRITZ!Box 7520 B' WHERE id = $1", c.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.store.GetCell(e.ctx, e.admin, c.ID); got.Model() != "FRITZ!Box 7520 B" || got.Detected() != "FRITZ!Box 7520" {
		t.Errorf("model %q, detected %q", got.Model(), got.Detected())
	}
}

func TestChosenNamesAreKept(t *testing.T) {
	e := newEnv(t)
	named := e.enrolNamed(t, "Kitchen")
	e.store.RecordHello(e.ctx, named.ID, hello("AVM FRITZ!Box 7412", 1, 2, 3, 4, 5))
	if got := e.nameOf(t, named); got != "Kitchen" {
		t.Errorf("a chosen hostname became %q", got)
	}

	// a hostname somebody gave the box, even one that looks like the default's numbered form, is a choice
	typed := e.enrolNamed(t, "OpenWrt-7")
	e.store.RecordHello(e.ctx, typed.ID, hello("AVM FRITZ!Box 7412", 5, 5, 5, 5, 5))
	if got := e.nameOf(t, typed); got != "OpenWrt-7" {
		t.Errorf("a hostname of the box's owner became %q", got)
	}

	// the owner types a name that looks like the default one: it is theirs
	same := e.enrolNamed(t, "OpenWrt")
	if _, err := e.store.UpdateProfile(e.ctx, e.admin, same.ID, cell.ProfileUpdate{Name: "OpenWrt-2", Location: "attic", PositionPrecision: "exact"}); err != nil {
		t.Fatal(err)
	}
	e.store.RecordHello(e.ctx, same.ID, hello("AVM FRITZ!Box 7520", 9, 9, 9, 9, 9))
	if got := e.nameOf(t, same); got != "OpenWrt-2" {
		t.Errorf("a name the owner typed, though a default one, became %q", got)
	}

	// a name saved unchanged (the page posts it with every save) does not count as the owner's
	keep := e.enrolNamed(t, "OpenWrt")
	cur := e.nameOf(t, keep)
	if _, err := e.store.UpdateProfile(e.ctx, e.admin, keep.ID, cell.ProfileUpdate{Name: cur, Location: "barn", PositionPrecision: "exact"}); err != nil {
		t.Fatal(err)
	}
	e.store.RecordHello(e.ctx, keep.ID, hello("AVM FRITZ!Box 7520", 0xab, 0xcd, 0xef, 0x01, 0x23))
	if got := e.nameOf(t, keep); got != "FRITZ!Box 7520 f0123" {
		t.Errorf("a default name saved unchanged was kept: %q", got)
	}
}

// Two boxes of one model with the same last five digits still get two names.
func TestAutoNamesAreUnique(t *testing.T) {
	e := newEnv(t)
	a, b := e.enrolNamed(t, "OpenWrt"), e.enrolNamed(t, "OpenWrt")
	e.store.RecordHello(e.ctx, a.ID, hello("AVM FRITZ!Box 7520", 1, 2, 3, 4, 5))
	e.store.RecordHello(e.ctx, b.ID, hello("AVM FRITZ!Box 7520", 9, 2, 3, 4, 5))
	na, nb := e.nameOf(t, a), e.nameOf(t, b)
	if na != "FRITZ!Box 7520 30405" || nb != "FRITZ!Box 7520 30405-2" {
		t.Errorf("names %q and %q", na, nb)
	}
}

// The migration fills in the model of cells that enrolled before it from their enrolment record. The test runs
// that statement itself.
func TestMigrationBackfillsTheModel(t *testing.T) {
	e := newEnv(t)
	c := e.enrolNamed(t, "Barn")
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO audit_log (actor_name, action, object_type, object_id, details) VALUES ('system', 'cell.enroll', 'cell', $1, '{"hw_model": "AVM FRITZ!Box 7362 SL"}')`, c.ID.String()); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile("../db/migrations/0027_cell_hardware.sql")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	i, j := strings.Index(text, "UPDATE cell c SET hw_model"), strings.Index(text, "-- +down")
	if i < 0 || j < i {
		t.Fatal("the backfill statement is not in the migration")
	}
	if _, err := e.pool.Exec(e.ctx, "UPDATE cell SET hw_model = ''"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(e.ctx, text[i:j]); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	got, _ := e.store.GetCell(e.ctx, e.admin, c.ID)
	if got.Detected() != "FRITZ!Box 7362 SL" {
		t.Errorf("model after the backfill: %q", got.Detected())
	}
}
