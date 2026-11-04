// SPDX-License-Identifier: AGPL-3.0-or-later

package handset

import (
	"errors"
	"testing"
)

// IPUIFromIPEI is the inverse of Handset.IPEI, and refuses what is not an IPEI.
func TestIPUIFromIPEIRoundTrips(t *testing.T) {
	for _, digits := range []string{"001300012345", "123450067890", "655351048575", "000000000000"} {
		ipui := mustIPUI(t, digits)
		text, _, ok := Handset{IPUI: ipui}.IPEI() // the label text, with its check character
		if !ok {
			t.Fatalf("%s: not an IPEI again", digits)
		}
		for _, in := range []string{text, digits} { // with and without the check character
			back, err := IPUIFromIPEI(in)
			if err != nil || string(back) != string(ipui) {
				t.Errorf("%q -> %x, want %x (%v)", in, back, ipui, err)
			}
		}
		// a wrong check character is refused
		wrong := text[:len(text)-1] + "0"
		if text[len(text)-1] == '0' {
			wrong = text[:len(text)-1] + "1"
		}
		if _, err := IPUIFromIPEI(wrong); !errors.Is(err, ErrBadIPEI) {
			t.Errorf("a wrong check character was accepted: %q", wrong)
		}
	}
	for _, bad := range []string{"", "1234", "00130 0012345 7 8", "ABCDE 0012345 6", "70000 0000001 0", "00130 2000000 0"} {
		if _, err := IPUIFromIPEI(bad); !errors.Is(err, ErrBadIPEI) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func mustIPUI(t *testing.T, digits string) []byte {
	t.Helper()
	b, err := IPUIFromIPEI(digits)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
