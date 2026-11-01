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
	"github.com/openDECTcell/opendectcell-controller/internal/dect/regwin"
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
	// the TPUI each free record was given, in the order of free
	freeTPUI [][3]byte
	started  bool
	radio    bool
	closed   bool
	mails    []stack.Mail
	ticks    int
	// the registration window (ADR 0005); free defaults to true
	regPINs  []string
	regOpen  bool
	regSecs  uint
	regOpens int
	notFree  bool
	// onMail runs on the cell's goroutine for each mail, with the env.
	onMail func(env stack.Env, m stack.Mail)
	onTick func(env stack.Env)
	seen   map[int]uint64
	// the bridge commands the core sent, and what to do about each
	ctls  []ctlLine
	onCtl func(env stack.Env, line string)
	// the working set: answers to a Fetch, records dropped, a busy stack that
	// refuses a drop, the validities set later
	resumes  int
	dropped  []int
	busy     bool
	validity []uint
}

type ctlLine struct {
	line     string
	isBridge bool
}

func (f *fakeStack) AddRecord(i int, r stack.Record) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records[i] = r
}

func (f *fakeStack) AddFreeStore(i int, tpui [3]byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.free = append(f.free, i)
	f.freeTPUI = append(f.freeTPUI, tpui)
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

// seen is what Seen answers, by record index; the test sets it.
func (f *fakeStack) Seen(i int) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen[i]
}

func (f *fakeStack) SetRegPINs(p []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.regPINs = slices.Clone(p)
}

func (f *fakeStack) RegWindow(open bool, secs uint, _ uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.regOpen, f.regSecs = open, secs
	if open {
		f.regOpens++
	}
}

func (f *fakeStack) RegState() (bool, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.regOpen, !f.notFree
}

// Resume, Drop and SetLocateValidity are noted. A drop is refused while busy is set.
func (f *fakeStack) Resume(uint64) {
	f.mu.Lock()
	f.resumes++
	f.mu.Unlock()
}

func (f *fakeStack) Drop(i int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.busy {
		return false
	}
	delete(f.records, i)
	f.dropped = append(f.dropped, i)
	return true
}

func (f *fakeStack) Quiet() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.busy
}

func (f *fakeStack) SetLocateValidity(u uint) {
	f.mu.Lock()
	f.validity = append(f.validity, u)
	f.mu.Unlock()
}

func (f *fakeStack) Hello() {}

// Ctl records the command; "bridge" says the client became the bridge, as the
// real stack does.
func (f *fakeStack) Ctl(line string, _ uint64, isBridge bool) bool {
	f.mu.Lock()
	f.ctls = append(f.ctls, ctlLine{line, isBridge})
	cb := f.onCtl
	f.mu.Unlock()
	if cb != nil {
		cb(f.env, line)
	}
	return line == "bridge"
}
func (f *fakeStack) Close()                    { f.mu.Lock(); f.closed = true; f.mu.Unlock() }
func (f *fakeStack) get(fn func(f *fakeStack)) { f.mu.Lock(); defer f.mu.Unlock(); fn(f) }
func (f *fakeStack) isClosed() bool            { f.mu.Lock(); defer f.mu.Unlock(); return f.closed }

type fakeLink struct {
	mu    sync.Mutex
	mails []sent
	// identity pushes: the RFPI asked for, nil for "your own again"
	pushes []*[5]byte
}

func (l *fakeLink) SetIdentity(_ uuid.UUID, rfpi *[5]byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pushes = append(l.pushes, rfpi)
	return nil
}

func (l *fakeLink) nPushes() int { l.mu.Lock(); defer l.mu.Unlock(); return len(l.pushes) }

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
	owners    []uuid.UUID
	newID     uuid.UUID
	installed [][]uint
	unknown   []uint
	forgot    []uuid.UUID
	seenOn    map[uuid.UUID]uuid.UUID // handset -> the cell it was last seen on
	seenTimes []time.Time
	// the pool: ReserveTPUI offers TPUIs when pool is set; place is what
	// PlaceHandset returns, by handset; forgetPooled is Forget's answer
	pool         bool
	nextTPUI     uint8
	reserved     [][3]byte
	released     [][3]byte
	place        map[uuid.UUID]Handset
	placed       []uuid.UUID
	unplaced     []uuid.UUID
	unplaceHook  func()
	full         bool // PlaceHandset says the cell is full
	known        map[uuid.UUID]bool
	hsOn         map[uuid.UUID][]Handset // CellRecords by cell, instead of hs
	forgetPooled bool
	saved        []stack.Record
	lastOn       map[uuid.UUID]uuid.UUID // handset -> LastSeenCell's answer
}

func (r *fakeRecords) CellRecords(_ context.Context, cell uuid.UUID, _ int) ([]Handset, []uuid.UUID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail > 0 {
		r.fail--
		return nil, nil, errors.New("database down")
	}
	if hs, ok := r.hsOn[cell]; ok {
		return slices.Clone(hs), nil, nil
	}
	return slices.Clone(r.hs), nil, nil
}

func (r *fakeRecords) SaveRecord(_ context.Context, _, id, owner uuid.UUID, rec stack.Record) (Saved, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.saves = append(r.saves, id)
	r.owners = append(r.owners, owner)
	r.saved = append(r.saved, rec)
	if id == uuid.Nil {
		_, pooled := poolTPUI(rec.TPUI)
		return Saved{ID: r.newID, Enrolled: true, Pooled: pooled}, nil
	}
	return Saved{ID: id}, nil
}

func (r *fakeRecords) Forget(_ context.Context, _, id uuid.UUID) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forgot = append(r.forgot, id)
	return r.forgetPooled, nil
}

// PlaceHandset and PlaceByIPUI hand out what the test put in place (by handset),
// for as long as the handset is placed; a full cell says so.
func (r *fakeRecords) PlaceHandset(_ context.Context, cell, id uuid.UUID) (Handset, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.full {
		return Handset{}, false, ErrCellFull
	}
	h, ok := r.place[id]
	if ok {
		r.placed = append(r.placed, id)
	}
	return h, ok, nil
}

func (r *fakeRecords) PlaceByIPUI(ctx context.Context, cell uuid.UUID, ipui []byte) (Handset, bool, error) {
	r.mu.Lock()
	var id uuid.UUID
	for k, h := range r.place {
		if string(h.Rec.PortID) == string(ipui) {
			id = k
		}
	}
	r.mu.Unlock()
	if id == uuid.Nil {
		return Handset{}, false, nil
	}
	return r.PlaceHandset(ctx, cell, id)
}

func (r *fakeRecords) Unplace(_ context.Context, _, id uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.unplaced = append(r.unplaced, id)
	if r.unplaceHook != nil {
		r.unplaceHook()
	}
	return nil
}

func (r *fakeRecords) HandsetKnown(_ context.Context, id uuid.UUID) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.place[id]; ok {
		return true, nil
	}
	for _, h := range r.hs {
		if h.ID == id {
			return true, nil
		}
	}
	return r.known[id], nil
}

// ReserveTPUI offers {00, 11, 7}, {00, 11, 8}, ... when pool is set.
func (r *fakeRecords) ReserveTPUI(context.Context) ([3]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.pool {
		return [3]byte{}, nil
	}
	r.nextTPUI++
	t := [3]byte{0, 0x11, poolFirst + r.nextTPUI - 1}
	r.reserved = append(r.reserved, t)
	return t, nil
}

func (r *fakeRecords) ReleaseTPUI(t [3]byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.released = append(r.released, t)
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

func (r *fakeRecords) Seen(_ context.Context, cell, id uuid.UUID, at time.Time) (uuid.UUID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seenOn == nil {
		r.seenOn = map[uuid.UUID]uuid.UUID{}
	}
	prev := r.seenOn[id]
	r.seenOn[id] = cell
	r.seenTimes = append(r.seenTimes, at)
	return prev, nil
}

func (r *fakeRecords) LastSeenCell(_ context.Context, id uuid.UUID) (uuid.UUID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastOn[id], nil
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

// owner owns the test handsets.
var owner = uuid.New()

func handset(slot uint, owner uuid.UUID) Handset {
	h := Handset{ID: uuid.New(), Owner: owner, Rec: stack.Record{Slot: slot, PortID: []byte{0x80, byte(slot)}}}
	h.Rec.UAK[0] = byte(slot)
	return h
}

func radioOn(g *rig) func() bool { return radioOnAt(g, 0) }

// radioOnAt: the i-th stack built has its radio on.
func radioOnAt(g *rig, i int) func() bool {
	return func() bool {
		on := false
		if f := g.stack(i); f != nil {
			f.get(func(f *fakeStack) { on = f.radio })
		}
		return on
	}
}

func TestBringUpAndRecords(t *testing.T) {
	recs := &fakeRecords{hs: []Handset{handset(2, owner), handset(5, owner)}}
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
		if !f.cfg.NamesRO || f.cfg.UAKTable != 0x067F || f.cfg.CheckDelayMs != 50 || f.cfg.Stub != "06.88" {
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
	}{{"08.25", 0x9518, 50, 0x0153}, {"07.56", 0, 0, 0x0153}, {"06.88", 0x067F, 50, 0x015B}} {
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
	g := newRig(t, &fakeRecords{hs: []Handset{handset(1, owner)}}, nil)
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
	newID, anna := uuid.New(), uuid.New()
	recs := &fakeRecords{newID: newID}
	reg := regwin.New(slog.New(slog.DiscardHandler), nil, nil)
	g := newRig(t, recs, func(f *fakeStack) {
		f.onMail = func(env stack.Env, m stack.Mail) {
			// the record keeps its RegPIN1 for the run
			env.Save(0, stack.Record{Slot: 1, PortID: []byte{0x80, 0x01}, RegPIN1: 2})
		}
	})
	g.core.Reg = reg
	w0, _, _ := reg.Open(uuid.New(), uuid.New(), 8)
	w1, _, _ := reg.Open(anna, anna, 8)
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
		if r.owners[0] != anna {
			t.Errorf("enrolment owned by %v, want the PIN's user", r.owners[0])
		}
	})
	waitFor(t, "the window bound", func() bool {
		w, _ := reg.Get(w1.ID)
		return w.State == regwin.Completed && w.Cell == id && w.Handset == newID
	})
	if w, _ := reg.Get(w0.ID); w.State != regwin.Open {
		t.Errorf("the other user's window %s", w.State)
	}
	if g.nStacks() != 1 {
		t.Error("the stack was rebuilt: a later save of the record claimed again")
	}
}

// The window follows the live PINs: open with them on a cell that can take
// a handset, closed when none is live, never on a full cell.
func TestRegistrationWindow(t *testing.T) {
	reg := regwin.New(slog.New(slog.DiscardHandler), nil, nil)
	full := false
	g := newRig(t, &fakeRecords{}, func(f *fakeStack) { f.notFree = full })
	g.core.Reg = reg
	id := uuid.New()
	g.core.CellUp(id, cell.CellUp{StubVersion: "08.25"})
	waitFor(t, "radio on", radioOn(g))
	g.stack(0).get(func(f *fakeStack) {
		if f.regOpen || len(f.regPINs) != 0 {
			t.Errorf("open with no PIN: %v %q", f.regOpen, f.regPINs)
		}
	})
	w, pin, _ := reg.Open(uuid.New(), uuid.New(), 8)
	waitFor(t, "open", func() bool {
		ok := false
		g.stack(0).get(func(f *fakeStack) {
			ok = f.regOpen && slices.Equal(f.regPINs, []string{pin}) && f.regSecs > 55 && f.regSecs <= 60
		})
		return ok
	})
	// the stack closes its window after an enrolment; a live PIN reopens it
	g.stack(0).get(func(f *fakeStack) { f.regOpen = false })
	waitFor(t, "reopened", func() bool {
		n := 0
		g.stack(0).get(func(f *fakeStack) {
			if f.regOpen {
				n = f.regOpens
			}
		})
		return n == 2
	})
	reg.Cancel(w.ID)
	waitFor(t, "closed", func() bool {
		ok := false
		g.stack(0).get(func(f *fakeStack) { ok = !f.regOpen && len(f.regPINs) == 0 })
		return ok
	})

	full = true
	id2 := uuid.New()
	g.core.CellUp(id2, cell.CellUp{StubVersion: "08.25"})
	waitFor(t, "radio on", radioOnAt(g, 1))
	reg.Open(uuid.New(), uuid.New(), 8)
	waitFor(t, "the first cell open", func() bool {
		ok := false
		g.stack(0).get(func(f *fakeStack) { ok = f.regOpen })
		return ok
	})
	time.Sleep(20 * time.Millisecond)
	g.stack(1).get(func(f *fakeStack) {
		if f.regOpen {
			t.Error("a cell without a free record opened")
		}
	})
}

// The library's "registration rejected" line counts towards the lockout.
func TestRegistrationFailures(t *testing.T) {
	reg := regwin.New(slog.New(slog.DiscardHandler), nil, nil)
	g := newRig(t, &fakeRecords{}, func(f *fakeStack) {
		f.onMail = func(env stack.Env, m stack.Mail) {
			if m.Prim == 1 {
				env.Log("reg: registration rejected (expected: this model tries PIN 0000 by itself first, profile reg.tries_0000_first)")
			} else {
				env.Log("reg: registration rejected")
			}
		}
	})
	g.core.Reg = reg
	reg.Open(uuid.New(), uuid.New(), 8)
	id := uuid.New()
	g.core.CellUp(id, cell.CellUp{StubVersion: "08.25"})
	waitFor(t, "radio on", radioOn(g))
	for range regwin.FailLimit {
		g.core.MailUp(id, wire.MailUpMsg{Prim: 1})
	}
	waitFor(t, "open", func() bool {
		ok := false
		g.stack(0).get(func(f *fakeStack) { ok = f.regOpen && len(f.mails) == regwin.FailLimit })
		return ok
	})
	for range regwin.FailLimit {
		g.core.MailUp(id, wire.MailUpMsg{Prim: 2})
	}
	waitFor(t, "locked out", func() bool {
		ok := false
		g.stack(0).get(func(f *fakeStack) { ok = !f.regOpen && len(f.regPINs) == 0 })
		return ok
	})
}

// An enrolment without a registration window is not stored, and the stack
// is rebuilt so the key leaves it (the maintainers' notes point 2).
func TestEnrolmentWithoutWindow(t *testing.T) {
	recs := &fakeRecords{newID: uuid.New()}
	g := newRig(t, recs, func(f *fakeStack) {
		f.onMail = func(env stack.Env, m stack.Mail) {
			env.Save(0, stack.Record{Slot: 1, PortID: []byte{0x80, 0x01}})
		}
	})
	id := uuid.New()
	g.core.CellUp(id, cell.CellUp{StubVersion: "08.25"})
	waitFor(t, "radio on", radioOn(g))
	g.core.MailUp(id, wire.MailUpMsg{Prim: 0x0203})
	waitFor(t, "the first stack closed", func() bool {
		c := false
		g.stack(0).get(func(f *fakeStack) { c = f.closed })
		return c
	})
	waitFor(t, "a new stack", func() bool { return g.nStacks() == 2 })
	recs.get(func(r *fakeRecords) {
		if len(r.saves) != 0 {
			t.Errorf("saves %v without a window", r.saves)
		}
	})
}

func TestNameVisible(t *testing.T) {
	anna, ben := uuid.New(), uuid.New()
	g := &gen{owner: map[uint]uuid.UUID{1: anna, 2: anna, 3: ben}}
	for _, c := range []struct {
		viewer, slot uint
		want         bool
	}{{1, 2, true}, {1, 3, false}, {3, 1, false}, {1, 6, false}, {6, 6, true}} {
		if got := g.NameVisible(c.viewer, c.slot); got != c.want {
			t.Errorf("NameVisible(%d, %d) = %v", c.viewer, c.slot, got)
		}
	}
}

func TestSlotWriteRejected(t *testing.T) {
	recs := &fakeRecords{hs: []Handset{handset(3, owner)}}
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

// A panic while the stack is built still closes it (its keys are wiped)
// and ends its writer, and the build is retried.
func TestPanicWhileBuilding(t *testing.T) {
	var mu sync.Mutex
	broken := 1
	g := newRig(t, &fakeRecords{hs: []Handset{handset(1, owner)}}, func(f *fakeStack) {
		mu.Lock()
		defer mu.Unlock()
		if broken > 0 {
			broken--
			f.records = nil // AddRecord panics: assignment to a nil map
		}
	})
	g.core.CellUp(uuid.New(), cell.CellUp{StubVersion: "08.25"})
	waitFor(t, "the broken stack closed", func() bool {
		c := false
		if f := g.stack(0); f != nil {
			f.get(func(f *fakeStack) { c = f.closed })
		}
		return c
	})
	waitFor(t, "rebuilt", radioOnAt(g, 1))
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

// A cell whose chip is still starting rejects the bring-up (CHIP_DOWN): the
// stack must not stay "radio on" on a chip that heard nothing, but start again.
func TestChipDownRejectRestartsTheStack(t *testing.T) {
	recs := &fakeRecords{hs: []Handset{handset(2, owner)}}
	g := newRig(t, recs, nil)
	id := uuid.New()
	g.core.CellUp(id, cell.CellUp{StubVersion: "08.25", CapSubRecords: 6})
	waitFor(t, "radio on", radioOn(g))
	n := len(g.link.all())
	g.core.MailReject(id, wire.MailRejectMsg{RefCorr: 0, Reason: wire.RejectChipDown})
	waitFor(t, "a second stack", func() bool { return g.stack(1) != nil })
	waitFor(t, "the bring-up again", func() bool { return len(g.link.all()) >= 2*n-1 })
	// slots must not be marked unknown by such a reject
	recs.get(func(r *fakeRecords) {
		if len(r.unknown) != 0 {
			t.Errorf("slots marked unknown: %v", r.unknown)
		}
	})
}

// A cell whose daemon restarted behind a live link reports its radio parked
// while the stack believes it is on: the stack starts again and brings it up.
func TestParkedRadioAfterBringUpRestartsTheStack(t *testing.T) {
	recs := &fakeRecords{hs: []Handset{handset(2, owner)}}
	g := newRig(t, recs, nil)
	id := uuid.New()
	g.core.CellUp(id, cell.CellUp{StubVersion: "08.25", CapSubRecords: 6})
	waitFor(t, "radio on", radioOn(g))
	// right after the bring-up a stale "off" is ignored
	g.core.CellRadio(id, 0, 2)
	time.Sleep(50 * time.Millisecond)
	if g.stack(1) != nil {
		t.Fatal("a STATUS right after the bring-up restarted the stack")
	}
	time.Sleep(radioSettle + 100*time.Millisecond)
	g.core.CellRadio(id, 1, 2) // radio on: nothing
	time.Sleep(50 * time.Millisecond)
	if g.stack(1) != nil {
		t.Fatal("radio on restarted the stack")
	}
	g.core.CellRadio(id, 0, 1) // chip down: not our case
	time.Sleep(50 * time.Millisecond)
	if g.stack(1) != nil {
		t.Fatal("a down chip restarted the stack")
	}
	g.core.CellRadio(id, 0, 2)
	waitFor(t, "a second stack", func() bool { return g.stack(1) != nil })
}

// fakeIdentity is what the core is told the cell should broadcast.
type fakeIdentity struct {
	mu     sync.Mutex
	want   cell.IdentityWant
	pushed []*[5]byte
}

func (f *fakeIdentity) Want(context.Context, uuid.UUID) (cell.IdentityWant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.want, nil
}

func (f *fakeIdentity) Pushed(_ context.Context, _ uuid.UUID, r *[5]byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushed = append(f.pushed, r)
	return nil
}

func (f *fakeIdentity) set(w cell.IdentityWant) { f.mu.Lock(); f.want = w; f.mu.Unlock() }

var (
	rfpiOwn    = [5]byte{0x02, 0x70, 0x0d, 0x5e, 0x68}
	rfpiSystem = [5]byte{0x10, 0xf4, 0x20, 0x11, 0x01}
)

func TestIdentityPushAndWait(t *testing.T) {
	recs := &fakeRecords{}
	g := newRig(t, recs, nil)
	idn := &fakeIdentity{want: cell.IdentityWant{Set: true, RFPI: rfpiSystem, ParkPLI: 19}}
	g.core.Identity = idn
	id := uuid.New()

	// the cell broadcasts its own: it is asked, and no stack starts yet
	g.core.CellUp(id, cell.CellUp{StubVersion: "06.88", RFPI: rfpiOwn, HasRFPI: true})
	waitFor(t, "the push", func() bool { return g.link.nPushes() == 1 })
	if p := g.link.pushes[0]; p == nil || *p != rfpiSystem {
		t.Fatalf("asked for %v", p)
	}
	time.Sleep(30 * time.Millisecond)
	if g.nStacks() != 0 {
		t.Fatal("a stack started on a cell about to reload its chip")
	}
	idn.mu.Lock()
	if len(idn.pushed) != 1 || idn.pushed[0] == nil {
		t.Errorf("the push was not recorded: %v", idn.pushed)
	}
	idn.mu.Unlock()

	// it comes back with the identity: the stack starts with the PARK length
	g.core.CellDown(id)
	g.core.CellUp(id, cell.CellUp{StubVersion: "06.88", RFPI: rfpiSystem, HasRFPI: true})
	waitFor(t, "the stack", func() bool { return g.stack(0) != nil })
	g.stack(0).get(func(f *fakeStack) {
		if f.cfg.ParkPLI != 19 {
			t.Errorf("PARK length %d, want 19", f.cfg.ParkPLI)
		}
	})
	if g.link.nPushes() != 1 {
		t.Errorf("asked again: %d pushes", g.link.nPushes())
	}
}

func TestIdentityRefusedIsNotAskedAgain(t *testing.T) {
	g := newRig(t, &fakeRecords{}, nil)
	g.core.Identity = &fakeIdentity{want: cell.IdentityWant{Set: true, RFPI: rfpiSystem, ParkPLI: 19}}
	id := uuid.New()
	up := cell.CellUp{StubVersion: "06.88", RFPI: rfpiOwn, HasRFPI: true}
	g.core.CellUp(id, up)
	waitFor(t, "the push", func() bool { return g.link.nPushes() == 1 })
	// back with its own identity: it did not take it. The stack runs with it, and
	// with no shortened PARK (that would be wrong for a class A RFPI).
	g.core.CellDown(id)
	g.core.CellUp(id, up)
	waitFor(t, "the stack", func() bool { return g.stack(0) != nil })
	if g.link.nPushes() != 1 {
		t.Errorf("asked a second time: %d", g.link.nPushes())
	}
	g.stack(0).get(func(f *fakeStack) {
		if f.cfg.ParkPLI != 0 {
			t.Errorf("PARK length %d on a cell with its own identity", f.cfg.ParkPLI)
		}
	})
}

func TestIdentityRevertAndOldCells(t *testing.T) {
	g := newRig(t, &fakeRecords{}, nil)
	idn := &fakeIdentity{}
	g.core.Identity = idn
	// an older cell reports no RFPI: left alone, even if an identity is set
	idn.set(cell.IdentityWant{Set: true, RFPI: rfpiSystem})
	old := uuid.New()
	g.core.CellUp(old, cell.CellUp{StubVersion: "06.88"})
	waitFor(t, "the old cell's stack", func() bool { return g.stack(0) != nil })
	if g.link.nPushes() != 0 {
		t.Fatal("an old cell was asked")
	}
	// turned off after a push: the cell is asked for its own, once
	idn.set(cell.IdentityWant{Revert: true})
	id := uuid.New()
	up := cell.CellUp{StubVersion: "06.88", RFPI: rfpiSystem, HasRFPI: true}
	g.core.CellUp(id, up)
	waitFor(t, "the way back", func() bool { return g.link.nPushes() == 1 })
	if g.link.pushes[0] != nil {
		t.Fatalf("asked for %v, want its own", g.link.pushes[0])
	}
	g.core.CellDown(id)
	g.core.CellUp(id, up)
	waitFor(t, "a stack for it", func() bool { return g.stack(1) != nil })
	if g.link.nPushes() != 1 {
		t.Errorf("the way back asked twice: %d", g.link.nPushes())
	}
}

func TestIdentityCellThatNeverComesBack(t *testing.T) {
	old := identityWait
	identityWait = 50 * time.Millisecond
	defer func() { identityWait = old }()
	g := newRig(t, &fakeRecords{}, nil)
	g.core.Identity = &fakeIdentity{want: cell.IdentityWant{Set: true, RFPI: rfpiSystem, ParkPLI: 19}}
	g.core.CellUp(uuid.New(), cell.CellUp{StubVersion: "06.88", RFPI: rfpiOwn, HasRFPI: true})
	waitFor(t, "the push", func() bool { return g.link.nPushes() == 1 })
	waitFor(t, "the stack after the wait", func() bool { return g.stack(0) != nil })
}

// A handset heard on a cell is noted: the cell and the time, at most once per
// seenEvery, and a later contact on the same cell after the pause is noted too.
func TestHandsetSeenOnACell(t *testing.T) {
	old := seenEvery
	seenEvery = 60 * time.Millisecond
	defer func() { seenEvery = old }()
	h := handset(1, owner)
	recs := &fakeRecords{hs: []Handset{h}}
	g := newRig(t, recs, func(f *fakeStack) { f.seen = map[int]uint64{} })
	id := uuid.New()
	g.core.CellUp(id, cell.CellUp{StubVersion: "06.88", CapSubRecords: 6})
	waitFor(t, "radio on", radioOn(g))
	// nobody heard yet: nothing written
	time.Sleep(50 * time.Millisecond)
	recs.get(func(r *fakeRecords) {
		if len(r.seenTimes) != 0 {
			t.Fatalf("a handset nobody heard was noted: %v", r.seenTimes)
		}
	})
	set := func(ms uint64) { g.stack(0).get(func(f *fakeStack) { f.seen[0] = ms }) }
	set(g.core.now())
	waitFor(t, "the first contact", func() bool {
		n := 0
		recs.get(func(r *fakeRecords) { n = len(r.seenTimes) })
		return n == 1
	})
	recs.get(func(r *fakeRecords) {
		if r.seenOn[h.ID] != id {
			t.Errorf("noted on cell %v, want %v", r.seenOn[h.ID], id)
		}
		if d := time.Since(r.seenTimes[0]); d < 0 || d > time.Second {
			t.Errorf("the contact time is %v ago", d)
		}
	})
	// the same value again, or a new one inside the pause: no second write
	set(g.core.now())
	time.Sleep(20 * time.Millisecond)
	recs.get(func(r *fakeRecords) {
		if len(r.seenTimes) != 1 {
			t.Errorf("written again inside the pause: %d", len(r.seenTimes))
		}
	})
	// after the pause a newer contact is written
	time.Sleep(80 * time.Millisecond)
	set(g.core.now())
	waitFor(t, "a later contact", func() bool {
		n := 0
		recs.get(func(r *fakeRecords) { n = len(r.seenTimes) })
		return n == 2
	})
}

// With the pool on, a cell's chip records come first, then the places a new
// handset can enrol (each with a TPUI of the pool). The table above the chip's
// slots starts empty.
func TestPooledRecordsAndOffers(t *testing.T) {
	recs := &fakeRecords{hs: []Handset{handset(2, owner)}, pool: true}
	g := newRig(t, recs, nil)
	id := uuid.New()
	g.core.CellUp(id, cell.CellUp{StubVersion: "08.25", CapSubRecords: 6})
	waitFor(t, "radio on", radioOn(g))
	g.stack(0).get(func(f *fakeStack) {
		if len(f.records) != 1 || f.records[0].Slot != 2 {
			t.Errorf("records %+v: want the chip record only", f.records)
		}
		if !slices.Equal(f.free, []int{1, 2, 3, 4, 5}) {
			t.Errorf("free stores %v", f.free)
		}
		for k, tp := range f.freeTPUI {
			if tp != ([3]byte{0, 0x11, byte(poolFirst + k)}) {
				t.Errorf("free store %d offered %x", k, tp)
			}
		}
		if !f.cfg.DeferUnknown || f.cfg.LocateValidity != 15 {
			t.Errorf("config %+v: unknown handsets are held for a fetch, validity 10 min is 15 units", f.cfg)
		}
	})
	waitFor(t, "slots recorded", func() bool {
		n := 0
		recs.get(func(r *fakeRecords) { n = len(r.installed) })
		return n == 1
	})
	recs.get(func(r *fakeRecords) {
		if !slices.Equal(r.installed[len(r.installed)-1], []uint{2}) {
			t.Errorf("installed %v", r.installed)
		}
	})
	g.core.CellGone(id)
	waitFor(t, "the offers given back", func() bool {
		n := 0
		recs.get(func(r *fakeRecords) { n = len(r.released) })
		return n == 5
	})
}

// A pooled handset that enrols on one cell: that cell rebuilds its stack so the
// handset leaves the chip slot it enrolled in; other cells do nothing, the handset
// comes to them when it is heard there.
func TestPooledEnrolmentRebuildsOnlyTheCell(t *testing.T) {
	old := reloadDelay
	reloadDelay = 20 * time.Millisecond
	defer func() { reloadDelay = old }()
	newID, anna := uuid.New(), uuid.New()
	recs := &fakeRecords{newID: newID, pool: true}
	reg := regwin.New(slog.New(slog.DiscardHandler), nil, nil)
	g := newRig(t, recs, func(f *fakeStack) {
		f.onMail = func(env stack.Env, m stack.Mail) {
			env.Save(0, stack.Record{Slot: 1, PortID: []byte{0x80, 0x01}, RegPIN1: 1, TPUI: [3]byte{0, 0x11, 7}})
		}
	})
	g.core.Reg = reg
	reg.Open(anna, anna, 8)
	a, b := uuid.New(), uuid.New()
	g.core.CellUp(a, cell.CellUp{StubVersion: "08.25"})
	waitFor(t, "a's radio on", radioOnAt(g, 0))
	g.core.CellUp(b, cell.CellUp{StubVersion: "08.25"})
	waitFor(t, "b's radio on", radioOnAt(g, 1))

	g.core.MailUp(a, wire.MailUpMsg{Prim: 0x0203})
	waitFor(t, "a rebuilt", func() bool { return g.nStacks() == 3 })
	time.Sleep(60 * time.Millisecond)
	if g.stack(1).isClosed() || g.nStacks() != 3 {
		t.Errorf("b was rebuilt (%d stacks)", g.nStacks())
	}
}

// The rebuild after a pooled enrolment ends every call on the cell, so it waits
// until the stack is quiet.
func TestEnrolmentRebuildWaitsForAQuietStack(t *testing.T) {
	old := reloadDelay
	reloadDelay = 20 * time.Millisecond
	defer func() { reloadDelay = old }()
	newID, anna := uuid.New(), uuid.New()
	recs := &fakeRecords{newID: newID, pool: true}
	reg := regwin.New(slog.New(slog.DiscardHandler), nil, nil)
	g := newRig(t, recs, func(f *fakeStack) {
		f.onMail = func(env stack.Env, m stack.Mail) {
			env.Save(0, stack.Record{Slot: 1, PortID: []byte{0x80, 0x01}, RegPIN1: 1, TPUI: [3]byte{0, 0x11, 7}})
		}
	})
	g.core.Reg = reg
	reg.Open(anna, anna, 8)
	id := uuid.New()
	g.core.CellUp(id, cell.CellUp{StubVersion: "08.25"})
	waitFor(t, "radio on", radioOn(g))
	g.stack(0).get(func(f *fakeStack) { f.busy = true }) // a call is up
	g.core.MailUp(id, wire.MailUpMsg{Prim: 0x0203})
	time.Sleep(1200 * time.Millisecond) // well past the delay and several ticks
	if g.nStacks() != 1 || g.stack(0).isClosed() {
		t.Fatalf("the stack was rebuilt during a call (%d stacks)", g.nStacks())
	}
	g.stack(0).get(func(f *fakeStack) { f.busy = false })
	waitFor(t, "rebuilt once quiet", func() bool { return g.nStacks() == 2 })
}

// fetchRig is a cell with one handset that the store knows but the cell does not
// hold, asking about it.
func fetchRig(t *testing.T) (*rig, *fakeRecords, Handset, uuid.UUID) {
	t.Helper()
	h := handset(7, owner)
	h.Rec.TPUI = [3]byte{0, 0x11, 9}
	recs := &fakeRecords{place: map[uuid.UUID]Handset{h.ID: h}}
	g := newRig(t, recs, nil)
	id := uuid.New()
	g.core.CellUp(id, cell.CellUp{StubVersion: "08.25", CapSubRecords: 6})
	waitFor(t, "radio on", radioOn(g))
	return g, recs, h, id
}

func resumes(g *rig) int {
	n := 0
	g.stack(0).get(func(f *fakeStack) { n = f.resumes })
	return n
}

// A locate from a handset the cell does not hold: the store has it, the record
// goes into the table above the chip's slots and the stack answers its held request.
func TestFetchAdmitsAHandsetTheStoreKnows(t *testing.T) {
	g, recs, h, id := fetchRig(t)
	_ = id
	g.stack(0).env.Fetch(h.Rec.PortID)
	waitFor(t, "the stack resumed", func() bool { return resumes(g) == 1 })
	g.stack(0).get(func(f *fakeStack) {
		r, ok := f.records[6] // after the six chip-or-free indices
		if !ok || r.Slot != 7 || r.TPUI != h.Rec.TPUI {
			t.Errorf("records %+v: the fetched handset should be at index 6", f.records)
		}
	})
	if len(recs.unplaced) != 0 {
		t.Errorf("unplaced %v", recs.unplaced)
	}
	// asked again (a retransmitted request): nothing is added twice
	g.stack(0).env.Fetch(h.Rec.PortID)
	waitFor(t, "the stack resumed again", func() bool { return resumes(g) == 2 })
	g.stack(0).get(func(f *fakeStack) {
		if len(f.records) != 1 {
			t.Errorf("records %+v", f.records)
		}
	})
}

// A handset nobody knows is answered at once, so the stack can turn it away.
func TestFetchOfAStrangerIsAnswered(t *testing.T) {
	g, _, _, _ := fetchRig(t)
	g.stack(0).env.Fetch([]byte{0x80, 0xee})
	waitFor(t, "the stack resumed", func() bool { return resumes(g) == 1 })
	g.stack(0).get(func(f *fakeStack) {
		if len(f.records) != 0 {
			t.Errorf("records %+v", f.records)
		}
	})
}

// The working set expires: not heard for the validity and the grace, it goes, and
// its record number is free in the store. One that is heard stays.
func TestWorkingSetExpiry(t *testing.T) {
	h1, h2 := handset(7, owner), handset(8, owner)
	h2.Rec.PortID = []byte{0x80, 0x08}
	recs := &fakeRecords{place: map[uuid.UUID]Handset{h1.ID: h1, h2.ID: h2}}
	g := newRig(t, recs, func(f *fakeStack) { f.seen = map[int]uint64{} })
	g.core.Roaming = func(context.Context) (Roaming, error) {
		return Roaming{LocateValidity: 400 * time.Millisecond, ExpireAfter: 400 * time.Millisecond, MovedAfter: time.Hour}, nil
	}
	id := uuid.New()
	g.core.CellUp(id, cell.CellUp{StubVersion: "08.25", CapSubRecords: 6})
	waitFor(t, "radio on", radioOn(g))
	g.stack(0).env.Fetch(h1.Rec.PortID)
	g.stack(0).env.Fetch(h2.Rec.PortID)
	waitFor(t, "both in", func() bool {
		n := 0
		g.stack(0).get(func(f *fakeStack) { n = len(f.records) })
		return n == 2
	})
	// h2 is heard now and then, h1 never
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(30 * time.Millisecond):
				g.stack(0).get(func(f *fakeStack) {
					for i, r := range f.records {
						if r.Slot == 8 {
							f.seen[i] = g.core.now()
						}
					}
				})
			}
		}
	}()
	waitFor(t, "h1 dropped", func() bool {
		var d []int
		g.stack(0).get(func(f *fakeStack) { d = slices.Clone(f.dropped) })
		return len(d) >= 1
	})
	waitFor(t, "its number freed in the store", func() bool {
		n := 0
		recs.get(func(r *fakeRecords) { n = len(r.unplaced) })
		return n >= 1
	})
	recs.get(func(r *fakeRecords) {
		if !slices.Equal(r.unplaced, []uuid.UUID{h1.ID}) {
			t.Errorf("unplaced %v, want h1 only", r.unplaced)
		}
	})
	time.Sleep(1500 * time.Millisecond) // well past the expiry of a handset nobody heard
	g.stack(0).get(func(f *fakeStack) {
		if len(f.dropped) != 1 || len(f.records) != 1 {
			t.Errorf("dropped %v, records %+v: the handset that was heard stays", f.dropped, f.records)
		}
	})
}

// A handset heard on another cell leaves this one MovedAfter later, not before.
func TestWorkingSetMoved(t *testing.T) {
	h := handset(7, owner)
	recs := &fakeRecords{place: map[uuid.UUID]Handset{h.ID: h}}
	g := newRig(t, recs, nil)
	g.core.Roaming = func(context.Context) (Roaming, error) {
		return Roaming{LocateValidity: time.Hour, ExpireAfter: time.Hour, MovedAfter: 150 * time.Millisecond}, nil
	}
	id := uuid.New()
	g.core.CellUp(id, cell.CellUp{StubVersion: "08.25", CapSubRecords: 6})
	waitFor(t, "radio on", radioOn(g))
	g.stack(0).env.Fetch(h.Rec.PortID)
	waitFor(t, "in", func() bool {
		n := 0
		g.stack(0).get(func(f *fakeStack) { n = len(f.records) })
		return n == 1
	})
	g.core.noteHeard(h.ID, uuid.New(), time.Now())
	time.Sleep(80 * time.Millisecond)
	g.stack(0).get(func(f *fakeStack) {
		if len(f.dropped) != 0 {
			t.Error("dropped before MovedAfter")
		}
	})
	waitFor(t, "dropped after MovedAfter", func() bool {
		n := 0
		g.stack(0).get(func(f *fakeStack) { n = len(f.dropped) })
		return n == 1
	})
}

// A busy stack refuses the drop: it is tried again until it is quiet.
func TestWorkingSetDropWaitsForAQuietStack(t *testing.T) {
	h := handset(7, owner)
	recs := &fakeRecords{place: map[uuid.UUID]Handset{h.ID: h}}
	g := newRig(t, recs, nil)
	g.core.Roaming = func(context.Context) (Roaming, error) {
		return Roaming{LocateValidity: 20 * time.Millisecond, ExpireAfter: 20 * time.Millisecond}, nil
	}
	id := uuid.New()
	g.core.CellUp(id, cell.CellUp{StubVersion: "08.25", CapSubRecords: 6})
	waitFor(t, "radio on", radioOn(g))
	g.stack(0).get(func(f *fakeStack) { f.busy = true })
	g.stack(0).env.Fetch(h.Rec.PortID)
	time.Sleep(1500 * time.Millisecond) // past the expiry and a sweep
	g.stack(0).get(func(f *fakeStack) {
		if len(f.dropped) != 0 {
			t.Fatal("dropped while busy")
		}
		f.busy = false
	})
	waitFor(t, "dropped once quiet", func() bool {
		n := 0
		g.stack(0).get(func(f *fakeStack) { n = len(f.dropped) })
		return n == 1
	})
}

// A full table makes room by dropping the handset heard least recently.
func TestFullTableDropsTheStalest(t *testing.T) {
	var all []Handset
	place := map[uuid.UUID]Handset{}
	for k := range maxRecord - maxSlot {
		h := handset(uint(maxSlot+1+k), owner)
		h.Rec.PortID = []byte{0x80, byte(0x40 + k)}
		all = append(all, h)
		place[h.ID] = h
	}
	extra := handset(maxRecord, owner)
	extra.Rec.PortID = []byte{0x80, 0xfe}
	place[extra.ID] = extra
	recs := &fakeRecords{place: place}
	g := newRig(t, recs, nil)
	g.core.Roaming = func(context.Context) (Roaming, error) {
		return Roaming{LocateValidity: time.Hour, ExpireAfter: time.Hour, MovedAfter: time.Hour}, nil
	}
	id := uuid.New()
	// no free stores: six chip-or-free indices come first, so give the cell six real chip records
	for k := range maxSlot {
		recs.hs = append(recs.hs, handset(uint(k+1), owner))
	}
	g.core.CellUp(id, cell.CellUp{StubVersion: "08.25", CapSubRecords: 6})
	waitFor(t, "radio on", radioOn(g))
	for _, h := range all {
		g.stack(0).env.Fetch(h.Rec.PortID)
		time.Sleep(3 * time.Millisecond) // a different time of arrival for each
	}
	waitFor(t, "table full", func() bool {
		n := 0
		g.stack(0).get(func(f *fakeStack) { n = len(f.records) })
		return n == maxRecord
	})
	g.stack(0).env.Fetch(extra.Rec.PortID)
	waitFor(t, "the stalest dropped, the new one in", func() bool {
		n := 0
		g.stack(0).get(func(f *fakeStack) { n = len(f.dropped) })
		return n == 1
	})
	g.stack(0).get(func(f *fakeStack) {
		if f.dropped[0] != maxSlot { // the first one fetched, index 6
			t.Errorf("dropped index %d, want %d (the first fetched)", f.dropped[0], maxSlot)
		}
	})
}

// A pooled handset that deregisters (or enrols again with a new key) on one cell is dropped
// from the other cells' tables.
func TestPooledForgetEvictsFromTheOtherCells(t *testing.T) {
	h := handset(1, owner) // on cell a it is a chip record
	hb := h
	hb.Rec.Slot = 7 // on cell b it comes into the table above the chip's slots
	a, b := uuid.New(), uuid.New()
	recs := &fakeRecords{place: map[uuid.UUID]Handset{h.ID: hb}, forgetPooled: true,
		hsOn: map[uuid.UUID][]Handset{a: {h}}}
	g := newRig(t, recs, func(f *fakeStack) {
		f.onMail = func(env stack.Env, m stack.Mail) { env.Forget(0, "test") }
	})
	g.core.CellUp(a, cell.CellUp{StubVersion: "08.25"})
	waitFor(t, "a's radio on", radioOnAt(g, 0))
	g.core.CellUp(b, cell.CellUp{StubVersion: "08.25"})
	waitFor(t, "b's radio on", radioOnAt(g, 1))
	g.stack(1).env.Fetch(h.Rec.PortID)
	waitFor(t, "h in b's table", func() bool {
		k := 0
		g.stack(1).get(func(f *fakeStack) { k = len(f.records) })
		return k == 1
	})
	g.core.MailUp(a, wire.MailUpMsg{Prim: 0x0203}) // h deregisters on a
	waitFor(t, "h dropped from b", func() bool {
		var d []int
		g.stack(1).get(func(f *fakeStack) { d = slices.Clone(f.dropped) })
		return slices.Equal(d, []int{6})
	})
	if g.stack(0).isClosed() {
		t.Error("a was rebuilt")
	}
}

// The admin's validity reaches the stacks that run: a new value is set on the stack
// within a moment, without a rebuild.
func TestRoamingChangeReachesTheStack(t *testing.T) {
	old := roamingEvery
	roamingEvery = 10 * time.Millisecond
	defer func() { roamingEvery = old }()
	var mu sync.Mutex
	v := 10 * time.Minute
	recs := &fakeRecords{}
	g := newRig(t, recs, nil)
	g.core.Roaming = func(context.Context) (Roaming, error) {
		mu.Lock()
		defer mu.Unlock()
		return Roaming{LocateValidity: v, ExpireAfter: time.Minute, MovedAfter: time.Minute}, nil
	}
	id := uuid.New()
	g.core.CellUp(id, cell.CellUp{StubVersion: "08.25"})
	waitFor(t, "radio on", radioOn(g))
	mu.Lock()
	v = time.Hour
	mu.Unlock()
	waitFor(t, "the new validity on the stack", func() bool {
		var got []uint
		g.stack(0).get(func(f *fakeStack) { got = slices.Clone(f.validity) })
		return slices.Equal(got, []uint{88})
	})
	if g.nStacks() != 1 {
		t.Error("the stack was rebuilt")
	}
}
