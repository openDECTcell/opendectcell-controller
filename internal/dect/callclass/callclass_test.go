// SPDX-License-Identifier: AGPL-3.0-or-later

package callclass

import "testing"

func TestBridge(t *testing.T) {
	for _, c := range []struct {
		v      uint8
		bridge bool
		wide   bool
		name   string
	}{
		{0x88, true, true, "normal call"},  // FRITZ!Fon C4 / A540 voice call
		{0x80, true, false, "normal call"}, // narrowband
		{0x28, false, true, "list access"}, // A540 phonebook read: must NOT bridge
		{0x38, false, true, "ULE service"}, // the silent class-3 SETUP of v33
		{0x98, false, true, "internal call"},
		{0xA0, true, false, "emergency call"},
		{0x09, false, false, "class 0x0"},
	} {
		cl, sv := Parse(c.v)
		if Bridge(cl) != c.bridge || Wideband(sv) != c.wide || cl.String() != c.name {
			t.Errorf("0x%02X: class %s bridge %v wide %v", c.v, cl, Bridge(cl), Wideband(sv))
		}
	}
}
