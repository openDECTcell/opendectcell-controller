// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/cell"
	"github.com/openDECTcell/opendectcell-controller/internal/cell/wire"
	"github.com/openDECTcell/opendectcell-controller/internal/dect/stack"
)

// fakeStack records what the core does to it. It runs on the cell's
// goroutine; the test reads it under mu.
type fakeStack struct {
	mu      sync.Mutex
	env     stack.Env
	cfg     stack.Config
	records map[int]stack.Record
	free    []int
	started bool
	radio   bool
	closed  bool
	mails   []stack.Mail
	ticks   int
	// onMail runs on the cell's goroutine for each mail, with the env.
	onMail func(env stack.Env, m stack.Mail)
	onTick func(env stack.Env)
}

func (f *fakeStack) AddRecord(i int, r stack.Record) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records[i] = r
}

func (f *fakeStack) AddFreeStore(i int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.free = append(f.free, i)
}

func (f *fakeStack) Start() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = true
	return nil
}

func (f *fakeStack) RadioUp(up bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.radio = up
}

func (f *fakeStack) Mail(m stack.Mail, now uint64) {
	f.mu.Lock()
	f.mails = append(f.mails, m)
	cb := f.onMail
	f.mu.Unlock()
	if cb != nil {
		cb(f.env, m)
	}
}

func (f *fakeStack) Tick(now uint64) {
	f.mu.Lock()
	f.ticks++
	cb, radio := f.onTick, f.radio
	f.mu.Unlock()
	if cb != nil && radio {
		cb(f.env)
	}
}

func (f *fakeStack) Hello()                        {}
func (f *fakeStack) Ctl(string, uint64, bool) bool { return false }
func (f *fakeStack) Close()                        { f.mu.Lock(); f.closed = true; f.mu.Unlock() }
func (f *fakeStack) get(fn func(f *fakeStack))     { f.mu.Lock(); defer f.mu.Unlock(); fn(f) }

type fakeLink struct {
	mu    sync.Mutex
	mails []sent
}

type sent struct {
	corr uint16
	m    wire.MailDownMsg
}

func (l *fakeLink) SendMail(_ uuid.UUID, corr uint16, m wire.MailDownMsg) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.mails = append(l.mails, sent{corr, m})
	return nil
}

func (l *fakeLink) all() []sent {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.mails)
}

type fakeRecords struct {
	mu        sync.Mutex
	hs        []Handset
	fail      int // CellRecords fails this many times
	saves     []uuid.UUID
	newID     uuid.UUID
	installed [][]uint
	unknown   []uint
	forgot    []uuid.UUID
}

func (r *fakeRecords) CellRecords(context.Context, uuid.UUID, int) ([]Handset, []uuid.UUID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail > 0 {
		r.fail--
		return nil, nil, errors.New("database down")
	}
	return slices.Clone(r.hs), nil, nil
}

func (r *fakeRecords) SaveRecord(_ context.Context, _, id uuid.UUID, _ stack.Record) (uuid.UUID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.saves = append(r.saves, id)
	if id == uuid.Nil {
		return r.newID, nil
	}
	return id, nil
}

func (r *fakeRecords) Forget(_ context.Context, _, id uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forgot = append(r.forgot, id)
	return nil
}

func (r *fakeRecords) Rename(context.Context, uuid.UUID, string) error { return nil }

func (r *fakeRecords) Installed(_ context.Context, _ uuid.UUID, slots []uint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.installed = append(r.installed, slots)
	return nil
}

func (r *fakeRecords) SlotUnknown(_ context.Context, _ uuid.UUID, slot uint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.unknown = append(r.unknown, slot)
	return nil
}

func (r *fakeRecords) get(fn func(r *fakeRecords)) { r.mu.Lock(); defer r.mu.Unlock(); fn(r) }

type rig struct {
	core    *Core
	link    *fakeLink
	recs    *fakeRecords
	mu      sync.Mutex
	stacks  []*fakeStack
	prepare func(*fakeStack)
	cancel  context.CancelFunc
	done    chan struct{}
}

func newRig(t *testing.T, recs *fakeRecords, prepare func(*fakeStack)) *rig {
	t.Helper()
	g := &rig{link: &fakeLink{}, recs: recs, prepare: prepare, done: make(chan struct{})}
	g.core = &Core{Link: g.link, Records: recs, Log: slog.New(slog.DiscardHandler),
		Tick: time.Millisecond, BringUp: time.Millisecond,
		Retry: 5 * time.Millisecond,
		New: func(env stack.Env, cfg stack.Config) stack.Stack {
			f := &fakeStack{env: env, cfg: cfg, records: map[int]stack.Record{}}
			if g.prepare != nil {
				g.prepare(f)
			}
			g.mu.Lock()
			g.stacks = append(g.stacks, f)
			g.mu.Unlock()
			return f
		}}
	ctx, cancel := context.WithCancel(context.Background())
	g.cancel = cancel
	g.core.Start(ctx)
	go func() { g.core.Wait(); close(g.done) }()
	t.Cleanup(func() { cancel(); <-g.done })
	return g
}

func (g *rig) stack(i int) *fakeStack {
	g.mu.Lock()
	defer g.mu.Unlock()
	if i < len(g.stacks) {
		return g.stacks[i]
	}
	return nil
}

func (g *rig) nStacks() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.stacks)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func handset(slot uint, owner *uuid.UUID) Handset {
	h := Handset{ID: uuid.New(), Owner: owner, Rec: stack.Record{Slot: slot, PortID: []byte{0x80, byte(slot)}}}
	h.Rec.UAK[0] = byte(slot)
	return h
}

func radioOn(g *rig) func() bool {
	return func() bool {
		f := g.stack(0)
		on := false
		if f != nil {
			f.get(func(f *fakeStack) { on = f.radio })
		}
		return on
	}
}

func TestBringUpAndRecords(t *testing.T) {
	recs := &fakeRecords{hs: []Handset{handset(2, nil), handset(5, nil)}}
	g := newRig(t, recs, nil)
	id := uuid.New()
	g.core.CellUp(id, cell.CellUp{StubVersion: "06.88", CapSubRecords: 6})
	waitFor(t, "radio on", radioOn(g))
	waitFor(t, "slots recorded", func() bool {
		n := 0
		recs.get(func(r *fakeRecords) { n = len(r.installed) })
		return n == 1
	})

	f := g.stack(0)
	f.get(func(f *fakeStack) {
		if !f.started || len(f.records) != 2 || f.records[0].Slot != 2 || f.records[1].Slot != 5 {
			t.Errorf("records: started %v, %+v", f.started, f.records)
		}
		if !slices.Equal(f.free, []int{2, 3, 4, 5}) {
			t.Errorf("free stores %v, want 2..5", f.free)
		}
		if !f.cfg.NamesRO || f.cfg.UAKTable != 0x067F || f.cfg.CheckDelayMs != 1000 || f.cfg.Stub != "06.88" {
			t.Errorf("config %+v", f.cfg)
		}
	})
	var got []string
	for _, s := range g.link.all() {
		got = append(got, fmt.Sprintf("%04x %x", s.m.Prim, s.m.Param))
	}
	want := []string{"015b 00", "0153 00", "0169 07", "0157 00", "015b 01", "022c 01"}
	if !slices.Equal(got, want) {
		t.Errorf("bring-up %v, want %v (06.88: radio off first)", got, want)
	}
	recs.get(func(r *fakeRecords) {
		if !slices.Equal(r.installed[0], []uint{2, 5}) {
			t.Errorf("installed %v", r.installed[0])
		}
	})
}

func TestChipConfigPerBuild(t *testing.T) {
	for _, c := range []struct {
		stub         string
		table, delay uint32
		first        uint16
	}{{"08.25", 0x9518, 50, 0x0153}, {"07.56", 0, 0, 0x0153}, {"06.88", 0x067F, 1000, 0x015B}} {
		table, delay := chipConfig(c.stub)
		if table != c.table || delay != c.delay {
			t.Errorf("%s: table %#x delay %d", c.stub, table, delay)
		}
		if p := bringUp(c.stub, false, false)[0].Prim; p != c.first {
			t.Errorf("%s: first bring-up mail %#04x", c.stub, p)
		}
	}
	if p := bringUp("08.25", true, false)[0]; p.Prim != 0x015B || p.Param[0] != 0 {
		t.Error("a cell whose radio is on is switched off before the configuration")
	}
	for _, stub := range []string{"06.88", "08.25"} {
		if p := bringUp(stub, true, true)[0].Prim; p != 0x0153 {
			t.Errorf("%s: a resumed session's radio is switched off (first mail %#04x)", stub, p)
		}
	}
}

// A resumed session the controller has no stack for (the records did not
// load before) gets one, without the radio switched off: it may carry calls.
func TestResumeWithoutStackKeepsTheRadio(t *testing.T) {
	g := newRig(t, &fakeRecords{fail: 1}, nil)
	id := uuid.New()
	g.core.CellUp(id, cell.CellUp{StubVersion: "06.88", RadioState: 1, Resumed: true})
	waitFor(t, "radio on", radioOn(g))
	if m := g.link.all()[0].m; m.Prim != 0x0153 {
		t.Errorf("first mail %04x %x, want the configuration, no radio off", m.Prim, m.Param)
	}
}

// A panic in one cell's stack drops that stack and rebuilds it; another
// cell carries on.
func TestPanicContainedPerCell(t *testing.T) {
	var mu sync.Mutex
	broken := 1 // the first stack built panics on its first mail
	g := newRig(t, &fakeRecords{}, func(f *fakeStack) {
		mu.Lock()
		defer mu.Unlock()
		if broken > 0 {
			broken--
			f.onMail = func(stack.Env, stack.Mail) { panic("stack broke") }
		}
	})
	bad, good := uuid.New(), uuid.New()
	g.core.CellUp(bad, cell.CellUp{StubVersion: "08.25"})
	waitFor(t, "the bad cell's stack", func() bool { return g.nStacks() == 1 })
	g.core.CellUp(good, cell.CellUp{StubVersion: "08.25"})
	waitFor(t, "the good cell's stack", func() bool { return g.nStacks() == 2 })

	g.core.MailUp(bad, wire.MailUpMsg{Prim: 0x0200})
	waitFor(t, "the bad stack closed", func() bool {
		c := false
		g.stack(0).get(func(f *fakeStack) { c = f.closed })
		return c
	})
	waitFor(t, "the bad cell rebuilt", func() bool { return g.nStacks() == 3 })

	g.core.MailUp(good, wire.MailUpMsg{Prim: 0x0201})
	waitFor(t, "the good cell's mail", func() bool {
		n := 0
		g.stack(1).get(func(f *fakeStack) { n = len(f.mails) })
		return n == 1
	})
	if c := false; func() bool { g.stack(1).get(func(f *fakeStack) { c = f.closed }); return c }() {
		t.Error("the good cell's stack was closed")
	}
}

func TestResumeKeepsTheStack(t *testing.T) {
	g := newRig(t, &fakeRecords{hs: []Handset{handset(1, nil)}}, nil)
	id := uuid.New()
	g.core.CellUp(id, cell.CellUp{StubVersion: "08.25"})
	waitFor(t, "radio on", radioOn(g))

	g.core.MailUp(id, wire.MailUpMsg{Prim: 0x0200, Param: []byte{1}})
	waitFor(t, "mail at the stack", func() bool {
		n := 0
		g.stack(0).get(func(f *fakeStack) { n = len(f.mails) })
		return n == 1
	})

	g.core.CellDown(id)
	g.core.CellUp(id, cell.CellUp{StubVersion: "08.25", Resumed: true})
	g.core.MailUp(id, wire.MailUpMsg{Prim: 0x0201, Param: []byte{1}})
	waitFor(t, "second mail", func() bool {
		n := 0
		g.stack(0).get(func(f *fakeStack) { n = len(f.mails) })
		return n == 2
	})
	if g.nStacks() != 1 {
		t.Fatalf("a resumed session built %d stacks", g.nStacks())
	}

	g.core.CellUp(id, cell.CellUp{StubVersion: "08.25"}) // not resumed: a new chip state
	waitFor(t, "a fresh stack", func() bool { return g.nStacks() == 2 })
	waitFor(t, "the old one closed", func() bool {
		c := false
		g.stack(0).get(func(f *fakeStack) { c = f.closed })
		return c
	})

	g.core.CellGone(id)
	waitFor(t, "closed when gone", func() bool {
		c := false
		g.stack(1).get(func(f *fakeStack) { c = f.closed })
		return c
	})
}

func TestEnrolmentSaves(t *testing.T) {
	newID := uuid.New()
	recs := &fakeRecords{newID: newID}
	g := newRig(t, recs, func(f *fakeStack) {
		f.onMail = func(env stack.Env, m stack.Mail) {
			env.Save(0, stack.Record{Slot: 1, PortID: []byte{0x80, 0x01}})
		}
	})
	id := uuid.New()
	g.core.CellUp(id, cell.CellUp{StubVersion: "08.25"})
	waitFor(t, "radio on", radioOn(g))
	g.core.MailUp(id, wire.MailUpMsg{Prim: 0x0203})
	g.core.MailUp(id, wire.MailUpMsg{Prim: 0x0203})
	waitFor(t, "two saves", func() bool {
		n := 0
		recs.get(func(r *fakeRecords) { n = len(r.saves) })
		return n == 2
	})
	recs.get(func(r *fakeRecords) {
		if r.saves[0] != uuid.Nil || r.saves[1] != newID {
			t.Errorf("saves %v: the first creates the handset, the second updates it", r.saves)
		}
	})
}

func TestNameVisible(t *testing.T) {
	anna, ben := uuid.New(), uuid.New()
	g := &gen{owner: map[uint]*uuid.UUID{1: &anna, 2: &anna, 3: &ben, 4: nil, 5: nil}}
	for _, c := range []struct {
		viewer, slot uint
		want         bool
	}{{1, 2, true}, {1, 3, false}, {3, 1, false}, {4, 5, true}, {4, 1, false}, {1, 4, false}, {1, 6, false}, {6, 6, true}} {
		if got := g.NameVisible(c.viewer, c.slot); got != c.want {
			t.Errorf("NameVisible(%d, %d) = %v", c.viewer, c.slot, got)
		}
	}
}

func TestSlotWriteRejected(t *testing.T) {
	recs := &fakeRecords{hs: []Handset{handset(3, nil)}}
	g := newRig(t, recs, func(f *fakeStack) {
		once := false
		f.onTick = func(env stack.Env) {
			if !once {
				once = true
				_ = env.Send(stack.Mail{Src: 0x00, Dst: 0x02, Prim: 0x0247, Param: []byte{3, 1, 0}})
			}
		}
	})
	id := uuid.New()
	g.core.CellUp(id, cell.CellUp{StubVersion: "08.25"})
	var corr uint16
	waitFor(t, "the slot write", func() bool {
		for _, s := range g.link.all() {
			if s.m.Prim == 0x0247 {
				corr = s.corr
				return true
			}
		}
		return false
	})
	if corr == 0 {
		t.Fatal("a slot write went out without a corr (cell-protocol.md §7.4)")
	}
	g.core.MailReject(id, wire.MailRejectMsg{RefCorr: corr, Reason: wire.RejectQueueFull})
	waitFor(t, "slot unknown", func() bool {
		var u []uint
		recs.get(func(r *fakeRecords) { u = slices.Clone(r.unknown) })
		return slices.Equal(u, []uint{3})
	})
}

func TestRecordsRetried(t *testing.T) {
	recs := &fakeRecords{fail: 2}
	g := newRig(t, recs, nil)
	g.core.CellUp(uuid.New(), cell.CellUp{StubVersion: "08.25"})
	waitFor(t, "a stack after two failed loads", func() bool { return g.nStacks() == 1 })
}

func TestNoLibopendectcell(t *testing.T) {
	c := &Core{Link: &fakeLink{}, Records: &fakeRecords{}, Log: slog.New(slog.DiscardHandler)}
	ctx, cancel := context.WithCancel(context.Background())
	c.Start(ctx)
	id := uuid.New()
	c.CellUp(id, cell.CellUp{})
	c.MailUp(id, wire.MailUpMsg{Prim: 0x0200})
	cancel()
	c.Wait()
}
