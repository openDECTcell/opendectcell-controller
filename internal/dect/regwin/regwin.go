// SPDX-License-Identifier: AGPL-3.0-or-later

// Package regwin keeps the registration windows (docs/adr/0005-registration-flow.md,
// the maintainers' notes): one PIN per user's window, valid 60 s, single use, all live
// PINs on every cell that can take a handset. A handset proves one PIN; the
// stack reports which by its index (libopendectcell API 139, reg_pin1), and the
// index names the window and so the user.
//
// The PINs are in clear here and in the stacks, which need them; they are
// never written to the database or a log (threat-model.md §5).
package regwin

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/google/uuid"
)

// Limits (ADR 0005).
const (
	MaxPINs     = 32               // libopendectcell's DECT_MM_REG_PINS_MAX
	Lifetime    = 60 * time.Second // a PIN's life, never extended
	Grace       = 30 * time.Second // an expired PIN still binds a link set up before
	Quarantine  = 2 * time.Minute  // an index is not reused sooner after its PIN left the lists
	PerHour     = 10               // windows per user per hour
	FailLimit   = 5                // failed attempts per cell ...
	FailWindow  = time.Minute      // ... within this ...
	Lockout     = 5 * time.Minute  // ... close the cell's registration this long
	keepClosed  = 10 * time.Minute // a closed window stays visible for polling
	rateHorizon = time.Hour
)

// Errors of Open.
var (
	ErrWindowOpen  = errors.New("regwin: a window of this user is open")
	ErrRateLimited = errors.New("regwin: too many windows this hour")
	ErrBusy        = errors.New("regwin: too many live PINs")
	ErrDigits      = errors.New("regwin: a PIN has 8 or 6 digits")
)

// State is a window's state, as openapi.yaml names it.
type State string

const (
	Open      State = "open"
	Completed State = "completed"
	Expired   State = "expired"
	Cancelled State = "cancelled"
)

// Window is one user's registration window.
type Window struct {
	ID        uuid.UUID
	Owner     uuid.UUID // the handset's owner
	OpenedBy  uuid.UUID // the caller: the owner, or an admin
	State     State
	OpenedAt  time.Time
	ExpiresAt time.Time
	ClosedAt  time.Time
	Cell      uuid.UUID // where the handset registered, once completed
	Handset   uuid.UUID // once stored
	Name      string    // what the user called the handset, before registering
	Profile   string    // the id of the device profile they chose ("": none)
}

// entry is one PIN index.
type entry struct {
	pin string
	win *Window
	out time.Time // when the PIN left the lists; zero while live or never used
}

// Registry is the controller's set of windows. The zero value is not
// usable: call New.
type Registry struct {
	// Done, if set, is called (without the lock) when a window's handset is
	// stored: it may write to the database, it must not call back in.
	Done func(Window)

	now  func() time.Time
	rand io.Reader
	log  *slog.Logger

	ver atomic.Uint64

	mu      sync.Mutex
	slots   [MaxPINs]entry
	windows map[uuid.UUID]*Window
	opened  map[uuid.UUID][]time.Time // per caller, for PerHour
	fails   map[uuid.UUID][]time.Time
	locked  map[uuid.UUID]time.Time // cell -> until
	next    time.Time               // the earliest time something expires
	pins    []string                // the lists' cached snapshot
	until   time.Time
}

// New makes a registry. now and r may be nil (time.Now, crypto/rand).
func New(log *slog.Logger, now func() time.Time, r io.Reader) *Registry {
	if now == nil {
		now = time.Now
	}
	if r == nil {
		r = rand.Reader
	}
	if log == nil {
		log = slog.Default()
	}
	return &Registry{now: now, rand: r, log: log, windows: map[uuid.UUID]*Window{},
		opened: map[uuid.UUID][]time.Time{}, fails: map[uuid.UUID][]time.Time{},
		locked: map[uuid.UUID]time.Time{}}
}

// Open issues a PIN of digits (8, or 6 for a handset that refuses 8) for
// owner's window, opened by caller. The PIN is shown once.
func (g *Registry) Open(owner, caller uuid.UUID, digits int) (Window, string, error) {
	if digits == 0 {
		digits = 8
	}
	if digits != 8 && digits != 6 {
		return Window{}, "", ErrDigits
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	g.sweep(now)
	for _, w := range g.windows {
		if w.OpenedBy == caller && w.State == Open {
			return Window{}, "", ErrWindowOpen
		}
	}
	recent := g.opened[caller][:0]
	for _, t := range g.opened[caller] {
		if now.Sub(t) < rateHorizon {
			recent = append(recent, t)
		}
	}
	g.opened[caller] = recent
	if len(recent) >= PerHour {
		return Window{}, "", ErrRateLimited
	}
	k := -1
	for i := range g.slots {
		if e := &g.slots[i]; e.win == nil || (!e.out.IsZero() && now.Sub(e.out) >= Quarantine) {
			k = i
			break
		}
	}
	if k < 0 {
		return Window{}, "", ErrBusy
	}
	pin, err := g.newPIN(digits)
	if err != nil {
		return Window{}, "", err
	}
	w := &Window{ID: uuid.New(), Owner: owner, OpenedBy: caller, State: Open,
		OpenedAt: now, ExpiresAt: now.Add(Lifetime)}
	g.windows[w.ID] = w
	g.slots[k] = entry{pin: pin, win: w}
	g.opened[caller] = append(g.opened[caller], now)
	g.changed(now)
	g.log.Info("registration window opened", "window", w.ID, "owner", owner, "by", caller, "index", k)
	return *w, pin, nil
}

// newPIN draws digits decimal digits, unequal to every PIN the lists may
// still name. Called with mu held.
func (g *Registry) newPIN(digits int) (string, error) {
	max := big.NewInt(1)
	for range digits {
		max.Mul(max, big.NewInt(10))
	}
	for range 100 {
		n, err := rand.Int(g.rand, max)
		if err != nil {
			return "", fmt.Errorf("regwin: no randomness: %w", err)
		}
		pin := fmt.Sprintf("%0*d", digits, n)
		dup := false
		for i := range g.slots {
			if g.slots[i].win != nil && g.slots[i].pin == pin {
				dup = true
			}
		}
		if !dup {
			return pin, nil
		}
	}
	return "", errors.New("regwin: no unique PIN drawn")
}

// Get returns window id; whether the caller may see it is the API's check.
func (g *Registry) Get(id uuid.UUID) (Window, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sweep(g.now())
	w, ok := g.windows[id]
	if !ok {
		return Window{}, false
	}
	return *w, true
}

// Cancel closes an open window; its PIN leaves every cell.
func (g *Registry) Cancel(id uuid.UUID) (Window, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	g.sweep(now)
	w, ok := g.windows[id]
	if !ok {
		return Window{}, false
	}
	if w.State == Open {
		g.close(w, Cancelled, now)
		g.log.Info("registration window cancelled", "window", w.ID)
	}
	return *w, true
}

// PINs is the list every cell's stack gets: the live PINs at their
// indices, "" for an unused index, empty when none is live or the cell is
// locked out; until is when the cells' window closes (60 s after the
// latest opening); ver changes with any of it. The slice is shared:
// read it, never write it. It does not block.
func (g *Registry) PINs(cell uuid.UUID) (ver uint64, pins []string, until time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	g.sweep(now)
	if t, ok := g.locked[cell]; ok && now.Before(t) {
		return g.ver.Load(), nil, time.Time{}
	}
	return g.ver.Load(), g.pins, g.until
}

// Claim binds an enrolment on cell to the window of the PIN it proved
// (pin1 = 1 + its index, the record's reg_pin1). A live PIN, or one
// expired within Grace, is spent; ok is false for anything else.
func (g *Registry) Claim(cell uuid.UUID, pin1 uint) (owner, window uuid.UUID, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	g.sweep(now)
	if pin1 < 1 || pin1 > MaxPINs {
		return uuid.Nil, uuid.Nil, false
	}
	w := g.slots[pin1-1].win
	switch {
	case w == nil:
		return uuid.Nil, uuid.Nil, false
	case w.State == Expired && now.Before(w.ExpiresAt.Add(Grace)):
	case w.State != Open:
		g.log.Warn("enrolment with a spent registration PIN refused", "window", w.ID, "state", w.State, "cell", cell)
		return uuid.Nil, uuid.Nil, false
	}
	if w.State == Open {
		g.close(w, Completed, now)
	} else {
		w.State = Completed
	}
	w.Cell = cell
	g.log.Info("handset registered through a window", "window", w.ID, "owner", w.Owner, "cell", cell)
	return w.Owner, w.ID, true
}

// MaxName is the longest handset name a window takes.
const MaxName = 64

// SetHandset notes what the handset of an open window is, as the user said
// before registering: its name and the id of its device profile ("": none).
// False: no such open window, or the name is empty, too long or has a control
// character.
func (g *Registry) SetHandset(window uuid.UUID, name, profile string) bool {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > MaxName || strings.ContainsFunc(name, unicode.IsControl) {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	w, ok := g.windows[window]
	if !ok || w.State != Open {
		return false
	}
	w.Name, w.Profile = name, profile
	return true
}

// Enrolled notes the stored handset of a completed window and tells Done.
func (g *Registry) Enrolled(window, handset uuid.UUID) {
	g.mu.Lock()
	w, ok := g.windows[window]
	var done Window
	if ok {
		w.Handset = handset
		done = *w
	}
	cb := g.Done
	g.mu.Unlock()
	if ok && cb != nil {
		cb(done)
	}
}

// Failed counts a rejected registration attempt on cell; FailLimit within
// FailWindow closes the cell's registration for Lockout.
func (g *Registry) Failed(cell uuid.UUID) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	recent := g.fails[cell][:0]
	for _, t := range g.fails[cell] {
		if now.Sub(t) < FailWindow {
			recent = append(recent, t)
		}
	}
	recent = append(recent, now)
	g.fails[cell] = recent
	g.log.Warn("registration attempt failed", "cell", cell, "in_last_minute", len(recent))
	if len(recent) >= FailLimit {
		delete(g.fails, cell)
		g.locked[cell] = now.Add(Lockout)
		g.log.Warn("registration on the cell locked out: too many failed attempts", "cell", cell, "for", Lockout)
		g.ver.Add(1)
		g.changed(now)
	}
}

// close ends an open window: its PIN leaves the lists now.
func (g *Registry) close(w *Window, st State, now time.Time) {
	w.State, w.ClosedAt = st, now
	for i := range g.slots {
		if g.slots[i].win == w && g.slots[i].out.IsZero() {
			g.slots[i].out = now
		}
	}
	g.changed(now)
}

// sweep moves what time has ended. Called with mu held.
func (g *Registry) sweep(now time.Time) {
	if g.next.IsZero() || now.Before(g.next) {
		return
	}
	for id, w := range g.windows {
		if w.State == Open && !now.Before(w.ExpiresAt) {
			g.close(w, Expired, w.ExpiresAt)
			g.log.Info("registration window expired", "window", id)
		}
		if w.State != Open && now.Sub(w.ClosedAt) > keepClosed && !g.referenced(w) {
			delete(g.windows, id)
		}
	}
	for c, t := range g.locked {
		if !now.Before(t) {
			delete(g.locked, c)
			g.ver.Add(1)
			g.log.Info("registration on the cell no longer locked out", "cell", c)
		}
	}
	g.changed(now)
}

// referenced: an index still names w (its quarantine has not passed).
func (g *Registry) referenced(w *Window) bool {
	for i := range g.slots {
		if g.slots[i].win == w {
			return true
		}
	}
	return false
}

// changed frees the indices past their quarantine, rebuilds the lists'
// snapshot and the next time anything ends, and bumps the version when the
// lists changed. Called with mu held.
func (g *Registry) changed(now time.Time) {
	n := 0
	var until, next time.Time
	soonest := func(t time.Time) {
		if next.IsZero() || t.Before(next) {
			next = t
		}
	}
	for i := range g.slots {
		e := &g.slots[i]
		switch {
		case e.win == nil:
		case e.win.State == Open:
			n = i + 1
			if e.win.ExpiresAt.After(until) {
				until = e.win.ExpiresAt
			}
		case now.Sub(e.out) >= Quarantine:
			*e = entry{} // the index is free, the PIN forgotten
		default:
			soonest(e.out.Add(Quarantine))
		}
	}
	pins := make([]string, n)
	for i := range n {
		if e := &g.slots[i]; e.win != nil && e.win.State == Open {
			pins[i] = e.pin
		}
	}
	if !until.IsZero() {
		soonest(until)
	}
	for _, t := range g.locked {
		soonest(t)
	}
	for _, w := range g.windows {
		if w.State != Open {
			soonest(w.ClosedAt.Add(keepClosed))
		}
	}
	g.next = next
	if !slices.Equal(pins, g.pins) || !until.Equal(g.until) {
		g.pins, g.until = pins, until
		g.ver.Add(1)
	}
}
