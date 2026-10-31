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
	x.AddFreeStore(0)
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
