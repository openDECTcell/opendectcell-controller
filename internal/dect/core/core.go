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
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/openDECTcell/opendectcell-controller/internal/cell"
	"github.com/openDECTcell/opendectcell-controller/internal/cell/wire"
	"github.com/openDECTcell/opendectcell-controller/internal/dect/profiles"
	"github.com/openDECTcell/opendectcell-controller/internal/dect/stack"
)

// Link is the cell link (cell.Server).
type Link interface {
	SendMail(id uuid.UUID, corr uint16, m wire.MailDownMsg) error
	// SetIdentity asks an active cell to broadcast rfpi, or with nil to go
	// back to its own identity (CONFIG_SET tag 6).
	SetIdentity(id uuid.UUID, rfpi *[5]byte) error
}

// Identity is the system identity the cells share (cell.Store). nil in Core:
// every cell keeps its box's own identity.
type Identity interface {
	// Want says what the cell should broadcast.
	Want(ctx context.Context, id uuid.UUID) (cell.IdentityWant, error)
	// Pushed records what the cell was told: an RFPI, or nil for its own.
	Pushed(ctx context.Context, id uuid.UUID, rfpi *[5]byte) error
}

// Defaults.
const (
	DefaultTick         = 10 * time.Millisecond  // dect-stubd's loop in its traces
	DefaultBringUp      = 100 * time.Millisecond // one start-up mail per loop pass, as dect-stubd
	DefaultRetry        = 5 * time.Second        // after the records could not be loaded
	DefaultPageDelay    = 3 * time.Second        // <LCE.03>: a page is repeated after this
	DefaultRingWaveSize = 8                      // cells paged together after the last-heard one
	DefaultRingMaxCells = 24                     // cells a call is offered to in all
	inboxSize           = 1024
	saveQueue           = 64
	storeTimeout        = 10 * time.Second
	firstMonotonicMs    = 1000
)

// Core runs the DECT stacks of the controller's cells. It is the cell
// link's cell.MailSink and cell.CellWatcher. Set the fields, then Start.
type Core struct {
	Link    Link
	Records Records
	// Identity gives the cells the system's DECT identity; nil leaves them
	// their own.
	Identity Identity
	New      stack.New
	Log      *slog.Logger
	// Reg is the registration windows (adr/0005-registration-flow.md); a
	// handset enrols only through one (the maintainers' notes point 2). nil: none is
	// ever open.
	Reg Registrations
	// Profiles is the device profiles; nil: the catalogue built in.
	Profiles *profiles.Catalogue

	// Tick, BringUp and Retry default to the constants above; tests
	// shorten them.
	Tick, BringUp, Retry time.Duration

	// PageDelay is how long the cell a handset was last on is paged alone,
	// before the others are (route.go); 0 is DefaultPageDelay.
	PageDelay time.Duration

	// MWIFor gives the lines of a handset with the number of new messages in
	// each mailbox, for a handset that comes into a cell's table; nil: none
	// (mwi.go).
	MWIFor func(ctx context.Context, hid uuid.UUID) []MWILine

	// Contacts is the handsets' phonebook (contacts.go); nil: they have none.
	Contacts Contacts
	// RingWaveSize is how many more cells are paged each PageDelay while a
	// call is unanswered, RingMaxCells how many cells it is offered to in all
	// (the last-heard one included); 0 are the defaults (route.go).
	RingWaveSize, RingMaxCells int

	// Originate takes a call a handset placed to the SIP side (outgoing.go);
	// it runs on its own goroutine and answers through the Outgoing. nil: a
	// handset's call is released as unreachable.
	Originate func(*Outgoing)

	calls routes

	// Roaming gives the admin's roaming settings; nil: the defaults. Read at
	// most every roamingEvery.
	Roaming func(ctx context.Context) (Roaming, error)
	rmAt    time.Time
	rm      Roaming
	rmMu    sync.Mutex

	fetchSem chan struct{}
	heardMu  sync.Mutex
	heardOn  map[uuid.UUID]heard

	mu    sync.Mutex
	ctx   context.Context
	start time.Time
	cells map[uuid.UUID]*cellRun
	wg    sync.WaitGroup

	// EnforceTrust runs one at a time (mu guards both).
	trustRunning, trustAgain bool
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
	c.fetchSem = make(chan struct{}, fetchLimit)
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

// Clock is the stacks' monotonic clock in milliseconds: what a stack in a worker
// process is given, to keep time with this one.
func (c *Core) Clock() uint64 { return c.now() }

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
	evRadio
	evFetched      // the answer to a Fetch: the handset's record, or none
	evEvict        // take this handset out of the cell's table (its key or registration changed)
	evReload       // rebuild the stack soon, from the records
	evRecycle      // a pooled handset enrolled into a chip slot: free the slot in place
	evCtl          // a bridge command for the stack (route.go)
	evHome         // a handset enrolled into a chip slot of this cell and stays there: record idx is its
	evInfo         // a handset's name or device profile changed: tell the stack that holds it
	evDereg        // the user asks for the handset to be deregistered
	evPump         // a stack in a worker process has callbacks waiting: run them here (stack.Pumper)
	evContacts     // somebody's contacts changed (hid: the owner, uuid.Nil: all): the handsets get a new Contact List
	evContactsDone // a change a handset made to its Contact List was stored: hid, seq, contact, entry
	evCalls        // the call lists of these handsets (hids) changed
	evDirectory    // handsets moved between tables or changed name: the directory is read again
)

func (k evKind) String() string {
	return [...]string{"up", "down", "gone", "mail", "reject", "radio", "fetched", "evict", "reload", "recycle", "ctl", "home", "info", "dereg", "pump", "contacts", "contactsdone", "calls", "directory"}[k]
}

type event struct {
	kind    evKind
	up      cell.CellUp
	mail    stack.Mail
	reject  wire.MailRejectMsg
	radio   uint8 // evRadio: the cell's radio and chip state
	chip    uint8
	hid     uuid.UUID   // evEvict, evRecycle, evCtl: the handset
	idx     int         // evRecycle: its record in the stack
	line    string      // evCtl: the command; {hs} is the handset's record number on the cell
	name    string      // evInfo: the handset's name
	force   bool        // evDereg: do not ask the handset, it may be out of range
	profile string      // evInfo: the id of its device profile
	sip     string      // evCtl: the call it belongs to
	hids    []uuid.UUID // evCalls: the handsets whose call lists changed
	seq     uint32      // evContactsDone: the journal sequence of the handset's change
	contact int64       // evContactsDone: the contact it made or changed
	entry   uint16      // evContactsDone: the handset's identifier for it
	// evFetched: the identity asked about, what the store said
	ipui []byte
	h    Handset
	ok   bool
	full bool
	err  error
}

// cellIDs are the cells that have a goroutine now.
func (c *Core) cellIDs() []uuid.UUID {
	c.mu.Lock()
	defer c.mu.Unlock()
	ids := make([]uuid.UUID, 0, len(c.cells))
	for id := range c.cells {
		ids = append(ids, id)
	}
	return ids
}

// enrolled: a pooled handset enrolled on origin into record i, where it sits in
// a chip slot until the record is recycled (cell.go, recycle). A handset that enrolled
// again has a new key and TPUI: the cells that hold it as a roamer drop it.
func (c *Core) enrolled(origin, hid uuid.UUID, i int, again bool) {
	c.post(origin, event{kind: evRecycle, hid: hid, idx: i})
	if again {
		c.evictElsewhere(origin, hid)
	}
}

// HandsetChanged tells the core that the name or the device profile of the
// handset changed (the web UI, a registration wizard). Every cell that holds
// it tells its stack, so the handsets' name lists and the model's habits follow.
func (c *Core) HandsetChanged(hid uuid.UUID) {
	c.mu.Lock()
	ctx := c.ctx
	c.mu.Unlock()
	if ctx == nil || c.Records == nil {
		return
	}
	qctx, cancel := context.WithTimeout(ctx, storeTimeout)
	name, profile, ok, err := c.Records.HandsetInfo(qctx, hid)
	cancel()
	if err != nil {
		c.Log.Warn("a handset's name and profile could not be read", "handset", hid, "err", err)
		return
	}
	if !ok {
		return
	}
	for _, id := range c.cellIDs() {
		c.post(id, event{kind: evInfo, hid: hid, name: name, profile: profile})
	}
}

// Deregister asks for the handset to be deregistered. The cell it was heard on
// last (every cell when none is known) tells it to forget the base; whether it
// answers or not the cell forgets the handset after about 30 s, and the store
// follows (Forget). force skips the asking, for a handset that is out of
// range: every cell that holds it forgets it as soon as its stack is quiet.
func (c *Core) Deregister(hid uuid.UUID, force bool) {
	if h, ok := c.lastHeard(hid); ok && !force {
		c.post(h.cell, event{kind: evDereg, hid: hid})
		return
	}
	for _, id := range c.cellIDs() {
		c.post(id, event{kind: evDereg, hid: hid, force: force})
	}
}

// forgotten: a pooled handset deregistered on origin, which is now without it;
// the other cells drop it too.
func (c *Core) forgotten(origin, hid uuid.UUID) { c.evictElsewhere(origin, hid) }

func (c *Core) evictElsewhere(origin, hid uuid.UUID) {
	for _, id := range c.cellIDs() {
		if id != origin {
			c.post(id, event{kind: evEvict, hid: hid})
		}
	}
}

// fetchLimit bounds the lookups running at once; a cell asks one at a time.
const (
	fetchLimit   = 8
	fetchTimeout = 5 * time.Second
)

// fetch looks the handset with this identity up for a cell that holds no record
// of it (Env.Fetch) and posts the answer. The store is asked off the cell's
// goroutine; the handset waits about 15 s (openDECTcell notes, API 18).
func (c *Core) fetch(cell uuid.UUID, ipui []byte) {
	ip := append([]byte(nil), ipui...)
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		select {
		case c.fetchSem <- struct{}{}:
			defer func() { <-c.fetchSem }()
		case <-c.ctx.Done():
			return
		}
		ctx, cancel := context.WithTimeout(c.ctx, fetchTimeout)
		defer cancel()
		h, ok, err := c.Records.PlaceByIPUI(ctx, cell, ip)
		ev := event{kind: evFetched, ipui: ip, h: h, ok: ok, err: err}
		if errors.Is(err, ErrCellFull) {
			ev.full, ev.err = true, nil
		}
		c.post(cell, ev)
	}()
}

// heard says where a handset was last heard, from the cells' own sightings
// (not the database's, which is written at most every 30 s).
type heard struct {
	cell uuid.UUID
	at   time.Time
}

func (c *Core) noteHeard(hid, cell uuid.UUID, at time.Time) {
	c.heardMu.Lock()
	defer c.heardMu.Unlock()
	if c.heardOn == nil {
		c.heardOn = map[uuid.UUID]heard{}
	}
	if old, ok := c.heardOn[hid]; !ok || at.After(old.at) {
		c.heardOn[hid] = heard{cell, at}
	}
}

func (c *Core) lastHeard(hid uuid.UUID) (heard, bool) {
	c.heardMu.Lock()
	defer c.heardMu.Unlock()
	h, ok := c.heardOn[hid]
	return h, ok
}

// MailUp takes a mail from the cell (§7). The parameters are copied: the
// caller's slice is only valid during the call.
func (c *Core) MailUp(id uuid.UUID, m wire.MailUpMsg) {
	c.post(id, event{kind: evMail, mail: stack.Mail{Src: m.Src, Dst: m.Dst, Prim: m.Prim,
		Param: append([]byte(nil), m.Param...)}})
}

// MailReject takes a MAIL_REJECT from the cell (§7.2): a mail the controller
// sent with a correlation number did not reach the chip. A rejection of RejectChipDown
// restarts the cell's stack; one for a slot write marks that slot unknown.
func (c *Core) MailReject(id uuid.UUID, m wire.MailRejectMsg) {
	c.post(id, event{kind: evReject, reject: m})
}

// CellRadio takes the cell's STATUS: a cell whose daemon restarted reports its
// radio parked while the stack believes it is on.
func (c *Core) CellRadio(id uuid.UUID, radio, chip uint8) {
	c.post(id, event{kind: evRadio, radio: radio, chip: chip})
}

// CellUp is cell.CellWatcher: the cell's tunnel opened or resumed. The stack
// starts on a new session and carries on on a resumed one.
func (c *Core) CellUp(id uuid.UUID, up cell.CellUp) { c.post(id, event{kind: evUp, up: up}) }

// CellDown is cell.CellWatcher: the link is lost, the grace period runs, and the
// stack and its calls stay.
func (c *Core) CellDown(id uuid.UUID) { c.post(id, event{kind: evDown}) }

// CellGone is cell.CellWatcher: the grace period ended, or the cell is no longer
// active. Its stack ends and its calls with it.
func (c *Core) CellGone(id uuid.UUID) { c.post(id, event{kind: evGone}) }

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

// trustStore is what a Records may also do: remove records from cells their owners do not trust.
type trustStore interface {
	DropUntrusted(ctx context.Context) ([]Dropped, error)
}

// EnforceTrust makes the cells' tables follow the owners' trust settings
// (docs/adr/0007-organizations-and-trust.md): a handset's record leaves every cell whose
// operator its owner does not trust. It returns at once; the work runs in the background, one
// run at a time (a call made while one runs makes it run once more). A record in a chip slot
// stays in the chip until the slot is overwritten or the chip reloaded.
func (c *Core) EnforceTrust() {
	c.mu.Lock()
	if c.ctx == nil || c.ctx.Err() != nil {
		c.mu.Unlock()
		return
	}
	if c.trustRunning {
		c.trustAgain = true
		c.mu.Unlock()
		return
	}
	c.trustRunning = true
	ctx := c.ctx
	c.wg.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.wg.Done()
		for {
			c.enforceTrustOnce(ctx)
			c.mu.Lock()
			if !c.trustAgain || ctx.Err() != nil {
				c.trustRunning = false
				c.mu.Unlock()
				return
			}
			c.trustAgain = false
			c.mu.Unlock()
		}
	}()
}

func (c *Core) enforceTrustOnce(ctx context.Context) {
	ts, ok := c.Records.(trustStore)
	if !ok {
		return
	}
	qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	dropped, err := ts.DropUntrusted(qctx)
	cancel()
	if err != nil {
		c.Log.Error("trust settings could not be applied to the cells' tables", "err", err)
		return
	}
	for _, d := range dropped {
		if d.Slot <= maxSlot {
			c.Log.Warn("a handset's key stays in a chip slot until the slot is overwritten", "cell", d.Cell, "handset", d.Handset, "slot", d.Slot)
		}
		c.post(d.Cell, event{kind: evEvict, hid: d.Handset})
	}
	if len(dropped) > 0 {
		c.Log.Info("records removed from cells their owners do not trust", "records", len(dropped))
	}
}
