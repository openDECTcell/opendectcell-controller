// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/cell"
)

// fakePhonebook is Core.Contacts: a list is its handset, the sequence and the hints.
type fakePhonebook struct {
	mu      sync.Mutex
	lists   []string // the calls of ListFile, as "handset seq hints"
	applied []string
	calls   []string // the calls of CallsFile
	result  ContactsApplied
}

func (p *fakePhonebook) ListFile(_ context.Context, h uuid.UUID, seq uint32, hints map[int64]uint16) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := fmt.Sprintf("%s seq=%d hints=%v", h, seq, hints)
	p.lists = append(p.lists, s)
	return s, nil
}

func (p *fakePhonebook) CallsFile(_ context.Context, hs []CallsHandset) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	slots := make([]int, 0, len(hs))
	for _, h := range hs {
		slots = append(slots, int(h.Slot))
	}
	sort.Ints(slots)
	s := fmt.Sprintf("calls for slots %v", slots)
	p.calls = append(p.calls, s)
	return s, nil
}

func (p *fakePhonebook) ApplyChange(_ context.Context, h uuid.UUID, line string) (ContactsApplied, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.applied = append(p.applied, line)
	return p.result, nil
}

func (p *fakePhonebook) listsOf(h uuid.UUID) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, l := range p.lists {
		if len(l) >= 36 && l[:36] == h.String() {
			out = append(out, l)
		}
	}
	return out
}

func textOf(g *rig, i int) map[int]string {
	out := map[int]string{}
	st := g.stack(i)
	if st == nil {
		return out
	}
	st.get(func(f *fakeStack) {
		for k, v := range f.contactsOf {
			out[k] = v
		}
	})
	return out
}

// A handset's Contact List is given to its record when the stack starts and when the handset comes
// into the cell's table.
func TestContactsGivenOnStartAndArrival(t *testing.T) {
	h1, h2 := handset(1, owner), handset(8, owner)
	h2.Rec.PortID = []byte{0x80, 0x08}
	recs := &fakeRecords{hs: []Handset{h1}, place: map[uuid.UUID]Handset{h2.ID: h2}}
	g := newRig(t, recs, nil)
	pb := &fakePhonebook{}
	g.core.Contacts = pb
	g.core.CellUp(uuid.New(), cell.CellUp{StubVersion: "08.25", CapSubRecords: 6})
	waitFor(t, "the first handset's list", func() bool { return len(textOf(g, 0)) == 1 })
	if len(pb.listsOf(h1.ID)) != 1 {
		t.Errorf("lists %v", pb.lists)
	}
	g.stack(0).env.Fetch(h2.Rec.PortID)
	waitFor(t, "the second handset's list on arrival", func() bool { return len(textOf(g, 0)) == 2 })
}

// What a handset changes is stored, and the handset then gets the list that holds its change: the
// journal sequence and the entry identifier it gave a contact it made.
func TestHandsetsChangeIsStoredAndTheListGivenAgain(t *testing.T) {
	h := handset(1, owner)
	recs := &fakeRecords{hs: []Handset{h}}
	g := newRig(t, recs, nil)
	pb := &fakePhonebook{result: ContactsApplied{Seq: 3, Contact: 42, Entry: 5}}
	g.core.Contacts = pb
	g.core.CellUp(uuid.New(), cell.CellUp{StubVersion: "08.25", CapSubRecords: 6})
	waitFor(t, "the first list", func() bool { return len(pb.listsOf(h.ID)) >= 1 })
	g.stack(0).env.ContactsChanged(0, "3\tput\t5\t00000000\tMeyer\tEva\t0\t*\t04:0171")
	waitFor(t, "the change stored", func() bool { pb.mu.Lock(); defer pb.mu.Unlock(); return len(pb.applied) == 1 })
	waitFor(t, "the list with its sequence and hint", func() bool {
		for _, l := range pb.listsOf(h.ID) {
			if l == fmt.Sprintf("%s seq=3 hints=map[42:5]", h.ID) {
				return true
			}
		}
		return false
	})
}

// A change of somebody's contacts gives a new list to the handsets that read them and to no others.
func TestContactsChangedRefreshesTheOwnersHandsets(t *testing.T) {
	other := uuid.New()
	h1, h2 := handset(1, owner), handset(2, other)
	recs := &fakeRecords{hs: []Handset{h1, h2}}
	g := newRig(t, recs, nil)
	pb := &fakePhonebook{}
	g.core.Contacts = pb
	g.core.CellUp(uuid.New(), cell.CellUp{StubVersion: "08.25", CapSubRecords: 6})
	waitFor(t, "both lists", func() bool { return len(pb.listsOf(h1.ID)) == 1 && len(pb.listsOf(h2.ID)) == 1 })
	g.core.ContactsChanged(owner)
	waitFor(t, "the owner's handset again", func() bool { return len(pb.listsOf(h1.ID)) == 2 })
	if n := len(pb.listsOf(h2.ID)); n != 1 {
		t.Errorf("another owner's handset got %d lists", n)
	}
	g.core.ContactsChanged(uuid.Nil) // a shared contact: everybody
	waitFor(t, "everybody", func() bool { return len(pb.listsOf(h1.ID)) == 3 && len(pb.listsOf(h2.ID)) == 2 })
}

func (p *fakePhonebook) callCount() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.calls) }

// The call lists of the handsets in a cell's table go to the stack when it starts and when a handset
// comes in, and again soon after a call of one of them ended: a burst of calls is one load; a call of a
// handset that is not in this cell does not touch it.
func TestCallListsGiven(t *testing.T) {
	old := callsEvery
	callsEvery = 30 * time.Millisecond
	defer func() { callsEvery = old }()
	h1, h2 := handset(1, owner), handset(8, owner)
	h2.Rec.PortID = []byte{0x80, 0x08}
	recs := &fakeRecords{hs: []Handset{h1}, place: map[uuid.UUID]Handset{h2.ID: h2}}
	g := newRig(t, recs, nil)
	pb := &fakePhonebook{}
	g.core.Contacts = pb
	g.core.CellUp(uuid.New(), cell.CellUp{StubVersion: "08.25", CapSubRecords: 6})
	waitFor(t, "the lists at the start", func() bool {
		return len(callsOf(g)) >= 1 && callsOf(g)[len(callsOf(g))-1] == "calls for slots [1]"
	})
	g.stack(0).env.Fetch(h2.Rec.PortID)
	waitFor(t, "the lists with the handset that came in", func() bool {
		l := callsOf(g)
		return l[len(l)-1] == "calls for slots [1 8]"
	})
	n := pb.callCount()
	g.core.CallListsChanged(uuid.New()) // a handset that is not here
	time.Sleep(100 * time.Millisecond)
	if pb.callCount() != n {
		t.Error("a call of another cell's handset reloaded the lists")
	}
	for range 5 {
		g.core.CallListsChanged(h1.ID)
	}
	waitFor(t, "a load after the calls", func() bool { return pb.callCount() > n })
	time.Sleep(100 * time.Millisecond)
	if got := pb.callCount() - n; got > 2 {
		t.Errorf("five calls in a burst made %d loads", got)
	}
}

func callsOf(g *rig) []string {
	st := g.stack(0)
	if st == nil {
		return nil
	}
	var out []string
	st.get(func(f *fakeStack) { out = append(out, f.callsText...) })
	return out
}
