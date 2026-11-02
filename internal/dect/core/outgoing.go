// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"fmt"
	"strconv"

	"github.com/google/uuid"
)

// A handset's call (docs/bridge-interface.md in openDECTcell: `call-out`). With a
// stack in bridge mode a dialled number is not connected locally: the stack
// says `call-out call=N hs=S line=L codec=wb|nb number=...` and waits for the
// far end. Core.Originate takes the call to the SIP side; it answers through
// the Outgoing's methods, which are the stack's remote-ringing,
// remote-answered and remote-released.

// outKey names a handset's call: the cell's stack and its number for it.
type outKey struct {
	cell uuid.UUID
	call string
}

// mediaKey names a link of a cell's chip.
type mediaKey struct {
	cell   uuid.UUID
	handle uint8
}

// callSink is where the events of a call go: an offer (a call to a handset)
// or an Outgoing (a call from one).
type callSink interface {
	emit(c *Core, ev CallEvent)
}

// OutCall is what a cell's stack reports of a handset's call.
type OutCall struct {
	Cell uuid.UUID
	Call string // the stack's number for the call
	// Handset is the handset that dialled; uuid.Nil when its record number
	// is not one this controller knows.
	Handset uuid.UUID
	Slot    uint
	Line    int // 1 or 2: the line the handset chose
	Number  string
	// Wideband: the handset's air codec is G.722 (codec=wb).
	Wideband bool
	// Internal: the handset called another handset (call class 9, `class=int`); Number is what
	// it dialled and Target the handset that is, uuid.Nil if there is none it may call (no such
	// number, or another account's: the core never resolves one).
	Internal bool
	Target   uuid.UUID
}

// Outgoing is a call a handset placed, offered to Core.Originate. Events
// carries the handset's side of it: its keys, holds, the speech (CallMediaUp
// and CallMediaDown) and CallReleased when the handset hangs up or the call is
// lost; it closes after CallReleased.
type Outgoing struct {
	OutCall
	Events <-chan CallEvent

	core *Core
	ev   chan CallEvent
	// over: the call is finished; closed: Events is closed. Both guarded by
	// core.calls.mu, which every emit and finish runs under.
	over, closed bool
}

func (o *Outgoing) emit(c *Core, ev CallEvent) {
	if o.closed {
		return
	}
	select {
	case o.ev <- ev:
	default:
		c.Log.Error("a call event was dropped: nobody reads the call's events", "call", o.Call, "kind", ev.Kind)
	}
}

// Ringing tells the handset the far end rings (the ring-back tone).
func (o *Outgoing) Ringing() { o.send("remote-ringing call=" + o.Call) }

// Answered tells the handset the far end answered. Narrowband: the far end
// took a narrowband codec, so the handset's leg is narrowband too and nothing
// is transcoded.
func (o *Outgoing) Answered(narrowband bool) {
	line := "remote-answered call=" + o.Call
	if narrowband {
		line += " codec=nb"
	}
	o.send(line)
}

// Release ends the call from the far side: reason is busy, normal,
// unreachable or rejected. The Events channel closes.
func (o *Outgoing) Release(reason string) {
	rs := &o.core.calls
	rs.mu.Lock()
	if o.over {
		rs.mu.Unlock()
		return
	}
	o.over = true
	delete(rs.out, outKey{o.Cell, o.Call})
	o.finish(reason)
	rs.mu.Unlock()
	o.core.post(o.Cell, event{kind: evCtl, line: fmt.Sprintf("remote-released call=%s reason=%s", o.Call, reason)})
}

func (o *Outgoing) send(line string) {
	rs := &o.core.calls
	rs.mu.Lock()
	over := o.over
	rs.mu.Unlock()
	if !over {
		o.core.post(o.Cell, event{kind: evCtl, line: line})
	}
}

// finish closes the event stream with a CallReleased. The caller holds
// core.calls.mu.
func (o *Outgoing) finish(reason string) {
	o.emit(o.core, CallEvent{Kind: CallReleased, Cell: o.Cell, Call: o.Call, Reason: reason})
	o.closed = true
	close(o.ev)
}

// callOut takes a stack's `call-out`. hid is the handset the stack's record
// number belongs to, if the cell knows it.
func (c *Core) callOut(cell uuid.UUID, kv map[string]string, hid uuid.UUID, target uuid.UUID) {
	slot, _ := strconv.ParseUint(kv["hs"], 10, 8)
	line, _ := strconv.Atoi(kv["line"])
	if line != 2 {
		line = 1
	}
	oc := OutCall{Cell: cell, Call: kv["call"], Handset: hid, Slot: uint(slot), Line: line,
		Number: cleanNumber(kv["number"]), Wideband: kv["codec"] == "wb", Internal: kv["class"] == "int", Target: target}
	if oc.Call == "" {
		return
	}
	ev := make(chan CallEvent, 16)
	o := &Outgoing{OutCall: oc, Events: ev, core: c, ev: ev}
	rs := &c.calls
	rs.mu.Lock()
	if rs.out == nil {
		rs.out = map[outKey]*Outgoing{}
	}
	rs.out[outKey{cell, oc.Call}] = o
	rs.mu.Unlock()
	if c.Originate == nil {
		c.Log.Warn("a handset's call is released: no SIP side is attached", "cell", cell, "number", oc.Number)
		go o.Release("unreachable")
		return
	}
	go c.Originate(o)
}

// find says where the events of the stack's call go, or nil. The caller
// holds rs.mu.
func (rs *routes) find(cell uuid.UUID, call string) callSink {
	if call == "" {
		return nil
	}
	if o := rs.out[outKey{cell, call}]; o != nil {
		return o
	}
	for _, rt := range rs.live {
		if l := rt.legs[cell]; l != nil && l.call == call && !l.dead && !rt.over {
			return rt
		}
	}
	return nil
}

// outgoingLine is a line the stack wrote about a handset's call. The caller
// holds rs.mu.
func (rs *routes) outgoingLine(o *Outgoing, verb string, kv map[string]string) {
	if verb != "released" {
		return
	}
	// the handset hung up, or the call failed on the DECT side
	if o.over {
		return
	}
	o.over = true
	delete(rs.out, outKey{o.Cell, o.Call})
	reason := "hs"
	if r := kv["reason"]; r != "" && kv["by"] != "hs" {
		reason = r
	}
	o.finish(reason)
}

// audioUp is an `audio` line: the call's speech is bound on a chip channel.
// The caller holds rs.mu.
func (rs *routes) audioUp(c *Core, cell uuid.UUID, kv map[string]string) {
	sink := rs.find(cell, kv["call"])
	h, _ := strconv.ParseUint(kv["handle"], 10, 8)
	ch, _ := strconv.ParseUint(kv["channel"], 10, 8)
	if sink == nil {
		return
	}
	if rs.media == nil {
		rs.media = map[mediaKey]callSink{}
	}
	rs.media[mediaKey{cell, uint8(h)}] = sink
	sink.emit(c, CallEvent{Kind: CallMediaUp, Cell: cell, Call: kv["call"], Handle: uint8(h), Channel: uint8(ch), Wideband: kv["wideband"] == "1"})
}

// audioDown is an `audio-down` line: the link's speech is released.
func (rs *routes) audioDown(c *Core, cell uuid.UUID, kv map[string]string) {
	h, _ := strconv.ParseUint(kv["handle"], 10, 8)
	k := mediaKey{cell, uint8(h)}
	sink := rs.media[k]
	if sink == nil {
		return
	}
	delete(rs.media, k)
	sink.emit(c, CallEvent{Kind: CallMediaDown, Cell: cell, Handle: uint8(h)})
}

// cellGone ends what ran on a cell whose stack is gone: the handsets' calls,
// the speech, and the legs of offers.
func (c *Core) cellGone(cell uuid.UUID) {
	rs := &c.calls
	rs.mu.Lock()
	defer rs.mu.Unlock()
	for k, o := range rs.out {
		if k.cell == cell && !o.over {
			o.over = true
			delete(rs.out, k)
			o.finish("cell")
		}
	}
	for k, sink := range rs.media {
		if k.cell == cell {
			sink.emit(c, CallEvent{Kind: CallMediaDown, Cell: cell, Handle: k.handle})
			delete(rs.media, k)
		}
	}
	for _, rt := range rs.live {
		l := rt.legs[cell]
		if l == nil || l.dead || rt.over {
			continue
		}
		l.dead = true
		if rt.winner == l {
			go rs.finish(c, rt, CallReleased, "cell", false)
			continue
		}
		rs.legEnded(c, rt, CallReleased, "cell")
	}
}
