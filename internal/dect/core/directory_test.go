// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"testing"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/cell"
	"github.com/openDECTcell/opendectcell-controller/internal/dect/stack"
)

func dirOf(g *rig, i int) []stack.DirEntry {
	var out []stack.DirEntry
	if st := g.stack(i); st != nil {
		st.get(func(f *fakeStack) {
			if n := len(f.dirSets); n > 0 {
				out = f.dirSets[n-1]
			}
		})
	}
	return out
}

// A handset sees its account's handsets on other cells in its Internal Names List: the stack gets them
// as the remote directory, each with a number, and the number belongs to the owner for NameVisible.
func TestDirectoryHoldsTheAccountsHandsetsOnOtherCells(t *testing.T) {
	old := dirEvery
	dirEvery = 0
	t.Cleanup(func() { dirEvery = old })
	h := handset(1, owner)
	away := DirHandset{ID: uuid.New(), Owner: owner, Name: "AS690H"}
	other := DirHandset{ID: uuid.New(), Owner: uuid.New(), Name: "Stranger"}
	recs := &fakeRecords{hs: []Handset{h}, dirHs: []DirHandset{away, other}}
	g := newRig(t, recs, nil)
	g.core.CellUp(uuid.New(), cell.CellUp{StubVersion: "08.25", CapSubRecords: 6})
	waitFor(t, "the directory", func() bool { return len(dirOf(g, 0)) == 1 })
	e := dirOf(g, 0)[0]
	if e.Name != "AS690H" || e.ID != stack.RemoteFirst {
		t.Fatalf("entry %+v", e)
	}
	env := g.stack(0).env
	if !env.NameVisible(1, e.ID) {
		t.Error("the owner's own handset on another cell is not visible to it")
	}
	if env.NameVisible(1, e.ID+1) {
		t.Error("a number nobody holds is visible")
	}

	// the handset moves in here: it is no longer remote; the other one stays out
	recs.mu.Lock()
	recs.dirHs = []DirHandset{other}
	recs.mu.Unlock()
	g.core.DirectoryChanged(uuid.Nil)
	waitFor(t, "the directory emptied", func() bool {
		var n = -1
		g.stack(0).get(func(f *fakeStack) { n = len(f.dirSets) })
		return n >= 2 && len(dirOf(g, 0)) == 0
	})
	if env.NameVisible(1, e.ID) {
		t.Error("a number that left the directory is still visible")
	}
}

// A handset that stays in the directory keeps its number; a new one takes the lowest free.
func TestDirectoryNumbersAreStable(t *testing.T) {
	old := dirEvery
	dirEvery = 0
	t.Cleanup(func() { dirEvery = old })
	h := handset(1, owner)
	a := DirHandset{ID: uuid.New(), Owner: owner, Name: "B"}
	b := DirHandset{ID: uuid.New(), Owner: owner, Name: "C"}
	recs := &fakeRecords{hs: []Handset{h}, dirHs: []DirHandset{a, b}}
	g := newRig(t, recs, nil)
	g.core.CellUp(uuid.New(), cell.CellUp{StubVersion: "08.25", CapSubRecords: 6})
	waitFor(t, "two entries", func() bool { return len(dirOf(g, 0)) == 2 })
	first := dirOf(g, 0)
	c := DirHandset{ID: uuid.New(), Owner: owner, Name: "A"} // sorts first, takes the next number
	recs.mu.Lock()
	recs.dirHs = []DirHandset{a, b, c}
	recs.mu.Unlock()
	g.core.DirectoryChanged(uuid.Nil)
	waitFor(t, "three entries", func() bool { return len(dirOf(g, 0)) == 3 })
	got := map[string]uint{}
	for _, e := range dirOf(g, 0) {
		got[e.Name] = e.ID
	}
	if got["B"] != first[0].ID || got["C"] != first[1].ID || got["A"] != stack.RemoteFirst+2 {
		t.Errorf("numbers %v, before %v", got, first)
	}
}
