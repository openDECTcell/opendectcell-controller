// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/cell"
	"github.com/openDECTcell/opendectcell-controller/internal/dect/stack"
)

// In a system of many cells a call to a handset nobody answers for is not offered to
// every cell: the last-heard one alone, then waves of RingWaveSize, at most
// RingMaxCells in all, the rest never (docs/roaming.md §4).
func TestRingWavesAreCapped(t *testing.T) {
	const cells = 40
	h := handset(7, owner)
	recs := &fakeRecords{hs: []Handset{h}, lastOn: map[uuid.UUID]uuid.UUID{}}
	ids := make([]uuid.UUID, cells)
	for i := range ids {
		ids[i] = uuid.New()
	}
	recs.lastOn[h.ID] = ids[17]
	var mu sync.Mutex
	order := map[int]int{} // stack index -> the position its call-in came in
	n, seq := 0, 0
	rg := newRig(t, recs, func(f *fakeStack) {
		idx := n
		n++
		f.onCtl = func(env stack.Env, line string) {
			if !strings.HasPrefix(line, "call-in ") {
				return
			}
			mu.Lock()
			if _, ok := order[idx]; !ok {
				order[idx] = seq
				seq++
			}
			mu.Unlock()
		}
	})
	rg.core.PageDelay = 30 * time.Millisecond
	rg.core.RingWaveSize, rg.core.RingMaxCells = 4, 10
	for i, id := range ids {
		rg.core.CellUp(id, cell.CellUp{StubVersion: "08.25", CapSubRecords: 6})
		waitFor(t, "a cell's radio on", radioOnAt(rg, i))
	}
	if _, err := rg.core.Ring(context.Background(), h.ID, Call{SIP: "w1", Line: 1, Number: "1"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "every wave offered", func() bool { mu.Lock(); defer mu.Unlock(); return len(order) == 10 })
	time.Sleep(150 * time.Millisecond) // and nothing more
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 10 {
		t.Fatalf("the call was offered to %d cells, want 10", len(order))
	}
	byPos := make([]int, 10)
	for idx, pos := range order {
		byPos[pos] = idx
	}
	if rg.stack(byPos[0]) == nil {
		t.Fatal("no stack")
	}
	// the first call-in is the last-heard cell's: its stack is the 18th brought up
	if byPos[0] != 17 {
		t.Errorf("the first call-in went to stack %d, want the last-heard cell's (17)", byPos[0])
	}
	// the others come in waves of four, then one: no position is repeated
	if got := slices.Clone(byPos); len(slices.Compact(slices.Sorted(slices.Values(got)))) != 10 {
		t.Errorf("a cell was offered the call twice: %v", byPos)
	}
}
