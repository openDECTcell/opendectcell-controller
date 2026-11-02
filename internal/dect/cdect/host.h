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
	uint8_t  tpui[3];	/* all zero: derived from the slot */
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
	int      no_dummy;		/* trace replays: no dummy key in an empty table */
	int      started;		/* cdect_host_start has run: a new record is a runtime add */
	/* the identity of a held LOCATE or CC-SETUP the host was asked about (defer_unknown) */
	uint8_t  fetch_id[DECT_MM_PORTID_MAX];
	size_t   fetch_len;

	/* record i's own Contact List (SetContacts), NULL until it is given one */
	struct dect_contacts *contacts[DECT_BASE_HS_MAX];

	/* The field strength the chip last reported on a link to each record (dBm), and when (0: never). */
	int      sig_dbm[DECT_BASE_HS_MAX];
	uint64_t sig_ms[DECT_BASE_HS_MAX];
	uint64_t sig_poll_ms;		/* when the chip was last asked */
	int      poll_signal;		/* ask at all (Config.PollSignal) */

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

/* Record i from c->rec (present), or a free store for an enrolment (takes only c->rec.tpui). */
void cdect_host_record(struct cdect_host *c, unsigned i);
void cdect_host_free_store(struct cdect_host *c, unsigned i);
/* After the records and cfg: slots for free stores, record 0 current. */
int cdect_host_start(struct cdect_host *c);

/* The mail in c->in, from the chip (MAIL_UP). */
void cdect_host_mail(struct cdect_host *c, uint64_t now);
/*
 * The host has answered cdectFetch(): the record is in the table (cdect_host_record) or
 * there is none. Hold it on the live link and answer the held request.
 */
void cdect_host_resume(struct cdect_host *c, uint64_t now);
/* Take record i out of the table; 0, or -1 when the base is not quiet or i is no record. */
int cdect_host_drop(struct cdect_host *c, unsigned i);
int cdect_host_recycle(struct cdect_host *c, unsigned i);
int cdect_host_quiet(const struct cdect_host *c);
/* The last field strength measured for record i: 1 and dBm and the clock it was taken at; 0 if none. */
int cdect_host_signal(const struct cdect_host *c, unsigned i, int *dbm, uint64_t *at);
/*
 * What record i's handset said of itself in <<TERMINAL-CAPABILITY>>, from the link it
 * is on: 1 and the values, 0 if there is no such report.
 */
int cdect_host_capability(const struct cdect_host *c, unsigned i, uint8_t *ng_part1, uint8_t *ng_part3,
			  uint8_t *display, uint8_t *tone, uint8_t *profiles, size_t cap, size_t *n_profiles);
/* Record i's name in the Internal Names List: 0, or -1 if the library refuses it. */
int cdect_host_set_name(struct cdect_host *c, unsigned i, const char *name);
/*
 * Record i behaves as the handset model of two profile files (either may be
 * empty): 0, or -1 for a file the library cannot read (the record keeps the
 * standard behaviour).
 */
int cdect_host_set_profile(struct cdect_host *c, unsigned i, const char *mfr, size_t mfr_len,
			   const char *dev, size_t dev_len);
/*
 * Give record i a Contact List from a contacts file (dect_contacts.h): a store is made on
 * the first call and attached to the record (dect_base_record_contacts). 0, or -1 for a
 * text that is not a contacts file, or no memory.
 */
int cdect_host_set_contacts(struct cdect_host *c, unsigned i, const char *text, size_t len);
/*
 * Replace the call lists with a call lists file (dect_calls.h); a record whose handset has more
 * unread missed calls on a line than before is told (the missed call notice). 0, or -1 if the
 * text is not a call lists file.
 */
int cdect_host_set_calls(struct cdect_host *c, const char *text, size_t len);
/* One entry of the remote directory (empty name: remove it), and the handsets told of the change (API 152). 0, or -1. */
int cdect_host_remote_set(struct cdect_host *c, unsigned id, const char *name);
void cdect_host_names_refresh(struct cdect_host *c);
/* The oldest journalled change of record i's Contact List as a line in c->out (no newline): its length, 0 if none. */
size_t cdect_host_contact_change(struct cdect_host *c, unsigned i);
/* Tests: a contact "name" put into record i's list as a handset would (journalled). */
int cdect_host_test_contact_put(struct cdect_host *c, unsigned i, const char *name, const char *digits);
/* One loop pass: the host's steps and dect_base_tick(). */
void cdect_host_tick(struct cdect_host *c, uint64_t now);
/* When record i last had C-plane traffic, on the stack's clock; 0: never. */
uint64_t cdect_host_last_seen(const struct cdect_host *c, unsigned i);
/* The line in c->line from a client; is_bridge: that client is the bridge. */
int cdect_host_ctl(struct cdect_host *c, uint64_t now, int is_bridge);
/* `hello` and an `hs` line per listed record, to a client that connected. */
void cdect_host_hello(struct cdect_host *c);

#endif /* CDECT_HOST_H */
