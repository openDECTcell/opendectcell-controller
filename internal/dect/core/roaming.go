// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"encoding/hex"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/dect/profiles"
)

// A cell's table above the chip's slots is a working set (docs/roaming.md
// §4.2). A handset gets into it when it is heard on the cell or called there,
// from the database; the cell asks the handset to register again every
// LocateValidity; it leaves when it has not been heard for that and
// ExpireAfter, or when it was heard on another cell and MovedAfter has passed.
// What is in a table costs RAM, what is in the database costs nothing: a
// handset that left is fetched again the next time it talks to the cell.

// Roaming is the admin's setting of the working sets.
type Roaming struct {
	// LocateValidity is how long a location registration holds: the handset
	// registers again after it. 0: it is not asked to.
	LocateValidity time.Duration
	// ExpireAfter is how long past the validity a handset that has not been
	// heard stays in a cell's table.
	ExpireAfter time.Duration
	// MovedAfter is how long after the handset was heard on another cell it
	// leaves this cell's table.
	MovedAfter time.Duration
}

// DefaultRoaming is what an installation without settings runs with.
func DefaultRoaming() Roaming {
	return Roaming{LocateValidity: 246 * time.Second, ExpireAfter: 60 * time.Second, MovedAfter: 180 * time.Second}
}

// locateUnit is the unit of <<DURATION>> in a LOCATE-ACCEPT (EN 300 175-5 §7.7.13:
// 2^8 multiframes of 160 ms).
const locateUnit = 40960 * time.Millisecond

// Units is LocateValidity in the unit the stack takes: 1 to 255, 0 for none.
func (r Roaming) Units() uint {
	if r.LocateValidity <= 0 {
		return 0
	}
	return uint(min(255, max(1, math.Round(float64(r.LocateValidity)/float64(locateUnit)))))
}

// unheardKeep (a variable for the tests) is how long a handset that was placed in a
// cell's table, for a call or a lookup, and has not been heard there since stays: a call
// to a handset nobody knows the cell of is offered to several cells (route.go), and the
// ones it is not on should not keep it for the whole validity.
var unheardKeep = 90 * time.Second

// roamingEvery (a variable for the tests) is how often the settings are read.
var roamingEvery = 10 * time.Second

func (c *Core) roaming() Roaming {
	c.rmMu.Lock()
	defer c.rmMu.Unlock()
	if c.Roaming == nil {
		return DefaultRoaming()
	}
	if !c.rmAt.IsZero() && time.Since(c.rmAt) < roamingEvery {
		return c.rm
	}
	ctx, cancel := context.WithTimeout(c.ctx, storeTimeout)
	defer cancel()
	rm, err := c.Roaming(ctx)
	if err != nil {
		c.Log.Warn("the roaming settings could not be read: the last ones stay", "err", err)
		if c.rmAt.IsZero() {
			c.rm = DefaultRoaming()
		}
		c.rmAt = time.Now()
		return c.rm
	}
	c.rm, c.rmAt = rm, time.Now()
	return rm
}

// transRec is a handset in a cell's working set.
type transRec struct {
	idx         int
	slot        uint
	added       time.Time // placed, or called for
	heard       time.Time // the last contact the stack reported
	evict, busy bool      // to go: its key changed, or the stack was not quiet at the last try
}

// index allocation: the lowest free one, else the next.
func (r *cellRun) allocIdx() (int, bool) {
	if n := len(r.freeIdx); n > 0 {
		i := r.freeIdx[n-1]
		r.freeIdx = r.freeIdx[:n-1]
		return i, true
	}
	if r.next < maxRecord {
		i := r.next
		r.next++
		return i, true
	}
	return 0, false
}

// admit puts a handset the store placed here into the running stack. False:
// no index is free and nothing can be dropped to make one.
func (r *cellRun) admit(h Handset, now time.Time) bool {
	if _, ok := r.idxOf[h.ID]; ok {
		if t := r.trans[h.ID]; t != nil {
			t.added = now // wanted again
		}
		return true
	}
	if h.Rec.Slot <= maxSlot {
		return false // a chip record is loaded with the stack, not added to it
	}
	i, ok := r.allocIdx()
	if !ok && r.evictOldest() {
		i, ok = r.allocIdx()
	}
	if !ok {
		ctx, cancel := context.WithTimeout(r.core.ctx, storeTimeout)
		_ = r.core.Records.Unplace(ctx, r.id, h.ID)
		cancel()
		return false
	}
	r.st.AddRecord(i, h.Rec)
	r.applyInfo(i, h.Name, h.Profile)
	r.gen.place(i, h.Rec.Slot, h.Owner)
	r.gen.job(job{kind: jobTrack, i: i, hid: h.ID})
	r.idxOf[h.ID], r.hidOf[i] = i, h.ID
	r.trans[h.ID] = &transRec{idx: i, slot: h.Rec.Slot, added: now}
	r.log.Info("a handset is in this cell's table", "handset", h.ID, "record", h.Rec.Slot, "held", len(r.trans))
	r.replayMWI(h.ID)
	r.pushContacts(i)
	r.callsDirty = true // its calls, with the others
	r.core.DirectoryChanged()
	return true
}

// dropTransient takes a working-set handset out of the stack. False: the
// stack is not quiet (a link or call is up), try again.
func (r *cellRun) dropTransient(hid uuid.UUID, why string) bool {
	t := r.trans[hid]
	if t == nil {
		return true
	}
	if !r.st.Drop(t.idx) {
		t.busy = true
		return false
	}
	delete(r.trans, hid)
	delete(r.idxOf, hid)
	delete(r.hidOf, t.idx)
	r.freeIdx = append(r.freeIdx, t.idx)
	delete(r.seenMs, t.idx)
	delete(r.detMs, t.idx)
	delete(r.cseq, t.idx)
	delete(r.chints, t.idx)
	delete(r.seenAt, t.idx)
	r.gen.unplace(t.idx)
	r.gen.job(job{kind: jobUntrack, i: t.idx, hid: hid})
	r.log.Info("a handset left this cell's table", "handset", hid, "why", why, "held", len(r.trans))
	r.core.DirectoryChanged()
	return true
}

// evictOldest makes room: the handset heard least recently goes.
func (r *cellRun) evictOldest() bool {
	var oldest uuid.UUID
	var at time.Time
	for hid, t := range r.trans {
		last := t.added
		if t.heard.After(last) {
			last = t.heard
		}
		if oldest == uuid.Nil || last.Before(at) {
			oldest, at = hid, last
		}
	}
	return oldest != uuid.Nil && r.dropTransient(oldest, "the table is full")
}

// fetched is the store's answer to a Fetch: the handset joins the table, and
// the stack answers its held request either way.
func (r *cellRun) fetched(ev event) {
	defer clear(ev.h.Rec.UAK[:])
	if r.st == nil || r.gen == nil {
		return
	}
	h, ok := ev.h, ev.ok
	r.log.Debug("the store answered a lookup", "ipui", hex.EncodeToString(ev.ipui), "found", ok, "full", ev.full, "err", ev.err)
	if ev.err != nil {
		r.log.Error("the store could not look a handset up", "err", ev.err)
		ok = false
	}
	if ev.full {
		if !r.evictOldest() {
			r.log.Warn("the cell's table is full and every handset in it is busy: a handset is turned away")
		} else {
			ctx, cancel := context.WithTimeout(r.core.ctx, storeTimeout)
			var err error
			h, ok, err = r.core.Records.PlaceByIPUI(ctx, r.id, ev.ipui)
			cancel()
			defer clear(h.Rec.UAK[:])
			if err != nil {
				r.log.Error("the store could not look a handset up", "err", err)
				ok = false
			}
		}
	}
	if ok && !r.admit(h, time.Now()) {
		ok = false
	}
	if !ok {
		r.log.Debug("a handset this cell does not hold is told its identity is unknown")
	}
	r.st.Resume(r.core.now())
}

// evict marks a handset to leave the table (its key or registration changed);
// the next sweep takes it out, repeatedly until the stack is quiet.
func (r *cellRun) evict(hid uuid.UUID) {
	if t := r.trans[hid]; t != nil {
		t.evict = true
	}
}

// ensure makes sure the handset is in the table, for a call, and says its
// record number. False: the store does not know it, or no room.
func (r *cellRun) ensure(hid uuid.UUID) (uint, bool) {
	now := time.Now()
	if idx, ok := r.idxOf[hid]; ok {
		if t := r.trans[hid]; t != nil {
			t.added = now // a call is being offered: keep it
		}
		slot, ok := r.gen.slotOf[idx]
		return slot, ok
	}
	ctx, cancel := context.WithTimeout(r.core.ctx, storeTimeout)
	h, ok, err := r.core.Records.PlaceHandset(ctx, r.id, hid)
	cancel()
	defer clear(h.Rec.UAK[:])
	if err != nil && r.evictOldest() {
		ctx, cancel = context.WithTimeout(r.core.ctx, storeTimeout)
		h, ok, err = r.core.Records.PlaceHandset(ctx, r.id, hid)
		cancel()
		defer clear(h.Rec.UAK[:])
	}
	if err != nil || !ok || !r.admit(h, now) {
		return 0, false
	}
	return h.Rec.Slot, true
}

// sweep is the working set's expiry, once a second: a handset that has not
// been heard for the validity and the grace after it leaves, one that was
// heard on another cell leaves MovedAfter after that, one marked for
// eviction leaves as soon as the stack is quiet.
func (r *cellRun) sweep(now time.Time) {
	rm := r.core.roaming()
	if u := rm.Units(); u != r.units {
		r.st.SetLocateValidity(u)
		r.units = u
	}
	for hid, t := range r.trans {
		last := t.added
		if t.heard.After(last) {
			last = t.heard
		}
		why := ""
		switch o, moved := r.core.lastHeard(hid); {
		case t.evict:
			why = "its registration changed"
		case t.heard.IsZero() && now.Sub(t.added) > unheardKeep:
			why = "never heard here"
		case now.Sub(last) > rm.LocateValidity+rm.ExpireAfter:
			why = "not heard"
		case moved && o.cell != r.id && o.at.After(last) && now.Sub(o.at) >= rm.MovedAfter:
			why = "heard on another cell"
		}
		if why != "" {
			r.dropTransient(hid, why)
		}
	}
}

// withSlot puts the handset's record number on this cell into a command.
func withSlot(line string, slot uint) string {
	return strings.ReplaceAll(line, "{hs}", strconv.FormatUint(uint64(slot), 10))
}

// listName is a handset's name as its Internal Names List takes it: at most
// 15 octets of UTF-8, cut at a character.
func listName(s string) string {
	s = strings.TrimSpace(strings.Map(func(c rune) rune {
		if c < ' ' || c == 0x7f {
			return -1
		}
		return c
	}, s))
	const max = 15
	if len(s) <= max {
		return s
	}
	n := 0
	for i := range s {
		if i > max {
			break
		}
		n = i
	}
	return s[:n]
}

// applyInfo gives record i of the stack its name for the handsets' lists and
// the habits of its device profile.
func (r *cellRun) applyInfo(i int, name, profile string) {
	if n := listName(name); n != "" && !r.st.SetName(i, n) {
		r.log.Warn("the stack refused a handset's name", "record", i)
	}
	cat := r.core.Profiles
	if cat == nil {
		cat = profiles.Default
	}
	mfr, dev, ok := cat.Texts(profile)
	switch {
	case !ok:
		r.log.Warn("a handset has a device profile this controller does not know: it keeps the standard behaviour", "record", i, "profile", profile)
	case !r.st.SetProfile(i, mfr, dev):
		r.log.Warn("the stack could not read a handset's device profile", "record", i, "profile", profile)
	}
}

// applyStored reads handset hid's name and device profile from the store and gives
// them to record i: a record a registration just filled gets them here, the
// registration's wizard may have written them already or does a moment later
// (Core.HandsetChanged, whose evInfo finds the record by now).
func (r *cellRun) applyStored(hid uuid.UUID, i int) {
	ctx, cancel := context.WithTimeout(r.core.ctx, storeTimeout)
	name, profile, ok, err := r.core.Records.HandsetInfo(ctx, hid)
	cancel()
	if err != nil || !ok {
		return
	}
	r.applyInfo(i, name, profile)
}
