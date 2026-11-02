// SPDX-License-Identifier: AGPL-3.0-or-later

package contacts_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/contacts"
)

func TestFileText(t *testing.T) {
	cs := []*contacts.Contact{
		{ID: 7, LastName: "Meyer", FirstName: "Eva", Numbers: []contacts.Number{
			{Number: "0301234567", Kind: "fixed"}, {Number: "+491711234567", Kind: "mobile", Default: true},
			{Number: "111", Kind: "work"}, {Number: "222", Kind: "other"}}},
		{ID: 9, LastName: strings.Repeat("ä", 20), Numbers: []contacts.Number{{Number: "100", Kind: "work", Default: true}}},
		{ID: 1 << 40, LastName: "too big a key"},
	}
	got := contacts.FileText(cs, 5, map[int64]uint16{7: 12})
	want := "#opendectcell-contacts 1 seq=5\n" +
		"00000007\t12\tMeyer\tEva\t0\t*\t04:00491711234567\t08:0301234567\t02:111\n" + // default first, three numbers, + as 00
		"00000009\t-\t" + strings.Repeat("ä", 15) + "\t\t0\t*\t02:100\n" // cut at a character: 30 of 31 bytes
	if got != want {
		t.Errorf("got\n%q\nwant\n%q", got, want)
	}
}

func TestParseChange(t *testing.T) {
	ch, err := contacts.ParseChange("3\tput\t5\t00000000\tMeyer\tEva\t0\t*\t04:0171\t08:030")
	if err != nil || ch.Seq != 3 || ch.Op != "put" || ch.Entry != 5 || ch.Key != 0 || ch.Last != "Meyer" || ch.First != "Eva" ||
		len(ch.Nums) != 2 || ch.Nums[0].Kind != "mobile" || !ch.Nums[0].Default || ch.Nums[1].Kind != "fixed" || ch.Nums[1].Default {
		t.Errorf("%+v %v", ch, err)
	}
	if ch, err := contacts.ParseChange("4\tdel\t5\t0000001f"); err != nil || ch.Op != "del" || ch.Key != 0x1f {
		t.Errorf("%+v %v", ch, err)
	}
	if ch, err := contacts.ParseChange("5\tclear"); err != nil || ch.Op != "clear" {
		t.Errorf("%+v %v", ch, err)
	}
	for _, bad := range []string{"", "x\tput", "1\tbogus", "1\tput\t5\t0", "1\tdel\t99999\t0"} {
		if _, err := contacts.ParseChange(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// What a handset changes lands in its owner's personal list, and only there: a contact it made,
// one it edited (the numbers and the long name it could not see stay), one it deleted; the shared
// contacts and "delete list" are refused.
func TestApplyChange(t *testing.T) {
	s, admin, anna, _ := setup(t)
	h := handset(t, s.Pool, anna.ID, 1)
	var told []string
	s.OnChange = func(owner *uuid.UUID) {
		if owner == nil {
			told = append(told, "shared")
		} else {
			told = append(told, "own")
		}
	}

	// made on the handset
	got, err := s.ApplyChange(ctx, h, "1\tput\t3\t00000000\tMeyer\tEva\t0\t*\t04:0171")
	if err != nil || got.Contact == 0 || got.Entry != 3 || got.Seq != 1 {
		t.Fatalf("create: %+v %v", got, err)
	}
	list, _ := s.ForHandset(ctx, h)
	if len(list) != 1 || list[0].LastName != "Meyer" || list[0].Numbers[0].Kind != "mobile" {
		t.Fatalf("list %+v", list)
	}

	// a contact with five numbers and a long name, edited on the handset
	long := strings.Repeat("Long", 12)
	c, err := s.Create(ctx, anna, contacts.Contact{LastName: long, FirstName: "Ann", Numbers: []contacts.Number{
		{Number: "1", Kind: "fixed", Default: true}, {Number: "2", Kind: "fixed"}, {Number: "3", Kind: "fixed"},
		{Number: "4", Kind: "work"}, {Number: "5", Kind: "other"}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	line := "2\tput\t4\t" + hex8(c.ID) + "\t" + long[:31] + "\tAnnette\t0\t*\t08:1\t08:2\t08:33"
	if _, err := s.ApplyChange(ctx, h, line); err != nil {
		t.Fatal(err)
	}
	back, _ := s.Get(ctx, anna, c.ID)
	if back.LastName != long || back.FirstName != "Annette" || len(back.Numbers) != 5 || back.Numbers[2].Number != "33" || back.Numbers[3].Number != "4" {
		t.Errorf("after the edit: %+v", back)
	}

	// deleted on the handset
	if _, err := s.ApplyChange(ctx, h, "3\tdel\t4\t"+hex8(c.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, anna, c.ID); !errors.Is(err, contacts.ErrNotFound) {
		t.Errorf("the deleted contact is still there: %v", err)
	}

	// refused: the shared list is read-only from a handset, and it may not empty the list
	shared, _ := s.Create(ctx, admin, contacts.Contact{LastName: "Reception", Numbers: []contacts.Number{{Number: "100"}}}, true)
	if _, err := s.ApplyChange(ctx, h, "4\tput\t9\t"+hex8(shared.ID)+"\tHack\t\t0\t*\t08:1"); !errors.Is(err, contacts.ErrReadOnly) {
		t.Errorf("a handset changed a shared contact: %v", err)
	}
	if _, err := s.ApplyChange(ctx, h, "5\tdel\t9\t"+hex8(shared.ID)); !errors.Is(err, contacts.ErrReadOnly) {
		t.Errorf("a handset deleted a shared contact: %v", err)
	}
	if _, err := s.ApplyChange(ctx, h, "6\tclear"); !errors.Is(err, contacts.ErrNotApplied) {
		t.Errorf("delete list: %v", err)
	}
	if len(told) < 3 {
		t.Errorf("the DECT side was told %d times", len(told))
	}
}

func hex8(id int64) string { return fmt.Sprintf("%08x", id) }
