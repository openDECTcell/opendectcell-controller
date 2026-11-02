// SPDX-License-Identifier: AGPL-3.0-or-later

package worker

import (
	"net"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/dect/stack"
)

// pair is the two ends of a SOCK_SEQPACKET socket pair, as the supervisor makes them.
func pair(t *testing.T) (a, b net.Conn) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET, 0)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(fd int, name string) net.Conn {
		f := os.NewFile(uintptr(fd), name)
		c, err := net.FileConn(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	a, b = mk(fds[0], "a"), mk(fds[1], "b")
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b
}

// fakeStack is a stack that answers like the real one in the ways the proxy relies on, and
// calls the callbacks the real one does.
type fakeStack struct {
	env      stack.Env
	records  map[int]stack.Record
	blockCtl chan struct{}
	closed   atomic.Bool
}

func (f *fakeStack) AddRecord(i int, r stack.Record)     { f.records[i] = r }
func (f *fakeStack) AddFreeStore(i int, t [3]byte)       { f.records[i] = stack.Record{TPUI: t} }
func (f *fakeStack) Start() error                        { return nil }
func (f *fakeStack) RadioUp(bool)                        {}
func (f *fakeStack) SetRegPINs([]string)                 {}
func (f *fakeStack) RegWindow(bool, uint, uint64)        {}
func (f *fakeStack) RegState() (bool, bool)              { return true, true }
func (f *fakeStack) Tick(uint64)                         {}
func (f *fakeStack) Seen(i int) uint64                   { return 1000 + uint64(i) }
func (f *fakeStack) Hello()                              { f.env.Event(stack.ToAll, "hello\n") }
func (f *fakeStack) Resume(uint64)                       {}
func (f *fakeStack) Drop(i int) bool                     { return i == 1 }
func (f *fakeStack) Recycle(int, [3]byte) stack.Recycled { return stack.RecycleBusy }
func (f *fakeStack) Quiet() bool                         { return true }
func (f *fakeStack) Signal(i int) (int, uint64, bool)    { return -50 - i, 77, true }
func (f *fakeStack) SetRFPI([5]byte)                     {}
func (f *fakeStack) HasRFPI() bool                       { return false }
func (f *fakeStack) SetName(i int, n string) bool        { return n != "" }
func (f *fakeStack) SetProfile(int, string, string) bool { return true }
func (f *fakeStack) SetLocateValidity(uint)              {}
func (f *fakeStack) Close()                              { f.closed.Store(true) }

func (f *fakeStack) Mail(m stack.Mail, now uint64) {
	// a mail from the chip: answer it with one for the chip, ask whether it may go, and tell what the host should save
	if f.env.Room() {
		_ = f.env.Send(stack.Mail{Src: m.Dst, Dst: m.Src, Prim: m.Prim + 1, Param: append([]byte("re:"), m.Param...)})
	}
	f.env.Save(2, stack.Record{Slot: 2, PortID: []byte{1, 2, 3}})
	f.env.Named(2, "Anna")
	if f.env.NameVisible(1, 2) {
		f.env.Log("visible")
	}
	f.env.Media(3, 1, true, true)
}

func (f *fakeStack) Ctl(line string, now uint64, isBridge bool) bool {
	if f.blockCtl != nil {
		<-f.blockCtl
	}
	f.env.Event(stack.ToAsker, "echo "+line)
	return line == "bridge"
}

// ownerEnv records what the stack calls. It is used by one goroutine only, the test's: the
// proxy has to run every callback there, and the race detector says when it does not.
type ownerEnv struct {
	sent    []stack.Mail
	events  []string
	saved   []int
	named   []string
	logs    []string
	media   []uint8
	room    bool
	visible bool
}

func (e *ownerEnv) Send(m stack.Mail) error { e.sent = append(e.sent, m); return nil }
func (e *ownerEnv) Room() bool              { return e.room }
func (e *ownerEnv) Random(b []byte)         {}
func (e *ownerEnv) Log(l string)            { e.logs = append(e.logs, l) }
func (e *ownerEnv) Event(_ stack.To, l string) {
	e.events = append(e.events, l)
}
func (e *ownerEnv) Save(i int, _ stack.Record)         { e.saved = append(e.saved, i) }
func (e *ownerEnv) Fetch([]byte)                       {}
func (e *ownerEnv) Forget(int, string)                 {}
func (e *ownerEnv) Named(i int, n string)              { e.named = append(e.named, n) }
func (e *ownerEnv) NameVisible(viewer, slot uint) bool { return e.visible }
func (e *ownerEnv) Switched(int, string)               {}
func (e *ownerEnv) Registered(int)                     {}
func (e *ownerEnv) CapLearned(int, stack.Capability)   {}
func (e *ownerEnv) Clock() (time.Time, bool)           { return time.Unix(1700000000, 0), true }
func (e *ownerEnv) Media(h, c uint8, wb, up bool)      { e.media = append(e.media, h) }

func pumpUntil(t *testing.T, p stack.Stack, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		p.Tick(0)
		time.Sleep(2 * time.Millisecond)
	}
}

func newPair(t *testing.T, env stack.Env, fs *fakeStack) (*Remote, stack.Stack) {
	t.Helper()
	ctl, wrk := pair(t)
	go Serve(wrk, func(e stack.Env, cfg stack.Config) stack.Stack {
		fs.env = e
		fs.records = map[int]stack.Record{}
		return fs
	})
	r := NewRemote(ctl)
	t.Cleanup(r.Close)
	st := r.NewStack(env, stack.Config{PIN: "12345678", LocateValidity: 6}, func() uint64 { return 5000 })
	if st == nil {
		t.Fatal("no stack")
	}
	return r, st
}

// Commands go to the stack in the worker, answers come back, and the stack's callbacks run
// on the goroutine that owns the proxy.
func TestRemoteStackRoundTrip(t *testing.T) {
	env := &ownerEnv{room: true, visible: true}
	fs := &fakeStack{}
	_, st := newPair(t, env, fs)

	st.AddRecord(1, stack.Record{Slot: 1, PortID: []byte{9}})
	st.AddFreeStore(2, [3]byte{0, 0x11, 0x21})
	if err := st.Start(); err != nil {
		t.Fatal(err)
	}
	if !st.Ctl("bridge", 10, false) || st.Ctl("other", 11, true) {
		t.Error("Ctl does not say whether the client became the bridge")
	}
	if !st.Drop(1) || st.Drop(2) {
		t.Error("Drop")
	}
	if !st.SetName(1, "Anna") || st.SetName(1, "") {
		t.Error("SetName")
	}
	if st.Recycle(1, [3]byte{}) != stack.RecycleBusy {
		t.Error("Recycle")
	}
	// the Ctl answered with an Event: it ran here, inside the call
	if len(env.events) != 2 || env.events[0] != "echo bridge" {
		t.Errorf("events %q", env.events)
	}

	st.Mail(stack.Mail{Src: 1, Dst: 2, Prim: 0x0200, Param: []byte("x")}, 20)
	pumpUntil(t, st, "the stack's callbacks", func() bool { return len(env.media) == 1 && len(env.logs) == 1 })
	if len(env.sent) != 1 || env.sent[0].Prim != 0x0201 || string(env.sent[0].Param) != "re:x" {
		t.Errorf("sent %+v", env.sent)
	}
	if len(env.saved) != 1 || env.saved[0] != 2 || env.named[0] != "Anna" {
		t.Errorf("saved %v, named %v", env.saved, env.named)
	}

	// what the controller reads often comes from the snapshot the worker pushes
	pumpUntil(t, st, "the snapshot", func() bool { return st.Seen(2) == 1002 })
	if dbm, at, ok := st.Signal(2); dbm != -52 || at != 77 || !ok {
		t.Errorf("signal %d %d %v", dbm, at, ok)
	}
	if open, free := st.RegState(); !open || !free || !st.Quiet() {
		t.Error("RegState or Quiet")
	}
	st.SetRFPI([5]byte{1})
	if !st.HasRFPI() {
		t.Error("HasRFPI after SetRFPI")
	}

	st.Close()
	if !fs.closed.Load() {
		t.Error("the stack was not closed in the worker")
	}
}

// A worker that ends (it crashed, it was killed) takes its stacks with it: they report it,
// the calls return at once with their zero answers, and the connection reports why.
func TestWorkerEndsAndTheStacksSeeIt(t *testing.T) {
	env := &ownerEnv{}
	ctl, wrk := pair(t)
	go Serve(wrk, func(e stack.Env, cfg stack.Config) stack.Stack {
		return &fakeStack{env: e, records: map[int]stack.Record{}}
	})
	r := NewRemote(ctl)
	st := r.NewStack(env, stack.Config{}, func() uint64 { return 1 })
	if st == nil {
		t.Fatal("no stack")
	}
	woken := make(chan struct{}, 1)
	st.(Pumper).SetWake(func() {
		select {
		case woken <- struct{}{}:
		default:
		}
	})
	if !st.(Pumper).Alive() || !r.Alive() {
		t.Fatal("not alive at the start")
	}
	wrk.Close() // the worker is gone
	select {
	case <-r.Gone():
	case <-time.After(3 * time.Second):
		t.Fatal("the connection did not report the worker's end")
	}
	select {
	case <-woken:
	case <-time.After(time.Second):
		t.Error("the owner was not woken")
	}
	if st.(Pumper).Alive() {
		t.Error("the stack is alive in a dead worker")
	}
	if err := st.Start(); err == nil {
		t.Error("Start worked in a dead worker")
	}
	if st.Ctl("bridge", 1, false) || st.Drop(1) {
		t.Error("calls into a dead worker answer yes")
	}
	if r.NewStack(env, stack.Config{}, func() uint64 { return 1 }) != nil {
		t.Error("a stack was made in a dead worker")
	}
	st.Close() // must not hang
}

// A worker whose stack does not answer within the timeout is declared dead, so one hung
// stack cannot hang its cell's goroutine for ever.
func TestWorkerThatHangsIsDeclaredDead(t *testing.T) {
	old := callTimeout
	callTimeout = 150 * time.Millisecond
	defer func() { callTimeout = old }()
	env := &ownerEnv{}
	fs := &fakeStack{blockCtl: make(chan struct{})}
	defer close(fs.blockCtl)
	r, st := newPair(t, env, fs)
	start := time.Now()
	if st.Ctl("bridge", 1, false) {
		t.Error("an answer from a hung stack")
	}
	if d := time.Since(start); d < 100*time.Millisecond || d > 2*time.Second {
		t.Errorf("returned after %v", d)
	}
	select {
	case <-r.Gone():
	case <-time.After(time.Second):
		t.Error("the worker was not declared dead")
	}
	if st.(Pumper).Alive() {
		t.Error("alive after a hang")
	}
}

// Ping is the supervisor's watchdog.
func TestPing(t *testing.T) {
	env := &ownerEnv{}
	r, _ := newPair(t, env, &fakeStack{})
	if !r.Ping(time.Second) {
		t.Error("no pong from a live worker")
	}
}
