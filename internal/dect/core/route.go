// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Routing a call to a handset (docs/roaming.md §4, point 4). A handset may be
// on any cell that is up. The cell it was last heard on is paged alone first;
// when nothing answers within PageDelay (<LCE.03>) the next RingWaveSize cells
// are paged too, and so on every PageDelay up to RingMaxCells in all, the
// first to answer takes the call and the legs on the other cells are released.
// The others are the cells the handset was heard on in the last week, then the
// nearest to the one it was last on (Records.CellOrder): paging every cell of
// a system of hundreds would flood their radios and fill their tables. A cell that does not hold the handset in its table takes it in from
// the database when its leg is offered (roaming.go, ensure).
//
// The SIP side (internal/bridge) uses it: Ring starts an offer, the events
// come back on the returned Offer, Cancel is the caller hanging up. Each leg is
// one `call-in` on one cell's stack (openDECTcell docs/bridge-interface.md §2.2). A
// stack is told it carries external calls (`bridge`) when it starts, so a
// number a handset dials is a `call-out` (outgoing.go), not a local connect.

// Errors of Ring.
var (
	// ErrNoCell: no cell has its radio on.
	ErrNoCell = errors.New("core: no cell is up")
	// ErrNoHandset: there is no such handset.
	ErrNoHandset = errors.New("core: no such handset")
)

// Call is what the SIP side asks for.
type Call struct {
	// SIP names the call for the SIP side; it is unique among the live calls.
	// Empty: one is made up.
	SIP string
	// Line is the line (1 or 2) the call came in on.
	Line int
	// Number and Name are the caller's, shown on the handset; Number is
	// digits, + * # only, anything else is dropped.
	Number, Name string
	// Narrowband: the caller offered no G.722.
	Narrowband bool
	// Internal: a call from another handset of the same account (the `call-in int=1` of API
	// 153): Number is the caller's internal number and Name its name; there is no line.
	Internal bool
}

// CallEventKind says what happened to a call.
type CallEventKind string

// The events an Offer carries: the first of Ringing; Answered once, with the
// cell that took the call; Released at the end, whoever ended it; Busy when no
// leg could be offered (every handset is in a call, no audio channel is free).
const (
	CallRinging  CallEventKind = "ringing"
	CallAnswered CallEventKind = "answered"
	CallReleased CallEventKind = "released"
	CallBusy     CallEventKind = "busy"
)

// The events of a call that is on: the handset's keys and holds, and the
// speech. CallMediaUp says the call's audio channel on the chip is bound
// (Handle, Channel and Wideband say how); CallMediaDown that it is released.
const (
	CallDigit     CallEventKind = "digit"
	CallHold      CallEventKind = "hold"
	CallResume    CallEventKind = "resume"
	CallMediaUp   CallEventKind = "media-up"
	CallMediaDown CallEventKind = "media-down"
)

// CallEvent is one event of an offer. Cell and Call (the stack's call number)
// say which leg it is, when it is one.
type CallEvent struct {
	Kind   CallEventKind
	Cell   uuid.UUID
	Call   string
	Reason string
	// CallDigit: the key; CallMediaUp: the link handle, the chip's audio
	// channel and whether the air codec is G.722.
	Digit    rune
	Handle   uint8
	Channel  uint8
	Wideband bool
}

// Offer is a call offered to a handset. Events closes after Released or Busy.
type Offer struct {
	SIP    string
	Events <-chan CallEvent

	core *Core
	rt   *route
}

// Cancel is the caller hanging up: the legs still ringing are released.
func (o *Offer) Cancel() { o.core.calls.finish(o.core, o.rt, CallReleased, "normal", true) }

// leg is the call on one cell.
type leg struct {
	cell uuid.UUID
	call string // the stack's number for it, once it answered call-in
	// asked: its call-in is sent; failed: the stack said busy or error;
	// dead: the leg is over (or, with no call number yet, to be released
	// the moment the stack gives one)
	asked, failed, dead bool
	// noResp: the stack gave up the page, nobody answered it (`released by=base` before it rang)
	noResp bool
}

// route is one offer's state.
type route struct {
	call   Call
	hid    uuid.UUID
	events chan CallEvent
	legs   map[uuid.UUID]*leg // by cell
	waves  [][]*leg           // the legs of the later waves, each paged PageDelay after the one before
	timer  *time.Timer
	winner *leg
	rang   bool
	over   bool
	closed bool // events is closed: the caller holds rs.mu when it sets or reads this
}

// noOneAnswered: every cell the handset was paged on gave up the page. A cell that was not asked
// (down) or did not say so (an error) is not the handset's silence.
func (rt *route) noOneAnswered() bool {
	asked := false
	for _, l := range rt.legs {
		if !l.asked {
			continue
		}
		if !l.noResp {
			return false
		}
		asked = true
	}
	return asked
}

// notePageFailed records that nobody answered a page for the handset: it is shown as unresponsive.
func (c *Core) notePageFailed(hid uuid.UUID) {
	ctx, cancel := context.WithTimeout(c.ctx, storeTimeout)
	defer cancel()
	if err := c.Records.PageFailed(ctx, hid, time.Now()); err != nil {
		c.Log.Warn("a handset that did not answer its page could not be noted", "handset", hid, "err", err)
	}
}

// pending: a call-in is out whose answer has not come. The route stays
// known until it does, so a leg released early can be released for real.
func (rt *route) pending() bool {
	for _, l := range rt.legs {
		if l.asked && l.call == "" && !l.failed {
			return true
		}
	}
	return false
}

// routes is the router's state: the live offers by their SIP name.
type routes struct {
	mu   sync.Mutex
	live map[string]*route
	out  map[outKey]*Outgoing // handsets' calls (outgoing.go)
	// media: where a link's `audio` events go, by cell and link handle
	media map[mediaKey]callSink
}

func (c *Core) pageDelay() time.Duration {
	if c.PageDelay > 0 {
		return c.PageDelay
	}
	return DefaultPageDelay
}

// readyCells lists the cells that have their radio on.
func (c *Core) readyCells() []uuid.UUID {
	c.mu.Lock()
	runs := make([]*cellRun, 0, len(c.cells))
	for _, r := range c.cells {
		runs = append(runs, r)
	}
	c.mu.Unlock()
	var up []uuid.UUID
	for _, r := range runs {
		r.pub.Lock()
		ready := r.ready
		r.pub.Unlock()
		if ready {
			up = append(up, r.id)
		}
	}
	return up
}

// ringOrder is the cells a call to the handset is offered to, in the order of
// the waves: the one it was last heard on first (if it is up), then those
// Records.CellOrder ranks, at most RingMaxCells in all.
func (c *Core) ringOrder(ctx context.Context, hid, last uuid.UUID, cells []uuid.UUID) []uuid.UUID {
	limit := c.ringMaxCells()
	ordered := cells
	if len(cells) > 1 {
		o, err := c.Records.CellOrder(ctx, hid, last, cells, limit)
		if err != nil {
			c.Log.Warn("the cells could not be ranked for a call: they are paged in any order", "handset", hid, "err", err)
		} else if len(o) > 0 {
			ordered = o
		}
	}
	out := make([]uuid.UUID, 0, min(len(ordered)+1, limit))
	if slices.Contains(cells, last) {
		out = append(out, last)
	}
	for _, id := range ordered {
		if id != last {
			out = append(out, id)
		}
	}
	return out[:min(len(out), limit)]
}

func (c *Core) ringWaveSize() int {
	if c.RingWaveSize > 0 {
		return c.RingWaveSize
	}
	return DefaultRingWaveSize
}

func (c *Core) ringMaxCells() int {
	if c.RingMaxCells > 0 {
		return c.RingMaxCells
	}
	return DefaultRingMaxCells
}

// Ring offers a call to a handset where it is.
func (c *Core) Ring(ctx context.Context, hid uuid.UUID, call Call) (*Offer, error) {
	known, err := c.Records.HandsetKnown(ctx, hid)
	if err != nil {
		return nil, err
	}
	if !known {
		return nil, ErrNoHandset
	}
	cells := c.readyCells()
	if len(cells) == 0 {
		return nil, ErrNoCell
	}
	last, err := c.Records.LastSeenCell(ctx, hid)
	if err != nil {
		c.Log.Warn("where the handset was last seen is not known: every cell is paged", "handset", hid, "err", err)
		last = uuid.Nil
	}
	call.Number = cleanNumber(call.Number)
	call.Name = cleanName(call.Name)
	if call.Line != 2 {
		call.Line = 1
	}

	c.calls.mu.Lock()
	if c.calls.live == nil {
		c.calls.live = map[string]*route{}
	}
	if call.SIP == "" {
		call.SIP = "r" + uuid.NewString()[:8]
	}
	if c.calls.live[call.SIP] != nil {
		c.calls.mu.Unlock()
		return nil, fmt.Errorf("core: call %q is already being offered", call.SIP)
	}
	rt := &route{call: call, hid: hid, events: make(chan CallEvent, 16), legs: map[uuid.UUID]*leg{}}
	c.calls.live[call.SIP] = rt
	// wave one: the last cell alone, or the first few if it is not up; the
	// others follow in waves (ringOrder)
	order := c.ringOrder(ctx, hid, last, cells)
	var first []*leg
	var rest []*leg
	for _, id := range order {
		l := &leg{cell: id}
		rt.legs[id] = l
		if id == last && first == nil {
			first = []*leg{l}
		} else {
			rest = append(rest, l)
		}
	}
	size := c.ringWaveSize()
	if first == nil {
		n := min(size, len(rest))
		first, rest = rest[:n], rest[n:]
	}
	for len(rest) > 0 {
		n := min(size, len(rest))
		rt.waves = append(rt.waves, rest[:n])
		rest = rest[n:]
	}
	if len(rt.waves) > 0 {
		rt.timer = time.AfterFunc(c.pageDelay(), func() { c.calls.nextWave(c, rt) })
	}
	for _, l := range first {
		l.asked = true
	}
	c.calls.mu.Unlock()
	for _, l := range first {
		c.offerLeg(rt, l)
	}
	return &Offer{SIP: call.SIP, Events: rt.events, core: c, rt: rt}, nil
}

// offerLeg sends the call-in to the leg's cell. The reply and the stack's
// events come back through bridgeLine.
func (c *Core) offerLeg(rt *route, l *leg) {
	var b strings.Builder
	fmt.Fprintf(&b, "call-in line=%d hs={hs} number=%s", rt.call.Line, rt.call.Number)
	if rt.call.Internal {
		b.WriteString(" int=1")
	}
	if rt.call.Name != "" {
		fmt.Fprintf(&b, " name=%q", rt.call.Name)
	}
	if rt.call.Narrowband {
		b.WriteString(" codec=nb")
	}
	fmt.Fprintf(&b, " sip=%s", rt.call.SIP)
	c.post(l.cell, event{kind: evCtl, line: b.String(), sip: rt.call.SIP, hid: rt.hid})
}

// nextWave pages the next wave of cells, if the call is still unanswered, and
// arms the one after it.
func (rs *routes) nextWave(c *Core, rt *route) {
	rs.mu.Lock()
	if rt.over || rt.winner != nil || len(rt.waves) == 0 {
		rs.mu.Unlock()
		return
	}
	wave := rt.waves[0]
	rt.waves = rt.waves[1:]
	for _, l := range wave {
		l.asked = true
	}
	if rt.timer != nil {
		rt.timer.Stop()
	}
	if len(rt.waves) > 0 {
		rt.timer = time.AfterFunc(c.pageDelay(), func() { c.calls.nextWave(c, rt) })
	}
	rs.mu.Unlock()
	for _, l := range wave {
		c.offerLeg(rt, l)
	}
}

// finish ends the offer: the legs that are live are released if release is
// set, and Events gets kind and reason and closes. Once.
func (rs *routes) finish(c *Core, rt *route, kind CallEventKind, reason string, release bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rt.over {
		return
	}
	rt.over = true
	rt.waves = nil
	if rt.timer != nil {
		rt.timer.Stop()
	}
	if release {
		for _, l := range rt.legs {
			if !l.failed {
				c.releaseLeg(l, reason)
			}
		}
	}
	rt.emit(c, CallEvent{Kind: kind, Reason: reason})
	close(rt.events)
	rt.closed = true
	rs.reap(rt)
}

// reap forgets an offer that is over and has no answer outstanding. The
// caller holds rs.mu.
func (rs *routes) reap(rt *route) {
	if rt.over && !rt.pending() {
		delete(rs.live, rt.call.SIP)
	}
}

// releaseLeg tells a cell's stack the far end is gone. A leg that has not got
// its call number yet is released when the stack gives one. The caller holds
// rs.mu.
func (c *Core) releaseLeg(l *leg, reason string) {
	if l.dead {
		return
	}
	l.dead = true
	if l.call != "" {
		c.post(l.cell, event{kind: evCtl, line: fmt.Sprintf("remote-released call=%s reason=%s", l.call, reason)})
	}
}

// emit hands an event to the offer's reader. The speech of a call is released
// after the offer ended (the far end hung up first), and that is not an error:
// the reader has gone. The caller holds rs.mu.
func (rt *route) emit(c *Core, ev CallEvent) {
	if rt.closed {
		return
	}
	select {
	case rt.events <- ev:
	default:
		c.Log.Error("a call event was dropped: nobody reads the offer's events", "sip", rt.call.SIP, "kind", ev.Kind)
	}
}

// legFailed: the cell's stack could not take the command for the call.
func (c *Core) legFailed(cell uuid.UUID, sip string) {
	c.bridgeLine(cell, "error verb=call-in sip="+sip)
}

// bridgeLine takes one line the cell's stack wrote for the bridge: the
// answer to a call-in, or an event of a call.
func (c *Core) bridgeLine(cell uuid.UUID, line string) {
	verb, kv := parseBridgeLine(line)
	rs := &c.calls
	rs.mu.Lock()
	defer rs.mu.Unlock()
	switch verb {
	case "audio":
		rs.audioUp(c, cell, kv)
		return
	case "audio-down":
		rs.audioDown(c, cell, kv)
		return
	case "digit", "hold", "resume":
		if sink := rs.find(cell, kv["call"]); sink != nil {
			ev := CallEvent{Cell: cell, Call: kv["call"]}
			switch verb {
			case "digit":
				ev.Kind = CallDigit
				ev.Digit, _ = utf8.DecodeRuneInString(kv["d"])
			case "hold":
				ev.Kind = CallHold
			default:
				ev.Kind = CallResume
			}
			sink.emit(c, ev)
		}
		return
	}
	if o := rs.out[outKey{cell, kv["call"]}]; o != nil && verb != "" {
		rs.outgoingLine(o, verb, kv)
		return
	}
	var rt *route
	var l *leg
	if sip := kv["sip"]; sip != "" {
		if rt = rs.live[sip]; rt != nil {
			l = rt.legs[cell]
		}
	}
	if l == nil {
		// an event of a call: by the stack's number for it
		for _, r := range rs.live {
			if x := r.legs[cell]; x != nil && x.call != "" && x.call == kv["call"] {
				rt, l = r, x
			}
		}
	}
	if rt == nil || l == nil {
		return
	}
	if rt.over {
		// an answer that came after the offer ended: the leg is released for real
		switch verb {
		case "call-in-ok":
			l.call = kv["call"]
			c.post(l.cell, event{kind: evCtl, line: fmt.Sprintf("remote-released call=%s reason=normal", l.call)})
		case "busy", "error":
			l.failed = true
		}
		rs.reap(rt)
		return
	}
	switch verb {
	case "call-in-ok":
		l.call = kv["call"]
		if l.dead { // released while the stack was still deciding
			c.post(l.cell, event{kind: evCtl, line: fmt.Sprintf("remote-released call=%s reason=normal", l.call)})
		}
	case "busy", "error":
		l.failed, l.dead = true, true
		rs.legEnded(c, rt, CallBusy, kv["why"])
	case "ringing":
		if !rt.rang {
			rt.rang = true
			rt.emit(c, CallEvent{Kind: CallRinging, Cell: cell, Call: l.call})
		}
	case "answered":
		if rt.winner != nil {
			return
		}
		rt.winner = l
		if rt.timer != nil {
			rt.timer.Stop()
		}
		rt.waves = nil
		for _, o := range rt.legs {
			if o != l {
				c.releaseLeg(o, "normal")
			}
		}
		rt.emit(c, CallEvent{Kind: CallAnswered, Cell: cell, Call: l.call})
	case "released":
		l.dead = true
		if kv["by"] == "base" && !rt.rang {
			l.noResp = true
		}
		if rt.winner == l {
			go rs.finish(c, rt, CallReleased, kv["reason"], false)
			return
		}
		rs.legEnded(c, rt, CallReleased, kv["reason"])
	}
}

// legEnded: a leg that was not the answered one is over. When none is left
// and no later wave is coming, the offer is: busy if no leg ever rang, else
// released. The caller holds rs.mu.
func (rs *routes) legEnded(c *Core, rt *route, kind CallEventKind, reason string) {
	if rt.winner != nil {
		return
	}
	for _, l := range rt.legs {
		if l.asked && !l.dead {
			return
		}
	}
	if len(rt.waves) > 0 {
		// nothing paged so far is left: the next wave is paged now
		go rs.nextWave(c, rt)
		return
	}
	if kind == CallBusy && !rt.rang {
		go rs.finish(c, rt, CallBusy, reason, false)
		return
	}
	if !rt.rang && rt.noOneAnswered() {
		go c.notePageFailed(rt.hid)
	}
	go rs.finish(c, rt, CallReleased, reason, false)
}

// parseBridgeLine splits `verb key=value key="a value"` ...
func parseBridgeLine(line string) (verb string, kv map[string]string) {
	kv = map[string]string{}
	var toks []string
	var cur strings.Builder
	in := false
	started := false
	for _, r := range line {
		switch {
		case r == '"':
			in = !in
			started = true
		case r == ' ' && !in:
			if started {
				toks = append(toks, cur.String())
				cur.Reset()
				started = false
			}
		default:
			cur.WriteRune(r)
			started = true
		}
	}
	if started {
		toks = append(toks, cur.String())
	}
	if len(toks) == 0 {
		return "", kv
	}
	for _, t := range toks[1:] {
		if k, v, ok := strings.Cut(t, "="); ok {
			kv[k] = v
		}
	}
	return toks[0], kv
}

// cleanNumber keeps what a dialled number is made of; the stack's command line
// is not a place for anything else.
func cleanNumber(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= '0' && r <= '9') || r == '+' || r == '*' || r == '#' {
			return r
		}
		return -1
	}, s)
}

// cleanName drops what would end the name's quoting or the line.
func cleanName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < ' ' || r == '"' || r == '\\' || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) > 32 {
		s = s[:32]
	}
	return s
}
