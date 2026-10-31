// SPDX-License-Identifier: AGPL-3.0-or-later

package cdect

// #include <stdlib.h>
// #include "ppsim.h"
import "C"

import "unsafe"

// PP is a simulated handset's C-plane, for tests: the handset side of
// openDECTcell's golden traces (tests/test_stubd.c). It sends I-frames with N(S)
// and N(R) modulo 2 and acknowledges each of the base's I-frames at once.
type PP struct {
	c *C.struct_cdect_pp
}

// What PP.FromBase found in a base's 0x0244.
const (
	PPUnreadable = C.CDECT_PP_UNREADABLE // not a frame the handset can read
	PPNone       = C.CDECT_PP_NONE       // the base's RR or RNR
	PPPart       = C.CDECT_PP_PART       // an I-frame, more follows: answer RR
	PPMsg        = C.CDECT_PP_MSG        // a complete NWK message: answer RR
)

// NewPP starts a handset whose next frame opens its link.
func NewPP() *PP {
	c := (*C.struct_cdect_pp)(C.calloc(1, C.sizeof_struct_cdect_pp))
	if c == nil {
		panic("cdect: out of C memory")
	}
	c.first = 1
	return &PP{c: c}
}

// Close frees the handset. It is safe to call more than once.
func (p *PP) Close() {
	if p.c != nil {
		C.free(unsafe.Pointer(p.c))
		p.c = nil
	}
}

// Send returns the Lc frame of an I-frame carrying the NWK message nwk.
func (p *PP) Send(nwk []byte) ([]byte, error) {
	if len(nwk) == 0 || len(nwk) > len(p.c.in) {
		return nil, ErrLength
	}
	copyIn(p.c.in[:], nwk)
	n := C.cdect_pp_send(p.c, C.size_t(len(nwk)))
	if n == 0 {
		return nil, ErrLength
	}
	return C.GoBytes(unsafe.Pointer(&p.c.frame[0]), C.int(n)), nil
}

// RR returns the Lc frame of an RR for the base's last I-frame.
func (p *PP) RR() []byte {
	n := C.cdect_pp_rr(p.c)
	return C.GoBytes(unsafe.Pointer(&p.c.frame[0]), C.int(n))
}

// FromBase reads the parameters of a base's 0x0244 on a link signed lsig.
// With PPMsg, msg is the NWK message the frames carried.
func (p *PP) FromBase(lsig uint16, param []byte) (state int, msg []byte) {
	if len(param) > len(p.c.in) {
		return PPUnreadable, nil
	}
	copyIn(p.c.in[:], param)
	state = int(C.cdect_pp_base(p.c, C.uint16_t(lsig), C.size_t(len(param))))
	if state == PPMsg {
		msg = C.GoBytes(unsafe.Pointer(&p.c.msg[0]), C.int(p.c.msg_len))
		p.c.msg_len = 0
	}
	return state, msg
}

// LinkLSIG is the link signature of the base's link on handle.
func (x *Host) LinkLSIG(handle uint8) (uint16, bool) {
	l := C.cdect_host_lsig(x.c, C.uint8_t(handle))
	return uint16(l), l >= 0
}
