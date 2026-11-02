// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// The handsets' phonebook. The controller keeps the contacts (internal/contacts); the stack keeps
// one Contact List per record, which the handset reads and edits (libopendectcell API 151). The core
// gives a record its list when its handset comes into a cell's table or a stack starts, again
// when a contact changes, and takes what the handset changes back to the database.

// CallsHandset is a handset in a cell's table and the terminal id (record slot) it has there.
type CallsHandset struct {
	Handset uuid.UUID
	Slot    uint
}

// ContactsApplied is what a handset's change came to.
type ContactsApplied struct {
	// Seq is the journal sequence the change had; the next file for that handset says it holds
	// the journal up to there.
	Seq uint32
	// Contact is the contact the change made or changed here, 0 if none; Entry is the
	// identifier the handset gave the entry, which a contact it made keeps there.
	Contact int64
	Entry   uint16
}

// Contacts is the phonebook the handsets read (Core.Contacts).
type Contacts interface {
	// ListFile is the Contact List of a handset as a contacts file (dect_contacts.h) that holds
	// its journal up to seq, with hints saying the entry identifier a contact has on that handset.
	ListFile(ctx context.Context, handset uuid.UUID, seq uint32, hints map[int64]uint16) (string, error)
	// CallsFile is the call lists file (dect_calls.h) for the handsets in a cell's table: the calls each
	// of them sees (internal/contacts).
	CallsFile(ctx context.Context, handsets []CallsHandset) (string, error)
	// ApplyChange stores a change the handset made (a journal line) in its owner's list.
	ApplyChange(ctx context.Context, handset uuid.UUID, line string) (ContactsApplied, error)
}

// ContactsChanged tells every cell that owner's contacts changed (uuid.Nil: the shared ones, so
// everybody's): the handsets that read them get a new list.
func (c *Core) ContactsChanged(owner uuid.UUID) {
	c.mu.Lock()
	ids := make([]uuid.UUID, 0, len(c.cells))
	for id := range c.cells {
		ids = append(ids, id)
	}
	c.mu.Unlock()
	for _, id := range ids {
		c.post(id, event{kind: evContacts, hid: owner})
	}
}

// pushContacts gives record i's handset its Contact List.
func (r *cellRun) pushContacts(i int) {
	hid, ok := r.hidOf[i]
	if !ok || r.st == nil || r.core.Contacts == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.core.ctx, storeTimeout)
	text, err := r.core.Contacts.ListFile(ctx, hid, r.cseq[i], r.chints[i])
	cancel()
	if err != nil {
		r.log.Warn("a handset's phonebook could not be read", "handset", hid, "err", err)
		return
	}
	if !r.st.SetContacts(i, text) {
		r.log.Warn("the stack refused a handset's phonebook", "handset", hid, "bytes", len(text))
	}
}

// contactsOf refreshes the lists of the handsets in this cell's table that read owner's contacts
// (owner uuid.Nil: all of them).
func (r *cellRun) contactsOf(owner uuid.UUID) {
	if r.st == nil || r.gen == nil {
		return
	}
	for i := range r.hidOf {
		if owner == uuid.Nil || r.gen.owner[r.gen.slotOf[i]] == owner {
			r.pushContacts(i)
		}
	}
}

// contactsDone: a change a handset made has been stored (or refused). Its sequence is noted for
// the next file, a contact it made keeps the handset's entry identifier, and everybody who reads
// that owner's contacts gets the new list.
func (r *cellRun) contactsDone(ev event) {
	i, ok := r.idxOf[ev.hid]
	if !ok {
		return
	}
	if ev.seq > r.cseq[i] {
		if r.cseq == nil {
			r.cseq = map[int]uint32{}
		}
		r.cseq[i] = ev.seq
	}
	if ev.contact != 0 && ev.entry != 0 {
		if r.chints == nil {
			r.chints = map[int]map[int64]uint16{}
		}
		if r.chints[i] == nil {
			r.chints[i] = map[int64]uint16{}
		}
		r.chints[i][ev.contact] = ev.entry
	}
	owner := uuid.Nil
	if r.gen != nil {
		owner = r.gen.owner[r.gen.slotOf[i]]
	}
	r.core.ContactsChanged(owner)
}

// ContactsChanged is the stack's: record i's handset changed its list.
func (g *gen) ContactsChanged(i int, line string) {
	g.job(job{kind: jobContacts, i: i, name: line})
}

// The call lists. The stack keeps one set for the base, each entry saying which terminal (record slot)
// sees it on which line; the controller gives it the calls of every handset in the cell's table from the
// database (internal/contacts, CallsFile) when a handset comes in, when the stack starts and shortly after
// a call of one of them ended, so the lists follow the handsets from cell to cell.

// callsEvery (a variable for the tests) is the shortest time between two loads of a cell's call lists.
var callsEvery = 500 * time.Millisecond

// CallListsChanged tells the cells that the call lists of these handsets changed (a call ended).
func (c *Core) CallListsChanged(hids ...uuid.UUID) {
	if len(hids) == 0 {
		return
	}
	c.mu.Lock()
	ids := make([]uuid.UUID, 0, len(c.cells))
	for id := range c.cells {
		ids = append(ids, id)
	}
	c.mu.Unlock()
	for _, id := range ids {
		c.post(id, event{kind: evCalls, hids: hids})
	}
}

// callsFor marks the call lists to be given again if one of hids is in this cell's table.
func (r *cellRun) callsFor(hids []uuid.UUID) {
	for _, h := range hids {
		if _, ok := r.idxOf[h]; ok {
			r.callsDirty = true
			return
		}
	}
}

// pushCalls gives the stack the call lists of the handsets in the table, if they are due.
func (r *cellRun) pushCalls(now time.Time) {
	if !r.callsDirty || r.st == nil || r.gen == nil || r.core.Contacts == nil || now.Sub(r.callsAt) < callsEvery {
		return
	}
	r.callsDirty = false
	r.callsAt = now
	hs := make([]CallsHandset, 0, len(r.hidOf))
	for i, hid := range r.hidOf {
		hs = append(hs, CallsHandset{Handset: hid, Slot: r.gen.slotOf[i]})
	}
	ctx, cancel := context.WithTimeout(r.core.ctx, storeTimeout)
	text, err := r.core.Contacts.CallsFile(ctx, hs)
	cancel()
	if err != nil {
		r.log.Warn("the call lists could not be read", "err", err)
		return
	}
	if !r.st.SetCalls(text) {
		r.log.Warn("the stack refused the call lists", "bytes", len(text))
	}
}
