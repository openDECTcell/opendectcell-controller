// SPDX-License-Identifier: AGPL-3.0-or-later

package cdect

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openDECTcell/opendectcell-controller/internal/dect/profiles"
)

// fetchEnv is a base with defer_unknown and nothing but a free store, and a link
// up on handle 1.
func fetchEnv(t *testing.T) *traceEnv { return fetchEnvWith(t, func(*Config) {}) }

func fetchEnvWith(t *testing.T, tune func(*Config)) *traceEnv {
	t.Helper()
	e := &traceEnv{t: t, now: 1000, wall: 1793350800, pps: map[uint8]*PP{}}
	t.Cleanup(func() {
		for _, p := range e.pps {
			p.Close()
		}
	})
	cfg := Config{PIN: "12345678", UAKTable: 0x9518, CheckDelayMs: 50, Stub: "dectfw_secondlevel_441.hex",
		NoDummyKey: true, NoLocAuth: true, DeferUnknown: true}
	tune(&cfg)
	e.host = NewHost(e, cfg)
	t.Cleanup(e.host.Close)
	e.host.AddFreeStore(0, [3]byte{})
	if err := e.host.Start(); err != nil {
		t.Fatal(err)
	}
	e.chip(0xF601, []byte{0x00, 0x01, 0x23, 0x45, 0x60})
	e.host.RadioUp(true)
	e.host.Hello()
	e.settle()
	e.out = nil
	e.pps[1] = NewPP()
	e.chip(0x0200, []byte{1})
	e.settle()
	return e
}

// a {LOCATE-REQUEST} with <<PORTABLE-IDENTITY>> 80 a8 03 11 22 33 44 and the
// fixed identity (the golden trace locate.trace)
var locateRequest = []byte{0x05, 0x54, 0x05, 0x07, 0x80, 0xa8, 0x03, 0x11, 0x22, 0x33, 0x44,
	0x06, 0x07, 0xa0, 0xa5, 0x00, 0x01, 0x23, 0x45, 0x60}

func send(t *testing.T, e *traceEnv, nwk []byte) {
	t.Helper()
	fr, err := e.pp(1).Send(nwk)
	if err != nil {
		t.Fatal(err)
	}
	e.lc(1, fr)
	e.settle()
}

func answered(e *traceEnv) []string {
	var fp []string
	for _, l := range e.out {
		if strings.HasPrefix(l, "fp") {
			fp = append(fp, l)
		}
	}
	return fp
}

// A locate from a handset the cell holds no record of is held; the host is asked once
// about its identity; with the record added and Resume, the handset is accepted under
// the record's own TPUI (a pool one, not the slot's).
func TestUnknownLocateIsFetched(t *testing.T) {
	e := fetchEnv(t)
	send(t, e, locateRequest)
	if len(e.fetched) != 1 || !bytes.Equal(e.fetched[0], locateRequest[4:11]) {
		t.Fatalf("the host was asked about %x, want the locating handset's identity once", e.fetched)
	}
	if fp := answered(e); len(fp) != 0 {
		t.Fatalf("the base answered before it had the record: %v", fp)
	}
	// time passes, nothing is asked twice
	for range 20 {
		e.now += 10
		e.settle()
	}
	if len(e.fetched) != 1 {
		t.Errorf("asked %d times", len(e.fetched))
	}
	rec := subRecord(7, locateRequest[4:11])
	rec.TPUI = [3]byte{0x00, 0x11, 0x2a}
	e.host.AddRecord(1, rec)
	e.out = nil
	e.host.Resume(e.now)
	e.settle()
	fp := answered(e)
	if len(fp) == 0 {
		t.Fatal("no LOCATE-ACCEPT after Resume")
	}
	if !strings.Contains(strings.Join(fp, "|"), "00 11 2a") {
		t.Errorf("the accept does not carry the record's own TPUI 00 11 2a: %v", fp)
	}
}

// With no record the held locate is rejected ("IPUI unknown"), so the handset
// does not wait for its patience to run out.
func TestUnknownLocateOfAStrangerIsRejected(t *testing.T) {
	e := fetchEnv(t)
	send(t, e, locateRequest)
	if len(e.fetched) != 1 {
		t.Fatalf("asked %d times", len(e.fetched))
	}
	e.out = nil
	e.host.Resume(e.now)
	e.settle()
	fp := answered(e)
	if len(fp) == 0 {
		t.Fatal("no answer after Resume")
	}
	if strings.Contains(strings.Join(fp, "|"), "00 11") {
		t.Errorf("a stranger was accepted: %v", fp)
	}
}

// A record can leave the table when nothing is going; while a link is up it stays.
func TestDropRecordNeedsAQuietBase(t *testing.T) {
	e := fetchEnv(t)
	rec := subRecord(7, []byte{0x80, 0xa8, 0x03, 0x55, 0x66, 0x77, 0x88})
	e.host.AddRecord(1, rec)
	if e.host.Quiet() {
		t.Error("quiet with a link up")
	}
	if e.host.Drop(1) {
		t.Error("dropped with a link up")
	}
	e.chip(0x0201, []byte{1})
	e.settle()
	if !e.host.Quiet() {
		t.Error("not quiet after the link ended")
	}
	if !e.host.Drop(1) {
		t.Error("not dropped on a quiet base")
	}
	if e.host.Drop(1) {
		t.Error("dropped twice")
	}
}

// A record takes a name for the lists and a device profile; the library
// refuses a name it cannot list and a profile file it cannot read.
func TestSetNameAndProfile(t *testing.T) {
	e := fetchEnv(t)
	if !e.host.SetName(0, "Kitchen") {
		t.Error("a plain name was refused")
	}
	if e.host.SetName(0, "a name of sixteen!") || e.host.SetName(5, "Kitchen") {
		t.Error("a name that is too long, or a record that does not exist, was taken")
	}
	mfr, dev, _ := profiles.Default.Texts("gigaset-a540")
	if !e.host.SetProfile(0, mfr, dev) {
		t.Error("the A540's profile was refused")
	}
	if !e.host.SetProfile(0, "", "") {
		t.Error("no profile is the standard behaviour")
	}
	if e.host.SetProfile(0, "this is not a profile file", "") {
		t.Error("garbage was taken for a profile")
	}
	if e.host.SetProfile(5, mfr, dev) {
		t.Error("a record that does not exist took a profile")
	}
}

// Freeing a handset's chip slot must not leave the chip with an empty table:
// it goes silent then (07.56, and on the 7412 the maintainer's A540 got no
// answer). The placeholder key is written again, as at radio-up.
func TestRecycleLeavesAKeyInTheChip(t *testing.T) {
	e := &traceEnv{t: t, now: 1000, wall: 1793350800, pps: map[uint8]*PP{}}
	cfg := Config{PIN: "12345678", UAKTable: 0x9518, CheckDelayMs: 50, Stub: "dectfw_secondlevel_441.hex",
		NoLocAuth: true, DeferUnknown: true} // the dummy key is on
	e.host = NewHost(e, cfg)
	t.Cleanup(e.host.Close)
	e.host.AddRecord(0, subRecord(1, []byte{0x80, 0xa8, 0x03, 0x11, 0x22, 0x33, 0x44}))
	e.host.AddFreeStore(1, [3]byte{0, 0x11, 0x08})
	if err := e.host.Start(); err != nil {
		t.Fatal(err)
	}
	e.chip(0xF601, []byte{0x00, 0x01, 0x23, 0x45, 0x60})
	e.host.RadioUp(true)
	e.host.Hello()
	e.settle()
	e.sent = nil
	if got := e.host.Recycle(0, [3]byte{0, 0x11, 0x2A}); got != RecycleDone {
		t.Fatalf("Recycle: %v", got)
	}
	var zeros, placeholder bool
	for _, m := range e.sent {
		if m.Prim != 0x020A || len(m.Param) < 2 || m.Param[0] != 1 {
			continue
		}
		if allZero(m.Param[1:]) {
			zeros = true
		} else if zeros {
			placeholder = true
		}
	}
	if !zeros {
		t.Error("the slot was not emptied")
	}
	if !placeholder {
		t.Error("the chip's table was left empty after the recycle")
	}
}

// The stack's RFPI is what a registering handset gets as its base's PARK: it must
// be settable by the host (the chip's contact answer does not reach the
// controller in cell mode), and the first one stays, as with the chip's own.
func TestSetRFPI(t *testing.T) {
	e := fetchEnv(t) // its chip answered 0xF601 already
	if !e.host.HasRFPI() {
		t.Fatal("the chip's answer was not taken")
	}
	e2 := &traceEnv{t: t, now: 1000, wall: 1793350800, pps: map[uint8]*PP{}}
	h := NewHost(e2, Config{PIN: "12345678", UAKTable: 0x9518, Stub: "dectfw_secondlevel_441.hex"})
	t.Cleanup(h.Close)
	if h.HasRFPI() {
		t.Fatal("a stack that was told nothing knows an RFPI")
	}
	h.SetRFPI([5]byte{0x03, 0x69, 0x0e, 0x24, 0xb8})
	h.SetRFPI([5]byte{1, 2, 3, 4, 5})
	if !h.HasRFPI() {
		t.Error("not known after SetRFPI")
	}
}

// While a handset is connected the chip is asked once a second for the link's row (0xF624),
// and the level of the carrier the link is on, in dBm as FRITZ!OS shows it, is kept for the
// handset (clean room A-0121).
func TestSignalFromTheChipsRow(t *testing.T) {
	e := fetchEnvWith(t, func(c *Config) { c.PollSignal = true }) // a link is up on handle 1, the current record is 0
	if _, _, ok := e.host.Signal(0); ok {
		t.Fatal("a signal before any was measured")
	}
	e.sent = nil
	e.now += 1500
	e.host.Tick(e.now)
	var polled bool
	for _, m := range e.sent {
		polled = polled || (m.Prim == 0xF624 && len(m.Param) == 2 && m.Param[0] == 1 && m.Param[1] == 0x0A)
	}
	if !polled {
		t.Fatalf("the chip was not asked: %v", e.sent)
	}
	row := make([]byte, 16)
	row[0], row[2], row[3] = 1, 2, 3 // link 1, slot 2, carrier 3
	for k := 0; k < 10; k++ {
		row[6+k] = 0x20
	}
	row[6+3] = 0x2c // the level on carrier 3
	e.chip(0xF625, row)
	dbm, at, ok := e.host.Signal(0)
	if !ok || dbm != -59 || at == 0 {
		t.Errorf("signal %d dBm at %d, ok %v: want -59", dbm, at, ok)
	}
	// a row for another link says nothing about this handset
	row[0] = 5
	row[6+3] = 0x30
	e.chip(0xF625, row)
	if dbm, _, _ := e.host.Signal(0); dbm != -59 {
		t.Errorf("another link's row was taken: %d", dbm)
	}
}

// A record that was recycled out of its chip slot (the pool's way) is a free store
// like the others: a handset the base knows nothing of that locates next is asked
// about, with the cell's own settings (the dummy key on, locate authentication on).
func TestLocateAfterRecycle(t *testing.T) {
	for _, noAuth := range []bool{true, false} {
		e := &traceEnv{t: t, now: 1000, wall: 1793350800, pps: map[uint8]*PP{}}
		cfg := Config{PIN: "12345678", UAKTable: 0x9518, CheckDelayMs: 50, Stub: "dectfw_secondlevel_441.hex",
			NoLocAuth: noAuth, DeferUnknown: true}
		e.host = NewHost(e, cfg)
		e.host.AddRecord(0, subRecord(1, []byte{0x80, 0xa8, 0x03, 0x11, 0x22, 0x33, 0x44}))
		e.host.AddFreeStore(1, [3]byte{0, 0x11, 0x08})
		if err := e.host.Start(); err != nil {
			t.Fatal(err)
		}
		e.chip(0xF601, []byte{0x00, 0x01, 0x23, 0x45, 0x60})
		e.host.RadioUp(true)
		e.host.Hello()
		e.settle()
		if got := e.host.Recycle(0, [3]byte{0, 0x11, 0x2A}); got != RecycleDone {
			t.Fatalf("Recycle: %v", got)
		}
		e.out, e.sent, e.fetched = nil, nil, nil
		e.pps[1] = NewPP()
		e.chip(0x0200, []byte{1})
		e.settle()
		send(t, e, locateRequest)
		for range 10 {
			e.now += 10
			e.settle()
		}
		if len(e.fetched) != 1 {
			t.Errorf("NoLocAuth %v: asked %d times about the locating handset, want once; answered %v", noAuth, len(e.fetched), answered(e))
		}
		e.host.Close()
		for _, p := range e.pps {
			p.Close()
		}
	}
}

// The same after a real enrolment (the golden registration trace), with the cell's
// way of holding unknown handsets: the handset that enrolled into a chip slot and was
// recycled out of it is asked about when it locates.
func TestLocateAfterEnrolmentAndRecycle(t *testing.T) {
	replayTune = func(c *Config) { c.DeferUnknown = true }
	replayHook = func(e *traceEnv) {
		if len(e.saves) == 0 {
			t.Fatal("no record saved")
		}
		rec := e.saves[0]
		var done bool
		for i := 0; i < 8 && !done; i++ {
			done = e.host.Recycle(i, [3]byte{0, 0x11, 0x2A}) == RecycleDone
		}
		if !done {
			t.Fatal("no record could be recycled")
		}
		for h := uint8(0); h < 8; h++ {
			e.chip(0x0201, []byte{h})
		}
		e.settle()
		e.out, e.sent, e.fetched = nil, nil, nil
		e.pps[1] = NewPP()
		e.chip(0x0200, []byte{1})
		e.settle()
		nwk := append([]byte{0x05, 0x54, 0x05, 0x07}, rec.PortID...)
		nwk = append(nwk, locateRequest[11:]...)
		send(t, e, nwk)
		for range 10 {
			e.now += 10
			e.settle()
		}
		t.Logf("portid %x fetched %x answered %v", rec.PortID, e.fetched, answered(e))
		if len(e.fetched) != 1 {
			t.Errorf("asked %d times about the locating handset, want once", len(e.fetched))
		}
	}
	defer func() { replayHook, replayTune = nil, nil }()
	replay(t, filepath.Join(traceDir, "register.trace"), nil)
}
