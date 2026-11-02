// SPDX-License-Identifier: AGPL-3.0-or-later

package worker

import (
	"crypto/rand"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/dect/stack"
)

func fillRandom(b []byte) { _, _ = rand.Read(b) }

// callTimeout is how long the controller waits for a worker to answer a request
// before it declares the worker dead.
var callTimeout = 5 * time.Second

// ErrDead is what a stack of a worker that ended answers.
var ErrDead = errors.New("worker: the worker ended")

// Remote is the controller's end of one worker connection. It makes stacks
// that live in the worker.
type Remote struct {
	conn net.Conn
	wmu  sync.Mutex

	mu      sync.Mutex
	nextID  uint32
	nextReq uint32
	stacks  map[uint32]*proxy
	pending map[uint32]chan frame
	pong    chan struct{}

	dead atomic.Bool
	gone chan struct{} // closed when the connection ended
	err  error
}

// NewRemote starts reading conn, the controller's end of a worker's socket.
func NewRemote(conn net.Conn) *Remote {
	r := &Remote{conn: conn, stacks: map[uint32]*proxy{}, pending: map[uint32]chan frame{}, pong: make(chan struct{}, 1), gone: make(chan struct{})}
	go r.read()
	return r
}

// Alive says whether the worker's connection is up.
func (r *Remote) Alive() bool { return !r.dead.Load() }

// Gone is closed when the worker's connection ended (it exited, crashed or was killed).
func (r *Remote) Gone() <-chan struct{} { return r.gone }

// Err is why the connection ended, once Gone is closed.
func (r *Remote) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// Close ends the connection; the worker ends with it.
func (r *Remote) Close() { r.end(nil) }

// Ping asks the worker for a sign of life and waits for it.
func (r *Remote) Ping(timeout time.Duration) bool {
	if r.dead.Load() {
		return false
	}
	select { // a pong that came since the last one is old
	case <-r.pong:
	default:
	}
	if r.write(frame{typ: mPing}) != nil {
		return false
	}
	select {
	case <-r.pong:
		return true
	case <-time.After(timeout):
		return false
	case <-r.gone:
		return false
	}
}

// Stacks is how many stacks the worker hosts for this controller.
func (r *Remote) Stacks() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.stacks)
}

func (r *Remote) write(f frame) error {
	r.wmu.Lock()
	defer r.wmu.Unlock()
	_, err := r.conn.Write(f.bytes())
	if err != nil {
		go r.end(err)
	}
	return err
}

// end marks the worker dead and wakes everything that waits for it.
func (r *Remote) end(err error) {
	if !r.dead.CompareAndSwap(false, true) {
		return
	}
	r.mu.Lock()
	r.err = err
	ps := make([]*proxy, 0, len(r.stacks))
	for _, p := range r.stacks {
		ps = append(ps, p)
	}
	r.mu.Unlock()
	_ = r.conn.Close()
	close(r.gone)
	for _, p := range ps {
		p.died()
	}
}

func (r *Remote) read() {
	buf := make([]byte, maxFrame+hdrLen)
	for {
		n, err := r.conn.Read(buf)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				err = nil
			}
			r.end(err)
			return
		}
		f, err := parseFrame(append([]byte(nil), buf[:n]...))
		if err != nil {
			continue
		}
		r.dispatch(f)
	}
}

func (r *Remote) dispatch(f frame) {
	switch f.typ {
	case mPong:
		select {
		case r.pong <- struct{}{}:
		default:
		}
		return
	case mRet:
		r.mu.Lock()
		ch := r.pending[f.req]
		delete(r.pending, f.req)
		r.mu.Unlock()
		if ch != nil {
			ch <- f
		}
		return
	}
	r.mu.Lock()
	p := r.stacks[f.id]
	r.mu.Unlock()
	if p != nil {
		p.fromWorker(f)
	}
}

// request sends f and returns the id its answer will carry.
func (r *Remote) request(f frame) (chan frame, uint32) {
	ch := make(chan frame, 1)
	r.mu.Lock()
	r.nextReq++
	f.req = r.nextReq
	r.pending[f.req] = ch
	r.mu.Unlock()
	if r.write(f) != nil {
		r.mu.Lock()
		delete(r.pending, f.req)
		r.mu.Unlock()
		return nil, 0
	}
	return ch, f.req
}

// NewStack makes a stack in the worker for one cell. clock is the controller's
// monotonic clock in ms, which the stack's times are on. The result implements
// stack.Stack and stack.Pumper: its callbacks into env run on the goroutine that
// calls it, inside its calls and in Tick and Pump.
func (r *Remote) NewStack(env stack.Env, cfg stack.Config, clock func() uint64) stack.Stack {
	if r.dead.Load() {
		return nil
	}
	p := &proxy{r: r, env: env, q: make(chan func(), 4096), dead: make(chan struct{}), clock: clock}
	r.mu.Lock()
	r.nextID++
	p.id = r.nextID
	r.stacks[p.id] = p
	r.mu.Unlock()
	var w wbuf
	w.gob(cfg)
	w.u64(clock())
	ch, _ := r.request(frame{typ: mNew, id: p.id, body: w.b})
	if ch == nil {
		p.forget()
		return nil
	}
	select {
	case f := <-ch:
		if len(f.body) != 1 || f.body[0] != 1 {
			p.forget()
			return nil
		}
	case <-time.After(callTimeout):
		p.forget()
		return nil
	case <-r.gone:
		p.forget()
		return nil
	}
	return p
}

// proxy is a stack.Stack that lives in a worker.
type proxy struct {
	r     *Remote
	id    uint32
	env   stack.Env
	clock func() uint64

	q    chan func() // callbacks for the owner goroutine
	wake atomic.Pointer[func()]

	dead     chan struct{}
	deadOnce sync.Once

	mu   sync.Mutex
	st   snapshot
	rfpi bool
	shut sync.Once
}

type sig struct {
	dbm int
	at  uint64
	ok  bool
}

type snapshot struct {
	regOpen, regFree, quiet, hasRFPI bool
	seen                             map[int]uint64
	sig                              map[int]sig
}

var _ stack.Stack = (*proxy)(nil)
var _ stack.Pumper = (*proxy)(nil)

func (p *proxy) SetWake(f func()) { p.wake.Store(&f) }
func (p *proxy) Alive() bool      { return !p.isDead() }

func (p *proxy) isDead() bool {
	select {
	case <-p.dead:
		return true
	default:
		return false
	}
}

func (p *proxy) died() {
	p.deadOnce.Do(func() { close(p.dead) })
	if w := p.wake.Load(); w != nil {
		(*w)()
	}
}

func (p *proxy) forget() {
	p.r.mu.Lock()
	delete(p.r.stacks, p.id)
	p.r.mu.Unlock()
}

// enqueue hands a callback to the owner goroutine.
func (p *proxy) enqueue(fn func()) {
	select {
	case p.q <- fn:
	case <-p.dead:
		return
	}
	if w := p.wake.Load(); w != nil {
		(*w)()
	}
}

// Pump runs the callbacks that wait.
func (p *proxy) Pump() {
	for {
		select {
		case fn := <-p.q:
			fn()
		default:
			return
		}
	}
}

func (p *proxy) send(typ uint8, w *wbuf) {
	var b []byte
	if w != nil {
		b = w.b
	}
	_ = p.r.write(frame{typ: typ, id: p.id, body: b})
}

// call is a request to the stack; while it waits, callbacks run on this goroutine.
func (p *proxy) call(typ uint8, w *wbuf) ([]byte, bool) {
	if p.isDead() {
		return nil, false
	}
	var b []byte
	if w != nil {
		b = w.b
	}
	ch, req := p.r.request(frame{typ: typ, id: p.id, body: b})
	if ch == nil {
		return nil, false
	}
	t := time.NewTimer(callTimeout)
	defer t.Stop()
	for {
		select {
		case fn := <-p.q:
			fn()
		case f := <-ch:
			return f.body, true
		case <-t.C:
			p.r.mu.Lock()
			delete(p.r.pending, req)
			p.r.mu.Unlock()
			p.r.end(errors.New("worker: no answer"))
			return nil, false
		case <-p.dead:
			return nil, false
		}
	}
}

func (p *proxy) fromWorker(f frame) {
	r := &rbuf{b: f.body}
	switch f.typ {
	case mState:
		p.mu.Lock()
		p.st = readSnapshot(r)
		p.mu.Unlock()
	case mEnvSend:
		var m stack.Mail
		m.Src, m.Dst, m.Prim, m.Param = r.u8(), r.u8(), r.u16(), r.bytes()
		req := f.req
		p.enqueue(func() {
			var w wbuf
			msg := ""
			if err := p.env.Send(m); err != nil {
				msg = err.Error()
			}
			w.str(msg)
			_ = p.r.write(frame{typ: mRet, id: p.id, req: req, body: w.b})
		})
	case mEnvRoom:
		req := f.req
		p.enqueue(func() { _ = p.r.write(frame{typ: mRet, id: p.id, req: req, body: b2u(p.env.Room())}) })
	case mEnvLog:
		line := r.str()
		p.enqueue(func() { p.env.Log(line) })
	case mEnvEvent:
		to := stack.To(r.u8())
		line := r.str()
		p.enqueue(func() { p.env.Event(to, line) })
	case mEnvSave:
		i := int(r.u32())
		var rec stack.Record
		r.gob(&rec)
		if r.err == nil {
			p.enqueue(func() { p.env.Save(i, rec) })
		}
	case mEnvFetch:
		ipui := r.bytes()
		p.enqueue(func() { p.env.Fetch(ipui) })
	case mEnvForget:
		i, why := int(r.u32()), r.str()
		p.enqueue(func() { p.env.Forget(i, why) })
	case mEnvNamed:
		i, name := int(r.u32()), r.str()
		p.enqueue(func() { p.env.Named(i, name) })
	case mEnvNameVis:
		viewer, slot := uint(r.u32()), uint(r.u32())
		req := f.req
		p.enqueue(func() {
			_ = p.r.write(frame{typ: mRet, id: p.id, req: req, body: b2u(p.env.NameVisible(viewer, slot))})
		})
	case mEnvSwitched:
		cur, why := int(r.u32()), r.str()
		p.enqueue(func() { p.env.Switched(cur, why) })
	case mEnvRegisted:
		cur := int(r.u32())
		p.enqueue(func() { p.env.Registered(cur) })
	case mEnvCapLearn:
		i := int(r.u32())
		var c stack.Capability
		r.gob(&c)
		if r.err == nil {
			p.enqueue(func() { p.env.CapLearned(i, c) })
		}
	case mEnvClock:
		req := f.req
		p.enqueue(func() {
			t, ok := p.env.Clock()
			var w wbuf
			w.u64(uint64(t.UnixNano()))
			w.bool(ok)
			_ = p.r.write(frame{typ: mRet, id: p.id, req: req, body: w.b})
		})
	case mEnvContacts:
		i, line := int(r.u32()), r.str()
		p.enqueue(func() { p.env.ContactsChanged(i, line) })
	case mEnvMedia:
		h, c := r.u8(), r.u8()
		wb, up := r.bool(), r.bool()
		p.enqueue(func() { p.env.Media(h, c, wb, up) })
	}
}

func readSnapshot(r *rbuf) snapshot {
	s := snapshot{seen: map[int]uint64{}, sig: map[int]sig{}}
	s.regOpen, s.regFree, s.quiet, s.hasRFPI = r.bool(), r.bool(), r.bool(), r.bool()
	n := int(r.u32())
	for k := 0; k < n && r.err == nil && k < 1024; k++ {
		i := int(r.u32())
		s.seen[i] = r.u64()
		dbm := int(int32(r.u32()))
		at := r.u64()
		s.sig[i] = sig{dbm: dbm, at: at, ok: r.bool()}
	}
	return s
}

// stack.Stack

func (p *proxy) AddRecord(i int, rec stack.Record) {
	var w wbuf
	w.u32(uint32(i))
	w.gob(rec)
	p.send(mAddRecord, &w)
}

func (p *proxy) AddFreeStore(i int, tpui [3]byte) {
	var w wbuf
	w.u32(uint32(i))
	w.raw(tpui[:])
	p.send(mAddFree, &w)
}

func (p *proxy) Start() error {
	body, ok := p.call(mStart, nil)
	if !ok {
		return ErrDead
	}
	r := &rbuf{b: body}
	if msg := r.str(); msg != "" {
		return errors.New(msg)
	}
	return nil
}

func (p *proxy) RadioUp(up bool) {
	var w wbuf
	w.bool(up)
	p.send(mRadioUp, &w)
}

func (p *proxy) SetRegPINs(pins []string) {
	var w wbuf
	w.u32(uint32(len(pins)))
	for _, s := range pins {
		w.str(s)
	}
	p.send(mRegPINs, &w)
}

func (p *proxy) RegWindow(open bool, secs uint, now uint64) {
	var w wbuf
	w.bool(open)
	w.u32(uint32(secs))
	w.u64(now)
	p.send(mRegWindow, &w)
}

func (p *proxy) RegState() (open, free bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.st.regOpen, p.st.regFree
}

func (p *proxy) Mail(m stack.Mail, now uint64) {
	var w wbuf
	w.u8(m.Src)
	w.u8(m.Dst)
	w.u16(m.Prim)
	w.bytes(m.Param)
	w.u64(now)
	p.send(mMail, &w)
}

// Tick runs the callbacks that wait; the worker runs the stack's timers itself.
func (p *proxy) Tick(uint64) { p.Pump() }

func (p *proxy) Seen(i int) uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.st.seen[i]
}

func (p *proxy) Hello() { p.send(mHello, nil) }

func (p *proxy) Ctl(line string, now uint64, isBridge bool) bool {
	var w wbuf
	w.str(line)
	w.u64(now)
	w.bool(isBridge)
	body, ok := p.call(mCtl, &w)
	return ok && len(body) == 1 && body[0] == 1
}

func (p *proxy) Resume(now uint64) {
	var w wbuf
	w.u64(now)
	p.send(mResume, &w)
}

func (p *proxy) Drop(i int) bool {
	var w wbuf
	w.u32(uint32(i))
	body, ok := p.call(mDrop, &w)
	return ok && len(body) == 1 && body[0] == 1
}

func (p *proxy) Recycle(i int, tpui [3]byte) stack.Recycled {
	var w wbuf
	w.u32(uint32(i))
	w.raw(tpui[:])
	body, ok := p.call(mRecycle, &w)
	if !ok || len(body) != 1 {
		return stack.RecycleBusy // try again later, or the stack is rebuilt
	}
	return stack.Recycled(body[0])
}

func (p *proxy) Quiet() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.st.quiet
}

func (p *proxy) Signal(i int) (dbm int, at uint64, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.st.sig[i]
	return s.dbm, s.at, s.ok
}

func (p *proxy) SetRFPI(rfpi [5]byte) {
	p.mu.Lock()
	p.rfpi = true
	p.mu.Unlock()
	var w wbuf
	w.raw(rfpi[:])
	p.send(mSetRFPI, &w)
}

func (p *proxy) HasRFPI() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rfpi || p.st.hasRFPI
}

func (p *proxy) SetName(i int, name string) bool {
	var w wbuf
	w.u32(uint32(i))
	w.str(name)
	body, ok := p.call(mSetName, &w)
	return ok && len(body) == 1 && body[0] == 1
}

func (p *proxy) SetProfile(i int, mfr, dev string) bool {
	var w wbuf
	w.u32(uint32(i))
	w.str(mfr)
	w.str(dev)
	body, ok := p.call(mSetProfile, &w)
	return ok && len(body) == 1 && body[0] == 1
}

func (p *proxy) SetContacts(i int, text string) bool {
	if len(text) > maxFrame-64 {
		return false // does not fit one message
	}
	var w wbuf
	w.u32(uint32(i))
	w.str(text)
	body, ok := p.call(mSetContacts, &w)
	return ok && len(body) == 1 && body[0] == 1
}

func (p *proxy) SetLocateValidity(units uint) {
	var w wbuf
	w.u32(uint32(units))
	p.send(mSetValidity, &w)
}

func (p *proxy) Close() {
	p.shut.Do(func() {
		if !p.isDead() {
			p.call(mClose, nil)
		}
		p.forget()
		p.died()
	})
}
