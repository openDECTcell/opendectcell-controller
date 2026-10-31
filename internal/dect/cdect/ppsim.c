// SPDX-License-Identifier: AGPL-3.0-or-later
/*
 * A simulated handset's C-plane for tests (ppsim.go): what openDECTcell's
 * tests/test_stubd.c plays against the base, I-frames with N(S) and N(R)
 * modulo 2, each of the base's I-frames acknowledged at once.
 */
#include <string.h>

#include "ppsim.h"

size_t cdect_pp_send(struct cdect_pp *pp, size_t n)
{
	uint8_t lapc[DECT_LC_LAPC_MAX];
	size_t fl = dect_lapc_build_i(lapc, sizeof(lapc), 0, pp->first,
				      DECT_LAPC_LLN_A, pp->ns, pp->nr, 0, 0,
				      pp->in, n);

	if (!fl)
		return 0;
	pp->first = 0;
	pp->ns ^= 1u;
	return dect_lc_build(lapc, fl, 0, pp->frame, sizeof(pp->frame));
}

size_t cdect_pp_rr(struct cdect_pp *pp)
{
	uint8_t rr[4];
	size_t n = dect_lapc_build_rr(rr, sizeof(rr), 0, 0, DECT_LAPC_LLN_A,
				      pp->nr, 0);

	return n ? dect_lc_build(rr, n, 0, pp->frame, sizeof(pp->frame)) : 0;
}

int cdect_pp_base(struct cdect_pp *pp, uint16_t lsig, size_t n)
{
	struct dect_lc_rx rx;
	uint8_t lapc[DECT_LC_LAPC_MAX];
	struct dect_lapc_frame f;
	size_t off, octets;
	int got = 0;

	if (n < 2)
		return CDECT_PP_UNREADABLE;
	octets = pp->in[1];	/* 0x0244 {handle, length, fragments} */
	dect_lc_rx_init(&rx, lsig);
	for (off = 0; off + 5u <= octets && 2u + off + 5u <= n; off += 5u) {
		int r = dect_lc_rx_fragment(&rx, &pp->in[2 + off], lapc,
					    sizeof(lapc));

		if (r > 0)
			got = r;
	}
	if (got <= 0 || dect_lapc_parse(lapc, (size_t)got, &f))
		return CDECT_PP_UNREADABLE;
	if (f.type != DECT_LAPC_I)
		return CDECT_PP_NONE;
	if (pp->msg_len + f.info_len <= sizeof(pp->msg)) {
		memcpy(pp->msg + pp->msg_len, f.info, f.info_len);
		pp->msg_len += f.info_len;
	}
	pp->nr = (uint8_t)((f.ns + 1u) & 1u);
	return f.more ? CDECT_PP_PART : CDECT_PP_MSG;
}

long cdect_host_lsig(const struct cdect_host *c, uint8_t handle)
{
	unsigned k;

	if (c->b.main_link.active && c->b.main_link.handle == handle)
		return c->b.main_link.link.lc.lsig;
	for (k = 0; k < DECT_BASE_SIDES; k++)
		if (c->b.side[k].active && c->b.side[k].handle == handle)
			return c->b.side[k].link.lc.lsig;
	return -1;
}
