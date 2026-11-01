// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/cell"
	"github.com/openDECTcell/opendectcell-controller/internal/cell/wire"
	"github.com/openDECTcell/opendectcell-controller/internal/dect/stack"
)

// cellRun is one cell's goroutine and everything only it touches.
type cellRun struct {
	core *Core
	id   uuid.UUID
	in   chan event
	log  *slog.Logger

	up     cell.CellUp // the last HELLO
	linkUp bool
	// The identity (CONFIG_SET): what the cell was last asked to take, so a
	// cell that does not take it is not asked on every connection; when the
	// stack may start anyway; the PARK length this cell's stack uses.
	tried       [5]byte
	triedSet    bool
	revertTried bool
	waitUntil   time.Time
	parkPLI     uint
	st          stack.Stack
	gen         *gen
	bring       []stack.Mail // start-up mails still to send, one per BringUp
	nextAt      time.Time
	slots       []uint    // the chip slots the records went into, for Installed
	next        int       // the next free record index in the stack
	offered     [][3]byte // pool TPUIs the free records were given, released on drop
	reloadAt    time.Time
	recycleDue  []recycleDue      // pooled enrolments whose chip slot is to be freed
	nudgeDue    []nudgeDue        // freshly enrolled handsets to tell their names to once more
	releases    int               // links the chip reported released, since the stack started
	forgetDue   []uuid.UUID       // handsets the user removes without asking them
	rfpiAskAt   time.Time         // when the chip was last asked for its RFPI
	sigAt       map[int]time.Time // when each record's field strength was last written
	sigMs       map[int]uint64    // the stack's clock of the one written
	rfpiWarned  bool
	bridge      bool // this stack has been told it carries external calls (route.go)
	// what the router reads from other goroutines (route.go): the handsets
	// this stack holds by record number, and whether its radio is on
	pub   sync.Mutex
	ready bool
	// the stack's records by handset, and the working set above the chip's slots
	idxOf   map[uuid.UUID]int
	hidOf   map[int]uuid.UUID
	trans   map[uuid.UUID]*transRec
	freeIdx []int
	units   uint // the location validity set in the stack
	sweepAt time.Time
	install bool // the radio is on: the records go in at the next tick
	retryAt time.Time
	// the last contact written for each record, and when
	seenMs    map[int]uint64
	seenAt    map[int]time.Time
	seenPend  map[int]time.Time
	seenCheck time.Time
	noStack   bool // said once: this binary has no libopendectcell
	// resumed: the session resumed, so the radio may carry calls and is
	// never switched off for the bring-up (the maintainers' notes)
	resumed bool
	// refused: an enrolment came without a registration window; the stack
	// is rebuilt once the call into it has returned, so the handset's key
	// leaves the stack and the chip
	refused bool

	corr     uint16
	corrSlot map[uint16]uint // slot writes in flight, by corr (§7.4)

	// The registration window (ADR 0005): the radio is on, the PIN list's
	// version the stack has, and the closing time it was opened with.
	radioOn  bool
	radioAt  time.Time // when the bring-up finished
	regSet   bool
	regVer   uint64
	regUntil time.Time
}

func (r *cellRun) run(ctx context.Context) {
	t := time.NewTicker(r.core.Tick)
	defer t.Stop()
	defer r.drop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-r.in:
			r.guard(func() { r.handle(ev) })
		case <-t.C:
			r.guard(r.tick)
		}
	}
}

// guard contains a panic in the cell's stack or its callbacks to this
// cell: the stack is dropped and rebuilt after the retry delay, the other
// cells carry on (libopendectcell-integration.md §8).
func (r *cellRun) guard(fn func()) {
	defer func() {
		v := recover()
		if v == nil {
			return
		}
		r.log.Error("DECT stack panicked: dropped, rebuilt after the retry delay",
			"panic", v, "stack", string(debug.Stack()), "retry", r.core.Retry)
		func() {
			defer func() {
				if v := recover(); v != nil {
					r.log.Error("DECT stack panicked again while closing", "panic", v)
				}
			}()
			r.drop()
		}()
		r.resumed, r.refused = false, false // the calls went with the stack
		r.retryAt = time.Now().Add(r.core.Retry)
	}()
	fn()
	if r.refused {
		r.refused = false
		r.log.Error("enrolment without a live registration PIN refused: DECT stack rebuilt")
		r.drop()
		r.resumed = false // the calls went with the stack
		r.retryAt = time.Now()
	}
}

func (r *cellRun) handle(ev event) {
	switch ev.kind {
	case evUp:
		r.linkUp, r.up, r.resumed = true, ev.up, ev.up.Resumed
		if ev.up.Resumed && r.st != nil {
			r.log.Info("DECT stack resumed")
			return
		}
		if r.negotiateIdentity() {
			return
		}
		r.restart()
	case evDown:
		r.linkUp = false
	case evFetched:
		r.fetched(ev)
	case evEvict:
		r.evict(ev.hid)
	case evHome:
		if r.st != nil && r.idxOf != nil {
			r.idxOf[ev.hid], r.hidOf[ev.idx] = ev.idx, ev.hid
		}
	case evDereg:
		if _, ok := r.idxOf[ev.hid]; ok && ev.force && r.st != nil && r.gen != nil {
			r.log.Info("removing a handset without asking it, on request", "handset", ev.hid)
			r.forgetDue = append(r.forgetDue, ev.hid)
		} else if i, ok := r.idxOf[ev.hid]; ok && r.st != nil && r.gen != nil {
			if slot, ok := r.gen.slotOf[i]; ok {
				r.log.Info("deregistering a handset on request", "handset", ev.hid, "record", slot)
				if !r.bridge {
					r.bridge = r.st.Ctl("bridge", r.core.now(), false)
				}
				r.st.Ctl("deregister hs="+strconv.FormatUint(uint64(slot), 10), r.core.now(), r.bridge)
			}
		}
	case evInfo:
		if i, ok := r.idxOf[ev.hid]; ok && r.st != nil {
			r.applyInfo(i, ev.name, ev.profile)
		}
	case evCtl:
		r.ctl(ev)
	case evReload:
		if r.st != nil && r.reloadAt.IsZero() {
			r.reloadAt = time.Now().Add(reloadDelay)
		}
	case evRecycle:
		if r.st == nil {
			return
		}
		due := recycleDue{idx: ev.idx, hid: ev.hid, at: time.Now().Add(reloadDelay), rel: r.releases}
		if r.st.Quiet() { // no link: the registration's has ended already
			due.first = time.Now()
		}
		r.recycleDue = append(r.recycleDue, due)
		if r.trans[ev.hid] == nil && r.idxOf != nil {
			// while the record is in its chip slot the cell knows whose it is: a call
			// finds it, its name and profile reach the stack, its contacts are noted
			r.idxOf[ev.hid], r.hidOf[ev.idx] = ev.idx, ev.hid
			r.applyStored(ev.hid, ev.idx)
			r.nudgeDue = append(r.nudgeDue, nudgeDue{idx: ev.idx, hid: ev.hid, at: time.Now().Add(nudgeAfter)})
		}
		// a handset that enrolled again may sit in this cell's working set with its old key
		if t := r.trans[ev.hid]; t != nil {
			t.evict = true
		}
	case evGone:
		r.linkUp = false
		if r.st != nil {
			r.log.Info("DECT stack ended: the cell is gone")
		}
		r.drop()
	case evMail:
		r.log.Debug("chip mail", "prim", fmt.Sprintf("%04x", ev.mail.Prim), "len", len(ev.mail.Param))
		switch ev.mail.Prim { // the link's own mails, with what they carry: no memory reads, so no keys
		case 0x0200, 0x0201, 0x0202, 0x0203, 0x0242, 0xF601, 0xF602, 0xF623: // F601/F602/F623: the chip's RFPI, its broadcast report, the scan report (the beacon carrier and bearer)
			r.log.Debug("chip mail content", "prim", fmt.Sprintf("%04x", ev.mail.Prim), "param", hex.EncodeToString(ev.mail.Param))
		}
		if ev.mail.Prim == 0x0201 {
			r.releases++
		}
		if ev.mail.Prim == 0xF601 { // the chip's answer to the contact: its RFPI, which is public
			r.log.Debug("the chip names its RFPI", "mail", hex.EncodeToString(ev.mail.Param))
		}
		if r.st != nil {
			r.st.Mail(ev.mail, r.core.now())
		}
	case evRadio:
		// The cell's daemon restarted behind a link that stayed up: its chip is
		// up again but parked, and none of our keys or bring-up is in it. A
		// STATUS soon after our own bring-up can still show the old state, so
		// only one later than a few seconds counts.
		if r.st != nil && r.radioOn && ev.radio == 0 && ev.chip == 2 && time.Since(r.radioAt) > radioSettle {
			r.log.Warn("the cell's radio is parked but the stack thinks it is on (its daemon restarted): starting the stack again", "retry", r.core.Retry)
			r.drop()
			r.retryAt = time.Now().Add(r.core.Retry)
		}
	case evReject:
		if ev.reject.Reason == wire.RejectChipDown {
			// The cell's chip session was not up when the bring-up went out
			// (a chip freshly uploaded takes a few seconds; seen on the 7520,
			// 2026-11-01). Nothing reached the chip, so the stack's idea that
			// the radio is on is wrong: start it again after the retry delay.
			if r.st != nil {
				r.log.Warn("the cell's chip is not up yet: starting the stack again", "retry", r.core.Retry)
				r.drop()
				r.retryAt = time.Now().Add(r.core.Retry)
			}
			return
		}
		slot, ok := r.corrSlot[ev.reject.RefCorr]
		if !ok {
			r.log.Warn("MAIL_REJECT", "corr", ev.reject.RefCorr, "reason", ev.reject.Reason)
			return
		}
		delete(r.corrSlot, ev.reject.RefCorr)
		r.log.Warn("slot write not delivered: slot unknown", "slot", slot, "reason", ev.reject.Reason)
		if r.gen != nil {
			r.gen.job(job{kind: jobUnknown, slot: slot})
		}
	}
}

// nudgeAfter is how long after a pooled handset enrolled its name is given to the
// stack once more (a variable for the tests): the notice that goes out with the
// registration reaches the handset while it is still registering, and an A540
// does not act on it; a moment later, in the link it holds open for a few seconds,
// it may.
var nudgeAfter = 2 * time.Second

// nudgeDue is a handset whose name is to be given to record idx again.
type nudgeDue struct {
	idx int
	hid uuid.UUID
	at  time.Time
}

// nudge gives the names of freshly enrolled handsets to the stack again, which tells
// the handsets in a link that the Internal Names List changed. Only while a link is up:
// without one the notice has nobody to reach.
func (r *cellRun) nudge(now time.Time) {
	for len(r.nudgeDue) > 0 && !now.Before(r.nudgeDue[0].at) {
		n := r.nudgeDue[0]
		r.nudgeDue = r.nudgeDue[1:]
		if r.st == nil || r.gen == nil || r.hidOf[n.idx] != n.hid || r.st.Quiet() {
			continue
		}
		r.applyStored(n.hid, n.idx)
		r.log.Info("a handset's name is told again", "handset", n.hid, "record", n.idx)
	}
}

// recycleDue is a pooled handset that enrolled into record idx, a chip slot's.
type recycleDue struct {
	idx   int
	hid   uuid.UUID
	at    time.Time
	rel   int       // r.releases when it enrolled: the registration link is the next release, the follow-up the one after
	first time.Time // when the registration link was seen to have ended
}

// recycle frees the chip slots of pooled handsets that enrolled here: the
// record becomes a free store again, with a fresh pool TPUI, and the handset
// comes back into the table above the chip's slots when it is heard (or
// called). It waits for the delay after the enrolment, for a quiet stack, and
// for the handset's follow-up contact (followWait); a record the stack will not
// recycle goes the old way, a rebuild.
func (r *cellRun) recycle(now time.Time) {
	if len(r.recycleDue) == 0 || r.gen == nil {
		return
	}
	p := r.recycleDue[0]
	if now.Before(p.at) || !r.st.Quiet() {
		return
	}
	// not before the handset's follow-up contact has come and gone (followWait)
	if p.first.IsZero() { // quiet: the registration's link has ended
		r.recycleDue[0].first = now
		p.first = now
	}
	if r.releases < p.rel+2 && now.Sub(p.first) < followWait {
		return
	}
	ctx, cancel := context.WithTimeout(r.core.ctx, storeTimeout)
	t, err := r.core.Records.ReserveTPUI(ctx)
	cancel()
	if err != nil {
		r.log.Warn("no pool TPUI to recycle a record with: the stack is rebuilt instead", "err", err)
		r.recycleDue = r.recycleDue[1:]
		r.rebuild(now)
		return
	}
	switch r.st.Recycle(p.idx, t) {
	case stack.RecycleBusy:
		r.core.Records.ReleaseTPUI(t)
		return // the next tick
	case stack.RecycleNo:
		r.core.Records.ReleaseTPUI(t)
		r.log.Warn("the stack would not recycle the enrolled record: it is rebuilt instead", "record", p.idx)
		r.recycleDue = r.recycleDue[1:]
		r.rebuild(now)
		return
	}
	r.recycleDue = r.recycleDue[1:]
	if t != ([3]byte{}) {
		r.offered = append(r.offered, t)
	}
	if r.hidOf[p.idx] == p.hid {
		delete(r.hidOf, p.idx) // the record is a free store again; the handset comes back through a lookup
		delete(r.idxOf, p.hid)
	}
	r.gen.unplace(p.idx)
	delete(r.seenMs, p.idx)
	delete(r.seenAt, p.idx)
	delete(r.seenPend, p.idx)
	r.gen.job(job{kind: jobRelease, i: p.idx, hid: p.hid})
	r.log.Info("a pooled handset left the chip slot it enrolled in", "handset", p.hid, "record", p.idx)
}

// forgetNow removes the handsets the user asked to remove without the handset
// (it may be out of range), one at a time, when the stack is quiet: the record
// becomes a free store, its chip slot is cleared, and the store forgets the
// handset as it does after a deregistration the handset answered.
func (r *cellRun) forgetNow() {
	if len(r.forgetDue) == 0 || r.gen == nil || !r.st.Quiet() {
		return
	}
	hid := r.forgetDue[0]
	i, held := r.idxOf[hid]
	if !held {
		r.forgetDue = r.forgetDue[1:] // gone already, say a deregistration that was running
		return
	}
	if t := r.trans[hid]; t != nil {
		// a pooled handset in the working set: leave the table, then the store forgets it everywhere
		if r.dropTransient(hid, "removed by the user") {
			r.forgetDue = r.forgetDue[1:]
			r.gen.job(job{kind: jobForgetHandset, hid: hid})
		}
		return
	}
	ctx, cancel := context.WithTimeout(r.core.ctx, storeTimeout)
	t, err := r.core.Records.ReserveTPUI(ctx)
	cancel()
	if err != nil {
		t = [3]byte{} // no pool: the TPUI is derived from the slot
	}
	switch r.st.Recycle(i, t) {
	case stack.RecycleBusy:
		r.core.Records.ReleaseTPUI(t)
		return // the next tick
	case stack.RecycleNo:
		r.core.Records.ReleaseTPUI(t)
		r.log.Warn("the stack would not remove the record", "record", i)
		r.forgetDue = r.forgetDue[1:]
		return
	}
	r.forgetDue = r.forgetDue[1:]
	if t != ([3]byte{}) {
		r.offered = append(r.offered, t)
	}
	delete(r.seenMs, i)
	delete(r.seenAt, i)
	delete(r.seenPend, i)
	r.gen.Forget(i, "removed by the user without the handset")
}

// rebuild asks for the stack to be built again from the records, at the next tick.
func (r *cellRun) rebuild(now time.Time) {
	if r.reloadAt.IsZero() {
		r.reloadAt = now
	}
}

// reloadDelay (a variable for the tests) is how long after a change of the
// handsets a stack is rebuilt at the earliest, so a handset that has just
// enrolled finishes what it started. The rebuild also waits for a quiet stack,
// so it never ends a call.
var reloadDelay = 3 * time.Second

// parkPLIOverride (OPENDECTCELL_PARK_PLI, a number of bits; for experiments) replaces the PARK
// length handsets get with a class B identity: does a handset re-find a class B base
// when its PARK is longer than the EIC's? 0: the identity's own.
var parkPLIOverride = func() uint {
	n, _ := strconv.ParseUint(os.Getenv("OPENDECTCELL_PARK_PLI"), 10, 8)
	return uint(n)
}()

// followWait is how long a freshly enrolled pooled handset's record stays in its
// chip slot after the registration link ended, for the handset's follow-up
// contact: a handset that was never located before makes it a moment after the
// link (the A540: a location-less reconnect that reads its lists), and a
// record already freed from the chip gets no answer then, so the handset
// stays without its names until it is restarted. A handset that makes no
// such contact is let go after this (a variable for the tests).
var followWait = 20 * time.Second

// setReady publishes what the router reads (route.go).
func (r *cellRun) setReady(v bool) {
	r.pub.Lock()
	r.ready = v
	r.pub.Unlock()
}

// ctl runs a bridge command for the router on the stack. The stack is told it
// carries external calls before its first one.
func (r *cellRun) ctl(ev event) {
	if r.st == nil {
		r.core.legFailed(r.id, ev.sip)
		return
	}
	line := ev.line
	if ev.hid != uuid.Nil {
		slot, ok := r.ensure(ev.hid)
		if !ok {
			r.core.legFailed(r.id, ev.sip)
			return
		}
		line = withSlot(line, slot)
	}
	if !r.bridge {
		r.bridge = r.st.Ctl("bridge", r.core.now(), false)
	}
	r.st.Ctl(line, r.core.now(), r.bridge)
}

// radioSettle is how long after our bring-up a STATUS saying "radio off" is
// believed.
const radioSettle = 3 * time.Second

// identityWait (a variable for the tests) is how long a cell asked to take another identity has to
// reload its chip and come back before its stack starts anyway.
var identityWait = 90 * time.Second

// negotiateIdentity asks the cell to take the system's identity (or give it
// back) if its chip broadcasts another. True: the cell was asked and will come
// back with a new HELLO, so no stack yet. A cell that does not report its
// RFPI is older than the identity messages and is left alone.
func (r *cellRun) negotiateIdentity() bool {
	r.waitUntil, r.parkPLI = time.Time{}, 0
	if r.core.Identity == nil || !r.up.HasRFPI {
		return false
	}
	ctx, cancel := context.WithTimeout(r.core.ctx, storeTimeout)
	defer cancel()
	w, err := r.core.Identity.Want(ctx, r.id)
	if err != nil {
		r.log.Error("the cell's identity is not known: it keeps what it has", "err", err)
		return false
	}
	switch {
	case w.Set:
		r.revertTried = false
		if r.up.RFPI == w.RFPI {
			r.triedSet = false
			r.parkPLI = w.ParkPLI
			if parkPLIOverride > 0 && w.ParkPLI > 0 {
				r.parkPLI = parkPLIOverride
			}
			return false
		}
		if r.triedSet && r.tried == w.RFPI {
			r.log.Warn("the cell did not take the system identity: its stack runs with its own", "wanted", fmt.Sprintf("%x", w.RFPI), "has", fmt.Sprintf("%x", r.up.RFPI))
			return false
		}
		if err := r.core.Link.SetIdentity(r.id, &w.RFPI); err != nil {
			r.log.Warn("the cell could not be asked to take the system identity", "err", err)
			return false
		}
		r.tried, r.triedSet = w.RFPI, true
		if err := r.core.Identity.Pushed(ctx, r.id, &w.RFPI); err != nil {
			r.log.Error("the identity pushed to the cell is not recorded", "err", err)
		}
		r.log.Info("the cell is asked to broadcast the system identity: it reloads its chip", "rfpi", fmt.Sprintf("%x", w.RFPI))
	case w.Revert:
		if r.revertTried {
			return false
		}
		if err := r.core.Link.SetIdentity(r.id, nil); err != nil {
			r.log.Warn("the cell could not be asked to take its own identity back", "err", err)
			return false
		}
		r.revertTried, r.triedSet = true, false
		if err := r.core.Identity.Pushed(ctx, r.id, nil); err != nil {
			r.log.Error("the identity taken back is not recorded", "err", err)
		}
		r.log.Info("the cell is asked to go back to its own identity: it reloads its chip")
	default:
		r.triedSet, r.revertTried = false, false
		return false
	}
	r.waitUntil = time.Now().Add(identityWait)
	return true
}

func (r *cellRun) tick() {
	now := time.Now()
	if r.st == nil && r.linkUp && !r.waitUntil.IsZero() && now.After(r.waitUntil) {
		r.log.Warn("the cell did not come back after the identity change: the stack starts with what it reported")
		r.waitUntil = time.Time{}
		r.restart()
	}
	if r.st == nil && r.linkUp && !r.retryAt.IsZero() && now.After(r.retryAt) {
		r.restart()
	}
	// the rebuild ends every call on the cell: it waits until the stack is quiet
	if r.st != nil && !r.reloadAt.IsZero() && now.After(r.reloadAt) && r.st.Quiet() {
		r.log.Info("the handsets changed: DECT stack rebuilt from the records")
		r.drop()
		r.resumed = false // the calls went with the stack
		r.retryAt = now
	}
	if r.st == nil {
		return
	}
	r.syncRFPI(now)
	r.recycle(now)
	r.nudge(now)
	r.forgetNow()
	if r.st == nil {
		return
	}
	if len(r.bring) > 0 && r.linkUp && !now.Before(r.nextAt) {
		m := r.bring[0]
		r.bring = r.bring[1:]
		r.nextAt = now.Add(r.core.BringUp)
		if err := r.send(0, m); err != nil {
			r.log.Warn("radio bring-up mail not sent", "prim", m.Prim, "err", err)
		}
		if len(r.bring) == 0 {
			r.st.RadioUp(true)
			r.install = true
			r.radioOn = true
			r.setReady(true)
			r.radioAt = time.Now()
			r.log.Info("cell radio on")
		}
	}
	r.registration(now)
	r.st.Tick(r.core.now())
	r.seen(now)
	if r.gen != nil && now.Sub(r.sweepAt) >= time.Second {
		r.sweepAt = now
		r.sweep(now)
	}
	if r.install {
		// dect_base's install() ran in that tick (host.c, cdect_host_tick)
		r.install = false
		r.gen.job(job{kind: jobInstalled, slots: slices.Clone(r.slots)})
	}
}

// seenEvery is how often a handset's contact is written to the database: a
// move between cells shows within this, and a handset on a call does not make
// a write per segment.
var seenEvery = 30 * time.Second

// seen notes which handsets of this cell's stack have been heard since the last
// time, for the database: the cell a handset was last on is what a call is
// routed to first (docs/roaming.md).
func (r *cellRun) seen(now time.Time) {
	if r.gen == nil || now.Sub(r.seenCheck) < 200*time.Millisecond {
		return
	}
	r.seenCheck = now
	if r.seenAt == nil {
		r.seenAt, r.seenMs, r.seenPend = map[int]time.Time{}, map[int]uint64{}, map[int]time.Time{}
	}
	if r.sigAt == nil {
		r.sigAt, r.sigMs = map[int]time.Time{}, map[int]uint64{}
	}
	for i := range r.gen.slotOf {
		if ms := r.st.Seen(i); ms != 0 && ms != r.seenMs[i] {
			r.seenMs[i] = ms
			// the stack's clock is monotonic ms; the contact was this long ago
			at := now.Add(-time.Duration(r.core.now()-ms) * time.Millisecond)
			r.seenPend[i] = at
			if hid, ok := r.hidOf[i]; ok {
				r.core.noteHeard(hid, r.id, at)
				if t := r.trans[hid]; t != nil {
					t.heard = at
				}
			}
		}
		// the database hears of it at most every seenEvery per handset
		if at, ok := r.seenPend[i]; ok && now.Sub(r.seenAt[i]) >= seenEvery {
			r.seenAt[i] = now
			delete(r.seenPend, i)
			r.gen.job(job{kind: jobSeen, i: i, at: at})
		}
		// and the field strength the chip measured on its link, as rarely
		if dbm, ms, ok := r.st.Signal(i); ok && ms != r.sigMs[i] && now.Sub(r.sigAt[i]) >= seenEvery {
			r.sigMs[i], r.sigAt[i] = ms, now
			at := now.Add(-time.Duration(r.core.now()-ms) * time.Millisecond)
			r.gen.job(job{kind: jobSignal, i: i, dbm: dbm, at: at})
		}
	}
}

// rfpiAskEvery is how often a chip is asked for its RFPI while the stack does not
// know it.
var rfpiAskEvery = 2 * time.Second

// syncRFPI makes sure the stack knows the RFPI the chip broadcasts: a handset
// that registers is given it as its base's PARK. A cell in this mode does the chip's
// contact itself, before the controller is there, so the stack never sees the
// chip's answer on its own. The cell says it in its HELLO or STATUS; a cell that
// does not (older software) is asked, with the contact mail, whose answer
// the stack takes. True: the stack knows it.
func (r *cellRun) syncRFPI(now time.Time) bool {
	if r.st == nil {
		return false
	}
	if r.st.HasRFPI() {
		return true
	}
	rfpi, ok := r.up.RFPI, r.up.HasRFPI
	if !ok {
		if ls, is := r.core.Link.(interface {
			CellStatus(uuid.UUID) cell.Status
		}); is {
			if st := ls.CellStatus(r.id); st.HasRFPI {
				rfpi, ok = st.RFPI, true
			}
		}
	}
	if ok {
		r.st.SetRFPI(rfpi)
		r.log.Info("the stack knows what the chip broadcasts", "rfpi", hex.EncodeToString(rfpi[:]))
		return true
	}
	// after the radio bring-up: the contact is not one of its mails
	if r.linkUp && r.radioOn && len(r.bring) == 0 && now.Sub(r.rfpiAskAt) >= rfpiAskEvery {
		r.rfpiAskAt = now
		if err := r.send(0, stack.Mail{Src: 0x14, Dst: 0x02, Prim: 0xF600}); err != nil && !r.rfpiWarned {
			r.rfpiWarned = true
			r.log.Warn("the chip could not be asked for its RFPI", "err", err)
		}
	}
	return false
}

// registration keeps the stack's PIN list and window in step with the
// registry: open while any PIN is live, on a cell with a free record, until
// 60 s after the latest opening (ADR 0005). The stack closes the window
// itself after an enrolment; the next tick opens it again if a PIN is
// still live.
func (r *cellRun) registration(now time.Time) {
	reg := r.core.Reg
	if reg == nil || !r.radioOn || !r.linkUp {
		return
	}
	if !r.st.HasRFPI() {
		// a handset registering now would be given an empty base identity
		if !r.rfpiWarned {
			r.rfpiWarned = true
			r.log.Warn("registration stays closed: the stack does not know what the chip broadcasts yet")
		}
		return
	}
	ver, pins, until := reg.PINs(r.id)
	if !r.regSet || ver != r.regVer {
		r.st.SetRegPINs(pins)
		r.regSet, r.regVer = true, ver
	}
	open, free := r.st.RegState()
	want := len(pins) > 0 && free && until.After(now)
	switch {
	case want && (!open || !until.Equal(r.regUntil)):
		secs := uint((until.Sub(now) + time.Second - 1) / time.Second)
		r.st.RegWindow(true, secs, r.core.now())
		r.regUntil = until
	case !want && open:
		r.st.RegWindow(false, 0, r.core.now())
		r.regUntil = time.Time{}
	}
}

// restart drops the cell's stack, if any, and builds a fresh one from the
// records: a new session, a new chip, or the cell made active.
func (r *cellRun) restart() {
	r.drop()
	r.retryAt = time.Time{}
	if r.core.New == nil {
		if !r.noStack {
			r.noStack = true
			r.log.Warn("this binary has no libopendectcell: the cell's mails are dropped")
		}
		return
	}
	max := int(r.up.CapSubRecords)
	if max == 0 || max > maxSlot {
		max = maxSlot
	}
	ctx, cancel := context.WithTimeout(r.core.ctx, storeTimeout)
	hs, unplaced, err := r.core.Records.CellRecords(ctx, r.id, max)
	cancel()
	if err != nil {
		r.log.Error("handset records not loaded: no DECT stack yet", "err", err, "retry", r.core.Retry)
		r.retryAt = time.Now().Add(r.core.Retry)
		return
	}
	for _, id := range unplaced {
		r.log.Error("cell full: a handset has no record number free here (cell-protocol.md §7.4)", "handset", id)
	}
	table, delay := chipConfig(r.up.StubVersion)
	g := &gen{r: r, slotOf: map[int]uint{}, owner: map[uint]uuid.UUID{}, rePIN: map[int]uint{}, jobs: make(chan job, saveQueue)}
	defer func() {
		for i := range hs {
			clear(hs[i].Rec.UAK[:])
		}
	}()
	// r.st and r.gen are set as soon as they exist, so a panic from here
	// on leaves them for guard's drop: the keys are wiped, the writer ends.
	st := r.core.New(g, stack.Config{NamesRO: true, PollSignal: true, UAKTable: table, CheckDelayMs: delay, Stub: r.up.StubVersion, ParkPLI: r.parkPLI,
		DeferUnknown: true, LocateValidity: r.core.roaming().Units()})
	r.st = st
	r.units = r.core.roaming().Units()
	ids := map[int]uuid.UUID{}
	r.idxOf, r.hidOf, r.trans, r.freeIdx = map[uuid.UUID]int{}, map[int]uuid.UUID{}, map[uuid.UUID]*transRec{}, nil
	r.slots, r.next = r.slots[:0], 0
	// The chip-slot records first, then the places a new handset can enrol:
	// record 0 is the one the main link serves at start, and it keeps being
	// a chip record, or a free one. Handsets above the chip's slots come
	// into the table when they are heard or called (roaming.go).
	chipRecs := 0
	for _, h := range hs {
		chipRecs++
		st.AddRecord(r.next, h.Rec)
		ids[r.next] = h.ID
		r.idxOf[h.ID], r.hidOf[r.next] = r.next, h.ID
		g.place(r.next, h.Rec.Slot, h.Owner)
		r.slots = append(r.slots, h.Rec.Slot)
		r.next++
	}
	free := 0
	for ; chipRecs+free < max; free++ {
		ctx, cancel := context.WithTimeout(r.core.ctx, storeTimeout)
		t, err := r.core.Records.ReserveTPUI(ctx)
		cancel()
		if err != nil {
			// no pooled TPUI: this store takes a chip-slot handset as before
			r.log.Warn("no pool TPUI for a free record: a handset enrolling here sits in a chip slot", "err", err)
			t = [3]byte{}
		} else if t != ([3]byte{}) {
			r.offered = append(r.offered, t)
		}
		st.AddFreeStore(r.next, t)
		r.next++
	}
	g.start(ids)
	r.gen = g
	if err := st.Start(); err != nil {
		r.drop()
		r.log.Error("DECT stack did not start", "err", err)
		return
	}
	// Calls of the handsets go to the SIP side, not to a local connect: the
	// stack is told it has a bridge, from the start (route.go told it only at
	// the first call-in).
	r.bridge = st.Ctl("bridge", r.core.now(), false)
	// Start gives every record the standard behaviour: names and device
	// profiles go on top of that.
	for k, h := range hs {
		r.applyInfo(k, h.Name, h.Profile)
	}
	r.bring = bringUp(r.up.StubVersion, r.up.RadioState == 1, r.resumed)
	r.nextAt = time.Now()
	r.log.Info("DECT stack started", "records", len(hs), "free", free, "validity", r.units, "stub", r.up.StubVersion, "rfpi", rfpiText(r.up))
}

// drop closes the stack. The fields are cleared first, so a panic in
// Close cannot close anything twice.
func (r *cellRun) drop() {
	st, g := r.st, r.gen
	if st != nil {
		r.core.cellGone(r.id) // its calls end with it
	}
	r.st, r.gen = nil, nil
	r.bring, r.install = nil, false
	r.seenMs, r.seenAt, r.seenPend = nil, nil, nil
	r.idxOf, r.hidOf, r.trans, r.freeIdx, r.units = nil, nil, nil, nil, 0
	r.reloadAt = time.Time{}
	r.recycleDue, r.nudgeDue, r.forgetDue = nil, nil, nil
	r.rfpiAskAt, r.rfpiWarned = time.Time{}, false
	r.sigAt, r.sigMs = nil, nil
	for _, t := range r.offered {
		r.core.Records.ReleaseTPUI(t)
	}
	r.offered = nil
	r.radioOn, r.regSet, r.regUntil = false, false, time.Time{}
	r.bridge = false
	r.setReady(false)
	clear(r.corrSlot)
	if g != nil {
		g.stop()
	}
	if st != nil {
		st.Close()
	}
}

// send is MAIL_DOWN. A slot write gets a corr, so a cell that cannot
// deliver it says so (§7.4).
func (r *cellRun) send(corr uint16, m stack.Mail) error {
	if m.Prim == 0x0300 { // a page: whom it names is public, no key travels in it
		r.log.Debug("chip mail down: page", "param", hex.EncodeToString(m.Param))
	}
	if m.Prim == 0x0244 { // the C-plane going out (APPEND_CONNECTION_DATA): signalling, no key in it
		r.log.Debug("chip mail down", "prim", fmt.Sprintf("%04x", m.Prim), "param", hex.EncodeToString(m.Param))
	}
	if corr == 0 && (m.Prim == 0x0247 || m.Prim == 0x020A) && len(m.Param) > 0 {
		r.corr++
		if r.corr == 0 {
			r.corr = 1
		}
		corr = r.corr
		if r.corrSlot == nil || len(r.corrSlot) > 256 {
			r.corrSlot = map[uint16]uint{}
		}
		r.corrSlot[corr] = uint(m.Param[0])
	}
	return r.core.Link.SendMail(r.id, corr, wire.MailDownMsg{Src: m.Src, Dst: m.Dst, Prim: m.Prim, Param: m.Param})
}

// gen is one stack's environment (stack.Env) and its writer, which keeps
// the stack's records in the database in the order they changed.
type gen struct {
	r      *cellRun
	slotOf map[int]uint // record index -> chip slot
	owner  map[uint]uuid.UUID
	jobs   chan job
	// rePIN is the registration PIN a known record last saved with: an
	// enrolment again keeps its record, so the PIN is claimed when the stack
	// reports the handset registered, not when the record is saved.
	rePIN map[int]uint
}

type jobKind int

const (
	jobSave jobKind = iota
	jobForget
	jobRename
	jobInstalled
	jobUnknown
	jobSeen
	jobTrack         // a record added to the running stack: its handset
	jobUntrack       // a record dropped from it: the handset leaves the cell's table
	jobRelease       // a record recycled out of its chip slot: the slot is free in the store
	jobReenrol       // a known handset enrolled again: its window's PIN is claimed
	jobCaps          // a handset's terminal capability, for its page
	jobSignal        // the field strength measured on a link to a handset
	jobForgetHandset // the user removed a handset: the store forgets it, wherever it is placed
)

type job struct {
	kind   jobKind
	i      int
	owner  uuid.UUID // an enrolment's
	window uuid.UUID // the registration window it came through
	rec    stack.Record
	name   string
	slot   uint
	slots  []uint
	at     time.Time      // jobSeen: when the handset was heard
	hid    uuid.UUID      // jobTrack
	caps   map[string]any // jobCaps
	dbm    int            // jobSignal
	pin    uint           // jobReenrol: the record's reg_pin1
}

// place notes record i in chip slot with its owner, for NameVisible.
func (g *gen) place(i int, slot uint, owner uuid.UUID) {
	if old, ok := g.slotOf[i]; ok && old != slot {
		delete(g.owner, old)
	}
	g.slotOf[i] = slot
	g.owner[slot] = owner
}

// unplace forgets record i's place (it left the table).
func (g *gen) unplace(i int) {
	delete(g.rePIN, i)
	if slot, ok := g.slotOf[i]; ok {
		delete(g.owner, slot)
		delete(g.slotOf, i)
	}
}

// job queues a database write. It blocks when the queue is full rather
// than lose a key.
func (g *gen) job(j job) {
	select {
	case g.jobs <- j:
	case <-g.r.core.ctx.Done():
	}
}

// start runs the writer; ids (record index -> handset) is its own from now.
func (g *gen) start(ids map[int]uuid.UUID) {
	core := g.r.core
	core.wg.Add(1)
	go func() {
		defer core.wg.Done()
		for j := range g.jobs {
			g.write(ids, j)
		}
	}()
}

// write is one database job on the writer goroutine. A panic in it is
// logged and the writer goes on with the next job.
func (g *gen) write(ids map[int]uuid.UUID, j job) {
	core, id, log := g.r.core, g.r.id, g.r.log
	defer func() {
		if v := recover(); v != nil {
			log.Error("handset record writer panicked: job lost", "job", j.kind, "panic", v, "stack", string(debug.Stack()))
		}
	}()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(core.ctx), storeTimeout)
	defer cancel()
	var err error
	switch j.kind {
	case jobSave:
		var saved Saved
		saved, err = core.Records.SaveRecord(ctx, id, ids[j.i], j.owner, j.rec)
		clear(j.rec.UAK[:])
		if err == nil {
			hid := saved.ID
			if saved.Enrolled {
				log.Info("handset enrolled: record stored", "handset", hid, "record", j.rec.Slot, "pooled", saved.Pooled)
			}
			ids[j.i] = hid
			if saved.Enrolled && !saved.Pooled {
				// it stays in this chip slot: the cell's goroutine learns whose record it is
				core.post(id, event{kind: evHome, hid: hid, idx: j.i})
			}
			if j.window != uuid.Nil && core.Reg != nil {
				core.Reg.Enrolled(j.window, hid)
			}
			if saved.Enrolled && saved.Pooled {
				core.enrolled(id, hid, j.i, saved.Again)
			}
		}
	case jobReenrol:
		hid, ok := ids[j.i]
		if !ok || core.Reg == nil {
			break
		}
		owner, window, claimed := core.Reg.Claim(id, j.pin)
		switch {
		case !claimed:
		case owner != j.owner:
			// Not built: a handset changing hands. Its key changed all the same
			// (the stack accepted the PIN), so say so rather than hide it.
			log.Warn("a handset enrolled again through another user's window: the window is not completed", "handset", hid)
		default:
			core.Reg.Enrolled(window, hid)
			log.Info("handset enrolled again: record stored", "handset", hid, "record", j.i)
		}
	case jobTrack:
		ids[j.i] = j.hid
	case jobUntrack:
		delete(ids, j.i)
		err = core.Records.Unplace(ctx, id, j.hid)
	case jobRelease:
		delete(ids, j.i)
		err = core.Records.ReleaseChipSlot(ctx, id, j.hid)
	case jobForget:
		if hid, ok := ids[j.i]; ok {
			var pooled bool
			pooled, err = core.Records.Forget(ctx, id, hid)
			delete(ids, j.i)
			if err == nil && pooled {
				core.forgotten(id, hid)
			}
		}
	case jobCaps:
		if hid, ok := ids[j.i]; ok {
			err = core.Records.SetCapabilities(ctx, hid, j.caps)
		}
	case jobSignal:
		if hid, ok := ids[j.i]; ok {
			err = core.Records.SetSignal(ctx, hid, j.dbm, j.at)
		}
	case jobForgetHandset:
		var pooled bool
		pooled, err = core.Records.Forget(ctx, id, j.hid)
		if err == nil && pooled {
			core.forgotten(id, j.hid)
		}
	case jobRename:
		if hid, ok := ids[j.i]; ok {
			err = core.Records.Rename(ctx, hid, j.name)
		}
	case jobInstalled:
		err = core.Records.Installed(ctx, id, j.slots)
	case jobUnknown:
		err = core.Records.SlotUnknown(ctx, id, j.slot)
	case jobSeen:
		if hid, ok := ids[j.i]; ok {
			var prev uuid.UUID
			if prev, err = core.Records.Seen(ctx, id, hid, j.at); err == nil && prev != uuid.Nil && prev != id {
				log.Info("handset moved to this cell", "handset", hid, "from", prev)
			}
		}
	}
	if err != nil {
		log.Error("handset record not stored", "job", j.kind, "err", err)
	}
}

// stop lets the writer finish what is queued, then end.
func (g *gen) stop() { close(g.jobs) }

// stack.Env, on the cell's goroutine.

func (g *gen) Send(m stack.Mail) error { return g.r.send(0, m) }
func (g *gen) Room() bool              { return true }

func (g *gen) Random(b []byte) {
	if _, err := rand.Read(b); err != nil {
		panic("core: no randomness: " + err.Error())
	}
}

func (g *gen) Log(line string) {
	g.r.log.Info(line)
	// The library has no callback for a failed registration; its log line
	// is the signal (ADR 0005), less a model's own PIN 0000 try.
	if reg := g.r.core.Reg; reg != nil && strings.Contains(line, "reg: registration rejected") &&
		!strings.Contains(line, "(expected:") {
		reg.Failed(g.r.id)
	}
}

func (g *gen) Event(to stack.To, line string) {
	line = strings.TrimSpace(line)
	g.r.log.Debug("bridge line", "line", line)
	if verb, kv := parseBridgeLine(line); verb == "call-out" {
		g.r.core.callOut(g.r.id, kv, g.r.handsetBySlot(kv["hs"]))
		return
	}
	g.r.core.bridgeLine(g.r.id, line)
}

// handsetBySlot is the handset whose record number on this cell is slot.
func (r *cellRun) handsetBySlot(slot string) uuid.UUID {
	n, err := strconv.ParseUint(slot, 10, 8)
	if err != nil || r.gen == nil {
		return uuid.Nil
	}
	for i, s := range r.gen.slotOf {
		if s == uint(n) {
			return r.hidOf[i]
		}
	}
	return uuid.Nil
}

func (g *gen) Save(i int, rec stack.Record) {
	var owner, window uuid.UUID
	if _, ok := g.slotOf[i]; !ok {
		// An enrolment: the user of the PIN it proved owns the handset.
		// Without a live PIN it is refused (the maintainers' notes point 2). The
		// record keeps its RegPIN1 for the run, so only this first save
		// claims the PIN.
		ok := false
		if reg := g.r.core.Reg; reg != nil && rec.RegPIN1 != 0 {
			owner, window, ok = reg.Claim(g.r.id, rec.RegPIN1)
		}
		if !ok || owner == uuid.Nil {
			clear(rec.UAK[:])
			g.r.refused = true
			return
		}
		g.place(i, rec.Slot, owner)
	} else if rec.RegPIN1 != 0 {
		g.rePIN[i] = rec.RegPIN1
	}
	g.job(job{kind: jobSave, i: i, rec: rec, owner: owner, window: window})
}

func (g *gen) Fetch(ipui []byte) { g.r.core.fetch(g.r.id, ipui) }

func (g *gen) Forget(i int, why string) {
	g.r.log.Info("handset deregistered", "record", i, "why", why)
	if hid, ok := g.r.hidOf[i]; ok { // not recycled: the stack made the index a free store
		delete(g.r.trans, hid)
		delete(g.r.idxOf, hid)
		delete(g.r.hidOf, i)
	}
	if slot, ok := g.slotOf[i]; ok {
		delete(g.owner, slot)
		delete(g.slotOf, i)
	}
	g.job(job{kind: jobForget, i: i})
}

func (g *gen) Named(i int, name string) { g.job(job{kind: jobRename, i: i, name: name}) }

// NameVisible: a handset sees the handsets of its own user's account
// (list-access.md §1.1).
func (g *gen) NameVisible(viewer, slot uint) bool {
	if viewer == slot {
		return true
	}
	ov, ok1 := g.owner[viewer]
	os, ok2 := g.owner[slot]
	return ok1 && ok2 && ov == os
}

func (g *gen) Switched(cur int, why string) {
	g.r.log.Debug("record current", "record", cur, "why", why)
}
func (g *gen) Registered(cur int) {
	g.r.log.Info("handset registered", "record", cur)
	// A handset that enrolled again (its record was known) completes the
	// window of the PIN it proved, as a new one does when it is saved.
	if pin := g.rePIN[cur]; pin != 0 {
		delete(g.rePIN, cur)
		g.job(job{kind: jobReenrol, i: cur, pin: pin, owner: g.owner[g.slotOf[cur]]})
	}
}
func (g *gen) CapLearned(i int, c stack.Capability) {
	g.r.log.Debug("handset capability learned", "record", i)
	g.job(job{kind: jobCaps, i: i, caps: capabilityText(c)})
}

// Clock: the controller's clock is trusted (NTP on its host).
func (g *gen) Clock() (time.Time, bool) { return time.Now(), true }

func (g *gen) Media(handle, channel uint8, wideband, up bool) {
	// MEDIA_OPEN and MEDIA_CLOSE are step 6 of C3.
	g.r.log.Debug("audio", "handle", handle, "channel", channel, "wideband", wideband, "up", up)
}

// rfpiText is what the cell's chip broadcasts, for the log ("" when the cell does not say).
func rfpiText(up cell.CellUp) string {
	if !up.HasRFPI {
		return ""
	}
	return hex.EncodeToString(up.RFPI[:])
}

// capabilityText is what the page shows of a terminal capability (EN 300 175-5
// 7.7.41): the standard the handset claims, its display and tone capability,
// and the profile indicator octets as it sent them.
func capabilityText(c stack.Capability) map[string]any {
	standard := "GAP (no NG-DECT)"
	switch {
	case c.NGPart3:
		standard = "NG-DECT Part 3 (CAT-iq)"
	case c.NGPart1:
		standard = "NG-DECT Part 1 (wideband)"
	}
	display := map[uint8]string{0: "not applicable", 1: "no display", 2: "numeric", 3: "numeric plus", 4: "alphanumeric", 5: "full display"}
	tone := map[uint8]string{0: "not applicable", 1: "no tones", 2: "dial tone only", 3: "ITU-T E.182 tones", 4: "complete DECT tones"}
	name := func(m map[uint8]string, v uint8) string {
		if s, ok := m[v]; ok {
			return s
		}
		return fmt.Sprintf("code %d", v)
	}
	out := map[string]any{"standard": standard, "display": name(display, c.Display), "tone": name(tone, c.Tone)}
	if len(c.Profiles) > 0 {
		out["profiles"] = hex.EncodeToString(c.Profiles)
	}
	return out
}
