// SPDX-License-Identifier: AGPL-3.0-or-later

package logctl

import "testing"

func TestMaskNumber(t *testing.T) {
	for in, want := range map[string]string{
		"+4930123456":                 "********456",
		"123":                         "123",
		"":                            "",
		"sip:+4930123456@example.org": "********456@example.org",
		"alice@example.org":           "**ice@example.org",
	} {
		if got := MaskNumber(in); got != want {
			t.Errorf("MaskNumber(%q) = %q, want %q", in, got, want)
		}
	}
}
