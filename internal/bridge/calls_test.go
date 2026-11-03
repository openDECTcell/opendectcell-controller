// SPDX-License-Identifier: AGPL-3.0-or-later

package bridge

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/calllog"
	"github.com/openDECTcell/opendectcell-controller/internal/cell"
	"github.com/openDECTcell/opendectcell-controller/internal/cell/wire"
	"github.com/openDECTcell/opendectcell-controller/internal/dect/core"
	"github.com/openDECTcell/opendectcell-controller/internal/media"
	"github.com/openDECTcell/opendectcell-controller/internal/sip"
	"github.com/openDECTcell/opendectcell-controller/internal/sip/rtp"
	"github.com/openDECTcell/opendectcell-controller/internal/sip/sdp"
)

var lo = netip.MustParseAddr("127.0.0.1")

// fakeSIP is a SIP call whose far end the test plays.
type fakeSIP struct {
	events chan sip.Event
	neg    *sdp.Media
	ep     *rtp.Endpoint
	mu     sync.Mutex
	acts   []string
}

func newFakeSIP(t *testing.T, codec sdp.Codec) *fakeSIP {
	t.Helper()
	ports := &rtp.Ports{IP: lo, Min: 45000, Max: 45200}
	ep, err := ports.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ep.Close() })
	peer, err := ports.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { peer.Close() })
	neg := &sdp.Media{Remote: netip.AddrPortFrom(lo, uint16(peer.LocalPort())), Codec: codec, PTime: 20}
	if err := ep.Configure(neg); err != nil {
		t.Fatal(err)
	}
	return &fakeSIP{events: make(chan sip.Event, 16), neg: neg, ep: ep}
}

func (f *fakeSIP) note(a string) { f.mu.Lock(); f.acts = append(f.acts, a); f.mu.Unlock() }
func (f *fakeSIP) actions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.acts...)
}
func (f *fakeSIP) has(a string) bool {
	for _, x := range f.actions() {
		if x == a {
			return true
		}
	}
	return false
}

func (f *fakeSIP) Events() <-chan sip.Event { return f.events }
func (f *fakeSIP) Negotiated() *sdp.Media   { return f.neg }
func (f *fakeSIP) Media() *rtp.Endpoint     { return f.ep }
func (f *fakeSIP) Ringing() error           { f.note("ringing"); return nil }
func (f *fakeSIP) Answer() error            { f.note("answer"); return nil }
func (f *fakeSIP) Reject(code int) {
	f.note("reject " + string(rune('0'+code/100)) + string(rune('0'+code/10%10)) + string(rune('0'+code%10)))
}
func (f *fakeSIP) Hangup() { f.note("hangup") }
func (f *fakeSIP) SendDTMF(_ context.Context, d rune) error {
	f.note("dtmf " + string(d))
	return nil
}
func (f *fakeSIP) Hold(_ context.Context, on bool) error {
	if on {
		f.note("hold")
	} else {
		f.note("resume")
	}
	return nil
}

type fakeCaller struct {
	call *fakeSIP
	err  error
	got  string
}

func (c *fakeCaller) Dial(_ context.Context, _ uuid.UUID, number string) (SIPCall, error) {
	c.got = number
	if c.err != nil {
		return nil, c.err
	}
	return c.call, nil
}

type fakeLines struct {
	acct   uuid.UUID
	has    bool
	recv   []Receiver
	hlines []HandsetLine
}

func (l fakeLines) OutLine(context.Context, uuid.UUID, int) (uuid.UUID, bool, error) {
	return l.acct, l.has, nil
}
func (l fakeLines) Receivers(context.Context, uuid.UUID) ([]Receiver, error) { return l.recv, nil }
func (l fakeLines) HandsetLines(context.Context, uuid.UUID) ([]HandsetLine, error) {
	return l.hlines, nil
}

// fakeHandset is core.Outgoing as the manager sees it.
type fakeHandset struct {
	info   core.OutCall
	events chan core.CallEvent
	mu     sync.Mutex
	acts   []string
}

func newFakeHandset(wb bool) *fakeHandset {
	return &fakeHandset{info: core.OutCall{Cell: uuid.New(), Call: "5", Handset: uuid.New(), Slot: 7, Line: 1, Number: "0301234", Wideband: wb},
		events: make(chan core.CallEvent, 16)}
}
func (h *fakeHandset) note(a string) { h.mu.Lock(); h.acts = append(h.acts, a); h.mu.Unlock() }
func (h *fakeHandset) actions() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.acts...)
}
func (h *fakeHandset) Info() core.OutCall            { return h.info }
func (h *fakeHandset) Events() <-chan core.CallEvent { return h.events }
func (h *fakeHandset) Ringing()                      { h.note("ringing") }
func (h *fakeHandset) Answered(nb bool) {
	if nb {
		h.note("answered nb")
	} else {
		h.note("answered wb")
	}
}
func (h *fakeHandset) Release(r string) {
	h.note("release " + r)
	h.mu.Lock()
	defer h.mu.Unlock()
}

type fakeOffer struct {
	events    chan core.CallEvent
	cancelled chan struct{}
	once      sync.Once
}

func (o *fakeOffer) Events() <-chan core.CallEvent { return o.events }
func (o *fakeOffer) Cancel() {
	o.once.Do(func() {
		close(o.cancelled)
		o.events <- core.CallEvent{Kind: core.CallReleased, Reason: "normal"}
		close(o.events)
	})
}

type fakeRouter struct {
	mu     sync.Mutex
	offers map[uuid.UUID]*fakeOffer
	calls  map[uuid.UUID]core.Call
	err    map[uuid.UUID]error
}

func (r *fakeRouter) Ring(_ context.Context, h uuid.UUID, c core.Call) (Offer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.err[h]; e != nil {
		return nil, e
	}
	o := &fakeOffer{events: make(chan core.CallEvent, 16), cancelled: make(chan struct{})}
	if r.offers == nil {
		r.offers, r.calls = map[uuid.UUID]*fakeOffer{}, map[uuid.UUID]core.Call{}
	}
	r.offers[h] = o
	r.calls[h] = c
	return o, nil
}

type fakeCells struct {
	m      *Manager
	mu     sync.Mutex
	opened []wire.MediaOpenMsg
	closed []uint32
	status cell.Status
	ack    uint16
	silent bool
}

func (c *fakeCells) OpenMedia(id uuid.UUID, mo wire.MediaOpenMsg) error {
	c.mu.Lock()
	c.opened = append(c.opened, mo)
	c.mu.Unlock()
	if !c.silent {
		c.m.MediaOpened(id, wire.MediaOpenAckMsg{MediaID: mo.MediaID, Status: c.ack})
	}
	return nil
}
func (c *fakeCells) CloseMedia(_ uuid.UUID, id uint32) error {
	c.mu.Lock()
	c.closed = append(c.closed, id)
	c.mu.Unlock()
	return nil
}
func (c *fakeCells) CellStatus(uuid.UUID) cell.Status { return c.status }
func (c *fakeCells) counts() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.opened), len(c.closed)
}

type fakeLog struct {
	mu      sync.Mutex
	started []calllog.Call
	answer  int
	ended   []calllog.End
}

func (l *fakeLog) Start(_ context.Context, c calllog.Call) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.started = append(l.started, c)
	return int64(len(l.started)), nil
}
func (l *fakeLog) Answered(context.Context, int64, time.Time) error {
	l.mu.Lock()
	l.answer++
	l.mu.Unlock()
	return nil
}
func (l *fakeLog) Ended(_ context.Context, _ int64, e calllog.End) error {
	l.mu.Lock()
	l.ended = append(l.ended, e)
	l.mu.Unlock()
	return nil
}
func (l *fakeLog) ends() []calllog.End {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]calllog.End(nil), l.ended...)
}

type rig struct {
	m     *Manager
	cells *fakeCells
	log   *fakeLog
}

func newRig(t *testing.T, lines fakeLines) *rig {
	t.Helper()
	r := &rig{cells: &fakeCells{ack: wire.MediaOK, status: cell.Status{AudioDown: []uint8{wire.FormatL16_8k, wire.FormatG722},
		AudioUp: []uint8{wire.FormatL16_8k, wire.FormatG722}, CapAudioWB: 3}}, log: &fakeLog{}}
	r.m = &Manager{Cells: r.cells, Lines: lines, CallLog: r.log, Log: slog.New(slog.DiscardHandler), MediaIP: lo,
		OpenTimeout: 300 * time.Millisecond}
	r.cells.m = r.m
	return r
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for range 300 {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

var pcma = sdp.Codec{Name: "PCMA", PT: 8, Clock: 8000}

// A handset dials: the far end rings and answers, the handset is told (narrowband:
// the far end took G.711), the speech opens when the chip's channel is bound, a
// key and a hold reach the far end, and the handset hanging up ends it all.
func TestHandsetCallEndToEnd(t *testing.T) {
	r := newRig(t, fakeLines{acct: uuid.New(), has: true})
	sc := newFakeSIP(t, pcma)
	r.m.SIP = &fakeCaller{call: sc}
	h := newFakeHandset(true)
	done := make(chan struct{})
	go func() { r.m.handsetCall(h); close(done) }()

	sc.events <- sip.Event{Kind: sip.EvRinging}
	waitFor(t, "ringing", func() bool { return len(h.actions()) == 1 })
	sc.events <- sip.Event{Kind: sip.EvAnswered}
	waitFor(t, "answered", func() bool { return len(h.actions()) == 2 })
	if got := h.actions(); got[0] != "ringing" || got[1] != "answered nb" {
		t.Fatalf("the handset was told %q", got)
	}
	h.events <- core.CallEvent{Kind: core.CallMediaUp, Cell: h.info.Cell, Handle: 0, Channel: 1, Wideband: false}
	waitFor(t, "MEDIA_OPEN", func() bool { n, _ := r.cells.counts(); return n == 1 })
	mo := r.cells.opened[0]
	if mo.AudioChannel != 1 || mo.Format != wire.FormatL16_8k || mo.SRTPProfile != wire.SRTPAES128SHA80 || mo.CtlAddr.Addr() != lo || mo.CtlAddr.Port() == 0 {
		t.Errorf("MEDIA_OPEN %+v", mo)
	}
	h.events <- core.CallEvent{Kind: core.CallDigit, Digit: '5'}
	h.events <- core.CallEvent{Kind: core.CallHold}
	h.events <- core.CallEvent{Kind: core.CallResume}
	waitFor(t, "the far end got the key and the holds", func() bool { return sc.has("dtmf 5") && sc.has("hold") && sc.has("resume") })
	h.events <- core.CallEvent{Kind: core.CallReleased, Reason: "hs"}
	<-done
	if !sc.has("hangup") {
		t.Errorf("the far end was not hung up: %q", sc.actions())
	}
	if _, c := r.cells.counts(); c != 1 {
		t.Errorf("the cell's flow was closed %d times, want 1", c)
	}
	e := r.log.ends()
	if len(e) != 1 || e[0].Reason != "normal" || e[0].SIPCodec != "PCMA" || e[0].CellCodec != "L16" || e[0].Quality == nil {
		t.Errorf("call log %+v", e)
	}
	if len(r.log.started) != 1 || r.log.started[0].Direction != "out" || r.log.started[0].RemoteParty != "0301234" || r.log.answer != 1 {
		t.Errorf("call log start %+v", r.log.started)
	}
}

// A G.722 answer keeps the handset's leg wideband: nothing is transcoded.
func TestHandsetCallWideband(t *testing.T) {
	r := newRig(t, fakeLines{acct: uuid.New(), has: true})
	sc := newFakeSIP(t, sdp.Codec{Name: "G722", PT: 9, Clock: 8000})
	r.m.SIP = &fakeCaller{call: sc}
	h := newFakeHandset(true)
	go r.m.handsetCall(h)
	sc.events <- sip.Event{Kind: sip.EvAnswered}
	waitFor(t, "answered", func() bool { return len(h.actions()) == 1 })
	if got := h.actions(); got[0] != "answered wb" {
		t.Fatalf("the handset was told %q", got)
	}
	h.events <- core.CallEvent{Kind: core.CallMediaUp, Cell: h.info.Cell, Channel: 1, Wideband: true}
	waitFor(t, "MEDIA_OPEN", func() bool { n, _ := r.cells.counts(); return n == 1 })
	if mo := r.cells.opened[0]; mo.Format != wire.FormatG722 || mo.PayloadType != 9 {
		t.Errorf("MEDIA_OPEN %+v", mo)
	}
	h.events <- core.CallEvent{Kind: core.CallReleased}
}

// The speech opens whichever comes last: the answer or the channel.
func TestMediaWaitsForTheAnswer(t *testing.T) {
	r := newRig(t, fakeLines{acct: uuid.New(), has: true})
	sc := newFakeSIP(t, pcma)
	r.m.SIP = &fakeCaller{call: sc}
	h := newFakeHandset(false)
	go r.m.handsetCall(h)
	h.events <- core.CallEvent{Kind: core.CallMediaUp, Cell: h.info.Cell, Channel: 1}
	time.Sleep(50 * time.Millisecond)
	if n, _ := r.cells.counts(); n != 0 {
		t.Fatal("the speech opened before the far end answered")
	}
	sc.events <- sip.Event{Kind: sip.EvAnswered}
	waitFor(t, "MEDIA_OPEN", func() bool { n, _ := r.cells.counts(); return n == 1 })
	h.events <- core.CallEvent{Kind: core.CallReleased}
}

func TestHandsetCallEnds(t *testing.T) {
	for _, c := range []struct {
		name string
		ev   sip.Event
		want string
	}{
		{"busy", sip.Event{Kind: sip.EvEnded, Code: 486}, "release busy"},
		{"declined", sip.Event{Kind: sip.EvEnded, Code: 603}, "release rejected"},
		{"unknown", sip.Event{Kind: sip.EvEnded, Code: 404}, "release unreachable"},
		{"hangup", sip.Event{Kind: sip.EvEnded}, "release normal"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, fakeLines{acct: uuid.New(), has: true})
			sc := newFakeSIP(t, pcma)
			r.m.SIP = &fakeCaller{call: sc}
			h := newFakeHandset(false)
			done := make(chan struct{})
			go func() { r.m.handsetCall(h); close(done) }()
			sc.events <- c.ev
			<-done
			if got := h.actions(); len(got) != 1 || got[0] != c.want {
				t.Errorf("the handset was told %q, want %q", got, c.want)
			}
		})
	}
}

func TestHandsetCallWithoutALineOrDial(t *testing.T) {
	r := newRig(t, fakeLines{})
	h := newFakeHandset(false)
	r.m.handsetCall(h)
	if got := h.actions(); len(got) != 1 || got[0] != "release unreachable" {
		t.Errorf("no line: %q", got)
	}
	r = newRig(t, fakeLines{acct: uuid.New(), has: true})
	r.m.SIP = &fakeCaller{err: errors.New("no route")}
	h = newFakeHandset(false)
	r.m.handsetCall(h)
	if got := h.actions(); len(got) != 1 || got[0] != "release unreachable" {
		t.Errorf("dial failed: %q", got)
	}
}

// A cell that refuses the speech leaves the call silent, not dead.
func TestMediaRefusedByTheCell(t *testing.T) {
	r := newRig(t, fakeLines{acct: uuid.New(), has: true})
	r.cells.ack = wire.MediaNoCapacity
	sc := newFakeSIP(t, pcma)
	r.m.SIP = &fakeCaller{call: sc}
	h := newFakeHandset(false)
	done := make(chan struct{})
	go func() { r.m.handsetCall(h); close(done) }()
	sc.events <- sip.Event{Kind: sip.EvAnswered}
	h.events <- core.CallEvent{Kind: core.CallMediaUp, Cell: h.info.Cell, Channel: 1}
	waitFor(t, "MEDIA_OPEN", func() bool { n, _ := r.cells.counts(); return n == 1 })
	time.Sleep(30 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("the call ended because the cell refused the speech")
	default:
	}
	h.events <- core.CallEvent{Kind: core.CallReleased}
	<-done
	if e := r.log.ends(); len(e) != 1 || e[0].Quality != nil {
		t.Errorf("call log %+v", e)
	}
}

// A cell that never answers MEDIA_OPEN is given up on and its flow closed.
func TestMediaOpenTimeout(t *testing.T) {
	r := newRig(t, fakeLines{acct: uuid.New(), has: true})
	r.cells.silent = true
	sc := newFakeSIP(t, pcma)
	r.m.SIP = &fakeCaller{call: sc}
	h := newFakeHandset(false)
	go r.m.handsetCall(h)
	sc.events <- sip.Event{Kind: sip.EvAnswered}
	h.events <- core.CallEvent{Kind: core.CallMediaUp, Cell: h.info.Cell, Channel: 1}
	waitFor(t, "the flow to be closed again", func() bool { _, c := r.cells.counts(); return c == 1 })
	h.events <- core.CallEvent{Kind: core.CallReleased}
}

func twoHandsets(t *testing.T) (*rig, *fakeRouter, *fakeSIP, []Receiver, <-chan struct{}) {
	t.Helper()
	recv := []Receiver{{Handset: uuid.New(), Line: 1}, {Handset: uuid.New(), Line: 2}}
	r := newRig(t, fakeLines{recv: recv})
	rt := &fakeRouter{}
	r.m.DECT = rt
	sc := newFakeSIP(t, pcma)
	done := make(chan struct{})
	go func() {
		r.m.incoming(callInfo{ID: uuid.New(), AccountID: uuid.New(), Remote: "0301234", RemoteDisplay: "Anna"}, sc)
		close(done)
	}()
	waitFor(t, "both handsets rung", func() bool { rt.mu.Lock(); defer rt.mu.Unlock(); return len(rt.offers) == 2 })
	return r, rt, sc, recv, done
}

// A call to an account rings every handset that has it: the caller hears the
// ring, the first handset to answer takes the call and the others stop, the speech
// opens on the cell that holds the handset, and the caller hanging up ends it.
func TestIncomingFirstAnswerWins(t *testing.T) {
	r, rt, sc, recv, done := twoHandsets(t)
	if c := rt.calls[recv[0].Handset]; c.Number != "0301234" || c.Name != "Anna" || c.Line != 1 || !c.Narrowband {
		t.Errorf("the first handset is offered %+v", c)
	}
	if c := rt.calls[recv[1].Handset]; c.Line != 2 || c.SIP == rt.calls[recv[0].Handset].SIP {
		t.Errorf("the second handset is offered %+v", c)
	}
	a, b := rt.offers[recv[0].Handset], rt.offers[recv[1].Handset]
	b.events <- core.CallEvent{Kind: core.CallRinging}
	waitFor(t, "180", func() bool { return sc.has("ringing") })
	a.events <- core.CallEvent{Kind: core.CallRinging}
	cell := uuid.New()
	b.events <- core.CallEvent{Kind: core.CallAnswered, Cell: cell}
	waitFor(t, "200", func() bool { return sc.has("answer") })
	select {
	case <-a.cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("the other handset keeps ringing")
	}
	b.events <- core.CallEvent{Kind: core.CallMediaUp, Cell: cell, Channel: 1}
	waitFor(t, "MEDIA_OPEN", func() bool { n, _ := r.cells.counts(); return n == 1 })
	b.events <- core.CallEvent{Kind: core.CallDigit, Digit: '#'}
	waitFor(t, "the key", func() bool { return sc.has("dtmf #") })
	sc.events <- sip.Event{Kind: sip.EvEnded}
	<-done
	select {
	case <-b.cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("the answering handset was not released")
	}
	e := r.log.ends()
	if len(e) != 1 || e[0].Reason != "normal" || r.log.started[0].Direction != "in" {
		t.Errorf("call log %+v %+v", e, r.log.started)
	}
	if _, c := r.cells.counts(); c != 1 {
		t.Errorf("the cell's flow was closed %d times", c)
	}
}

// The handset hanging up ends the SIP call.
func TestIncomingHandsetHangsUp(t *testing.T) {
	_, rt, sc, recv, done := twoHandsets(t)
	a := rt.offers[recv[0].Handset]
	a.events <- core.CallEvent{Kind: core.CallAnswered, Cell: uuid.New()}
	waitFor(t, "200", func() bool { return sc.has("answer") })
	a.events <- core.CallEvent{Kind: core.CallReleased, Reason: "normal"}
	<-done
	if !sc.has("hangup") {
		t.Errorf("the caller was not hung up: %q", sc.actions())
	}
}

// Nobody answers: busy everywhere is 486, anything else 480; a caller that gives
// up is a missed call.
func TestIncomingNobodyAnswers(t *testing.T) {
	_, rt, sc, recv, done := twoHandsets(t)
	rt.offers[recv[0].Handset].events <- core.CallEvent{Kind: core.CallBusy}
	rt.offers[recv[1].Handset].events <- core.CallEvent{Kind: core.CallBusy}
	<-done
	if !sc.has("reject 486") {
		t.Errorf("busy everywhere: %q", sc.actions())
	}

	r, rt2, sc2, recv2, done2 := twoHandsets(t)
	rt2.offers[recv2[0].Handset].events <- core.CallEvent{Kind: core.CallBusy}
	rt2.offers[recv2[1].Handset].events <- core.CallEvent{Kind: core.CallReleased, Reason: "normal"}
	<-done2
	if !sc2.has("reject 480") {
		t.Errorf("one released: %q", sc2.actions())
	}
	_ = r

	r3, rt3, sc3, _, done3 := twoHandsets(t)
	_ = rt3
	sc3.events <- sip.Event{Kind: sip.EvEnded, Code: 487}
	<-done3
	if e := r3.log.ends(); len(e) != 1 || e[0].Reason != "missed" {
		t.Errorf("a caller that gave up: %+v", e)
	}
}

func TestIncomingNoHandsetNoRouter(t *testing.T) {
	r := newRig(t, fakeLines{})
	sc := newFakeSIP(t, pcma)
	r.m.incoming(callInfo{ID: uuid.New()}, sc)
	if !sc.has("reject 480") {
		t.Errorf("no handset: %q", sc.actions())
	}
	recv := []Receiver{{Handset: uuid.New(), Line: 1}}
	r = newRig(t, fakeLines{recv: recv})
	r.m.DECT = &fakeRouter{err: map[uuid.UUID]error{recv[0].Handset: core.ErrNoCell}}
	sc = newFakeSIP(t, pcma)
	r.m.incoming(callInfo{ID: uuid.New()}, sc)
	if !sc.has("reject 480") {
		t.Errorf("no cell up: %q", sc.actions())
	}
}

func TestReasonOf(t *testing.T) {
	for code, want := range map[int]string{0: "normal", 200: "normal", 487: "normal", 486: "busy", 600: "busy", 603: "rejected", 403: "rejected",
		404: "unreachable", 503: "unreachable", 408: "unreachable"} {
		if got := reasonOf(sip.Event{Kind: sip.EvEnded, Code: code}); got != want {
			t.Errorf("%d: %q, want %q", code, got, want)
		}
	}
}

func TestCellPayloadType(t *testing.T) {
	for _, c := range []struct {
		plan media.Plan
		want uint8
	}{
		{media.Plan{CellDown: wire.FormatL16_8k, CellUp: wire.FormatL16_8k}, 96},
		{media.Plan{CellDown: wire.FormatG722, CellUp: wire.FormatG722}, 9},
		{media.Plan{CellDown: wire.FormatPCMA, CellUp: wire.FormatPCMA}, 8},
		{media.Plan{CellDown: wire.FormatPCMU, CellUp: wire.FormatPCMU}, 0},
		{media.Plan{CellDown: wire.FormatG722, CellUp: wire.FormatL16_8k}, 96},
	} {
		if got := cellPayloadType(c.plan); got != c.want {
			t.Errorf("%+v: %d, want %d", c.plan, got, c.want)
		}
	}
}

// The cell ending a flow cancels the pipeline; the call goes on.
func TestMediaClosedByTheCell(t *testing.T) {
	r := newRig(t, fakeLines{acct: uuid.New(), has: true})
	sc := newFakeSIP(t, pcma)
	r.m.SIP = &fakeCaller{call: sc}
	h := newFakeHandset(false)
	go r.m.handsetCall(h)
	sc.events <- sip.Event{Kind: sip.EvAnswered}
	h.events <- core.CallEvent{Kind: core.CallMediaUp, Cell: h.info.Cell, Channel: 1}
	waitFor(t, "MEDIA_OPEN", func() bool { n, _ := r.cells.counts(); return n == 1 })
	id := r.cells.opened[0].MediaID
	r.m.MediaClosed(h.info.Cell, wire.MediaClosedMsg{MediaID: id, Reason: wire.MediaClosedByChip})
	r.m.mu.Lock()
	s := r.m.sessions[id]
	r.m.mu.Unlock()
	select {
	case <-s.done:
	case <-time.After(3 * time.Second):
		t.Fatal("the pipeline still runs")
	}
	h.events <- core.CallEvent{Kind: core.CallReleased}
}

func TestSIPPtime(t *testing.T) {
	for ms, want := range map[int]time.Duration{0: 0, 10: 10 * time.Millisecond, 20: 20 * time.Millisecond, 30: 30 * time.Millisecond,
		40: 40 * time.Millisecond, 25: 0, 60: 0, 15: 0, -1: 0} {
		if got := sipPtime(ms); got != want {
			t.Errorf("%d ms: %v, want %v", ms, got, want)
		}
	}
}

type fakeCallLists struct {
	mu  sync.Mutex
	got [][]uuid.UUID
}

func (f *fakeCallLists) CallListsChanged(hids ...uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, hids)
}

// When a handset's call has ended and is in the call log, the core is told whose call lists it is in.
func TestFinishedCallTellsTheCallLists(t *testing.T) {
	r := newRig(t, fakeLines{acct: uuid.New(), has: true})
	cl := &fakeCallLists{}
	r.m.CallLists = cl
	sc := newFakeSIP(t, pcma)
	r.m.SIP = &fakeCaller{call: sc}
	h := newFakeHandset(false)
	done := make(chan struct{})
	go func() { r.m.handsetCall(h); close(done) }()
	sc.events <- sip.Event{Kind: sip.EvEnded}
	<-done
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if len(cl.got) != 1 || len(cl.got[0]) != 1 {
		t.Errorf("told %v", cl.got)
	}
}

// With MediaFromLink a cell is sent to the address it connected to, not to the one address of the setting:
// cells on different networks each get one they can reach.
func TestMediaOpenGoesToTheAddressTheCellConnectedTo(t *testing.T) {
	r := newRig(t, fakeLines{acct: uuid.New(), has: true})
	public := netip.MustParseAddr("2001:db8::7")
	r.cells.status.LocalAddr = public
	r.m.MediaFromLink = true
	sc := newFakeSIP(t, pcma)
	r.m.SIP = &fakeCaller{call: sc}
	h := newFakeHandset(true)
	go r.m.handsetCall(h)
	sc.events <- sip.Event{Kind: sip.EvRinging}
	waitFor(t, "ringing", func() bool { return len(h.actions()) == 1 })
	sc.events <- sip.Event{Kind: sip.EvAnswered}
	waitFor(t, "answered", func() bool { return len(h.actions()) == 2 })
	h.events <- core.CallEvent{Kind: core.CallMediaUp, Cell: h.info.Cell, Handle: 0, Channel: 1}
	waitFor(t, "MEDIA_OPEN", func() bool { n, _ := r.cells.counts(); return n == 1 })
	if got := r.cells.opened[0].CtlAddr.Addr(); got != public {
		t.Errorf("the cell was told to send speech to %v, want the address it connected to, %v", got, public)
	}
	h.events <- core.CallEvent{Kind: core.CallReleased, Reason: "hs"}
}
