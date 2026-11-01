// SPDX-License-Identifier: AGPL-3.0-or-later

package regwin

import (
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }
func newReg() (*Registry, *clock) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	return New(nil, c.now, nil), c
}
func live(g *Registry, cell uuid.UUID) int { _, p, _ := g.PINs(cell); return count(p) }

func count(p []string) int {
	n := 0
	for _, s := range p {
		if s != "" {
			n++
		}
	}
	return n
}

func TestOpenClaim(t *testing.T) {
	g, c := newReg()
	anna, ben, cell := uuid.New(), uuid.New(), uuid.New()
	w1, pin1, err := g.Open(anna, anna, 0)
	if err != nil || !regexp.MustCompile(`^[0-9]{8}$`).MatchString(pin1) {
		t.Fatalf("open: %v %q", err, pin1)
	}
	c.add(20 * time.Second)
	_, pin2, err := g.Open(ben, ben, 6)
	if err != nil || len(pin2) != 6 {
		t.Fatalf("open 6: %v %q", err, pin2)
	}
	if _, _, err := g.Open(anna, anna, 8); !errors.Is(err, ErrWindowOpen) {
		t.Errorf("second window of a user: %v", err)
	}
	ver, pins, until := g.PINs(cell)
	if len(pins) != 2 || pins[0] != pin1 || pins[1] != pin2 {
		t.Fatalf("pins %q", pins)
	}
	// one global timer: the cells close 60 s after the latest opening
	if !until.Equal(c.t.Add(Lifetime)) {
		t.Errorf("until %v, want %v", until, c.t.Add(Lifetime))
	}

	owner, win, ok := g.Claim(cell, 2)
	if !ok || owner != ben {
		t.Fatalf("claim index 1: %v %v", owner, ok)
	}
	if _, _, ok := g.Claim(cell, 2); ok {
		t.Error("a PIN binds twice")
	}
	g.Enrolled(win, uuid.New())
	if w, _ := g.Get(win); w.State != Completed || w.Cell != cell || w.Handset == uuid.Nil {
		t.Errorf("window after claim %+v", w)
	}
	v2, pins, until := g.PINs(cell)
	if v2 == ver || len(pins) != 1 || pins[0] != pin1 || !until.Equal(w1.ExpiresAt) {
		t.Errorf("after claim: ver %d->%d pins %q until %v", ver, v2, pins, until)
	}
	// each PIN keeps its own 60 s: Anna's is not extended by Ben's
	c.add(41 * time.Second)
	if live(g, cell) != 0 {
		t.Error("anna's PIN outlived its 60 s")
	}
	if w, _ := g.Get(w1.ID); w.State != Expired {
		t.Errorf("state %s", w.State)
	}
}

func TestGraceAndQuarantine(t *testing.T) {
	g, c := newReg()
	anna, ben, cell := uuid.New(), uuid.New(), uuid.New()
	w, _, _ := g.Open(anna, anna, 8)
	c.add(Lifetime + Grace - time.Second)
	// a link set up while the PIN was live finishes late
	if owner, _, ok := g.Claim(cell, 1); !ok || owner != anna {
		t.Errorf("claim within the grace: %v", ok)
	}
	if got, _ := g.Get(w.ID); got.State != Completed {
		t.Errorf("state %s", got.State)
	}
	// index 0 is quarantined: Ben gets index 1
	if _, _, err := g.Open(ben, ben, 8); err != nil {
		t.Fatal(err)
	}
	if _, pins, _ := g.PINs(cell); len(pins) != 2 || pins[0] != "" {
		t.Errorf("pins %q: index 0 reused inside its quarantine", pins)
	}
	// past the grace a stale index binds nobody
	g2, c2 := newReg()
	g2.Open(anna, anna, 8)
	c2.add(Lifetime + Grace)
	if _, _, ok := g2.Claim(cell, 1); ok {
		t.Error("an expired PIN bound after the grace")
	}
	if _, _, ok := g2.Claim(cell, 0); ok {
		t.Error("index 0 (a stored record) bound")
	}
}

func TestCancelled(t *testing.T) {
	g, _ := newReg()
	anna, cell := uuid.New(), uuid.New()
	w, _, _ := g.Open(anna, anna, 8)
	g.Cancel(w.ID)
	if _, _, ok := g.Claim(cell, 1); ok {
		t.Error("a cancelled PIN bound")
	}
	if live(g, cell) != 0 {
		t.Error("cancelled PIN still listed")
	}
	if _, _, err := g.Open(anna, anna, 8); err != nil {
		t.Errorf("reopen after cancel: %v", err)
	}
}

func TestLimits(t *testing.T) {
	g, c := newReg()
	anna := uuid.New()
	for i := range PerHour {
		w, _, err := g.Open(anna, anna, 8)
		if err != nil {
			t.Fatalf("window %d: %v", i, err)
		}
		g.Cancel(w.ID)
	}
	if _, _, err := g.Open(anna, anna, 8); !errors.Is(err, ErrRateLimited) {
		t.Errorf("window %d: %v", PerHour+1, err)
	}
	c.add(time.Hour)
	if _, _, err := g.Open(anna, anna, 8); err != nil {
		t.Errorf("an hour later: %v", err)
	}
	if _, _, err := g.Open(uuid.New(), uuid.New(), 4); !errors.Is(err, ErrDigits) {
		t.Errorf("4 digits: %v", err)
	}

	g, c = newReg()
	for i := range MaxPINs {
		if _, _, err := g.Open(uuid.New(), uuid.New(), 8); err != nil {
			t.Fatalf("PIN %d: %v", i, err)
		}
	}
	if _, _, err := g.Open(uuid.New(), uuid.New(), 8); !errors.Is(err, ErrBusy) {
		t.Errorf("PIN 33: %v", err)
	}
	c.add(Lifetime + Quarantine)
	if _, _, err := g.Open(uuid.New(), uuid.New(), 8); err != nil {
		t.Errorf("after the quarantine: %v", err)
	}
}

func TestUniquePINs(t *testing.T) {
	g, _ := newReg()
	seen := map[string]bool{}
	for range MaxPINs {
		_, pin, err := g.Open(uuid.New(), uuid.New(), 6)
		if err != nil || seen[pin] {
			t.Fatalf("%q %v", pin, err)
		}
		seen[pin] = true
	}
}

func TestLockout(t *testing.T) {
	g, c := newReg()
	a, b := uuid.New(), uuid.New()
	g.Open(uuid.New(), uuid.New(), 8)
	for range FailLimit - 1 {
		g.Failed(a)
	}
	if live(g, a) != 1 {
		t.Fatal("locked before the limit")
	}
	ver, _, _ := g.PINs(a)
	g.Failed(a)
	v2, pins, _ := g.PINs(a)
	if pins != nil || v2 == ver {
		t.Errorf("cell a not locked: %q, ver %d->%d", pins, ver, v2)
	}
	if live(g, b) != 1 {
		t.Error("the lockout reached another cell")
	}
	c.add(Lockout)
	g.Open(uuid.New(), uuid.New(), 8)
	if live(g, a) == 0 {
		t.Error("still locked after the lockout")
	}
	// spread out failures do not lock
	for range FailLimit {
		g.Failed(b)
		c.add(FailWindow / FailLimit * 2)
	}
	if _, p, _ := g.PINs(b); p == nil && live(g, b) == 0 {
		t.Error("spread failures locked the cell")
	}
}

func TestModelReachesDone(t *testing.T) {
	g, _ := newReg()
	var got Window
	g.Done = func(w Window) { got = w }
	anna, cell, hs := uuid.New(), uuid.New(), uuid.New()
	w, _, err := g.Open(anna, anna, 0)
	if err != nil {
		t.Fatal(err)
	}
	if g.SetModel(w.ID, "bad\x07text") || g.SetModel(w.ID, string(make([]rune, MaxModel+1))) {
		t.Error("a model with a control character or too long was taken")
	}
	if !g.SetModel(w.ID, "  Gigaset A540 ") {
		t.Fatal("a model was refused")
	}
	if _, _, ok := g.Claim(cell, 1); !ok {
		t.Fatal("claim")
	}
	g.Enrolled(w.ID, hs)
	if got.Handset != hs || got.Model != "Gigaset A540" {
		t.Errorf("Done got %+v", got)
	}
	if g.SetModel(w.ID, "later") {
		t.Error("a model was taken for a window that is no longer open")
	}
}
