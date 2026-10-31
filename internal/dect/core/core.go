// SPDX-License-Identifier: AGPL-3.0-or-later

// Package core is the controller's DECT core (C3): one base stack per
// active cell, fed by the cell's MAIL_UP, sending MAIL_DOWN, ticked by the
// controller's clock, its records kept encrypted in the database
// (the maintainers' notes step 2, ../opendectcell/docs/hosting-dect-base.md).
//
// Each cell has one goroutine that owns its stack; the cell link's session
// goroutine only posts to it and never blocks. Database writes go through a
// second, ordered goroutine per stack, so a slow database never stalls the
// stack's timers.
package core

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/cell"
	"github.com/openDECTcell/opendectcell-controller/internal/cell/wire"
	"github.com/openDECTcell/opendectcell-controller/internal/dect/stack"
)

// Link is the cell link (cell.Server).
type Link interface {
	SendMail(id uuid.UUID, corr uint16, m wire.MailDownMsg) error
}

// Defaults.
const (
	DefaultTick      = 10 * time.Millisecond  // dect-stubd's loop in its traces
	DefaultBringUp   = 100 * time.Millisecond // one start-up mail per loop pass, as dect-stubd
	DefaultRetry     = 5 * time.Second        // after the records could not be loaded
	inboxSize        = 1024
	saveQueue        = 64
	storeTimeout     = 10 * time.Second
	firstMonotonicMs = 1000
)

// Core runs the DECT stacks of the controller's cells. It is the cell
// link's cell.MailSink and cell.CellWatcher. Set the fields, then Start.
type Core struct {
	Link    Link
	Records Records
	New     stack.New
	Log     *slog.Logger
	// Reg is the registration windows (adr/0005-registration-flow.md); a
	// handset enrols only through one (the maintainers' notes point 2). nil: none is
	// ever open.
	Reg Registrations

	// Tick, BringUp and Retry default to the constants above; tests
	// shorten them.
	Tick, BringUp, Retry time.Duration

	mu    sync.Mutex
	ctx   context.Context
	start time.Time
	cells map[uuid.UUID]*cellRun
	wg    sync.WaitGroup
}

// Registrations is what the core needs of the registration windows
// (internal/dect/regwin). Its methods run on the cells' goroutines, some
// inside a stack call: they must not block.
type Registrations interface {
	// PINs is every cell's list of live registration PINs by index ("" =
	// unused; empty: registration closed on cell), when the cells' window
	// closes, and a version that changes with either.
	PINs(cell uuid.UUID) (ver uint64, pins []string, until time.Time)
	// Claim binds an enrolment on cell to the window of the PIN it proved
	// (pin1 = 1 + its index): the handset's owner, and the window.
	Claim(cell uuid.UUID, pin1 uint) (owner, window uuid.UUID, ok bool)
	// Enrolled: the window's handset is stored.
	Enrolled(window, handset uuid.UUID)
	// Failed: a registration attempt on cell was rejected.
	Failed(cell uuid.UUID)
}

var (
	_ cell.MailSink    = (*Core)(nil)
	_ cell.CellWatcher = (*Core)(nil)
)

// Start sets the core up; call it before the cell link serves. The stacks
// end with ctx.
func (c *Core) Start(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Log == nil {
		c.Log = slog.Default()
	}
	for p, d := range map[*time.Duration]time.Duration{&c.Tick: DefaultTick, &c.BringUp: DefaultBringUp, &c.Retry: DefaultRetry} {
		if *p == 0 {
			*p = d
		}
	}
	c.ctx, c.start = ctx, time.Now()
	c.cells = map[uuid.UUID]*cellRun{}
}

// Wait returns once ctx has ended and every stack is closed and its
// records written.
func (c *Core) Wait() {
	c.mu.Lock()
	ctx := c.ctx
	c.mu.Unlock()
	if ctx != nil {
		<-ctx.Done()
	}
	c.wg.Wait()
}

// now is the stacks' monotonic clock in milliseconds.
func (c *Core) now() uint64 {
	return uint64(time.Since(c.start)/time.Millisecond) + firstMonotonicMs
}

// post hands an event to the cell's goroutine, starting it if need be. It
// never blocks: a full inbox drops the event, loudly.
func (c *Core) post(id uuid.UUID, ev event) {
	c.mu.Lock()
	if c.ctx == nil || c.ctx.Err() != nil {
		c.mu.Unlock()
		return
	}
	r := c.cells[id]
	if r == nil {
		r = &cellRun{core: c, id: id, in: make(chan event, inboxSize),
			log: c.Log.With("cell", id)}
		c.cells[id] = r
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			r.run(c.ctx)
		}()
	}
	c.mu.Unlock()
	select {
	case r.in <- ev:
	default:
		r.log.Error("DECT core inbox full: event dropped", "kind", ev.kind)
	}
}

type evKind int

const (
	evUp evKind = iota
	evDown
	evGone
	evMail
	evReject
)

func (k evKind) String() string {
	return [...]string{"up", "down", "gone", "mail", "reject"}[k]
}

type event struct {
	kind   evKind
	up     cell.CellUp
	mail   stack.Mail
	reject wire.MailRejectMsg
}

// MailUp takes a mail from the cell (§7). The parameters are copied: the
// caller's slice is only valid during the call.
func (c *Core) MailUp(id uuid.UUID, m wire.MailUpMsg) {
	c.post(id, event{kind: evMail, mail: stack.Mail{Src: m.Src, Dst: m.Dst, Prim: m.Prim,
		Param: append([]byte(nil), m.Param...)}})
}

func (c *Core) MailReject(id uuid.UUID, m wire.MailRejectMsg) {
	c.post(id, event{kind: evReject, reject: m})
}

func (c *Core) CellUp(id uuid.UUID, up cell.CellUp) { c.post(id, event{kind: evUp, up: up}) }
func (c *Core) CellDown(id uuid.UUID)               { c.post(id, event{kind: evDown}) }
func (c *Core) CellGone(id uuid.UUID)               { c.post(id, event{kind: evGone}) }

// chipConfig is the per-build chip settings (hosting-dect-base.md §6,
// the maintainers' notes, §0i): the STUB build's UAK table, and how long the
// read-back waits after a key write. On both builds a read in the same
// instant gets no answer and from 20 ms it matches (08.25 and 06.88,
// measured 2026-10-31), so 50 ms.
func chipConfig(stub string) (table, delayMs uint32) {
	switch stub {
	case "06.88":
		return 0x067F, 50
	case "08.25":
		return 0x9518, 50
	}
	return 0, 0 // 07.56 and any other build: no table known, no read-back
}

// bringUp is the radio's start: dect-stubd's auto bring-up (stock's order,
// FRITZ!OS 07.56 capture of 2026-10-24), radio and beacon on, registration
// closed. 06.88 boots with its radio on and needs it off before the
// configuration (openDECTcell docs/7412-spi-tdm-and-loader.md §1b); so does a
// cell whose radio is on already. Never on a resumed session: the radio
// may carry calls (the maintainers' notes).
func bringUp(stub string, radioOn, resumed bool) []stack.Mail {
	var ms []stack.Mail
	if (stub == "06.88" || radioOn) && !resumed {
		ms = append(ms, stack.Mail{Src: 0x22, Dst: 0x02, Prim: 0x015B, Param: []byte{0x00}})
	}
	return append(ms,
		stack.Mail{Src: 0x14, Dst: 0x02, Prim: 0x0153, Param: []byte{0x00}}, // ECO_CONFIG
		stack.Mail{Src: 0x14, Dst: 0x02, Prim: 0x0169, Param: []byte{0x07}}, // FEATURE_FLAGS
		stack.Mail{Src: 0x14, Dst: 0x02, Prim: 0x0157, Param: []byte{0x00}}, // CAPABILITY_PROFILE
		stack.Mail{Src: 0x22, Dst: 0x02, Prim: 0x015B, Param: []byte{0x01}}, // radio and beacon on
		stack.Mail{Src: 0x22, Dst: 0x02, Prim: 0x022C, Param: []byte{0x01}}, // registration closed
	)
}
