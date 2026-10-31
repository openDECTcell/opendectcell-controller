// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"strings"
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
	slots       []uint // the slots the records went into, for Installed
	install     bool   // the radio is on: the records go in at the next tick
	retryAt     time.Time
	// the last contact written for each record, and when
	seenMs    map[int]uint64
	seenAt    map[int]time.Time
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
	case evGone:
		r.linkUp = false
		if r.st != nil {
			r.log.Info("DECT stack ended: the cell is gone")
		}
		r.drop()
	case evMail:
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
			r.radioAt = time.Now()
			r.log.Info("cell radio on")
		}
	}
	r.registration(now)
	r.st.Tick(r.core.now())
	r.seen(now)
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
		r.seenAt, r.seenMs = map[int]time.Time{}, map[int]uint64{}
	}
	for i := range r.gen.slotOf {
		ms := r.st.Seen(i)
		if ms == 0 || ms == r.seenMs[i] || now.Sub(r.seenAt[i]) < seenEvery {
			continue
		}
		r.seenMs[i], r.seenAt[i] = ms, now
		// the stack's clock is monotonic ms; the contact was this long ago
		ago := time.Duration(r.core.now()-ms) * time.Millisecond
		r.gen.job(job{kind: jobSeen, i: i, at: now.Add(-ago)})
	}
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
		r.log.Error("cell full: a home-set handset has no slot here (cell-protocol.md §7.4)", "handset", id)
	}
	table, delay := chipConfig(r.up.StubVersion)
	g := &gen{r: r, slotOf: map[int]uint{}, owner: map[uint]uuid.UUID{}, jobs: make(chan job, saveQueue)}
	defer func() {
		for i := range hs {
			clear(hs[i].Rec.UAK[:])
		}
	}()
	// r.st and r.gen are set as soon as they exist, so a panic from here
	// on leaves them for guard's drop: the keys are wiped, the writer ends.
	st := r.core.New(g, stack.Config{NamesRO: true, UAKTable: table, CheckDelayMs: delay, Stub: r.up.StubVersion, ParkPLI: r.parkPLI})
	r.st = st
	ids := map[int]uuid.UUID{}
	r.slots = r.slots[:0]
	for i, h := range hs {
		st.AddRecord(i, h.Rec)
		ids[i] = h.ID
		g.place(i, h.Rec.Slot, h.Owner)
		r.slots = append(r.slots, h.Rec.Slot)
	}
	for i := len(hs); i < max; i++ {
		st.AddFreeStore(i)
	}
	g.start(ids)
	r.gen = g
	if err := st.Start(); err != nil {
		r.drop()
		r.log.Error("DECT stack did not start", "err", err)
		return
	}
	r.bring = bringUp(r.up.StubVersion, r.up.RadioState == 1, r.resumed)
	r.nextAt = time.Now()
	r.log.Info("DECT stack started", "records", len(hs), "free", max-len(hs), "stub", r.up.StubVersion)
}

// drop closes the stack. The fields are cleared first, so a panic in
// Close cannot close anything twice.
func (r *cellRun) drop() {
	st, g := r.st, r.gen
	r.st, r.gen = nil, nil
	r.bring, r.install = nil, false
	r.seenMs, r.seenAt = nil, nil
	r.radioOn, r.regSet, r.regUntil = false, false, time.Time{}
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
}

type jobKind int

const (
	jobSave jobKind = iota
	jobForget
	jobRename
	jobInstalled
	jobUnknown
	jobSeen
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
	at     time.Time // jobSeen: when the handset was heard
}

// place notes record i in chip slot with its owner, for NameVisible.
func (g *gen) place(i int, slot uint, owner uuid.UUID) {
	if old, ok := g.slotOf[i]; ok && old != slot {
		delete(g.owner, old)
	}
	g.slotOf[i] = slot
	g.owner[slot] = owner
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
		var hid uuid.UUID
		hid, err = core.Records.SaveRecord(ctx, id, ids[j.i], j.owner, j.rec)
		clear(j.rec.UAK[:])
		if err == nil {
			if ids[j.i] == uuid.Nil {
				log.Info("handset enrolled: record stored", "handset", hid, "slot", j.rec.Slot)
			}
			ids[j.i] = hid
			if j.window != uuid.Nil && core.Reg != nil {
				core.Reg.Enrolled(j.window, hid)
			}
		}
	case jobForget:
		if hid, ok := ids[j.i]; ok {
			err = core.Records.Forget(ctx, id, hid)
			delete(ids, j.i)
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
	// The bridge to the SIP side is step 5 of C3.
	g.r.log.Debug("bridge line, no bridge yet", "line", strings.TrimSpace(line))
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
	}
	g.job(job{kind: jobSave, i: i, rec: rec, owner: owner, window: window})
}

func (g *gen) Forget(i int, why string) {
	g.r.log.Info("handset deregistered", "record", i, "why", why)
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
func (g *gen) Registered(cur int) { g.r.log.Info("handset registered", "record", cur) }
func (g *gen) CapLearned(i int)   { g.r.log.Debug("handset capability learned", "record", i) }

// Clock: the controller's clock is trusted (NTP on its host).
func (g *gen) Clock() (time.Time, bool) { return time.Now(), true }

func (g *gen) Media(handle, channel uint8, wideband, up bool) {
	// MEDIA_OPEN and MEDIA_CLOSE are step 6 of C3.
	g.r.log.Debug("audio", "handle", handle, "channel", channel, "wideband", wideband, "up", up)
}
