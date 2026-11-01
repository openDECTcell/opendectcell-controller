// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"testing"
	"time"

	"github.com/openDECTcell/opendectcell-controller/internal/dect/stack"
)

func nextOut(t *testing.T, o *Outgoing) (CallEvent, bool) {
	t.Helper()
	select {
	case ev, ok := <-o.Events:
		return ev, ok
	case <-time.After(3 * time.Second):
		t.Fatal("no call event")
	}
	return CallEvent{}, false
}

// A handset dials: the stack says call-out, the SIP side gets an Outgoing
// that names the handset, the line, the number and the air codec, answers it
// (ringing, answered), sees the keys and the speech, and the handset hanging
// up ends it.
func TestHandsetCallGoesToTheSIPSide(t *testing.T) {
	rr := newRouteRig(t, "a")
	got := make(chan *Outgoing, 1)
	rr.core.Originate = func(o *Outgoing) { got <- o }
	env := rr.stack(0).env
	env.Event(stack.ToBridge, "call-out call=5 hs=7 line=2 codec=wb number=0301234567")
	var o *Outgoing
	select {
	case o = <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("the call was not offered to the SIP side")
	}
	if o.Cell != rr.a || o.Call != "5" || o.Handset != rr.h.ID || o.Slot != 7 || o.Line != 2 || o.Number != "0301234567" || !o.Wideband {
		t.Fatalf("offered %+v", o.OutCall)
	}
	o.Ringing()
	o.Answered(true)
	lines := rr.waitLines(t, 0, 2)
	if lines[0] != "remote-ringing call=5" || lines[1] != "remote-answered call=5 codec=nb" {
		t.Errorf("the stack got %q", lines)
	}
	env.Event(stack.ToBridge, "digit call=5 d=7")
	if ev, _ := nextOut(t, o); ev.Kind != CallDigit || ev.Digit != '7' {
		t.Errorf("digit %+v", ev)
	}
	env.Event(stack.ToBridge, "hold call=5")
	if ev, _ := nextOut(t, o); ev.Kind != CallHold {
		t.Errorf("hold %+v", ev)
	}
	env.Event(stack.ToBridge, "resume call=5")
	if ev, _ := nextOut(t, o); ev.Kind != CallResume {
		t.Errorf("resume %+v", ev)
	}
	env.Event(stack.ToBridge, "audio call=5 handle=0 channel=2 wideband=1")
	ev, _ := nextOut(t, o)
	if ev.Kind != CallMediaUp || ev.Handle != 0 || ev.Channel != 2 || !ev.Wideband || ev.Cell != rr.a || ev.Call != "5" {
		t.Errorf("media up %+v", ev)
	}
	env.Event(stack.ToBridge, "audio-down handle=0")
	if ev, _ := nextOut(t, o); ev.Kind != CallMediaDown || ev.Handle != 0 {
		t.Errorf("media down %+v", ev)
	}
	env.Event(stack.ToBridge, "released call=5 by=hs reason=normal")
	if ev, _ := nextOut(t, o); ev.Kind != CallReleased || ev.Reason != "hs" {
		t.Errorf("release %+v", ev)
	}
	if _, ok := <-o.Events; ok {
		t.Error("events not closed after the release")
	}
	// the call is over: later commands are not sent
	o.Ringing()
	o.Release("busy")
	time.Sleep(20 * time.Millisecond)
	if n := len(rr.lines(0)); n != 2 {
		t.Errorf("the stack got %d commands, want 2: %q", n, rr.lines(0))
	}
}

// The far end hangs up: the stack is told, the events close.
func TestOutgoingReleasedByTheFarEnd(t *testing.T) {
	rr := newRouteRig(t, "a")
	got := make(chan *Outgoing, 1)
	rr.core.Originate = func(o *Outgoing) { got <- o }
	rr.stack(0).env.Event(stack.ToBridge, "call-out call=6 hs=7 line=1 codec=nb number=112")
	o := <-got
	if o.Wideband {
		t.Error("codec=nb is not wideband")
	}
	o.Release("busy")
	if got := rr.waitLines(t, 0, 1); got[0] != "remote-released call=6 reason=busy" {
		t.Errorf("the stack got %q", got)
	}
	if ev, _ := nextOut(t, o); ev.Kind != CallReleased || ev.Reason != "busy" {
		t.Errorf("event %+v", ev)
	}
	if _, ok := <-o.Events; ok {
		t.Error("events not closed")
	}
}

// With no SIP side attached a handset's call is released as unreachable.
func TestHandsetCallWithoutASIPSide(t *testing.T) {
	rr := newRouteRig(t, "a")
	rr.stack(0).env.Event(stack.ToBridge, "call-out call=6 hs=7 line=1 codec=nb number=112")
	if got := rr.waitLines(t, 0, 1); got[0] != "remote-released call=6 reason=unreachable" {
		t.Errorf("the stack got %q", got)
	}
}

// A cell that goes ends its handsets' calls.
func TestCellGoneEndsItsCalls(t *testing.T) {
	rr := newRouteRig(t, "a")
	got := make(chan *Outgoing, 1)
	rr.core.Originate = func(o *Outgoing) { got <- o }
	rr.stack(0).env.Event(stack.ToBridge, "call-out call=6 hs=7 line=1 codec=nb number=112")
	o := <-got
	rr.core.CellGone(rr.a)
	if ev, _ := nextOut(t, o); ev.Kind != CallReleased || ev.Reason != "cell" {
		t.Errorf("event %+v", ev)
	}
}
