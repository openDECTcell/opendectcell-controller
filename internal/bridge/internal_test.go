// SPDX-License-Identifier: AGPL-3.0-or-later

package bridge

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/cell/wire"
	"github.com/openDECTcell/opendectcell-controller/internal/dect/core"
	"github.com/openDECTcell/opendectcell-controller/internal/sip"
)

// fakeIdentity is the handsets' internal numbers, names and owners.
type fakeIdentity struct {
	mu sync.Mutex
	h  map[uuid.UUID]struct {
		number, name string
		owner        uuid.UUID
	}
}

func (f *fakeIdentity) add(id uuid.UUID, number, name string, owner uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.h == nil {
		f.h = map[uuid.UUID]struct {
			number, name string
			owner        uuid.UUID
		}{}
	}
	f.h[id] = struct {
		number, name string
		owner        uuid.UUID
	}{number, name, owner}
}

func (f *fakeIdentity) InternalIdentity(_ context.Context, id uuid.UUID) (string, string, uuid.UUID, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	x, ok := f.h[id]
	return x.number, x.name, x.owner, ok, nil
}

func internalRig(t *testing.T) (*rig, *fakeRouter, *fakeIdentity, *fakeHandset, uuid.UUID, uuid.UUID) {
	t.Helper()
	r := newRig(t, fakeLines{})
	router := &fakeRouter{}
	r.m.DECT = router
	ids := &fakeIdentity{}
	r.m.Identity = ids
	owner := uuid.New()
	h := newFakeHandset(true)
	target := uuid.New()
	h.info.Internal, h.info.Target, h.info.Number = true, target, "202"
	ids.add(h.info.Handset, "201", "Büro", owner)
	ids.add(target, "202", "Werkstatt", owner)
	return r, router, ids, h, target, owner
}

// A handset calls another of its account: the target is rung as an internal call from the caller's
// number and name, the caller hears it ring, and when it answers both cells' speech is opened and joined.
func TestInternalCallEndToEnd(t *testing.T) {
	r, router, _, h, target, _ := internalRig(t)
	done := make(chan struct{})
	go func() { r.m.handsetCall(h); close(done) }()
	waitFor(t, "the target rung", func() bool { router.mu.Lock(); defer router.mu.Unlock(); return router.offers[target] != nil })
	router.mu.Lock()
	got, off := router.calls[target], router.offers[target]
	router.mu.Unlock()
	if !got.Internal || got.Number != "201" || got.Name != "Büro" {
		t.Fatalf("the target was offered %+v", got)
	}
	off.events <- core.CallEvent{Kind: core.CallRinging}
	waitFor(t, "ringing", func() bool { return len(h.actions()) == 1 })
	calleeCell := uuid.New()
	off.events <- core.CallEvent{Kind: core.CallAnswered, Cell: calleeCell}
	waitFor(t, "answered", func() bool { return len(h.actions()) == 2 })
	if a := h.actions(); a[0] != "ringing" || a[1] != "answered wb" {
		t.Fatalf("the caller was told %v", a)
	}
	h.events <- core.CallEvent{Kind: core.CallMediaUp, Cell: h.info.Cell, Channel: 1, Wideband: true}
	if n, _ := r.cells.counts(); n != 0 {
		t.Fatal("the speech opened before the target's channel was bound")
	}
	off.events <- core.CallEvent{Kind: core.CallMediaUp, Cell: calleeCell, Channel: 2, Wideband: true}
	waitFor(t, "both MEDIA_OPENs", func() bool { n, _ := r.cells.counts(); return n == 2 })
	for _, mo := range r.cells.opened {
		if mo.Format != wire.FormatG722 {
			t.Errorf("a wideband call met in format %d", mo.Format)
		}
	}
	h.events <- core.CallEvent{Kind: core.CallReleased}
	<-done
	if _, c := r.cells.counts(); c != 2 {
		t.Errorf("%d flows closed", c)
	}
	select {
	case <-off.cancelled:
	default:
		t.Error("the target was not released when the caller hung up")
	}
	if ends := r.log.ends(); len(ends) != 1 || ends[0].Reason != "normal" {
		t.Errorf("call log %+v", ends)
	}
	if s := r.log.started; len(s) != 1 || s[0].Direction != "out" || s[0].RemoteParty != "202" {
		t.Errorf("call log start %+v", s)
	}
}

// Another account's handset is never rung, however the call got here.
func TestInternalCallToAnotherAccountIsRefused(t *testing.T) {
	r, router, ids, h, target, _ := internalRig(t)
	ids.add(target, "202", "Fremd", uuid.New())
	r.m.handsetCall(h)
	if a := h.actions(); len(a) != 1 || a[0] != "release unreachable" {
		t.Errorf("the caller was told %v", a)
	}
	router.mu.Lock()
	defer router.mu.Unlock()
	if len(router.offers) != 0 {
		t.Error("a handset of another account was rung")
	}
}

// No target (the core found none), a caller with no number, or no identity source: refused, nobody rung.
func TestInternalCallWithoutATarget(t *testing.T) {
	for name, tweak := range map[string]func(*fakeHandset, *Manager, *fakeIdentity){
		"no target":     func(h *fakeHandset, _ *Manager, _ *fakeIdentity) { h.info.Target = uuid.Nil },
		"self":          func(h *fakeHandset, _ *Manager, _ *fakeIdentity) { h.info.Target = h.info.Handset },
		"no identities": func(_ *fakeHandset, m *Manager, _ *fakeIdentity) { m.Identity = nil },
		"no number": func(h *fakeHandset, _ *Manager, ids *fakeIdentity) {
			ids.add(h.info.Handset, "", "Büro", uuid.New())
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, router, ids, h, _, _ := internalRig(t)
			tweak(h, r.m, ids)
			r.m.handsetCall(h)
			if a := h.actions(); len(a) != 1 || a[0] != "release unreachable" {
				t.Errorf("the caller was told %v", a)
			}
			router.mu.Lock()
			defer router.mu.Unlock()
			if len(router.offers) != 0 {
				t.Error("somebody was rung")
			}
		})
	}
}

// The target is busy, or declines: the caller hears so, and nothing is opened.
func TestInternalCallBusyAndDeclined(t *testing.T) {
	for _, tc := range []struct {
		ev   core.CallEvent
		want string
	}{
		{core.CallEvent{Kind: core.CallBusy}, "release busy"},
		{core.CallEvent{Kind: core.CallReleased, Reason: "hs"}, "release rejected"},
		{core.CallEvent{Kind: core.CallReleased, Reason: "cell"}, "release unreachable"},
	} {
		r, router, _, h, target, _ := internalRig(t)
		done := make(chan struct{})
		go func() { r.m.handsetCall(h); close(done) }()
		waitFor(t, "the target rung", func() bool { router.mu.Lock(); defer router.mu.Unlock(); return router.offers[target] != nil })
		router.offers[target].events <- tc.ev
		<-done
		if a := h.actions(); a[len(a)-1] != tc.want {
			t.Errorf("%v: the caller was told %v", tc.ev, a)
		}
		if n, _ := r.cells.counts(); n != 0 {
			t.Error("speech was opened")
		}
	}
}

// Another cell cannot answer or end a cell's media: the cell that was sent MEDIA_OPEN alone answers
// it, and only the cell a flow runs on can close it.
func TestMediaOfAnotherCellIsNotTouched(t *testing.T) {
	r := newRig(t, fakeLines{acct: uuid.New(), has: true})
	r.cells.silent = true // the cell does not answer by itself
	sc := newFakeSIP(t, pcma)
	r.m.SIP = &fakeCaller{call: sc}
	h := newFakeHandset(true)
	go r.m.handsetCall(h)
	sc.events <- sip.Event{Kind: sip.EvAnswered}
	waitFor(t, "answered", func() bool { return len(h.actions()) == 1 })
	h.events <- core.CallEvent{Kind: core.CallMediaUp, Cell: h.info.Cell, Channel: 1}
	waitFor(t, "MEDIA_OPEN", func() bool { n, _ := r.cells.counts(); return n == 1 })
	id := r.cells.opened[0].MediaID
	// a stranger's acknowledgement is not taken: the call's media is still waiting for the real one
	r.m.MediaOpened(uuid.New(), wire.MediaOpenAckMsg{MediaID: id, Status: wire.MediaInternalError})
	r.m.MediaOpened(h.info.Cell, wire.MediaOpenAckMsg{MediaID: id, Status: wire.MediaOK})
	waitFor(t, "the speech open", func() bool {
		r.m.mu.Lock()
		defer r.m.mu.Unlock()
		return r.m.sessions[id] != nil
	})
	// a stranger's MEDIA_CLOSED does not stop it
	r.m.MediaClosed(uuid.New(), wire.MediaClosedMsg{MediaID: id, Reason: wire.MediaClosedByChip})
	time.Sleep(50 * time.Millisecond)
	r.m.mu.Lock()
	s := r.m.sessions[id]
	r.m.mu.Unlock()
	select {
	case <-s.done:
		t.Error("a stranger ended a call's speech")
	default:
	}
	h.events <- core.CallEvent{Kind: core.CallReleased}
}
