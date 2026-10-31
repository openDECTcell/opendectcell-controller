/* SPDX-License-Identifier: AGPL-3.0-or-later */
/* A simulated handset's C-plane, for tests (ppsim.go). */
#ifndef CDECT_PPSIM_H
#define CDECT_PPSIM_H

#include "host.h"

enum {
	CDECT_PP_UNREADABLE = -1,	/* not a frame the handset can read */
	CDECT_PP_NONE,			/* the base's RR or RNR: LAPC's own */
	CDECT_PP_PART,			/* an I-frame, more of the message follows */
	CDECT_PP_MSG,			/* an I-frame that completes the message in msg */
};

struct cdect_pp {
	uint8_t ns, nr, first;
	uint8_t msg[DECT_LAPC_MSG_MAX];	/* the base's NWK message so far */
	size_t  msg_len;
	uint8_t in[DECT_MAIL_PARAM_MAX];	/* staged by Go */
	uint8_t frame[DECT_LC_FRAME_MAX];	/* the handset's Lc frame, to Go */
};

/* An I-frame carrying the NWK message in pp->in (n octets), as an Lc frame. */
size_t cdect_pp_send(struct cdect_pp *pp, size_t n);
/* An RR acknowledging the base's last I-frame, as an Lc frame. */
size_t cdect_pp_rr(struct cdect_pp *pp);
/* The base's 0x0244 parameters in pp->in (n octets), on a link signed lsig. */
int cdect_pp_base(struct cdect_pp *pp, uint16_t lsig, size_t n);

/* The link signature of the base's link on handle, or -1 when none. */
long cdect_host_lsig(const struct cdect_host *c, uint8_t handle);

#endif /* CDECT_PPSIM_H */
