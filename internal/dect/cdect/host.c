// SPDX-License-Identifier: AGPL-3.0-or-later
/*
 * The controller's host of struct dect_base (host.h). Each step names the
 * dect-stubd function it follows (third_party/opendectcell/src/stubd/dect-stubd.c
 * at the submodule's commit); where the controller differs, it says so.
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "host.h"
#include "_cgo_export.h"

static void wipe(void *p, size_t n)
{
	volatile uint8_t *v = p;

	while (n--)
		*v++ = 0;
}

/* ---- I/O: to Go ---------------------------------------------------------- */

static void line_to(struct cdect_host *c, int to, struct dect_ctl_out *o)
{
	int n = dect_ctl_end(o);

	if (n < 0) {
		dect_base_log(&c->b, "ctl: a line overflowed %u octets -- not sent",
			      DECT_CTL_LINE_MAX);
		return;
	}
	cdectEvent(c->h, to, o->buf, (size_t)n);
}

static void reply(struct cdect_host *c, const char *verb, const char *reason,
		  const char *key, unsigned v)
{
	struct dect_ctl_out o;

	dect_ctl_begin(&o, reason ? "error" : "ok");
	dect_ctl_add(&o, "verb", verb);
	if (key)
		dect_ctl_add_uint(&o, key, v);
	if (reason)
		dect_ctl_add(&o, "reason", reason);
	line_to(c, CDECT_TO_ASKER, &o);
}

/* dect-stubd's ctl_hs_to(), without the profile's model (no profiles yet). */
static void hs_line(struct cdect_host *c, int to, unsigned i, const char *state)
{
	struct dect_ctl_out o;
	char nm[DECT_IWU_NAME_MAX];

	if (i >= c->b.n_hs)
		return;
	dect_ctl_begin(&o, "hs");
	dect_ctl_add_uint(&o, "hs", dect_base_slot(&c->b, i));
	dect_ctl_add(&o, "state", state);
	if (strcmp(state, "gone")) {
		dect_base_hs_name(&c->b, i, nm, sizeof(nm));
		dect_ctl_add(&o, "name", nm);
		if (c->b.cap[i].known)
			dect_ctl_add_uint(&o, "part3", c->b.cap[i].part3 ? 1 : 0);
		dect_ctl_add_uint(&o, "wideband", !c->b.cfg.narrowband &&
				  (dect_base_basic_svc(&c->b, (int)i,
						       c->b.cfg.basic_svc) &
				   0x0Fu) == 0x08u);
	}
	line_to(c, to, &o);
}

void cdect_host_hello(struct cdect_host *c)
{
	struct dect_ctl_out o;
	unsigned i, n = 0;

	for (i = 0; i < c->b.n_hs; i++)
		n += (unsigned)dect_base_listed(&c->b, i);
	dect_ctl_begin(&o, "hello");
	dect_ctl_add_uint(&o, "api", OPENDECTCELL_API_VERSION);
	dect_ctl_add_uint(&o, "lines", 1);
	dect_ctl_add_uint(&o, "hs", n);
	line_to(c, CDECT_TO_ASKER, &o);
	for (i = 0; i < c->b.n_hs; i++)
		if (dect_base_listed(&c->b, i))
			hs_line(c, CDECT_TO_ASKER, i, "registered");
}

/* ---- The chip's slots: dect-stubd's send_subscription() ---------------- */

static uint8_t live_slot_mask(const struct cdect_host *c)
{
	unsigned i, mask = 0;

	for (i = 0; i < c->b.n_hs; i++) {
		unsigned sl = dect_base_slot(&c->b, i);
		int valid = i == c->b.cur ? c->b.rec.uak_valid
					  : c->b.loaded[i] && c->b.hs[i].uak_valid;

		if (valid && sl >= 1u && sl <= DECT_MM_MAX_SLOT)
			mask |= 1u << (sl - 1u);
	}
	return (uint8_t)mask;
}

/*
 * Every record this cell serves into its chip slot, the current one last so
 * its read-back is the one outstanding; every other slot cleared and the
 * clear audited. dect-stubd's holdback= and roamers are lab options with no
 * counterpart here.
 */
static int any_uak(const struct cdect_host *c);

static void install(struct cdect_host *c)
{
	/*
	 * A record numbered above the chip's six slots is the host's alone
	 * (API 144): it has no key in the chip and nothing to read back.
	 */
	unsigned cur = dect_base_slot(&c->b, c->b.cur);
	int curchip = cur >= 1u && cur <= DECT_MM_MAX_SLOT && c->b.rec.uak_valid;
	uint8_t slot = curchip ? (uint8_t)cur : 0u;
	unsigned keep = curchip ? 1u << slot : 0u;
	/*
	 * No record holds a key: the chip still needs one in its table to
	 * run (07.56 stays silent on an empty table, bench 2026-10-31), but
	 * none of an earlier run's, so slot 1 gets a random key nobody
	 * holds. A handset's own key replaces it when one is installed.
	 */
	int dummy = !c->no_dummy && !any_uak(c);
	unsigned i;

	if (dummy)
		keep = 1u << 1;

	for (i = 0; i < c->b.n_hs; i++) {
		unsigned hsl = dect_base_slot(&c->b, i);

		if (i != c->b.cur && c->b.loaded[i] && c->b.hs[i].uak_valid &&
		    hsl >= 1u && hsl <= DECT_MM_MAX_SLOT)
			keep |= 1u << hsl;
	}
	dect_base_slot_clear(&c->b, keep);
	dect_base_slot_audit(&c->b, slot,
			     (uint8_t)(live_slot_mask(c) | (dummy ? 1u : 0u)),
			     c->now);
	if (dummy) {
		uint8_t k[DECT_MM_UAK_LEN];

		cdectRandom(c->h, k, sizeof(k));
		(void)dect_base_slot_install(&c->b, 1u, k);
		memset(k, 0, sizeof(k));
		dect_base_slot_mask(&c->b, 1u);
		return;
	}
	for (i = 0; i < c->b.n_hs; i++) {
		unsigned hsl = dect_base_slot(&c->b, i);

		if (i != c->b.cur && c->b.loaded[i] && c->b.hs[i].uak_valid &&
		    hsl >= 1u && hsl <= DECT_MM_MAX_SLOT && hsl != slot)
			(void)dect_base_slot_install(&c->b, hsl, c->b.hs[i].uak);
	}
	if (curchip) {
		(void)dect_base_slot_install(&c->b, slot, c->b.rec.uak);
		dect_base_uak_verify(&c->b, slot, c->now);
	}
	dect_base_slot_mask(&c->b, live_slot_mask(c));
}

static int any_uak(const struct cdect_host *c)
{
	unsigned i;

	unsigned cur = dect_base_slot(&c->b, c->b.cur);

	if (c->b.rec.uak_valid && cur >= 1u && cur <= DECT_MM_MAX_SLOT)
		return 1;
	for (i = 0; i < c->b.n_hs; i++) {
		unsigned sl = dect_base_slot(&c->b, i);

		if (i != c->b.cur && c->b.loaded[i] && c->b.hs[i].uak_valid &&
		    sl >= 1u && sl <= DECT_MM_MAX_SLOT)
			return 1;
	}
	return 0;
}

/* ---- Audio: dect-stubd's send_audio_*(), xlane_*() --------------------- */

/*
 * The controller sends the bind's mails over the tunnel (the maintainer,
 * 2026-10-30; cell-protocol.md §8.1): 0x0251 and 0x0230 here, 0x022D on the
 * chip's 0x0242, and cdectMedia() for MEDIA_OPEN/MEDIA_CLOSE.
 */
static void main_bind(struct cdect_host *c, uint8_t handle)
{
	int wb = c->call_wideband;

	dect_base_audio_param(&c->b, handle, c->sel, c->gain);
	dect_base_audio_assign(&c->b, handle,
			       wb ? (uint8_t)(c->flags | 0x40u) : c->flags,
			       c->mask, c->mode, wb ? 0u : c->codec);
}

static void main_connect(struct cdect_host *c, uint8_t handle, uint8_t chan)
{
	if (c->b.main_link.path_on)
		return;
	c->b.main_link.path_on = 1;
	dect_base_audio_path(&c->b, handle);
	cdectMedia(c->h, handle, chan, c->call_wideband, 1);
}

static void audio_release(struct cdect_host *c, uint8_t handle)
{
	dect_base_audio_release(&c->b, handle);
	cdectMedia(c->h, handle, 0, 0, 0);
}

/* No 0x0242 within 500 ms: connect anyway (dect-stubd's audio_connect_fallback()). */
static void connect_fallback(struct cdect_host *c, uint64_t now)
{
	if (!c->b.main_link.bound || c->b.main_link.path_on ||
	    !c->b.main_link.active || now - c->bind_ms < 500u)
		return;
	dect_base_log(&c->b, "call: audio connect: no 0x0242 within 500 ms, "
		      "connecting anyway");
	main_connect(c, c->b.main_link.handle, c->flags & 0x0Fu);
}

static struct cdect_lane *lane_of(struct cdect_host *c, uint8_t handle)
{
	unsigned i;

	for (i = 0; i < CDECT_LANES; i++)
		if (c->lane[i].used && c->lane[i].handle == handle)
			return &c->lane[i];
	return NULL;
}

static int lane_free(const struct cdect_host *c)
{
	unsigned i;

	for (i = 0; i < CDECT_LANES; i++)
		if (!c->lane[i].used)
			return (int)i;
	return -1;
}

/* dect-stubd's lanes_available(): free lanes less those already promised. */
static int lanes_left(const struct cdect_host *c, int me)
{
	int n = 0;
	unsigned i;

	for (i = 0; i < CDECT_LANES; i++)
		if (!c->lane[i].used)
			n++;
	for (i = 0; i < DECT_BASE_CALLS; i++)
		if (c->b.calls[i].id && c->b.calls[i].party == DECT_BASE_PARTY_PAGING)
			n--;
	for (i = 0; i < DECT_BASE_SIDES; i++) {
		const struct dect_base_link *o = &c->b.side[i];

		if ((int)i != me && o->active && o->lane < 0 &&
		    dect_cc_busy(&o->link.cc))
			n--;
	}
	return n;
}

/* dect-stubd's xlane_bind(): the side call's own channel, as stock binds a second call. */
static void lane_bind(struct cdect_host *c, struct dect_base_link *bl, int i)
{
	struct cdect_lane *x = &c->lane[i];
	uint8_t flags;

	memset(x, 0, sizeof(*x));
	x->used = 1;
	x->handle = bl->handle;
	x->channel = c->lane_ch[i];
	x->wideband = !c->b.cfg.narrowband &&
		      dect_cc_codec(&bl->link.cc) == DECT_CC_CODEC_G722;
	bl->lane = i;
	dect_base_audio_param(&c->b, x->handle, x->channel, c->gain);
	flags = (uint8_t)((c->flags & 0xB0u) | (1u << (x->channel - 1u)) |
			  (x->wideband ? 0x40u : 0u));
	dect_base_audio_assign(&c->b, x->handle, flags,
			       (uint16_t)(1u << x->channel), c->mode,
			       x->wideband ? 0u : c->codec);
}

static void lane_connect(struct cdect_host *c, struct cdect_lane *x)
{
	if (x->connected)
		return;
	x->connected = 1;
	dect_base_audio_path(&c->b, x->handle);
	cdectMedia(c->h, x->handle, x->channel, x->wideband, 1);
}

static void lane_release(struct cdect_host *c, struct dect_base_link *bl)
{
	if (bl->lane < 0)
		return;
	if (c->lane[bl->lane].connected || c->lane[bl->lane].used)
		audio_release(c, bl->handle);
	memset(&c->lane[bl->lane], 0, sizeof(c->lane[0]));
	bl->lane = -1;
}

/* ---- The callbacks (struct dect_base_ops) ----------------------------- */

static void op_save(void *ctx, unsigned i, const struct dect_hs_rec *r)
{
	struct cdect_host *c = ctx;

	/*
	 * The record exists now and is no longer a free store (dect-stubd's
	 * base_save() does the same): without it a second enrolment picks the
	 * same store, and the record cannot be recycled.
	 */
	if (i < c->b.n_hs && r->port_id_len) {
		c->b.loaded[i] = 1;
		c->b.absent[i] = 0;
	}
	cdectSave(c->h, i, (struct dect_hs_rec *)r);
}

static int op_send(void *ctx, const struct dect_mail *m)
{
	struct cdect_host *c = ctx;

	return cdectSend(c->h, (struct dect_mail *)m);
}

static void op_random(void *ctx, uint8_t *out, size_t n)
{
	struct cdect_host *c = ctx;

	cdectRandom(c->h, out, n);
}

static void op_log(void *ctx, const char *line)
{
	struct cdect_host *c = ctx;

	cdectLog(c->h, (char *)line);
}

static void op_install(void *ctx)
{
	install(ctx);
}

static void op_event(void *ctx, struct dect_ctl_out *o)
{
	line_to(ctx, CDECT_TO_BRIDGE, o);
}

/* dect-stubd's pump_lapc(): a frame leaves LAPC only when the tunnel has room for it. */
static void op_pump(void *ctx, struct dect_base_link *bl)
{
	struct cdect_host *c = ctx;
	struct dect_mail m;

	while (cdectRoom(c->h) && dect_link_poll_append_mail(&bl->link, &m))
		if (cdectSend(c->h, &m))
			dect_base_log(&c->b, "reg: C-plane mail LOST (%u bytes)",
				      (unsigned)m.param_len);
}

static int op_lanes(void *ctx, int p)
{
	return lanes_left(ctx, p);
}

static void op_forget(void *ctx, unsigned i, const char *why)
{
	struct cdect_host *c = ctx;

	hs_line(c, CDECT_TO_ALL, i, "gone");
	cdectForget(c->h, i, (char *)why);
}

static void op_mask(void *ctx)
{
	struct cdect_host *c = ctx;

	dect_base_slot_mask(&c->b, live_slot_mask(c));
}

/* The date and time come from Go, local time with its zone (host.go). */
static int op_clock(void *ctx, struct dect_time *dt, long *off_s)
{
	struct cdect_host *c = ctx;

	return cdectClock(c->h, dt, off_s);
}

static void op_wall(void *ctx, struct dect_time *dt)
{
	struct cdect_host *c = ctx;
	long off;

	(void)cdectClock(c->h, dt, &off);
}

static void op_named(void *ctx, unsigned i)
{
	struct cdect_host *c = ctx;

	cdectNamed(c->h, i);
	hs_line(c, CDECT_TO_ALL, i, "registered");
}

static int op_name_visible(void *ctx, unsigned viewer, unsigned slot)
{
	struct cdect_host *c = ctx;

	return cdectNameVisible(c->h, viewer, slot);
}

static void op_switched(void *ctx, const char *why)
{
	struct cdect_host *c = ctx;

	cdectSwitched(c->h, c->b.cur, (char *)why);
}

static void op_link_up(void *ctx, int p)
{
	struct cdect_host *c = ctx;

	if (p < 0) {
		c->call_wideband = 0;
		c->nonspeech_noted = 0;
	}
}

static void op_side_down(void *ctx, unsigned k)
{
	struct cdect_host *c = ctx;

	lane_release(c, &c->b.side[k]);
}

/* dect-stubd's base_registered(): the enrolment is in the chip; close the window. */
static void op_registered(void *ctx, uint64_t now)
{
	struct cdect_host *c = ctx;

	c->sub_restored = 1;
	dect_base_reg_window(&c->b, 0, 0, now, "handset enrolled");
	cdectRegistered(c->h, c->b.cur);
}

/*
 * dect-stubd's base_call_up(): a speech call that is connected (or, with
 * bindfirst, about to be) gets its audio bound and its cipher started. A
 * list-access call never does (the maintainers' notes).
 */
static void op_call_up(void *ctx, uint64_t now)
{
	struct cdect_host *c = ctx;
	struct dect_base_link *ml = &c->b.main_link;
	struct dect_cc *cc = &ml->link.cc;

	if (!ml->bound && !c->nonspeech_noted && !cc->incoming &&
	    !dect_cc_peer_is_speech(cc) && cc->state == DECT_CC_ST_ACTIVE) {
		c->nonspeech_noted = 1;
		dect_base_log(&c->b, "call: not a speech call (<<BASIC-SERVICE>> "
			      "%02x) -- no audio path bound",
			      cc->peer_basic_service);
	}
	if (ml->bound || !dect_base_call_auth_ok(&c->b, &ml->link) ||
	    !(cc->state == DECT_CC_ST_ACTIVE ||
	      (c->b.cfg.bind_first && ml->connect_staged &&
	       cc->state == DECT_CC_ST_PROCEEDING)) ||
	    !(cc->incoming || dect_cc_peer_is_speech(cc)))
		return;
	ml->path_on = 0;
	c->bind_ms = now;
	c->call_wideband = !c->b.cfg.narrowband && !(c->flags & 0x40u) &&
			   dect_cc_codec(cc) == DECT_CC_CODEC_G722;
	main_bind(c, ml->handle);
	ml->bound = 1;
	c->nonspeech_noted = 0;
	dect_base_main_cipher_start(&c->b, now);
}

/* dect-stubd's side_route(): a side link's number is complete. */
static void side_route(struct cdect_host *c, unsigned k, uint64_t now)
{
	struct dect_base_link *bl = &c->b.side[k];

	bl->ic_tried = 1;
	if (dect_base_ic_start(&c->b, (int)k, now) ||
	    dect_base_call_out(&c->b, (int)k, now) ||
	    dect_cc_accept(&bl->link.cc) == 0) {
		dect_link_drain(&bl->link, now);
		op_pump(c, bl);
	}
}

/* dect-stubd's side_call_tick(): twocalls on a side link. */
static void op_side_call(void *ctx, unsigned k, uint64_t now)
{
	struct cdect_host *c = ctx;
	struct dect_base_link *bl = &c->b.side[k];
	struct dect_cc *cc = &bl->link.cc;

	if (!c->b.cfg.two_calls)
		return;
	if (!dect_cc_busy(cc))
		dect_cc_set_busy(cc, bl->lane < 0 && lanes_left(c, (int)k) <= 0);
	switch (dect_base_dial_tick(&c->b, (int)k, now)) {
	case 2:
	case 1:
		side_route(c, k, now);
		break;
	}
	if (bl->lane < 0 && cc->state == DECT_CC_ST_ACTIVE &&
	    (cc->incoming || dect_cc_peer_is_speech(cc)) &&
	    dect_base_call_auth_ok(&c->b, &bl->link)) {
		int i = lane_free(c);

		if (i >= 0)
			lane_bind(c, bl, i);
	}
	if (bl->lane >= 0 && !dect_cc_busy(cc))
		lane_release(c, bl);
}

static void op_swapped(void *ctx, unsigned k)
{
	struct cdect_host *c = ctx;

	(void)k;
	c->call_wideband = 0;
	c->nonspeech_noted = 0;
}

static void op_cap_learned(void *ctx, unsigned i)
{
	struct cdect_host *c = ctx;

	cdectCapLearned(c->h, i);
}

static void op_call_down(void *ctx)
{
	struct cdect_host *c = ctx;

	audio_release(c, c->b.main_link.handle);
	c->call_wideband = 0;
}

static const struct dect_base_ops host_ops = {
	.save = op_save, .send = op_send, .random = op_random,
	.log = op_log, .install = op_install, .event = op_event,
	.pump = op_pump, .lanes = op_lanes, .forget = op_forget,
	.mask = op_mask, .clock = op_clock, .named = op_named,
	.name_visible = op_name_visible, .wall = op_wall,
	.switched = op_switched, .link_up = op_link_up,
	.side_down = op_side_down, .registered = op_registered,
	.call_up = op_call_up, .side_call = op_side_call,
	.swapped = op_swapped, .cap_learned = op_cap_learned,
	.call_down = op_call_down,
};

/* ---- Set-up ------------------------------------------------------------- */

struct cdect_host *cdect_host_new(uintptr_t h)
{
	struct cdect_host *c = calloc(1, sizeof(*c));
	unsigned k;

	if (!c)
		return NULL;
	dect_base_init(&c->b);
	c->b.ops = &host_ops;
	c->b.ctx = c;
	c->h = h;
	dect_calls_init(&c->calls);
	c->b.call_lists = &c->calls;
	dect_clip_init(&c->b.clip);
	for (k = 0; k < DECT_BASE_SIDES; k++)
		c->b.side[k].lane = -1;
	/* dect-stubd's defaults (its main()) */
	c->sel = 1;
	c->gain = 0x2Bu;
	c->flags = 1;
	c->lane_ch[0] = 2;
	c->lane_ch[1] = 3;
	c->b.cfg.dial_wait_ms = 3000u;
	c->b.cfg.signal_on = 1;
	c->b.cfg.signal_value = DECT_SIGNAL_ALERT_PATTERN1;
	c->b.cfg.nlr = 1;
	c->b.cfg.locauth = 1;
	c->b.cfg.auth_first = 1;
	return c;
}

void cdect_host_free(struct cdect_host *c)
{
	if (!c)
		return;
	wipe(c, sizeof(*c));
	free(c);
}

/* dect-stubd's sub_load() for record i, from c->rec. */
void cdect_host_record(struct cdect_host *c, unsigned i)
{
	struct dect_hs_rec *r = &c->b.hs[i];
	const struct cdect_rec_in *in = &c->rec;

	if (i >= DECT_BASE_HS_MAX)
		return;
	memset(r, 0, sizeof(*r));
	memcpy(r->uak, in->uak, DECT_MM_UAK_LEN);
	r->uak_valid = 1;
	memcpy(r->port_id, in->port_id, in->port_id_len);
	r->port_id_len = in->port_id_len;
	memcpy(r->cc_port_id, in->cc_port_id, in->cc_port_id_len);
	r->cc_port_id_len = in->cc_port_id_len;
	memcpy(r->cc_fixed_id, in->cc_fixed_id, in->cc_fixed_id_len);
	r->cc_fixed_id_len = in->cc_fixed_id_len;
	if (!r->port_id_len && r->cc_port_id_len <= DECT_MM_PORTID_MAX) {
		memcpy(r->port_id, r->cc_port_id, r->cc_port_id_len);
		r->port_id_len = r->cc_port_id_len;
	}
	r->slot = in->slot;
	memcpy(r->tpui, in->tpui, sizeof(r->tpui));
	c->b.loaded[i] = 1;
	c->b.absent[i] = 0;
	if (i >= c->b.n_hs)
		c->b.n_hs = i + 1u;
	/* a record added to a running stack gets what start gave the others */
	if (c->started)
		dect_profile_defaults(&c->b.prof[i]);
	wipe(&c->rec, sizeof(c->rec));
}

void cdect_host_free_store(struct cdect_host *c, unsigned i)
{
	if (i >= DECT_BASE_HS_MAX)
		return;
	memset(&c->b.hs[i], 0, sizeof(c->b.hs[i]));
	memcpy(c->b.hs[i].tpui, c->rec.tpui, sizeof(c->b.hs[i].tpui));
	wipe(&c->rec, sizeof(c->rec));
	c->b.loaded[i] = 0;
	c->b.absent[i] = 1;
	if (i >= c->b.n_hs)
		c->b.n_hs = i + 1u;
}

/*
 * dect-stubd's main() after the stores: a store that takes an enrolment
 * gets the lowest chip slot nothing else holds, then record 0 is current.
 * 0, or -1 when no slot is left for record 0.
 */
int cdect_host_start(struct cdect_host *c)
{
	unsigned i, k, slot;
	uint64_t used;

	if (c->pin[0])
		c->b.cfg.pin = c->pin;
	if (!c->b.cfg.basic_svc_set)
		c->b.cfg.basic_svc = c->b.cfg.narrowband ? 0x80u : 0x88u;
	/* the standard profile for every record: no model chosen yet */
	for (i = 0; i < c->b.n_hs; i++)
		dect_profile_defaults(&c->b.prof[i]);
	if (c->b.n_hs && !c->b.loaded[0]) {
		used = 0;
		for (i = 1; i < c->b.n_hs; i++)
			if (c->b.loaded[i])
				used |= (uint64_t)1 << c->b.hs[i].slot;
		for (slot = 1; slot <= DECT_MM_MAX_SLOT && (used & ((uint64_t)1 << slot)); slot++)
			;
		if (slot > DECT_MM_MAX_SLOT)
			return -1;
		c->b.hs[0].slot = slot;
	}
	for (i = 1; i < c->b.n_hs; i++) {
		if (c->b.loaded[i] || !c->b.absent[i])
			continue;
		used = 0;
		for (k = 0; k < c->b.n_hs; k++)
			if (k != i && (c->b.loaded[k] || k < i))
				used |= (uint64_t)1 << c->b.hs[k].slot;
		for (slot = 1; slot <= DECT_MM_MAX_SLOT && (used & ((uint64_t)1 << slot)); slot++)
			;
		if (slot > DECT_MM_MAX_SLOT) {
			c->b.remote[i] = 1;	/* never a free store */
			continue;
		}
		c->b.hs[i].slot = slot;
	}
	c->b.cur = 0;
	dect_base_load(&c->b, 0);
	c->started = 1;
	return 0;
}

/* ---- Running ------------------------------------------------------------ */

/* dect-stubd's handle_reg_mail(). */
void cdect_host_mail(struct cdect_host *c, uint64_t now)
{
	const struct dect_mail *m = &c->in;

	c->now = now;
	switch (m->prim) {
	case 0x0209:
		if (m->param_len >= 1)
			dect_base_cipher_on(&c->b, m->param[0]);
		break;
	case DECT_PRIM_CONTACT_ACK1:
		dect_base_contact(&c->b, m);
		break;
	case DECT_PRIM_CONN_UP:
		dect_base_conn_up(&c->b, m->param_len ? m->param[0] : 0, now);
		break;
	case DECT_PRIM_CONN_DOWN:
		dect_base_conn_down(&c->b, m->param_len ? m->param[0] : 0,
				    m->param_len != 0);
		break;
	case DECT_PRIM_SIGNALLING:
		(void)dect_base_segment(&c->b, m, now);
		break;
	case 0x0242:
		/* the channel the chip names; arrival is not success [CR A-0075] */
		if (c->b.main_link.bound && !c->b.main_link.path_on &&
		    m->param_len >= 4 && m->param[0] == c->b.main_link.handle) {
			uint8_t chan = m->param[1] & 0x0Fu;

			if (chan >= 1u && chan <= 9u)
				main_connect(c, c->b.main_link.handle, chan);
		} else if (m->param_len >= 2 && lane_of(c, m->param[0])) {
			lane_connect(c, lane_of(c, m->param[0]));
		}
		break;
	case 0xFFF1u:
		dect_base_mem_reply(&c->b, m);
		break;
	default:
		break;
	}
}

/*
 * With defer_unknown the library holds a {LOCATE-REQUEST}, or a {CC-SETUP} of a
 * handset that moved within the location area and did not locate, from an
 * identity no record of this cell holds. The host is asked once per identity
 * (dect-stubd's fetch_tick, without its simulation) and answers with
 * cdect_host_resume() within the handset's patience (about 15 s, API 18/19).
 */
static void fetch_poll(struct cdect_host *c)
{
	struct dect_base_link *ml = &c->b.main_link;
	const uint8_t *id;
	size_t len;

	if (!c->b.cfg.defer_unknown || !ml->active ||
	    !(dect_mm_locate_pending(&ml->link.mm, &id, &len) ||
	      dect_link_setup_pending(&ml->link, &id, &len))) {
		c->fetch_len = 0;
		return;
	}
	if (len == c->fetch_len && !memcmp(id, c->fetch_id, len))
		return;
	if (len > sizeof(c->fetch_id))
		return;
	memcpy(c->fetch_id, id, len);
	c->fetch_len = len;
	cdectFetch(c->h, (uint8_t *)c->fetch_id, len);
}

void cdect_host_resume(struct cdect_host *c, uint64_t now)
{
	struct dect_base_link *ml = &c->b.main_link;
	const uint8_t *id;
	size_t len;
	unsigned i;
	int setup;

	c->now = now;
	if (!ml->active)
		return;	/* the handset stopped waiting: the record stays for its next attempt */
	if (dect_mm_locate_pending(&ml->link.mm, &id, &len))
		setup = 0;
	else if (dect_link_setup_pending(&ml->link, &id, &len))
		setup = 1;
	else
		return;
	if (len != c->fetch_len || memcmp(id, c->fetch_id, len))
		return;	/* another request is waiting: fetch_poll asks about it */
	for (i = 0; i < c->b.n_hs; i++) {
		const struct dect_hs_rec *r = i == c->b.cur ? &c->b.rec : &c->b.hs[i];
		unsigned sl = dect_base_slot(&c->b, i);
		uint8_t tp[3];

		if (!c->b.loaded[i] || r->port_id_len != len || memcmp(r->port_id, id, len))
			continue;
		memcpy(tp, r->tpui, sizeof(tp));
		(void)dect_mm_hold(&ml->link.mm, r->port_id, r->port_id_len, (uint8_t)sl,
				   (tp[0] | tp[1] | tp[2]) ? tp : NULL, (int)i);
		break;
	}
	if (setup)
		(void)dect_link_setup_resume(&ml->link, now);
	else
		(void)dect_mm_locate_resume(&ml->link.mm);
	c->fetch_len = 0;
	dect_link_drain(&ml->link, now);
}

int cdect_host_drop(struct cdect_host *c, unsigned i)
{
	return dect_base_record_drop(&c->b, i);
}

/* dect_base_record_recycle() with the TPUI from c->rec, wiped after. */
int cdect_host_recycle(struct cdect_host *c, unsigned i)
{
	uint8_t tpui[3];
	int rc;

	memcpy(tpui, c->rec.tpui, sizeof(tpui));
	wipe(&c->rec, sizeof(c->rec));
	rc = dect_base_record_recycle(&c->b, i, tpui);
	return rc;
}

int cdect_host_quiet(const struct cdect_host *c)
{
	return dect_base_quiet(&c->b);
}

/* dect_base_ctl_admin()'s "rename", without the control line. */
int cdect_host_set_name(struct cdect_host *c, unsigned i, const char *name)
{
	if (i >= c->b.n_hs || !dect_base_name_ok(name))
		return -1;
	snprintf(c->b.name[i], sizeof(c->b.name[i]), "%s", name);
	if (c->started) {
		dect_base_names_spread(&c->b, i);
		dect_base_names_notify(&c->b);
	}
	return 0;
}

static void profile_report(void *ctx, unsigned line, const char *msg)
{
	struct cdect_host *c = ctx;

	dect_base_log(&c->b, "profile: line %u: %s", line, msg);
}

/* dect-stubd's way: defaults, the manufacturer's file, the device's. */
int cdect_host_set_profile(struct cdect_host *c, unsigned i, const char *mfr, size_t mfr_len,
			   const char *dev, size_t dev_len)
{
	struct dect_profile *m = NULL, *d = NULL;
	int rc = -1;

	if (i >= c->b.n_hs)
		return -1;
	if (mfr_len) {
		m = malloc(sizeof(*m));
		if (!m || dect_profile_parse(mfr, mfr_len, m, profile_report, c))
			goto out;
	}
	if (dev_len) {
		d = malloc(sizeof(*d));
		if (!d || dect_profile_parse(dev, dev_len, d, profile_report, c))
			goto out;
	}
	dect_profile_resolve(m, d, NULL, &c->b.prof[i]);
	rc = 0;
out:
	free(m);
	free(d);
	return rc;
}

void cdect_host_tick(struct cdect_host *c, uint64_t now)
{
	c->now = now;
	if (c->b.reg_open && c->b.reg_open_until && now >= c->b.reg_open_until)
		dect_base_reg_window(&c->b, 0, 0, now, "window elapsed");
	connect_fallback(c, now);
	dect_base_tick(&c->b, now, c->radio_up);
	fetch_poll(c);
	/* the records into the chip once its radio is up (cell-protocol.md §7.4) */
	if (c->radio_up && !c->sub_restored && (any_uak(c) || !c->no_dummy)) {
		install(c);
		c->sub_restored = 1;
	}
}

/* dect-stubd's ctl_command(), less reload-profiles (no profiles yet). */
int cdect_host_ctl(struct cdect_host *c, uint64_t now, int is_bridge)
{
	struct dect_ctl_msg m;
	struct dect_base_reply r;
	unsigned v;

	c->now = now;
	if (dect_ctl_parse(c->line, strlen(c->line), &m)) {
		reply(c, "?", "malformed", NULL, 0);
		return CDECT_CTL_DONE;
	}
	if (dect_base_ctl_admin(&c->b, &m, now, &r)) {
		reply(c, m.verb, r.err, r.key, r.val);
		if (r.named >= 0) {
			cdectNamed(c->h, (unsigned)r.named);
			hs_line(c, CDECT_TO_ALL, (unsigned)r.named, "registered");
		}
		return CDECT_CTL_DONE;
	}
	if (!strcmp(m.verb, "register-window")) {
		v = 120u;
		if (dect_ctl_get(&m, "seconds") &&
		    dect_ctl_get_uint(&m, "seconds", 1, 3600, &v)) {
			reply(c, m.verb, "bad-seconds", NULL, 0);
			return CDECT_CTL_DONE;
		}
		if (dect_ctl_get(&m, "profile")) {
			reply(c, m.verb, "bad-profile", NULL, 0);
			return CDECT_CTL_DONE;
		}
		if (!c->radio_up) {
			reply(c, m.verb, "not-ready", NULL, 0);
			return CDECT_CTL_DONE;
		}
		dect_base_reg_window(&c->b, 1, v, now, "the control socket");
		reply(c, m.verb, NULL, "seconds", v);
		return CDECT_CTL_DONE;
	}
	if (!strcmp(m.verb, "bridge")) {
		if (c->b.bridge && !is_bridge) {
			reply(c, m.verb, "busy", NULL, 0);
			return CDECT_CTL_DONE;
		}
		c->b.bridge = 1;
		reply(c, m.verb, NULL, NULL, 0);
		return CDECT_CTL_BRIDGE;
	}
	if (dect_base_ctl_call(&c->b, &m, is_bridge, now, &r)) {
		if (!r.sent)
			reply(c, m.verb, r.err, r.key, r.val);
		return CDECT_CTL_DONE;
	}
	reply(c, m.verb, "unknown", NULL, 0);
	return CDECT_CTL_DONE;
}

uint64_t cdect_host_last_seen(const struct cdect_host *c, unsigned i)
{
	return i < c->b.n_hs && i < DECT_BASE_HS_MAX ? c->b.last_seen_ms[i] : 0u;
}
