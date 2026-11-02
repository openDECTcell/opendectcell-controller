// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/cell"
	"github.com/openDECTcell/opendectcell-controller/internal/dect/stack"
)

// pumped is a stack as a worker process makes it: its callbacks wait for the cell's goroutine,
// it says when its worker has ended.
type pumped struct {
	*fakeStack
	dead  atomic.Bool
	pumps atomic.Int32
	wake  atomic.Pointer[func()]
}

func (p *pumped) SetWake(f func()) { p.wake.Store(&f) }
func (p *pumped) Pump()            { p.pumps.Add(1) }
func (p *pumped) Alive() bool      { return !p.dead.Load() }

var _ stack.Pumper = (*pumped)(nil)

func newPumpRig(t *testing.T) (*rig, func(i int) *pumped) {
	t.Helper()
	h := handset(7, owner)
	recs := &fakeRecords{hs: []Handset{h}}
	g := newRig(t, recs, nil)
	var all []*pumped
	inner := g.core.New
	g.core.New = func(env stack.Env, cfg stack.Config) stack.Stack {
		f := inner(env, cfg).(*fakeStack)
		p := &pumped{fakeStack: f}
		g.mu.Lock()
		all = append(all, p)
		g.mu.Unlock()
		return p
	}
	return g, func(i int) *pumped {
		g.mu.Lock()
		defer g.mu.Unlock()
		if i < len(all) {
			return all[i]
		}
		return nil
	}
}

// A stack in a worker wakes the cell, which runs the stack's waiting callbacks on its own
// goroutine; many wakes before the cell gets to it are one event.
func TestWakeRunsThePump(t *testing.T) {
	g, st := newPumpRig(t)
	g.core.CellUp(uuid.New(), cell.CellUp{StubVersion: "08.25", CapSubRecords: 6})
	waitFor(t, "the stack", func() bool { return st(0) != nil && st(0).wake.Load() != nil })
	p := st(0)
	before := p.pumps.Load()
	for range 50 {
		(*p.wake.Load())() // from another goroutine than the cell's
	}
	waitFor(t, "the pump", func() bool { return p.pumps.Load() > before })
}

// The worker of a stack that ends takes its calls with it: the core drops the stack and makes a
// new one, in another worker.
func TestDeadWorkerRestartsTheStack(t *testing.T) {
	g, st := newPumpRig(t)
	g.core.CellUp(uuid.New(), cell.CellUp{StubVersion: "08.25", CapSubRecords: 6})
	waitFor(t, "radio on", radioOn(g))
	st(0).dead.Store(true)
	waitFor(t, "a second stack", func() bool { return st(1) != nil })
	waitFor(t, "the first one closed", func() bool {
		var c bool
		st(0).get(func(f *fakeStack) { c = f.closed })
		return c
	})
	waitFor(t, "radio on again", radioOnAt(g, 1))
}

// No stack could be made (no worker is up): the core asks again after the retry delay.
func TestNoStackTriesAgain(t *testing.T) {
	h := handset(7, owner)
	g := newRig(t, &fakeRecords{hs: []Handset{h}}, nil)
	var asked atomic.Int32
	inner := g.core.New
	g.core.New = func(env stack.Env, cfg stack.Config) stack.Stack {
		if asked.Add(1) <= 2 {
			return nil
		}
		return inner(env, cfg)
	}
	g.core.CellUp(uuid.New(), cell.CellUp{StubVersion: "08.25", CapSubRecords: 6})
	waitFor(t, "a stack at the third try", radioOn(g))
	if asked.Load() < 3 {
		t.Errorf("asked %d times", asked.Load())
	}
}
