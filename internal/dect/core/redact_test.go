// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"strings"
	"testing"
)

// The debug log keeps no numbers, names or dialled digits from a bridge line (threat model B7, review F-15).
func TestRedactBridgeLine(t *testing.T) {
	in := `call-in-ok call=7 sip="sip:0171234567@pbx.example.org" name="Eva Meyer" number=0301234567 d=5 codec=g722`
	out := redactBridgeLine(in)
	for _, leak := range []string{"0171234", "Eva", "Meyer", "0301234", "d=5"} {
		if strings.Contains(out, leak) {
			t.Errorf("redacted line still holds %q: %s", leak, out)
		}
	}
	for _, keep := range []string{"call-in-ok", "call=7", "codec=g722", "number=*******567", `name="<9 chars>"`, "d=*", "@pbx.example.org"} {
		if !strings.Contains(out, keep) {
			t.Errorf("redacted line lost %q: %s", keep, out)
		}
	}
	// the words come back as parseBridgeLine reads them
	v, kv := parseBridgeLine(out)
	if v != "call-in-ok" || kv["call"] != "7" || kv["name"] != "<9 chars>" {
		t.Errorf("redacted line does not parse back: %q %v", v, kv)
	}
}

// A mail's bytes are replaced by their length and a digest: equal mails give equal digests.
func TestMailDigest(t *testing.T) {
	a, b := mailDigest([]byte{1, 2, 3}), mailDigest([]byte{1, 2, 3})
	if a != b || !strings.HasPrefix(a, "3 bytes, sha256 ") || strings.Contains(a, "010203") {
		t.Errorf("digest %q / %q", a, b)
	}
}
