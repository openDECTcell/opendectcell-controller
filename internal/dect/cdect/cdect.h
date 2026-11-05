/* SPDX-License-Identifier: AGPL-3.0-or-later */
/*
 * C side of the cgo boundary (docs/libopendectcell-integration.md §5).
 *
 * Every libopendectcell struct and every I/O buffer lives in one C allocation, so
 * Go only ever holds a C pointer and no Go pointer is passed to C.
 */
#ifndef CDECT_H
#define CDECT_H

#include <stddef.h>
#include <stdint.h>

#include "dect_proto.h"
#include "dect_lc.h"
#include "dect_lapc.h"
#include "dect_mm.h"

/*
 * The libopendectcell API this package was written against. A bump means a public
 * signature or struct layout changed: re-check libopendectcell-integration.md §1,
 * then raise this number.
 */
#if OPENDECTCELL_API_VERSION != 158
#error "libopendectcell API version mismatch: cdect is written for OPENDECTCELL_API_VERSION 158"
#endif

struct cdect_lc {
	struct dect_lc_rx rx;
	uint8_t in[DECT_LC_FRAME_MAX];	/* input copied in from Go */
	uint8_t out[DECT_LC_FRAME_MAX];	/* output copied out by Go */
};

struct cdect_lc *cdect_lc_new(uint16_t lsig);
void cdect_lc_free(struct cdect_lc *c);

/* Feed the 5-octet fragment in c->in; a complete LAPC frame lands in c->out. */
int cdect_lc_feed(struct cdect_lc *c);

/* Build an Lc frame from the LAPC frame in c->in (lapc_len octets) into c->out. */
size_t cdect_lc_build(struct cdect_lc *c, size_t lapc_len, uint16_t lsig);

/* Build a LAPC RR frame into c->in, ready for cdect_lc_build. */
size_t cdect_lapc_rr(struct cdect_lc *c, uint8_t nr, uint8_t pf);

#endif /* CDECT_H */
