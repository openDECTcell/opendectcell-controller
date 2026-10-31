// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build libopendectcell

package core

import (
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/cell"
	"github.com/openDECTcell/opendectcell-controller/internal/dect/cdect"
)

// The core over the real stack: a cell comes up, its radio is brought up,
// and its one handset's record goes into the chip, the slot writes with a
// corr (§7.4). The mails are register.trace's start, except that on 08.25
// the read-backs wait 50 ms (the maintainers' notes), so they follow the mask.
func TestCellUpWithLibopendectcell(t *testing.T) {
	h := handset(1, owner)
	for k := range h.Rec.UAK {
		h.Rec.UAK[k] = byte(0xB0 + k)
	}
	g := newRig(t, &fakeRecords{hs: []Handset{h}}, nil)
	g.core.New = cdect.NewStack
	g.core.CellUp(uuid.New(), cell.CellUp{StubVersion: "08.25", CapSubRecords: 6})
	waitFor(t, "the read-backs, held back 50 ms on 08.25", func() bool {
		n := 0
		for _, s := range g.link.all() {
			if s.m.Prim == 0xFFF1 {
				n++
			}
		}
		return n == 7
	})
	var got []string
	for _, s := range g.link.all() {
		line := fmt.Sprintf("%04x %x", s.m.Prim, s.m.Param)
		if (s.m.Prim == 0x0247 || s.m.Prim == 0x020A) != (s.corr != 0) {
			t.Errorf("%s: corr %d", line, s.corr)
		}
		got = append(got, line)
	}
	want := []string{
		"0153 00", "0169 07", "0157 00", "015b 01", "022c 01",
		"0247 020000", "0247 030000", "0247 040000", "0247 050000", "0247 060000",
		"0247 010100", "020a 01b0b1b2b3b4b5b6b7b8b9babbbcbdbebf00", "017c 0001",
		"fff1 07189500001100", "fff1 07299500001100", "fff1 073a9500001100",
		"fff1 074b9500001100", "fff1 075c9500001100", "fff1 076d9500001100",
		"fff1 07189500001100",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("mails\n got  %v\n want %v", got, want)
	}
}
