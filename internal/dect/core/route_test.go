// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/cell"
	"github.com/openDECTcell/opendectcell-controller/internal/dect/stack"
)

// routeRig is two cells that both hold one handset, each with a stack that
// answers a call-in like dect-stubd: call-in-ok with the number it gave the call.
type routeRig struct {
	*rig
	a, b uuid.UUID
	h    Handset
	// what a stack says to a call-in; the default is call-in-ok
	reply func(stackIdx int, sip string) string
}

func newRouteRig(t *testing.T, last string) *routeRig {
	t.Helper()
	h := handset(7, owner)
	recs := &fakeRecords{hs: []Handset{h}, lastOn: map[uuid.UUID]uuid.UUID{}}
	rr := &routeRig{h: h, a: uuid.New(), b: uuid.New()}
	switch last {
	case "a":
		recs.lastOn[h.ID] = rr.a
	case "b":
		recs.lastOn[h.ID] = rr.b
	}
	n := 0
	rr.rig = newRig(t, recs, func(f *fakeStack) {
		idx := n
		n++
		f.onCtl = func(env stack.Env, line string) {
			if !strings.HasPrefix(line, "call-in ") {
				return
			}
			sip := line[strings.Index(line, "sip=")+4:]
			if rr.reply != nil {
				env.Event(stack.ToAsker, rr.reply(idx, sip))
				return
			}
			env.Event(stack.ToAsker, "call-in-ok call="+string(rune('7'+idx))+" sip="+sip)
		}
	})
	rr.core.PageDelay = 80 * time.Millisecond
	rr.core.CellUp(rr.a, cell.CellUp{StubVersion: "08.25", CapSubRecords: 6})
	waitFor(t, "a's radio on", radioOnAt(rr.rig, 0))
	rr.core.CellUp(rr.b, cell.CellUp{StubVersion: "08.25", CapSubRecords: 6})
	waitFor(t, "b's radio on", radioOnAt(rr.rig, 1))
	return rr
}

// lines are the call-in and remote-released commands a stack got.
func (rr *routeRig) lines(i int) []string {
	var out []string
	rr.stack(i).get(func(f *fakeStack) {
		for _, c := range f.ctls {
			if c.line != "bridge" {
				out = append(out, c.line)
			}
		}
	})
	return out
}

func (rr *routeRig) waitLines(t *testing.T, i, n int) []string {
	t.Helper()
	waitFor(t, "commands on a stack", func() bool { return len(rr.lines(i)) >= n })
	return rr.lines(i)
}

func nextEvent(t *testing.T, o *Offer) (CallEvent, bool) {
	t.Helper()
	select {
	case ev, ok := <-o.Events:
		return ev, ok
	case <-time.After(3 * time.Second):
		t.Fatal("no call event")
	}
	return CallEvent{}, false
}

// The cell the handset was last on is offered the call alone; the other cell
// after the page delay; the first to answer takes it and the other leg is
// released.
func TestRingLastCellFirstThenTheOthers(t *testing.T) {
	rr := newRouteRig(t, "a")
	o, err := rr.core.Ring(context.Background(), rr.h.ID, Call{SIP: "x1", Line: 2, Number: "030 1234-567", Name: `Ann "the" caller`})
	if err != nil {
		t.Fatal(err)
	}
	first := rr.waitLines(t, 0, 1)
	want := `call-in line=2 hs=7 number=0301234567 name="Ann the caller" sip=x1`
	if first[0] != want {
		t.Errorf("a got %q, want %q", first[0], want)
	}
	rr.stack(0).get(func(f *fakeStack) {
		if f.ctls[0].line != "bridge" || f.ctls[0].isBridge || !f.ctls[1].isBridge {
			t.Errorf("a: %+v: the stack is made the bridge first, then the call-in goes to it as the bridge", f.ctls)
		}
	})
	time.Sleep(40 * time.Millisecond)
	if n := len(rr.lines(1)); n != 0 {
		t.Fatalf("b was offered the call %d ms early", 40)
	}
	b := rr.waitLines(t, 1, 1)
	if b[0] != want {
		t.Errorf("b got %q", b[0])
	}
	// b's handset answers
	env := rr.stack(1).env
	env.Event(stack.ToBridge, "ringing call=8 hs=7")
	if ev, _ := nextEvent(t, o); ev.Kind != CallRinging || ev.Cell != rr.b {
		t.Errorf("first event %+v", ev)
	}
	env.Event(stack.ToBridge, "answered call=8 hs=7")
	if ev, _ := nextEvent(t, o); ev.Kind != CallAnswered || ev.Cell != rr.b || ev.Call != "8" {
		t.Errorf("answer %+v", ev)
	}
	got := rr.waitLines(t, 0, 2)
	if got[1] != "remote-released call=7 reason=normal" {
		t.Errorf("a's leg: %q, want it released", got[1])
	}
	// the call ends on b's handset
	env.Event(stack.ToBridge, "released call=8 by=hs reason=normal")
	if ev, _ := nextEvent(t, o); ev.Kind != CallReleased || ev.Reason != "normal" {
		t.Errorf("end %+v", ev)
	}
	if _, ok := <-o.Events; ok {
		t.Error("events not closed after the release")
	}
}

// An answer on the first cell stops the second wave.
func TestRingAnsweredInTheFirstWave(t *testing.T) {
	rr := newRouteRig(t, "b")
	o, err := rr.core.Ring(context.Background(), rr.h.ID, Call{SIP: "x2", Number: "1"})
	if err != nil {
		t.Fatal(err)
	}
	rr.waitLines(t, 1, 1)
	rr.stack(1).env.Event(stack.ToBridge, "answered call=8 hs=7")
	if ev, _ := nextEvent(t, o); ev.Kind != CallAnswered || ev.Cell != rr.b {
		t.Fatalf("answer %+v", ev)
	}
	time.Sleep(160 * time.Millisecond)
	if n := len(rr.lines(0)); n != 0 {
		t.Errorf("a was paged (%v) after b had answered", rr.lines(0))
	}
}

// A first cell whose stack is busy does not hold the others back.
func TestRingBusyFirstCellPagesTheRestAtOnce(t *testing.T) {
	rr := newRouteRig(t, "a")
	rr.reply = func(i int, sip string) string {
		if i == 0 {
			return "busy sip=" + sip + " why=in-call"
		}
		return "call-in-ok call=8 sip=" + sip
	}
	start := time.Now()
	if _, err := rr.core.Ring(context.Background(), rr.h.ID, Call{SIP: "x3", Number: "1"}); err != nil {
		t.Fatal(err)
	}
	rr.waitLines(t, 1, 1)
	if d := time.Since(start); d > 60*time.Millisecond {
		t.Errorf("b was paged after %v, the page delay is 80 ms: it should not have waited", d)
	}
}

// Every leg busy: the offer is busy, once, and over.
func TestRingEveryCellBusy(t *testing.T) {
	rr := newRouteRig(t, "")
	rr.reply = func(_ int, sip string) string { return "busy sip=" + sip + " why=channels" }
	o, err := rr.core.Ring(context.Background(), rr.h.ID, Call{SIP: "x4", Number: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if ev, _ := nextEvent(t, o); ev.Kind != CallBusy || ev.Reason != "channels" {
		t.Errorf("event %+v", ev)
	}
	if _, ok := <-o.Events; ok {
		t.Error("more events after busy")
	}
	waitFor(t, "the offer forgotten", func() bool {
		rr.core.calls.mu.Lock()
		defer rr.core.calls.mu.Unlock()
		return len(rr.core.calls.live) == 0
	})
}

// The caller hangs up: the legs that have a call number are released, one that
// has none yet is released when its stack gives it.
func TestRingCancel(t *testing.T) {
	rr := newRouteRig(t, "")
	o, err := rr.core.Ring(context.Background(), rr.h.ID, Call{SIP: "x5", Number: "1"})
	if err != nil {
		t.Fatal(err)
	}
	rr.waitLines(t, 0, 1)
	rr.waitLines(t, 1, 1)
	waitFor(t, "both answered", func() bool {
		rr.core.calls.mu.Lock()
		defer rr.core.calls.mu.Unlock()
		rt := rr.core.calls.live["x5"]
		return rt != nil && rt.legs[rr.a].call != "" && rt.legs[rr.b].call != ""
	})
	o.Cancel()
	if ev, _ := nextEvent(t, o); ev.Kind != CallReleased {
		t.Errorf("event %+v", ev)
	}
	if got := rr.waitLines(t, 0, 2); got[1] != "remote-released call=7 reason=normal" {
		t.Errorf("a: %v", got)
	}
	if got := rr.waitLines(t, 1, 2); got[1] != "remote-released call=8 reason=normal" {
		t.Errorf("b: %v", got)
	}
	o.Cancel() // twice is nothing
}

func TestRingErrors(t *testing.T) {
	rr := newRouteRig(t, "a")
	if _, err := rr.core.Ring(context.Background(), uuid.New(), Call{}); !errors.Is(err, ErrNoHandset) {
		t.Errorf("unknown handset: %v", err)
	}
	// no cell up
	rr.core.CellGone(rr.a)
	rr.core.CellGone(rr.b)
	waitFor(t, "both gone", func() bool { return len(rr.core.readyCells()) == 0 })
	if _, err := rr.core.Ring(context.Background(), rr.h.ID, Call{}); !errors.Is(err, ErrNoCell) {
		t.Errorf("no cell: %v", err)
	}
}

func TestParseBridgeLine(t *testing.T) {
	v, kv := parseBridgeLine(`call-in-ok call=7 sip=abc name="two words" empty=`)
	if v != "call-in-ok" || kv["call"] != "7" || kv["sip"] != "abc" || kv["name"] != "two words" || kv["empty"] != "" {
		t.Errorf("%q %v", v, kv)
	}
	if n := cleanNumber("+49 (30) 12*3#\n"); n != "+493012*3#" {
		t.Errorf("number %q", n)
	}
	if n := cleanName("a\"b\\c\nd" + strings.Repeat("x", 40)); strings.ContainsAny(n, "\"\\\n") || len(n) != 32 {
		t.Errorf("name %q", n)
	}
}

// A cell that does not hold the handset takes it into its table when its leg is
// offered, and the call-in names the number it got there.
func TestRingPlacesTheHandsetOnTheCell(t *testing.T) {
	h := handset(9, owner)
	recs := &fakeRecords{place: map[uuid.UUID]Handset{h.ID: h}}
	g := newRig(t, recs, func(f *fakeStack) {
		f.onCtl = func(env stack.Env, line string) {
			if strings.HasPrefix(line, "call-in ") {
				env.Event(stack.ToAsker, "call-in-ok call=7 sip="+line[strings.Index(line, "sip=")+4:])
			}
		}
	})
	id := uuid.New()
	g.core.CellUp(id, cell.CellUp{StubVersion: "08.25", CapSubRecords: 6})
	waitFor(t, "radio on", radioOn(g))
	if _, err := g.core.Ring(context.Background(), h.ID, Call{SIP: "p1", Number: "1"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the call-in", func() bool {
		n := 0
		g.stack(0).get(func(f *fakeStack) { n = len(f.ctls) })
		return n >= 2
	})
	g.stack(0).get(func(f *fakeStack) {
		if f.ctls[1].line != "call-in line=1 hs=9 number=1 sip=p1" {
			t.Errorf("call-in %q", f.ctls[1].line)
		}
		if r, ok := f.records[6]; !ok || r.Slot != 9 {
			t.Errorf("records %+v: the handset should have been added at index 6", f.records)
		}
	})
	// a full cell with nothing to drop: the leg fails and the offer ends busy
	recs.get(func(r *fakeRecords) { r.full = true })
	g.stack(0).get(func(f *fakeStack) { f.busy = true })
	h2 := handset(10, owner)
	recs.get(func(r *fakeRecords) { r.place[h2.ID] = h2 })
	o, err := g.core.Ring(context.Background(), h2.ID, Call{SIP: "p2", Number: "2"})
	if err != nil {
		t.Fatal(err)
	}
	if ev, _ := nextEvent(t, o); ev.Kind != CallBusy {
		t.Errorf("event %+v, want busy: the handset could not be placed", ev)
	}
}

// Every cell gave up the page and none rang: the handset is noted as one that did not answer. One
// cell that did not say so (it was busy) is not the handset's silence.
func TestRingNobodyAnsweredMarksThePageFailed(t *testing.T) {
	rr := newRouteRig(t, "")
	recs := rr.core.Records.(*fakeRecords)
	rr.core.PageDelay = time.Millisecond
	o, err := rr.core.Ring(context.Background(), rr.h.ID, Call{SIP: "p1", Number: "1"})
	if err != nil {
		t.Fatal(err)
	}
	rr.waitLines(t, 0, 1)
	rr.waitLines(t, 1, 1)
	for _, i := range []int{0, 1} {
		rr.stack(i).env.Event(stack.ToBridge, "released call="+string(rune('7'+i))+" by=base sip=p1")
	}
	if ev, _ := nextEvent(t, o); ev.Kind != CallReleased {
		t.Errorf("event %+v", ev)
	}
	waitFor(t, "the page failure noted", func() bool {
		recs.mu.Lock()
		defer recs.mu.Unlock()
		return len(recs.pageFailed) == 1 && recs.pageFailed[0] == rr.h.ID
	})
}

func TestRingOneBusyCellIsNotASilentHandset(t *testing.T) {
	rr := newRouteRig(t, "")
	recs := rr.core.Records.(*fakeRecords)
	rr.core.PageDelay = time.Millisecond
	rr.reply = func(i int, sip string) string {
		if i == 1 {
			return "busy sip=" + sip + " why=channels"
		}
		return "call-in-ok call=7 sip=" + sip
	}
	o, err := rr.core.Ring(context.Background(), rr.h.ID, Call{SIP: "p2", Number: "1"})
	if err != nil {
		t.Fatal(err)
	}
	rr.waitLines(t, 0, 1)
	rr.stack(0).env.Event(stack.ToBridge, "released call=7 by=base sip=p2")
	// busy or released, by which of the two ends last; either way the handset is not noted
	if ev, _ := nextEvent(t, o); ev.Kind != CallReleased && ev.Kind != CallBusy {
		t.Errorf("event %+v", ev)
	}
	time.Sleep(100 * time.Millisecond)
	recs.mu.Lock()
	defer recs.mu.Unlock()
	if len(recs.pageFailed) != 0 {
		t.Error("a cell that was busy made the handset unresponsive")
	}
}
