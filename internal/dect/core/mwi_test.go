// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/cell"
)

func ctlLines(g *rig, i int) []string {
	var out []string
	g.stack(i).get(func(f *fakeStack) {
		for _, c := range f.ctls {
			out = append(out, c.line)
		}
	})
	return out
}

// A mailbox that changes is told to the cell the handset was last heard on, with the
// handset's record number there and its line.
func TestSendMWIGoesToTheLastHeardCell(t *testing.T) {
	rr := newRouteRig(t, "b")
	rr.core.SendMWI(rr.h.ID, 2, 3)
	waitFor(t, "the mwi command on b", func() bool {
		return slices.Contains(ctlLines(rr.rig, 1), "mwi hs=7 line=2 count=3")
	})
	if slices.Contains(ctlLines(rr.rig, 0), "mwi hs=7 line=2 count=3") {
		t.Error("the other cell was told as well")
	}
	// a handset that is on no cell is told when it comes to one: nobody is asked now
	rr.core.SendMWI(uuid.New(), 1, 1)
}

// A stack that starts with a handset in its table tells it what its mailboxes hold once
// the radio is on; a handset that comes into the table later is told when it does.
func TestMWIReplayedOnStartAndArrival(t *testing.T) {
	h1, h2 := handset(7, owner), handset(8, owner)
	h2.Rec.PortID = []byte{0x80, 0x08}
	recs := &fakeRecords{hs: []Handset{h1}, place: map[uuid.UUID]Handset{h2.ID: h2}}
	g := newRig(t, recs, nil)
	g.core.MWIFor = func(_ context.Context, hid uuid.UUID) []MWILine {
		switch hid {
		case h1.ID:
			return []MWILine{{Line: 1, Count: 2}}
		case h2.ID:
			return []MWILine{{Line: 1, Count: 0}, {Line: 2, Count: 5}}
		}
		return nil
	}
	g.core.CellUp(uuid.New(), cell.CellUp{StubVersion: "08.25", CapSubRecords: 6})
	waitFor(t, "radio on", radioOn(g))
	waitFor(t, "the first handset told", func() bool {
		return slices.Contains(ctlLines(g, 0), "mwi hs=7 line=1 count=2")
	})
	g.stack(0).env.Fetch(h2.Rec.PortID)
	waitFor(t, "the second handset told on arrival", func() bool {
		l := ctlLines(g, 0)
		return slices.Contains(l, "mwi hs=8 line=1 count=0") && slices.Contains(l, "mwi hs=8 line=2 count=5")
	})
}
