// SPDX-License-Identifier: AGPL-3.0-or-later

package handset

import "testing"

func TestWidebandClaim(t *testing.T) {
	for _, c := range []struct {
		name string
		caps map[string]any
		want bool
	}{
		{"nothing reported", nil, false},
		{"flag set", map[string]any{"wideband": true, "standard": "NG-DECT Part 1 (wideband)"}, true},
		{"flag clear", map[string]any{"wideband": false, "standard": "GAP (no NG-DECT)"}, false},
		{"old record, part 1", map[string]any{"standard": "NG-DECT Part 1 (wideband)"}, true},
		{"old record, part 3", map[string]any{"standard": "NG-DECT Part 3 (CAT-iq)"}, true},
		{"old record, GAP", map[string]any{"standard": "GAP (no NG-DECT)"}, false},
	} {
		if got := (Handset{Capabilities: c.caps}).WidebandClaim(); got != c.want {
			t.Errorf("%s: WidebandClaim = %v, want %v", c.name, got, c.want)
		}
	}
}
