// SPDX-License-Identifier: AGPL-3.0-or-later

package worker

import (
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/dect/stack"
)

// tickEvery is how often a hosted stack's timers run; stateEvery how often its
// snapshot goes to the controller (when it changed).
const (
	tickEvery  = 10 * time.Millisecond
	stateEvery = 100 * time.Millisecond
	// envTimeout is how long a stack waits for the controller to answer a callback
	// (Send, Room, NameVisible, Clock): the controller's cell goroutine may be busy
	// with other work for a moment.
	envTimeout = 5 * time.Second
)

// Serve is the worker: it hosts the stacks the controller asks for over conn
// until the connection ends, and returns then. newStack makes a stack
// (cdect.NewStack in production).
func Serve(conn net.Conn, newStack stack.New) error {
	s := &server{conn: conn, mk: newStack, stacks: map[uint32]*hosted{}, pending: map[uint32]chan frame{}, start: time.Now()}
	defer s.closeAll()
	buf := make([]byte, maxFrame+hdrLen)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		f, err := parseFrame(append([]byte(nil), buf[:n]...))
		if err != nil {
			continue
		}
		s.dispatch(f)
	}
}

type server struct {
	conn  net.Conn
	mk    stack.New
	start time.Time

	wmu sync.Mutex // one packet per write; writers take turns

	mu      sync.Mutex
	stacks  map[uint32]*hosted
	pending map[uint32]chan frame // callbacks waiting for the controller's answer, by request
	nextReq uint32
}

func (s *server) write(f frame) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.conn.Write(f.bytes())
	return err
}

func (s *server) dispatch(f frame) {
	switch f.typ {
	case mRet:
		s.mu.Lock()
		ch := s.pending[f.req]
		delete(s.pending, f.req)
		s.mu.Unlock()
		if ch != nil {
			ch <- f
		}
	case mPing:
		_ = s.write(frame{typ: mPong})
	case mNew:
		h := s.newHosted(f)
		if h == nil {
			_ = s.write(frame{typ: mRet, id: f.id, req: f.req, body: []byte{0}})
			return
		}
		s.mu.Lock()
		s.stacks[f.id] = h
		s.mu.Unlock()
		go h.run()
		_ = s.write(frame{typ: mRet, id: f.id, req: f.req, body: []byte{1}})
	default:
		s.mu.Lock()
		h := s.stacks[f.id]
		s.mu.Unlock()
		if h != nil {
			h.inbox.push(f)
		} else if f.req != 0 {
			_ = s.write(frame{typ: mRet, id: f.id, req: f.req}) // a stack that is gone answers nothing useful
		}
	}
}

func (s *server) closeAll() {
	s.mu.Lock()
	all := make([]*hosted, 0, len(s.stacks))
	for _, h := range s.stacks {
		all = append(all, h)
	}
	s.mu.Unlock()
	for _, h := range all {
		h.stop()
	}
}

// call sends a request to the controller and waits for its answer.
func (s *server) call(f frame) ([]byte, bool) {
	ch := make(chan frame, 1)
	s.mu.Lock()
	s.nextReq++
	f.req = s.nextReq
	s.pending[f.req] = ch
	s.mu.Unlock()
	if err := s.write(f); err != nil {
		s.mu.Lock()
		delete(s.pending, f.req)
		s.mu.Unlock()
		return nil, false
	}
	t := time.NewTimer(envTimeout)
	defer t.Stop()
	select {
	case r := <-ch:
		return r.body, true
	case <-t.C:
		s.mu.Lock()
		delete(s.pending, f.req)
		s.mu.Unlock()
		return nil, false
	}
}

// hosted is one stack and the goroutine that owns it.
type hosted struct {
	s     *server
	id    uint32
	st    stack.Stack
	env   *workerEnv
	inbox queue
	quit  chan struct{}
	done  chan struct{}
	once  sync.Once

	off  uint64 // the controller's clock minus ours, ms
	last uint64 // the last time the stack was given
	idx  map[int]bool
	sent []byte // the snapshot last sent
}

func (s *server) newHosted(f frame) *hosted {
	r := &rbuf{b: f.body}
	var cfg stack.Config
	r.gob(&cfg)
	coreNow := r.u64()
	if r.err != nil {
		return nil
	}
	h := &hosted{s: s, id: f.id, inbox: queue{wake: make(chan struct{}, 1)}, quit: make(chan struct{}), done: make(chan struct{}), idx: map[int]bool{}}
	h.env = &workerEnv{h: h}
	h.off = coreNow - h.mono()
	h.st = s.mk(h.env, cfg)
	if h.st == nil {
		return nil
	}
	return h
}

// mono is our monotonic clock in ms.
func (h *hosted) mono() uint64 { return uint64(time.Since(h.s.start) / time.Millisecond) }

// now is the controller's clock, as far as we know it, never going back.
func (h *hosted) now() uint64 {
	if v := h.mono() + h.off; v > h.last {
		h.last = v
	}
	return h.last
}

// at takes the time a command carries, keeping the clock monotonic.
func (h *hosted) at(v uint64) uint64 {
	if v > h.last {
		h.last = v
	}
	return h.last
}

func (h *hosted) stop() {
	h.once.Do(func() { close(h.quit) })
	<-h.done
}

func (h *hosted) run() {
	defer close(h.done)
	tick := time.NewTicker(tickEvery)
	defer tick.Stop()
	nextState := time.Now()
	for {
		select {
		case <-h.quit:
			h.st.Close()
			return
		case <-h.inbox.wake:
			for _, f := range h.inbox.take() {
				if h.handle(f) {
					return
				}
			}
		case <-tick.C:
			h.st.Tick(h.now())
			if time.Now().After(nextState) {
				nextState = time.Now().Add(stateEvery)
				h.pushState()
			}
		}
	}
}

func (h *hosted) reply(f frame, body []byte) {
	_ = h.s.write(frame{typ: mRet, id: h.id, req: f.req, body: body})
}

func b2u(v bool) []byte {
	if v {
		return []byte{1}
	}
	return []byte{0}
}

// handle runs one command; true: the stack is closed and the goroutine ends.
func (h *hosted) handle(f frame) bool {
	r := &rbuf{b: f.body}
	switch f.typ {
	case mAddRecord:
		i := int(r.u32())
		var rec stack.Record
		r.gob(&rec)
		if r.err == nil {
			h.idx[i] = true
			h.st.AddRecord(i, rec)
		}
	case mAddFree:
		i := int(r.u32())
		var t [3]byte
		copy(t[:], r.take(3))
		h.idx[i] = true
		h.st.AddFreeStore(i, t)
	case mStart:
		msg := ""
		if err := h.st.Start(); err != nil {
			msg = err.Error()
		}
		var w wbuf
		w.str(msg)
		h.reply(f, w.b)
	case mRadioUp:
		h.st.RadioUp(r.bool())
	case mRegPINs:
		n := int(r.u32())
		pins := make([]string, 0, min(n, 16))
		for k := 0; k < n && r.err == nil && k < 16; k++ {
			pins = append(pins, r.str())
		}
		h.st.SetRegPINs(pins)
	case mRegWindow:
		open := r.bool()
		secs := uint(r.u32())
		h.st.RegWindow(open, secs, h.at(r.u64()))
	case mMail:
		m := stack.Mail{Src: r.u8(), Dst: r.u8(), Prim: r.u16(), Param: r.bytes()}
		now := h.at(r.u64())
		if r.err == nil {
			h.st.Mail(m, now)
		}
	case mHello:
		h.st.Hello()
	case mCtl:
		line := r.str()
		now := h.at(r.u64())
		isBridge := r.bool()
		h.reply(f, b2u(h.st.Ctl(line, now, isBridge)))
	case mResume:
		h.st.Resume(h.at(r.u64()))
	case mDrop:
		i := int(r.u32())
		ok := h.st.Drop(i)
		if ok {
			delete(h.idx, i)
		}
		h.reply(f, b2u(ok))
	case mRecycle:
		i := int(r.u32())
		var t [3]byte
		copy(t[:], r.take(3))
		h.reply(f, []byte{byte(h.st.Recycle(i, t))})
	case mSetName:
		i := int(r.u32())
		h.reply(f, b2u(h.st.SetName(i, r.str())))
	case mSetProfile:
		i := int(r.u32())
		mfr, dev := r.str(), r.str()
		h.reply(f, b2u(h.st.SetProfile(i, mfr, dev)))
	case mSetRFPI:
		var a [5]byte
		copy(a[:], r.take(5))
		h.st.SetRFPI(a)
	case mSetValidity:
		h.st.SetLocateValidity(uint(r.u32()))
	case mSetContacts:
		i := int(r.u32())
		text := r.str()
		h.reply(f, b2u(r.err == nil && h.st.SetContacts(i, text)))
	case mClose:
		h.st.Close()
		h.reply(f, nil)
		h.s.mu.Lock()
		delete(h.s.stacks, h.id)
		h.s.mu.Unlock()
		return true
	}
	return false
}

// pushState sends the snapshot of what the controller reads often (Seen,
// Signal, RegState, Quiet, HasRFPI), if it changed.
func (h *hosted) pushState() {
	var w wbuf
	open, free := h.st.RegState()
	w.bool(open)
	w.bool(free)
	w.bool(h.st.Quiet())
	w.bool(h.st.HasRFPI())
	idx := make([]int, 0, len(h.idx))
	for i := range h.idx {
		idx = append(idx, i)
	}
	w.u32(uint32(len(idx)))
	for _, i := range idx {
		w.u32(uint32(i))
		w.u64(h.st.Seen(i))
		dbm, at, ok := h.st.Signal(i)
		w.u32(uint32(int32(dbm)))
		w.u64(at)
		w.bool(ok)
	}
	if string(w.b) == string(h.sent) {
		return
	}
	h.sent = w.b
	_ = h.s.write(frame{typ: mState, id: h.id, body: w.b})
}

// workerEnv is the stack's view of the controller: every callback is a message,
// and the ones that return a value wait for the answer.
type workerEnv struct{ h *hosted }

func (e *workerEnv) send(typ uint8, w *wbuf) {
	var b []byte
	if w != nil {
		b = w.b
	}
	_ = e.h.s.write(frame{typ: typ, id: e.h.id, body: b})
}

func (e *workerEnv) call(typ uint8, w *wbuf) ([]byte, bool) {
	var b []byte
	if w != nil {
		b = w.b
	}
	return e.h.s.call(frame{typ: typ, id: e.h.id, body: b})
}

func (e *workerEnv) Send(m stack.Mail) error {
	var w wbuf
	w.u8(m.Src)
	w.u8(m.Dst)
	w.u16(m.Prim)
	w.bytes(m.Param)
	body, ok := e.call(mEnvSend, &w)
	if !ok {
		return errors.New("worker: the controller did not take the mail")
	}
	r := &rbuf{b: body}
	if msg := r.str(); msg != "" {
		return errors.New(msg)
	}
	return nil
}

func (e *workerEnv) Room() bool {
	body, ok := e.call(mEnvRoom, nil)
	return ok && len(body) == 1 && body[0] == 1
}

func (e *workerEnv) Random(b []byte) { fillRandom(b) }

func (e *workerEnv) Log(line string) {
	var w wbuf
	w.str(line)
	e.send(mEnvLog, &w)
}

func (e *workerEnv) Event(to stack.To, line string) {
	var w wbuf
	w.u8(uint8(to))
	w.str(line)
	e.send(mEnvEvent, &w)
}

func (e *workerEnv) Save(i int, rec stack.Record) {
	var w wbuf
	w.u32(uint32(i))
	w.gob(rec)
	e.send(mEnvSave, &w)
}

func (e *workerEnv) Fetch(ipui []byte) {
	var w wbuf
	w.bytes(ipui)
	e.send(mEnvFetch, &w)
}

func (e *workerEnv) Forget(i int, why string) {
	var w wbuf
	w.u32(uint32(i))
	w.str(why)
	e.send(mEnvForget, &w)
}

func (e *workerEnv) Named(i int, name string) {
	var w wbuf
	w.u32(uint32(i))
	w.str(name)
	e.send(mEnvNamed, &w)
}

func (e *workerEnv) NameVisible(viewer, slot uint) bool {
	var w wbuf
	w.u32(uint32(viewer))
	w.u32(uint32(slot))
	body, ok := e.call(mEnvNameVis, &w)
	return ok && len(body) == 1 && body[0] == 1
}

func (e *workerEnv) Switched(cur int, why string) {
	var w wbuf
	w.u32(uint32(cur))
	w.str(why)
	e.send(mEnvSwitched, &w)
}

func (e *workerEnv) Registered(cur int) {
	var w wbuf
	w.u32(uint32(cur))
	e.send(mEnvRegisted, &w)
}

func (e *workerEnv) CapLearned(i int, c stack.Capability) {
	var w wbuf
	w.u32(uint32(i))
	w.gob(c)
	e.send(mEnvCapLearn, &w)
}

func (e *workerEnv) Clock() (time.Time, bool) {
	body, ok := e.call(mEnvClock, nil)
	r := &rbuf{b: body}
	ns := int64(r.u64())
	good := r.bool()
	if !ok || r.err != nil {
		return time.Time{}, false
	}
	return time.Unix(0, ns), good
}

func (e *workerEnv) ContactsChanged(i int, line string) {
	var w wbuf
	w.u32(uint32(i))
	w.str(line)
	e.send(mEnvContacts, &w)
}

func (e *workerEnv) Media(handle, channel uint8, wideband, up bool) {
	var w wbuf
	w.u8(handle)
	w.u8(channel)
	w.bool(wideband)
	w.bool(up)
	e.send(mEnvMedia, &w)
}

// queue is a stack's inbox. It never blocks the reader: the reader also takes
// the controller's answers to the stack's callbacks, which the stack may be
// waiting for.
type queue struct {
	mu    sync.Mutex
	items []frame
	wake  chan struct{}
}

func (q *queue) push(f frame) {
	q.mu.Lock()
	q.items = append(q.items, f)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *queue) take() []frame {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := q.items
	q.items = nil
	return out
}
