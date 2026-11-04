// SPDX-License-Identifier: AGPL-3.0-or-later

package handset

import (
	"slices"
	"strings"
	"testing"
)

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

func TestDecodeProfilesOfTheHandsetsWeHave(t *testing.T) {
	// measured on air: the A540 sends 02 00 00 00 00 40 16, the AS690H and the C430A end in 12
	p, f := DecodeProfiles("02000000004016")
	if len(p) != 1 || p[0] != "GAP (basic cordless telephony)" {
		t.Errorf("A540 profiles: %q", p)
	}
	want := []string{"no-emission mode (radio silent while idle)", "cipher re-keying and early encryption", "radio: GFSK only"}
	if len(f) != len(want) {
		t.Fatalf("A540 features: %q", f)
	}
	for _, w := range want {
		if !slices.Contains(f, w) {
			t.Errorf("A540 lacks %q: %q", w, f)
		}
	}
	// no Part 1 or Part 3 in the lists: those are the Standard row
	for _, s := range append(p, f...) {
		if strings.Contains(s, "NG-DECT Part 1") || strings.Contains(s, "NG-DECT Part 3") {
			t.Errorf("a standard in the lists: %q", s)
		}
	}
	_, f = DecodeProfiles("02000000004012")
	if !slices.Contains(f, "cipher re-keying and early encryption") || slices.Contains(f, "headset management") {
		t.Errorf("AS690H features: %q", f)
	}
	// every bit of a made-up handset has its own words; reserved bits and bad input say nothing
	p, f = DecodeProfiles("7f7f7f7f1f7f7f")
	if len(p) < 20 || len(f) < 10 {
		t.Errorf("a full set decodes to %d profiles and %d features", len(p), len(f))
	}
	for _, in := range []string{"", "zz", "01"} {
		if p, f := DecodeProfiles(in); in != "01" && (p != nil || f != nil) {
			t.Errorf("%q decodes to %q %q", in, p, f)
		}
	}
	if _, f := DecodeProfiles("00000000000000" + "08"); !slices.Contains(f, "packet data category 1") {
		t.Errorf("octet 8 = 0x08: %q", f)
	}
	if _, f := DecodeProfiles("00000000000000" + "20"); !slices.Contains(f, "packet data category 4 (up to 8PSK)") {
		t.Errorf("octet 8 = 0x20: %q", f)
	}
	if p, _ := DecodeProfiles("01"); len(p) != 1 || p[0] != "CAP (cordless access profile)" {
		t.Errorf("octet 1 alone: %q", p)
	}
}

func TestStandardDropsTheWidebandNote(t *testing.T) {
	for in, want := range map[string]string{"NG-DECT Part 1 (wideband)": "NG-DECT Part 1", "NG-DECT Part 1": "NG-DECT Part 1", "NG-DECT Part 3 (CAT-iq)": "NG-DECT Part 3 (CAT-iq)", "GAP (no NG-DECT)": "GAP (no NG-DECT)"} {
		if got := (Handset{Capabilities: map[string]any{"standard": in}}).Standard(); got != want {
			t.Errorf("Standard(%q) = %q, want %q", in, got, want)
		}
	}
}
