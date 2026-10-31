// SPDX-License-Identifier: AGPL-3.0-or-later

package cdect

import (
	"bytes"
	"strings"
	"testing"
)

// fetchEnv is a base with defer_unknown and nothing but a free store, and a link
// up on handle 1.
func fetchEnv(t *testing.T) *traceEnv {
	t.Helper()
	e := &traceEnv{t: t, now: 1000, wall: 1793350800, pps: map[uint8]*PP{}}
	t.Cleanup(func() {
		for _, p := range e.pps {
			p.Close()
		}
	})
	cfg := Config{PIN: "12345678", UAKTable: 0x9518, CheckDelayMs: 50, Stub: "dectfw_secondlevel_441.hex",
		NoDummyKey: true, NoLocAuth: true, DeferUnknown: true}
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
	if e.host.Drop(1) {
		t.Error("dropped with a link up")
	}
	e.chip(0x0201, []byte{1})
	e.settle()
	if !e.host.Drop(1) {
		t.Error("not dropped on a quiet base")
	}
	if e.host.Drop(1) {
		t.Error("dropped twice")
	}
}
