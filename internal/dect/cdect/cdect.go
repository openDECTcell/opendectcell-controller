// SPDX-License-Identifier: AGPL-3.0-or-later

// Package cdect is the only cgo package: a thin wrapper around libopendectcell
// (docs/libopendectcell-integration.md). This first cut is the A2 proof of concept
// and wraps just the Lc layer and one LAPC frame builder.
//
// Bytes cross the boundary by copy into and out of C-owned buffers; no Go
// pointer is ever passed to C.
package cdect

// #cgo CFLAGS: -std=c11 -O2 -Wall -Wextra -Werror -I${SRCDIR}/../../../third_party/opendectcell/src/libopendectcell
// #include "cdect.h"
import "C"

import (
	"errors"
	"unsafe"
)

// FragmentLen is the size of one Cs fragment carried in a 0x0203 mail.
const FragmentLen = C.DECT_LC_FRAG_LEN

// MaxLAPC is the largest LAPC frame (address, control, length, information).
const MaxLAPC = C.DECT_LC_LAPC_MAX

var (
	// ErrDropped means the Lc layer discarded a frame (checksum or length
	// error) and resynchronised.
	ErrDropped = errors.New("cdect: frame dropped")
	// ErrLength means the input does not fit the layer's limits.
	ErrLength = errors.New("cdect: bad length")
	// ErrClosed means the receiver was used after Close.
	ErrClosed = errors.New("cdect: use after close")
)

// LcRx reassembles Cs fragments into LAPC frames for one radio connection.
// It is not safe for concurrent use: one session goroutine owns it.
type LcRx struct {
	c *C.struct_cdect_lc
}

// NewLcRx allocates a receiver whose first frame is tried with link
// signature lsig (0 for an unregistered handset).
func NewLcRx(lsig uint16) *LcRx {
	c := C.cdect_lc_new(C.uint16_t(lsig))
	if c == nil {
		panic("cdect: out of C memory")
	}
	return &LcRx{c: c}
}

// Close zeroes and frees the C state. It is safe to call more than once.
func (r *LcRx) Close() {
	if r.c != nil {
		C.cdect_lc_free(r.c)
		r.c = nil
	}
}

// LSIG returns the link signature the receiver has locked onto.
func (r *LcRx) LSIG() uint16 {
	if r.c == nil {
		return 0
	}
	return uint16(r.c.rx.lsig)
}

// Feed adds one Cs fragment. It returns a complete LAPC frame, nil if more
// fragments are needed, or ErrDropped if a frame failed its checks.
func (r *LcRx) Feed(seg []byte) ([]byte, error) {
	if r.c == nil {
		return nil, ErrClosed
	}
	if len(seg) != FragmentLen {
		return nil, ErrLength
	}
	copyIn(r.c.in[:], seg)
	n := int(C.cdect_lc_feed(r.c))
	switch {
	case n < 0:
		return nil, ErrDropped
	case n == 0:
		return nil, nil
	}
	return C.GoBytes(unsafe.Pointer(&r.c.out[0]), C.int(n)), nil
}

// BuildFrame wraps a LAPC frame in an Lc frame (fill plus checksum signed
// with lsig), ready to be sent with APPEND_CONNECTION_DATA.
func (r *LcRx) BuildFrame(lapc []byte, lsig uint16) ([]byte, error) {
	if r.c == nil {
		return nil, ErrClosed
	}
	if len(lapc) < 3 || len(lapc) > MaxLAPC {
		return nil, ErrLength
	}
	copyIn(r.c.in[:], lapc)
	n := int(C.cdect_lc_build(r.c, C.size_t(len(lapc)), C.uint16_t(lsig)))
	if n == 0 {
		return nil, ErrLength
	}
	return C.GoBytes(unsafe.Pointer(&r.c.out[0]), C.int(n)), nil
}

// BuildRR returns a LAPC receive-ready frame from the fixed part.
func (r *LcRx) BuildRR(nr uint8, pf bool) ([]byte, error) {
	if r.c == nil {
		return nil, ErrClosed
	}
	var p C.uint8_t
	if pf {
		p = 1
	}
	n := int(C.cdect_lapc_rr(r.c, C.uint8_t(nr), p))
	if n == 0 {
		return nil, ErrLength
	}
	return C.GoBytes(unsafe.Pointer(&r.c.in[0]), C.int(n)), nil
}

// copyIn copies Go bytes into a C-owned buffer; callers check the length.
func copyIn(dst []C.uint8_t, src []byte) {
	for i, b := range src {
		dst[i] = C.uint8_t(b)
	}
}
