# Next steps and project overview, 2026-11-02

Written for the maintainer, after the 7412 TPUI work. Three parts: the **next steps** (the
list), the **placement audit** (is each change where it belongs), and the **overview**
(where the project stands, what needs desk work, what a good implementation still lacks).
The overview comes from two read-only surveys of the openDECTcell and controller repositories
(2026-11-02, HEAD controller `32f13ea` and later, openDECTcell `e55bed7`); statements marked *(I)* are
inferences, not seen in a file.

> **Update, later on 2026-11-02 (desk work, no bench).** Done since this was written: the store scale test and a
> capped, ranked ring (step 1, the part that needs no hardware: see the maintainers' notes); the cell credentials in the
> sysupgrade keep list (step 3, in the repository; it reaches boxes with the next package release); the compose file
> builds with the DECT core by default (step 8); a user wiki, `docs/wiki/` (new, not in the list below); the READMEs
> and the stale documents of §3.4 that could be fixed without a decision. Still open: the scale test with real cell
> connections, steps 2 and 4 to 7, `registration_window`, `openapi.yaml`, the version mismatch of the openDECTcell docs.

## 1. Next steps, in order

| # | Step | Who | Needs | Why first |
|---|---|---|---|---|
| 1 | **Synthetic scale test**: fake cells and a few hundred handsets against the controller (tables of 32, expiry, per-cell TPUIs, ring waves, DB load, the media ports) | Claude | nothing | The documented target is 10 cells / 50 handsets (`docs/architecture.md` §7); the goal is hundreds of cells and about 500 handsets. No load test exists. |
| 2 | **Walk test**: both boxes up, carry the A540 between rooms; watch the move log, page it, call it while it sits on the far base | maintainer and Claude | 15 min of the maintainer | Roaming was only forced by stopping boxes; handset-chosen switching, `MovedAfter` and "last cell first" ringing have not been seen. |
| 3 | **Cell credentials in the sysupgrade keep list**, `opendectcell-cell` conffiles, and a note in `docs/install.md` | Claude | nothing | A firmware upgrade drops a cell's enrolment *(I)*; with hundreds of cells that is a site visit each. |
| 4 | **Ring policy for same-class collisions** (retry order, what the caller hears) and a listening test for audio at same-class locates | Claude, then maintainer | 10 min listening | One link per slot class on the 06.88 chip; a ring group over six handsets cannot all ring at once. |
| 5 | **Decisions 17 to 23** (`docs/decisions-2026-11-01.md`), roaming questions 1 to 3 (`docs/roaming.md` §6), images I1 to I5 (`opendectcell/docs/images-plan.md`) | maintainer | the question tool, in a batch | They block release preparation and several UI items. |
| 6 | **Cell update channel**: package or firmware update over the cell protocol (message range 0x0500 is reserved) | Claude, after a design note | decision on scope | Nothing updates a cell today; at hundreds of cells this is required. |
| 7 | **Feature gaps users notice**: list access over the DB (phonebook, call lists), message waiting from SIP NOTIFY, transfer, internal calls, call waiting on air | Claude | one decision each | See §3. |
| 8 | **Housekeeping**: AS690H test setup (TPUI `11 65`, line 1 enabled), stale docs (§3.4), `registration_window` table, default Docker build without `LIBOPENDECTCELL` | Claude | nothing | Cheap and removes traps. |

Not in the list on purpose: a chip firmware patch (parked, `host-cache` decision), 7362 SL
service (another agent), the first public release (the maintainer decided: not before the
controller is done).

## 2. Placement audit: is it where it belongs?

The rule of thumb: what the **chip or the air interface does** belongs in libopendectcell (every
consumer needs it, including the standalone daemon and the 7362); what the **controller decides**
(who owns what, which cell holds whom, the pool) belongs in the controller.

| Change today | Where it is | Belongs | Status |
|---|---|---|---|
| A page response nobody waits for is rejected and its link released | libopendectcell `dect_base.c` (main link, now also side links, one helper) | libopendectcell | Right. The side-link case is new and **not seen on air**; only the main link was. |
| A side link tells a locating handset when to register again | libopendectcell `dect_base_side_start` | libopendectcell | Right, seen on air. Gap: a side link still sets no `defer_unknown`, so a roamer that locates while a call holds the main link is turned away until the call ends. |
| Which TPUIs the 06.88 chip hears (low nibble 1 to 6) | was **only** in the controller (`poolValid`) and copied into migration 0016 | libopendectcell | Moved: `dect_tpui_hearable()` (API 150). The controller keeps a Go copy for builds without the library, and a test (`TestPoolIsWhatTheChipHears`) fails if the two disagree. The SQL of migration 0016 is a frozen copy by nature. |
| Default TPUI of a record the host sets none for | libopendectcell `dect_mm_tpui` | libopendectcell | Was deaf on the 06.88 chip for record numbers 7 to 32; now hearable (API 150). |
| TPUI unique per cell, new TPUI at a locate on a clashing cell | controller `Store.uniqueTPUIs`, migration 0017 | controller | Right: it is a database and ownership matter. A defensive "two records with one TPUI" check inside the library is optional and not built. |
| Pool reservation per cell | controller `Store.ReserveTPUI(ctx, cell)` | controller | Right. |
| Resident-cell code (slots for every handset) | removed from the controller | nowhere | Right: the chip needs no slot. |
| Locate interval hint, default, setting | controller | controller | Right; the interval no longer claims to keep a handset audible. |
| Integration notes for host authors | openDECTcell `docs/notes-for-the-controller.md` | openDECTcell | Updated (the old "keep N from 7" advice is superseded). |
| Packages on the boxes | openDECTcell openwrt feed | openDECTcell | The stack runs in the controller in cell mode, so these library fixes reach the boxes **only through the controller image**. The standalone `opendectcell` daemon needs a package release for them. No release is planned until the controller is done. |

Open from the audit: the same-class ring policy (step 4) probably wants a small piece in the
library (retry while the main link is busy) and a policy in the controller.

## 3. Overview: where the project stands

### 3.1 Done and seen on air
Registration with a PIN window, locate, calls in and out with audio both ways (G.722 passed
through), ciphering, the 7520 and the 7412 as cells of one controller, handsets moving between
them when a base goes away (A540, AS690H), the pool TPUI rule for the 06.88 chip, two handsets of
one slot class. The cell side (TLS 1.3 mutual authentication, token enrolment) interoperates with
the Go controller on the 7412. SIP: registration, SRV/NAPTR, TLS, SRTP, DTMF, interop against
Asterisk and FreeSWITCH in CI. Media engine: jitter buffer, PLC, transcoding, RTCP.

### 3.2 Built but not seen on air
Handset-chosen roaming and the `MovedAfter` expiry, hold and resume, two calls, three parallel
calls on one cell, shared lines with call waiting, DTMF and message waiting in the standalone
daemon (openDECTcell `docs/status.md`), cell media beyond a scripted peer, certificate renewal beyond a
scripted peer, CardDAV beyond Nextcloud, the LuCI firmware upload page, the side-link reject.

### 3.3 Missing features (controller)
Transfer and conference, early media, message waiting from SIP (the NOTIFY handler only answers
200), list access over the database (the contacts table and page exist; no DECT list wiring
*(I)*), call lists on the handset, internal calls (refused: "not supported yet"), a per-handset
dual-ring policy *(I)*, `max_calls` per cell, a firmware or package update path for cells, CA
rotation. UI: a Calls page for users (stub), WebAuthn, personal API tokens, a session list, GDPR
export and account deletion, German, accessibility audit, map tile proxy.

### 3.4 Desk work that needs no bench
- **Decisions** waiting for the maintainer: step 5 above.
- **Documentation that disagrees with the code**: README "design phase"; AGENTS §7 "no DECT or SIP
  call logic before G7"; `decisions-2026-11-01` and `ui-backlog` B15 "calllog has no live writer"
  (it has one); `docs/deployment.md` "libopendectcell Docker build not yet tried"; `docs/sip-ua.md`
  "DECT core is not there yet"; `docs/api/openapi.yaml` lists about 32 of about 87 routes;
  `docs/schema.md` and the `registration_window` table; `docs/architecture.md` roaming text
  predates `docs/roaming.md`; openDECTcell: OpenWrt 25.12.5 against 25.12.6 in README, `status.md`,
  `packages.yml`, `images-plan.md`; the 7362 SL is red in `status.md` and yellow in the README;
  the README advertises releases that are not made. the maintainers' notes is 874 lines of history; §0r is
  the current state.
- **Packaging decisions** (openDECTcell the maintainers' notes): a `opendectcell-fritz7412` package and dropping SSC
  from `opendectcell`; the licence of `kmod-lantiq-tdm-pcm`; the deploy key for the private PCM repo
  (without it a 7412 release has no transport); the signing key lives only in the GitHub secret
  (move to a protected environment); which names and files go public and the final scrub.
- **Upstream submission gaps** (openDECTcell `openwrt/README.md`): the ACL allows `dect-stubd --ctl *`,
  the console getty edit belongs in the board definitions, the `.pot` is not made with LuCI's tools.
- **Task list** `opendectcell/docs/tasks-2026-10-31.md`: L1 (refuse an external call to a handset with
  no line), L2 to L5 (list order and display bugs).

## 4. What a good implementation still lacks

### 4.1 Controller
- **Scale.** Documented target 10 cells / 50 handsets, goal hundreds / 500. No load test; the
  compose file publishes 100 RTP ports (about 50 calls) and 100 cell-media ports; media is always
  relayed through one process.
- **Isolation.** ADR 0003 (worker processes) is decided and unbuilt: the C stacks run in the
  controller, so one library crash takes down every cell and call.
- **Security.** No WebAuthn, session list or API tokens; rate limits for login, sign-up and
  registration only, none seen on the cell port or the API *(I)*; no CA revocation.
- **Observability.** About ten coarse metrics; no per-call, per-cell latency, SIP registration or
  media-quality series; no DECT-side quality (no diagnostics message in the cell protocol); an
  alert only for "cell offline".
- **Backup and HA.** A page that explains `pg_dump` and the master key; no scheduled backup or
  tested restore; one controller, state in PostgreSQL, no HA.
- **Tenancy.** Ownership per user; no per-tenant quotas; the cell operator attribute is proposed only.
- **Docs and i18n.** No admin or user manual, no runbook for the 7412 and 7520 quirks outside the
  openDECTcell notes; English only.
- **Release.** CI builds an image but pushes none; the default Docker build has `LIBOPENDECTCELL=0`, and
  such an image silently drops every cell mail; the private submodule key is optional in CI, and
  without it the cdect tests are skipped.

### 4.2 OpenWrt packages (openDECTcell)
- **Upgrades.** The keep list holds firmware, profiles and handset stores but not
  `/etc/opendectcell/cell/` (key, certificate, CA, enrolment) *(I)*; `opendectcell-cell` has no conffiles; UCI
  migration is ad-hoc shell scripts with no config version; a hand-edited `/etc` file meets the
  `*.apk-new` problem (`docs/journals/bench-later.md`).
- **Resilience.** The 7412 stop hang (4 in about 190 stops, network dead, hardware watchdog does not
  fire) is open; no `prerm` or `postinst`; logs are RAM-only, so evidence of a hang is lost unless a
  syslog sink was added by hand.
- **First run.** `enabled=0` after install; the firmware must be extracted on a PC or uploaded in
  LuCI; no wizard; no German or other translation (only a `.pot`).
- **Factory reset.** Nothing opendectcell-specific; the handset stores live in the overlay and go with
  it; undocumented.
- **Update channel.** GitHub releases and a pasted key; no hosted apk repository or index; the kernel
  module needs the exact kernel and is built for one release only.
- **CI.** `check.yml` runs `make check` only; `make cross` and `make check-cell` are not in CI;
  packages build on a tag; no test installs the apks, runs the uci-defaults or the init script; no
  shellcheck.
- **Hardware matrix.** 7520 works; 7530 untested; 7412 partial (private repo for the PCM module);
  7362 SL partial (service off, no SIP call).

## 5. How this was checked
The placement audit is mine (read of the diffs and the code paths). The overview is two read-only
surveys by sub-agents that read the repositories without touching the AVM material and without
printing secrets; their statements marked *(I)* were not verified in a file. The unit tests, the
library checks and the on-air runs behind §2 are in the maintainers' notes (2026-11-02).
