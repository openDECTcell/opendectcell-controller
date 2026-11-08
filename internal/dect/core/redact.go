// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/openDECTcell/opendectcell-controller/internal/logctl"
)

// What the debug log may say about a chip mail or a bridge line (threat model B7): no handset identities, numbers or
// names. A mail's bytes become their length and a short digest, so two lines about the same handset still match up.

// mailDigest stands in for a mail parameter in a log line.
func mailDigest(p []byte) string {
	sum := sha256.Sum256(p)
	return fmt.Sprintf("%d bytes, sha256 %x", len(p), sum[:4])
}

// splitBridgeLine cuts a bridge line into its words; a quoted part is one word, without its quotes.
func splitBridgeLine(line string) []string {
	var toks []string
	var cur strings.Builder
	in, started := false, false
	for _, r := range line {
		switch {
		case r == '"':
			in = !in
			started = true
		case r == ' ' && !in:
			if started {
				toks = append(toks, cur.String())
				cur.Reset()
				started = false
			}
		default:
			cur.WriteRune(r)
			started = true
		}
	}
	if started {
		toks = append(toks, cur.String())
	}
	return toks
}

// redactBridgeLine masks what in a bridge line names a caller or what was dialled: a number or SIP address keeps
// its last three digits (logctl.MaskNumber), a name becomes its length, a keypad digit a star.
func redactBridgeLine(line string) string {
	toks := splitBridgeLine(line)
	for i, t := range toks {
		k, v, ok := strings.Cut(t, "=")
		if !ok || i == 0 {
			continue
		}
		switch k {
		case "number", "sip":
			v = logctl.MaskNumber(v)
		case "name":
			v = fmt.Sprintf("<%d chars>", utf8.RuneCountInString(v))
		case "d":
			v = "*"
		}
		if strings.ContainsAny(v, " \"") || v == "" {
			v = `"` + strings.ReplaceAll(v, `"`, "") + `"`
		}
		toks[i] = k + "=" + v
	}
	return strings.Join(toks, " ")
}
