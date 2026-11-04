// SPDX-License-Identifier: AGPL-3.0-or-later

package handset

import (
	"encoding/hex"
	"strings"
)

// piBit is one bit of a profile indicator octet (bit 1 is the lowest) and what it says in plain words.
type piBit struct {
	bit  uint
	name string
}

// The profile indicator octets of <<TERMINAL-CAPABILITY>>, EN 300 175-5 V2.9.1 §7.7.41 (octet group 4, "Profile
// Indicator_1" to "_11"), in the words a user reads. Bits the standard reserves are not listed.
var (
	piProfiles = [11][]piBit{
		{{1, "CAP (cordless access profile)"}, {2, "GAP (basic cordless telephony)"}, {3, "DECT/GSM interworking"}, {4, "ISDN end system"},
			{5, "LRMS (data service)"}, {6, "DPRS stream (packet data)"}, {7, "DPRS asymmetric bearers"}},
		{{1, "DPRS class 2 management"}, {2, "Data services profile D"}, {3, "ISDN intermediate access"}, {4, "UMTS-GSM bearer service"},
			{5, "UMTS-GSM SMS"}, {6, "UMTS-GSM fax"}, {7, "RAP 1"}},
		{{1, "DECT/GSM dual-mode terminal"}, {2, "repeater (WRS V1)"}, {3, "SMS over LRMS"}, {4, "DMAP"},
			{5, "multiport CTA"}, {6, "Ethernet"}, {7, "Token Ring"}},
		{{1, "IP"}, {2, "PPP"}, {3, "V.24"}, {4, "CF"}, {5, "IPQ services"}, {6, "RAP 2"}, {7, "generic media encapsulation"}},
		nil, // 5: the modulation, below
		nil, // 6: features
		nil, // 7: features
		nil, // 8: packet data
		{{1, "DPRS class 3 management"}, {2, "DPRS class 4 management"}, {6, "light data services"}},
		nil, // 10: features
		nil, // 11: features
	}
	piFeatures = [11][]piBit{
		5: {{1, "DECT/UMTS interworking"}, {2, "GPRS services"}, {3, "basic ODAP"}, {4, "F-MMS interworking"}, {5, "channel GF"},
			{6, "fast-hopping radio"}, {7, "no-emission mode (radio silent while idle)"}},
		6:  {{4, "headset management"}, {5, "cipher re-keying and early encryption"}, {6, "own melody per contact"}, {7, "NG-DECT Part 5"}},
		9:  {{1, "date and time recovery"}, {2, "extended list change notification"}, {3, "screening"}, {4, "repeater (WRS V2)"}, {5, "ULE relay"}},
		10: {{1, "wireless microphone"}, {2, "advanced audio microphone"}, {3, "advanced audio speaker or headphone"}},
	}
	// the packet data categories of octet 4g (bits 6 to 4)
	piPacketData = map[byte]string{1: "packet data category 1", 2: "packet data category 2", 3: "packet data category 3",
		4: "packet data category 4 (up to 8PSK)", 5: "packet data category 4 (up to 64QAM)"}
	// the modulations of octet 4d (bits 4 to 1), the value 0 being the basic one
	piModulation = map[byte]string{0: "GFSK only", 2: "BPSK", 4: "QPSK", 5: "QPSK", 6: "8PSK", 7: "8PSK", 8: "16QAM", 9: "16QAM", 10: "64QAM", 11: "64QAM"}
)

// DecodeProfiles reads the profile indicator octets a handset sent (as the DECT core stored them, in hexadecimal)
// into two lists of plain words: the profiles it supports (GAP, CAP, data profiles) and its other features (radio,
// encryption, headset). NG-DECT Part 1 and Part 3 are not named here: they are the "Standard" row. Octets this
// version of the standard does not know are ignored; so are bits it reserves.
func DecodeProfiles(hexOctets string) (profiles, features []string) {
	raw, err := hex.DecodeString(strings.TrimSpace(hexOctets))
	if err != nil {
		return nil, nil
	}
	for i, o := range raw {
		if i >= len(piProfiles) {
			break
		}
		for _, b := range piProfiles[i] {
			if o>>(b.bit-1)&1 == 1 {
				profiles = append(profiles, b.name)
			}
		}
		for _, b := range piFeatures[i] {
			if o>>(b.bit-1)&1 == 1 {
				features = append(features, b.name)
			}
		}
		switch i {
		case 4: // modulation: bits 4 to 1
			if m, ok := piModulation[o&0x0f]; ok {
				features = append(features, "radio: "+m)
			}
		case 7: // packet data category: bits 6 to 4
			if c, ok := piPacketData[o>>3&7]; ok {
				features = append(features, c)
			}
		}
	}
	return profiles, features
}

// Standard is the NG-DECT standard the handset claims, without a note on wideband (the page has a row for that).
// Records the DECT core stored before it dropped the note are cleaned here.
func (h Handset) Standard() string {
	s, _ := h.Capabilities["standard"].(string)
	return strings.TrimSuffix(s, " (wideband)")
}

// ProfileNames is the profiles in the handset's capability in plain words, see DecodeProfiles.
func (h Handset) ProfileNames() []string {
	p, _ := h.decoded()
	return p
}

// Features is the other features in the handset's capability in plain words, see DecodeProfiles.
func (h Handset) Features() []string {
	_, f := h.decoded()
	return f
}

func (h Handset) decoded() (profiles, features []string) {
	s, _ := h.Capabilities["profiles"].(string)
	return DecodeProfiles(s)
}

// SignalWord is a rough word for a field strength measured at the base, for people who do not read dBm: DECT
// receivers work down to about -90 dBm, and the bands are a guide, not a standard.
func SignalWord(dBm int) string {
	switch {
	case dBm >= -65:
		return "excellent"
	case dBm >= -75:
		return "good"
	case dBm >= -85:
		return "fair"
	}
	return "weak"
}
