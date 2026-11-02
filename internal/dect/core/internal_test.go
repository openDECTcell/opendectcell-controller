// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/cell"
	"github.com/openDECTcell/opendectcell-controller/internal/dect/stack"
)

// A handset can call the handsets of its own account, by internal number or by what its INT list
// shows; never another account's, by any of the ways to name it. The cell holds the caller (slot 1,
// internal number 201) and a handset of another account (slot 2); the account's other handset is on
// another cell (internal number 202, directory entry 64).
func TestInternalCallsStayInsideTheAccount(t *testing.T) {
	old := dirEvery
	dirEvery = 0
	t.Cleanup(func() { dirEvery = old })
	other := uuid.New()
	h1, h2 := handset(1, owner), handset(2, other)
	away := DirHandset{ID: uuid.New(), Owner: owner, Name: "Werkstatt"}
	stranger := uuid.New()
	recs := &fakeRecords{hs: []Handset{h1, h2}, dirHs: []DirHandset{away},
		internal: map[uuid.UUID]fakeInternal{
			h1.ID:    {201, "Büro", owner},
			away.ID:  {202, "Werkstatt", owner},
			stranger: {203, "Fremd", other},
			h2.ID:    {200, "Nachbar", other},
		}}
	g := newRig(t, recs, nil)
	got := make(chan *Outgoing, 1)
	g.core.Originate = func(o *Outgoing) { got <- o }
	g.core.CellUp(uuid.New(), cell.CellUp{StubVersion: "08.25", CapSubRecords: 6})
	waitFor(t, "the directory", func() bool { return len(dirOf(g, 0)) == 1 })
	env := g.stack(0).env
	for i, c := range []struct {
		digits string
		want   uuid.UUID
		what   string
	}{
		{"202", away.ID, "the internal number of a handset on another cell"},
		{"64", away.ID, "its directory entry"},
		{"203", uuid.Nil, "another account's internal number"},
		{"200", uuid.Nil, "another account's handset, by its own number"},
		{"2", uuid.Nil, "another account's handset in this cell's table, by its record number"},
		{"1", uuid.Nil, "itself, by its record number"},
		{"201", uuid.Nil, "itself, by its internal number"},
		{"999", uuid.Nil, "a number nobody has"},
		{"65", uuid.Nil, "a directory number nobody holds"},
	} {
		env.Event(stack.ToBridge, fmt.Sprintf("call-out call=%d hs=1 line=1 codec=wb class=int number=%s", i+1, c.digits))
		var o *Outgoing
		select {
		case o = <-got:
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: the call was not offered", c.what)
		}
		if !o.Internal || o.Target != c.want || o.Handset != h1.ID {
			t.Errorf("%s (%s): internal %v, target %s, want %s", c.what, c.digits, o.Internal, o.Target, c.want)
		}
		o.Release("normal")
	}
	// an ordinary call is not internal
	env.Event(stack.ToBridge, "call-out call=20 hs=1 line=1 codec=wb number=202")
	if o := <-got; o.Internal || o.Target != uuid.Nil {
		t.Errorf("a call to the number 202 is internal: %+v", o.OutCall)
	}
}

// A handset's {DETACH} and an unanswered page reach the store, once each.
func TestPresenceEventsReachTheStore(t *testing.T) {
	h := handset(1, owner)
	recs := &fakeRecords{hs: []Handset{h}}
	g := newRig(t, recs, nil)
	g.core.CellUp(uuid.New(), cell.CellUp{StubVersion: "08.25", CapSubRecords: 6})
	waitFor(t, "the stack", func() bool { return g.stack(0) != nil })
	st := g.stack(0)
	st.mu.Lock()
	st.seen = map[int]uint64{0: 5000}
	st.detached = map[int]uint64{0: 5000}
	st.mu.Unlock()
	waitFor(t, "the detach noted", func() bool {
		recs.mu.Lock()
		defer recs.mu.Unlock()
		return len(recs.detachedAt) == 1 && recs.detachedAt[0] == h.ID
	})
	// heard again after it: not detached any more, nothing more noted
	st.mu.Lock()
	st.seen[0] = 9000
	st.mu.Unlock()
	time.Sleep(600 * time.Millisecond)
	recs.mu.Lock()
	defer recs.mu.Unlock()
	if len(recs.detachedAt) != 1 {
		t.Errorf("the detach was noted %d times", len(recs.detachedAt))
	}
}
