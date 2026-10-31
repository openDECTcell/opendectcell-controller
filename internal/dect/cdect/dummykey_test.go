// SPDX-License-Identifier: AGPL-3.0-or-later

package cdect

import "testing"

// A cell with no records still runs on one key: when its radio comes up the
// chip's table is cleared except slot 1, which gets a key nobody holds, and
// the locked-in mask names it (L8: no key of an earlier run stays).
func TestEmptyCellWritesDummyKey(t *testing.T) {
	e := &traceEnv{t: t}
	x := NewHost(e, Config{PIN: "12345678"})
	defer x.Close()
	e.host = x
	x.AddFreeStore(0, [3]byte{})
	if err := x.Start(); err != nil {
		t.Fatal(err)
	}
	x.RadioUp(true)
	for now := uint64(1000); now < 2000; now += 10 {
		x.Tick(now)
	}
	var clears, writes, mask int
	for _, m := range e.sent {
		switch {
		case m.Prim == 0x0247 && len(m.Param) == 3 && m.Param[1] == 0:
			clears++
			if m.Param[0] == 1 {
				t.Error("slot 1 cleared, the dummy key's slot")
			}
		case m.Prim == 0x020A:
			writes++
			if m.Param[0] != 1 {
				t.Errorf("key written to slot %d, want 1", m.Param[0])
			}
		case m.Prim == 0x017C && len(m.Param) == 2:
			mask++
			if m.Param[1] != 0x01 {
				t.Errorf("mask %02x, want 01", m.Param[1])
			}
		}
	}
	if clears != 5 || writes != 1 || mask != 1 {
		t.Errorf("clears %d writes %d masks %d, want 5 1 1", clears, writes, mask)
	}
}

// A record numbered above the chip's six slots is the host's alone: with only
// such records a cell still runs on the dummy key, and nothing is written to a
// slot that does not exist, whether the record is current (index 0) or not.
func TestHostOnlyRecordsLeaveTheChipAlone(t *testing.T) {
	for _, hostFirst := range []bool{false, true} {
		e := &traceEnv{t: t}
		x := NewHost(e, Config{PIN: "12345678"})
		e.host = x
		rec := Record{Slot: 9, PortID: []byte{0x80, 0x01, 0x02, 0x03, 0x04}, TPUI: [3]byte{0, 0x11, 9}}
		rec.UAK[0] = 0x55
		if hostFirst {
			x.AddRecord(0, rec)
			x.AddFreeStore(1, [3]byte{0, 0x11, 10})
		} else {
			x.AddFreeStore(0, [3]byte{0, 0x11, 10})
			x.AddRecord(1, rec)
		}
		if err := x.Start(); err != nil {
			t.Fatal(err)
		}
		x.RadioUp(true)
		for now := uint64(1000); now < 2000; now += 10 {
			x.Tick(now)
		}
		var clears, writes int
		for _, m := range e.sent {
			switch {
			case m.Prim == 0x0247 && len(m.Param) == 3 && m.Param[1] == 0:
				clears++
			case m.Prim == 0x020A:
				writes++
				if m.Param[0] != 1 {
					t.Errorf("host first %v: key written to slot %d, want only the dummy's 1", hostFirst, m.Param[0])
				}
			case m.Prim == 0x0247 && len(m.Param) > 0 && m.Param[0] > 6:
				t.Errorf("host first %v: a write to slot %d", hostFirst, m.Param[0])
			}
		}
		if clears != 5 || writes != 1 {
			t.Errorf("host first %v: clears %d writes %d, want 5 1", hostFirst, clears, writes)
		}
		x.Close()
	}
}

// A record added to a stack that runs is held like the others: the handset
// shows in the stack's list and nothing goes to the chip.
func TestRecordAddedWhileRunning(t *testing.T) {
	e := &traceEnv{t: t}
	x := NewHost(e, Config{PIN: "12345678"})
	defer x.Close()
	e.host = x
	x.AddFreeStore(0, [3]byte{})
	if err := x.Start(); err != nil {
		t.Fatal(err)
	}
	x.RadioUp(true)
	for now := uint64(1000); now < 1500; now += 10 {
		x.Tick(now)
	}
	n := len(e.sent)
	rec := Record{Slot: 7, PortID: []byte{0x80, 0x01, 0x02, 0x03, 0x04}, TPUI: [3]byte{0, 0x11, 7}}
	x.AddRecord(1, rec)
	for now := uint64(1500); now < 2500; now += 10 {
		x.Tick(now)
	}
	for _, m := range e.sent[n:] {
		if m.Prim == 0x0247 || m.Prim == 0x020A {
			t.Errorf("a host-only record reached the chip: %04x %x", m.Prim, m.Param)
		}
	}
}
