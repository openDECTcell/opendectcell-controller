/* SPDX-License-Identifier: AGPL-3.0-or-later */
/*
 * One cell's DECT base stack (libopendectcell's struct dect_base) hosted by the
 * controller: ADR 0002 option B, ../opendectcell/docs/hosting-dect-base.md.
 *
 * The host's own steps that sit beside the library -- which records go
 * into the chip, the audio bind, the side calls' lanes, the bridge verbs
 * the library does not answer -- are here, written after dect-stubd's
 * (the first host), so the golden traces replay the same. What crosses to
 * Go is I/O only: mails out, bridge lines, records to store, randomness,
 * the clock and the log (host.go).
 *
 * Everything lives in one C allocation; Go stages its inputs into the
 * buffers below and never passes a Go pointer to C.
 */
#ifndef CDECT_HOST_H
#define CDECT_HOST_H

#include <stddef.h>
#include <stdint.h>

#include "dect_base.h"
#include "dect_calls.h"
#include "dect_ctl.h"
#include "dect_dsaa.h"

#define CDECT_LANES	2u	/* side calls' audio lanes, as dect-stubd's XLANE_MAX */

/* Where a bridge line goes (cdectEvent). */
enum cdect_to {
	CDECT_TO_ASKER,		/* the reply to the client that sent the verb */
	CDECT_TO_BRIDGE,	/* call traffic: the bridge's client */
	CDECT_TO_ALL,		/* `hs` lines: every client */
};

/* cdect_host_ctl's result. */
enum cdect_ctl {
	CDECT_CTL_DONE,
	CDECT_CTL_BRIDGE,	/* the asking client now carries external calls */
};

/* A record staged by Go for cdect_host_record(). */
struct cdect_rec_in {
	unsigned slot;
	uint8_t  uak[DECT_MM_UAK_LEN];
	uint8_t  port_id[DECT_MM_PORTID_MAX];
	size_t   port_id_len;
	uint8_t  cc_port_id[DECT_CC_ID_MAX];
	size_t   cc_port_id_len;
	uint8_t  cc_fixed_id[DECT_CC_ID_MAX];
	size_t   cc_fixed_id_len;
};

/* A side call's audio lane (dect-stubd's struct xlane, without the TDM). */
struct cdect_lane {
	int     used, connected, wideband;
	uint8_t handle, channel;
};

struct cdect_host {
	struct dect_base  b;
	struct dect_calls calls;	/* the call lists the library keeps */
	uintptr_t h;			/* the Go side's handle */
	uint64_t  now;			/* the last clock the host was given */

	/* The audio bind's chip values (dect-stubd's defaults, cell-protocol.md §8.1). */
	uint8_t  sel, gain, flags, mode, codec;
	uint16_t mask;
	uint8_t  lane_ch[CDECT_LANES];
	struct cdect_lane lane[CDECT_LANES];

	/* The main link's call. */
	int      call_wideband;
	int      nonspeech_noted;
	uint64_t bind_ms;

	int      radio_up;		/* the cell's radio is on (STATUS) */
	int      sub_restored;		/* the records went into the chip */

	/* Go stages here. */
	struct dect_mail     in;
	struct cdect_rec_in  rec;
	char     line[DECT_CTL_LINE_MAX + 1];
	char     pin[DECT_PIN_DIGITS_MAX + 1];
	/* the registration PINs by index (API 139); b.cfg.reg_pins points here */
	char        reg_pin[DECT_MM_REG_PINS_MAX][DECT_PIN_DIGITS_MAX + 1];
	const char *reg_pin_ptr[DECT_MM_REG_PINS_MAX];
	char     out[DECT_CTL_LINE_MAX + 1];	/* a mail's params or a line, to Go */
};

struct cdect_host *cdect_host_new(uintptr_t h);
void cdect_host_free(struct cdect_host *c);

/* Record i from c->rec (present), or a free store for an enrolment. */
void cdect_host_record(struct cdect_host *c, unsigned i);
void cdect_host_free_store(struct cdect_host *c, unsigned i);
/* After the records and cfg: slots for free stores, record 0 current. */
int cdect_host_start(struct cdect_host *c);

/* The mail in c->in, from the chip (MAIL_UP). */
void cdect_host_mail(struct cdect_host *c, uint64_t now);
/* One loop pass: the host's steps and dect_base_tick(). */
void cdect_host_tick(struct cdect_host *c, uint64_t now);
/* The line in c->line from a client; is_bridge: that client is the bridge. */
int cdect_host_ctl(struct cdect_host *c, uint64_t now, int is_bridge);
/* `hello` and an `hs` line per listed record, to a client that connected. */
void cdect_host_hello(struct cdect_host *c);

#endif /* CDECT_HOST_H */
