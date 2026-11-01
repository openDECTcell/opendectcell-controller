// SPDX-License-Identifier: AGPL-3.0-or-later

package handset

import "testing"

// The vectors are libopendectcell's own (tests/test_mm.c): EN 300 175-6 annex C's
// example, a handset, and an identity that is not an IPEI.
func TestIPEI(t *testing.T) {
	for _, c := range []struct {
		ipui []byte
		want string
		emc  int
		ok   bool
	}{
		{[]byte{0x80, 0xa8, 0x00, 0x00, 0xc0, 0x00, 0x89}, "00012 0000137 9", 12, true},
		{[]byte{0x80, 0xa8, 0x02, 0x12, 0x34, 0x56, 0x78}, "08483 0284280 0", 8483, true},
		{[]byte{0x80, 0xa8, 0x10, 0x00, 0xc0, 0x00, 0x89}, "", 0, false}, // PUT O
		{[]byte{0x80, 0xa8, 0x00, 0x00, 0xc0, 0x00}, "", 0, false},       // too short
	} {
		got, emc, ok := Handset{IPUI: c.ipui}.IPEI()
		if got != c.want || emc != c.emc || ok != c.ok {
			t.Errorf("IPEI(%x) = %q %d %v, want %q %d %v", c.ipui, got, emc, ok, c.want, c.emc, c.ok)
		}
	}
}
