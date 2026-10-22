// SPDX-License-Identifier: AGPL-3.0-or-later

// Package callclass reads the <<BASIC-SERVICE>> value octet of a CC-SETUP
// (EN 300 175-5 V2.9.1 §7.6.4) and decides whether the session is a voice
// call the controller may bridge to SIP.
//
// A {CC-SETUP}…{CC-CONNECT} is not necessarily a call: a handset reading
// the phonebook or call list (CAT-iq list access, class 0x2) runs the same
// sequence, and a Gigaset A540 does so several times for every real call
// (openDECTcell docs/notes-for-the-controller.md, 2026-10-22). Bridging on
// CONNECT alone would connect phonebook lookups to SIP. The DECT core (C3)
// must ask Bridge before opening media or dialling.
package callclass

import "fmt"

// Class is the high nibble of the <<BASIC-SERVICE>> value.
type Class uint8

// Service is the low nibble.
type Service uint8

const (
	ListAccess    Class = 0x2 // CAT-iq list access (LiA service setup)
	ULE           Class = 0x3
	Message       Class = 0x4
	IIP           Class = 0x7 // DECT/ISDN intermediate system
	Normal        Class = 0x8
	Internal      Class = 0x9
	Emergency     Class = 0xA
	ServiceCall   Class = 0xB
	ExtHandover   Class = 0xC
	Supplementary Class = 0xD
	OAM           Class = 0xE
)

const (
	BasicSpeech    Service = 0x0
	WidebandSpeech Service = 0x8
	SUOTA1         Service = 0x9
	SUOTA2         Service = 0xA
	DTAMWideband   Service = 0xB
	Other          Service = 0xF
)

var classNames = map[Class]string{
	ListAccess: "list access", ULE: "ULE service", Message: "message call", IIP: "DECT/ISDN IIP",
	Normal: "normal call", Internal: "internal call", Emergency: "emergency call", ServiceCall: "service call",
	ExtHandover: "external handover", Supplementary: "supplementary service", OAM: "OA&M",
}

func (c Class) String() string {
	if n, ok := classNames[c]; ok {
		return n
	}
	return fmt.Sprintf("class 0x%X", uint8(c))
}

// Parse splits a <<BASIC-SERVICE>> value octet.
func Parse(v uint8) (Class, Service) { return Class(v >> 4), Service(v & 0x0F) }

// Bridge reports whether a session of this class is a voice call to bridge
// to SIP:
//
//   - 0x8 normal call: yes.
//   - 0xA emergency call: yes. Emergency calls are never blocked
//     (voip-requirements.md §3); the number goes to the PBX unchanged.
//   - 0x9 internal call: not yet. Internal calls go through the SIP server
//     like any other (architecture.md §6), but how a handset's INT key maps
//     to a SIP destination is undesigned; until then it is refused.
//   - everything else (list access, ULE, SUOTA sessions, …): no. These are
//     data sessions; list access is answered by the DECT core itself.
func Bridge(c Class) bool { return c == Normal || c == Emergency }

// Wideband reports whether the service asks for wideband speech.
func Wideband(s Service) bool { return s == WidebandSpeech || s == DTAMWideband }
