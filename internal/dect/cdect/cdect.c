// SPDX-License-Identifier: AGPL-3.0-or-later

#include <stdlib.h>

#include "cdect.h"

/* Zero memory in a way the compiler may not elide (keys live in these structs). */
static void cdect_wipe(void *p, size_t n)
{
	volatile uint8_t *v = p;

	while (n--)
		*v++ = 0;
}

struct cdect_lc *cdect_lc_new(uint16_t lsig)
{
	struct cdect_lc *c = calloc(1, sizeof(*c));

	if (c)
		dect_lc_rx_init(&c->rx, lsig);
	return c;
}

void cdect_lc_free(struct cdect_lc *c)
{
	if (!c)
		return;
	cdect_wipe(c, sizeof(*c));
	free(c);
}

int cdect_lc_feed(struct cdect_lc *c)
{
	return dect_lc_rx_fragment(&c->rx, c->in, c->out, sizeof(c->out));
}

size_t cdect_lc_build(struct cdect_lc *c, size_t lapc_len, uint16_t lsig)
{
	if (lapc_len > DECT_LC_LAPC_MAX)
		return 0;
	return dect_lc_build(c->in, lapc_len, lsig, c->out, sizeof(c->out));
}

size_t cdect_lapc_rr(struct cdect_lc *c, uint8_t nr, uint8_t pf)
{
	/* We are the FT: responses carry C/R = 0, class A uses LLN 1. */
	return dect_lapc_build_rr(c->in, sizeof(c->in), 0, 0, DECT_LAPC_LLN_A, nr, pf);
}
