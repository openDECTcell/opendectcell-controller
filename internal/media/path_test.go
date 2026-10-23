// SPDX-License-Identifier: AGPL-3.0-or-later

package media

import (
	"errors"
	"testing"

	"github.com/openDECTcell/opendectcell-controller/internal/cell/wire"
	"github.com/openDECTcell/opendectcell-controller/internal/media/codec"
)

// The cell as measured on 2026-10-23: L16 8 kHz down, uplink unmeasured
// (the leading candidate is L16), no wideband.
var narrowCell = CellCaps{Down: []uint8{wire.FormatL16_8k}, Up: []uint8{wire.FormatL16_8k}}

// A cell that reports wideband, should it ever work.
var wideCell = CellCaps{Down: []uint8{wire.FormatG722, wire.FormatL16_8k}, Up: []uint8{wire.FormatG722, wire.FormatL16_8k}, WidebandEndpoints: 4}

func TestChoose(t *testing.T) {
	for _, c := range []struct {
		name     string
		caps     CellCaps
		wideband bool
		sip      *codec.Codec
		want     Plan
	}{
		// The case that matters today: a G.722 SIP call on a narrowband-only
		// cell is transcoded down, not refused.
		{"G.722 SIP, narrowband cell", narrowCell, true, codec.G722,
			Plan{SIP: codec.G722, CellDown: wire.FormatL16_8k, CellUp: wire.FormatL16_8k, Rate: 8000}},
		{"PCMA SIP, narrowband cell", narrowCell, false, codec.PCMA,
			Plan{SIP: codec.PCMA, CellDown: wire.FormatL16_8k, CellUp: wire.FormatL16_8k, Rate: 8000}},
		// The uplink measurement could come back A-law: the cell then lists
		// it in tag 16, and only the uplink changes.
		{"A-law uplink", CellCaps{Down: []uint8{wire.FormatL16_8k}, Up: []uint8{wire.FormatPCMA}}, false, codec.PCMA,
			Plan{SIP: codec.PCMA, CellDown: wire.FormatL16_8k, CellUp: wire.FormatPCMA, Rate: 8000, PassUp: true}},
		{"A-law uplink, G.722 SIP", CellCaps{Down: []uint8{wire.FormatL16_8k}, Up: []uint8{wire.FormatPCMA}}, false, codec.G722,
			Plan{SIP: codec.G722, CellDown: wire.FormatL16_8k, CellUp: wire.FormatPCMA, Rate: 8000}},
		// Wideband passthrough becomes an optimisation a capable cell
		// switches on.
		{"G.722 both ways", wideCell, true, codec.G722,
			Plan{SIP: codec.G722, CellDown: wire.FormatG722, CellUp: wire.FormatG722, Rate: 16000, PassDown: true, PassUp: true}},
		{"wideband cell, PCMU SIP", wideCell, true, codec.PCMU,
			Plan{SIP: codec.PCMU, CellDown: wire.FormatG722, CellUp: wire.FormatG722, Rate: 8000}},
		{"wideband cell, narrowband air", wideCell, false, codec.G722,
			Plan{SIP: codec.G722, CellDown: wire.FormatL16_8k, CellUp: wire.FormatL16_8k, Rate: 8000}},
		{"wideband formats but no endpoint", CellCaps{Down: wideCell.Down, Up: wideCell.Up}, true, codec.G722,
			Plan{SIP: codec.G722, CellDown: wire.FormatL16_8k, CellUp: wire.FormatL16_8k, Rate: 8000}},
	} {
		got, err := Choose(c.caps, c.wideband, c.sip)
		if err != nil || got != c.want {
			t.Errorf("%s: %v %v\n want %v", c.name, got, err, c.want)
		}
	}
}

func TestChooseNoFormat(t *testing.T) {
	for _, caps := range []CellCaps{
		{},
		{Down: []uint8{wire.FormatL16_8k}, Up: []uint8{wire.FormatG726_32}},
		{Down: []uint8{wire.FormatPCMA16k}, Up: []uint8{wire.FormatL16_8k}},
	} {
		if _, err := Choose(caps, false, codec.PCMA); !errors.Is(err, ErrNoFormat) {
			t.Errorf("%+v: %v", caps, err)
		}
	}
}

// No code path may assume wideband: a cell that does not report it is
// never treated as wideband, whatever the handset wants.
func TestWidebandIsReported(t *testing.T) {
	if narrowCell.Wideband() {
		t.Error("narrowband cell reports wideband")
	}
	if !wideCell.Wideband() {
		t.Error("wideband cell does not")
	}
	half := CellCaps{Down: []uint8{wire.FormatG722}, Up: []uint8{wire.FormatL16_8k}, WidebandEndpoints: 4}
	if half.Wideband() {
		t.Error("wideband in one direction only counts as wideband")
	}
}
