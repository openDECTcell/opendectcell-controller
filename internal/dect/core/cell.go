// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"crypto/rand"
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

	up      cell.CellUp // the last HELLO
	linkUp  bool
	st      stack.Stack
	gen     *gen
	bring   []stack.Mail // start-up mails still to send, one per BringUp
	nextAt  time.Time
	slots   []uint // the slots the records went into, for Installed
	install bool   // the radio is on: the records go in at the next tick
	retryAt time.Time
	noStack bool // said once: this binary has no libopendectcell
	// resumed: the session resumed, so the radio may carry calls and is
	// never switched off for the bring-up (the maintainers' notes)
	resumed bool
	// refused: an enrolment came without a registration window; the stack
	// is rebuilt once the call into it has returned, so the handset's key
	// leaves the stack and the chip
	refused bool

	corr     uint16
	corrSlot map[uint16]uint // slot writes in flight, by corr (§7.4)
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
		r.log.Error("enrolment without a registration window refused: DECT stack rebuilt")
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
	case evReject:
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

func (r *cellRun) tick() {
	now := time.Now()
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
			r.log.Info("cell radio on")
		}
	}
	r.st.Tick(r.core.now())
	if r.install {
		// dect_base's install() ran in that tick (host.c, cdect_host_tick)
		r.install = false
		r.gen.job(job{kind: jobInstalled, slots: slices.Clone(r.slots)})
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
	st := r.core.New(g, stack.Config{NamesRO: true, UAKTable: table, CheckDelayMs: delay, Stub: r.up.StubVersion})
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
)

type job struct {
	kind  jobKind
	i     int
	owner uuid.UUID // an enrolment's
	rec   stack.Record
	name  string
	slot  uint
	slots []uint
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

func (g *gen) Log(line string) { g.r.log.Info(line) }

func (g *gen) Event(to stack.To, line string) {
	// The bridge to the SIP side is step 5 of C3.
	g.r.log.Debug("bridge line, no bridge yet", "line", strings.TrimSpace(line))
}

func (g *gen) Save(i int, rec stack.Record) {
	var owner uuid.UUID
	if _, ok := g.slotOf[i]; !ok {
		// An enrolment: the open window's user owns the handset. Without
		// a window it is refused (the maintainers' notes point 2).
		ok := false
		if w := g.r.core.Window; w != nil {
			owner, ok = w(g.r.id)
		}
		if !ok || owner == uuid.Nil {
			clear(rec.UAK[:])
			g.r.refused = true
			return
		}
		g.place(i, rec.Slot, owner)
	}
	g.job(job{kind: jobSave, i: i, rec: rec, owner: owner})
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
